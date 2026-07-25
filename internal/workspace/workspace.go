// Package workspace materializes a repository at a specific commit into a
// per-run directory, using the host's git. It does a shallow fetch (depth 1) so
// only the triggering commit is pulled, then a detached checkout. With Reuse it
// instead updates an existing checkout in place (fetch + hard reset), keeping
// untracked files such as dependency and build caches. A third mode splits the
// two concerns: SyncMirror keeps a per-repo bare mirror current (the only step
// that touches the network), and Checkout with MirrorDir materializes a
// pristine per-run workspace from that mirror at disk speed. Private-repo
// authentication is the host git configuration's responsibility (SSH agent,
// credential helper, .netrc); Janus does not manage credentials in v1. Git runs
// non-interactively (terminal prompts disabled, ssh in batch mode), so missing
// credentials or an unprovisioned known_hosts fail the checkout fast instead of
// blocking on a prompt no one can answer.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var (
	// shaRe matches a full or abbreviated hex commit. The 7-char minimum keeps
	// the fetch/verify from accepting an ambiguously short prefix.
	shaRe = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
	// refRe is a conservative subset of git's ref grammar: it must start
	// alphanumeric (so it can never be read as a `-option`) and use only a
	// safe alphabet. hasBadRefSeq below rejects the remaining forbidden runs.
	refRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@-]*$`)
)

// validateTarget rejects SHAs and refs that git could parse as options (e.g.
// "--upload-pack=/bin/sh") or resolve ambiguously, before any git process
// runs. These values come from the webhook payload or the manual API, so they
// are untrusted; validation — not a `--` terminator — is the guarantee, since
// `git fetch` does not portably honor `--` before a refspec. A validated SHA
// is hex and a validated ref starts alphanumeric, so neither can be an option,
// and the checkout/reset target is only ever such a SHA or the literal
// "FETCH_HEAD".
func validateTarget(sha, ref string) error {
	if sha != "" && !ValidSHA(sha) {
		return fmt.Errorf("workspace: invalid SHA %q (want 7-64 hex characters)", sha)
	}
	if ref != "" && (!refRe.MatchString(ref) || hasBadRefSeq(ref)) {
		return fmt.Errorf("workspace: invalid ref %q", ref)
	}
	return nil
}

// ValidSHA reports whether s is a full or abbreviated hex commit id (7-64 hex
// characters). It is the single definition of "looks like a git object id":
// besides gating git arguments here, callers upstream use it to reject an
// untrusted SHA at ingestion, before it reaches anything that embeds it in a
// path (a URL path segment, an argument, an environment value).
func ValidSHA(s string) bool { return shaRe.MatchString(s) }

// hasBadRefSeq flags the multi-character sequences git check-ref-format
// forbids that refRe's alphabet alone cannot exclude.
func hasBadRefSeq(ref string) bool {
	return strings.Contains(ref, "..") ||
		strings.Contains(ref, "@{") ||
		strings.Contains(ref, "//") ||
		strings.HasSuffix(ref, ".lock") ||
		strings.HasSuffix(ref, "/")
}

// Options configures a checkout.
type Options struct {
	Dir       string // target directory; created if absent
	RepoURL   string // clone URL (or local path)
	SHA       string // commit to check out (preferred)
	Ref       string // ref to fetch as a fallback when fetch-by-SHA is refused
	Keep      bool   // if true, Cleanup is a no-op (debugging)
	Reuse     bool   // reuse an existing checkout in Dir: fetch + hard-reset, keep untracked files
	MirrorDir string // materialize from this local bare mirror (see SyncMirror) instead of fetching RepoURL; requires SHA, excludes Reuse
}

// Workspace is a checked-out repository on disk.
type Workspace struct {
	Dir  string
	Head string // full 40-char SHA actually checked out (set after verifyHEAD)
	keep bool
	env  []string // hardened env for git subprocesses, built once per checkout
}

// Checkout shallow-fetches the requested commit into opt.Dir and checks it out
// detached. It tries fetch-by-SHA first (some servers disable it) and falls
// back to fetching opt.Ref; either way the checked-out HEAD is then verified
// against opt.SHA, so a run can never silently execute a different commit than
// the one it records. With opt.Reuse, an existing checkout in opt.Dir is
// updated in place instead; if that fails for any reason the directory is
// rebuilt from scratch. The caller must call Cleanup when finished.
func Checkout(ctx context.Context, opt Options) (*Workspace, error) {
	if opt.RepoURL == "" {
		return nil, fmt.Errorf("workspace: RepoURL is required")
	}
	if opt.SHA == "" && opt.Ref == "" {
		return nil, fmt.Errorf("workspace: a SHA or Ref is required")
	}
	if err := validateTarget(opt.SHA, opt.Ref); err != nil {
		return nil, err
	}
	if opt.MirrorDir != "" {
		// The mirror path never falls through to a remote fetch here: the
		// caller (runner) owns the fallback so a broken mirror can be retried
		// with a plain direct checkout under a fresh deadline.
		if opt.Reuse {
			return nil, fmt.Errorf("workspace: MirrorDir and Reuse are mutually exclusive")
		}
		if opt.SHA == "" {
			return nil, fmt.Errorf("workspace: MirrorDir requires a SHA (resolve the ref via SyncMirror first)")
		}
		return mirrorCheckout(ctx, opt)
	}
	if opt.Reuse {
		ws, err := reuseCheckout(ctx, opt)
		if err == nil {
			return ws, nil
		}
		// A reuse failure caused by cancellation or a deadline says nothing
		// about the directory's health — the same reasoning ensureMirror
		// applies to its own probe. Rebuilding here would delete the untracked
		// build caches this strategy exists to keep, and every git command
		// below would fail against the same dead context anyway.
		if ctx.Err() != nil {
			return nil, err
		}
		// Self-heal: any other reuse failure (missing/corrupt .git, stale lock
		// files, unreachable commit) rebuilds the directory from scratch.
		if err := os.RemoveAll(opt.Dir); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(opt.Dir, 0o700); err != nil {
		return nil, err
	}
	// The conservative default env is fine for the network-free init/remote
	// steps; the real env is probed in the initialized workspace below, where
	// local config and `includeIf "gitdir:"` rules resolve correctly.
	ws := &Workspace{Dir: opt.Dir, keep: opt.Keep, env: gitEnv(os.Environ(), false, false)}

	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", opt.RepoURL},
	} {
		if err := ws.git(ctx, args...); err != nil {
			_ = ws.Cleanup()
			return nil, err
		}
	}
	ws.env = gitEnv(os.Environ(),
		gitConfigSet(ctx, ws.Dir, "core.sshCommand"),
		gitConfigSet(ctx, ws.Dir, "core.askpass"))

	target, err := ws.fetchTarget(ctx, opt)
	if err != nil {
		_ = ws.Cleanup()
		return nil, err
	}
	if err := ws.git(ctx, "checkout", "-q", "--detach", target); err != nil {
		_ = ws.Cleanup()
		return nil, err
	}
	if err := ws.verifyHEAD(ctx, opt.SHA); err != nil {
		_ = ws.Cleanup()
		return nil, err
	}
	return ws, nil
}

// fetchTarget shallow-fetches the commit to run and returns the rev to
// materialize: opt.SHA itself when the server allows fetch-by-SHA, otherwise
// "FETCH_HEAD" after falling back to fetching opt.Ref — a shallow ref fetch
// only contains the ref tip, so the (possibly-absent) SHA must not be named.
func (w *Workspace) fetchTarget(ctx context.Context, opt Options) (string, error) {
	if opt.SHA != "" {
		if err := w.git(ctx, "fetch", "--depth", "1", "origin", opt.SHA); err == nil {
			return opt.SHA, nil
		}
	}
	if opt.Ref == "" {
		return "", fmt.Errorf("workspace: fetch by SHA %q failed and no Ref to fall back to", opt.SHA)
	}
	if err := w.git(ctx, "fetch", "--depth", "1", "origin", opt.Ref); err != nil {
		return "", err
	}
	return "FETCH_HEAD", nil
}

// verifyHEAD resolves the checked-out commit, records it on w.Head, and — when
// a SHA was requested — fails unless HEAD matches it. After the ref fallback in
// fetchTarget the materialized commit is whatever the ref points at *now*; if
// the branch moved (or was rewritten) between the event and the checkout,
// running it would silently execute code the run's recorded SHA does not
// identify. Prefix match: events carry the full 40-char SHA, but `janus run
// --sha` may abbreviate. With an empty SHA (ref-only runs) it only records
// Head, so callers can pin metadata to the exact commit that ran.
func (w *Workspace) verifyHEAD(ctx context.Context, sha string) error {
	head, err := w.gitOut(ctx, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if sha != "" && !strings.HasPrefix(head, strings.ToLower(sha)) {
		return fmt.Errorf("workspace: checked-out commit %s is not the requested SHA %s (the ref moved after the event; re-trigger, or omit the SHA to run the current tip)", head, sha)
	}
	w.Head = head
	return nil
}

// maxChangedFiles and maxChangedBytes bound the diff read by ChangedFiles. A
// larger diff returns an error rather than a truncated list: a dropped file
// could wrongly skip a path-filtered run, and the caller treats errors as
// "run everything". The byte cap is enforced while reading, so a huge push
// never buffers an unbounded name list in memory just to be rejected.
// maxChangedBytes is a var so tests can shrink it.
const maxChangedFiles = 10_000

var maxChangedBytes int64 = 4 << 20

// commitSHARe accepts (possibly abbreviated) hex commit ids. Beyond format
// validation it guards the value's use as a git argument — hex can never be
// mistaken for a flag.
var commitSHARe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// ChangedFiles returns the files that differ between the before commit and
// the checked-out HEAD, slash-separated and repo-relative, for `paths`
// filtering. It is a pure tree diff, so it works between two disjoint
// depth-1 commits: the checkout stays shallow and before is fetched on
// demand (one extra round-trip, paid only by workflows that declare a path
// filter). Any failure — unknown before (force-push pruned it, server
// forbids fetch-by-SHA), oversized diff — is an error; callers must fail
// open and run the pipeline.
func (w *Workspace) ChangedFiles(ctx context.Context, before string) ([]string, error) {
	before = strings.ToLower(strings.TrimSpace(before))
	if !commitSHARe.MatchString(before) {
		return nil, fmt.Errorf("workspace: before %q is not a commit SHA", before)
	}
	if err := w.git(ctx, "cat-file", "-e", before+"^{commit}"); err != nil {
		if err := w.git(ctx, "fetch", "--depth", "1", "origin", before); err != nil {
			return nil, err
		}
	}
	// -z: NUL separators and no quoting of unusual names; --no-renames: a
	// rename is a change to both paths, and rename detection would cost time
	// just to hide one of them from the filter. The output is read raw off a
	// pipe under a byte budget — not through gitOut, whose whole-output
	// buffering would be unbounded and whose TrimSpace would corrupt a
	// filename that legitimately starts with whitespace.
	dctx, cancel := context.WithCancel(ctx)
	defer cancel() // over-budget: kill git rather than drain an oversized diff
	cmd := w.gitCmd(dctx, "diff", "--name-only", "--no-renames", "-z", before, "HEAD")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(stdout, maxChangedBytes+1))
	if err != nil || int64(len(raw)) > maxChangedBytes {
		cancel()
		_ = cmd.Wait()
		if err != nil {
			return nil, fmt.Errorf("workspace: read diff %s..HEAD: %w", before, err)
		}
		return nil, fmt.Errorf("workspace: diff %s..HEAD exceeds %d bytes (max for path filtering)", before, maxChangedBytes)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("git diff: %w\n%s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	var files []string
	for _, f := range bytes.Split(raw, []byte{0}) {
		if len(f) == 0 {
			continue
		}
		files = append(files, string(f))
	}
	if len(files) > maxChangedFiles {
		return nil, fmt.Errorf("workspace: diff %s..HEAD touches %d files (max %d for path filtering)", before, len(files), maxChangedFiles)
	}
	return files, nil
}

// reuseCheckout updates an existing checkout in place: fetch the commit and
// hard-reset tracked files to it. Untracked files (dependency caches, build
// output) deliberately survive — reset, unlike checkout, also overwrites
// untracked files that a new commit starts tracking. Any failure is returned
// so the caller can rebuild the directory from scratch.
func reuseCheckout(ctx context.Context, opt Options) (*Workspace, error) {
	if _, err := os.Stat(filepath.Join(opt.Dir, ".git")); err != nil {
		return nil, err
	}
	ws := &Workspace{Dir: opt.Dir, keep: opt.Keep, env: gitEnv(os.Environ(),
		gitConfigSet(ctx, opt.Dir, "core.sshCommand"),
		gitConfigSet(ctx, opt.Dir, "core.askpass"))}
	if err := ws.git(ctx, "remote", "set-url", "origin", opt.RepoURL); err != nil {
		return nil, err
	}
	target, err := ws.fetchTarget(ctx, opt)
	if err != nil {
		return nil, err
	}
	if err := ws.git(ctx, "reset", "-q", "--hard", target); err != nil {
		return nil, err
	}
	if err := ws.verifyHEAD(ctx, opt.SHA); err != nil {
		return nil, err
	}
	return ws, nil
}

// mirrorCheckout materializes opt.Dir from the local bare mirror at
// opt.MirrorDir: a --local clone hardlinks the mirror's object store (git
// degrades to plain copies across filesystems or on Windows), so no network
// is involved and worktree files are written fresh — runs can never mutate
// the mirror through shared inodes. --no-checkout skips materializing the
// mirror's HEAD branch just to replace it with the detached checkout (and
// tolerates a mirror whose HEAD ref doesn't match the remote's default
// branch). origin is then re-pointed at the real remote so on-demand fetches
// after the checkout (ChangedFiles' before commit) reach the remote, not the
// mirror.
//
// The clone may run concurrently with another run's fetch into the mirror —
// git documents --local as racy against source modification in general, but
// this specific pairing is safe: every mirror file becomes visible only via
// atomic rename (packs, indexes, loose objects, refs), so a hardlink can
// never capture partial content; the objects this run needs were verified
// present while the sync held the repo lock; and implicit auto-maintenance
// is disabled in the mirror (gc.auto=0, maintenance.auto=false — see
// configureMirror). The one path that does rewrite mirror files —
// maintainMirror's compaction — runs only synchronously under that same
// repo lock, and git's prune grace period keeps any recently-fetched object
// alive, so a racing clone losing an object it needs is practically
// impossible. What remains is a rare clone *error* (a --prune'd loose ref or
// a repacked pack unlinked between readdir and link — hardlinks already made
// survive), which the caller handles by retrying with a direct checkout.
// Serializing clones against fetches was considered and rejected: it would
// deny or delay the cache for busy repos to defend against a benign race.
func mirrorCheckout(ctx context.Context, opt Options) (*Workspace, error) {
	if err := os.MkdirAll(opt.Dir, 0o700); err != nil {
		return nil, err
	}
	ws := &Workspace{Dir: opt.Dir, keep: opt.Keep, env: gitEnv(os.Environ(), false, false)}
	if err := ws.git(ctx, "clone", "-q", "--local", "--no-checkout", opt.MirrorDir, "."); err != nil {
		_ = ws.Cleanup()
		return nil, err
	}
	ws.env = gitEnv(os.Environ(),
		gitConfigSet(ctx, ws.Dir, "core.sshCommand"),
		gitConfigSet(ctx, ws.Dir, "core.askpass"))
	for _, args := range [][]string{
		{"remote", "set-url", "origin", opt.RepoURL},
		{"checkout", "-q", "--detach", opt.SHA},
	} {
		if err := ws.git(ctx, args...); err != nil {
			_ = ws.Cleanup()
			return nil, err
		}
	}
	if err := ws.verifyHEAD(ctx, opt.SHA); err != nil {
		_ = ws.Cleanup()
		return nil, err
	}
	return ws, nil
}

// SyncMirror ensures the bare mirror of repoURL at dir contains the requested
// commit and returns the full SHA to materialize, fetching from the remote
// only when the commit isn't already cached — the network cost of a busy repo
// converges to one fetch per new commit, shared by every run. The mirror is
// only ever an accelerator: callers treat any error as "mirror unavailable"
// and fall back to a direct checkout, so a broken mirror can never fail a run
// the direct path would have served.
func SyncMirror(ctx context.Context, dir, repoURL, sha, ref string) (string, error) {
	if repoURL == "" {
		return "", fmt.Errorf("workspace: RepoURL is required")
	}
	if sha == "" && ref == "" {
		return "", fmt.Errorf("workspace: a SHA or Ref is required")
	}
	if err := validateTarget(sha, ref); err != nil {
		return "", err
	}
	m, err := ensureMirror(ctx, dir, repoURL)
	if err != nil {
		return "", err
	}
	return m.mirrorTarget(ctx, sha, ref)
}

// ensureMirror opens the bare mirror at dir, creating it if absent. A dir
// that exists but that git itself rejects as a bare repository — garbage, or
// debris from an interrupted creation — is rebuilt from scratch once (the
// same self-heal contract as persistent workspaces), and the mirror's
// required configuration is re-asserted on every call (see configureMirror).
// Only git's own verdict triggers the rebuild: a probe that fails because
// the context was cancelled or git couldn't run at all (resource or
// permission trouble) says nothing about the repository, so it propagates as
// an error — the caller falls back to a direct checkout for this run and the
// cache survives. Fetch failures likewise deliberately do NOT rebuild:
// they're usually the network's fault, and deleting a large healthy mirror
// on a transient error would force a full re-clone on the same bad network.
func ensureMirror(ctx context.Context, dir, repoURL string) (*Workspace, error) {
	m := &Workspace{Dir: dir, keep: true, env: gitEnv(os.Environ(), false, false)}
	healthy := false
	if _, err := os.Stat(dir); err == nil {
		switch bare, err := m.gitOut(ctx, "rev-parse", "--is-bare-repository"); {
		case err == nil && bare == "true":
			healthy = true
		case err == nil:
			// git answered, and the answer is "not a bare repository" —
			// rebuild below.
		case ctx.Err() != nil:
			// Cancellation/deadline — checked before the ExitError test,
			// because a deadline killing git mid-run also surfaces as one.
			return nil, err
		default:
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				// git never ran to a verdict (spawn or resource failure):
				// unknown health is not corruption.
				return nil, err
			}
			// git ran and rejected the directory — rebuild below.
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if !healthy {
		if err := os.RemoveAll(dir); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if err := m.git(ctx, "init", "-q", "--bare"); err != nil {
			return nil, err
		}
	}
	// Re-probe now that the repository exists, mirroring Checkout: local
	// config and includeIf "gitdir:" rules resolve correctly.
	m.env = gitEnv(os.Environ(),
		gitConfigSet(ctx, dir, "core.sshCommand"),
		gitConfigSet(ctx, dir, "core.askpass"))
	if err := configureMirror(ctx, m, repoURL); err != nil {
		return nil, err
	}
	return m, nil
}

// configureMirror (re)asserts every setting the mirror's correctness rests
// on, idempotently, on every sync:
//
//   - remote.origin.url — the configured clone_url may change over time;
//     written as a config key (not `remote add`/`set-url`, which each fail
//     depending on whether origin already exists), so a creation interrupted
//     right after `init --bare` heals here instead of wedging every sync.
//   - explicit heads+tags refspecs — deliberately not `clone --mirror`'s
//     +refs/*:refs/*, which would also drag in hosting-side namespaces
//     (GitLab's refs/merge-requests/*, refs/keep-around/*, ...) with
//     unbounded growth. --replace-all then --add leaves exactly these two
//     entries no matter what was there before.
//   - gc.auto=0 AND maintenance.auto=false — git must never repack or prune
//     mirror files *implicitly*: the only sanctioned rewrite path is the
//     explicit, synchronous maintainMirror pass on the locked sync path.
//     Both knobs are needed: gc.auto=0 covers the default post-fetch gc task
//     (and older gits that run `gc --auto` directly), but host config can
//     route auto-maintenance to tasks that ignore gc.auto and rewrite object
//     files (maintenance.strategy=incremental, loose-objects,
//     incremental-repack — e.g. set globally by scalar), and modern git
//     detaches maintenance into the background, outliving the repo lock.
//     maintenance.auto=false stops the post-fetch hook entirely; scheduled
//     `git maintenance start` jobs touch only registered repos, which the
//     mirror never is. Repo-local config rather than fetch flags — flag
//     spellings vary by git version and unknown flags error, while config
//     keys are always accepted and cover every git invocation in the mirror.
func configureMirror(ctx context.Context, m *Workspace, repoURL string) error {
	for _, args := range [][]string{
		{"config", "remote.origin.url", repoURL},
		{"config", "--replace-all", "remote.origin.fetch", "+refs/heads/*:refs/heads/*"},
		{"config", "--add", "remote.origin.fetch", "+refs/tags/*:refs/tags/*"},
		{"config", "gc.auto", "0"},
		{"config", "maintenance.auto", "false"},
	} {
		if err := m.git(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

// mirrorTarget resolves the requested commit in the mirror, fetching only on
// a cache miss, and returns its full SHA. A requested SHA must resolve to a
// commit it prefixes (the verifyHEAD contract); it is fetched by refreshing
// all branches and tags first — the way a new commit normally arrives, and
// the only way when the server refuses fetch-by-SHA — then by bare SHA for
// commits no longer reachable from any ref. Ref-only requests always fetch:
// the tip is whatever the remote has now. Syncs that fetched end with a
// maintainMirror pass (cache hits stay compaction-free), after the resolve
// so housekeeping can never affect the answer.
func (w *Workspace) mirrorTarget(ctx context.Context, sha, ref string) (string, error) {
	if sha != "" {
		fetched := false
		if w.git(ctx, "cat-file", "-e", sha+"^{commit}") != nil {
			if err := w.git(ctx, "fetch", "-q", "--prune", "origin"); err != nil {
				return "", err
			}
			fetched = true
			if w.git(ctx, "cat-file", "-e", sha+"^{commit}") != nil {
				if err := w.git(ctx, "fetch", "-q", "origin", sha); err != nil {
					return "", err
				}
			}
		}
		head, err := w.gitOut(ctx, "rev-parse", "--verify", sha+"^{commit}")
		if err != nil {
			return "", err
		}
		if !strings.HasPrefix(head, strings.ToLower(sha)) {
			return "", fmt.Errorf("workspace: mirror resolved %s, which the requested SHA %s does not identify", head, sha)
		}
		if fetched {
			if err := w.compactAndVerify(ctx, head); err != nil {
				return "", err
			}
		}
		return head, nil
	}
	if err := w.git(ctx, "fetch", "-q", "--prune", "origin"); err != nil {
		return "", err
	}
	head, err := w.gitOut(ctx, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	if err := w.compactAndVerify(ctx, head); err != nil {
		return "", err
	}
	return head, nil
}

// gcAutoOpts configures the one sanctioned compaction run. Each pinned
// setting is load-bearing for the mirror's safety argument, so none may be
// inherited from host config:
//
//   - gc.auto re-enables git's stock heuristics, which the mirror's own
//     config pins to 0 against implicit maintenance (this necessarily also
//     shadows an operator's global gc.auto).
//   - gc.autoDetach=false keeps the work in-process, so compaction can never
//     outlive the caller's lock.
//   - gc.pruneExpire fixes the grace period at git's own default. Prune only
//     ever touches unreachable objects, but a commit fetched by bare SHA is
//     exactly that, so a host `gc.pruneExpire=now` would let compaction
//     delete the very commit the sync just resolved — the grace period is an
//     invariant of this design, not an operator preference.
//
// Everything else (gc.autoPackLimit, aggressiveness) stays host-tunable
// through normal git config. A var so tests can tighten the thresholds to
// force a compaction.
var gcAutoOpts = []string{"-c", "gc.auto=6700", "-c", "gc.autoDetach=false", "-c", "gc.pruneExpire=2.weeks.ago"}

// maintainMirror lets git's own heuristics decide whether the mirror needs
// compacting — roughly every gc.autoPackLimit fetch-created packs — and runs
// the work synchronously. Callers reach it only from the sync path, which the
// runner serializes with the per-repo lock, making this the single sanctioned
// path that may rewrite mirror files (see mirrorCheckout for why that keeps
// unlocked clones safe). Best-effort: a failed compaction leaves a working,
// merely uncompacted mirror, so the error is deliberately dropped rather
// than failing a sync that already has its answer.
func (w *Workspace) maintainMirror(ctx context.Context) {
	args := append(append([]string{}, gcAutoOpts...), "gc", "--auto", "--quiet")
	_ = w.git(ctx, args...)
}

// compactAndVerify runs the compaction pass and then enforces this package's
// postcondition — the mirror still contains the commit just resolved — rather
// than merely arguing it: gcAutoOpts pins a grace period that protects a
// freshly fetched (and possibly ref-unreachable) commit, but a skewed clock
// or an exotic host configuration should surface as a named error, not as an
// opaque clone failure later. The caller treats the error as "mirror
// unavailable" and falls back to a direct checkout.
func (w *Workspace) compactAndVerify(ctx context.Context, head string) error {
	w.maintainMirror(ctx)
	if err := w.git(ctx, "cat-file", "-e", head+"^{commit}"); err != nil {
		return fmt.Errorf("workspace: mirror no longer contains %s after compaction: %w", head, err)
	}
	return nil
}

// Cleanup removes the workspace directory unless Keep was set.
func (w *Workspace) Cleanup() error {
	if w == nil || w.keep || w.Dir == "" {
		return nil
	}
	return os.RemoveAll(w.Dir)
}

// gitEnv returns base (the daemon's environment) hardened so a checkout can
// never block until the checkout deadline. Defaults apply only where the
// operator configured nothing — environment or gitconfig (the *Configured
// flags carry the gitconfig probes); anything they did configure is respected
// verbatim and must itself be non-interactive:
//
//   - GIT_TERMINAL_PROMPT=0, always: no terminal credential prompts.
//   - Unless GIT_SSH_COMMAND/GIT_SSH is set or core.sshCommand is configured,
//     ssh runs with BatchMode=yes (host-key and passphrase prompts fail
//     immediately), a bounded connect (an unreachable advertised host fails in
//     ~10s instead of the ~2min TCP timeout), and keepalive probes that abort
//     a stalled connection in ~60s.
//   - Unless GIT_ASKPASS is set or core.askpass is configured, GIT_ASKPASS is
//     set *empty*: git treats set-but-empty as "askpass disabled" (and stops
//     falling back to core.askpass/SSH_ASKPASS), so credential requests hit
//     the terminal path that GIT_TERMINAL_PROMPT=0 blocks and fail
//     immediately — GIT_TERMINAL_PROMPT alone does not stop askpass helpers,
//     so a blocking helper could otherwise occupy the checkout slot until the
//     deadline. (Not a no-op program like echo: git passes the prompt as an
//     argument, which echo would return as the credential.)
//
// Host keys are still verified; the operator provisions known_hosts.
func gitEnv(base []string, sshConfigured, askpassConfigured bool) []string {
	env := append(append([]string{}, base...), "GIT_TERMINAL_PROMPT=0")
	if !sshConfigured && !envHas(base, "GIT_SSH_COMMAND") && !envHas(base, "GIT_SSH") {
		env = append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=4")
	}
	if !askpassConfigured && !envHas(base, "GIT_ASKPASS") {
		env = append(env, "GIT_ASKPASS=")
	}
	return env
}

// envHas reports whether base contains a variable named name. Windows
// environment names are case-insensitive — and Go's exec dedups them that way,
// so an appended default would clobber a differently-cased operator value —
// hence the comparison folds case there; elsewhere git reads names exactly, so
// the match is exact.
func envHas(base []string, name string) bool {
	return envHasFold(base, name, runtime.GOOS == "windows")
}

func envHasFold(base []string, name string, fold bool) bool {
	for _, kv := range base {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		key := kv[:i]
		if key == name || (fold && strings.EqualFold(key, name)) {
			return true
		}
	}
	return false
}

// gitConfigSet reports whether git resolves a value for key in dir's context —
// system, global, `includeIf "gitdir:"` rules, and the workspace's own local
// config all evaluate exactly as they will for the checkout's git commands —
// so the hardening defaults can defer to an operator's gitconfig the same way
// they defer to environment variables. Probing in dir (not the daemon's CWD)
// also means an unrelated repository containing the daemon's working directory
// can never suppress hardening.
func gitConfigSet(ctx context.Context, dir, key string) bool {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "config", "--get", key).Output()
	return err == nil && len(strings.TrimSpace(string(out))) > 0
}

// gitCmd builds the git invocation for this workspace with the hardened env.
func (w *Workspace) gitCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", w.Dir}, args...)...)
	cmd.Env = w.env
	if cmd.Env == nil {
		cmd.Env = gitEnv(os.Environ(), false, false)
	}
	return cmd
}

func (w *Workspace) git(ctx context.Context, args ...string) error {
	out, err := w.gitCmd(ctx, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gitOut runs git in the workspace and returns its trimmed stdout.
func (w *Workspace) gitOut(ctx context.Context, args ...string) (string, error) {
	out, err := w.gitCmd(ctx, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

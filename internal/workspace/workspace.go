// Package workspace materializes a repository at a specific commit into a
// per-run directory, using the host's git. It does a shallow fetch (depth 1) so
// only the triggering commit is pulled, then a detached checkout. With Reuse it
// instead updates an existing checkout in place (fetch + hard reset), keeping
// untracked files such as dependency and build caches. Private-repo
// authentication is the host git configuration's responsibility (SSH agent,
// credential helper, .netrc); Janus does not manage credentials in v1. Git runs
// non-interactively (terminal prompts disabled, ssh in batch mode), so missing
// credentials or an unprovisioned known_hosts fail the checkout fast instead of
// blocking on a prompt no one can answer.
package workspace

import (
	"context"
	"errors"
	"fmt"
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
	if sha != "" && !shaRe.MatchString(sha) {
		return fmt.Errorf("workspace: invalid SHA %q (want 7-64 hex characters)", sha)
	}
	if ref != "" && (!refRe.MatchString(ref) || hasBadRefSeq(ref)) {
		return fmt.Errorf("workspace: invalid ref %q", ref)
	}
	return nil
}

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
	Dir     string // target directory; created if absent
	RepoURL string // clone URL (or local path)
	SHA     string // commit to check out (preferred)
	Ref     string // ref to fetch as a fallback when fetch-by-SHA is refused
	Keep    bool   // if true, Cleanup is a no-op (debugging)
	Reuse   bool   // reuse an existing checkout in Dir: fetch + hard-reset, keep untracked files
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
	if opt.Reuse {
		if ws, err := reuseCheckout(ctx, opt); err == nil {
			return ws, nil
		}
		// Self-heal: any reuse failure (missing/corrupt .git, stale lock
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

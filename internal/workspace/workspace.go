// Package workspace materializes a repository at a specific commit into a
// per-run directory, using the host's git. It does a shallow fetch (depth 1) so
// only the triggering commit is pulled, then a detached checkout. With Reuse it
// instead updates an existing checkout in place (fetch + hard reset), keeping
// untracked files such as dependency and build caches. Private-repo
// authentication is the host git configuration's responsibility (SSH agent,
// credential helper, .netrc); Janus does not manage credentials in v1.
package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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
	keep bool
}

// Checkout shallow-fetches the requested commit into opt.Dir and checks it out
// detached. It tries fetch-by-SHA first (some servers disable it) and falls
// back to fetching opt.Ref. With opt.Reuse, an existing checkout in opt.Dir is
// updated in place instead; if that fails for any reason the directory is
// rebuilt from scratch. The caller must call Cleanup when finished.
func Checkout(ctx context.Context, opt Options) (*Workspace, error) {
	if opt.RepoURL == "" {
		return nil, fmt.Errorf("workspace: RepoURL is required")
	}
	if opt.SHA == "" && opt.Ref == "" {
		return nil, fmt.Errorf("workspace: a SHA or Ref is required")
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
	ws := &Workspace{Dir: opt.Dir, keep: opt.Keep}

	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", opt.RepoURL},
	} {
		if err := ws.git(ctx, args...); err != nil {
			_ = ws.Cleanup()
			return nil, err
		}
	}

	// Prefer fetching the exact commit. If the server refuses fetch-by-SHA we
	// fall back to the ref — but a shallow ref fetch only contains the ref tip,
	// so we must then check out FETCH_HEAD, not the (possibly-absent) SHA.
	fetchedSHA := false
	if opt.SHA != "" {
		if err := ws.git(ctx, "fetch", "--depth", "1", "origin", opt.SHA); err == nil {
			fetchedSHA = true
		}
	}
	if !fetchedSHA {
		if opt.Ref == "" {
			_ = ws.Cleanup()
			return nil, fmt.Errorf("workspace: fetch by SHA %q failed and no Ref to fall back to", opt.SHA)
		}
		if err := ws.git(ctx, "fetch", "--depth", "1", "origin", opt.Ref); err != nil {
			_ = ws.Cleanup()
			return nil, err
		}
	}

	target := "FETCH_HEAD"
	if fetchedSHA {
		target = opt.SHA
	}
	if err := ws.git(ctx, "checkout", "-q", "--detach", target); err != nil {
		_ = ws.Cleanup()
		return nil, err
	}
	return ws, nil
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
	ws := &Workspace{Dir: opt.Dir, keep: opt.Keep}
	if err := ws.git(ctx, "remote", "set-url", "origin", opt.RepoURL); err != nil {
		return nil, err
	}
	fetchedSHA := false
	if opt.SHA != "" {
		if err := ws.git(ctx, "fetch", "--depth", "1", "origin", opt.SHA); err == nil {
			fetchedSHA = true
		}
	}
	if !fetchedSHA {
		if opt.Ref == "" {
			return nil, fmt.Errorf("workspace: fetch by SHA %q failed and no Ref to fall back to", opt.SHA)
		}
		if err := ws.git(ctx, "fetch", "--depth", "1", "origin", opt.Ref); err != nil {
			return nil, err
		}
	}
	target := "FETCH_HEAD"
	if fetchedSHA {
		target = opt.SHA
	}
	if err := ws.git(ctx, "reset", "-q", "--hard", target); err != nil {
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

func (w *Workspace) git(ctx context.Context, args ...string) error {
	full := append([]string{"-C", w.Dir}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

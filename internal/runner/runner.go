// Package runner is the coordinator between a trigger event and a pipeline run:
// it checks out the repository at the event's commit, reads and parses the
// pipeline file from that checkout (the configured path, or the event's
// per-trigger override), matches the event against the workflow's `on:` filters
// (manual triggers always match), records a run, and executes it
// asynchronously. It is the single entry point shared by the manual trigger and
// the webhook handlers.
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hutusi/janus/internal/allowlist"
	"github.com/hutusi/janus/internal/engine"
	"github.com/hutusi/janus/internal/model"
	"github.com/hutusi/janus/internal/pipeline"
	"github.com/hutusi/janus/internal/store"
	"github.com/hutusi/janus/internal/workspace"
)

// ErrRepoNotAllowed is returned by Trigger when the event's repository URL is
// not permitted by the configured allowlist. Handlers map it to HTTP 403.
var ErrRepoNotAllowed = errors.New("repository not allowed")

// ErrBusy is returned by Trigger when the bounded admission queue — runs
// executing plus runs waiting for a slot — is full. Handlers map it to HTTP
// 503 so callers back off and retry instead of piling up checkouts.
var ErrBusy = errors.New("runner at capacity")

// Runner coordinates checkout → parse → match → execute.
type Runner struct {
	store        store.Store
	engine       *engine.Engine
	wsRoot       string
	pipelinePath string
	keepWS       bool
	persistent   bool
	allow        allowlist.Allowlist

	ctx    context.Context // root context for run execution; cancelled on Shutdown
	cancel context.CancelFunc
	sem    chan struct{}  // caps concurrently executing runs
	admit  chan struct{}  // caps the whole trigger lifecycle: checkout + parse + pending queue
	wg     sync.WaitGroup // tracks in-flight runs for graceful shutdown

	locksMu sync.Mutex             // guards locks
	locks   map[string]*sync.Mutex // per-repo workspace locks (persistent strategy)
}

// Options configures a Runner.
type Options struct {
	WSRoot       string              // where per-run workspaces are created
	PipelinePath string              // in-repo path to the pipeline file (an event may override it)
	KeepWS       bool                // keep workspaces after runs (debugging)
	Persistent   bool                // one reusable workspace per repo, updated in place (workspace_strategy: persistent)
	MaxRuns      int                 // max concurrent runs (<=0 means 4)
	Allowlist    allowlist.Allowlist // repos permitted to run (empty denies all)
}

// Result reports the outcome of a trigger. Started is false (with a Reason)
// when the event did not match the workflow's `on:` filters — that is not an
// error, just a no-op.
type Result struct {
	RunID   string
	Started bool
	Reason  string
}

// New creates a Runner from opts.
func New(st store.Store, eng *engine.Engine, opts Options) *Runner {
	maxRuns := opts.MaxRuns
	if maxRuns <= 0 {
		maxRuns = 4
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{
		store:        st,
		engine:       eng,
		wsRoot:       opts.WSRoot,
		pipelinePath: opts.PipelinePath,
		keepWS:       opts.KeepWS,
		persistent:   opts.Persistent,
		allow:        opts.Allowlist,
		ctx:          ctx,
		cancel:       cancel,
		sem:          make(chan struct{}, maxRuns),
		// maxRuns executing plus a 3×maxRuns pending backlog. Derived rather
		// than configurable: the point is that a trigger burst cannot start
		// unbounded git processes or workspaces, not the exact queue depth.
		admit: make(chan struct{}, 4*maxRuns),
		locks: make(map[string]*sync.Mutex),
	}
}

// repoLock returns the mutex serializing the persistent workspace of repoURL.
func (r *Runner) repoLock(repoURL string) *sync.Mutex {
	r.locksMu.Lock()
	defer r.locksMu.Unlock()
	mu, ok := r.locks[repoURL]
	if !ok {
		mu = &sync.Mutex{}
		r.locks[repoURL] = mu
	}
	return mu
}

// persistDirName is the stable directory name for a repo's persistent
// workspace. Hex-only, so it is safe on every filesystem. It is keyed on the
// exact repo URL string — the same string that keys the repo lock, so URL
// variants of one repo get separate (duplicated) caches, never a shared
// unlocked directory.
func persistDirName(repoURL string) string {
	sum := sha256.Sum256([]byte(repoURL))
	return "persist-" + hex.EncodeToString(sum[:8])
}

// Sweep removes leftover workspaces from a previous process (crash recovery).
// Call once at startup, before serving. Only per-run directories (run-*) are
// removed; persistent per-repo workspaces (persist-*) deliberately survive
// restarts — their caches are the point.
func (r *Runner) Sweep() error {
	matches, err := filepath.Glob(filepath.Join(r.wsRoot, "run-*"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		_ = os.RemoveAll(m)
	}
	return nil
}

// ReconcileInterrupted marks runs left non-terminal by a previous process —
// a crash or hard kill — as cancelled, so they stop displaying as running
// forever and log followers terminate. Anything mid-flight becomes cancelled;
// work that never started becomes skipped. Call once at startup, before
// serving (like Sweep); the executing goroutines died with the old process,
// so nothing will ever advance these records again. Returns how many runs
// were repaired; per-run store errors skip that run, and the first is
// returned after the pass completes.
func (r *Runner) ReconcileInterrupted() (int, error) {
	runs, err := r.store.ListRuns(0)
	if err != nil {
		return 0, err
	}
	var repaired int
	var firstErr error
	now := time.Now()
	for _, run := range runs {
		if run.Status.Terminal() {
			continue
		}
		for _, jr := range run.Jobs {
			for _, sr := range jr.Steps {
				if !sr.Status.Terminal() {
					if sr.Status == model.StatusRunning {
						sr.Status = model.StatusCancelled
						sr.FinishedAt = now
					} else {
						sr.Status = model.StatusSkipped
					}
				}
			}
			if !jr.Status.Terminal() {
				if jr.Status == model.StatusRunning {
					jr.Status = model.StatusCancelled
					jr.FinishedAt = now
				} else {
					jr.Status = model.StatusSkipped
				}
			}
		}
		run.Status = model.StatusCancelled
		run.FinishedAt = now
		if err := r.store.UpdateRun(run); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		repaired++
	}
	return repaired, firstErr
}

// Shutdown stops accepting new run work and waits up to grace for in-flight
// runs to finish; if they don't, it cancels them (killing their host
// processes) and waits for the unwind. Call after the HTTP listener is closed.
func (r *Runner) Shutdown(grace time.Duration) {
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		r.cancel()
	case <-time.After(grace):
		r.cancel() // grace expired: cancel in-flight runs, then wait for unwind
		<-done
	}
}

// Trigger checks out the repo at ev's commit, parses the pipeline (the
// configured path, or ev.PipelinePath when set), and — if the event matches —
// records and asynchronously executes a run. Fresh workspaces are removed when
// the run finishes; under the persistent strategy the repo's reusable
// workspace is updated in place and kept (a concurrent run of the same repo
// falls back to a fresh workspace). A repo not on the allowlist returns
// ErrRepoNotAllowed (before any disk work); an invalid pipeline path and
// checkout/parse failures return an error; a non-matching event returns
// Result{Started: false} with nil error.
func (r *Runner) Trigger(ctx context.Context, ev model.Event) (Result, error) {
	if !r.allow.Allows(ev.RepoURL) {
		return Result{}, fmt.Errorf("%w: %s", ErrRepoNotAllowed, ev.RepoURL)
	}
	// Bounded admission, before any disk work: everything below — the git
	// checkout, the parse, the run pending its sem slot — consumes processes
	// and workspace directories, so it must be capped, not just execution.
	select {
	case r.admit <- struct{}{}:
	default:
		return Result{}, ErrBusy
	}
	release := func() { <-r.admit }

	pipelinePath, err := pipelineFile(r.pipelinePath, ev)
	if err != nil {
		release()
		return Result{}, err
	}
	if err := os.MkdirAll(r.wsRoot, 0o700); err != nil {
		release()
		return Result{}, err
	}

	// Workspace selection. Under the persistent strategy each repo has one
	// reusable directory, serialized by a per-repo try-lock; if another run of
	// the same repo holds it, this run falls back to a fresh per-run dir —
	// occasionally slower, never blocking the caller.
	var wsDir string
	reuse := false
	unlock := func() {}
	if r.persistent {
		if mu := r.repoLock(ev.RepoURL); mu.TryLock() {
			unlock = mu.Unlock
			reuse = true
			wsDir = filepath.Join(r.wsRoot, persistDirName(ev.RepoURL))
		}
	}
	if !reuse {
		var err error
		if wsDir, err = os.MkdirTemp(r.wsRoot, "run-*"); err != nil {
			release()
			return Result{}, err
		}
	}

	ws, err := workspace.Checkout(ctx, workspace.Options{
		Dir: wsDir, RepoURL: ev.RepoURL, SHA: ev.SHA, Ref: ev.Ref,
		Keep: r.keepWS || reuse, Reuse: reuse,
	})
	if err != nil {
		unlock()
		release()
		return Result{}, fmt.Errorf("checkout: %w", err)
	}
	// Every pre-execution exit must release the workspace, the repo lock, and
	// the admission slot — a leaked repo lock would silently wedge the repo
	// onto the fallback path, and a leaked slot would shrink capacity forever.
	abort := func() { _ = ws.Cleanup(); unlock(); release() }

	data, err := os.ReadFile(filepath.Join(ws.Dir, pipelinePath))
	if err != nil {
		abort()
		return Result{}, fmt.Errorf("read %s: %w", pipelinePath, err)
	}
	wf, err := pipeline.Parse(data)
	if err != nil {
		abort()
		return Result{}, fmt.Errorf("pipeline %s: %w", pipelinePath, err)
	}

	if !matches(wf, ev) {
		abort()
		return Result{Started: false, Reason: fmt.Sprintf("event %s on %q does not match the workflow's on:", ev.Kind, ev.Branch)}, nil
	}

	run := r.engine.NewRun(wf, ev, ws.Dir)
	if err := r.store.SaveRun(run); err != nil {
		abort()
		return Result{}, err
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer release()
		defer unlock()                      // after cleanup: dir settled before the next run can claim it
		defer func() { _ = ws.Cleanup() }() // no-op for persistent workspaces (Keep)
		// Wait for a run slot; the run stays Pending until one frees.
		r.sem <- struct{}{}
		defer func() { <-r.sem }()
		// r.ctx is cancelled on Shutdown, so in-flight runs are stopped.
		r.engine.Execute(r.ctx, run, wf, ws.Dir)
	}()

	return Result{RunID: run.ID, Started: true}, nil
}

// pipelineFile resolves the effective in-repo pipeline path for ev. Without an
// override it is def as configured. An override names a file relative to def's
// directory (default .janus/) — "release.yml", not ".janus/release.yml" — so
// only YAML deliberately placed with the pipelines is runnable and callers
// need not know where pipelines live. Absolute paths, Windows drive-relative
// paths, and `..` escapes are rejected before any disk work (the checkout
// does not exist yet).
func pipelineFile(def string, ev model.Event) (string, error) {
	base := filepath.Clean(def)
	if filepath.IsAbs(base) || filepath.VolumeName(base) != "" ||
		base == ".." || strings.HasPrefix(base, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("pipeline path %q must be a relative path inside the repository", def)
	}
	if ev.PipelinePath == "" {
		return base, nil
	}
	p := filepath.Clean(ev.PipelinePath)
	if filepath.IsAbs(p) || filepath.VolumeName(p) != "" {
		return "", fmt.Errorf("pipeline path %q must be relative to the pipeline directory", ev.PipelinePath)
	}
	dir := filepath.Dir(base)
	full := filepath.Join(dir, p)
	rel, err := filepath.Rel(dir, full)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("pipeline path %q must name a file inside %q, the pipeline directory", ev.PipelinePath, dir)
	}
	return full, nil
}

// matches reports whether the event should start the workflow. Manual triggers
// always match; push/merge_request match when the trigger is declared and the
// branch passes its filter.
func matches(wf *model.Workflow, ev model.Event) bool {
	switch ev.Kind {
	case model.EventManual:
		return true
	case model.EventPush:
		return wf.On.Push != nil && wf.On.Push.Matches(ev.Branch)
	case model.EventMergeRequest:
		return wf.On.MergeRequest != nil && wf.On.MergeRequest.Matches(ev.Branch)
	default:
		return false
	}
}

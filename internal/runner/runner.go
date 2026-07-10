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

// Runner coordinates checkout → parse → match → execute.
type Runner struct {
	store        store.Store
	engine       *engine.Engine
	wsRoot       string
	pipelinePath string
	keepWS       bool
	allow        allowlist.Allowlist

	ctx    context.Context // root context for run execution; cancelled on Shutdown
	cancel context.CancelFunc
	sem    chan struct{}  // caps concurrently executing runs
	wg     sync.WaitGroup // tracks in-flight runs for graceful shutdown
}

// Options configures a Runner.
type Options struct {
	WSRoot       string              // where per-run workspaces are created
	PipelinePath string              // in-repo path to the pipeline file (an event may override it)
	KeepWS       bool                // keep workspaces after runs (debugging)
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
		allow:        opts.Allowlist,
		ctx:          ctx,
		cancel:       cancel,
		sem:          make(chan struct{}, maxRuns),
	}
}

// Sweep removes leftover workspaces from a previous process (crash recovery).
// Call once at startup, before serving. Only directories Janus created
// (run-*) are removed.
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
// records and asynchronously executes a run. The workspace is removed when the
// run finishes. A repo not on the allowlist returns ErrRepoNotAllowed (before
// any disk work); an invalid pipeline path and checkout/parse failures return
// an error; a non-matching event returns Result{Started: false} with nil error.
func (r *Runner) Trigger(ctx context.Context, ev model.Event) (Result, error) {
	if !r.allow.Allows(ev.RepoURL) {
		return Result{}, fmt.Errorf("%w: %s", ErrRepoNotAllowed, ev.RepoURL)
	}
	pipelinePath, err := pipelineFile(r.pipelinePath, ev)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(r.wsRoot, 0o700); err != nil {
		return Result{}, err
	}
	wsDir, err := os.MkdirTemp(r.wsRoot, "run-*")
	if err != nil {
		return Result{}, err
	}
	ws, err := workspace.Checkout(ctx, workspace.Options{
		Dir: wsDir, RepoURL: ev.RepoURL, SHA: ev.SHA, Ref: ev.Ref, Keep: r.keepWS,
	})
	if err != nil {
		return Result{}, fmt.Errorf("checkout: %w", err)
	}

	data, err := os.ReadFile(filepath.Join(ws.Dir, pipelinePath))
	if err != nil {
		_ = ws.Cleanup()
		return Result{}, fmt.Errorf("read %s: %w", pipelinePath, err)
	}
	wf, err := pipeline.Parse(data)
	if err != nil {
		_ = ws.Cleanup()
		return Result{}, fmt.Errorf("pipeline %s: %w", pipelinePath, err)
	}

	if !matches(wf, ev) {
		_ = ws.Cleanup()
		return Result{Started: false, Reason: fmt.Sprintf("event %s on %q does not match the workflow's on:", ev.Kind, ev.Branch)}, nil
	}

	run := r.engine.NewRun(wf, ev, ws.Dir)
	if err := r.store.SaveRun(run); err != nil {
		_ = ws.Cleanup()
		return Result{}, err
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() { _ = ws.Cleanup() }()
		// Wait for a run slot; the run stays Pending until one frees.
		r.sem <- struct{}{}
		defer func() { <-r.sem }()
		// r.ctx is cancelled on Shutdown, so in-flight runs are stopped.
		r.engine.Execute(r.ctx, run, wf, ws.Dir)
	}()

	return Result{RunID: run.ID, Started: true}, nil
}

// pipelineFile resolves the effective in-repo pipeline path for ev — its
// override when set, otherwise def. The value must stay inside the (not yet
// created) checkout, so absolute paths, Windows drive-relative paths, and
// `..` escapes are rejected before any disk work. An override is further
// confined to def's directory (default .janus/): only YAML deliberately
// placed with the pipelines is runnable, not every committed file that
// happens to parse as one.
func pipelineFile(def string, ev model.Event) (string, error) {
	p := ev.PipelinePath
	if p == "" {
		p = def
	}
	clean := filepath.Clean(p)
	if filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" ||
		clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("pipeline path %q must be a relative path inside the repository", p)
	}
	if ev.PipelinePath != "" {
		dir := filepath.Dir(filepath.Clean(def))
		rel, err := filepath.Rel(dir, clean)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("pipeline path %q must be inside %q, the configured pipeline file's directory", p, dir)
		}
	}
	return clean, nil
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

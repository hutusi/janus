// Package runner is the coordinator between a trigger event and a pipeline run:
// it checks out the repository at the event's commit, reads and parses
// .janus/ci.yml from that checkout, matches the event against the workflow's
// `on:` filters (manual triggers always match), records a run, and executes it
// asynchronously. It is the single entry point shared by the manual trigger and
// the webhook handlers.
package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hutusi/janus/internal/engine"
	"github.com/hutusi/janus/internal/model"
	"github.com/hutusi/janus/internal/pipeline"
	"github.com/hutusi/janus/internal/store"
	"github.com/hutusi/janus/internal/workspace"
)

// Runner coordinates checkout → parse → match → execute.
type Runner struct {
	store        store.Store
	engine       *engine.Engine
	wsRoot       string
	pipelinePath string
	keepWS       bool
}

// Result reports the outcome of a trigger. Started is false (with a Reason)
// when the event did not match the workflow's `on:` filters — that is not an
// error, just a no-op.
type Result struct {
	RunID   string
	Started bool
	Reason  string
}

// New creates a Runner. wsRoot is where per-run workspaces are created;
// pipelinePath is the in-repo path to the pipeline file (e.g. .janus/ci.yml).
func New(st store.Store, eng *engine.Engine, wsRoot, pipelinePath string, keepWS bool) *Runner {
	return &Runner{store: st, engine: eng, wsRoot: wsRoot, pipelinePath: pipelinePath, keepWS: keepWS}
}

// Trigger checks out the repo at ev's commit, parses the pipeline, and — if the
// event matches — records and asynchronously executes a run. The workspace is
// removed when the run finishes. Checkout/parse failures return an error; a
// non-matching event returns Result{Started: false} with nil error.
func (r *Runner) Trigger(ctx context.Context, ev model.Event) (Result, error) {
	if err := os.MkdirAll(r.wsRoot, 0o755); err != nil {
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

	data, err := os.ReadFile(filepath.Join(ws.Dir, r.pipelinePath))
	if err != nil {
		_ = ws.Cleanup()
		return Result{}, fmt.Errorf("read %s: %w", r.pipelinePath, err)
	}
	wf, err := pipeline.Parse(data)
	if err != nil {
		_ = ws.Cleanup()
		return Result{}, fmt.Errorf("pipeline %s: %w", r.pipelinePath, err)
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

	go func() {
		defer func() { _ = ws.Cleanup() }()
		r.engine.Execute(context.Background(), run, wf, ws.Dir)
	}()

	return Result{RunID: run.ID, Started: true}, nil
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

// Package engine executes a validated pipeline: it builds the job DAG, runs
// independent jobs concurrently while honoring `needs`, runs each job's steps
// sequentially as host processes, streams combined output to the store, and is
// fail-fast — the first job failure cancels in-flight work and skips the rest.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/hutusi/janus/internal/model"
	"github.com/hutusi/janus/internal/store"
)

// Engine runs workflows against a workspace directory.
type Engine struct {
	store       store.Store
	maxJobs     int
	stepTimeout time.Duration // 0 = no per-step timeout
	tee         io.Writer     // optional: mirror all step output here (the CLI uses os.Stdout)
	logger      *slog.Logger

	// runJob executes one job's steps and returns its terminal status. It is a
	// field (not just a method) so tests can drive the scheduler with a fake.
	runJob func(ctx context.Context, rs *runState, job *model.Job, jr *model.JobRun) model.Status
}

// Option configures an Engine.
type Option func(*Engine)

// WithMaxParallelJobs caps how many jobs run concurrently (default 4).
func WithMaxParallelJobs(n int) Option {
	return func(e *Engine) {
		if n > 0 {
			e.maxJobs = n
		}
	}
}

// WithTee mirrors every step's combined output to w (used by `janus run`).
func WithTee(w io.Writer) Option {
	return func(e *Engine) { e.tee = w }
}

// WithStepTimeout fails any step that runs longer than d (0 = no timeout).
func WithStepTimeout(d time.Duration) Option {
	return func(e *Engine) {
		if d > 0 {
			e.stepTimeout = d
		}
	}
}

// WithLogger sets the logger used for non-fatal diagnostics (e.g. persistence
// errors during a run).
func WithLogger(l *slog.Logger) Option {
	return func(e *Engine) {
		if l != nil {
			e.logger = l
		}
	}
}

// New creates an Engine backed by st.
func New(st store.Store, opts ...Option) *Engine {
	e := &Engine{store: st, maxJobs: 4, logger: slog.Default()}
	e.runJob = e.executeJob
	for _, o := range opts {
		o(e)
	}
	return e
}

// runState is the shared, mutable state for a single run. All mutations to the
// run (and its persistence) go through update, which holds mu — so concurrent
// job goroutines never race on the run, and the store always serializes a
// consistent snapshot.
type runState struct {
	run     *model.Run
	wf      *model.Workflow
	event   model.Event
	workDir string
	store   store.Store

	tee    io.Writer
	teeMu  *sync.Mutex // guards tee across parallel jobs
	logger *slog.Logger

	mu sync.Mutex
}

func (rs *runState) update(mutate func()) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	mutate()
	if err := rs.store.UpdateRun(rs.run); err != nil {
		rs.logger.Warn("persisting run state failed", "run", rs.run.ID, "err", err)
	}
}

type jobResult struct {
	name   string
	status model.Status
}

// NewRun builds a pending run record for wf, with one JobRun per job and one
// StepRun per step. It does not execute or persist anything; pass it to Execute.
func (e *Engine) NewRun(wf *model.Workflow, ev model.Event, workDir string) *model.Run {
	run := &model.Run{
		ID:           newRunID(),
		WorkflowName: wf.Name,
		Event:        ev,
		Status:       model.StatusPending,
		CreatedAt:    time.Now(),
		WorkspaceDir: workDir,
	}
	names := make([]string, 0, len(wf.Jobs))
	for name := range wf.Jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		job := wf.Jobs[name]
		jr := &model.JobRun{Name: name, Needs: job.Needs, Status: model.StatusPending}
		for i, s := range job.Steps {
			jr.Steps = append(jr.Steps, &model.StepRun{Index: i, Command: s.Run, Status: model.StatusPending})
		}
		run.Jobs = append(run.Jobs, jr)
	}
	return run
}

// Run is a synchronous convenience: it creates, persists, executes, and returns
// a run for wf. Used by `janus run`.
func (e *Engine) Run(ctx context.Context, wf *model.Workflow, ev model.Event, workDir string) (*model.Run, error) {
	run := e.NewRun(wf, ev, workDir)
	if err := e.store.SaveRun(run); err != nil {
		return nil, err
	}
	e.Execute(ctx, run, wf, workDir)
	return run, nil
}

// Execute runs the scheduler over an already-saved run (built by NewRun),
// blocking until every job reaches a terminal state. Status changes are
// persisted via the store as they happen.
func (e *Engine) Execute(ctx context.Context, run *model.Run, wf *model.Workflow, workDir string) {
	rs := &runState{run: run, wf: wf, event: run.Event, workDir: workDir, store: e.store, tee: e.tee, logger: e.logger}
	if rs.tee != nil {
		rs.teeMu = &sync.Mutex{}
	}

	g, err := buildGraph(wf)
	if err != nil {
		rs.update(func() {
			run.Status = model.StatusFailed
			run.FinishedAt = time.Now()
		})
		return
	}

	byName := make(map[string]*model.JobRun, len(run.Jobs))
	for _, jr := range run.Jobs {
		byName[jr.Name] = jr
	}
	rs.update(func() {
		run.Status = model.StatusRunning
		run.StartedAt = time.Now()
	})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, e.maxJobs)
	results := make(chan jobResult)
	indeg := make(map[string]int, len(g.indegree))
	for k, v := range g.indegree {
		indeg[k] = v
	}

	running := 0
	launch := func(name string) {
		running++
		job := wf.Jobs[name]
		jr := byName[name]
		go func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			results <- jobResult{name: name, status: e.runJob(runCtx, rs, job, jr)}
		}()
	}

	for _, name := range g.order {
		if indeg[name] == 0 {
			launch(name)
		}
	}

	failed := false
	for running > 0 {
		res := <-results
		running--
		if res.status != model.StatusSuccess && !failed {
			failed = true
			cancel() // fail-fast: kill in-flight processes, stop launching new jobs
		}
		if failed {
			continue
		}
		for _, dep := range g.dependents[res.name] {
			indeg[dep]--
			if indeg[dep] == 0 {
				launch(dep)
			}
		}
	}

	rs.update(func() {
		for _, jr := range run.Jobs {
			if !jr.Status.Terminal() {
				jr.Status = model.StatusSkipped // never started due to fail-fast
			}
			for _, sr := range jr.Steps {
				if !sr.Status.Terminal() {
					sr.Status = model.StatusSkipped
				}
			}
		}
		if failed {
			run.Status = model.StatusFailed
		} else {
			run.Status = model.StatusSuccess
		}
		run.FinishedAt = time.Now()
	})
}

func newRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

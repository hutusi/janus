// Package engine executes a validated pipeline: it builds the job DAG, runs
// independent jobs concurrently while honoring `needs`, runs each job's steps
// sequentially as host processes, streams combined output to the store, and is
// fail-fast — the first job failure cancels in-flight work and skips the rest.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
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

	// degraded latches true when a run's terminal state could not be persisted
	// even after retries. The stored history is then stale, so the daemon
	// surfaces it via /healthz. Cleared only by restart.
	degraded atomic.Bool
}

// Degraded reports whether a run's final state was ever abandoned unpersisted.
func (e *Engine) Degraded() bool { return e.degraded.Load() }

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
	// Log-and-continue: a missed intermediate write self-heals on the next
	// transition, and failing the run over its own bookkeeping helps nobody.
	if err := rs.store.UpdateRun(rs.run); err != nil {
		rs.logger.Warn("persisting run state failed", "run", rs.run.ID, "err", err)
	}
}

// finalPersistAttempts / finalPersistBackoff bound the retry of a run's
// terminal write. Vars, not consts, so tests can zero the backoff.
var (
	finalPersistAttempts = 3
	finalPersistBackoff  = 250 * time.Millisecond
)

// updateFinal is update for a run's terminal transition: there is no later
// write to self-heal a miss, so a transient failure (a full or briefly
// read-only disk) would otherwise strand the stored run non-terminal forever —
// which startup reconciliation later records as cancelled even for a run that
// actually succeeded. It retries the write a few times, then gives up, logs at
// Error, and returns the error so Execute can surface it (a non-zero exit for
// `janus run`, a degraded /healthz for the daemon).
func (rs *runState) updateFinal(mutate func()) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	mutate()
	var err error
	for attempt := 1; ; attempt++ {
		if err = rs.store.UpdateRun(rs.run); err == nil {
			return nil
		}
		if attempt >= finalPersistAttempts {
			break
		}
		time.Sleep(finalPersistBackoff)
	}
	rs.logger.Error("persisting final run state failed after retries; the stored run stays stale", "run", rs.run.ID, "status", rs.run.Status, "attempts", finalPersistAttempts, "err", err)
	return err
}

type jobResult struct {
	name   string
	status model.Status
}

// NewPendingRun builds a pending "shell" run for ev: a recordable run identity
// with no workflow name or jobs yet, because the pipeline is only known after
// the checkout. PopulateRun fills it in once the workflow is parsed; until then
// the run must eventually reach a terminal state via the caller (the store
// never prunes non-terminal runs).
func (e *Engine) NewPendingRun(ev model.Event) *model.Run {
	return &model.Run{
		ID:        newRunID(),
		Event:     ev,
		Status:    model.StatusPending,
		CreatedAt: time.Now(),
	}
}

// PopulateRun fills a shell run (NewPendingRun) with wf's identity and job
// tree: one JobRun per job and one StepRun per step. Jobs whose branch filter
// does not match the run's branch — and, transitively, jobs that need one —
// are recorded skipped up front; the rest start pending.
func (e *Engine) PopulateRun(run *model.Run, wf *model.Workflow, workDir string) {
	run.WorkflowName = wf.Name
	run.WorkspaceDir = workDir
	skip := branchSkipped(wf, run.Event.Branch)
	names := make([]string, 0, len(wf.Jobs))
	for name := range wf.Jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		job := wf.Jobs[name]
		status := model.StatusPending
		if skip[name] {
			status = model.StatusSkipped
		}
		jr := &model.JobRun{Name: name, Needs: job.Needs, Status: status}
		for i, s := range job.Steps {
			jr.Steps = append(jr.Steps, &model.StepRun{Index: i, Command: s.Run, Status: status})
		}
		run.Jobs = append(run.Jobs, jr)
	}
}

// branchSkipped returns the set of jobs excluded from a run on branch: jobs
// whose Filter does not match, plus — transitively — jobs that need an
// excluded job (a job must not run when the work it depends on didn't).
func branchSkipped(wf *model.Workflow, branch string) map[string]bool {
	skip := make(map[string]bool)
	for name, job := range wf.Jobs {
		if job.Filter != nil && !job.Filter.Matches(branch) {
			skip[name] = true
		}
	}
	for changed := len(skip) > 0; changed; {
		changed = false
		for name, job := range wf.Jobs {
			if skip[name] {
				continue
			}
			for _, dep := range job.Needs {
				if skip[dep] {
					skip[name] = true
					changed = true
					break
				}
			}
		}
	}
	return skip
}

// NewRun builds a pending run record for wf, with one JobRun per job and one
// StepRun per step. It does not execute or persist anything; pass it to Execute.
func (e *Engine) NewRun(wf *model.Workflow, ev model.Event, workDir string) *model.Run {
	run := e.NewPendingRun(ev)
	e.PopulateRun(run, wf, workDir)
	return run
}

// Run is a synchronous convenience: it creates, persists, executes, and returns
// a run for wf. Used by `janus run`. A non-nil error means the steps ran but
// the run's terminal state could not be persisted (see Execute).
func (e *Engine) Run(ctx context.Context, wf *model.Workflow, ev model.Event, workDir string) (*model.Run, error) {
	run := e.NewRun(wf, ev, workDir)
	if err := e.store.SaveRun(run); err != nil {
		return nil, err
	}
	return run, e.Execute(ctx, run, wf, workDir)
}

// Execute runs the scheduler over an already-saved run (built by NewRun),
// blocking until every job reaches a terminal state. Status changes are
// persisted via the store as they happen. It returns a non-nil error only when
// the run's *terminal* state could not be persisted after retries — a run that
// fails on its own merits (failing steps) returns nil. Such a persist failure
// also latches the engine Degraded().
func (e *Engine) Execute(ctx context.Context, run *model.Run, wf *model.Workflow, workDir string) error {
	rs := &runState{run: run, wf: wf, event: run.Event, workDir: workDir, store: e.store, tee: e.tee, logger: e.logger}
	if rs.tee != nil {
		rs.teeMu = &sync.Mutex{}
	}

	g, err := buildGraph(wf)
	if err != nil {
		return e.finalize(rs, func() {
			run.Status = model.StatusFailed
			run.FinishedAt = time.Now()
		})
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
	// Jobs already terminal were branch-skipped by PopulateRun; they are never
	// launched, and neither are their dependents (the skip is transitive), so
	// the remaining sub-DAG is closed over runnable jobs.
	launchable := func(name string) bool {
		jr := byName[name]
		return jr != nil && !jr.Status.Terminal()
	}

	for _, name := range g.order {
		if indeg[name] == 0 && launchable(name) {
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
			if indeg[dep] == 0 && launchable(dep) {
				launch(dep)
			}
		}
	}

	return e.finalize(rs, func() {
		allSkipped := len(run.Jobs) > 0
		for _, jr := range run.Jobs {
			if !jr.Status.Terminal() {
				jr.Status = model.StatusSkipped // never started due to fail-fast
			}
			if jr.Status != model.StatusSkipped {
				allSkipped = false
			}
			for _, sr := range jr.Steps {
				if !sr.Status.Terminal() {
					sr.Status = model.StatusSkipped
				}
			}
		}
		switch {
		case failed && ctx.Err() != nil:
			// The parent context was cancelled from outside (shutdown,
			// Ctrl-C): the run was interrupted, not failed on its own merits.
			// Fail-fast sibling cancellation only cancels runCtx, so an
			// ordinary failing run still reports failed here.
			run.Status = model.StatusCancelled
		case failed:
			run.Status = model.StatusFailed
		case allSkipped:
			// Every job was excluded by its branch filter: nothing executed,
			// which is a skip, not a success.
			run.Status = model.StatusSkipped
			if run.Reason == "" {
				run.Reason = fmt.Sprintf("no job matches branch %q", rs.event.Branch)
			}
		default:
			run.Status = model.StatusSuccess
		}
		run.FinishedAt = time.Now()
	})
}

// finalize applies the terminal mutation and, if persisting it ultimately
// fails, latches the engine Degraded() and returns the error so callers can
// surface it. The run itself has completed either way.
func (e *Engine) finalize(rs *runState, mutate func()) error {
	if err := rs.updateFinal(mutate); err != nil {
		e.degraded.Store(true)
		return err
	}
	return nil
}

func newRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

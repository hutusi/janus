// Package runner is the coordinator between a trigger event and a pipeline
// run: it validates the event, records a pending run, and then — in the
// background, so callers can answer their client before any git work — checks
// out the repository at the event's commit, reads and parses the pipeline file
// from that checkout (the configured path, or the event's per-trigger
// override), matches the event against the workflow's `on:` filters (manual
// triggers always match), and executes the run. Pre-execution outcomes
// (checkout/parse failure, non-matching event) are recorded on the run. It is
// the single entry point shared by the manual trigger and the webhook
// handlers.
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// ErrStoreUnavailable is returned by Trigger when the store cannot record the
// run (a full/read-only data dir). Handlers map it to HTTP 503 + Retry-After —
// like ErrBusy, it is Janus's problem, not the repository's, so the event must
// be retried rather than acknowledged and dropped.
var ErrStoreUnavailable = errors.New("store unavailable")

// Runner coordinates checkout → parse → match → execute.
type Runner struct {
	store        store.Store
	engine       *engine.Engine
	wsRoot       string
	pipelinePath string
	keepWS       bool
	strategy     string
	historyLimit int
	allow        allowlist.Allowlist
	logger       *slog.Logger
	notifier     Notifier

	ctx    context.Context // root context for run execution and checkout; cancelled on Shutdown
	cancel context.CancelFunc
	sem    chan struct{}  // caps concurrently executing runs
	admit  chan struct{}  // caps the whole trigger lifecycle: checkout + parse + pending queue
	wg     sync.WaitGroup // tracks admitted triggers (checkout onward) for graceful shutdown

	admitMu sync.Mutex // guards closing and serializes admission against Shutdown
	closing bool       // once set (by Shutdown), no new triggers are admitted

	degraded atomic.Bool // latched on a startup failure (e.g. an unwritable store); surfaced via /healthz

	locksMu sync.Mutex             // guards locks
	locks   map[string]*sync.Mutex // per-repo workspace locks (persistent strategy)

	// cancels maps a run ID to the cancel func of its per-run context, from
	// registration (before the pending run is saved, so any non-terminal run
	// fetchable from the store has an entry) until the run settles. Cancel and
	// the concurrency-group supersede path both act through it.
	cancelsMu sync.Mutex
	cancels   map[string]context.CancelCauseFunc

	groups *groupReg // concurrency-group membership and trigger-order accounting (see groupReg)
}

// Workspace strategies (Options.Strategy). An empty string means fresh.
const (
	StrategyFresh      = "fresh"      // new directory per run, shallow fetch, removed after
	StrategyPersistent = "persistent" // one reusable workspace per repo, updated in place
	StrategyMirror     = "mirror"     // per-repo bare mirror cache; pristine per-run checkouts materialized locally
)

// Notifier announces a finished run to the outside world (implemented by
// *notify.Notifier). It is defined here — rather than runner importing notify —
// so the packages stay decoupled: runner depends only on model. A nil Notifier
// disables notifications. Notify must not block or fail the run.
type Notifier interface {
	Notify(run *model.Run)
}

// Options configures a Runner.
type Options struct {
	WSRoot       string              // where per-run workspaces are created
	PipelinePath string              // in-repo path to the pipeline file (an event may override it)
	KeepWS       bool                // keep workspaces after runs (debugging)
	Strategy     string              // workspace strategy (Strategy* consts; "" = fresh)
	MaxRuns      int                 // max concurrent runs (<=0 means 4)
	HistoryLimit int                 // max terminal runs to retain (<=0 = unlimited); pruned after each run
	Allowlist    allowlist.Allowlist // repos permitted to run (empty denies all)
	Logger       *slog.Logger        // for background events (prune failures); defaults to slog.Default()
	Notifier     Notifier            // announces finished runs; nil disables notifications
}

// Result reports an accepted trigger: the recorded run's ID. The run is
// pending at this point; its outcome (including checkout/parse failures and
// non-matching events) is recorded on the run itself.
type Result struct {
	RunID string
}

// New creates a Runner from opts.
func New(st store.Store, eng *engine.Engine, opts Options) *Runner {
	maxRuns := opts.MaxRuns
	if maxRuns <= 0 {
		maxRuns = 4
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{
		store:        st,
		engine:       eng,
		wsRoot:       opts.WSRoot,
		pipelinePath: opts.PipelinePath,
		keepWS:       opts.KeepWS,
		strategy:     opts.Strategy,
		historyLimit: opts.HistoryLimit,
		allow:        opts.Allowlist,
		logger:       logger,
		notifier:     opts.Notifier,
		ctx:          ctx,
		cancel:       cancel,
		sem:          make(chan struct{}, maxRuns),
		// maxRuns executing plus a 3×maxRuns pending backlog. Derived rather
		// than configurable: the point is that a trigger burst cannot start
		// unbounded git processes or workspaces, not the exact queue depth.
		admit:   make(chan struct{}, 4*maxRuns),
		locks:   make(map[string]*sync.Mutex),
		cancels: make(map[string]context.CancelCauseFunc),
		groups:  newGroupReg(),
	}
}

// maxGroupExpanded bounds the expanded concurrency-group string — it becomes
// a registry key held for the run's lifetime and is stored on the run record.
const maxGroupExpanded = 1 << 10

// expandGroup materializes wf's concurrency group for ev: the group template
// interpolated with the event and the workflow-level env (resolved verbatim in
// a single pass — job/step env does not exist yet, and the group must be
// deterministic at admission time), or the implicit "<name>-<branch|ref>"
// group when the template is empty. sha/short_sha never appear: validation
// rejects them in group templates.
func expandGroup(wf *model.Workflow, ev model.Event) (string, error) {
	tpl := strings.TrimSpace(wf.Concurrency.Group)
	if tpl == "" {
		target := ev.Branch
		if target == "" {
			target = ev.Ref
		}
		return wf.Name + "-" + target, nil
	}
	ictx := pipeline.Context{
		Env:    wf.Env,
		Ref:    ev.Ref,
		Branch: ev.Branch,
		Event:  string(ev.Kind),
	}
	return ictx.Interpolate(tpl, maxGroupExpanded)
}

// Cancel requests cancellation of a non-terminal run, recording reason as its
// stored Reason. It reports whether the run was known (still registered); a
// false return means the ID is unknown or the run already settled. Cancelling
// is asynchronous and best-effort: a run whose last process exits before the
// cancel is observed still finishes on its own terms, and repeated calls are
// no-ops.
func (r *Runner) Cancel(runID, reason string) bool {
	r.cancelsMu.Lock()
	cancel, ok := r.cancels[runID]
	r.cancelsMu.Unlock()
	if !ok {
		return false
	}
	cancel(errors.New(reason))
	return true
}

func (r *Runner) registerCancel(runID string, cancel context.CancelCauseFunc) {
	r.cancelsMu.Lock()
	r.cancels[runID] = cancel
	r.cancelsMu.Unlock()
}

func (r *Runner) unregisterCancel(runID string) {
	r.cancelsMu.Lock()
	delete(r.cancels, runID)
	r.cancelsMu.Unlock()
}

// cancelReason prefers the per-run cancel cause ("cancelled via API",
// "superseded by run X") over fallback. A plain context.Canceled — shutdown
// cancelling the root context — carries no message worth storing, so the
// fallback stands.
func cancelReason(ctx context.Context, fallback string) string {
	if c := context.Cause(ctx); c != nil && !errors.Is(c, context.Canceled) {
		return c.Error()
	}
	return fallback
}

// Degraded reports whether the daemon is in a known-bad state the operator
// should act on: the engine abandoned a run's terminal state unpersisted, or a
// startup step (e.g. reconciling interrupted runs) failed against an
// unwritable store. The daemon surfaces it via /healthz. A restart clears the
// startup latch, but if storage is still broken the failing startup step
// re-latches it, so a restart cannot falsely report healthy.
func (r *Runner) Degraded() bool { return r.degraded.Load() || r.engine.Degraded() }

// MarkDegraded latches the runner into a degraded state (see Degraded).
func (r *Runner) MarkDegraded() { r.degraded.Store(true) }

// pruneHistory enforces the retention cap (a no-op when unset), logging but
// not failing on error — pruning is housekeeping, not part of the run.
func (r *Runner) pruneHistory() {
	if r.historyLimit <= 0 {
		return
	}
	if n, err := r.store.Prune(r.historyLimit); err != nil {
		r.logger.Warn("pruning run history failed", "err", err)
	} else if n > 0 {
		r.logger.Info("pruned old runs beyond history_limit", "removed", n, "keep", r.historyLimit)
	}
}

// repoLock returns the mutex serializing repoURL's shared on-disk state: its
// persistent workspace, or its bare mirror's fetches.
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

// mirrorDirName is the stable directory name for a repo's bare mirror, with
// the same keying rationale as persistDirName.
func mirrorDirName(repoURL string) string {
	sum := sha256.Sum256([]byte(repoURL))
	return "mirror-" + hex.EncodeToString(sum[:8])
}

// Sweep removes leftover workspaces from a previous process (crash recovery).
// Call once at startup, before serving. Only per-run directories (run-*) are
// removed; per-repo caches — persistent workspaces (persist-*) and bare
// mirrors (mirror-*) — deliberately survive restarts: their caches are the
// point, and a mirror interrupted mid-creation self-heals on next use.
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
	// Scan compact summaries (bounded memory), then load only the non-terminal
	// runs — the few a crash left mid-flight — one full record at a time.
	summaries, err := r.store.ListRuns(0, 0)
	if err != nil {
		return 0, err
	}
	var repaired int
	var firstErr error
	now := time.Now()
	for _, s := range summaries {
		if s.Status.Terminal() {
			continue
		}
		run, err := r.store.GetRun(s.ID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// The summary is a listing cache and can lag behind run.json (the
		// source of truth). If the full record is actually terminal, the
		// summary was stale — do NOT repair (that would overwrite a finished
		// run); rewrite it to heal the sidecar and move on.
		if run.Status.Terminal() {
			_ = r.store.UpdateRun(run)
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

// admitOne reserves an admission slot and registers the trigger with the
// shutdown WaitGroup, atomically with respect to Shutdown. It returns ErrBusy
// if the runner is closing or the queue is full. The returned release must be
// called exactly once on every exit path — it frees the slot and the wg count,
// replacing both `<-r.admit` and `wg.Done()`. Acquiring wg here (not after
// checkout+parse) means Shutdown waits for in-flight checkouts too, and the
// admitMu-guarded closing check gives a happens-before with Shutdown so no
// wg.Add can ever race wg.Wait.
func (r *Runner) admitOne() (release func(), err error) {
	r.admitMu.Lock()
	defer r.admitMu.Unlock()
	if r.closing {
		return nil, ErrBusy
	}
	select {
	case r.admit <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	r.wg.Add(1)
	var once sync.Once
	return func() { once.Do(func() { <-r.admit; r.wg.Done() }) }, nil
}

// Shutdown stops accepting new triggers and waits up to grace for admitted
// work (checkout onward) to finish; if it doesn't, it cancels them (killing
// their host processes) and waits for the unwind. Call after the HTTP listener
// is closed.
func (r *Runner) Shutdown(grace time.Duration) {
	r.admitMu.Lock()
	r.closing = true
	r.admitMu.Unlock()

	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		r.cancel()
	case <-time.After(grace):
		r.cancel() // grace expired: cancel in-flight work, then wait for unwind
		<-done
	}
}

// checkoutTimeout bounds a single checkout so a stalled or malicious git
// server cannot pin an admission slot (and its run stuck pending)
// indefinitely. It is a var (not a const) only so tests can shrink it; 10m
// clears a large shallow clone with margin.
var checkoutTimeout = 10 * time.Minute

// changedFilesTimeout bounds the extra fetch+diff behind path filters — a
// single-commit fetch, far smaller than a checkout. On expiry the filters
// fail open.
var changedFilesTimeout = 2 * time.Minute

// Event-field length caps. These values come from the webhook body (up to
// 5 MiB) or the manual API, and flow into the stored run, the unauthenticated
// dashboard, and interpolation (${{ branch }}, ${{ ref }}); bound them at the
// single entry point so none of those can be amplified. Generous — real values
// are tens of bytes.
const (
	maxRepoURLLen      = 2 << 10
	maxRefLen          = 512
	maxBranchLen       = 512
	maxPipelinePathLen = 512
	maxTitleLen        = 4 << 10 // commit/MR title, display only
	maxBeforeLen       = 64      // hex commit id (sha1 or sha256)
)

// validateEvent rejects over-long event fields before any disk work.
func validateEvent(ev model.Event) error {
	for _, f := range []struct {
		name  string
		value string
		max   int
	}{
		{"repo_url", ev.RepoURL, maxRepoURLLen},
		{"ref", ev.Ref, maxRefLen},
		{"branch", ev.Branch, maxBranchLen},
		{"pipeline_path", ev.PipelinePath, maxPipelinePathLen},
		{"title", ev.Title, maxTitleLen},
		{"before", ev.Before, maxBeforeLen},
	} {
		if len(f.value) > f.max {
			return fmt.Errorf("%s is too long: %d bytes (max %d)", f.name, len(f.value), f.max)
		}
	}
	return nil
}

// Trigger validates ev, records a pending run for it, and returns — the
// checkout, pipeline parse, `on:` match, and execution all happen in the
// background, with the outcome recorded on the run (StatusFailed with a Reason
// for checkout/parse failures, StatusSkipped for a non-matching event).
// Callers can answer their client immediately with the run ID; webhook
// platforms with short delivery timeouts never race the checkout.
//
// Synchronous, so still HTTP-mappable: ErrRepoNotAllowed (403, before any
// other work), over-long event fields and an invalid pipeline path (400), the
// admission cap (ErrBusy, 503), and recording the pending run
// (ErrStoreUnavailable, 503 — the event must be retried, not acknowledged and
// dropped).
func (r *Runner) Trigger(ev model.Event) (Result, error) {
	if !r.allow.Allows(ev.RepoURL) {
		return Result{}, fmt.Errorf("%w: %s", ErrRepoNotAllowed, ev.RepoURL)
	}
	if err := validateEvent(ev); err != nil {
		return Result{}, err
	}
	pipelinePath, err := pipelineFile(r.pipelinePath, ev)
	if err != nil {
		return Result{}, err
	}
	// Bounded admission, before any disk work: everything in the background —
	// the git checkout, the parse, the run pending its sem slot — consumes
	// processes and workspace directories, so it must be capped, not just
	// execution. admitOne also enrolls in the shutdown WaitGroup, so a checkout
	// still in flight when Shutdown begins is waited on (and then cancelled via
	// r.ctx).
	release, err := r.admitOne()
	if err != nil {
		return Result{}, err
	}

	run := r.engine.NewPendingRun(ev)
	// The trigger-order stamp is taken here, in the synchronous path, so
	// concurrency-group ordering reflects when triggers arrived — not when
	// their checkouts happen to finish (see groupReg).
	seq := r.groups.beginTrigger()
	// The per-run context makes every wait and the execution itself
	// individually cancellable (Cancel, a concurrency-group supersede) while
	// still inheriting Shutdown's r.ctx. Register before SaveRun so any
	// non-terminal run fetchable from the store already has a cancel entry.
	runCtx, cancelRun := context.WithCancelCause(r.ctx)
	r.registerCancel(run.ID, cancelRun)
	// Until the background goroutine owns them, tear the acquired resources
	// down on ANY exit — the SaveRun error below, or a panic (net/http
	// recovers handler panics, so a leak here would otherwise pin the
	// admission slot and the trigger-order accounting forever). Each teardown
	// is idempotent.
	handedOff := false
	defer func() {
		if !handedOff {
			r.unregisterCancel(run.ID)
			cancelRun(nil)
			r.groups.endTrigger(seq)
			release()
		}
	}()
	if err := r.store.SaveRun(run); err != nil {
		// The store rejected recording a new run (a full/read-only/permission
		// problem for the local file store — persistent, not transient), so
		// the daemon can't do its job: latch degraded for /healthz, and return
		// a typed error so both handlers answer 503 + Retry-After.
		r.MarkDegraded()
		return Result{}, fmt.Errorf("%w: %v", ErrStoreUnavailable, err)
	}

	go r.runTrigger(runCtx, cancelRun, seq, run, ev, pipelinePath, release)
	handedOff = true
	return Result{RunID: run.ID}, nil
}

// runTrigger is the background half of Trigger: checkout → parse → match →
// execute for an already-recorded pending run. It holds the admission slot
// (release) for its whole span so the 4×cap and shutdown draining keep
// covering in-flight checkouts, and it must leave the run in a terminal state
// on every path — the store never prunes non-terminal runs, so an unsettled
// run would leak forever.
func (r *Runner) runTrigger(runCtx context.Context, cancelRun context.CancelCauseFunc, seq uint64, run *model.Run, ev model.Event, pipelinePath string, release func()) {
	defer release()
	// Announce the finished run, exactly once, on every terminal path. Placement
	// is deliberate: registered right after release() so (LIFO) it runs just
	// *before* it — after every other defer below has settled the run to a
	// terminal state and persisted it (via the finish closure or Execute), but
	// before release() calls wg.Done(). That ordering makes the notifier's
	// synchronous hand-off happen-before Shutdown's wg.Wait() returns, which is
	// what lets Notifier.Close drain in-flight deliveries at shutdown. By now
	// run is single-owner (Execute's goroutines have joined), so reading it is
	// race-free. A nil notifier — or the (never-reached) non-terminal case — is a
	// no-op. Notify never blocks or fails the run.
	defer func() {
		if r.notifier != nil && run.Status.Terminal() {
			r.notifier.Notify(run)
		}
	}()
	// Idempotent safety net for the pre-enter exits (workspace/checkout/parse
	// failures, non-matching events, panics): grouped triggers actually
	// resolve inside enter, and ungrouped ones right after parse — a trigger
	// must not stay "unresolved" for its whole (unboundedly long) run, or it
	// would pin every newer group mark in memory.
	defer r.groups.endTrigger(seq)
	defer cancelRun(nil) // release the context's resources on every path
	// Deregister only after the terminal state is recorded (defers run LIFO),
	// so Cancel never observes a registered-but-unsettled gap; a Cancel racing
	// completion is a harmless no-op on the already-cancelled context.
	defer r.unregisterCancel(run.ID)
	// Terminal-state net: any exit that has not settled the run — including a
	// panic unwinding through this frame — records a failure first. No recover:
	// a panic still crashes the daemon as before, but not with a forever-
	// pending run behind it.
	settled := false
	defer func() {
		if !settled {
			r.finishRun(run, model.StatusFailed, "internal error: trigger aborted before recording an outcome")
		}
	}()
	finish := func(status model.Status, reason string) {
		settled = true
		r.finishRun(run, status, reason)
		r.pruneHistory()
	}

	if err := os.MkdirAll(r.wsRoot, 0o700); err != nil {
		finish(model.StatusFailed, fmt.Sprintf("workspace root: %v", err))
		return
	}

	// Workspace selection. Under the persistent strategy each repo has one
	// reusable directory, serialized by a per-repo try-lock; if another run of
	// the same repo holds it, this run falls back to a fresh per-run dir —
	// occasionally slower, never blocking. Under the mirror strategy every run
	// gets a fresh dir, materialized from a per-repo bare mirror when one can
	// be synced; the same try-lock serializes only the sync — materialization
	// runs unlocked and may overlap a later run's fetch, which is safe because
	// mirror files only ever appear via atomic rename and compaction runs
	// only under this same lock (see mirrorCheckout) — and on contention or
	// any sync failure the run proceeds on the direct from-remote path: the
	// mirror is an accelerator, never a gate.
	var wsDir string
	reuse := false
	unlock := func() {}
	var mirrorDir, mirrorSHA string
	switch r.strategy {
	case StrategyPersistent:
		if mu := r.repoLock(ev.RepoURL); mu.TryLock() {
			unlock = mu.Unlock
			reuse = true
			wsDir = filepath.Join(r.wsRoot, persistDirName(ev.RepoURL))
		}
	case StrategyMirror:
		if mu := r.repoLock(ev.RepoURL); mu.TryLock() {
			dir := filepath.Join(r.wsRoot, mirrorDirName(ev.RepoURL))
			sctx, scancel := context.WithTimeout(runCtx, checkoutTimeout)
			head, err := workspace.SyncMirror(sctx, dir, ev.RepoURL, ev.SHA, ev.Ref)
			scancel()
			mu.Unlock()
			if err != nil {
				// Both fields redacted: the URL may embed credentials, and the
				// error echoes the URL through git's argument list and stderr.
				r.logger.Warn("mirror sync failed; using direct checkout", "repo", model.RedactURL(ev.RepoURL), "err", model.RedactURL(err.Error()))
			} else {
				mirrorDir, mirrorSHA = dir, head
			}
		}
	}
	if !reuse {
		var err error
		if wsDir, err = os.MkdirTemp(r.wsRoot, "run-*"); err != nil {
			finish(model.StatusFailed, fmt.Sprintf("workspace: %v", err))
			return
		}
	}

	// The checkout is bounded by its deadline or the per-run context (which
	// Shutdown, Cancel, and a group supersede all cancel) — never by the
	// originating HTTP request: the caller already answered its client, and a
	// webhook platform hanging up must not cancel work it was just told is
	// underway.
	opt := workspace.Options{
		Dir: wsDir, RepoURL: ev.RepoURL, SHA: ev.SHA, Ref: ev.Ref,
		Keep: r.keepWS || reuse, Reuse: reuse,
	}
	if mirrorDir != "" {
		opt.MirrorDir, opt.SHA = mirrorDir, mirrorSHA
	}
	cctx, cancel := context.WithTimeout(runCtx, checkoutTimeout)
	ws, err := workspace.Checkout(cctx, opt)
	cancel()
	if err != nil && mirrorDir != "" && runCtx.Err() == nil {
		// Materializing from the mirror failed (e.g. it was damaged between
		// sync and clone): retry once directly from the remote under a fresh
		// deadline, so a broken mirror never fails a run the direct path
		// would have served. The dir may hold a partial clone — clear it so
		// the direct checkout starts from an empty directory.
		r.logger.Warn("mirror checkout failed; retrying with direct checkout", "repo", model.RedactURL(ev.RepoURL), "err", model.RedactURL(err.Error()))
		_ = os.RemoveAll(wsDir)
		opt.MirrorDir, opt.SHA = "", ev.SHA
		cctx, cancel = context.WithTimeout(runCtx, checkoutTimeout)
		ws, err = workspace.Checkout(cctx, opt)
		cancel()
	}
	if err != nil {
		// MkdirTemp created wsDir before Checkout validated the target, so a
		// validation (or any pre-workspace) failure would otherwise leave an
		// empty run-* dir behind. A persistent dir self-heals, so never nuke it.
		if !reuse {
			_ = os.RemoveAll(wsDir)
		}
		unlock()
		if runCtx.Err() != nil {
			// Shutdown or a cancel killed the checkout — the trigger was
			// interrupted, it did not fail.
			finish(model.StatusCancelled, cancelReason(runCtx, fmt.Sprintf("checkout: %v", err)))
		} else {
			finish(model.StatusFailed, fmt.Sprintf("checkout: %v", err))
		}
		return
	}
	// Pin the run's metadata to the exact commit that will run: verifyHEAD
	// resolved the full 40-char SHA, so ${{ sha }} / JANUS_SHA are correct even
	// for an abbreviated or ref-only trigger. Execute reads run.Event, so the
	// run record gets the resolved SHA too.
	if ws.Head != "" {
		ev.SHA = ws.Head
		run.Event.SHA = ws.Head
	}
	// Every pre-execution exit must release the workspace and the repo lock —
	// a leaked repo lock would silently wedge the repo onto the fallback path.
	abort := func() { _ = ws.Cleanup(); unlock() }

	data, err := pipeline.ReadFile(filepath.Join(ws.Dir, pipelinePath))
	if err != nil {
		abort()
		finish(model.StatusFailed, fmt.Sprintf("read %s: %v", pipelinePath, err))
		return
	}
	wf, err := pipeline.Parse(data)
	if err != nil {
		abort()
		finish(model.StatusFailed, fmt.Sprintf("pipeline %s: %v", pipelinePath, err))
		return
	}

	if !matches(wf, ev) {
		abort()
		finish(model.StatusSkipped, fmt.Sprintf("event %s on %q does not match the workflow's on:", ev.Kind, ev.Branch))
		return
	}

	// Computed once, only when a path filter will consume it; Known stays
	// false on any failure so filters fail open (see model.ChangedFiles).
	changed := r.changedFiles(runCtx, ws, wf, ev, run.ID)
	if ev.Kind == model.EventPush && wf.On.Push.Paths != nil && changed.Known && !wf.On.Push.Paths.Matches(changed.Files) {
		abort()
		finish(model.StatusSkipped, fmt.Sprintf("push to %q changed no files matching the on.push path filter", ev.Branch))
		return
	}

	// The concurrency group is expanded before the populated-run update so a
	// queued run already displays it. The group is only knowable here — the
	// pipeline lives in the checkout — so admission (ErrBusy/503) behavior is
	// unchanged, and a superseded run has still consumed an admission slot for
	// its lifetime.
	var groupKey string
	if wf.Concurrency != nil {
		group, err := expandGroup(wf, ev)
		if err != nil {
			abort()
			finish(model.StatusFailed, fmt.Sprintf("concurrency.group: %v", err))
			return
		}
		run.ConcurrencyGroup = group
		// NUL joins the repo scope to the group: it cannot appear in either
		// part, so distinct (repo, group) pairs never collide.
		groupKey = ev.RepoURL + "\x00" + group
	} else {
		// Resolved ungrouped: this trigger will never enter a group, so its
		// trigger-order seq retires now — not when the (possibly very long)
		// run finishes.
		r.groups.endTrigger(seq)
	}

	r.engine.PopulateRun(run, wf, ws.Dir, changed)
	if err := r.store.UpdateRun(run); err != nil {
		// Not fatal: the run executes anyway, and the engine persists on every
		// status change (and latches Degraded if the terminal write fails too).
		r.logger.Warn("populated run could not be persisted; executing anyway", "run_id", run.ID, "err", err)
	}
	defer unlock()                      // after cleanup: dir settled before the next run can claim it
	defer func() { _ = ws.Cleanup() }() // no-op for persistent workspaces (Keep)

	// Group gate, ordered before the global run slot: a member waiting for its
	// group holds no slot, so a busy group cannot starve unrelated repos.
	var member *groupMember
	if groupKey != "" {
		member = r.groups.enter(runCtx, groupKey, seq, run.ID, cancelRun, wf.Concurrency.CancelInProgress)
		defer r.groups.leave(groupKey, member)
		select {
		case <-member.ready:
		case <-runCtx.Done():
			finish(model.StatusCancelled, cancelReason(runCtx, "cancelled while queued for its concurrency group"))
			return
		}
	}

	// Wait for a run slot; the run stays Pending until one frees, and a cancel
	// (Cancel, group supersede, Shutdown) releases it from the queue.
	select {
	case r.sem <- struct{}{}:
	case <-runCtx.Done():
		finish(model.StatusCancelled, cancelReason(runCtx, "cancelled while waiting for a run slot"))
		return
	}
	defer func() { <-r.sem }()
	// Re-check the group under its lock: a supersede can land between the run
	// slot acquire and here.
	if member != nil && !r.groups.claim(groupKey, member, runCtx) {
		finish(model.StatusCancelled, cancelReason(runCtx, "cancelled while queued for its concurrency group"))
		return
	}
	// From here Execute owns the run's terminal state (and reconciliation
	// covers a crash), so the terminal-state net must stand down.
	settled = true
	// The per-run context is cancelled on Shutdown too, so in-flight runs are
	// stopped. A terminal-persist failure is already logged and latched
	// (Degraded()) by the engine, so the returned error is intentionally
	// discarded here.
	_ = r.engine.Execute(runCtx, run, wf, ws.Dir)
	// Execute classifies an externally-cancelled run but cannot know why; the
	// cause is only attachable now that its goroutines have joined and the run
	// is single-owner again (writing Reason mid-flight would race runState).
	if run.Status == model.StatusCancelled && run.Reason == "" {
		if reason := cancelReason(runCtx, ""); reason != "" {
			run.Reason = model.RedactURL(reason) // keep the "stored reasons are redacted" invariant
			if err := r.store.UpdateRun(run); err != nil {
				r.logger.Warn("cancel reason could not be persisted", "run_id", run.ID, "err", err)
			}
		}
	}
	r.pruneHistory()
}

// maxReasonLen bounds Run.Reason at ingestion — git stderr flows into it.
const maxReasonLen = 4 << 10

// finishRun records a terminal pre-execution outcome on run. By now the
// trigger's HTTP response is long gone, so a store failure cannot surface as
// an error to anyone — log it and latch degraded for /healthz; startup
// reconciliation is the backstop that eventually settles the stored record.
func (r *Runner) finishRun(run *model.Run, status model.Status, reason string) {
	// A checkout/workspace failure echoes the git command — including a
	// credential-bearing clone URL — into the reason, which then surfaces on the
	// (unauthenticated) dashboard, the API, and notifications. Redact before the
	// length cap so a truncation can't strand a partial credential. A no-op on
	// non-URL reasons.
	reason = model.RedactURL(reason)
	if len(reason) > maxReasonLen {
		reason = reason[:maxReasonLen]
	}
	run.Status = status
	run.Reason = reason
	run.FinishedAt = time.Now()
	// A cancelled run may already be populated (cancelled while queued for a
	// run slot or behind its concurrency group); settle its never-started
	// jobs/steps so a terminal run never keeps pending jobs.
	for _, jr := range run.Jobs {
		for _, sr := range jr.Steps {
			if !sr.Status.Terminal() {
				sr.Status = model.StatusSkipped
			}
		}
		if !jr.Status.Terminal() {
			jr.Status = model.StatusSkipped
		}
	}
	if err := r.store.UpdateRun(run); err != nil {
		r.logger.Error("run outcome could not be persisted", "run_id", run.ID, "status", status, "err", err)
		r.MarkDegraded()
	}
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

// changedFiles computes the push's changed-file set when a path filter will
// consume it. The zero value (unknown) on any failure — no base commit, a
// diff error — leaves every path filter inert: a filter must never wrongly
// skip CI, so all failure paths run the pipeline and log why.
func (r *Runner) changedFiles(ctx context.Context, ws *workspace.Workspace, wf *model.Workflow, ev model.Event, runID string) model.ChangedFiles {
	if ev.Kind != model.EventPush || !hasPathFilters(wf) {
		return model.ChangedFiles{}
	}
	if ev.Before == "" {
		r.logger.Info("path filters fail open: push has no base commit", "run_id", runID)
		return model.ChangedFiles{}
	}
	fctx, cancel := context.WithTimeout(ctx, changedFilesTimeout)
	defer cancel()
	files, err := ws.ChangedFiles(fctx, ev.Before)
	if err != nil {
		r.logger.Warn("path filters fail open: changed files unknown", "run_id", runID, "before", ev.Before, "err", err)
		return model.ChangedFiles{}
	}
	return model.ChangedFiles{Known: true, Files: files}
}

// hasPathFilters reports whether wf declares any paths/paths-ignore key, so
// pushes to filterless workflows never pay the extra fetch and diff.
func hasPathFilters(wf *model.Workflow) bool {
	if wf.On.Push != nil && wf.On.Push.Paths != nil {
		return true
	}
	for _, job := range wf.Jobs {
		if job.PathFilter != nil {
			return true
		}
	}
	return false
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

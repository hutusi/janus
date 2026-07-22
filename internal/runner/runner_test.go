package runner

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hutusi/janus/internal/allowlist"
	"github.com/hutusi/janus/internal/engine"
	"github.com/hutusi/janus/internal/model"
	"github.com/hutusi/janus/internal/store"
)

// initGitRepo creates a repo containing .janus/ci.yml and returns its path +
// HEAD SHA. (Small enough that runner, workspace, and server tests each keep
// their own copy rather than sharing a test utility package.)
func initGitRepo(t *testing.T, pipeline string) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@e.com")
	git("config", "user.name", "T")
	if err := os.MkdirAll(filepath.Join(dir, ".janus"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".janus", "ci.yml"), []byte(pipeline), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "init")
	return dir, git("rev-parse", "HEAD")
}

// waitRun polls the store until run id reaches a terminal status.
func waitRun(t *testing.T, st store.Store, id string, timeout time.Duration) *model.Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		run, err := st.GetRun(id)
		if err == nil && run.Status.Terminal() {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not finish within %s", id, timeout)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestSweepRemovesOrphanWorkspaces(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "run-abc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "keep-me"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "persist-abc"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	r := New(st, engine.New(st), Options{WSRoot: root, PipelinePath: ".janus/ci.yml", MaxRuns: 1})
	if err := r.Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "run-abc")); !os.IsNotExist(err) {
		t.Error("run-* workspace should be swept")
	}
	if _, err := os.Stat(filepath.Join(root, "keep-me")); err != nil {
		t.Error("non run-* directory should be left alone")
	}
	if _, err := os.Stat(filepath.Join(root, "persist-abc")); err != nil {
		t.Error("persist-* workspace should survive the sweep")
	}
}

const echoPipeline = `name: ci
on: { push: { branches: [main] } }
jobs:
  build:
    steps:
      - run: echo building
`

func TestTriggerPersistentUsesRepoDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, echoPipeline)
	root := t.TempDir()
	st := store.NewMemory()
	allow, _ := allowlist.New([]string{"*"})
	r := New(st, engine.New(st), Options{WSRoot: root, PipelinePath: ".janus/ci.yml", MaxRuns: 2, Allowlist: allow, Persistent: true})
	ev := model.Event{Kind: model.EventManual, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"}

	res, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	run := waitRun(t, st, res.RunID, 15*time.Second)
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %s, want success", run.Status)
	}
	if base := filepath.Base(run.WorkspaceDir); !strings.HasPrefix(base, "persist-") {
		t.Fatalf("workspace dir = %q, want a persist-* dir", run.WorkspaceDir)
	}
	if _, err := os.Stat(run.WorkspaceDir); err != nil {
		t.Fatalf("persistent workspace should survive the run: %v", err)
	}

	// A second trigger reuses the same directory, and untracked files survive.
	marker := filepath.Join(run.WorkspaceDir, "marker")
	if err := os.WriteFile(marker, []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	res2, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("second Trigger: %v", err)
	}
	run2 := waitRun(t, st, res2.RunID, 15*time.Second)
	if run2.WorkspaceDir != run.WorkspaceDir {
		t.Errorf("second run dir = %q, want the same %q", run2.WorkspaceDir, run.WorkspaceDir)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("untracked marker should survive between runs: %v", err)
	}
}

func TestTriggerPersistentContentionFallsBackToFresh(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, echoPipeline)
	root := t.TempDir()
	st := store.NewMemory()
	allow, _ := allowlist.New([]string{"*"})
	r := New(st, engine.New(st), Options{WSRoot: root, PipelinePath: ".janus/ci.yml", MaxRuns: 2, Allowlist: allow, Persistent: true})

	// Simulate a run of the same repo holding the persistent workspace.
	mu := r.repoLock(repo)
	mu.Lock()
	defer mu.Unlock()

	res, err := r.Trigger(model.Event{Kind: model.EventManual, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"})
	if err != nil {
		t.Fatalf("Trigger under contention: %v", err)
	}
	// Completes without ever blocking on the held lock, in a fresh run-* dir.
	run := waitRun(t, st, res.RunID, 15*time.Second)
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %s, want success", run.Status)
	}
	if base := filepath.Base(run.WorkspaceDir); !strings.HasPrefix(base, "run-") {
		t.Errorf("workspace dir = %q, want a fresh run-* dir under contention", run.WorkspaceDir)
	}
	// Fresh fallback dirs are cleaned up after the run as usual.
	waitGone(t, run.WorkspaceDir, 5*time.Second)
}

// waitGone polls until path no longer exists (cleanup is async after Execute).
func waitGone(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still exists after %s", path, timeout)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestReconcileInterrupted(t *testing.T) {
	st := store.NewMemory()
	r := New(st, engine.New(st), Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 1})

	// A run as a crashed daemon would leave it: mid-flight, with a finished
	// step, a running step, and work that never started.
	orphan := &model.Run{
		ID: "orphan", Status: model.StatusRunning, CreatedAt: time.Now(), StartedAt: time.Now(),
		Jobs: []*model.JobRun{
			{Name: "build", Status: model.StatusRunning, StartedAt: time.Now(), Steps: []*model.StepRun{
				{Index: 0, Status: model.StatusSuccess},
				{Index: 1, Status: model.StatusRunning, StartedAt: time.Now()},
				{Index: 2, Status: model.StatusPending},
			}},
			{Name: "deploy", Status: model.StatusPending, Steps: []*model.StepRun{
				{Index: 0, Status: model.StatusPending},
			}},
		},
	}
	done := &model.Run{ID: "done", Status: model.StatusSuccess, CreatedAt: time.Now()}
	for _, run := range []*model.Run{orphan, done} {
		if err := st.SaveRun(run); err != nil {
			t.Fatal(err)
		}
	}

	n, err := r.ReconcileInterrupted()
	if err != nil {
		t.Fatalf("ReconcileInterrupted: %v", err)
	}
	if n != 1 {
		t.Fatalf("repaired = %d, want 1", n)
	}

	got, err := st.GetRun("orphan")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusCancelled || got.FinishedAt.IsZero() {
		t.Errorf("run = %s (finished %v), want cancelled with FinishedAt set", got.Status, got.FinishedAt)
	}
	build := got.Jobs[0]
	if build.Status != model.StatusCancelled || build.FinishedAt.IsZero() {
		t.Errorf("running job = %s, want cancelled with FinishedAt set", build.Status)
	}
	if s := build.Steps[0].Status; s != model.StatusSuccess {
		t.Errorf("finished step = %s, must stay success", s)
	}
	if s := build.Steps[1].Status; s != model.StatusCancelled {
		t.Errorf("running step = %s, want cancelled", s)
	}
	if s := build.Steps[2].Status; s != model.StatusSkipped {
		t.Errorf("pending step = %s, want skipped", s)
	}
	if s := got.Jobs[1].Status; s != model.StatusSkipped {
		t.Errorf("pending job = %s, want skipped", s)
	}

	if d, _ := st.GetRun("done"); d.Status != model.StatusSuccess {
		t.Errorf("terminal run = %s, must stay untouched", d.Status)
	}
}

func TestReconcileIgnoresStaleSidecar(t *testing.T) {
	dir := t.TempDir()
	st, err := store.NewFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := New(st, engine.New(st), Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 1})

	// A finished (terminal) run whose sidecar lagged at "running" — as a crash
	// between the run.json and summary.json writes would leave it.
	done := &model.Run{ID: "done", Status: model.StatusSuccess, CreatedAt: time.Now(),
		Jobs: []*model.JobRun{{Name: "build", Status: model.StatusSuccess, Steps: []*model.StepRun{{Index: 0, Status: model.StatusSuccess}}}}}
	if err := st.SaveRun(done); err != nil {
		t.Fatal(err)
	}
	stale, _ := json.Marshal(&model.RunSummary{ID: "done", Status: model.StatusRunning, CreatedAt: done.CreatedAt})
	if err := os.WriteFile(filepath.Join(dir, "runs", "done", "summary.json"), stale, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.ReconcileInterrupted(); err != nil {
		t.Fatalf("ReconcileInterrupted: %v", err)
	}
	// The finished run must NOT be clobbered to cancelled...
	got, _ := st.GetRun("done")
	if got.Status != model.StatusSuccess {
		t.Errorf("reconcile clobbered a terminal run: status = %s, want success", got.Status)
	}
	// ...and its stale sidecar must be healed to the real status.
	sums, _ := st.ListRuns(0, 0)
	if len(sums) != 1 || sums[0].Status != model.StatusSuccess {
		t.Errorf("stale sidecar not healed: %+v", sums)
	}
}

func TestTriggerAdmissionBound(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, echoPipeline)
	st := store.NewMemory()
	allow, _ := allowlist.New([]string{"*"})
	r := New(st, engine.New(st), Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 1, Allowlist: allow})
	ev := model.Event{Kind: model.EventManual, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"}

	// Fill every admission slot by hand: a further trigger must shed with
	// ErrBusy before doing any disk work.
	for i := 0; i < cap(r.admit); i++ {
		r.admit <- struct{}{}
	}
	if _, err := r.Trigger(ev); !errors.Is(err, ErrBusy) {
		t.Fatalf("Trigger at capacity = %v, want ErrBusy", err)
	}

	// Free one slot: an invalid pipeline path fails validation before taking
	// a slot at all, so the freed slot must remain free.
	<-r.admit
	bad := ev
	bad.PipelinePath = "../escape.yml"
	if _, err := r.Trigger(bad); err == nil || errors.Is(err, ErrBusy) {
		t.Fatalf("invalid pipeline path should fail with its own error, got %v", err)
	}
	if got, want := len(r.admit), cap(r.admit)-1; got != want {
		t.Fatalf("failed trigger leaked its admission slot: len = %d, want %d", got, want)
	}

	// The still-free slot admits a real run end-to-end.
	res, err := r.Trigger(ev)
	if err != nil || res.RunID == "" {
		t.Fatalf("Trigger after freeing a slot = %+v, %v", res, err)
	}
	if run := waitRun(t, st, res.RunID, 15*time.Second); run.Status != model.StatusSuccess {
		t.Fatalf("run status = %s, want success", run.Status)
	}
}

func TestTriggerPrunesHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, echoPipeline)
	st := store.NewMemory()
	allow, _ := allowlist.New([]string{"*"})
	r := New(st, engine.New(st), Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 1, HistoryLimit: 1, Allowlist: allow})
	ev := model.Event{Kind: model.EventManual, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"}

	first, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("first Trigger: %v", err)
	}
	waitRun(t, st, first.RunID, 15*time.Second)
	second, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("second Trigger: %v", err)
	}
	waitRun(t, st, second.RunID, 15*time.Second)

	// With history_limit=1, the first (older terminal) run is pruned once the
	// second finishes; the newest survives.
	deadline := time.Now().Add(5 * time.Second)
	for {
		runs, err := st.ListRuns(0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 && runs[0].ID == second.RunID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("history not pruned to the newest run: %+v", runs)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestTriggerInvalidTargetLeavesNoWorkspace(t *testing.T) {
	root := t.TempDir()
	st := store.NewMemory()
	allow, _ := allowlist.New([]string{"*"})
	r := New(st, engine.New(st), Options{WSRoot: root, PipelinePath: ".janus/ci.yml", MaxRuns: 1, Allowlist: allow})
	// Invalid SHA: workspace.Checkout rejects it before creating a
	// cleanup-capable Workspace, so the runner must remove the run-* dir it made.
	ev := model.Event{Kind: model.EventManual, RepoURL: "/some/repo", SHA: "not-hex", Ref: "refs/heads/main"}

	res, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	run := waitRun(t, st, res.RunID, 15*time.Second)
	if run.Status != model.StatusFailed || !strings.Contains(run.Reason, "checkout") {
		t.Fatalf("run = %s (%q), want failed with a checkout reason", run.Status, run.Reason)
	}
	// The workspace is removed before the run turns terminal, so no polling.
	matches, _ := filepath.Glob(filepath.Join(root, "run-*"))
	if len(matches) != 0 {
		t.Errorf("invalid target leaked workspace dirs: %v", matches)
	}
}

func TestTriggerCheckoutFailureRecordsFailedRun(t *testing.T) {
	root := t.TempDir()
	st := store.NewMemory()
	allow, _ := allowlist.New([]string{"*"})
	r := New(st, engine.New(st), Options{WSRoot: root, PipelinePath: ".janus/ci.yml", MaxRuns: 1, Allowlist: allow})
	ev := model.Event{Kind: model.EventManual, RepoURL: "/nonexistent/repo", SHA: "0123456789abcdef0123456789abcdef01234567", Ref: "refs/heads/main"}

	res, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	// The pending run is recorded synchronously, before Trigger returns — the
	// caller's 202 always names a fetchable run.
	if _, err := st.GetRun(res.RunID); err != nil {
		t.Fatalf("run not fetchable immediately after Trigger: %v", err)
	}
	run := waitRun(t, st, res.RunID, 15*time.Second)
	if run.Status != model.StatusFailed {
		t.Fatalf("run status = %s, want failed", run.Status)
	}
	if !strings.Contains(run.Reason, "checkout") {
		t.Errorf("run reason = %q, want it to name the checkout", run.Reason)
	}
	if len(run.Jobs) != 0 {
		t.Errorf("a pre-execution failure should have no jobs, got %d", len(run.Jobs))
	}
	matches, _ := filepath.Glob(filepath.Join(root, "run-*"))
	if len(matches) != 0 {
		t.Errorf("a failed checkout leaked workspace dirs: %v", matches)
	}
}

func TestTriggerNonMatchingEventRecordsSkippedRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, echoPipeline)
	root := t.TempDir()
	st := store.NewMemory()
	allow, _ := allowlist.New([]string{"*"})
	r := New(st, engine.New(st), Options{WSRoot: root, PipelinePath: ".janus/ci.yml", MaxRuns: 1, Allowlist: allow})
	// echoPipeline runs on push to main only; a push to another branch is
	// checked out and parsed, then recorded as skipped instead of executed.
	ev := model.Event{Kind: model.EventPush, RepoURL: repo, SHA: sha, Ref: "refs/heads/feature", Branch: "feature"}

	res, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	run := waitRun(t, st, res.RunID, 15*time.Second)
	if run.Status != model.StatusSkipped {
		t.Fatalf("run status = %s, want skipped", run.Status)
	}
	if run.Reason == "" {
		t.Error("a skipped run should record why it did not match")
	}
	if len(run.Jobs) != 0 {
		t.Errorf("a skipped run should have no jobs, got %d", len(run.Jobs))
	}
	matches, _ := filepath.Glob(filepath.Join(root, "run-*"))
	if len(matches) != 0 {
		t.Errorf("a skipped trigger leaked workspace dirs: %v", matches)
	}
}

func TestRunnerMarkDegraded(t *testing.T) {
	st := store.NewMemory()
	r := New(st, engine.New(st), Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 1})
	if r.Degraded() {
		t.Fatal("a fresh runner should not be degraded")
	}
	r.MarkDegraded()
	if !r.Degraded() {
		t.Error("MarkDegraded should latch Degraded()")
	}
}

// saveFailStore rejects SaveRun, as a full/read-only data dir would.
type saveFailStore struct {
	store.Store
}

func (saveFailStore) SaveRun(*model.Run) error { return errors.New("disk full") }

func TestTriggerSaveFailureDegrades(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, echoPipeline)
	allow, _ := allowlist.New([]string{"*"})
	r := New(saveFailStore{store.NewMemory()}, engine.New(store.NewMemory()),
		Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 1, Allowlist: allow})
	ev := model.Event{Kind: model.EventManual, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"}

	if _, err := r.Trigger(ev); err == nil {
		t.Fatal("Trigger should fail when SaveRun fails")
	}
	if !r.Degraded() {
		t.Error("a SaveRun failure should latch the runner degraded")
	}
}

func TestTriggerRejectsOversizedEventFields(t *testing.T) {
	st := store.NewMemory()
	allow, _ := allowlist.New([]string{"*"})
	r := New(st, engine.New(st), Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 1, Allowlist: allow})

	for _, ev := range []model.Event{
		{Kind: model.EventManual, RepoURL: "/repo", Ref: "refs/heads/main", Branch: strings.Repeat("b", maxBranchLen+1)},
		{Kind: model.EventManual, RepoURL: "/repo", Ref: "refs/heads/main", Branch: "main", Title: strings.Repeat("t", maxTitleLen+1)},
	} {
		if _, err := r.Trigger(ev); err == nil {
			t.Fatalf("Trigger with an over-long field should error: %+v", ev)
		}
	}
	if runs, _ := st.ListRuns(0, 0); len(runs) != 0 {
		t.Errorf("an oversized-field trigger must not record a run, got %d", len(runs))
	}
}

func TestShutdownRejectsNewTriggers(t *testing.T) {
	st := store.NewMemory()
	allow, _ := allowlist.New([]string{"*"})
	r := New(st, engine.New(st), Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 1, Allowlist: allow})

	r.Shutdown(time.Second) // no in-flight work: returns immediately after closing the gate

	ev := model.Event{Kind: model.EventManual, RepoURL: "/anything", SHA: "0123456789abcdef0123456789abcdef01234567", Ref: "refs/heads/main"}
	if _, err := r.Trigger(ev); !errors.Is(err, ErrBusy) {
		t.Fatalf("Trigger after Shutdown = %v, want ErrBusy (no disk work)", err)
	}
}

func TestShutdownWaitsForAdmittedTrigger(t *testing.T) {
	st := store.NewMemory()
	allow, _ := allowlist.New([]string{"*"})
	r := New(st, engine.New(st), Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 1, Allowlist: allow})

	// Simulate a trigger that has been admitted and is mid-checkout: it holds
	// an admission slot and a wg count, but has not spawned its run goroutine.
	release, err := r.admitOne()
	if err != nil {
		t.Fatalf("admitOne: %v", err)
	}

	done := make(chan struct{})
	go func() { r.Shutdown(30 * time.Second); close(done) }()

	// Shutdown must not complete while the admitted trigger is outstanding,
	// and the closing gate must become visible to new triggers.
	deadline := time.Now().Add(5 * time.Second)
	ev := model.Event{Kind: model.EventManual, RepoURL: "/x", SHA: "0123456789abcdef0123456789abcdef01234567", Ref: "refs/heads/main"}
	for {
		if _, err := r.Trigger(ev); errors.Is(err, ErrBusy) {
			break // closing observed
		}
		if time.Now().After(deadline) {
			t.Fatal("closing gate never became visible to Trigger")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("Shutdown returned while an admitted trigger was still outstanding")
	default:
	}

	release() // the trigger finishes: Shutdown can now drain
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after the trigger released")
	}
	if len(r.admit) != 0 {
		t.Errorf("admission slot leaked: len = %d, want 0", len(r.admit))
	}
}

func TestTriggerRejectsDisallowedRepo(t *testing.T) {
	root := t.TempDir()
	st := store.NewMemory()
	allow, _ := allowlist.New([]string{"https://allowed.example.com"})
	r := New(st, engine.New(st), Options{WSRoot: root, PipelinePath: ".janus/ci.yml", MaxRuns: 1, Allowlist: allow})

	_, err := r.Trigger(model.Event{
		Kind:    model.EventManual,
		RepoURL: "https://evil.example.com/x.git",
		Ref:     "refs/heads/main",
	})
	if !errors.Is(err, ErrRepoNotAllowed) {
		t.Fatalf("err = %v, want ErrRepoNotAllowed", err)
	}
	// Rejected before touching disk: no run-* workspace was created.
	entries, _ := filepath.Glob(filepath.Join(root, "run-*"))
	if len(entries) != 0 {
		t.Errorf("a workspace was created for a disallowed repo: %v", entries)
	}
}

func TestPipelineFile(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "evil.yml")
	def := ".janus/ci.yml"
	tests := []struct {
		name     string
		def      string
		override string
		want     string
		wantErr  bool
	}{
		{"empty falls back to default", def, "", filepath.FromSlash(".janus/ci.yml"), false},
		{"bare name resolves in the pipeline dir", def, "release.yml", filepath.FromSlash(".janus/release.yml"), false},
		{"subdirectory resolves in the pipeline dir", def, "nightly/build.yml", filepath.FromSlash(".janus/nightly/build.yml"), false},
		{"full path resolves under the dir, not from the root", def, ".janus/release.yml", filepath.FromSlash(".janus/.janus/release.yml"), false},
		{"escape from the pipeline dir rejected", def, "../examples/evil.yml", "", true},
		{"nested escape rejected", def, "../../etc/evil.yml", "", true},
		{"the directory itself rejected", def, ".", "", true},
		{"absolute path rejected", def, abs, "", true},
		{"root-level default resolves from the root", "janus.yml", "ci/other.yml", filepath.FromSlash("ci/other.yml"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pipelineFile(tc.def, model.Event{PipelinePath: tc.override})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("pipelineFile(%q, %q) = %q, want error", tc.def, tc.override, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("pipelineFile(%q, %q): %v", tc.def, tc.override, err)
			}
			if got != tc.want {
				t.Errorf("pipelineFile(%q, %q) = %q, want %q", tc.def, tc.override, got, tc.want)
			}
		})
	}
}

func TestMatches(t *testing.T) {
	pushMain := &model.Workflow{On: model.Triggers{Push: &model.BranchFilter{Branches: []string{"main"}}}}
	mrMain := &model.Workflow{On: model.Triggers{MergeRequest: &model.BranchFilter{Branches: []string{"main"}}}}
	pushAny := &model.Workflow{On: model.Triggers{Push: &model.BranchFilter{}}}
	pushIgnoreMain := &model.Workflow{On: model.Triggers{Push: &model.BranchFilter{Ignore: []string{"main"}}}}

	tests := []struct {
		name string
		wf   *model.Workflow
		ev   model.Event
		want bool
	}{
		{"manual always matches", pushMain, model.Event{Kind: model.EventManual}, true},
		{"push on listed branch", pushMain, model.Event{Kind: model.EventPush, Branch: "main"}, true},
		{"push on other branch", pushMain, model.Event{Kind: model.EventPush, Branch: "dev"}, false},
		{"push when only MR declared", mrMain, model.Event{Kind: model.EventPush, Branch: "main"}, false},
		{"MR on target branch", mrMain, model.Event{Kind: model.EventMergeRequest, Branch: "main"}, true},
		{"MR on other branch", mrMain, model.Event{Kind: model.EventMergeRequest, Branch: "dev"}, false},
		{"empty filter matches any branch", pushAny, model.Event{Kind: model.EventPush, Branch: "whatever"}, true},
		{"push to non-ignored branch", pushIgnoreMain, model.Event{Kind: model.EventPush, Branch: "dev"}, true},
		{"push to ignored branch", pushIgnoreMain, model.Event{Kind: model.EventPush, Branch: "main"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := matches(tc.wf, tc.ev); got != tc.want {
				t.Errorf("matches() = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- Per-run cancellation -----------------------------------------------------

const sleepPipeline = `name: ci
on: { push: { branches: [main] } }
jobs:
  build:
    steps:
      - run: sleep 30
`

// waitStatus polls the store until run id reaches status.
func waitStatus(t *testing.T, st store.Store, id string, want model.Status, timeout time.Duration) *model.Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		run, err := st.GetRun(id)
		if err == nil && run.Status == want {
			return run
		}
		if time.Now().After(deadline) {
			last := "unknown"
			if err == nil {
				last = string(run.Status)
			}
			t.Fatalf("run %s did not reach %s within %s (last: %s)", id, want, timeout, last)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitSlotsFree polls until the runner's admission and run slots are all
// released, catching leaks on the cancel paths.
func waitSlotsFree(t *testing.T, r *Runner, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for len(r.admit) != 0 || len(r.sem) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("slots leaked: admit=%d sem=%d", len(r.admit), len(r.sem))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func newCancelTestRunner(t *testing.T, maxRuns int) (*Runner, store.Store) {
	t.Helper()
	st := store.NewMemory()
	allow, _ := allowlist.New([]string{"*"})
	r := New(st, engine.New(st), Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: maxRuns, Allowlist: allow})
	return r, st
}

func TestCancelRunningRunKillsProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("blocking sh pipeline")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, sleepPipeline)
	r, st := newCancelTestRunner(t, 2)
	ev := model.Event{Kind: model.EventManual, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"}

	res, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	waitStatus(t, st, res.RunID, model.StatusRunning, 15*time.Second)

	start := time.Now()
	if !r.Cancel(res.RunID, "cancelled via API") {
		t.Fatal("Cancel returned false for a running run")
	}
	// Idempotent while teardown may still be in flight.
	r.Cancel(res.RunID, "again")
	run := waitRun(t, st, res.RunID, 15*time.Second)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("cancel took %s; the sleeping process was not killed", elapsed)
	}
	if run.Status != model.StatusCancelled {
		t.Fatalf("run status = %s, want cancelled", run.Status)
	}
	if run.Reason != "cancelled via API" {
		t.Errorf("reason = %q, want %q", run.Reason, "cancelled via API")
	}
	waitSlotsFree(t, r, 5*time.Second)
	// Once settled the run is deregistered; a late Cancel reports unknown.
	if r.Cancel(res.RunID, "late") {
		t.Error("Cancel on a settled run should return false")
	}
}

func TestCancelPendingRunQueuedForSlot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("blocking sh pipeline")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, sleepPipeline)
	r, st := newCancelTestRunner(t, 1)
	ev := model.Event{Kind: model.EventManual, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"}

	resA, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger A: %v", err)
	}
	waitStatus(t, st, resA.RunID, model.StatusRunning, 15*time.Second)

	// B checks out and parses, then queues pending on the single run slot.
	resB, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger B: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		run, err := st.GetRun(resB.RunID)
		if err == nil && len(run.Jobs) > 0 {
			break // populated: past parse, at (or headed for) the slot wait
		}
		if time.Now().After(deadline) {
			t.Fatal("run B never got populated")
		}
		time.Sleep(25 * time.Millisecond)
	}

	if !r.Cancel(resB.RunID, "cancelled via API") {
		t.Fatal("Cancel returned false for the queued run")
	}
	runB := waitRun(t, st, resB.RunID, 15*time.Second)
	if runB.Status != model.StatusCancelled {
		t.Fatalf("run B status = %s, want cancelled", runB.Status)
	}
	if runB.Reason != "cancelled via API" {
		t.Errorf("run B reason = %q, want %q", runB.Reason, "cancelled via API")
	}
	// The queued run never started; its jobs and steps settle as skipped.
	for _, jr := range runB.Jobs {
		if !jr.Status.Terminal() {
			t.Errorf("job %s status = %s, want terminal", jr.Name, jr.Status)
		}
		for _, sr := range jr.Steps {
			if !sr.Status.Terminal() {
				t.Errorf("job %s step %d status = %s, want terminal", jr.Name, sr.Index, sr.Status)
			}
		}
	}

	// A was untouched by B's cancel; stop it too and drain.
	if !r.Cancel(resA.RunID, "cancelled via API") {
		t.Fatal("Cancel returned false for run A")
	}
	waitRun(t, st, resA.RunID, 15*time.Second)
	waitSlotsFree(t, r, 5*time.Second)
}

func TestCancelUnknownRun(t *testing.T) {
	r, _ := newCancelTestRunner(t, 1)
	if r.Cancel("no-such-run", "cancelled via API") {
		t.Fatal("Cancel of an unknown run should return false")
	}
}

// --- Concurrency groups -------------------------------------------------------

const groupSleepPipeline = `name: ci
on: { push: { branches: [main] } }
concurrency:
  group: g-${{ branch }}
jobs:
  build:
    steps:
      - run: sleep 30
`

// waitGroupWaiter polls until runID is the registered waiter of the group, so
// a later trigger deterministically supersedes it (enter order, not trigger
// order, decides who supersedes whom).
func waitGroupWaiter(t *testing.T, r *Runner, key, runID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		r.groups.mu.Lock()
		e := r.groups.groups[key]
		ok := e != nil && e.pending != nil && e.pending.runID == runID
		r.groups.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never became the waiter of group %q", runID, key)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestGroupSupersedesQueuedRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("blocking sh pipeline")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, groupSleepPipeline)
	r, st := newCancelTestRunner(t, 4)
	ev := model.Event{Kind: model.EventPush, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"}
	groupKey := repo + "\x00" + "g-main"

	resA, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger A: %v", err)
	}
	runA := waitStatus(t, st, resA.RunID, model.StatusRunning, 15*time.Second)
	if runA.ConcurrencyGroup != "g-main" {
		t.Fatalf("run A concurrency group = %q, want g-main", runA.ConcurrencyGroup)
	}

	// B queues behind A in the group (holding no run slot: MaxRuns is 4).
	resB, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger B: %v", err)
	}
	waitGroupWaiter(t, r, groupKey, resB.RunID, 15*time.Second)

	// C supersedes B: only the newest waiter survives.
	resC, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger C: %v", err)
	}
	runB := waitRun(t, st, resB.RunID, 15*time.Second)
	if runB.Status != model.StatusCancelled {
		t.Fatalf("run B status = %s, want cancelled", runB.Status)
	}
	if want := "superseded by run " + resC.RunID; runB.Reason != want {
		t.Errorf("run B reason = %q, want %q", runB.Reason, want)
	}

	// Without cancel-in-progress A kept running; once it stops, C executes.
	if got, _ := st.GetRun(resA.RunID); got.Status != model.StatusRunning {
		t.Fatalf("run A status = %s, want still running", got.Status)
	}
	if !r.Cancel(resA.RunID, "cancelled via API") {
		t.Fatal("Cancel A")
	}
	waitRun(t, st, resA.RunID, 15*time.Second)
	waitStatus(t, st, resC.RunID, model.StatusRunning, 15*time.Second)
	if !r.Cancel(resC.RunID, "cancelled via API") {
		t.Fatal("Cancel C")
	}
	waitRun(t, st, resC.RunID, 15*time.Second)
	waitSlotsFree(t, r, 5*time.Second)

	// The registry self-cleans: once no trigger is in flight, every
	// high-water mark is retired (no per-branch memory growth).
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.groups.mu.Lock()
		n := len(r.groups.seen) + len(r.groups.inflight) + len(r.groups.groups)
		r.groups.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("group registry leaked %d entries after all runs settled", n)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestGroupCancelInProgressCancelsRunningRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("blocking sh pipeline")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// No group: key — exercises the implicit <name>-<branch> group too.
	repo, sha := initGitRepo(t, `name: ci
on: { push: { branches: [main] } }
concurrency:
  cancel-in-progress: true
jobs:
  build:
    steps:
      - run: sleep 30
`)
	r, st := newCancelTestRunner(t, 4)
	ev := model.Event{Kind: model.EventPush, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"}

	resA, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger A: %v", err)
	}
	runA := waitStatus(t, st, resA.RunID, model.StatusRunning, 15*time.Second)
	if runA.ConcurrencyGroup != "ci-main" {
		t.Fatalf("run A concurrency group = %q, want the implicit ci-main", runA.ConcurrencyGroup)
	}

	resB, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger B: %v", err)
	}
	runA = waitRun(t, st, resA.RunID, 15*time.Second)
	if runA.Status != model.StatusCancelled {
		t.Fatalf("run A status = %s, want cancelled", runA.Status)
	}
	if want := "superseded by run " + resB.RunID; runA.Reason != want {
		t.Errorf("run A reason = %q, want %q", runA.Reason, want)
	}

	waitStatus(t, st, resB.RunID, model.StatusRunning, 15*time.Second)
	if !r.Cancel(resB.RunID, "cancelled via API") {
		t.Fatal("Cancel B")
	}
	waitRun(t, st, resB.RunID, 15*time.Second)
	waitSlotsFree(t, r, 5*time.Second)
}

func TestGroupsAreRepoScoped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("blocking sh pipeline")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// Identical literal group in two different repos: both run concurrently.
	pipeline := `name: ci
on: { push: { branches: [main] } }
concurrency:
  group: shared
jobs:
  build:
    steps:
      - run: sleep 30
`
	repo1, sha1 := initGitRepo(t, pipeline)
	repo2, sha2 := initGitRepo(t, pipeline)
	r, st := newCancelTestRunner(t, 2)

	res1, err := r.Trigger(model.Event{Kind: model.EventPush, RepoURL: repo1, SHA: sha1, Ref: "refs/heads/main", Branch: "main"})
	if err != nil {
		t.Fatalf("Trigger repo1: %v", err)
	}
	res2, err := r.Trigger(model.Event{Kind: model.EventPush, RepoURL: repo2, SHA: sha2, Ref: "refs/heads/main", Branch: "main"})
	if err != nil {
		t.Fatalf("Trigger repo2: %v", err)
	}
	waitStatus(t, st, res1.RunID, model.StatusRunning, 15*time.Second)
	waitStatus(t, st, res2.RunID, model.StatusRunning, 15*time.Second)

	r.Cancel(res1.RunID, "cancelled via API")
	r.Cancel(res2.RunID, "cancelled via API")
	waitRun(t, st, res1.RunID, 15*time.Second)
	waitRun(t, st, res2.RunID, 15*time.Second)
	waitSlotsFree(t, r, 5*time.Second)
}

func TestUngroupedRunsExecuteIndependently(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, echoPipeline)
	r, st := newCancelTestRunner(t, 4)
	ev := model.Event{Kind: model.EventPush, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"}

	resA, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger A: %v", err)
	}
	resB, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger B: %v", err)
	}
	for _, id := range []string{resA.RunID, resB.RunID} {
		run := waitRun(t, st, id, 15*time.Second)
		if run.Status != model.StatusSuccess {
			t.Fatalf("run %s status = %s, want success (no concurrency key)", id, run.Status)
		}
		if run.ConcurrencyGroup != "" {
			t.Errorf("run %s concurrency group = %q, want empty", id, run.ConcurrencyGroup)
		}
	}
}

// registrySizes reads the group registry's accounting under its lock.
func registrySizes(r *Runner) (inflight, seen int) {
	r.groups.mu.Lock()
	defer r.groups.mu.Unlock()
	return len(r.groups.inflight), len(r.groups.seen)
}

// TestGroupRegistryDrainsWhileRunExecutes pins the trigger-resolution fix: a
// trigger retires from the order accounting when it enters its group (or
// resolves ungrouped), NOT when the run finishes — otherwise a single hung
// step (step_timeout defaults to 0) would pin every newer branch-group mark
// in memory for as long as it runs.
func TestGroupRegistryDrainsWhileRunExecutes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("blocking sh pipeline")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	for _, tc := range []struct {
		name, pipeline string
	}{
		{"grouped", groupSleepPipeline},
		{"ungrouped", sleepPipeline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, sha := initGitRepo(t, tc.pipeline)
			r, st := newCancelTestRunner(t, 2)
			ev := model.Event{Kind: model.EventPush, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"}
			res, err := r.Trigger(ev)
			if err != nil {
				t.Fatalf("Trigger: %v", err)
			}
			waitStatus(t, st, res.RunID, model.StatusRunning, 15*time.Second)
			// Entering the group (or resolving ungrouped) strictly precedes
			// execution, so by Running the accounting must already be empty.
			if inflight, seen := registrySizes(r); inflight != 0 || seen != 0 {
				t.Errorf("inflight=%d seen=%d while the run executes, want 0/0", inflight, seen)
			}
			if !r.Cancel(res.RunID, "cancelled via API") {
				t.Fatal("Cancel")
			}
			waitRun(t, st, res.RunID, 15*time.Second)
			waitSlotsFree(t, r, 5*time.Second)
		})
	}
}

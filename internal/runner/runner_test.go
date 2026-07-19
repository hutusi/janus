package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

	res, err := r.Trigger(context.Background(), ev)
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
	res2, err := r.Trigger(context.Background(), ev)
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

	res, err := r.Trigger(context.Background(), model.Event{Kind: model.EventManual, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"})
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
	if _, err := r.Trigger(context.Background(), ev); !errors.Is(err, ErrBusy) {
		t.Fatalf("Trigger at capacity = %v, want ErrBusy", err)
	}

	// Free one slot: a trigger that fails before checkout (invalid pipeline
	// path) must release its slot on the way out, not leak it.
	<-r.admit
	bad := ev
	bad.PipelinePath = "../escape.yml"
	if _, err := r.Trigger(context.Background(), bad); err == nil || errors.Is(err, ErrBusy) {
		t.Fatalf("invalid pipeline path should fail with its own error, got %v", err)
	}
	if got, want := len(r.admit), cap(r.admit)-1; got != want {
		t.Fatalf("failed trigger leaked its admission slot: len = %d, want %d", got, want)
	}

	// The still-free slot admits a real run end-to-end.
	res, err := r.Trigger(context.Background(), ev)
	if err != nil || !res.Started {
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

	first, err := r.Trigger(context.Background(), ev)
	if err != nil {
		t.Fatalf("first Trigger: %v", err)
	}
	waitRun(t, st, first.RunID, 15*time.Second)
	second, err := r.Trigger(context.Background(), ev)
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

	if _, err := r.Trigger(context.Background(), ev); err == nil {
		t.Fatal("Trigger with an invalid SHA should error")
	}
	matches, _ := filepath.Glob(filepath.Join(root, "run-*"))
	if len(matches) != 0 {
		t.Errorf("invalid target leaked workspace dirs: %v", matches)
	}
}

func TestTriggerCheckoutHonorsRequestCancellation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, echoPipeline)
	root := t.TempDir()
	st := store.NewMemory()
	allow, _ := allowlist.New([]string{"*"})
	r := New(st, engine.New(st), Options{WSRoot: root, PipelinePath: ".janus/ci.yml", MaxRuns: 1, Allowlist: allow})
	ev := model.Event{Kind: model.EventManual, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"}

	// An already-cancelled request context must abort the checkout — with the
	// old r.ctx-only code the request cancellation was ignored and this
	// checkout would succeed.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Trigger(ctx, ev); err == nil {
		t.Fatal("Trigger with a cancelled request ctx should fail the checkout")
	}
	if runs, _ := st.ListRuns(0, 0); len(runs) != 0 {
		t.Errorf("a cancelled checkout must not record a run, got %d", len(runs))
	}
	matches, _ := filepath.Glob(filepath.Join(root, "run-*"))
	if len(matches) != 0 {
		t.Errorf("a cancelled checkout leaked workspace dirs: %v", matches)
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

	if _, err := r.Trigger(context.Background(), ev); err == nil {
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
		if _, err := r.Trigger(context.Background(), ev); err == nil {
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
	if _, err := r.Trigger(context.Background(), ev); !errors.Is(err, ErrBusy) {
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
		if _, err := r.Trigger(context.Background(), ev); errors.Is(err, ErrBusy) {
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

	_, err := r.Trigger(context.Background(), model.Event{
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

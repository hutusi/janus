package runner

import (
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/hutusi/janus/internal/allowlist"
	"github.com/hutusi/janus/internal/engine"
	"github.com/hutusi/janus/internal/model"
	"github.com/hutusi/janus/internal/store"
)

// recordingReporter records the sequence of states the runner reports.
type recordingReporter struct {
	mu     sync.Mutex
	states []model.Status
}

func (r *recordingReporter) Report(_ *model.Run, state model.Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, state)
}

func (r *recordingReporter) snapshot() []model.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]model.Status(nil), r.states...)
}

func newReporterRunner(t *testing.T, rep StatusReporter, st store.Store) *Runner {
	t.Helper()
	allow, _ := allowlist.New([]string{"*"})
	return New(st, engine.New(st), Options{
		WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml",
		MaxRuns: 1, Allowlist: allow, Reporter: rep,
	})
}

func TestReporterReportsRunningThenTerminal(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, echoPipeline)
	rr := &recordingReporter{}
	st := store.NewMemory()
	r := newReporterRunner(t, rr, st)
	ev := model.Event{Kind: model.EventManual, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"}

	res, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if run := waitRun(t, st, res.RunID, 15*time.Second); run.Status != model.StatusSuccess {
		t.Fatalf("run status = %s, want success", run.Status)
	}
	r.Shutdown(5 * time.Second)

	if got := rr.snapshot(); len(got) != 2 || got[0] != model.StatusRunning || got[1] != model.StatusSuccess {
		t.Fatalf("reported states = %v, want [running success]", got)
	}
}

func TestReporterPreExecutionOutcomeSkipsRunning(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// echoPipeline only triggers on push to main; a push to another branch is a
	// pre-execution skip that never reaches Execute — so no "running" is posted.
	repo, sha := initGitRepo(t, echoPipeline)
	rr := &recordingReporter{}
	st := store.NewMemory()
	r := newReporterRunner(t, rr, st)
	ev := model.Event{Kind: model.EventPush, RepoURL: repo, SHA: sha, Ref: "refs/heads/other", Branch: "other"}

	res, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if run := waitRun(t, st, res.RunID, 15*time.Second); run.Status != model.StatusSkipped {
		t.Fatalf("run status = %s, want skipped", run.Status)
	}
	r.Shutdown(5 * time.Second)

	if got := rr.snapshot(); len(got) != 1 || got[0] != model.StatusSkipped {
		t.Fatalf("reported states = %v, want exactly [skipped] (no running)", got)
	}
}

func TestNilReporterRunsCleanly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, echoPipeline)
	st := store.NewMemory()
	r := newReporterRunner(t, nil, st)
	ev := model.Event{Kind: model.EventManual, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"}

	res, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if run := waitRun(t, st, res.RunID, 15*time.Second); run.Status != model.StatusSuccess {
		t.Fatalf("run status = %s, want success (nil reporter must be a no-op)", run.Status)
	}
}

func TestReporterTerminalGatedOnPersistence(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// The terminal post is gated on durable persistence like the notification;
	// "running" (posted before Execute) still fires, but with the terminal write
	// failing no terminal state is reported.
	repo, sha := initGitRepo(t, echoPipeline)
	rr := &recordingReporter{}
	st := updateFailStore{store.NewMemory()}
	r := newReporterRunner(t, rr, st)
	ev := model.Event{Kind: model.EventManual, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"}

	if _, err := r.Trigger(ev); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	r.Shutdown(15 * time.Second)

	if got := rr.snapshot(); len(got) != 1 || got[0] != model.StatusRunning {
		t.Fatalf("reported states = %v, want exactly [running] (terminal gated on persistence)", got)
	}
}

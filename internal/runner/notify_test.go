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

// recordingNotifier is a fake Notifier that records the status of every run it
// is told about. Safe for the concurrent calls parallel runs make.
type recordingNotifier struct {
	mu       sync.Mutex
	statuses []model.Status
}

func (n *recordingNotifier) Notify(run *model.Run) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.statuses = append(n.statuses, run.Status)
}

func (n *recordingNotifier) snapshot() []model.Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]model.Status(nil), n.statuses...)
}

func newNotifyRunner(t *testing.T, n Notifier, maxRuns int) (*Runner, store.Store) {
	t.Helper()
	st := store.NewMemory()
	allow, _ := allowlist.New([]string{"*"})
	r := New(st, engine.New(st), Options{
		WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml",
		MaxRuns: maxRuns, Allowlist: allow, Notifier: n,
	})
	return r, st
}

func TestNotifierFiresOnExecutedRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, echoPipeline)
	rn := &recordingNotifier{}
	r, st := newNotifyRunner(t, rn, 1)
	ev := model.Event{Kind: model.EventManual, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"}

	res, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if run := waitRun(t, st, res.RunID, 15*time.Second); run.Status != model.StatusSuccess {
		t.Fatalf("run status = %s, want success", run.Status)
	}
	// The dispatch runs in a defer just before release(); Shutdown waits on the
	// admission WaitGroup, whose Done is release() — so once Shutdown returns the
	// hand-off is guaranteed to have happened.
	r.Shutdown(5 * time.Second)

	if got := rn.snapshot(); len(got) != 1 || got[0] != model.StatusSuccess {
		t.Fatalf("notified statuses = %v, want exactly [success]", got)
	}
}

func TestNotifierFiresOnPreExecutionOutcome(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// echoPipeline only triggers on push to main; a push to another branch is a
	// pre-execution skip via the finish closure — which must still notify.
	repo, sha := initGitRepo(t, echoPipeline)
	rn := &recordingNotifier{}
	r, st := newNotifyRunner(t, rn, 1)
	ev := model.Event{Kind: model.EventPush, RepoURL: repo, SHA: sha, Ref: "refs/heads/other", Branch: "other"}

	res, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if run := waitRun(t, st, res.RunID, 15*time.Second); run.Status != model.StatusSkipped {
		t.Fatalf("run status = %s, want skipped", run.Status)
	}
	r.Shutdown(5 * time.Second)

	if got := rn.snapshot(); len(got) != 1 || got[0] != model.StatusSkipped {
		t.Fatalf("notified statuses = %v, want exactly [skipped]", got)
	}
}

func TestNilNotifierRunsCleanly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, echoPipeline)
	r, st := newNotifyRunner(t, nil, 1) // no notifier configured
	ev := model.Event{Kind: model.EventManual, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"}

	res, err := r.Trigger(ev)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if run := waitRun(t, st, res.RunID, 15*time.Second); run.Status != model.StatusSuccess {
		t.Fatalf("run status = %s, want success (nil notifier must be a no-op)", run.Status)
	}
}

// TestShutdownDrainsNotifications guards the dispatch-defer ordering: the
// notification hand-off must happen-before release() (wg.Done), so once
// Shutdown's wg.Wait returns every finished run has been announced. If the defer
// were registered before release() (running after wg.Done), this count would
// race low.
func TestShutdownDrainsNotifications(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, echoPipeline)
	rn := &recordingNotifier{}
	r, st := newNotifyRunner(t, rn, 3)
	ev := model.Event{Kind: model.EventManual, RepoURL: repo, SHA: sha, Ref: "refs/heads/main", Branch: "main"}

	const n = 5
	var ids []string
	for i := 0; i < n; i++ {
		res, err := r.Trigger(ev)
		if err != nil {
			t.Fatalf("Trigger %d: %v", i, err)
		}
		ids = append(ids, res.RunID)
	}
	for _, id := range ids {
		waitRun(t, st, id, 15*time.Second)
	}
	r.Shutdown(10 * time.Second)

	if got := rn.snapshot(); len(got) != n {
		t.Fatalf("got %d notifications after shutdown, want %d", len(got), n)
	}
}

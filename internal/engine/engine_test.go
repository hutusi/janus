//go:build unix

// These execution tests drive real steps through /bin/sh and use POSIX commands
// (sleep, true, `;`, 1>&2). Windows equivalents live in engine_windows_test.go;
// OS-agnostic scheduling/DAG tests live in dag_test.go.
package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hutusi/janus/internal/model"
	"github.com/hutusi/janus/internal/pipeline"
	"github.com/hutusi/janus/internal/store"
)

func mustParse(t *testing.T, src string) *model.Workflow {
	t.Helper()
	wf, err := pipeline.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return wf
}

func jobRun(run *model.Run, name string) *model.JobRun {
	for _, jr := range run.Jobs {
		if jr.Name == name {
			return jr
		}
	}
	return nil
}

func readStepLog(t *testing.T, st store.Store, runID, job string, idx int) string {
	t.Helper()
	rc, err := st.ReadLogs(runID, job, idx, 0)
	if err != nil {
		t.Fatalf("ReadLogs: %v", err)
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func TestRunSequentialStepsAndExitCode(t *testing.T) {
	wf := mustParse(t, `
name: ci
on: { push: {} }
jobs:
  build:
    steps:
      - run: echo first
      - run: echo "branch=${{ branch }}"
`)
	st := store.NewMemory()
	run, err := New(st).Run(context.Background(), wf, model.Event{Kind: model.EventManual, Branch: "main"}, t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %s, want success", run.Status)
	}
	build := jobRun(run, "build")
	if build.Status != model.StatusSuccess {
		t.Errorf("build status = %s, want success", build.Status)
	}
	for _, sr := range build.Steps {
		if sr.Status != model.StatusSuccess || sr.ExitCode != 0 {
			t.Errorf("step %d status=%s code=%d, want success/0", sr.Index, sr.Status, sr.ExitCode)
		}
	}
	if got := readStepLog(t, st, run.ID, "build", 1); !strings.Contains(got, "branch=main") {
		t.Errorf("step 1 log = %q, want it to contain branch=main (interpolation)", got)
	}
}

func TestRunFailingStepStopsJob(t *testing.T) {
	wf := mustParse(t, `
name: ci
on: { push: {} }
jobs:
  build:
    steps:
      - run: exit 3
      - run: echo "should not run"
`)
	st := store.NewMemory()
	run, _ := New(st).Run(context.Background(), wf, model.Event{Kind: model.EventManual}, t.TempDir())
	if run.Status != model.StatusFailed {
		t.Fatalf("run status = %s, want failed", run.Status)
	}
	build := jobRun(run, "build")
	if build.Steps[0].Status != model.StatusFailed || build.Steps[0].ExitCode != 3 {
		t.Errorf("step 0 = %s/%d, want failed/3", build.Steps[0].Status, build.Steps[0].ExitCode)
	}
	if build.Steps[1].Status != model.StatusSkipped {
		t.Errorf("step 1 = %s, want skipped", build.Steps[1].Status)
	}
}

func TestRunNeedsOrdering(t *testing.T) {
	wf := mustParse(t, `
name: ci
on: { push: {} }
jobs:
  build:
    steps:
      - run: "true"
  test:
    needs: [build]
    steps:
      - run: "true"
`)
	st := store.NewMemory()
	run, _ := New(st).Run(context.Background(), wf, model.Event{Kind: model.EventManual}, t.TempDir())
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %s, want success", run.Status)
	}
	build, test := jobRun(run, "build"), jobRun(run, "test")
	if test.StartedAt.Before(build.FinishedAt) {
		t.Errorf("test started (%v) before build finished (%v)", test.StartedAt, build.FinishedAt)
	}
}

func TestRunFailFastSkipsDependents(t *testing.T) {
	wf := mustParse(t, `
name: ci
on: { push: {} }
jobs:
  build:
    steps:
      - run: exit 1
  test:
    needs: [build]
    steps:
      - run: echo nope
`)
	st := store.NewMemory()
	run, _ := New(st).Run(context.Background(), wf, model.Event{Kind: model.EventManual}, t.TempDir())
	if run.Status != model.StatusFailed {
		t.Fatalf("run status = %s, want failed", run.Status)
	}
	if got := jobRun(run, "build").Status; got != model.StatusFailed {
		t.Errorf("build status = %s, want failed", got)
	}
	if got := jobRun(run, "test").Status; got != model.StatusSkipped {
		t.Errorf("test status = %s, want skipped (dependent of failed job)", got)
	}
}

func TestRunJobBranchFilterSkips(t *testing.T) {
	const src = `
name: ci
on: { push: {} }
jobs:
  build:
    steps:
      - run: "true"
  deploy:
    needs: [build]
    branches: [master, main]
    steps:
      - run: "true"
`
	st := store.NewMemory()
	run, _ := New(st).Run(context.Background(), mustParse(t, src),
		model.Event{Kind: model.EventPush, Branch: "feature/x"}, t.TempDir())
	if run.Status != model.StatusSuccess {
		t.Fatalf("feature run status = %s, want success (a branch-skipped job is not a failure)", run.Status)
	}
	if got := jobRun(run, "build").Status; got != model.StatusSuccess {
		t.Errorf("build status = %s, want success", got)
	}
	deploy := jobRun(run, "deploy")
	if deploy.Status != model.StatusSkipped {
		t.Errorf("deploy status = %s, want skipped on a non-release branch", deploy.Status)
	}
	for _, sr := range deploy.Steps {
		if sr.Status != model.StatusSkipped {
			t.Errorf("deploy step %d status = %s, want skipped", sr.Index, sr.Status)
		}
	}

	run, _ = New(st).Run(context.Background(), mustParse(t, src),
		model.Event{Kind: model.EventPush, Branch: "main"}, t.TempDir())
	if run.Status != model.StatusSuccess {
		t.Fatalf("main run status = %s, want success", run.Status)
	}
	if got := jobRun(run, "deploy").Status; got != model.StatusSuccess {
		t.Errorf("deploy status on main = %s, want success", got)
	}
}

func TestRunJobBranchFilterSkipPropagatesToNeeds(t *testing.T) {
	wf := mustParse(t, `
name: ci
on: { push: {} }
jobs:
  lint:
    steps:
      - run: "true"
  build:
    branches: [main]
    steps:
      - run: "true"
  verify:
    needs: [build]
    steps:
      - run: "true"
`)
	st := store.NewMemory()
	run, _ := New(st).Run(context.Background(), wf,
		model.Event{Kind: model.EventPush, Branch: "dev"}, t.TempDir())
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %s, want success", run.Status)
	}
	if got := jobRun(run, "lint").Status; got != model.StatusSuccess {
		t.Errorf("lint status = %s, want success", got)
	}
	if got := jobRun(run, "build").Status; got != model.StatusSkipped {
		t.Errorf("build status = %s, want skipped (filter)", got)
	}
	if got := jobRun(run, "verify").Status; got != model.StatusSkipped {
		t.Errorf("verify status = %s, want skipped (needs a branch-skipped job)", got)
	}
}

func TestRunAllJobsBranchFilteredSkipsRun(t *testing.T) {
	wf := mustParse(t, `
name: ci
on: { push: {} }
jobs:
  deploy:
    branches: [main]
    steps:
      - run: "true"
`)
	st := store.NewMemory()
	run, _ := New(st).Run(context.Background(), wf,
		model.Event{Kind: model.EventPush, Branch: "dev"}, t.TempDir())
	if run.Status != model.StatusSkipped {
		t.Fatalf("run status = %s, want skipped when every job is filtered out", run.Status)
	}
	if !strings.Contains(run.Reason, "no job matches") {
		t.Errorf("run reason = %q, want it to explain the branch filter", run.Reason)
	}
}

func TestRunJobWorkingDirDefault(t *testing.T) {
	wf := mustParse(t, `
name: ci
on: { push: {} }
jobs:
  build:
    working-directory: a
    steps:
      - run: pwd
      - run: pwd
        working-directory: b
`)
	ws := t.TempDir()
	for _, d := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st := store.NewMemory()
	run, _ := New(st).Run(context.Background(), wf, model.Event{Kind: model.EventManual}, ws)
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %s, want success", run.Status)
	}
	if got := strings.TrimSpace(readStepLog(t, st, run.ID, "build", 0)); !strings.HasSuffix(got, "/a") {
		t.Errorf("step 0 pwd = %q, want the job default subdir a", got)
	}
	if got := strings.TrimSpace(readStepLog(t, st, run.ID, "build", 1)); !strings.HasSuffix(got, "/b") {
		t.Errorf("step 1 pwd = %q, want the step override subdir b", got)
	}
}

func TestRunMissingWorkingDirNamesError(t *testing.T) {
	wf := mustParse(t, `
name: ci
on: { push: {} }
jobs:
  build:
    working-directory: missing-dir
    steps:
      - run: pwd
`)
	st := store.NewMemory()
	run, _ := New(st).Run(context.Background(), wf, model.Event{Kind: model.EventManual}, t.TempDir())
	if run.Status != model.StatusFailed {
		t.Fatalf("run status = %s, want failed", run.Status)
	}
	step := jobRun(run, "build").Steps[0]
	if step.Status != model.StatusFailed || step.ExitCode != -1 {
		t.Fatalf("step = %s exit %d, want failed exit -1", step.Status, step.ExitCode)
	}
	// The process never started, so the log's only content must be the
	// janus-written reason — previously it was empty, leaving a bare exit -1.
	log := readStepLog(t, st, run.ID, "build", 0)
	if !strings.Contains(log, "janus:") || !strings.Contains(log, "missing-dir") {
		t.Errorf("step log = %q, want a janus: line naming the missing directory", log)
	}
}

func TestResolveDir(t *testing.T) {
	ws := t.TempDir()
	if got, err := resolveDir(ws, ""); err != nil || got != ws {
		t.Errorf("resolveDir(ws, \"\") = %q, %v; want the workspace root", got, err)
	}
	if got, err := resolveDir(ws, "sub/dir"); err != nil || got != filepath.Join(ws, "sub", "dir") {
		t.Errorf("resolveDir(ws, sub/dir) = %q, %v; want the joined path", got, err)
	}
	for _, escape := range []string{"..", "../outside", "a/../../outside"} {
		if _, err := resolveDir(ws, escape); err == nil {
			t.Errorf("resolveDir(ws, %q) should reject escaping the workspace", escape)
		}
	}
	// An absolute path is anchored under the workspace by filepath.Join, not
	// taken literally — "/etc" runs in <workspace>/etc, never the host's /etc.
	if got, err := resolveDir(ws, "/etc"); err != nil || got != filepath.Join(ws, "etc") {
		t.Errorf("resolveDir(ws, /etc) = %q, %v; want it anchored under the workspace", got, err)
	}
}

func TestRunCombinedLogOrdering(t *testing.T) {
	wf := mustParse(t, `
name: ci
on: { push: {} }
jobs:
  build:
    steps:
      - run: 'echo out; echo err 1>&2; echo out2'
`)
	st := store.NewMemory()
	run, _ := New(st).Run(context.Background(), wf, model.Event{Kind: model.EventManual}, t.TempDir())
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %s, want success", run.Status)
	}
	if got := readStepLog(t, st, run.ID, "build", 0); got != "out\nerr\nout2\n" {
		t.Errorf("combined log = %q, want stdout/stderr interleaved in order", got)
	}
}

func TestRunCancellation(t *testing.T) {
	wf := mustParse(t, `
name: ci
on: { push: {} }
jobs:
  build:
    steps:
      - run: sleep 30
`)
	st := store.NewMemory()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	run, _ := New(st).Run(ctx, wf, model.Event{Kind: model.EventManual}, t.TempDir())
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("cancellation took %v, expected prompt return", elapsed)
	}
	// External cancellation (shutdown, Ctrl-C) is an interruption, not a
	// verdict: the run reports cancelled, unlike an ordinary failure.
	if run.Status != model.StatusCancelled {
		t.Errorf("run status = %s, want cancelled", run.Status)
	}
	if got := jobRun(run, "build").Status; got != model.StatusCancelled {
		t.Errorf("build status = %s, want cancelled", got)
	}
}

// terminalFailStore fails the terminal UpdateRun (the write of a terminal
// status) failFirst times, then succeeds, counting terminal-write attempts.
// It simulates a full or briefly read-only disk under a running daemon.
type terminalFailStore struct {
	store.Store
	failFirst int
	attempts  int
}

func (s *terminalFailStore) UpdateRun(run *model.Run) error {
	if run.Status.Terminal() {
		s.attempts++
		if s.attempts <= s.failFirst {
			return errors.New("disk full")
		}
	}
	return s.Store.UpdateRun(run)
}

const okPipeline = `
name: ci
on: { push: {} }
jobs:
  build:
    steps:
      - run: echo hi
`

func TestFinalPersistFailureLogsAtError(t *testing.T) {
	finalPersistBackoff = 0
	t.Cleanup(func() { finalPersistBackoff = 250 * time.Millisecond })

	wf := mustParse(t, okPipeline)
	var buf bytes.Buffer
	st := &terminalFailStore{Store: store.NewMemory(), failFirst: finalPersistAttempts} // never succeeds
	eng := New(st, WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	run, err := eng.Run(context.Background(), wf, model.Event{Kind: model.EventManual}, t.TempDir())
	// The steps ran, so the in-memory run is success — but the terminal write
	// was abandoned, so Run must report the error and the engine must degrade.
	if err == nil {
		t.Fatal("Run should return the abandoned-terminal-write error")
	}
	if run == nil || run.Status != model.StatusSuccess {
		t.Errorf("run = %+v, want a success run alongside the error", run)
	}
	if !eng.Degraded() {
		t.Error("engine should be Degraded() after abandoning a terminal write")
	}
	// The terminal write is retried the full budget before giving up.
	if st.attempts != finalPersistAttempts {
		t.Errorf("terminal write attempts = %d, want %d", st.attempts, finalPersistAttempts)
	}
	// And the unpersistable terminal state is logged at Error, not Warn.
	if logs := buf.String(); !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "final run state") {
		t.Errorf("expected an error-level log about the final persist failure, got:\n%s", logs)
	}
}

func TestFinalPersistRetrySucceeds(t *testing.T) {
	finalPersistBackoff = 0
	t.Cleanup(func() { finalPersistBackoff = 250 * time.Millisecond })

	wf := mustParse(t, okPipeline)
	var buf bytes.Buffer
	st := &terminalFailStore{Store: store.NewMemory(), failFirst: 1} // succeeds on the retry
	eng := New(st, WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	run, err := eng.Run(context.Background(), wf, model.Event{Kind: model.EventManual}, t.TempDir())
	if err != nil {
		t.Fatalf("Run: a recovered retry should not error: %v", err)
	}
	if eng.Degraded() {
		t.Error("a recovered retry should not degrade the engine")
	}
	stored, err := st.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.Status != model.StatusSuccess {
		t.Errorf("stored run status = %s, want success (retry should persist it)", stored.Status)
	}
	if strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("a recovered retry should not log at Error:\n%s", buf.String())
	}
}

// TestRunInterpolationOverflowFailsStep proves a bounded pipeline that expands
// hugely under interpolation fails the step cleanly instead of OOMing.
func TestRunInterpolationOverflowFailsStep(t *testing.T) {
	big := strings.Repeat("x", 65000) // under the per-env-value cap
	// 20 references × 65000 bytes expands past the 1 MiB command cap.
	src := "name: ci\non: { push: {} }\nenv: { BIG: \"" + big + "\" }\njobs:\n  build:\n    steps:\n      - run: \"" + strings.Repeat("${{ env.BIG }}", 20) + "\"\n"
	wf := mustParse(t, src)
	st := store.NewMemory()
	run, _ := New(st).Run(context.Background(), wf, model.Event{Kind: model.EventManual}, t.TempDir())
	if run.Status != model.StatusFailed {
		t.Fatalf("run status = %s, want failed (interpolation overflow)", run.Status)
	}
	log := readStepLog(t, st, run.ID, "build", 0)
	if !strings.Contains(log, "exceeds") {
		t.Errorf("step log = %q, want an interpolation-limit error", log)
	}
}

// TestRunFailureStaysFailed pins the boundary of the cancelled status: the
// fail-fast sibling cancellation inside a run must not relabel an ordinary
// failing run as cancelled.
func TestRunFailureStaysFailed(t *testing.T) {
	wf := mustParse(t, `
name: ci
on: { push: {} }
jobs:
  build:
    steps:
      - run: exit 1
`)
	st := store.NewMemory()
	run, _ := New(st).Run(context.Background(), wf, model.Event{Kind: model.EventManual}, t.TempDir())
	if run.Status != model.StatusFailed {
		t.Errorf("run status = %s, want failed", run.Status)
	}
}

func TestRunStepTimeout(t *testing.T) {
	wf := mustParse(t, `
name: ci
on: { push: {} }
jobs:
  build:
    steps:
      - run: sleep 30
`)
	st := store.NewMemory()
	e := New(st, WithStepTimeout(200*time.Millisecond))

	start := time.Now()
	run, _ := e.Run(context.Background(), wf, model.Event{Kind: model.EventManual}, t.TempDir())
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("step timeout took %v, expected prompt failure", elapsed)
	}
	if run.Status != model.StatusFailed {
		t.Errorf("run status = %s, want failed", run.Status)
	}
	if got := jobRun(run, "build").Steps[0].Status; got != model.StatusFailed {
		t.Errorf("step status = %s, want failed (timed out)", got)
	}
}

// TestSchedulerParallelism drives the scheduler with a fake job runner so it can
// measure real concurrency deterministically, independent of process execution.
func TestSchedulerParallelism(t *testing.T) {
	wf := mustParse(t, `
name: ci
on: { push: {} }
jobs:
  a:
    steps:
      - run: "true"
  b:
    steps:
      - run: "true"
  c:
    steps:
      - run: "true"
`)
	for _, tc := range []struct{ max, wantPeak int }{{1, 1}, {3, 3}} {
		var mu sync.Mutex
		var cur, peak int
		st := store.NewMemory()
		e := New(st, WithMaxParallelJobs(tc.max))
		e.runJob = func(_ context.Context, rs *runState, _ *model.Job, jr *model.JobRun) model.Status {
			mu.Lock()
			cur++
			if cur > peak {
				peak = cur
			}
			mu.Unlock()
			time.Sleep(40 * time.Millisecond) // hold the slot to expose overlap
			mu.Lock()
			cur--
			mu.Unlock()
			rs.update(func() { jr.Status = model.StatusSuccess })
			return model.StatusSuccess
		}
		run, err := e.Run(context.Background(), wf, model.Event{Kind: model.EventManual}, t.TempDir())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if run.Status != model.StatusSuccess {
			t.Errorf("max=%d: run status = %s, want success", tc.max, run.Status)
		}
		if peak != tc.wantPeak {
			t.Errorf("max=%d: peak concurrency = %d, want %d", tc.max, peak, tc.wantPeak)
		}
	}
}

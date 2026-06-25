package engine

import (
	"context"
	"io"
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
	rc, err := st.ReadLogs(runID, job, idx)
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
	if run.Status != model.StatusFailed {
		t.Errorf("run status = %s, want failed", run.Status)
	}
	if got := jobRun(run, "build").Status; got != model.StatusCancelled {
		t.Errorf("build status = %s, want cancelled", got)
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

//go:build windows

package engine

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hutusi/janus/internal/model"
	"github.com/hutusi/janus/internal/pipeline"
	"github.com/hutusi/janus/internal/store"
)

// Helpers duplicated from engine_test.go, which is //go:build unix and so is not
// compiled on Windows. The two never coexist, so there is no symbol clash.

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

// Assertions use Contains/TrimSpace to tolerate cmd/powershell CRLF and quoting.

// TestWindowsCmdStepsAndExitCode runs default (cmd) steps and checks exit-code
// propagation and fail-stop of the following step.
func TestWindowsCmdStepsAndExitCode(t *testing.T) {
	wf := mustParse(t, `
name: ci
on: { push: {} }
jobs:
  build:
    steps:
      - run: echo hello
      - run: exit 3
      - run: echo should-not-run
`)
	st := store.NewMemory()
	run, _ := New(st).Run(context.Background(), wf, model.Event{Kind: model.EventManual}, t.TempDir())
	if run.Status != model.StatusFailed {
		t.Fatalf("run status = %s, want failed", run.Status)
	}
	build := jobRun(run, "build")
	if got := strings.TrimSpace(readStepLog(t, st, run.ID, "build", 0)); !strings.Contains(got, "hello") {
		t.Errorf("step 0 log = %q, want it to contain hello", got)
	}
	if build.Steps[1].Status != model.StatusFailed || build.Steps[1].ExitCode != 3 {
		t.Errorf("step 1 = %s/%d, want failed/3", build.Steps[1].Status, build.Steps[1].ExitCode)
	}
	if build.Steps[2].Status != model.StatusSkipped {
		t.Errorf("step 2 = %s, want skipped", build.Steps[2].Status)
	}
}

// TestWindowsPowerShellStep exercises the `shell: powershell` selector plus
// ${{ ... }} interpolation.
func TestWindowsPowerShellStep(t *testing.T) {
	wf := mustParse(t, `
name: ci
on: { push: {} }
jobs:
  build:
    steps:
      - run: Write-Output "branch=${{ branch }}"
        shell: powershell
`)
	st := store.NewMemory()
	run, _ := New(st).Run(context.Background(), wf, model.Event{Kind: model.EventManual, Branch: "main"}, t.TempDir())
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %s, want success", run.Status)
	}
	if got := readStepLog(t, st, run.ID, "build", 0); !strings.Contains(got, "branch=main") {
		t.Errorf("powershell step log = %q, want it to contain branch=main", got)
	}
}

// TestWindowsStepTimeout verifies a per-step timeout tears down a long cmd step
// (taskkill tree-kill) promptly rather than waiting it out.
func TestWindowsStepTimeout(t *testing.T) {
	wf := mustParse(t, `
name: ci
on: { push: {} }
jobs:
  build:
    steps:
      - run: ping -n 31 127.0.0.1 >nul
`)
	st := store.NewMemory()
	e := New(st, WithStepTimeout(300*time.Millisecond))
	start := time.Now()
	run, _ := e.Run(context.Background(), wf, model.Event{Kind: model.EventManual}, t.TempDir())
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("step timeout took %v, expected prompt failure (taskkill)", elapsed)
	}
	if run.Status != model.StatusFailed {
		t.Errorf("run status = %s, want failed", run.Status)
	}
	if got := jobRun(run, "build").Steps[0].Status; got != model.StatusFailed {
		t.Errorf("step status = %s, want failed (timed out)", got)
	}
}

// TestWindowsCancellation verifies ctx cancellation promptly kills a long cmd step.
func TestWindowsCancellation(t *testing.T) {
	wf := mustParse(t, `
name: ci
on: { push: {} }
jobs:
  build:
    steps:
      - run: ping -n 31 127.0.0.1 >nul
`)
	st := store.NewMemory()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	run, _ := New(st).Run(ctx, wf, model.Event{Kind: model.EventManual}, t.TempDir())
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("cancellation took %v, expected prompt return", elapsed)
	}
	if got := jobRun(run, "build").Status; got != model.StatusCancelled {
		t.Errorf("build status = %s, want cancelled", got)
	}
}

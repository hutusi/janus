package store

import (
	"io"
	"testing"
	"time"

	"github.com/hutusi/janus/internal/model"
)

func sampleRun(id string, created time.Time) *model.Run {
	return &model.Run{
		ID:           id,
		WorkflowName: "ci",
		Status:       model.StatusSuccess,
		CreatedAt:    created,
		Jobs:         []*model.JobRun{{Name: "build", Status: model.StatusSuccess, Steps: []*model.StepRun{{Index: 0, Command: "echo hi", Status: model.StatusSuccess}}}},
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	run := sampleRun("abc123", time.Now())
	if err := st.SaveRun(run); err != nil {
		t.Fatal(err)
	}

	w, err := st.LogWriter(run.ID, "build", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, "log line\n")
	_ = w.Close()

	got, err := st.GetRun("abc123")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.WorkflowName != "ci" || len(got.Jobs) != 1 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestFileStoreSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewFile(dir)
	_ = st.SaveRun(sampleRun("r1", time.Now().Add(-time.Minute)))
	_ = st.SaveRun(sampleRun("r2", time.Now()))
	w, _ := st.LogWriter("r2", "build", 0)
	_, _ = io.WriteString(w, "persisted output\n")
	_ = w.Close()

	// A fresh store over the same directory sees everything.
	reopened, err := NewFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := reopened.ListRuns(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("ListRuns = %d runs, want 2", len(runs))
	}
	if runs[0].ID != "r2" { // newest first
		t.Errorf("ListRuns order = %s first, want r2", runs[0].ID)
	}

	rc, _ := reopened.ReadLogs("r2", "build", 0, 0)
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	if string(b) != "persisted output\n" {
		t.Errorf("logs after restart = %q, want persisted output", b)
	}
}

func TestFileReadLogsOffset(t *testing.T) {
	st, _ := NewFile(t.TempDir())
	_ = st.SaveRun(sampleRun("r1", time.Now()))
	w, _ := st.LogWriter("r1", "build", 0)
	_, _ = io.WriteString(w, "hello world")
	_ = w.Close()

	readAt := func(off int64) string {
		t.Helper()
		rc, err := st.ReadLogs("r1", "build", 0, off)
		if err != nil {
			t.Fatalf("ReadLogs(offset=%d): %v", off, err)
		}
		defer func() { _ = rc.Close() }()
		b, _ := io.ReadAll(rc)
		return string(b)
	}
	if got := readAt(6); got != "world" {
		t.Errorf("offset 6 = %q, want world", got)
	}
	if got := readAt(1000); got != "" {
		t.Errorf("offset past end = %q, want empty", got)
	}
}

func TestFileStoreMissingRun(t *testing.T) {
	st, _ := NewFile(t.TempDir())
	if _, err := st.GetRun("nope"); err == nil {
		t.Error("expected error for missing run")
	}
	// Reading logs for a missing step yields empty, not an error.
	rc, err := st.ReadLogs("nope", "build", 0, 0)
	if err != nil {
		t.Fatalf("ReadLogs missing: %v", err)
	}
	_ = rc.Close()
}

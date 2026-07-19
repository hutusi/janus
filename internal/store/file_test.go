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

func TestFileListRunsPagination(t *testing.T) {
	st, _ := NewFile(t.TempDir())
	base := time.Now()
	// r0 newest … r4 oldest.
	for i := 0; i < 5; i++ {
		_ = st.SaveRun(sampleRun("r"+string(rune('0'+i)), base.Add(time.Duration(-i)*time.Minute)))
	}
	ids := func(limit, offset int) []string {
		t.Helper()
		runs, err := st.ListRuns(limit, offset)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, len(runs))
		for i, r := range runs {
			got[i] = r.ID
		}
		return got
	}
	if got := ids(2, 0); len(got) != 2 || got[0] != "r0" || got[1] != "r1" {
		t.Errorf("page 1 = %v, want [r0 r1]", got)
	}
	if got := ids(2, 2); len(got) != 2 || got[0] != "r2" || got[1] != "r3" {
		t.Errorf("page 2 = %v, want [r2 r3]", got)
	}
	if got := ids(10, 4); len(got) != 1 || got[0] != "r4" {
		t.Errorf("last page = %v, want [r4]", got)
	}
	if got := ids(10, 99); len(got) != 0 {
		t.Errorf("offset past end = %v, want empty", got)
	}
}

func TestFilePruneKeepsNewestTerminal(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewFile(dir)
	base := time.Now()
	for i := 0; i < 4; i++ { // r0 newest … r3 oldest, all terminal
		_ = st.SaveRun(sampleRun("r"+string(rune('0'+i)), base.Add(time.Duration(-i)*time.Minute)))
	}
	// A running run older than the cap must survive — never prune non-terminal.
	running := sampleRun("live", base.Add(-10*time.Minute))
	running.Status = model.StatusRunning
	_ = st.SaveRun(running)
	// Give one victim a log file so we can confirm the whole dir goes.
	w, _ := st.LogWriter("r3", "build", 0)
	_, _ = io.WriteString(w, "old output")
	_ = w.Close()

	removed, err := st.Prune(2)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 2 { // r2 and r3 are the terminal runs beyond the newest 2
		t.Fatalf("removed = %d, want 2", removed)
	}
	for _, id := range []string{"r0", "r1", "live"} {
		if _, err := st.GetRun(id); err != nil {
			t.Errorf("run %s should survive: %v", id, err)
		}
	}
	for _, id := range []string{"r2", "r3"} {
		if _, err := st.GetRun(id); err == nil {
			t.Errorf("run %s should have been pruned", id)
		}
	}
	// keep=0 is a no-op.
	if n, _ := st.Prune(0); n != 0 {
		t.Errorf("Prune(0) removed %d, want 0", n)
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
	runs, err := reopened.ListRuns(0, 0)
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

func TestFileReadLogsTail(t *testing.T) {
	st, _ := NewFile(t.TempDir())
	_ = st.SaveRun(sampleRun("r1", time.Now()))
	w, _ := st.LogWriter("r1", "build", 0)
	_, _ = io.WriteString(w, "hello world")
	_ = w.Close()

	tailOf := func(max int64) (string, bool) {
		t.Helper()
		rc, truncated, err := st.ReadLogsTail("r1", "build", 0, max)
		if err != nil {
			t.Fatalf("ReadLogsTail(%d): %v", max, err)
		}
		defer func() { _ = rc.Close() }()
		b, _ := io.ReadAll(rc)
		return string(b), truncated
	}
	if got, tr := tailOf(5); got != "world" || !tr {
		t.Errorf("tail(5) = %q,%v, want world,true", got, tr)
	}
	if got, tr := tailOf(11); got != "hello world" || tr { // exactly the length: not truncated
		t.Errorf("tail(11) = %q,%v, want the whole log,false", got, tr)
	}
	if got, tr := tailOf(0); got != "hello world" || tr {
		t.Errorf("tail(0) = %q,%v, want the whole log,false", got, tr)
	}
	if got, tr := tailOf(1000); got != "hello world" || tr {
		t.Errorf("tail(oversized) = %q,%v, want the whole log,false", got, tr)
	}
	rc, _, _ := st.ReadLogsTail("r1", "build", 9, 5) // missing step
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if len(b) != 0 {
		t.Errorf("missing-step tail = %q, want empty", b)
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

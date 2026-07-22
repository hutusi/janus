package store

import (
	"io"
	"testing"
	"time"

	"github.com/hutusi/janus/internal/model"
)

func TestMemoryPruneDropsRunsAndLogs(t *testing.T) {
	m := NewMemory()
	base := time.Now()
	for i := 0; i < 3; i++ { // r0 newest … r2 oldest, terminal
		_ = m.SaveRun(sampleRun("r"+string(rune('0'+i)), base.Add(time.Duration(-i)*time.Minute)))
	}
	w, _ := m.LogWriter("r2", "build", 0)
	_, _ = io.WriteString(w, "old")
	_ = w.Close()

	removed, err := m.Prune(1)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if _, err := m.GetRun("r0"); err != nil {
		t.Errorf("newest run should survive: %v", err)
	}
	if _, err := m.GetRun("r2"); err == nil {
		t.Error("oldest run should be pruned")
	}
	// Its logs are gone too (empty reader, no leaked buffer).
	rc, _ := m.ReadLogs("r2", "build", 0, 0)
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if len(b) != 0 {
		t.Errorf("pruned run's logs = %q, want empty", b)
	}
	if len(m.order) != 1 || m.order[0] != "r0" {
		t.Errorf("order = %v, want [r0]", m.order)
	}
}

// TestMemoryReturnsSnapshot pins the contract that fixes the data race between
// the API encoding a run and the engine mutating it: the store holds an
// immutable snapshot taken at write time, not the caller's live pointer.
func TestMemoryReturnsSnapshot(t *testing.T) {
	m := NewMemory()
	run := &model.Run{
		ID:     "x",
		Status: model.StatusRunning,
		Jobs:   []*model.JobRun{{Name: "a", Status: model.StatusPending}},
	}
	if err := m.SaveRun(run); err != nil {
		t.Fatal(err)
	}

	// Mutate the original after saving (as the engine does as the run progresses).
	run.Status = model.StatusSuccess
	run.Jobs[0].Status = model.StatusSuccess

	got, err := m.GetRun("x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusRunning {
		t.Errorf("stored Status = %s, want a snapshot frozen at Running", got.Status)
	}
	if got.Jobs[0].Status != model.StatusPending {
		t.Errorf("nested JobRun leaked a later mutation: %s", got.Jobs[0].Status)
	}
}

func TestMemoryLogRoundTrip(t *testing.T) {
	m := NewMemory()
	w, err := m.LogWriter("r", "build", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, "hello\n")
	_ = w.Close()

	rc, err := m.ReadLogs("r", "build", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	if string(b) != "hello\n" {
		t.Errorf("logs = %q, want hello", b)
	}
}

func TestMemoryReadLogsTail(t *testing.T) {
	m := NewMemory()
	w, _ := m.LogWriter("r", "build", 0)
	_, _ = io.WriteString(w, "hello world")
	_ = w.Close()

	tailOf := func(max int64) (string, bool) {
		t.Helper()
		rc, truncated, err := m.ReadLogsTail("r", "build", 0, max)
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
	rc, _, _ := m.ReadLogsTail("r", "build", 9, 5) // missing step
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if len(b) != 0 {
		t.Errorf("missing-step tail = %q, want empty", b)
	}
}

func TestMemoryReadLogsOffset(t *testing.T) {
	m := NewMemory()
	w, _ := m.LogWriter("r", "build", 0)
	_, _ = io.WriteString(w, "hello world")
	_ = w.Close()

	readAt := func(off int64) string {
		t.Helper()
		rc, err := m.ReadLogs("r", "build", 0, off)
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

func TestMemoryCountRuns(t *testing.T) {
	st := NewMemory()
	if n, err := st.CountRuns(); err != nil || n != 0 {
		t.Fatalf("empty CountRuns = %d, %v; want 0, nil", n, err)
	}
	base := time.Now()
	for i := 0; i < 3; i++ {
		_ = st.SaveRun(sampleRun("c"+string(rune('0'+i)), base.Add(time.Duration(-i)*time.Minute)))
	}
	if n, err := st.CountRuns(); err != nil || n != 3 {
		t.Fatalf("CountRuns = %d, %v; want 3, nil", n, err)
	}
}

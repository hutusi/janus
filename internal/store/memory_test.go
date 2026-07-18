package store

import (
	"io"
	"testing"

	"github.com/hutusi/janus/internal/model"
)

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

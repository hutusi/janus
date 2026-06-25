package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/hutusi/janus/internal/model"
)

// Memory is an in-memory Store used by tests and `janus run`. Run history is
// lost on exit. It is safe for concurrent use.
type Memory struct {
	mu    sync.Mutex
	runs  map[string]*model.Run
	order []string // run IDs in insertion order
	logs  map[string]*bytes.Buffer
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		runs: make(map[string]*model.Run),
		logs: make(map[string]*bytes.Buffer),
	}
}

func logKey(runID, job string, stepIndex int) string {
	return fmt.Sprintf("%s/%s/%d", runID, job, stepIndex)
}

func (m *Memory) SaveRun(run *model.Run) error {
	snap := cloneRun(run)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[run.ID]; !ok {
		m.order = append(m.order, run.ID)
	}
	m.runs[run.ID] = snap
	return nil
}

func (m *Memory) UpdateRun(run *model.Run) error {
	snap := cloneRun(run)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[run.ID]; !ok {
		return fmt.Errorf("run %q not found", run.ID)
	}
	m.runs[run.ID] = snap
	return nil
}

// cloneRun returns a deep, immutable snapshot of run. The engine mutates the
// live run under its own lock and calls Save/Update from there, so snapshotting
// at write time decouples the stored value from later mutation — readers
// (GetRun/ListRuns) then never race with the engine.
func cloneRun(run *model.Run) *model.Run {
	data, err := json.Marshal(run)
	if err != nil {
		return run
	}
	var c model.Run
	if err := json.Unmarshal(data, &c); err != nil {
		return run
	}
	return &c
}

func (m *Memory) GetRun(id string) (*model.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[id]
	if !ok {
		return nil, fmt.Errorf("run %q not found", id)
	}
	return run, nil
}

func (m *Memory) ListRuns(limit int) ([]*model.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	runs := make([]*model.Run, 0, len(m.order))
	for _, id := range m.order {
		runs = append(runs, m.runs[id])
	}
	sort.SliceStable(runs, func(i, j int) bool {
		return runs[i].CreatedAt.After(runs[j].CreatedAt)
	})
	if limit > 0 && len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

func (m *Memory) LogWriter(runID, job string, stepIndex int) (io.WriteCloser, error) {
	return &memLogWriter{m: m, key: logKey(runID, job, stepIndex)}, nil
}

func (m *Memory) ReadLogs(runID, job string, stepIndex int) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf := m.logs[logKey(runID, job, stepIndex)]
	if buf == nil {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	// Copy so the caller reads a stable snapshot.
	data := append([]byte(nil), buf.Bytes()...)
	return io.NopCloser(bytes.NewReader(data)), nil
}

type memLogWriter struct {
	m   *Memory
	key string
}

func (w *memLogWriter) Write(p []byte) (int, error) {
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	buf := w.m.logs[w.key]
	if buf == nil {
		buf = &bytes.Buffer{}
		w.m.logs[w.key] = buf
	}
	return buf.Write(p)
}

func (w *memLogWriter) Close() error { return nil }

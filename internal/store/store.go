// Package store persists runs and their logs. The interface is small enough to
// back with memory (tests, `janus run`) or flat files (the server), and could
// later be backed by anything richer without touching callers.
package store

import (
	"io"

	"github.com/hutusi/janus/internal/model"
)

// Store records run metadata and step logs.
//
// Run metadata and logs are stored separately: metadata is small and rewritten
// on every status change, while logs are an append-only stream written during
// execution and keyed by (runID, job, stepIndex).
type Store interface {
	// SaveRun records a newly created run.
	SaveRun(run *model.Run) error
	// UpdateRun persists the current state of an existing run.
	UpdateRun(run *model.Run) error
	// GetRun returns the run with the given ID, or an error if absent.
	GetRun(id string) (*model.Run, error)
	// ListRuns returns runs newest-first, capped at limit (0 = no cap).
	ListRuns(limit int) ([]*model.Run, error)

	// LogWriter returns an append-only sink for one step's combined output.
	LogWriter(runID, job string, stepIndex int) (io.WriteCloser, error)
	// ReadLogs returns a reader over one step's recorded output.
	ReadLogs(runID, job string, stepIndex int) (io.ReadCloser, error)
}

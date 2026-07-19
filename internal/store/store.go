// Package store persists runs and their logs. The interface is small enough to
// back with memory (tests, `janus run`) or flat files (the server), and could
// later be backed by anything richer without touching callers.
package store

import (
	"io"

	"github.com/hutusi/janus/internal/model"
)

// page applies an offset then a limit to an already-ordered run slice. offset
// past the end yields empty; limit <= 0 means no cap. Shared by both stores.
func page[T any](items []T, limit, offset int) []T {
	if offset > 0 {
		if offset >= len(items) {
			return []T{}
		}
		items = items[offset:]
	}
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

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
	// ListRuns returns compact run summaries newest-first, skipping the first
	// offset of them (offset <= 0 = from the start) and capping the result at
	// limit (0 = no cap). Summaries omit the heavy Jobs slice so a listing
	// never holds every full record in memory; full detail is via GetRun. An
	// offset past the end returns an empty slice, not an error.
	ListRuns(limit, offset int) ([]*model.RunSummary, error)
	// Prune deletes the oldest terminal runs (metadata and logs) beyond keep,
	// returning how many were removed. Non-terminal runs are never pruned and
	// do not count toward keep; keep <= 0 is a no-op.
	Prune(keep int) (removed int, err error)

	// LogWriter returns an append-only sink for one step's combined output.
	LogWriter(runID, job string, stepIndex int) (io.WriteCloser, error)
	// ReadLogs returns a reader over one step's recorded output, starting at
	// byte offset (0 = from the beginning; an offset at or past the end reads
	// as empty, not an error). The offset lets a follower poll a growing log
	// without re-reading the whole file each tick.
	ReadLogs(runID, job string, stepIndex int, offset int64) (io.ReadCloser, error)
	// ReadLogsTail returns a reader over at most the last maxBytes of one
	// step's output (maxBytes <= 0 = the whole log; a missing step reads as
	// empty) and reports whether the log was longer than maxBytes (i.e. the
	// start was cut). The reader yields no more than maxBytes even if the log
	// keeps growing, so the HTML page is bounded regardless of log size. The
	// tail may begin mid-line.
	ReadLogsTail(runID, job string, stepIndex int, maxBytes int64) (rc io.ReadCloser, truncated bool, err error)
}

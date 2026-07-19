package server

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/hutusi/janus/internal/model"
)

const maxListLimit = 500

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	runs, err := s.store.ListRuns(limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetRun(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// handleLogs serves logs as plain text. With ?job=&step= it returns (or, with
// ?follow=1, streams) one step's output; otherwise it returns a snapshot of the
// whole run's logs with a header per step.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetRun(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	job := r.URL.Query().Get("job")
	stepStr := r.URL.Query().Get("step")
	if job != "" && stepStr != "" {
		step, err := strconv.Atoi(stepStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "step must be an integer")
			return
		}
		if r.URL.Query().Get("follow") == "1" {
			s.streamStep(w, r, run.ID, job, step)
			return
		}
		s.copyStep(w, run.ID, job, step)
		return
	}
	s.writeRunLogs(w, run)
}

// writeRunLogs writes every step's logs in job/step order, each under a header.
func (s *Server) writeRunLogs(w io.Writer, run *model.Run) {
	for _, jr := range run.Jobs {
		for _, sr := range jr.Steps {
			_, _ = fmt.Fprintf(w, "=== %s / step %d [%s] ===\n", jr.Name, sr.Index, sr.Status)
			s.copyStep(w, run.ID, jr.Name, sr.Index)
			_, _ = io.WriteString(w, "\n")
		}
	}
}

// copyStep streams one step's full log to w without buffering it in memory.
func (s *Server) copyStep(w io.Writer, runID, job string, step int) {
	rc, err := s.store.ReadLogs(runID, job, step, 0)
	if err != nil {
		s.logger.Warn("read logs failed", "run", runID, "job", job, "step", step, "err", err)
		return
	}
	defer func() { _ = rc.Close() }()
	_, _ = io.Copy(w, rc)
}

// streamStep tails a single step's output until the step is terminal or the
// client disconnects, polling the store.
func (s *Server) streamStep(w http.ResponseWriter, r *http.Request, runID, job string, step int) {
	flusher, _ := w.(http.Flusher)
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	// Each tick reads only what appended since the last one — re-reading the
	// whole log every 300ms would make following an O(n²) disk workload.
	var offset int64
	flush := func() {
		rc, err := s.store.ReadLogs(runID, job, step, offset)
		if err != nil {
			s.logger.Warn("read logs failed", "run", runID, "job", job, "step", step, "err", err)
			return
		}
		n, _ := io.Copy(w, rc)
		_ = rc.Close()
		if n > 0 {
			offset += n
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	lookupErrs := 0
	for {
		flush()
		run, err := s.store.GetRun(runID)
		switch {
		case err == nil && stepTerminal(run, job, step):
			flush()
			return
		case err != nil:
			if lookupErrs++; lookupErrs >= 3 {
				s.logger.Warn("stopping log stream; run lookup keeps failing", "run", runID, "err", err)
				return
			}
		default:
			lookupErrs = 0
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func stepTerminal(run *model.Run, job string, step int) bool {
	for _, jr := range run.Jobs {
		if jr.Name != job {
			continue
		}
		for _, sr := range jr.Steps {
			if sr.Index == step {
				return sr.Status.Terminal()
			}
		}
	}
	return true // unknown step: don't hang
}

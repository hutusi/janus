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

// handleCancelRun requests cancellation of a pending or running run. The 202
// is asynchronous and best-effort: the cancel was delivered and the run will
// normally settle cancelled (reason "cancelled via API"), but a run whose last
// process exits concurrently still finishes on its own terms.
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.store.GetRun(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if run.Status.Terminal() {
		writeError(w, http.StatusConflict, fmt.Sprintf("run %s already finished (%s)", id, run.Status))
		return
	}
	if s.runner == nil || !s.runner.Cancel(id, "cancelled via API") {
		// The run settled between GetRun and Cancel (or this server has no
		// runner and nothing to cancel with).
		writeError(w, http.StatusConflict, fmt.Sprintf("run %s already finished", id))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling", "run_id": id})
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

// Bounds on log following. Each follower costs a goroutine and polls the store
// ~3×/second for as long as it is connected, and a run queued behind a
// concurrency group can keep one connected for its whole wait. Read endpoints
// are open when no api_token is configured (the documented default) and the
// server deliberately sets no WriteTimeout (it would cut streams short), so
// without these a handful of clients is enough to keep the store busy
// indefinitely. Vars, not consts, only so tests can shrink them.
var (
	maxFollowers      = 32
	maxFollowDuration = 15 * time.Minute
)

// streamStep tails a single step's output until the step is terminal, the
// client disconnects, or the follow duration is reached, polling the store.
func (s *Server) streamStep(w http.ResponseWriter, r *http.Request, runID, job string, step int) {
	select {
	case s.followers <- struct{}{}:
		defer func() { <-s.followers }()
	default:
		// Nothing has been written yet, so writeError can still set its own
		// headers and status.
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "too many log followers; retry shortly")
		return
	}

	flusher, _ := w.(http.Flusher)
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	// A disconnect cancels the request context, but a client that connects and
	// then simply stops reading does not — it would hold its slot forever.
	deadline := time.NewTimer(maxFollowDuration)
	defer deadline.Stop()

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
		case <-deadline.C:
			_, _ = fmt.Fprintf(w, "\njanus: log follow ended after %s; reconnect to continue\n", maxFollowDuration)
			if flusher != nil {
				flusher.Flush()
			}
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

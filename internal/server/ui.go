package server

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"time"

	"github.com/hutusi/janus/internal/model"
)

// The HTML run page buffers its output and auto-refreshes, so it must be
// bounded regardless of log size or step count: at most runPageStepTailBytes
// of each step's log (its interesting end) and at most runPageTotalBytes across
// the whole page. Only the tail is shown, with a pointer to the full logs.
// Vars so tests can shrink them.
var (
	runPageStepTailBytes int64 = 64 << 10 // per step
	runPageTotalBytes    int64 = 1 << 20  // whole page
)

//go:embed templates/*.html
var templateFS embed.FS

var templateFuncs = template.FuncMap{
	"shortID": func(id string) string {
		if len(id) > 8 {
			return id[:8]
		}
		return id
	},
	"reltime": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.Format("2006-01-02 15:04:05")
	},
}

// indexData is the model for the run-list page.
type indexData struct {
	Version string
	Runs    []*model.Run
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	runs, err := s.store.ListRuns(50, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "list.html", indexData{Version: s.version, Runs: runs})
}

// runPageData is the model for the run-detail page.
type runPageData struct {
	Run     *model.Run
	Logs    string
	Refresh bool // auto-refresh while the run is not terminal
}

func (s *Server) handleRunPage(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetRun(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	var logs bytes.Buffer
	s.writeRunLogsTail(&logs, run, runPageStepTailBytes, runPageTotalBytes)
	s.render(w, "run.html", runPageData{
		Run:     run,
		Logs:    logs.String(),
		Refresh: !run.Status.Terminal(),
	})
}

// writeRunLogsTail is writeRunLogs for the HTML page: it renders the last
// perStep bytes of each step (with a marker when truncated), and stops once a
// running total budget is spent, so the page is bounded by total regardless of
// log size or step count. The API snapshot and streaming endpoints keep using
// the unbounded writeRunLogs / streamStep.
func (s *Server) writeRunLogsTail(w io.Writer, run *model.Run, perStep, total int64) {
	remaining := total
	writeCount := func(str string) { _, _ = io.WriteString(w, str); remaining -= int64(len(str)) }
	for _, jr := range run.Jobs {
		for _, sr := range jr.Steps {
			if remaining <= 0 {
				_, _ = fmt.Fprintf(w, "… (page size limit reached — remaining step logs omitted; full logs at /api/runs/%s/logs)\n", run.ID)
				return
			}
			writeCount(fmt.Sprintf("=== %s / step %d [%s] ===\n", jr.Name, sr.Index, sr.Status))
			if remaining > 0 {
				allowance := perStep
				if allowance > remaining {
					allowance = remaining
				}
				remaining -= s.copyStepTail(w, run.ID, jr.Name, sr.Index, allowance)
			}
			writeCount("\n")
		}
	}
}

// copyStepTail writes at most maxBytes of one step's log, prefixed with a
// marker when the log was longer so the reader knows the start was cut, and
// returns how many log bytes it wrote. The tail is read into memory first
// (bounded by maxBytes) so the marker can lead.
func (s *Server) copyStepTail(w io.Writer, runID, job string, step int, maxBytes int64) int64 {
	rc, truncated, err := s.store.ReadLogsTail(runID, job, step, maxBytes)
	if err != nil {
		s.logger.Warn("read logs failed", "run", runID, "job", job, "step", step, "err", err)
		return 0
	}
	defer func() { _ = rc.Close() }()
	tail, _ := io.ReadAll(rc) // bounded by maxBytes
	if truncated {
		_, _ = fmt.Fprintf(w, "… (earlier output truncated — full logs at /api/runs/%s/logs?job=%s&step=%d)\n", runID, job, step)
	}
	n, _ := w.Write(tail)
	return int64(n)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = buf.WriteTo(w)
}

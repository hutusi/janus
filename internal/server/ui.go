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

// runPageStepTailBytes caps how much of each step's log the HTML run page
// renders. The page buffers its output and auto-refreshes, so an unbounded log
// would OOM the daemon; only the tail is shown (the interesting end of a build)
// with a pointer to the full logs. A var so tests can shrink it.
var runPageStepTailBytes int64 = 64 << 10

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
	s.writeRunLogsTail(&logs, run, runPageStepTailBytes)
	s.render(w, "run.html", runPageData{
		Run:     run,
		Logs:    logs.String(),
		Refresh: !run.Status.Terminal(),
	})
}

// writeRunLogsTail is writeRunLogs for the HTML page: it renders only the last
// tailBytes of each step (with a marker when truncated), so total page memory
// is bounded by numSteps × tailBytes regardless of log size. The API snapshot
// and streaming endpoints keep using the unbounded writeRunLogs / streamStep.
func (s *Server) writeRunLogsTail(w io.Writer, run *model.Run, tailBytes int64) {
	for _, jr := range run.Jobs {
		for _, sr := range jr.Steps {
			_, _ = fmt.Fprintf(w, "=== %s / step %d [%s] ===\n", jr.Name, sr.Index, sr.Status)
			s.copyStepTail(w, run.ID, jr.Name, sr.Index, tailBytes)
			_, _ = io.WriteString(w, "\n")
		}
	}
}

// copyStepTail writes at most tailBytes of one step's log, prefixed with a
// marker when the log was longer so the reader knows the start was cut. The
// tail is read into memory first (bounded by tailBytes) so the marker can lead.
func (s *Server) copyStepTail(w io.Writer, runID, job string, step int, tailBytes int64) {
	rc, err := s.store.ReadLogsTail(runID, job, step, tailBytes)
	if err != nil {
		s.logger.Warn("read logs failed", "run", runID, "job", job, "step", step, "err", err)
		return
	}
	defer func() { _ = rc.Close() }()
	tail, _ := io.ReadAll(rc) // ReadLogsTail caps this at tailBytes
	if tailBytes > 0 && int64(len(tail)) >= tailBytes {
		_, _ = fmt.Fprintf(w, "… (earlier output truncated — full logs at /api/runs/%s/logs?job=%s&step=%d)\n", runID, job, step)
	}
	_, _ = w.Write(tail)
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

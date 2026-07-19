package server

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
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
	// trunc bounds a metadata string rendered in the dashboard (command, job
	// or workflow name, branch) so a large value stored on an old run — or one
	// that predates the ingestion caps — can't bloat the page.
	"trunc": func(n int, s string) string {
		if len(s) > n {
			return s[:n] + "…"
		}
		return s
	},
	// needs renders a job's needs list, bounded like trunc.
	"needs": func(n []string) string {
		s := strings.Join(n, ", ")
		if len(s) > 200 {
			return s[:200] + "…"
		}
		return s
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

// limitedWriter caps how many bytes reach the underlying writer. Writes past
// the budget are dropped and flip truncated; Write always reports len(p) so
// callers (Fprintf, io.Copy, io.WriteString) never see a short write and retry.
type limitedWriter struct {
	w         io.Writer
	remaining int64
	truncated bool
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.remaining <= 0 {
		lw.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > lw.remaining {
		_, err := lw.w.Write(p[:lw.remaining])
		lw.remaining = 0
		lw.truncated = true
		return len(p), err
	}
	n, err := lw.w.Write(p)
	lw.remaining -= int64(n)
	return n, err
}

// writeRunLogsTail is writeRunLogs for the HTML page: it renders the last
// perStep bytes of each step (with a marker when truncated). Everything —
// headers, markers, and tails — goes through a byte-limited writer, so the
// rendered logs are hard-capped at total regardless of job-name length, step
// count, or log size. The API snapshot and streaming endpoints keep using the
// unbounded writeRunLogs / streamStep.
func (s *Server) writeRunLogsTail(w io.Writer, run *model.Run, perStep, total int64) {
	lw := &limitedWriter{w: w, remaining: total}
	for _, jr := range run.Jobs {
		for _, sr := range jr.Steps {
			// Stop reading once the budget is spent: without this the loop would
			// still call ReadLogsTail for every remaining step, reading the
			// whole log set from disk (gigabytes) just to drop it.
			if lw.remaining <= 0 {
				lw.truncated = true
				goto done
			}
			_, _ = fmt.Fprintf(lw, "=== %s / step %d [%s] ===\n", jr.Name, sr.Index, sr.Status)
			s.copyStepTail(lw, run.ID, jr.Name, sr.Index, perStep)
			_, _ = io.WriteString(lw, "\n")
		}
	}
done:
	if lw.truncated {
		// A fixed trailer, written outside the budget (so its own bytes can't
		// be dropped), telling the reader where the full logs live.
		_, _ = fmt.Fprintf(w, "\n… (page size limit reached — output truncated; full logs at /api/runs/%s/logs)\n", run.ID)
	}
}

// copyStepTail writes at most maxBytes of one step's log through w, prefixed
// with a marker when the log was longer so the reader knows the start was cut.
// The tail is read into memory first (bounded by maxBytes) so the marker leads.
func (s *Server) copyStepTail(w io.Writer, runID, job string, step int, maxBytes int64) {
	rc, truncated, err := s.store.ReadLogsTail(runID, job, step, maxBytes)
	if err != nil {
		s.logger.Warn("read logs failed", "run", runID, "job", job, "step", step, "err", err)
		return
	}
	defer func() { _ = rc.Close() }()
	tail, _ := io.ReadAll(rc) // bounded by maxBytes
	if truncated {
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

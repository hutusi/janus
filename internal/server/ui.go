package server

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
	"time"

	"github.com/hutusi/janus/internal/model"
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
	runs, err := s.store.ListRuns(50)
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
	s.writeRunLogs(&logs, run)
	s.render(w, "run.html", runPageData{
		Run:     run,
		Logs:    logs.String(),
		Refresh: !run.Status.Terminal(),
	})
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

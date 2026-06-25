// Package server exposes Janus over HTTP: a JSON API (manual trigger, run list,
// run detail, logs), a read-only HTML dashboard, and a health check. Provider
// webhooks are mounted here too (added in Phase 5).
package server

import (
	"encoding/json"
	"html/template"
	"net/http"

	"github.com/hutusi/janus/internal/runner"
	"github.com/hutusi/janus/internal/store"
)

// Server holds the dependencies shared by all handlers.
type Server struct {
	store   store.Store
	runner  *runner.Runner
	version string
	mux     *http.ServeMux
	tmpl    *template.Template
}

// New builds a Server and registers its routes.
func New(st store.Store, rn *runner.Runner, version string) *Server {
	s := &Server{
		store:   st,
		runner:  rn,
		version: version,
		mux:     http.NewServeMux(),
		tmpl:    template.Must(template.New("").Funcs(templateFuncs).ParseFS(templateFS, "templates/*.html")),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	// JSON API
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("POST /api/trigger", s.handleTrigger)
	s.mux.HandleFunc("GET /api/runs", s.handleListRuns)
	s.mux.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	s.mux.HandleFunc("GET /api/runs/{id}/logs", s.handleLogs)

	// Read-only HTML dashboard
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /runs/{id}", s.handleRunPage)
}

// Handler returns the HTTP handler for the server.
func (s *Server) Handler() http.Handler { return s.mux }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.version})
}

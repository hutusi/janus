// Package server exposes Janus over HTTP: a JSON API (manual trigger, run list,
// run detail, logs), a read-only HTML dashboard, and a health check. Provider
// webhooks are mounted here too (added in Phase 5).
package server

import (
	"crypto/subtle"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hutusi/janus/internal/provider"
	"github.com/hutusi/janus/internal/runner"
	"github.com/hutusi/janus/internal/store"
)

// registeredProvider pairs a webhook provider with its configured secret.
type registeredProvider struct {
	provider provider.Provider
	secret   string
}

// Server holds the dependencies shared by all handlers.
type Server struct {
	store     store.Store
	runner    *runner.Runner
	version   string
	logger    *slog.Logger
	apiToken  string // if set, /api/* requires this bearer token
	providers map[string]registeredProvider
	mux       *http.ServeMux
	tmpl      *template.Template
}

// Option configures a Server.
type Option func(*Server)

// WithLogger sets the logger used for webhook/run diagnostics.
func WithLogger(l *slog.Logger) Option { return func(s *Server) { s.logger = l } }

// WithAPIToken requires a `Authorization: Bearer <token>` header on /api/*
// routes. Empty (the default) leaves the API open.
func WithAPIToken(token string) Option { return func(s *Server) { s.apiToken = token } }

// WithProvider registers a webhook provider and its secret, enabling
// POST /webhooks/<provider.Name()>.
func WithProvider(p provider.Provider, secret string) Option {
	return func(s *Server) {
		s.providers[p.Name()] = registeredProvider{provider: p, secret: secret}
	}
}

// New builds a Server and registers its routes.
func New(st store.Store, rn *runner.Runner, version string, opts ...Option) *Server {
	s := &Server{
		store:     st,
		runner:    rn,
		version:   version,
		logger:    slog.Default(),
		providers: map[string]registeredProvider{},
		mux:       http.NewServeMux(),
		tmpl:      template.Must(template.New("").Funcs(templateFuncs).ParseFS(templateFS, "templates/*.html")),
	}
	for _, o := range opts {
		o(s)
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	// Health and webhooks: open (webhooks authenticate via the provider secret).
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("POST /webhooks/{provider}", s.handleWebhook)

	// /api/trigger executes code on the host, so it ALWAYS requires a token —
	// if none is configured it is disabled (403) rather than left open.
	s.mux.HandleFunc("POST /api/trigger", s.requireToken(s.handleTrigger))
	// Read endpoints are optionally bearer-protected.
	s.mux.HandleFunc("GET /api/runs", s.requireAuth(s.handleListRuns))
	s.mux.HandleFunc("GET /api/runs/{id}", s.requireAuth(s.handleGetRun))
	s.mux.HandleFunc("GET /api/runs/{id}/logs", s.requireAuth(s.handleLogs))

	// Read-only HTML dashboard (unauthenticated; put a proxy in front if needed).
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /runs/{id}", s.handleRunPage)
}

// bearerOK reports whether the request carries the configured API token.
func (s *Server) bearerOK(r *http.Request) bool {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	tok := strings.TrimPrefix(auth, prefix)
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.apiToken)) == 1
}

// requireAuth enforces the bearer token only when one is configured.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiToken != "" && !s.bearerOK(r) {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next(w, r)
	}
}

// requireToken mandates a configured bearer token; without one the route is
// disabled (used for the code-executing /api/trigger).
func (s *Server) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiToken == "" {
			writeError(w, http.StatusForbidden, "/api/trigger is disabled; start janus with --api-token to enable it")
			return
		}
		if !s.bearerOK(r) {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next(w, r)
	}
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

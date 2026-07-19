package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/hutusi/janus/internal/model"
	"github.com/hutusi/janus/internal/runner"
)

type triggerRequest struct {
	RepoURL      string `json:"repo_url"`
	Branch       string `json:"branch"`
	SHA          string `json:"sha"`
	Ref          string `json:"ref"`
	PipelinePath string `json:"pipeline_path"` // optional; relative to the configured pipeline file's directory
}

// maxTriggerBody caps a manual-trigger request. The body is a handful of
// short strings; 1 MiB is generous.
const maxTriggerBody = 1 << 20

// handleTrigger starts a run manually. It builds a normalized manual Event and
// hands it to the runner, which checks out, parses, and executes the pipeline.
// Decoding is strict, matching the YAML parsers: unknown fields and trailing
// data are errors, not silently ignored.
func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) {
	var req triggerRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxTriggerBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	// Require the body to be exactly one JSON value: a second Decode must hit
	// EOF. dec.More() is not enough — it returns false for a trailing `]`/`}`,
	// so `{...}]` would slip through, and it can mask a body-limit error hit
	// while scanning trailing bytes.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "unexpected data after the JSON body")
		return
	}
	if req.RepoURL == "" {
		writeError(w, http.StatusBadRequest, "repo_url is required")
		return
	}
	if req.SHA == "" && req.Ref == "" {
		writeError(w, http.StatusBadRequest, "one of sha or ref is required")
		return
	}

	ev := model.Event{
		Provider:     "manual",
		Kind:         model.EventManual,
		RepoURL:      req.RepoURL,
		Branch:       req.Branch,
		SHA:          req.SHA,
		Ref:          req.Ref,
		PipelinePath: req.PipelinePath,
	}
	if ev.Branch == "" {
		ev.Branch = strings.TrimPrefix(req.Ref, "refs/heads/")
	}

	res, err := s.runner.Trigger(r.Context(), ev)
	if errors.Is(err, runner.ErrRepoNotAllowed) {
		s.logger.Warn("trigger rejected: repo not allowed", "repo", ev.RepoURL)
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	if errors.Is(err, runner.ErrBusy) || errors.Is(err, runner.ErrStoreUnavailable) {
		// Both are Janus-side transient conditions: retry, don't treat as a
		// client (400) error.
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"run_id": res.RunID})
}

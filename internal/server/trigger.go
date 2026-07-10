package server

import (
	"encoding/json"
	"errors"
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
	PipelinePath string `json:"pipeline_path"` // optional; must be in the configured pipeline file's directory
}

// handleTrigger starts a run manually. It builds a normalized manual Event and
// hands it to the runner, which checks out, parses, and executes the pipeline.
func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) {
	var req triggerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
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
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"run_id": res.RunID})
}

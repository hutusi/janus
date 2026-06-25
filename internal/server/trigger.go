package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hutusi/janus/internal/model"
)

type triggerRequest struct {
	RepoURL string `json:"repo_url"`
	Branch  string `json:"branch"`
	SHA     string `json:"sha"`
	Ref     string `json:"ref"`
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
		Provider: "manual",
		Kind:     model.EventManual,
		RepoURL:  req.RepoURL,
		Branch:   req.Branch,
		SHA:      req.SHA,
		Ref:      req.Ref,
	}
	if ev.Branch == "" {
		ev.Branch = strings.TrimPrefix(req.Ref, "refs/heads/")
	}

	res, err := s.runner.Trigger(r.Context(), ev)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"run_id": res.RunID})
}

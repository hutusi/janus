package server

import (
	"errors"
	"io"
	"net/http"

	"github.com/hutusi/janus/internal/provider"
	"github.com/hutusi/janus/internal/runner"
)

// maxWebhookBody caps how much of a webhook payload we read.
const maxWebhookBody = 5 << 20 // 5 MiB

// handleWebhook verifies, normalizes, and acts on a provider webhook.
//
// Status codes: 404 unknown provider, 401 bad signature, 400 unreadable/malformed
// payload, 200 for ignored or non-matching events, 200 (with an error note,
// logged) when the repo's pipeline is missing/invalid, and 202 when a run starts.
// Non-error outcomes stay 2xx so providers don't disable the hook.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	reg, ok := s.providers[name]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown or unconfigured provider: "+name)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}

	if err := reg.provider.Verify(r, body, reg.secret); err != nil {
		s.logger.Warn("webhook verification failed", "provider", name, "err", err)
		writeError(w, http.StatusUnauthorized, "signature verification failed")
		return
	}

	ev, err := reg.provider.Parse(r, body)
	if errors.Is(err, provider.ErrIgnoredEvent) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := s.runner.Trigger(r.Context(), *ev)
	if errors.Is(err, runner.ErrRepoNotAllowed) {
		// A rejected repo is a policy decision (or an attack / misconfig) — a
		// hard 403, distinct from the 2xx "no pipeline" case below.
		s.logger.Warn("webhook rejected: repo not allowed", "provider", name, "repo", ev.RepoURL, "branch", ev.Branch)
		writeJSON(w, http.StatusForbidden, map[string]string{"status": "rejected", "reason": "repository not in allowlist"})
		return
	}
	if err != nil {
		// The push/MR is valid; a missing or invalid .janus/ci.yml (or a
		// checkout problem) is the repository's concern. Stay 2xx, but log it.
		s.logger.Warn("webhook trigger did not run", "provider", name, "repo", ev.RepoURL, "branch", ev.Branch, "err", err)
		writeJSON(w, http.StatusOK, map[string]string{"status": "error", "error": err.Error()})
		return
	}
	if !res.Started {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": res.Reason})
		return
	}
	s.logger.Info("webhook started run", "provider", name, "run_id", res.RunID, "event", ev.Kind, "branch", ev.Branch)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "run_id": res.RunID})
}

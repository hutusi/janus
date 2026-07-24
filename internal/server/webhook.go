package server

import (
	"errors"
	"io"
	"net/http"

	"github.com/hutusi/janus/internal/model"
	"github.com/hutusi/janus/internal/provider"
	"github.com/hutusi/janus/internal/runner"
)

// maxWebhookBody caps a webhook payload. Oversized bodies are rejected with
// 413 — truncating instead would fail signature verification with a
// misleading 401.
const maxWebhookBody = 5 << 20 // 5 MiB

// handleWebhook verifies, normalizes, and acts on a provider webhook.
//
// Status codes: 404 unknown provider, 401 bad signature, 400 unreadable/
// malformed payload, 200 for ignored event types, and 202 when a run is
// recorded and accepted. The 202 is written before any git work — checkout,
// parse, and `on:` matching happen in the background so a provider's short
// delivery timeout never races the checkout; those outcomes (failed with a
// reason, skipped for a non-matching event) land on the run record instead of
// the response. Non-error outcomes stay 2xx so providers don't disable the
// hook.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	reg, ok := s.providers[name]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown or unconfigured provider: "+name)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
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

	// An optional ?pipeline_path= on the webhook URL selects a committed
	// pipeline file other than the configured default, with the same
	// relative-name rules as the manual API's field — register multiple hooks
	// on one project to route different deliveries to different pipelines.
	if p := r.URL.Query().Get("pipeline_path"); p != "" {
		ev.PipelinePath = p
	}

	// Logged before Trigger so every delivery leaves evidence of what will be
	// cloned, even if the background checkout later stalls.
	s.logger.Info("webhook trigger accepted", "provider", name, "event", ev.Kind, "repo", model.RedactURL(ev.RepoURL), "branch", ev.Branch)
	res, err := s.runner.Trigger(*ev)
	if errors.Is(err, runner.ErrRepoNotAllowed) {
		// A rejected repo is a policy decision (or an attack / misconfig) — a
		// hard 403, distinct from the 2xx "no pipeline" case below.
		s.logger.Warn("webhook rejected: repo not allowed", "provider", name, "repo", model.RedactURL(ev.RepoURL), "branch", ev.Branch)
		writeJSON(w, http.StatusForbidden, map[string]string{"status": "rejected", "reason": "repository not in allowlist"})
		return
	}
	if errors.Is(err, runner.ErrBusy) {
		// Overload is Janus's concern, not the repository's — deliberately
		// non-2xx so the provider retries the delivery instead of the event
		// being silently dropped.
		s.logger.Warn("webhook shed: runner at capacity", "provider", name, "repo", model.RedactURL(ev.RepoURL), "branch", ev.Branch)
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "busy"})
		return
	}
	if errors.Is(err, runner.ErrStoreUnavailable) {
		// The store couldn't record the run — Janus's problem, not the repo's.
		// Non-2xx so the provider retries instead of the event being dropped.
		s.logger.Error("webhook could not be recorded: store unavailable", "provider", name, "repo", model.RedactURL(ev.RepoURL), "branch", ev.Branch, "err", err)
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	if err != nil {
		// Synchronous validation failed (an over-long event field). The
		// push/MR itself is the repository's concern — stay 2xx, but log it.
		s.logger.Warn("webhook trigger did not run", "provider", name, "repo", model.RedactURL(ev.RepoURL), "branch", ev.Branch, "err", err)
		writeJSON(w, http.StatusOK, map[string]string{"status": "error", "error": err.Error()})
		return
	}
	s.logger.Info("webhook recorded run", "provider", name, "run_id", res.RunID, "event", ev.Kind, "branch", ev.Branch)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "run_id": res.RunID})
}

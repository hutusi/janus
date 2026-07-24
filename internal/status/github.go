package status

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/hutusi/janus/internal/model"
)

// maxGitHubDescLen is GitHub's practical cap on a commit-status description
// (longer text is truncated by the API); clip locally so it reads cleanly.
const maxGitHubDescLen = 140

// githubDialect posts to GitHub's Commit Statuses API
// (POST /repos/{owner}/{repo}/statuses/{sha}) with a Bearer token.
type githubDialect struct {
	token   string
	webBase *url.URL // GHES web base override; nil = github.com
	logger  *slog.Logger
}

// newGitHubDialect validates the token and optional GHES web base URL.
func newGitHubDialect(token, instanceRaw string, logger *slog.Logger) (githubDialect, error) {
	if strings.TrimSpace(token) == "" {
		return githubDialect{}, errors.New("github api token is empty")
	}
	d := githubDialect{token: token, logger: logger}
	if instanceRaw != "" {
		u, err := validateBaseURL(instanceRaw)
		if err != nil {
			return githubDialect{}, fmt.Errorf("github_url: %w", err)
		}
		d.webBase = u
	}
	return d, nil
}

func (d githubDialect) build(run *model.Run, state model.Status, targetURL string) (postJob, bool) {
	ghState := githubState(state)
	if ghState == "" {
		return postJob{}, false
	}
	ev := run.Event
	// GitHub addresses by "owner/repo": split into two path segments and require
	// each to be clean. RepoSlug comes from GitHub's repository.full_name (always
	// well-formed), but guard defensively — JoinPath cleans "."/".." and does not
	// re-escape an embedded "/", so a malformed slug could otherwise redirect the
	// token-bearing POST onto another path on the same host.
	owner, repo, ok := strings.Cut(ev.RepoSlug, "/")
	if !ok || !cleanSlugPart(owner) || !cleanSlugPart(repo) || ev.SHA == "" {
		return postJob{}, false
	}
	apiBase := d.apiBase(ev.RepoURL)
	if apiBase == nil {
		d.logger.Debug("commit status skipped: no resolvable GitHub API base", "run_id", run.ID, "repo", ev.RepoSlug)
		return postJob{}, false
	}
	endpoint := apiBase.JoinPath("repos", owner, repo, "statuses", ev.SHA).String()

	body, err := json.Marshal(statusBody{
		State:       ghState,
		Context:     statusContext,
		TargetURL:   targetURL, // run URLs are short; GitHub does not cap it like GitLab
		Description: clip(description(run, state), maxGitHubDescLen),
	})
	if err != nil {
		d.logger.Warn("commit status payload could not be encoded", "run_id", run.ID, "err", err)
		return postJob{}, false
	}
	label := "github " + ev.RepoSlug
	return postJob{
		key:      label + "|" + ev.SHA + "|" + statusContext,
		endpoint: endpoint,
		body:     body,
		header: map[string]string{
			"Authorization": "Bearer " + d.token,
			"Accept":        "application/vnd.github+json",
		},
		runID:    run.ID,
		label:    label,
		overHTTP: apiBase.Scheme == "http",
		retry409: false, // GitHub does not 409 on concurrent status updates
	}, true
}

// apiBase resolves the REST API base for a status post. With no GHES override,
// the host is derived from the clone URL: github.com maps to the api.github.com
// host (always https), while a GitHub Enterprise Server host gets a /api/v3
// prefix on its own scheme. Returns nil when the base can't be resolved (e.g. an
// ssh clone URL with no github_url), matching GitLab's ssh case.
func (d githubDialect) apiBase(repoURL string) *url.URL {
	web := d.webBase
	if web == nil {
		web = deriveBase(repoURL)
	}
	if web == nil {
		return nil
	}
	if strings.EqualFold(web.Host, "github.com") || strings.EqualFold(web.Host, "www.github.com") {
		return &url.URL{Scheme: "https", Host: "api.github.com"}
	}
	return web.JoinPath("api", "v3")
}

// githubState maps a Janus status to a GitHub commit-status state, or "" for
// states we deliberately don't post (pending, skipped). GitHub has no "running"
// (pending covers queued+running) and no "cancelled" — a cancelled run maps to
// "error" (it did not complete), distinct from a "failure" (a job that ran and
// failed).
func githubState(s model.Status) string {
	switch s {
	case model.StatusRunning:
		return "pending"
	case model.StatusSuccess:
		return "success"
	case model.StatusFailed:
		return "failure"
	case model.StatusCancelled:
		return "error"
	}
	return ""
}

// cleanSlugPart reports whether s is a single safe path segment for a
// statuses-API URL: non-empty, not a "."/".." traversal, and free of embedded
// path separators (strings.Cut splits only the first "/", so a nested slash lands
// here and is rejected).
func cleanSlugPart(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`)
}

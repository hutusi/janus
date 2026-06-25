package provider

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/hutusi/janus/internal/model"
)

// GitLab handles GitLab push and merge-request webhooks. GitLab authenticates
// with a plaintext secret in the X-Gitlab-Token header (not an HMAC), so Verify
// is a constant-time string compare.
type GitLab struct{}

func (GitLab) Name() string { return "gitlab" }

func (GitLab) Verify(r *http.Request, _ []byte, secret string) error {
	if secret == "" {
		return fmt.Errorf("gitlab: no webhook secret configured")
	}
	got := r.Header.Get("X-Gitlab-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
		return ErrInvalidSignature
	}
	return nil
}

func (GitLab) Parse(r *http.Request, body []byte) (*model.Event, error) {
	switch r.Header.Get("X-Gitlab-Event") {
	case "Push Hook":
		return parseGitLabPush(body)
	case "Merge Request Hook":
		return parseGitLabMR(body)
	default:
		return nil, ErrIgnoredEvent
	}
}

type glProject struct {
	GitHTTPURL string `json:"git_http_url"`
}

type glCommit struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func parseGitLabPush(body []byte) (*model.Event, error) {
	var p struct {
		Ref         string     `json:"ref"`
		After       string     `json:"after"`
		CheckoutSHA string     `json:"checkout_sha"`
		Project     glProject  `json:"project"`
		Commits     []glCommit `json:"commits"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("gitlab push: %w", err)
	}
	if isZeroSHA(p.After) {
		return nil, ErrIgnoredEvent // branch deletion
	}
	sha := p.After
	if sha == "" {
		sha = p.CheckoutSHA
	}
	ev := &model.Event{
		Provider: "gitlab",
		Kind:     model.EventPush,
		RepoURL:  p.Project.GitHTTPURL,
		Ref:      p.Ref,
		Branch:   strings.TrimPrefix(p.Ref, "refs/heads/"),
		SHA:      sha,
	}
	if n := len(p.Commits); n > 0 {
		ev.Title = p.Commits[n-1].Title
	}
	return ev, nil
}

func parseGitLabMR(body []byte) (*model.Event, error) {
	var m struct {
		Attrs struct {
			Action       string   `json:"action"`
			TargetBranch string   `json:"target_branch"`
			SourceBranch string   `json:"source_branch"`
			Title        string   `json:"title"`
			LastCommit   glCommit `json:"last_commit"`
		} `json:"object_attributes"`
		Project glProject `json:"project"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("gitlab merge_request: %w", err)
	}
	switch m.Attrs.Action {
	case "open", "reopen", "update":
		// actionable
	default:
		return nil, ErrIgnoredEvent // merge, close, approval, ...
	}
	// Match against the target branch (where the MR would land); check out the
	// MR's head commit. Branch is the target so ${{ branch }} and on:
	// merge_request.branches both refer to the destination.
	return &model.Event{
		Provider: "gitlab",
		Kind:     model.EventMergeRequest,
		RepoURL:  m.Project.GitHTTPURL,
		Ref:      "refs/heads/" + m.Attrs.SourceBranch,
		Branch:   m.Attrs.TargetBranch,
		SHA:      m.Attrs.LastCommit.ID,
		Title:    m.Attrs.Title,
	}, nil
}

// isZeroSHA reports whether s is the all-zero SHA git uses for deletions.
func isZeroSHA(s string) bool {
	return s != "" && strings.Trim(s, "0") == ""
}

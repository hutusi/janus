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
//
// SSH selects which clone URL the payload's project block contributes to the
// event: git_ssh_url instead of the default git_http_url. Whichever is chosen
// becomes Event.RepoURL and is the single string the allowlist gates and the
// workspace clones — so `allow_repos` entries must be written in the form the
// platform emits (see docs/configuration.md).
type GitLab struct {
	SSH bool
}

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

func (g GitLab) Parse(r *http.Request, body []byte) (*model.Event, error) {
	f := gitlabFormat{provider: "gitlab", ssh: g.SSH}
	switch r.Header.Get("X-Gitlab-Event") {
	case "Push Hook":
		return f.parsePush(body)
	case "Merge Request Hook":
		return f.parseMR(body)
	default:
		return nil, ErrIgnoredEvent
	}
}

// gitlabFormat parses GitLab-shaped push and merge_request webhook bodies. It is
// shared by the GitLab provider and by GitCode, whose webhooks use GitLab's exact
// payload format (differing only in header names and verification); provider is
// the name stamped on the resulting event, and ssh selects the clone transport.
type gitlabFormat struct {
	provider string
	ssh      bool
}

type glProject struct {
	ID         int64  `json:"id"`
	GitHTTPURL string `json:"git_http_url"`
	GitSSHURL  string `json:"git_ssh_url"`
}

// repoURL picks the configured clone URL out of the project block. A missing
// value is an error rather than a fall back to the other transport: some
// GitLab-compatible platforms omit git_ssh_url, and silently cloning over the
// transport the operator did not choose would surface downstream as a confusing
// "repository not in allowlist" instead of naming the real problem.
func (f gitlabFormat) repoURL(p glProject) (string, error) {
	if f.ssh {
		if p.GitSSHURL == "" {
			return "", fmt.Errorf("%s: clone_url is \"ssh\" but the payload has no project.git_ssh_url", f.provider)
		}
		return p.GitSSHURL, nil
	}
	if p.GitHTTPURL == "" {
		return "", fmt.Errorf("%s: the payload has no project.git_http_url", f.provider)
	}
	return p.GitHTTPURL, nil
}

type glCommit struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func (f gitlabFormat) parsePush(body []byte) (*model.Event, error) {
	var p struct {
		Ref         string     `json:"ref"`
		Before      string     `json:"before"`
		After       string     `json:"after"`
		CheckoutSHA string     `json:"checkout_sha"`
		Project     glProject  `json:"project"`
		Commits     []glCommit `json:"commits"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("%s push: %w", f.provider, err)
	}
	if isZeroSHA(p.After) {
		return nil, ErrIgnoredEvent // branch deletion
	}
	sha := p.After
	if sha == "" {
		sha = p.CheckoutSHA
	}
	repo, err := f.repoURL(p.Project)
	if err != nil {
		return nil, err
	}
	ev := &model.Event{
		Provider:  f.provider,
		Kind:      model.EventPush,
		RepoURL:   repo,
		Ref:       p.Ref,
		Branch:    strings.TrimPrefix(p.Ref, "refs/heads/"),
		SHA:       sha,
		ProjectID: p.Project.ID,
	}
	// An all-zeros before means a newly created branch: there is no base to
	// diff against, so leave Before empty and let path filters fail open.
	if !isZeroSHA(p.Before) {
		ev.Before = p.Before
	}
	if n := len(p.Commits); n > 0 {
		ev.Title = p.Commits[n-1].Title
	}
	return ev, nil
}

func (f gitlabFormat) parseMR(body []byte) (*model.Event, error) {
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
		return nil, fmt.Errorf("%s merge_request: %w", f.provider, err)
	}
	switch m.Attrs.Action {
	case "open", "reopen", "update":
		// actionable
	default:
		return nil, ErrIgnoredEvent // merge, close, approval, ...
	}
	repo, err := f.repoURL(m.Project)
	if err != nil {
		return nil, err
	}
	// Match against the target branch (where the MR would land); check out the
	// MR's head commit. Branch is the target so ${{ branch }} and on:
	// merge_request.branches both refer to the destination.
	return &model.Event{
		Provider:  f.provider,
		Kind:      model.EventMergeRequest,
		RepoURL:   repo,
		Ref:       "refs/heads/" + m.Attrs.SourceBranch,
		Branch:    m.Attrs.TargetBranch,
		SHA:       m.Attrs.LastCommit.ID,
		Title:     m.Attrs.Title,
		ProjectID: m.Project.ID,
	}, nil
}

// isZeroSHA reports whether s is the all-zero SHA git uses for deletions.
func isZeroSHA(s string) bool {
	return s != "" && strings.Trim(s, "0") == ""
}

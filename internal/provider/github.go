package provider

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/hutusi/janus/internal/model"
)

// GitHub handles GitHub push and pull-request webhooks. GitHub signs each
// delivery with an HMAC-SHA256 of the raw body under the webhook secret, sent as
// "sha256=<hex>" in the X-Hub-Signature-256 header, so Verify is a constant-time
// MAC comparison (unlike GitLab's plaintext token).
//
// SSH selects which clone URL the payload contributes to the event: ssh_url
// instead of the default clone_url (HTTPS). Whichever is chosen becomes
// Event.RepoURL — the single string the allowlist gates and the workspace
// clones — so `allow_repos` entries must be written in the form GitHub emits.
type GitHub struct {
	SSH bool
}

func (GitHub) Name() string { return "github" }

// Verify checks the X-Hub-Signature-256 HMAC against the configured secret. The
// legacy X-Hub-Signature (SHA-1) header is deliberately ignored.
func (GitHub) Verify(r *http.Request, body []byte, secret string) error {
	if secret == "" {
		return fmt.Errorf("github: no webhook secret configured")
	}
	got := r.Header.Get("X-Hub-Signature-256")
	const prefix = "sha256="
	if !strings.HasPrefix(got, prefix) {
		return ErrInvalidSignature
	}
	gotMAC, err := hex.DecodeString(strings.TrimPrefix(got, prefix))
	if err != nil {
		return ErrInvalidSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(gotMAC, mac.Sum(nil)) {
		return ErrInvalidSignature
	}
	return nil
}

func (g GitHub) Parse(r *http.Request, body []byte) (*model.Event, error) {
	switch r.Header.Get("X-GitHub-Event") {
	case "push":
		return g.parseGitHubPush(body)
	case "pull_request":
		return g.parseGitHubPR(body)
	default:
		return nil, ErrIgnoredEvent
	}
}

type ghRepo struct {
	FullName string `json:"full_name"`
	CloneURL string `json:"clone_url"`
	SSHURL   string `json:"ssh_url"`
}

// repoURL picks the configured clone URL out of a repository block. A missing
// value is an error rather than a fall back to the other transport, matching
// GitLab.repoURL: silently cloning over the transport the operator did not
// choose would surface downstream as a confusing allowlist rejection.
func (g GitHub) repoURL(repo ghRepo) (string, error) {
	if g.SSH {
		if repo.SSHURL == "" {
			return "", fmt.Errorf("github: clone_url is \"ssh\" but the payload has no repository.ssh_url")
		}
		return repo.SSHURL, nil
	}
	if repo.CloneURL == "" {
		return "", fmt.Errorf("github: the payload has no repository.clone_url")
	}
	return repo.CloneURL, nil
}

func (g GitHub) parseGitHubPush(body []byte) (*model.Event, error) {
	var p struct {
		Ref        string `json:"ref"`
		Before     string `json:"before"`
		After      string `json:"after"`
		Deleted    bool   `json:"deleted"`
		Repository ghRepo `json:"repository"`
		HeadCommit *struct {
			Message string `json:"message"`
		} `json:"head_commit"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("github push: %w", err)
	}
	if p.Deleted || isZeroSHA(p.After) {
		return nil, ErrIgnoredEvent // branch/tag deletion
	}
	if !strings.HasPrefix(p.Ref, "refs/heads/") {
		return nil, ErrIgnoredEvent // tag push or other non-branch ref
	}
	repo, err := g.repoURL(p.Repository)
	if err != nil {
		return nil, err
	}
	ev := &model.Event{
		Provider: "github",
		Kind:     model.EventPush,
		RepoURL:  repo,
		Ref:      p.Ref,
		Branch:   strings.TrimPrefix(p.Ref, "refs/heads/"),
		SHA:      p.After,
		RepoSlug: p.Repository.FullName,
	}
	// An all-zeros before means a newly created branch: no base to diff, so
	// leave Before empty and let path filters fail open.
	if !isZeroSHA(p.Before) {
		ev.Before = p.Before
	}
	if p.HeadCommit != nil {
		ev.Title = firstLine(p.HeadCommit.Message)
	}
	return ev, nil
}

func (g GitHub) parseGitHubPR(body []byte) (*model.Event, error) {
	var m struct {
		Action      string `json:"action"`
		PullRequest struct {
			Title string `json:"title"`
			Head  struct {
				SHA  string `json:"sha"`
				Ref  string `json:"ref"`
				Repo ghRepo `json:"repo"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("github pull_request: %w", err)
	}
	switch m.Action {
	case "opened", "reopened", "synchronize":
		// actionable: a PR was opened/reopened, or its head branch got new commits
	default:
		return nil, ErrIgnoredEvent // labeled, assigned, closed, ...
	}
	repo, err := g.repoURL(m.PullRequest.Head.Repo)
	if err != nil {
		return nil, err
	}
	// Match against the base branch (where the PR would land); check out the PR's
	// head commit from the head repo. Branch is the base so ${{ branch }} and
	// on: merge_request.branches both refer to the destination — mirroring the
	// GitLab MR mapping.
	return &model.Event{
		Provider: "github",
		Kind:     model.EventMergeRequest,
		RepoURL:  repo,
		Ref:      "refs/heads/" + m.PullRequest.Head.Ref,
		Branch:   m.PullRequest.Base.Ref,
		SHA:      m.PullRequest.Head.SHA,
		Title:    m.PullRequest.Title,
		RepoSlug: m.PullRequest.Head.Repo.FullName,
	}, nil
}

// firstLine returns the first line of a commit message, the GitHub equivalent of
// GitLab's commit title (GitHub payloads carry the full message, not a title).
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

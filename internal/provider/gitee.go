package provider

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hutusi/janus/internal/model"
)

// giteeTimestampWindow bounds how far X-Gitee-Timestamp may be from now. Gitee's
// signature covers only "<timestamp>\n<secret>", not the request body, so a
// leaked (timestamp, signature) pair would otherwise authenticate any forged
// payload; requiring a recent timestamp bounds that window. Gitee documents a
// ~1h clock-skew tolerance — a tighter window risks rejecting legitimate
// deliveries from a clock-skewed host.
const giteeTimestampWindow = time.Hour

// Gitee handles Gitee (gitee.com) push and pull-request webhooks. Gitee
// authenticates with the X-Gitee-Token header in one of two modes, chosen when
// the webhook is created: a plaintext password, or a signature — HMAC-SHA256 over
// "<X-Gitee-Timestamp>\n<secret>" (keyed by the secret), base64-encoded then
// URL-encoded. Verify accepts either, so it works regardless of how the hook was
// configured.
//
// SSH selects which clone URL the payload contributes to the event. Gitee sends
// both GitHub-style (clone_url/ssh_url) and GitLab-style (git_http_url/
// git_ssh_url) keys; Janus prefers the former and falls back to the latter.
type Gitee struct {
	SSH bool
}

func (Gitee) Name() string { return "gitee" }

func (Gitee) Verify(r *http.Request, _ []byte, secret string) error {
	if secret == "" {
		return fmt.Errorf("gitee: no webhook secret configured")
	}
	token := r.Header.Get("X-Gitee-Token")
	// Password mode: the plaintext secret echoed in the header. Like GitLab's
	// X-Gitlab-Token, this is a static secret and carries no freshness — its
	// confidentiality (TLS) is the protection.
	if subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1 {
		return nil
	}
	// Signature mode: HMAC-SHA256 over "<X-Gitee-Timestamp>\n<secret>". The MAC
	// does not cover the request body, so the timestamp must also be recent —
	// otherwise a single captured (timestamp, signature) pair would authenticate
	// arbitrary forged payloads (see giteeTimestampWindow).
	if ts := r.Header.Get("X-Gitee-Timestamp"); ts != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts + "\n" + secret))
		sig := url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
		if subtle.ConstantTimeCompare([]byte(token), []byte(sig)) == 1 && giteeTimestampFresh(ts) {
			return nil
		}
	}
	return ErrInvalidSignature
}

// giteeTimestampFresh reports whether ts (milliseconds since the Unix epoch) is
// within giteeTimestampWindow of now, in either direction.
func giteeTimestampFresh(ts string) bool {
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	nowMS := time.Now().UnixMilli()
	windowMS := giteeTimestampWindow.Milliseconds()
	// Range-check ms directly against [now-window, now+window]. nowMS is far from
	// the int64 extremes so the bounds cannot overflow; comparing ms rather than
	// differencing it avoids the Duration saturation/negation overflow an extreme
	// timestamp (e.g. math.MaxInt64) would otherwise slip through.
	return ms >= nowMS-windowMS && ms <= nowMS+windowMS
}

func (g Gitee) Parse(r *http.Request, body []byte) (*model.Event, error) {
	switch r.Header.Get("X-Gitee-Event") {
	case "Push Hook":
		return g.parseGiteePush(body)
	case "Merge Request Hook":
		return g.parseGiteeMR(body)
	default:
		return nil, ErrIgnoredEvent
	}
}

// giteeRepo carries both key spellings Gitee emits for a repository.
type giteeRepo struct {
	FullName   string `json:"full_name"`
	PathWithNS string `json:"path_with_namespace"`
	CloneURL   string `json:"clone_url"`
	GitHTTPURL string `json:"git_http_url"`
	SSHURL     string `json:"ssh_url"`
	GitSSHURL  string `json:"git_ssh_url"`
}

func (r giteeRepo) slug() string {
	if r.FullName != "" {
		return r.FullName
	}
	return r.PathWithNS
}

// repoURL picks the configured clone URL, trying the GitHub-style key then the
// GitLab-style one. A missing value is an error rather than a fall back to the
// other transport, matching the other providers.
func (g Gitee) repoURL(repo giteeRepo) (string, error) {
	if g.SSH {
		for _, u := range []string{repo.SSHURL, repo.GitSSHURL} {
			if u != "" {
				return u, nil
			}
		}
		return "", fmt.Errorf("gitee: clone_url is \"ssh\" but the payload has no ssh_url")
	}
	for _, u := range []string{repo.CloneURL, repo.GitHTTPURL} {
		if u != "" {
			return u, nil
		}
	}
	return "", fmt.Errorf("gitee: the payload has no clone_url")
}

func (g Gitee) parseGiteePush(body []byte) (*model.Event, error) {
	var p struct {
		Ref        string    `json:"ref"`
		Before     string    `json:"before"`
		After      string    `json:"after"`
		Deleted    bool      `json:"deleted"`
		Repository giteeRepo `json:"repository"`
		HeadCommit *struct {
			Message string `json:"message"`
		} `json:"head_commit"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("gitee push: %w", err)
	}
	if p.Deleted || isZeroSHA(p.After) {
		return nil, ErrIgnoredEvent // branch deletion
	}
	if !strings.HasPrefix(p.Ref, "refs/heads/") {
		return nil, ErrIgnoredEvent // tag push or other non-branch ref
	}
	repo, err := g.repoURL(p.Repository)
	if err != nil {
		return nil, err
	}
	ev := &model.Event{
		Provider: "gitee",
		Kind:     model.EventPush,
		RepoURL:  repo,
		Ref:      p.Ref,
		Branch:   strings.TrimPrefix(p.Ref, "refs/heads/"),
		SHA:      p.After,
		RepoSlug: p.Repository.slug(),
	}
	if !isZeroSHA(p.Before) {
		ev.Before = p.Before
	}
	if p.HeadCommit != nil {
		ev.Title = firstLine(p.HeadCommit.Message)
	}
	return ev, nil
}

func (g Gitee) parseGiteeMR(body []byte) (*model.Event, error) {
	var m struct {
		Action       string `json:"action"`
		Title        string `json:"title"`
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		PullRequest  struct {
			Head struct {
				SHA  string    `json:"sha"`
				Ref  string    `json:"ref"`
				Repo giteeRepo `json:"repo"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		} `json:"pull_request"`
		Repository giteeRepo `json:"repository"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("gitee merge_request: %w", err)
	}
	switch m.Action {
	case "open", "update":
		// actionable
	default:
		return nil, ErrIgnoredEvent // merge, close, approved, tested, ...
	}
	// Prefer the PR head repo (the branch being proposed); fall back to the
	// event's repository block for same-repo requests that omit it.
	head := m.PullRequest.Head.Repo
	repo, err := g.repoURL(head)
	if err != nil {
		if repo, err = g.repoURL(m.Repository); err != nil {
			return nil, err
		}
	}
	slug := head.slug()
	if slug == "" {
		slug = m.Repository.slug()
	}
	source := m.SourceBranch
	if source == "" {
		source = m.PullRequest.Head.Ref
	}
	target := m.TargetBranch
	if target == "" {
		target = m.PullRequest.Base.Ref
	}
	// Match against the target branch; check out the PR head commit — mirroring
	// the GitLab/GitHub mapping so ${{ branch }} is the destination.
	return &model.Event{
		Provider: "gitee",
		Kind:     model.EventMergeRequest,
		RepoURL:  repo,
		Ref:      "refs/heads/" + source,
		Branch:   target,
		SHA:      m.PullRequest.Head.SHA,
		Title:    m.Title,
		RepoSlug: slug,
	}, nil
}

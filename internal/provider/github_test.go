package provider

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/hutusi/janus/internal/model"
)

// ghSignature returns the "sha256=<hex>" value GitHub would send for body under
// secret.
func ghSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestGitHubVerify(t *testing.T) {
	const secret = "s3cret"
	body := payload(t, "github_push.json")

	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	r.Header.Set("X-Hub-Signature-256", ghSignature(secret, body))
	if err := (GitHub{}).Verify(r, body, secret); err != nil {
		t.Errorf("valid signature rejected: %v", err)
	}

	// Wrong secret produces a different MAC.
	if err := (GitHub{}).Verify(r, body, "wrong"); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("bad secret: got %v, want ErrInvalidSignature", err)
	}
	// A tampered body no longer matches the signature.
	if err := (GitHub{}).Verify(r, append(body, ' '), secret); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("tampered body: got %v, want ErrInvalidSignature", err)
	}
	// Missing sha256= prefix / malformed header.
	bad := httptest.NewRequest("POST", "/webhooks/github", nil)
	bad.Header.Set("X-Hub-Signature-256", "deadbeef")
	if err := (GitHub{}).Verify(bad, body, secret); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("no prefix: got %v, want ErrInvalidSignature", err)
	}
	// Empty configured secret is an error, never a pass.
	if err := (GitHub{}).Verify(r, body, ""); err == nil {
		t.Error("empty configured secret should error")
	}
}

func TestGitHubParsePush(t *testing.T) {
	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	r.Header.Set("X-GitHub-Event", "push")

	ev, err := GitHub{}.Parse(r, payload(t, "github_push.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ev.Kind != model.EventPush {
		t.Errorf("kind = %s, want push", ev.Kind)
	}
	if ev.Branch != "main" {
		t.Errorf("branch = %q, want main", ev.Branch)
	}
	if ev.SHA != "da1560886d4f094c3e6c9ef40349f7d38b5d27d7" {
		t.Errorf("sha = %q, want the `after` commit", ev.SHA)
	}
	if ev.RepoSlug != "acme/app" {
		t.Errorf("repo_slug = %q, want acme/app", ev.RepoSlug)
	}
	if ev.RepoURL != "https://github.com/acme/app.git" {
		t.Errorf("repo = %q", ev.RepoURL)
	}
	if ev.Before != "95790bf891e76fee5e1747ab589903a6a1f80f22" {
		t.Errorf("before = %q", ev.Before)
	}
	if ev.Title != "Fix the bug" {
		t.Errorf("title = %q, want the first line of head_commit.message", ev.Title)
	}
}

func TestGitHubParsePullRequest(t *testing.T) {
	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	r.Header.Set("X-GitHub-Event", "pull_request")

	ev, err := GitHub{}.Parse(r, payload(t, "github_pull_request.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ev.Kind != model.EventMergeRequest {
		t.Errorf("kind = %s, want merge_request", ev.Kind)
	}
	if ev.Branch != "main" {
		t.Errorf("branch = %q, want base ref main", ev.Branch)
	}
	if ev.SHA != "9b5f7c3a2e1d4b6a8c0f2e4d6b8a0c2e4f6a8b0c" {
		t.Errorf("sha = %q, want head.sha", ev.SHA)
	}
	if ev.Ref != "refs/heads/feature/login" {
		t.Errorf("ref = %q, want the head branch ref", ev.Ref)
	}
	if ev.RepoSlug != "acme/app" {
		t.Errorf("repo_slug = %q, want acme/app", ev.RepoSlug)
	}
}

// TestGitHubCloneURL covers the clone_url selection, like the GitLab test.
func TestGitHubCloneURL(t *testing.T) {
	const (
		httpURL = "https://github.com/acme/app.git"
		sshURL  = "git@github.com:acme/app.git"
	)
	tests := []struct {
		name    string
		gh      GitHub
		event   string
		payload string
		want    string
	}{
		{"push default is http", GitHub{}, "push", "github_push.json", httpURL},
		{"push ssh", GitHub{SSH: true}, "push", "github_push.json", sshURL},
		{"pr default is http", GitHub{}, "pull_request", "github_pull_request.json", httpURL},
		{"pr ssh", GitHub{SSH: true}, "pull_request", "github_pull_request.json", sshURL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/webhooks/github", nil)
			r.Header.Set("X-GitHub-Event", tc.event)
			ev, err := tc.gh.Parse(r, payload(t, tc.payload))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if ev.RepoURL != tc.want {
				t.Errorf("RepoURL = %q, want %q", ev.RepoURL, tc.want)
			}
		})
	}
}

func TestGitHubCloneURLMissing(t *testing.T) {
	body := `{"ref":"refs/heads/main","after":"da1560886d4f094c3e6c9ef40349f7d38b5d27d7",
	          "repository":{"full_name":"acme/app","clone_url":"https://github.com/acme/app.git"}}`
	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	r.Header.Set("X-GitHub-Event", "push")

	if ev, err := (GitHub{SSH: true}).Parse(r, []byte(body)); err == nil {
		t.Fatalf("missing ssh_url: got RepoURL %q, want an error", ev.RepoURL)
	} else if errors.Is(err, ErrIgnoredEvent) {
		t.Errorf("got ErrIgnoredEvent, want a descriptive parse error: %v", err)
	}
}

func TestGitHubParseIgnored(t *testing.T) {
	gh := GitHub{}

	// Unhandled event type.
	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	r.Header.Set("X-GitHub-Event", "issues")
	if _, err := gh.Parse(r, []byte(`{}`)); !errors.Is(err, ErrIgnoredEvent) {
		t.Errorf("issues: got %v, want ErrIgnoredEvent", err)
	}

	// Branch deletion (deleted flag / all-zero after).
	rp := httptest.NewRequest("POST", "/webhooks/github", nil)
	rp.Header.Set("X-GitHub-Event", "push")
	del := `{"ref":"refs/heads/x","deleted":true,"after":"0000000000000000000000000000000000000000","repository":{}}`
	if _, err := gh.Parse(rp, []byte(del)); !errors.Is(err, ErrIgnoredEvent) {
		t.Errorf("branch deletion: got %v, want ErrIgnoredEvent", err)
	}

	// Tag deletion.
	rtd := httptest.NewRequest("POST", "/webhooks/github", nil)
	rtd.Header.Set("X-GitHub-Event", "push")
	tagDel := `{"ref":"refs/tags/v1.0.0","deleted":true,"after":"0000000000000000000000000000000000000000","repository":{}}`
	if _, err := gh.Parse(rtd, []byte(tagDel)); !errors.Is(err, ErrIgnoredEvent) {
		t.Errorf("tag deletion: got %v, want ErrIgnoredEvent", err)
	}

	// A hosting-side ref namespace is neither a branch nor a tag.
	ro := httptest.NewRequest("POST", "/webhooks/github", nil)
	ro.Header.Set("X-GitHub-Event", "push")
	other := `{"ref":"refs/pull/7/head","after":"da1560886d4f094c3e6c9ef40349f7d38b5d27d7","repository":{"clone_url":"https://github.com/acme/app.git"}}`
	if _, err := gh.Parse(ro, []byte(other)); !errors.Is(err, ErrIgnoredEvent) {
		t.Errorf("refs/pull: got %v, want ErrIgnoredEvent", err)
	}

	// A PR action Janus does not act on.
	rm := httptest.NewRequest("POST", "/webhooks/github", nil)
	rm.Header.Set("X-GitHub-Event", "pull_request")
	labeled := `{"action":"labeled","pull_request":{"head":{"ref":"x","sha":"da15","repo":{"clone_url":"https://github.com/acme/app.git"}},"base":{"ref":"main"}}}`
	if _, err := gh.Parse(rm, []byte(labeled)); !errors.Is(err, ErrIgnoredEvent) {
		t.Errorf("PR labeled: got %v, want ErrIgnoredEvent", err)
	}
}

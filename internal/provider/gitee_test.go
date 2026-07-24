package provider

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/hutusi/janus/internal/model"
)

// giteeSignature computes the X-Gitee-Token value for signature mode.
func giteeSignature(secret, ts string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	return url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
}

func TestGiteeVerify(t *testing.T) {
	const secret = "s3cret"

	// Password mode: the plaintext secret in the header.
	pw := httptest.NewRequest("POST", "/webhooks/gitee", nil)
	pw.Header.Set("X-Gitee-Token", secret)
	if err := (Gitee{}).Verify(pw, nil, secret); err != nil {
		t.Errorf("password mode rejected: %v", err)
	}
	if err := (Gitee{}).Verify(pw, nil, "wrong"); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("bad password: got %v, want ErrInvalidSignature", err)
	}

	// Signature mode: HMAC over "<timestamp>\n<secret>".
	const ts = "1609459200000"
	sig := httptest.NewRequest("POST", "/webhooks/gitee", nil)
	sig.Header.Set("X-Gitee-Timestamp", ts)
	sig.Header.Set("X-Gitee-Token", giteeSignature(secret, ts))
	if err := (Gitee{}).Verify(sig, nil, secret); err != nil {
		t.Errorf("signature mode rejected: %v", err)
	}
	if err := (Gitee{}).Verify(sig, nil, "wrong"); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("bad signature: got %v, want ErrInvalidSignature", err)
	}

	// Empty configured secret is always an error.
	if err := (Gitee{}).Verify(pw, nil, ""); err == nil {
		t.Error("empty configured secret should error")
	}
}

func TestGiteeParsePush(t *testing.T) {
	r := httptest.NewRequest("POST", "/webhooks/gitee", nil)
	r.Header.Set("X-Gitee-Event", "Push Hook")

	ev, err := Gitee{}.Parse(r, payload(t, "gitee_push.json"))
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
	if ev.RepoURL != "https://gitee.com/acme/app.git" {
		t.Errorf("repo = %q", ev.RepoURL)
	}
	if ev.Before != "95790bf891e76fee5e1747ab589903a6a1f80f22" {
		t.Errorf("before = %q", ev.Before)
	}
	if ev.Title != "Fix the bug" {
		t.Errorf("title = %q, want the first line of head_commit.message", ev.Title)
	}
}

func TestGiteeParseMergeRequest(t *testing.T) {
	r := httptest.NewRequest("POST", "/webhooks/gitee", nil)
	r.Header.Set("X-Gitee-Event", "Merge Request Hook")

	ev, err := Gitee{}.Parse(r, payload(t, "gitee_merge_request.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ev.Kind != model.EventMergeRequest {
		t.Errorf("kind = %s, want merge_request", ev.Kind)
	}
	if ev.Branch != "main" {
		t.Errorf("branch = %q, want target_branch main", ev.Branch)
	}
	if ev.SHA != "9b5f7c3a2e1d4b6a8c0f2e4d6b8a0c2e4f6a8b0c" {
		t.Errorf("sha = %q, want pull_request.head.sha", ev.SHA)
	}
	if ev.Ref != "refs/heads/feature/login" {
		t.Errorf("ref = %q, want the source branch ref", ev.Ref)
	}
	if ev.RepoSlug != "acme/app" {
		t.Errorf("repo_slug = %q", ev.RepoSlug)
	}
}

// TestGiteeCloneURL covers clone_url selection across the GitHub- and GitLab-style
// keys Gitee sends.
func TestGiteeCloneURL(t *testing.T) {
	const (
		httpURL = "https://gitee.com/acme/app.git"
		sshURL  = "git@gitee.com:acme/app.git"
	)
	tests := []struct {
		name    string
		g       Gitee
		event   string
		payload string
		want    string
	}{
		{"push default is http", Gitee{}, "Push Hook", "gitee_push.json", httpURL},
		{"push ssh", Gitee{SSH: true}, "Push Hook", "gitee_push.json", sshURL},
		{"mr default is http", Gitee{}, "Merge Request Hook", "gitee_merge_request.json", httpURL},
		{"mr ssh", Gitee{SSH: true}, "Merge Request Hook", "gitee_merge_request.json", sshURL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/webhooks/gitee", nil)
			r.Header.Set("X-Gitee-Event", tc.event)
			ev, err := tc.g.Parse(r, payload(t, tc.payload))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if ev.RepoURL != tc.want {
				t.Errorf("RepoURL = %q, want %q", ev.RepoURL, tc.want)
			}
		})
	}
}

func TestGiteeParseIgnored(t *testing.T) {
	g := Gitee{}

	// Unhandled event type.
	r := httptest.NewRequest("POST", "/webhooks/gitee", nil)
	r.Header.Set("X-Gitee-Event", "Note Hook")
	if _, err := g.Parse(r, []byte(`{}`)); !errors.Is(err, ErrIgnoredEvent) {
		t.Errorf("Note Hook: got %v, want ErrIgnoredEvent", err)
	}

	// Branch deletion.
	rp := httptest.NewRequest("POST", "/webhooks/gitee", nil)
	rp.Header.Set("X-Gitee-Event", "Push Hook")
	del := `{"ref":"refs/heads/x","deleted":true,"after":"0000000000000000000000000000000000000000","repository":{}}`
	if _, err := g.Parse(rp, []byte(del)); !errors.Is(err, ErrIgnoredEvent) {
		t.Errorf("branch deletion: got %v, want ErrIgnoredEvent", err)
	}

	// MR merge (not open/update).
	rm := httptest.NewRequest("POST", "/webhooks/gitee", nil)
	rm.Header.Set("X-Gitee-Event", "Merge Request Hook")
	merged := `{"action":"merge","pull_request":{"head":{"sha":"da15","repo":{"clone_url":"https://gitee.com/acme/app.git"}}}}`
	if _, err := g.Parse(rm, []byte(merged)); !errors.Is(err, ErrIgnoredEvent) {
		t.Errorf("MR merge: got %v, want ErrIgnoredEvent", err)
	}
}

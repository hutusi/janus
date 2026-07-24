package provider

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/hutusi/janus/internal/model"
)

func TestGitCodeVerify(t *testing.T) {
	const secret = "s3cret"
	body := payload(t, "gitcode_push.json")

	// Token mode.
	tok := httptest.NewRequest("POST", "/webhooks/gitcode", nil)
	tok.Header.Set("X-GitCode-Token", secret)
	if err := (GitCode{}).Verify(tok, body, secret); err != nil {
		t.Errorf("token mode rejected: %v", err)
	}
	if err := (GitCode{}).Verify(tok, body, "wrong"); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("bad token: got %v, want ErrInvalidSignature", err)
	}

	// Signature mode: HMAC-SHA256 over the raw body (reuses ghSignature — same
	// "sha256=<hex>" construction as GitHub).
	sig := httptest.NewRequest("POST", "/webhooks/gitcode", nil)
	sig.Header.Set("X-GitCode-Signature-256", ghSignature(secret, body))
	if err := (GitCode{}).Verify(sig, body, secret); err != nil {
		t.Errorf("signature mode rejected: %v", err)
	}
	if err := (GitCode{}).Verify(sig, append(body, ' '), secret); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("tampered body: got %v, want ErrInvalidSignature", err)
	}

	// Empty configured secret is always an error.
	if err := (GitCode{}).Verify(tok, body, ""); err == nil {
		t.Error("empty configured secret should error")
	}
}

func TestGitCodeParsePush(t *testing.T) {
	r := httptest.NewRequest("POST", "/webhooks/gitcode", nil)
	r.Header.Set("X-GitCode-Event", "Push Hook")

	ev, err := GitCode{}.Parse(r, payload(t, "gitcode_push.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ev.Provider != "gitcode" {
		t.Errorf("provider = %q, want gitcode", ev.Provider)
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
	if ev.RepoURL != "https://gitcode.com/acme/app.git" {
		t.Errorf("repo = %q", ev.RepoURL)
	}
	if ev.Title != "Fix the bug" {
		t.Errorf("title = %q, want last commit title", ev.Title)
	}
}

func TestGitCodeParseMergeRequest(t *testing.T) {
	r := httptest.NewRequest("POST", "/webhooks/gitcode", nil)
	r.Header.Set("X-GitCode-Event", "Merge Request Hook")

	ev, err := GitCode{}.Parse(r, payload(t, "gitcode_merge_request.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ev.Provider != "gitcode" {
		t.Errorf("provider = %q, want gitcode", ev.Provider)
	}
	if ev.Kind != model.EventMergeRequest {
		t.Errorf("kind = %s, want merge_request", ev.Kind)
	}
	if ev.Branch != "main" {
		t.Errorf("branch = %q, want target_branch main", ev.Branch)
	}
	if ev.SHA != "9b5f7c3a2e1d4b6a8c0f2e4d6b8a0c2e4f6a8b0c" {
		t.Errorf("sha = %q, want last_commit.id", ev.SHA)
	}
	if ev.Ref != "refs/heads/feature/login" {
		t.Errorf("ref = %q, want source branch ref", ev.Ref)
	}
}

// TestGitCodeCloneURL confirms the shared parser's ssh selection works for GitCode.
func TestGitCodeCloneURL(t *testing.T) {
	r := httptest.NewRequest("POST", "/webhooks/gitcode", nil)
	r.Header.Set("X-GitCode-Event", "Push Hook")
	ev, err := GitCode{SSH: true}.Parse(r, payload(t, "gitcode_push.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ev.RepoURL != "git@gitcode.com:acme/app.git" {
		t.Errorf("RepoURL = %q, want the ssh URL", ev.RepoURL)
	}
}

func TestGitCodeParseIgnored(t *testing.T) {
	g := GitCode{}

	r := httptest.NewRequest("POST", "/webhooks/gitcode", nil)
	r.Header.Set("X-GitCode-Event", "Tag Push Hook")
	if _, err := g.Parse(r, []byte(`{}`)); !errors.Is(err, ErrIgnoredEvent) {
		t.Errorf("Tag Push Hook: got %v, want ErrIgnoredEvent", err)
	}

	rp := httptest.NewRequest("POST", "/webhooks/gitcode", nil)
	rp.Header.Set("X-GitCode-Event", "Push Hook")
	del := `{"ref":"refs/heads/x","after":"0000000000000000000000000000000000000000","project":{}}`
	if _, err := g.Parse(rp, []byte(del)); !errors.Is(err, ErrIgnoredEvent) {
		t.Errorf("branch deletion: got %v, want ErrIgnoredEvent", err)
	}
}

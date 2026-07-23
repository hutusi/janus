package provider

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hutusi/janus/internal/model"
)

func payload(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "payloads", name))
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	return data
}

func TestGitLabVerify(t *testing.T) {
	gl := GitLab{}
	r := httptest.NewRequest("POST", "/webhooks/gitlab", nil)
	r.Header.Set("X-Gitlab-Token", "s3cret")

	if err := gl.Verify(r, nil, "s3cret"); err != nil {
		t.Errorf("valid token rejected: %v", err)
	}
	if err := gl.Verify(r, nil, "wrong"); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("bad token: got %v, want ErrInvalidSignature", err)
	}
	if err := gl.Verify(r, nil, ""); err == nil {
		t.Error("empty configured secret should error")
	}
}

func TestGitLabParsePush(t *testing.T) {
	gl := GitLab{}
	r := httptest.NewRequest("POST", "/webhooks/gitlab", nil)
	r.Header.Set("X-Gitlab-Event", "Push Hook")

	ev, err := gl.Parse(r, payload(t, "gitlab_push.json"))
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
	if ev.RepoURL != "https://gitlab.example.com/acme/app.git" {
		t.Errorf("repo = %q", ev.RepoURL)
	}
	if ev.Title != "Fix the bug" {
		t.Errorf("title = %q, want last commit title", ev.Title)
	}
}

func TestGitLabParseMergeRequest(t *testing.T) {
	gl := GitLab{}
	r := httptest.NewRequest("POST", "/webhooks/gitlab", nil)
	r.Header.Set("X-Gitlab-Event", "Merge Request Hook")

	ev, err := gl.Parse(r, payload(t, "gitlab_merge_request.json"))
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
		t.Errorf("sha = %q, want last_commit.id", ev.SHA)
	}
	if ev.Ref != "refs/heads/feature/login" {
		t.Errorf("ref = %q, want source branch ref", ev.Ref)
	}
}

// TestGitLabCloneURL covers the clone_url selection: whichever URL is chosen
// becomes Event.RepoURL, the single string the allowlist gates and the
// workspace clones.
func TestGitLabCloneURL(t *testing.T) {
	const (
		httpURL = "https://gitlab.example.com/acme/app.git"
		sshURL  = "git@gitlab.example.com:acme/app.git"
	)
	tests := []struct {
		name    string
		gl      GitLab
		event   string
		payload string
		want    string
	}{
		{"push default is http", GitLab{}, "Push Hook", "gitlab_push.json", httpURL},
		{"push ssh", GitLab{SSH: true}, "Push Hook", "gitlab_push.json", sshURL},
		{"mr default is http", GitLab{}, "Merge Request Hook", "gitlab_merge_request.json", httpURL},
		{"mr ssh", GitLab{SSH: true}, "Merge Request Hook", "gitlab_merge_request.json", sshURL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/webhooks/gitlab", nil)
			r.Header.Set("X-Gitlab-Event", tc.event)
			ev, err := tc.gl.Parse(r, payload(t, tc.payload))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if ev.RepoURL != tc.want {
				t.Errorf("RepoURL = %q, want %q", ev.RepoURL, tc.want)
			}
		})
	}
}

// TestGitLabCloneURLMissing pins the deliberate refusal to fall back to the
// other transport: a platform that omits git_ssh_url must produce an error
// naming the missing field, not a silent HTTPS clone.
func TestGitLabCloneURLMissing(t *testing.T) {
	body := `{"ref":"refs/heads/main","after":"da1560886d4f094c3e6c9ef40349f7d38b5d27d7",
	          "project":{"git_http_url":"https://gitlab.example.com/acme/app.git"}}`
	r := httptest.NewRequest("POST", "/webhooks/gitlab", nil)
	r.Header.Set("X-Gitlab-Event", "Push Hook")

	ev, err := GitLab{SSH: true}.Parse(r, []byte(body))
	if err == nil {
		t.Fatalf("missing git_ssh_url: got RepoURL %q, want an error", ev.RepoURL)
	}
	if errors.Is(err, ErrIgnoredEvent) {
		t.Errorf("got ErrIgnoredEvent, want a descriptive parse error: %v", err)
	}
}

func TestGitLabParseIgnored(t *testing.T) {
	gl := GitLab{}

	// Unhandled event type.
	r := httptest.NewRequest("POST", "/webhooks/gitlab", nil)
	r.Header.Set("X-Gitlab-Event", "Note Hook")
	if _, err := gl.Parse(r, []byte(`{}`)); !errors.Is(err, ErrIgnoredEvent) {
		t.Errorf("Note Hook: got %v, want ErrIgnoredEvent", err)
	}

	// Branch deletion (all-zero after).
	rp := httptest.NewRequest("POST", "/webhooks/gitlab", nil)
	rp.Header.Set("X-Gitlab-Event", "Push Hook")
	del := `{"ref":"refs/heads/x","after":"0000000000000000000000000000000000000000","project":{}}`
	if _, err := gl.Parse(rp, []byte(del)); !errors.Is(err, ErrIgnoredEvent) {
		t.Errorf("branch deletion: got %v, want ErrIgnoredEvent", err)
	}

	// MR being closed.
	rm := httptest.NewRequest("POST", "/webhooks/gitlab", nil)
	rm.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	closed := `{"object_attributes":{"action":"close","target_branch":"main"}}`
	if _, err := gl.Parse(rm, []byte(closed)); !errors.Is(err, ErrIgnoredEvent) {
		t.Errorf("MR close: got %v, want ErrIgnoredEvent", err)
	}
}

func TestGitLabPushBefore(t *testing.T) {
	gl := GitLab{}
	mk := func(before string) []byte {
		return []byte(`{"ref":"refs/heads/main","before":"` + before + `",
			"after":"da1560886d4f094c3e6c9ef40349f7d38b5d27d7",
			"project":{"git_http_url":"https://gitlab.example.com/acme/app.git"}}`)
	}
	r := httptest.NewRequest("POST", "/webhooks/gitlab", nil)
	r.Header.Set("X-Gitlab-Event", "Push Hook")

	ev, err := gl.Parse(r, mk("95790bf891e76fee5e1747ab589903a6a1f80f22"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Before != "95790bf891e76fee5e1747ab589903a6a1f80f22" {
		t.Errorf("Before = %q, want the payload's before SHA", ev.Before)
	}

	// A new branch's all-zeros before has no diff base: normalized to empty
	// so path filters fail open.
	ev, err = gl.Parse(r, mk("0000000000000000000000000000000000000000"))
	if err != nil {
		t.Fatalf("parse zero-before: %v", err)
	}
	if ev.Before != "" {
		t.Errorf("zero before normalized to %q, want empty", ev.Before)
	}
}

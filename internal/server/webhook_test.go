package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hutusi/janus/internal/model"
)

// gitlabPush posts a GitLab "Push Hook" for repo@sha on the given branch.
func gitlabPush(t *testing.T, ts *httptest.Server, repo, sha, branch, token string) *http.Response {
	t.Helper()
	payload := fmt.Sprintf(`{
		"object_kind": "push",
		"ref": "refs/heads/%s",
		"after": "%s",
		"project": { "git_http_url": %q }
	}`, branch, sha, repo)
	req, _ := http.NewRequest("POST", ts.URL+"/webhooks/gitlab", strings.NewReader(payload))
	req.Header.Set("X-Gitlab-Event", "Push Hook")
	req.Header.Set("X-Gitlab-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestWebhookGitLabPushRuns(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, `name: ci
on: { push: { branches: [main] } }
jobs:
  build:
    steps:
      - run: echo "ran for ${{ short_sha }}"
`)
	ts := newTestServer(t)

	resp := gitlabPush(t, ts, repo, sha, "main", testGitLabSecret)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
		RunID  string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Status != "started" || body.RunID == "" {
		t.Fatalf("body = %+v, want started with a run_id", body)
	}

	run := pollRun(t, ts, body.RunID, 15*time.Second)
	if run.Status != model.StatusSuccess {
		t.Errorf("run status = %s, want success", run.Status)
	}
	if run.Event.Kind != model.EventPush || run.Event.Branch != "main" {
		t.Errorf("event = %+v, want push on main", run.Event)
	}
}

func TestWebhookGitLabBadToken(t *testing.T) {
	ts := newTestServer(t)
	resp := gitlabPush(t, ts, "https://example.com/x.git", "deadbeef", "main", "wrong-token")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestWebhookGitLabNonMatchingBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, `name: ci
on: { push: { branches: [main] } }
jobs:
  build:
    steps:
      - run: echo hi
`)
	ts := newTestServer(t)

	// Push to a branch the workflow does not listen on: accepted but ignored.
	resp := gitlabPush(t, ts, repo, sha, "dev", testGitLabSecret)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (ignored)", resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Status != "ignored" {
		t.Errorf("status = %q, want ignored", body.Status)
	}
}

func TestWebhookGitLabPushBranchesIgnore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, `name: ci
on: { push: { branches-ignore: [master] } }
jobs:
  build:
    steps:
      - run: echo "building ${{ branch }}"
`)
	ts := newTestServer(t)

	// A push to any non-ignored branch starts a run. (Checkout is by SHA, so
	// the branch name in the hook need not exist in the test repo.)
	resp := gitlabPush(t, ts, repo, sha, "feature", testGitLabSecret)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("feature push status = %d, want 202", resp.StatusCode)
	}
	var started struct {
		Status string `json:"status"`
		RunID  string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&started)
	if started.Status != "started" || started.RunID == "" {
		t.Fatalf("feature push body = %+v, want started with a run_id", started)
	}
	run := pollRun(t, ts, started.RunID, 15*time.Second)
	if run.Status != model.StatusSuccess {
		t.Errorf("run status = %s, want success", run.Status)
	}

	// A push to the ignored branch is accepted but starts nothing.
	resp2 := gitlabPush(t, ts, repo, sha, "master", testGitLabSecret)
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("master push status = %d, want 200 (ignored)", resp2.StatusCode)
	}
	var ignored struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&ignored)
	if ignored.Status != "ignored" {
		t.Errorf("master push status = %q, want ignored", ignored.Status)
	}
}

func TestWebhookUnknownProvider(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Post(ts.URL+"/webhooks/github", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unconfigured provider", resp.StatusCode)
	}
}

func TestWebhookDisallowedRepoForbidden(t *testing.T) {
	// Allowlist permits only a different host; the (local fixture) repo is denied.
	ts := newTestServerAllow(t, "https://allowed.example.com")
	resp := gitlabPush(t, ts, "https://gitlab.example.com/acme/app.git", "deadbeef", "main", testGitLabSecret)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Status != "rejected" {
		t.Errorf("status = %q, want rejected", body.Status)
	}
}

func TestTriggerDisallowedRepoForbidden(t *testing.T) {
	ts := newTestServerAllow(t, "https://allowed.example.com")
	resp := postTrigger(t, ts, `{"repo_url":"https://gitlab.example.com/acme/app.git","ref":"refs/heads/main"}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

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
	"github.com/hutusi/janus/internal/store"
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
	if body.Status != "accepted" || body.RunID == "" {
		t.Fatalf("body = %+v, want accepted with a run_id", body)
	}

	run := pollRun(t, ts, body.RunID, 15*time.Second)
	if run.Status != model.StatusSuccess {
		t.Errorf("run status = %s, want success", run.Status)
	}
	if run.Event.Kind != model.EventPush || run.Event.Branch != "main" {
		t.Errorf("event = %+v, want push on main", run.Event)
	}
}

func TestWebhookStoreUnavailableRetries(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, `name: ci
on: { push: {} }
jobs:
  build:
    steps:
      - run: echo hi
`)
	ts := newTestServerStore(t, saveFailStore{store.NewMemory()})

	resp := gitlabPush(t, ts, repo, sha, "main", testGitLabSecret)
	defer func() { _ = resp.Body.Close() }()
	// A storage failure must NOT be acked as 200 (GitLab would drop the event);
	// it is a retriable 503.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (store unavailable)", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("503 should carry Retry-After so the provider retries")
	}
}

func TestWebhookBodyTooLarge(t *testing.T) {
	ts := newTestServer(t)
	req, _ := http.NewRequest("POST", ts.URL+"/webhooks/gitlab", strings.NewReader(strings.Repeat("x", maxWebhookBody+1)))
	req.Header.Set("X-Gitlab-Event", "Push Hook")
	req.Header.Set("X-Gitlab-Token", testGitLabSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (not a truncated-body 401)", resp.StatusCode)
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

	// Push to a branch the workflow does not listen on: the delivery is
	// accepted (matching happens after the background checkout), and the run
	// is recorded as skipped with the non-match reason.
	resp := gitlabPush(t, ts, repo, sha, "dev", testGitLabSecret)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (accepted)", resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
		RunID  string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Status != "accepted" || body.RunID == "" {
		t.Fatalf("body = %+v, want accepted with a run_id", body)
	}
	run := pollRun(t, ts, body.RunID, 15*time.Second)
	if run.Status != model.StatusSkipped {
		t.Errorf("run status = %s, want skipped", run.Status)
	}
	if run.Reason == "" {
		t.Error("a skipped run should record why the event did not match")
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
	if started.Status != "accepted" || started.RunID == "" {
		t.Fatalf("feature push body = %+v, want accepted with a run_id", started)
	}
	run := pollRun(t, ts, started.RunID, 15*time.Second)
	if run.Status != model.StatusSuccess {
		t.Errorf("run status = %s, want success", run.Status)
	}

	// A push to the ignored branch is accepted but executes nothing: the run
	// is recorded as skipped after the background match.
	resp2 := gitlabPush(t, ts, repo, sha, "master", testGitLabSecret)
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("master push status = %d, want 202 (accepted)", resp2.StatusCode)
	}
	var ignored struct {
		Status string `json:"status"`
		RunID  string `json:"run_id"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&ignored)
	if ignored.Status != "accepted" || ignored.RunID == "" {
		t.Fatalf("master push body = %+v, want accepted with a run_id", ignored)
	}
	if run := pollRun(t, ts, ignored.RunID, 15*time.Second); run.Status != model.StatusSkipped {
		t.Errorf("master push run status = %s, want skipped", run.Status)
	}
}

func TestWebhookCheckoutFailureRecordsFailedRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ts := newTestServer(t)

	// An unclonable repo is accepted like any delivery — the checkout failure
	// lands on the run record, not the response (which is long gone by then).
	resp := gitlabPush(t, ts, "/nonexistent/repo", "0123456789abcdef0123456789abcdef01234567", "main", testGitLabSecret)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (accepted)", resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
		RunID  string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Status != "accepted" || body.RunID == "" {
		t.Fatalf("body = %+v, want accepted with a run_id", body)
	}
	run := pollRun(t, ts, body.RunID, 15*time.Second)
	if run.Status != model.StatusFailed {
		t.Errorf("run status = %s, want failed", run.Status)
	}
	if !strings.Contains(run.Reason, "checkout") {
		t.Errorf("run reason = %q, want it to name the checkout", run.Reason)
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

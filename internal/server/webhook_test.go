package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
	return gitlabPushTo(t, ts, "/webhooks/gitlab", repo, sha, branch, token)
}

// gitlabPushTo is gitlabPush against an explicit URL path (with any query
// string, e.g. "/webhooks/gitlab?pipeline_path=release.yml").
func gitlabPushTo(t *testing.T, ts *httptest.Server, path, repo, sha, branch, token string) *http.Response {
	t.Helper()
	payload := fmt.Sprintf(`{
		"object_kind": "push",
		"ref": "refs/heads/%s",
		"after": "%s",
		"project": { "git_http_url": %q }
	}`, branch, sha, repo)
	req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(payload))
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

func TestWebhookPipelinePathOverride(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, _ := initGitRepo(t, `name: ci
on: { push: { branches: [main] } }
jobs:
  build:
    steps:
      - run: echo default
`)
	sha := commitFileIn(t, repo, ".janus/release.yml", `name: release
on: { push: { branches: [main] } }
jobs:
  publish:
    steps:
      - run: echo releasing
`)
	ts := newTestServer(t)

	// ?pipeline_path= on the webhook URL routes this delivery to the named
	// file instead of the configured default.
	resp := gitlabPushTo(t, ts, "/webhooks/gitlab?pipeline_path=release.yml", repo, sha, "main", testGitLabSecret)
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
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %s (%q), want success", run.Status, run.Reason)
	}
	if run.WorkflowName != "release" {
		t.Errorf("workflow = %q, want the overridden release pipeline", run.WorkflowName)
	}
	if run.Event.PipelinePath != "release.yml" {
		t.Errorf("event pipeline_path = %q, want release.yml recorded on the run", run.Event.PipelinePath)
	}
}

func TestWebhookPipelinePathEscapeRejected(t *testing.T) {
	ts := newTestServer(t)

	resp := gitlabPushTo(t, ts, "/webhooks/gitlab?pipeline_path=../escape.yml",
		"/some/repo", "0123456789abcdef0123456789abcdef01234567", "main", testGitLabSecret)
	defer func() { _ = resp.Body.Close() }()
	// Synchronous validation: rejected before any run is recorded, but 2xx so
	// a URL typo cannot make the platform auto-disable the hook.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (error body)", resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Status != "error" || !strings.Contains(body.Error, "pipeline path") {
		t.Fatalf("body = %+v, want an error naming the pipeline path", body)
	}
}

func TestWebhookMergedPipelineRoutesByBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// One pipeline for every branch: build always runs, deploy only on
	// master/main via its job-level branch filter.
	repo, sha := initGitRepo(t, `name: ci
on: { push: {} }
jobs:
  build:
    steps:
      - run: echo building
  deploy:
    needs: [build]
    branches: [master, main]
    steps:
      - run: echo releasing
`)
	ts := newTestServer(t)

	deliver := func(branch string) *model.Run {
		t.Helper()
		resp := gitlabPush(t, ts, repo, sha, branch, testGitLabSecret)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("%s push status = %d, want 202", branch, resp.StatusCode)
		}
		var body struct {
			RunID string `json:"run_id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return pollRun(t, ts, body.RunID, 15*time.Second)
	}

	run := deliver("feature/x")
	if run.Status != model.StatusSuccess {
		t.Fatalf("feature run = %s (%q), want success", run.Status, run.Reason)
	}
	if got := findJob(run, "build").Status; got != model.StatusSuccess {
		t.Errorf("feature build = %s, want success", got)
	}
	if got := findJob(run, "deploy").Status; got != model.StatusSkipped {
		t.Errorf("feature deploy = %s, want skipped", got)
	}

	run = deliver("main")
	if run.Status != model.StatusSuccess {
		t.Fatalf("main run = %s (%q), want success", run.Status, run.Reason)
	}
	if got := findJob(run, "deploy").Status; got != model.StatusSuccess {
		t.Errorf("main deploy = %s, want success", got)
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
	// A credential-bearing URL: rejected, and the secret must not be echoed
	// back in the response body (Trigger's error wraps the raw URL).
	resp := postTrigger(t, ts, `{"repo_url":"https://ci:sekret@gitlab.example.com/acme/app.git","ref":"refs/heads/main"}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sekret") {
		t.Errorf("403 body echoes the credential: %s", raw)
	}
	if !strings.Contains(string(raw), "repository not in allowlist") {
		t.Errorf("403 body = %s, want the fixed allowlist message", raw)
	}
}

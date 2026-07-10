package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hutusi/janus/internal/allowlist"
	"github.com/hutusi/janus/internal/engine"
	"github.com/hutusi/janus/internal/model"
	"github.com/hutusi/janus/internal/provider"
	"github.com/hutusi/janus/internal/runner"
	"github.com/hutusi/janus/internal/store"
)

const (
	testGitLabSecret = "shh-secret"
	testAPIToken     = "test-api-token"
)

// initGitRepo creates a repo containing .janus/ci.yml and returns its path + SHA.
func initGitRepo(t *testing.T, pipeline string) (dir, sha string) {
	t.Helper()
	return initGitRepoFiles(t, map[string]string{".janus/ci.yml": pipeline})
}

// initGitRepoFiles creates a repo containing the given files (path → content)
// in one commit and returns its path + SHA.
func initGitRepoFiles(t *testing.T, files map[string]string) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@e.com")
	git("config", "user.name", "T")
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-q", "-m", "init")
	return dir, git("rev-parse", "HEAD")
}

// newTestServer builds a server that allows all repos ("*"), so existing
// end-to-end tests aren't gated by the allowlist.
func newTestServer(t *testing.T) *httptest.Server {
	return newTestServerAllow(t, "*")
}

// newTestServerAllow builds a server whose runner enforces the given allowlist
// entries. Used by allowlist-specific tests with a narrow list.
func newTestServerAllow(t *testing.T, entries ...string) *httptest.Server {
	t.Helper()
	st := store.NewMemory()
	eng := engine.New(st)
	allow, err := allowlist.New(entries)
	if err != nil {
		t.Fatalf("allowlist.New: %v", err)
	}
	rn := runner.New(st, eng, runner.Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 4, Allowlist: allow})
	srv := New(st, rn, "test",
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithProvider(provider.GitLab{}, testGitLabSecret),
		WithAPIToken(testAPIToken),
	)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// newTestServerPersistent builds an allow-all server whose runner reuses one
// workspace per repo (workspace_strategy: persistent).
func newTestServerPersistent(t *testing.T) *httptest.Server {
	t.Helper()
	st := store.NewMemory()
	eng := engine.New(st)
	allow, err := allowlist.New([]string{"*"})
	if err != nil {
		t.Fatalf("allowlist.New: %v", err)
	}
	rn := runner.New(st, eng, runner.Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 4, Allowlist: allow, Persistent: true})
	srv := New(st, rn, "test",
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithAPIToken(testAPIToken),
	)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// commitFileIn writes path/content in the repo at dir and commits it,
// returning the new HEAD SHA.
func commitFileIn(t *testing.T, dir, path, content string) (sha string) {
	t.Helper()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	full := filepath.Join(dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "update "+path)
	return git("rev-parse", "HEAD")
}

// apiGet issues an authenticated GET (harmless on open routes).
func apiGet(t *testing.T, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+testAPIToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// postTrigger issues an authenticated POST /api/trigger.
func postTrigger(t *testing.T, ts *httptest.Server, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/api/trigger", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHealth(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestTriggerRunEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, `name: ci
on: { push: { branches: [main] } }
jobs:
  build:
    steps:
      - run: echo "hello from janus on ${{ branch }}"
`)
	ts := newTestServer(t)

	// Trigger.
	body, _ := json.Marshal(map[string]string{"repo_url": repo, "sha": sha, "ref": "refs/heads/main", "branch": "main"})
	resp := postTrigger(t, ts, string(body))
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("trigger status = %d, want 202; body=%s", resp.StatusCode, b)
	}
	var tr struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tr)
	_ = resp.Body.Close()
	if tr.RunID == "" {
		t.Fatal("empty run_id")
	}

	// Poll the run to a terminal state.
	run := pollRun(t, ts, tr.RunID, 15*time.Second)
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %s, want success", run.Status)
	}
	if jr := findJob(run, "build"); jr == nil || jr.Status != model.StatusSuccess {
		t.Fatalf("build job = %+v, want success", jr)
	}

	// Logs contain the interpolated echo.
	logs := getText(t, ts.URL+"/api/runs/"+tr.RunID+"/logs")
	if !strings.Contains(logs, "hello from janus on main") {
		t.Errorf("logs = %q, want it to contain the echoed line", logs)
	}

	// Dashboard pages render.
	if body := getText(t, ts.URL+"/"); !strings.Contains(body, tr.RunID[:8]) {
		t.Errorf("index page does not list the run")
	}
	if code := statusOf(t, ts.URL+"/runs/"+tr.RunID); code != http.StatusOK {
		t.Errorf("run page status = %d, want 200", code)
	}
}

func TestAPIAuth(t *testing.T) {
	st := store.NewMemory()
	eng := engine.New(st)
	allow, _ := allowlist.New([]string{"*"})
	rn := runner.New(st, eng, runner.Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 4, Allowlist: allow})
	srv := New(st, rn, "test",
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithAPIToken("secret-token"),
	)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	rawStatus := func(method, url, token string) int {
		req, _ := http.NewRequest(method, url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if code := rawStatus("GET", ts.URL+"/api/runs", ""); code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", code)
	}
	if code := rawStatus("GET", ts.URL+"/api/runs", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", code)
	}
	if code := rawStatus("GET", ts.URL+"/healthz", ""); code != http.StatusOK {
		t.Errorf("health should stay open: status = %d, want 200", code)
	}
	if code := rawStatus("GET", ts.URL+"/api/runs", "secret-token"); code != http.StatusOK {
		t.Errorf("valid token: status = %d, want 200", code)
	}
}

func TestTriggerRequiresToken(t *testing.T) {
	// A server with no API token disables /api/trigger entirely.
	st := store.NewMemory()
	eng := engine.New(st)
	allow, _ := allowlist.New([]string{"*"})
	rn := runner.New(st, eng, runner.Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 4, Allowlist: allow})
	srv := New(st, rn, "test", WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/trigger", "application/json", strings.NewReader(`{"repo_url":"x","ref":"refs/heads/main"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when no --api-token is configured", resp.StatusCode)
	}
}

func TestTriggerValidation(t *testing.T) {
	ts := newTestServer(t)
	resp := postTrigger(t, ts, `{}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing repo_url", resp.StatusCode)
	}
}

func TestTriggerPipelinePathOverride(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepoFiles(t, map[string]string{
		".janus/ci.yml": `name: ci
on: { push: { branches: [main] } }
jobs:
  build:
    steps:
      - run: echo "default pipeline"
`,
		".janus/release.yml": `name: release
on: { push: { branches: [main] } }
jobs:
  publish:
    steps:
      - run: echo "release pipeline"
`,
	})
	ts := newTestServer(t)

	trigger := func(pipelinePath string) *model.Run {
		t.Helper()
		payload := map[string]string{"repo_url": repo, "sha": sha, "ref": "refs/heads/main", "branch": "main"}
		if pipelinePath != "" {
			payload["pipeline_path"] = pipelinePath
		}
		body, _ := json.Marshal(payload)
		resp := postTrigger(t, ts, string(body))
		if resp.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			t.Fatalf("trigger status = %d, want 202; body=%s", resp.StatusCode, b)
		}
		var tr struct {
			RunID string `json:"run_id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&tr)
		_ = resp.Body.Close()
		return pollRun(t, ts, tr.RunID, 15*time.Second)
	}

	// pipeline_path selects the alternate file, relative to the pipeline dir.
	run := trigger("release.yml")
	if run.Status != model.StatusSuccess {
		t.Fatalf("run status = %s, want success", run.Status)
	}
	if run.WorkflowName != "release" {
		t.Errorf("workflow = %q, want %q", run.WorkflowName, "release")
	}
	if run.Event.PipelinePath != "release.yml" {
		t.Errorf("event pipeline_path = %q, want the override recorded", run.Event.PipelinePath)
	}
	if jr := findJob(run, "publish"); jr == nil || jr.Status != model.StatusSuccess {
		t.Fatalf("publish job = %+v, want success", jr)
	}

	// Without the field, the configured default still runs.
	run = trigger("")
	if run.Status != model.StatusSuccess {
		t.Fatalf("default run status = %s, want success", run.Status)
	}
	if run.WorkflowName != "ci" {
		t.Errorf("default workflow = %q, want %q", run.WorkflowName, "ci")
	}
	if run.Event.PipelinePath != "" {
		t.Errorf("default event pipeline_path = %q, want empty", run.Event.PipelinePath)
	}
}

func TestTriggerPipelinePathRejected(t *testing.T) {
	ts := newTestServer(t)
	// Rejection happens before checkout, so no git repo is needed.
	for _, p := range []string{"../evil.yml", filepath.Join(t.TempDir(), "evil.yml")} {
		body, _ := json.Marshal(map[string]string{"repo_url": "/nonexistent/repo", "sha": "deadbeef", "pipeline_path": p})
		resp := postTrigger(t, ts, string(body))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("pipeline_path %q: status = %d, want 400", p, resp.StatusCode)
		}
	}
}

func TestPersistentWorkspaceEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha1 := initGitRepo(t, `name: ci
on: { push: { branches: [main] } }
jobs:
  build:
    steps:
      - run: echo "build v1"
`)
	ts := newTestServerPersistent(t)

	trigger := func(sha string) *model.Run {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"repo_url": repo, "sha": sha, "ref": "refs/heads/main", "branch": "main"})
		resp := postTrigger(t, ts, string(body))
		if resp.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			t.Fatalf("trigger status = %d, want 202; body=%s", resp.StatusCode, b)
		}
		var tr struct {
			RunID string `json:"run_id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&tr)
		_ = resp.Body.Close()
		return pollRun(t, ts, tr.RunID, 15*time.Second)
	}

	run1 := trigger(sha1)
	if run1.Status != model.StatusSuccess {
		t.Fatalf("run1 status = %s, want success", run1.Status)
	}
	if base := filepath.Base(run1.WorkspaceDir); !strings.HasPrefix(base, "persist-") {
		t.Fatalf("run1 workspace = %q, want a persist-* dir", run1.WorkspaceDir)
	}
	// Simulate a build cache: an untracked file in the persistent workspace.
	marker := filepath.Join(run1.WorkspaceDir, "node_modules_marker")
	if err := os.WriteFile(marker, []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}

	// New commit in the source repo; the next run must land it in the SAME dir.
	sha2 := commitFileIn(t, repo, ".janus/ci.yml", `name: ci
on: { push: { branches: [main] } }
jobs:
  build:
    steps:
      - run: echo "build v2"
`)
	run2 := trigger(sha2)
	if run2.Status != model.StatusSuccess {
		t.Fatalf("run2 status = %s, want success", run2.Status)
	}
	if run2.WorkspaceDir != run1.WorkspaceDir {
		t.Errorf("run2 workspace = %q, want reuse of %q", run2.WorkspaceDir, run1.WorkspaceDir)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("untracked cache marker should survive between runs: %v", err)
	}
	logs := getText(t, ts.URL+"/api/runs/"+run2.ID+"/logs")
	if !strings.Contains(logs, "build v2") {
		t.Errorf("run2 logs = %q, want the updated pipeline's output (fetch+reset landed)", logs)
	}
}

func pollRun(t *testing.T, ts *httptest.Server, id string, timeout time.Duration) *model.Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resp := apiGet(t, ts.URL+"/api/runs/"+id)
		var run model.Run
		_ = json.NewDecoder(resp.Body).Decode(&run)
		_ = resp.Body.Close()
		if run.Status.Terminal() {
			return &run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not finish within %s (last status %s)", id, timeout, run.Status)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func findJob(run *model.Run, name string) *model.JobRun {
	for _, jr := range run.Jobs {
		if jr.Name == name {
			return jr
		}
	}
	return nil
}

func getText(t *testing.T, url string) string {
	t.Helper()
	resp := apiGet(t, url)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func statusOf(t *testing.T, url string) int {
	t.Helper()
	resp := apiGet(t, url)
	_ = resp.Body.Close()
	return resp.StatusCode
}

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
	if err := os.MkdirAll(filepath.Join(dir, ".janus"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".janus", "ci.yml"), []byte(pipeline), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "init")
	return dir, git("rev-parse", "HEAD")
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st := store.NewMemory()
	eng := engine.New(st)
	rn := runner.New(st, eng, t.TempDir(), ".janus/ci.yml", false, 4)
	srv := New(st, rn, "test",
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithProvider(provider.GitLab{}, testGitLabSecret),
		WithAPIToken(testAPIToken),
	)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
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
	rn := runner.New(st, eng, t.TempDir(), ".janus/ci.yml", false, 4)
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
	rn := runner.New(st, eng, t.TempDir(), ".janus/ci.yml", false, 4)
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

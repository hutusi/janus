package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/hutusi/janus/internal/pipeline"
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

// saveFailStore rejects SaveRun, as a full/read-only data dir would.
type saveFailStore struct {
	store.Store
}

func (saveFailStore) SaveRun(*model.Run) error { return errors.New("disk full") }

// newTestServerStore builds an allow-all server over the given store (used to
// inject failure). Providers + token are wired like newTestServer.
func newTestServerStore(t *testing.T, st store.Store) *httptest.Server {
	t.Helper()
	allow, _ := allowlist.New([]string{"*"})
	rn := runner.New(st, engine.New(st), runner.Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 4, Allowlist: allow})
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

	// Following a terminal step streams its full output and terminates.
	followed := getText(t, ts.URL+"/api/runs/"+tr.RunID+"/logs?job=build&step=0&follow=1")
	if !strings.Contains(followed, "hello from janus on main") {
		t.Errorf("followed logs = %q, want the full step output", followed)
	}

	// Dashboard pages render.
	if body := getText(t, ts.URL+"/"); !strings.Contains(body, tr.RunID[:8]) {
		t.Errorf("index page does not list the run")
	}
	if code := statusOf(t, ts.URL+"/runs/"+tr.RunID); code != http.StatusOK {
		t.Errorf("run page status = %d, want 200", code)
	}
}

// terminalFailStore fails every terminal UpdateRun, so a completed run cannot
// persist its final state — which degrades the engine.
type terminalFailStore struct {
	store.Store
}

func (terminalFailStore) UpdateRun(run *model.Run) error {
	if run.Status.Terminal() {
		return errors.New("disk full")
	}
	return nil
}

func TestRunPageBoundsLogs(t *testing.T) {
	runPageStepTailBytes = 32
	t.Cleanup(func() { runPageStepTailBytes = 64 << 10 })

	st := store.NewMemory()
	eng := engine.New(st)
	allow, _ := allowlist.New([]string{"*"})
	rn := runner.New(st, eng, runner.Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 1, Allowlist: allow})
	srv := New(st, rn, "test", WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	run := &model.Run{ID: "big", Status: model.StatusSuccess, CreatedAt: time.Now(),
		Jobs: []*model.JobRun{{Name: "build", Status: model.StatusSuccess, Steps: []*model.StepRun{{Index: 0, Status: model.StatusSuccess}}}}}
	if err := st.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	w, _ := st.LogWriter("big", "build", 0)
	_, _ = io.WriteString(w, strings.Repeat("A", 100)+"TAILMARK")
	_ = w.Close()

	body := getText(t, ts.URL+"/runs/big")
	if !strings.Contains(body, "TAILMARK") {
		t.Error("run page should show the tail of the log")
	}
	if !strings.Contains(body, "earlier output truncated") {
		t.Error("run page should mark the truncation")
	}
	if strings.Contains(body, strings.Repeat("A", 100)) {
		t.Error("run page should NOT contain the full (head) of an oversized log")
	}

	// A small log renders whole with no marker.
	small := &model.Run{ID: "small", Status: model.StatusSuccess, CreatedAt: time.Now(),
		Jobs: []*model.JobRun{{Name: "build", Status: model.StatusSuccess, Steps: []*model.StepRun{{Index: 0, Status: model.StatusSuccess}}}}}
	_ = st.SaveRun(small)
	w2, _ := st.LogWriter("small", "build", 0)
	_, _ = io.WriteString(w2, "tiny")
	_ = w2.Close()
	body2 := getText(t, ts.URL+"/runs/small")
	if !strings.Contains(body2, "tiny") || strings.Contains(body2, "truncated") {
		t.Errorf("small log should render whole with no marker:\n%s", body2)
	}
}

func TestRunPageTotalBudget(t *testing.T) {
	runPageStepTailBytes = 64
	runPageTotalBytes = 200
	t.Cleanup(func() { runPageStepTailBytes, runPageTotalBytes = 64<<10, 1<<20 })

	st := store.NewMemory()
	eng := engine.New(st)
	allow, _ := allowlist.New([]string{"*"})
	rn := runner.New(st, eng, runner.Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 1, Allowlist: allow})
	srv := New(st, rn, "test", WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// 20 steps, each with a 64-byte log — far past the 200-byte page budget.
	steps := make([]*model.StepRun, 20)
	for i := range steps {
		steps[i] = &model.StepRun{Index: i, Status: model.StatusSuccess}
	}
	run := &model.Run{ID: "many", Status: model.StatusSuccess, CreatedAt: time.Now(),
		Jobs: []*model.JobRun{{Name: "build", Status: model.StatusSuccess, Steps: steps}}}
	if err := st.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	for i := range steps {
		w, _ := st.LogWriter("many", "build", i)
		_, _ = io.WriteString(w, strings.Repeat("x", 64))
		_ = w.Close()
	}

	body := getText(t, ts.URL+"/runs/many")
	// The rendered log content must be bounded near the total budget, not the
	// full 20×64 = 1280 bytes. Count the log payload ('x') rather than the
	// whole HTML (which also carries a bounded per-step status table).
	if xs := strings.Count(body, "x"); int64(xs) > runPageTotalBytes {
		t.Errorf("log content not bounded by the total budget: %d 'x' bytes rendered", xs)
	}
	if !strings.Contains(body, "page size limit reached") {
		t.Error("run page should note that remaining step logs were omitted")
	}
}

// countingStore counts ReadLogsTail calls to prove the renderer stops reading
// once the page budget is spent.
type countingStore struct {
	store.Store
	tailReads int
}

func (c *countingStore) ReadLogsTail(runID, job string, step int, maxBytes int64) (io.ReadCloser, bool, error) {
	c.tailReads++
	return c.Store.ReadLogsTail(runID, job, step, maxBytes)
}

func TestWriteRunLogsTailStopsReadingWhenFull(t *testing.T) {
	cs := &countingStore{Store: store.NewMemory()}
	srv := New(cs, nil, "test", WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))

	const total, perStep int64 = 100, 100
	steps := make([]*model.StepRun, 500)
	for i := range steps {
		steps[i] = &model.StepRun{Index: i, Status: model.StatusSuccess}
		w, _ := cs.LogWriter("r", "build", i)
		_, _ = io.WriteString(w, strings.Repeat("x", 200)) // each step overflows the budget
		_ = w.Close()
	}
	run := &model.Run{ID: "r", Status: model.StatusSuccess,
		Jobs: []*model.JobRun{{Name: "build", Status: model.StatusSuccess, Steps: steps}}}

	var buf bytes.Buffer
	srv.writeRunLogsTail(&buf, run, perStep, total)

	// The budget is spent within the first step or two — the render must not
	// have read all 500 step logs from the store.
	if cs.tailReads > 3 {
		t.Errorf("renderer read %d step logs, want it to stop once the budget was spent", cs.tailReads)
	}
}

func TestWriteRunLogsTailBounded(t *testing.T) {
	st := store.NewMemory()
	srv := New(st, nil, "test", WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))

	// A long job name (would blow a header) plus several oversized step logs —
	// everything (headers, markers, tails) must stay within the total budget.
	const total, perStep int64 = 500, 100
	longName := strings.Repeat("j", 300)
	steps := make([]*model.StepRun, 10)
	for i := range steps {
		steps[i] = &model.StepRun{Index: i, Status: model.StatusSuccess}
		w, _ := st.LogWriter("r", longName, i)
		_, _ = io.WriteString(w, strings.Repeat("x", 400))
		_ = w.Close()
	}
	run := &model.Run{ID: "r", Status: model.StatusSuccess,
		Jobs: []*model.JobRun{{Name: longName, Status: model.StatusSuccess, Steps: steps}}}

	var buf bytes.Buffer
	srv.writeRunLogsTail(&buf, run, perStep, total)

	const trailerMax = 160 // the fixed "page size limit reached …" trailer
	if int64(buf.Len()) > total+trailerMax {
		t.Errorf("rendered logs = %d bytes, want <= %d (budget %d + trailer)", buf.Len(), total+trailerMax, total)
	}
	if !strings.Contains(buf.String(), "page size limit reached") {
		t.Error("a truncated render should end with the page-limit trailer")
	}
}

func TestRunPageTruncatesLongCommand(t *testing.T) {
	st := store.NewMemory()
	srv := New(st, nil, "test", WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	longCmd := strings.Repeat("Z", 5000)
	run := &model.Run{ID: "cmd", Status: model.StatusSuccess, CreatedAt: time.Now(),
		Jobs: []*model.JobRun{{Name: "build", Status: model.StatusSuccess,
			Steps: []*model.StepRun{{Index: 0, Command: longCmd, Status: model.StatusSuccess}}}}}
	if err := st.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	body := getText(t, ts.URL+"/runs/cmd")
	if strings.Contains(body, longCmd) {
		t.Error("run page should truncate a very long command, not render it whole")
	}
	if !strings.Contains(body, "…") {
		t.Error("truncated command should show an ellipsis")
	}
}

func TestHealthDegraded(t *testing.T) {
	wf, err := pipeline.Parse([]byte("name: ci\non: { push: {} }\njobs:\n  build:\n    steps:\n      - run: echo hi\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st := terminalFailStore{store.NewMemory()}
	eng := engine.New(st, engine.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	// Run a trivial pipeline: its steps succeed but the terminal write fails,
	// so the engine latches degraded.
	if _, err := eng.Run(context.Background(), wf, model.Event{Kind: model.EventManual}, t.TempDir()); err == nil {
		t.Fatal("expected a terminal-persist error to degrade the engine")
	}
	rn := runner.New(st, eng, runner.Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 1})
	srv := New(st, rn, "test", WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("degraded /healthz status = %d, want 503", resp.StatusCode)
	}
	var body struct{ Status string }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Status != "degraded" {
		t.Errorf("status = %q, want degraded", body.Status)
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

func TestTriggerStoreUnavailableReturns503(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, "name: ci\non: { push: {} }\njobs:\n  build:\n    steps:\n      - run: echo hi\n")
	ts := newTestServerStore(t, saveFailStore{store.NewMemory()})

	body, _ := json.Marshal(map[string]string{"repo_url": repo, "sha": sha, "ref": "refs/heads/main", "branch": "main"})
	resp := postTrigger(t, ts, string(body))
	defer func() { _ = resp.Body.Close() }()
	// A storage failure is Janus's problem: 503 (retriable), not a 400 client error.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (store unavailable)", resp.StatusCode)
	}
}

func TestTriggerStrictJSON(t *testing.T) {
	ts := newTestServer(t)
	for name, body := range map[string]string{
		"unknown field":         `{"repo_url": "/tmp/x", "ref": "refs/heads/main", "bogus": 1}`,
		"trailing object":       `{"repo_url": "/tmp/x", "ref": "refs/heads/main"}{"more": true}`,
		"trailing close-array":  `{"repo_url": "/tmp/x", "ref": "refs/heads/main"}]`,
		"trailing close-object": `{"repo_url": "/tmp/x", "ref": "refs/heads/main"}}`,
		"trailing garbage":      `{"repo_url": "/tmp/x", "ref": "refs/heads/main"}xyz`,
	} {
		resp := postTrigger(t, ts, body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, resp.StatusCode)
		}
	}
}

func TestListRunsPagination(t *testing.T) {
	st := store.NewMemory()
	base := time.Now()
	for i := 0; i < 3; i++ { // n0 newest … n2 oldest
		run := &model.Run{ID: "n" + string(rune('0'+i)), Status: model.StatusSuccess, CreatedAt: base.Add(time.Duration(-i) * time.Minute)}
		if err := st.SaveRun(run); err != nil {
			t.Fatal(err)
		}
	}
	eng := engine.New(st)
	allow, _ := allowlist.New([]string{"*"})
	rn := runner.New(st, eng, runner.Options{WSRoot: t.TempDir(), PipelinePath: ".janus/ci.yml", MaxRuns: 1, Allowlist: allow})
	srv := New(st, rn, "test", WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), WithAPIToken(testAPIToken))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp := apiGet(t, ts.URL+"/api/runs?limit=1&offset=1")
	defer func() { _ = resp.Body.Close() }()
	var runs []model.Run
	if err := json.NewDecoder(resp.Body).Decode(&runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "n1" {
		t.Fatalf("limit=1&offset=1 = %+v, want the 2nd-newest (n1)", runs)
	}
}

func TestTriggerBodyTooLarge(t *testing.T) {
	ts := newTestServer(t)
	resp := postTrigger(t, ts, `{"repo_url": "`+strings.Repeat("a", maxTriggerBody)+`"}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
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

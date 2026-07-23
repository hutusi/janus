package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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

func TestTriggerCheckoutFailureIsAsync(t *testing.T) {
	ts := newTestServer(t)

	// An unclonable repo is still a 202: the manual API records the run and
	// returns before checkout, and the failure lands on the run record.
	body, _ := json.Marshal(map[string]string{
		"repo_url": "/nonexistent/repo", "sha": "0123456789abcdef0123456789abcdef01234567", "ref": "refs/heads/main",
	})
	resp := postTrigger(t, ts, string(body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("trigger status = %d, want 202; body=%s", resp.StatusCode, b)
	}
	var tr struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tr)
	if tr.RunID == "" {
		t.Fatal("empty run_id")
	}
	run := pollRun(t, ts, tr.RunID, 15*time.Second)
	if run.Status != model.StatusFailed {
		t.Errorf("run status = %s, want failed", run.Status)
	}
	if !strings.Contains(run.Reason, "checkout") {
		t.Errorf("run reason = %q, want it to name the checkout", run.Reason)
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

func TestDashboardTruncatesLongBranch(t *testing.T) {
	st := store.NewMemory()
	srv := New(st, nil, "test", WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	longBranch := strings.Repeat("B", 5000)
	run := &model.Run{ID: "br", Status: model.StatusSuccess, CreatedAt: time.Now(),
		WorkflowName: "ci", Event: model.Event{Kind: model.EventPush, Branch: longBranch},
		Jobs: []*model.JobRun{{Name: "build", Status: model.StatusSuccess, Steps: []*model.StepRun{{Index: 0, Status: model.StatusSuccess}}}}}
	if err := st.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/runs/br"} {
		body := getText(t, ts.URL+path)
		if strings.Contains(body, longBranch) {
			t.Errorf("%s renders the full long branch, want it truncated", path)
		}
	}
}

func TestDashboardDurations(t *testing.T) {
	st := store.NewMemory()
	srv := New(st, nil, "test", WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	base := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	finished := &model.Run{ID: "done", Status: model.StatusSuccess, WorkflowName: "ci",
		Event:     model.Event{Kind: model.EventPush, Branch: "main"},
		CreatedAt: base, StartedAt: base, FinishedAt: base.Add(83 * time.Second),
		Jobs: []*model.JobRun{{Name: "build", Status: model.StatusSuccess,
			StartedAt: base, FinishedAt: base.Add(2*time.Hour + 5*time.Minute + 3*time.Second),
			Steps: []*model.StepRun{
				{Index: 0, Status: model.StatusSuccess, StartedAt: base, FinishedAt: base.Add(12 * time.Second)},
				{Index: 1, Status: model.StatusSkipped}, // never started
			}}}}
	// Fractional-second start: the data-started anchor must keep the 900ms
	// (a whole-second anchor wobbles the ticker by ±1s on every re-anchor).
	liveStart := base.Add(time.Minute + 900*time.Millisecond)
	running := &model.Run{ID: "live", Status: model.StatusRunning, WorkflowName: "ci",
		Event:     model.Event{Kind: model.EventPush, Branch: "main"},
		CreatedAt: liveStart, StartedAt: liveStart,
		Jobs: []*model.JobRun{{Name: "build", Status: model.StatusRunning, StartedAt: liveStart,
			Steps: []*model.StepRun{{Index: 0, Status: model.StatusRunning, StartedAt: liveStart}}}}}
	for _, r := range []*model.Run{finished, running} {
		if err := st.SaveRun(r); err != nil {
			t.Fatal(err)
		}
	}

	list := getText(t, ts.URL+"/")
	if !strings.Contains(list, "1m 23s") {
		t.Error("run list misses the finished run's duration 1m 23s")
	}
	wantAttr := `data-started="` + strconv.FormatInt(running.StartedAt.UnixMilli(), 10) + `"`
	if !strings.Contains(list, wantAttr) {
		t.Errorf("run list misses %s for the running run", wantAttr)
	}
	if !strings.Contains(list, `<meta http-equiv="refresh" content="5">`) {
		t.Error("run list with an active run misses the 5s auto-refresh")
	}

	donePage := getText(t, ts.URL+"/runs/done")
	for _, want := range []string{"1m 23s", "2h 5m 3s", "12s", "—"} {
		if !strings.Contains(donePage, want) {
			t.Errorf("finished run page misses %q", want)
		}
	}
	// The ticker script mentions data-started in its selector, so check for
	// the attribute form specifically.
	if strings.Contains(donePage, `data-started="`) {
		t.Error("finished run page renders a ticking span, want static text only")
	}

	livePage := getText(t, ts.URL+"/runs/live")
	if got := strings.Count(livePage, wantAttr); got != 3 {
		t.Errorf("running run page has %d ticking spans (run/job/step), want 3", got)
	}
	for _, page := range []string{list, donePage, livePage} {
		if !strings.Contains(page, "setInterval") {
			t.Error("dashboard page misses the elapsed ticker script")
		}
	}
}

func TestIndexPagination(t *testing.T) {
	orig := indexPageSize
	indexPageSize = 2
	t.Cleanup(func() { indexPageSize = orig })

	st := store.NewMemory()
	srv := New(st, nil, "test", WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Single run: everything fits on one page, no pager.
	base := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)
	save := func(i int) {
		t.Helper()
		run := &model.Run{ID: fmt.Sprintf("pg%d", i), Status: model.StatusSuccess, WorkflowName: "ci",
			Event: model.Event{Kind: model.EventPush}, CreatedAt: base.Add(-time.Duration(i) * time.Minute)}
		if err := st.SaveRun(run); err != nil {
			t.Fatal(err)
		}
	}
	save(0)
	if body := getText(t, ts.URL+"/"); strings.Contains(body, `class="pager`) {
		t.Error("single-page run list renders a pager, want none")
	}

	// Five runs at 2 per page → 3 pages; pg0 newest … pg4 oldest.
	for i := 1; i < 5; i++ {
		save(i)
	}
	page1 := getText(t, ts.URL+"/")
	for _, want := range []string{`class="pager`, `<strong>1</strong>`, `href="/?page=2"`, `href="/?page=3"`, "pg0", "pg1"} {
		if !strings.Contains(page1, want) {
			t.Errorf("page 1 misses %q", want)
		}
	}
	if strings.Contains(page1, "pg2") {
		t.Error("page 1 lists pg2, want it on page 2")
	}

	page3 := getText(t, ts.URL+"/?page=3")
	for _, want := range []string{"pg4", `<strong>3</strong>`, `href="/"`, `href="/?page=2"`} {
		if !strings.Contains(page3, want) {
			t.Errorf("page 3 misses %q", want)
		}
	}
	if strings.Contains(page3, "pg1") {
		t.Error("page 3 lists pg1, want only the oldest run")
	}

	// Degradation: a page past the end renders empty but keeps the pager
	// (including int64-max, whose offset multiplication would wrap negative
	// without the maxIndexPage clamp and mislabel page 1); a non-numeric page
	// falls back to page 1.
	for _, q := range []string{"/?page=99", "/?page=9223372036854775807"} {
		past := getText(t, ts.URL+q)
		if strings.Contains(past, "pg0") || !strings.Contains(past, `class="pager`) {
			t.Errorf("%s should list nothing but keep the pager", q)
		}
	}
	if bad := getText(t, ts.URL+"/?page=abc"); !strings.Contains(bad, "pg0") {
		t.Error("non-numeric page should fall back to page 1")
	}
}

func TestFaviconAndLogo(t *testing.T) {
	st := store.NewMemory()
	srv := New(st, nil, "test", WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/favicon.svg")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/favicon.svg status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("/favicon.svg Content-Type = %q, want image/svg+xml", ct)
	}
	svg := string(body)
	if !strings.Contains(svg, "<svg") || !strings.Contains(svg, faviconFill) {
		t.Errorf("/favicon.svg misses svg markup or the %s brand fill:\n%s", faviconFill, svg)
	}

	run := &model.Run{ID: "logo", Status: model.StatusSuccess, WorkflowName: "ci",
		Event: model.Event{Kind: model.EventPush}, CreatedAt: time.Now()}
	if err := st.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/runs/logo"} {
		page := getText(t, ts.URL+path)
		if !strings.Contains(page, `<link rel="icon" type="image/svg+xml" href="/favicon.svg">`) {
			t.Errorf("%s misses the favicon link", path)
		}
		if !strings.Contains(page, `<svg class="logo"`) || !strings.Contains(page, `fill="currentColor"`) {
			t.Errorf("%s misses the inline currentColor header logo", path)
		}
		// The visible "Janus" wordmark names the mark; the svg must stay
		// decorative or screen readers announce "Janus Janus".
		if !strings.Contains(page, `aria-hidden="true"`) || strings.Contains(page, "aria-label") {
			t.Errorf("%s inline logo must be aria-hidden with no aria-label", path)
		}
	}
}

func TestIndexRefreshOnlyWhenActive(t *testing.T) {
	st := store.NewMemory()
	srv := New(st, nil, "test", WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	if body := getText(t, ts.URL+"/"); strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("empty run list auto-refreshes, want static")
	}
	terminal := &model.Run{ID: "t1", Status: model.StatusFailed, WorkflowName: "ci",
		Event: model.Event{Kind: model.EventPush}, CreatedAt: time.Now()}
	if err := st.SaveRun(terminal); err != nil {
		t.Fatal(err)
	}
	if body := getText(t, ts.URL+"/"); strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("all-terminal run list auto-refreshes, want static")
	}
	active := &model.Run{ID: "a1", Status: model.StatusPending, WorkflowName: "ci",
		Event: model.Event{Kind: model.EventPush}, CreatedAt: time.Now()}
	if err := st.SaveRun(active); err != nil {
		t.Fatal(err)
	}
	if body := getText(t, ts.URL+"/"); !strings.Contains(body, `<meta http-equiv="refresh" content="5">`) {
		t.Error("run list with a pending run misses the 5s auto-refresh")
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
	if code := rawStatus("POST", ts.URL+"/api/runs/x/cancel", ""); code != http.StatusUnauthorized {
		t.Errorf("cancel without token: status = %d, want 401", code)
	}
	if code := rawStatus("POST", ts.URL+"/api/runs/x/cancel", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("cancel with wrong token: status = %d, want 401", code)
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

	// The cancel route is token-mandatory in the same way.
	cresp, err := http.Post(ts.URL+"/api/runs/x/cancel", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cresp.Body.Close() }()
	if cresp.StatusCode != http.StatusForbidden {
		t.Fatalf("cancel status = %d, want 403 when no --api-token is configured", cresp.StatusCode)
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
	var runs []model.RunSummary
	if err := json.NewDecoder(resp.Body).Decode(&runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "n1" {
		t.Fatalf("limit=1&offset=1 = %+v, want the 2nd-newest (n1)", runs)
	}
}

func TestListRunsReturnsSummariesNotJobs(t *testing.T) {
	st := store.NewMemory()
	// A run with a heavy jobs slice — the list must not carry it.
	run := &model.Run{ID: "r", WorkflowName: "ci", Status: model.StatusSuccess, CreatedAt: time.Now(),
		Event: model.Event{Kind: model.EventPush, Branch: "main"},
		Jobs:  []*model.JobRun{{Name: "build", Steps: []*model.StepRun{{Index: 0, Command: "secret-heavy-command"}}}}}
	if err := st.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	srv := New(st, nil, "test", WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), WithAPIToken(testAPIToken))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	body := getText(t, ts.URL+"/api/runs")
	// The summary carries the listing fields but not the jobs/commands.
	if !strings.Contains(body, `"id":"r"`) || !strings.Contains(body, `"branch":"main"`) {
		t.Errorf("list should carry summary fields: %s", body)
	}
	if strings.Contains(body, "secret-heavy-command") || strings.Contains(body, `"jobs"`) {
		t.Errorf("list must not carry the heavy jobs slice: %s", body)
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

// --- Run cancel endpoint ------------------------------------------------------

// postCancel issues an authenticated POST /api/runs/{id}/cancel.
func postCancel(t *testing.T, ts *httptest.Server, id string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/api/runs/"+id+"/cancel", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestCancelRunEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("blocking sh pipeline")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, sha := initGitRepo(t, `name: ci
on: { push: { branches: [main] } }
jobs:
  build:
    steps:
      - run: sleep 30
`)
	ts := newTestServer(t)

	body, _ := json.Marshal(map[string]string{"repo_url": repo, "sha": sha, "ref": "refs/heads/main", "branch": "main"})
	resp := postTrigger(t, ts, string(body))
	var tr struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tr)
	_ = resp.Body.Close()
	if tr.RunID == "" {
		t.Fatal("empty run_id")
	}

	// Wait until the run is actually executing, then cancel it.
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp := apiGet(t, ts.URL+"/api/runs/"+tr.RunID)
		var run model.Run
		_ = json.NewDecoder(resp.Body).Decode(&run)
		_ = resp.Body.Close()
		if run.Status == model.StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never reached running (last %s)", run.Status)
		}
		time.Sleep(25 * time.Millisecond)
	}

	cresp := postCancel(t, ts, tr.RunID)
	b, _ := io.ReadAll(cresp.Body)
	_ = cresp.Body.Close()
	if cresp.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel status = %d, want 202; body=%s", cresp.StatusCode, b)
	}

	run := pollRun(t, ts, tr.RunID, 15*time.Second)
	if run.Status != model.StatusCancelled {
		t.Fatalf("run status = %s, want cancelled", run.Status)
	}
	if run.Reason != "cancelled via API" {
		t.Errorf("reason = %q, want %q", run.Reason, "cancelled via API")
	}

	// Cancelling a settled run conflicts.
	c2 := postCancel(t, ts, tr.RunID)
	_ = c2.Body.Close()
	if c2.StatusCode != http.StatusConflict {
		t.Errorf("second cancel status = %d, want 409", c2.StatusCode)
	}
}

func TestCancelRunNotFound(t *testing.T) {
	ts := newTestServer(t)
	resp := postCancel(t, ts, "no-such-run")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestCancelFinishedRunConflict(t *testing.T) {
	st := store.NewMemory()
	if err := st.SaveRun(&model.Run{ID: "done-1", Status: model.StatusSuccess, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ts := newTestServerStore(t, st)
	resp := postCancel(t, ts, "done-1")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

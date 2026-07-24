package status

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hutusi/janus/internal/model"
)

// okRT is a RoundTripper that returns 201 without a network call, counting
// requests — used to exercise scheme handling without a real (or TLS) server.
type okRT struct{ n *int32 }

func (t okRT) RoundTrip(*http.Request) (*http.Response, error) {
	atomic.AddInt32(t.n, 1)
	return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type captured struct {
	method string
	path   string
	header http.Header
	body   []byte
}

func recordingServer(t *testing.T, reqs chan<- captured) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqs <- captured{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: b}
		w.WriteHeader(http.StatusCreated) // GitLab replies 201
	}))
	t.Cleanup(ts.Close)
	return ts
}

const testSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

func gitlabRun(status model.Status) *model.Run {
	return &model.Run{
		ID:           "run123",
		WorkflowName: "ci",
		Status:       status,
		Reason:       "job build failed",
		Event: model.Event{
			Provider:  "gitlab",
			Kind:      model.EventPush,
			RepoURL:   "https://gitlab.example.com/acme/app.git",
			SHA:       testSHA,
			Ref:       "refs/heads/main",
			Branch:    "main",
			ProjectID: 15,
		},
	}
}

// newReporter points the GitLab API base at ts (via the WithGitLab instance URL)
// so posts land on the test server regardless of the run's RepoURL.
func newReporter(t *testing.T, ts *httptest.Server, opts ...Option) *Reporter {
	t.Helper()
	all := append([]Option{WithLogger(discardLogger()), WithGitLab("tok-secret", ts.URL)}, opts...)
	r, err := New(all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func recv(t *testing.T, ch <-chan captured) captured {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a status post")
		return captured{}
	}
}

func TestReportPostsStatus(t *testing.T) {
	reqs := make(chan captured, 1)
	ts := recordingServer(t, reqs)
	r := newReporter(t, ts, WithBaseURL("https://ci.example.com"))

	r.Report(gitlabRun(model.StatusFailed), model.StatusFailed)
	r.Close(2 * time.Second)

	got := recv(t, reqs)
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/api/v4/projects/15/statuses/"+testSHA {
		t.Errorf("path = %q", got.path)
	}
	if tok := got.header.Get("PRIVATE-TOKEN"); tok != "tok-secret" {
		t.Errorf("PRIVATE-TOKEN = %q, want tok-secret", tok)
	}
	if auth := got.header.Get("Authorization"); auth != "" {
		t.Errorf("Authorization = %q, want none (GitLab uses PRIVATE-TOKEN)", auth)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var b map[string]any
	if err := json.Unmarshal(got.body, &b); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if b["state"] != "failed" || b["context"] != "janus" {
		t.Errorf("body state/context wrong: %v", b)
	}
	if b["target_url"] != "https://ci.example.com/runs/run123" {
		t.Errorf("target_url = %v", b["target_url"])
	}
	if desc, _ := b["description"].(string); !strings.Contains(desc, "job build failed") {
		t.Errorf("description = %q, want the reason", desc)
	}
}

func TestReportStateMapping(t *testing.T) {
	tests := []struct {
		state model.Status
		want  string // "" = no post expected
	}{
		{model.StatusRunning, "running"},
		{model.StatusSuccess, "success"},
		{model.StatusFailed, "failed"},
		{model.StatusCancelled, "canceled"},
		{model.StatusSkipped, ""},
		{model.StatusPending, ""},
	}
	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			reqs := make(chan captured, 1)
			ts := recordingServer(t, reqs)
			r := newReporter(t, ts)

			run := gitlabRun(tc.state)
			r.Report(run, tc.state)
			r.Close(2 * time.Second)

			if tc.want == "" {
				if len(reqs) != 0 {
					t.Fatalf("posted for %s, want no post", tc.state)
				}
				return
			}
			got := recv(t, reqs)
			var b map[string]any
			_ = json.Unmarshal(got.body, &b)
			if b["state"] != tc.want {
				t.Errorf("state = %v, want %q", b["state"], tc.want)
			}
		})
	}
}

func TestReportScoping(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*model.Run)
		instance bool // pass WithInstanceURL(ts.URL)
		wantPost bool
	}{
		{"gitlab run posts", func(*model.Run) {}, true, true},
		{"non-gitlab provider", func(r *model.Run) { r.Event.Provider = "manual" }, true, false},
		{"zero project id", func(r *model.Run) { r.Event.ProjectID = 0 }, true, false},
		{"empty sha", func(r *model.Run) { r.Event.SHA = "" }, true, false},
		{"ssh repo, no instance url", func(r *model.Run) { r.Event.RepoURL = "git@gitlab.example.com:acme/app.git" }, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reqs := make(chan captured, 1)
			ts := recordingServer(t, reqs)
			instance := ""
			if tc.instance {
				instance = ts.URL
			}
			r, err := New(WithLogger(discardLogger()), WithGitLab("tok", instance))
			if err != nil {
				t.Fatal(err)
			}

			run := gitlabRun(model.StatusSuccess)
			tc.mutate(run)
			r.Report(run, model.StatusSuccess)
			r.Close(2 * time.Second)

			if got := len(reqs) > 0; got != tc.wantPost {
				t.Errorf("posted = %v, want %v", got, tc.wantPost)
			}
		})
	}
}

func TestReportDerivesBaseFromRepoURL(t *testing.T) {
	reqs := make(chan captured, 1)
	ts := recordingServer(t, reqs)
	// No instance URL: the API base must be derived from the run's http RepoURL.
	r, err := New(WithLogger(discardLogger()), WithGitLab("tok", ""))
	if err != nil {
		t.Fatal(err)
	}
	run := gitlabRun(model.StatusSuccess)
	run.Event.RepoURL = ts.URL + "/acme/app.git" // http URL pointing at the test server

	r.Report(run, model.StatusSuccess)
	r.Close(2 * time.Second)

	got := recv(t, reqs)
	if got.path != "/api/v4/projects/15/statuses/"+testSHA {
		t.Errorf("derived path = %q", got.path)
	}
}

func TestReportTargetURLOmittedWithoutBaseURL(t *testing.T) {
	reqs := make(chan captured, 1)
	ts := recordingServer(t, reqs)
	r := newReporter(t, ts) // no WithBaseURL

	r.Report(gitlabRun(model.StatusSuccess), model.StatusSuccess)
	r.Close(2 * time.Second)

	got := recv(t, reqs)
	var b map[string]any
	_ = json.Unmarshal(got.body, &b)
	if _, ok := b["target_url"]; ok {
		t.Errorf("target_url present without base_url: %v", b["target_url"])
	}
}

func TestReportErrorStatusNeverFailsOrPanics(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)
	r := newReporter(t, ts)
	r.Report(gitlabRun(model.StatusFailed), model.StatusFailed) // returns immediately
	r.Close(2 * time.Second)
}

func TestReportHangingEndpointDrainsWhenRequestTimesOut(t *testing.T) {
	block := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	t.Cleanup(func() { close(block); ts.Close() })
	r := newReporter(t, ts, WithTimeout(150*time.Millisecond))

	r.Report(gitlabRun(model.StatusFailed), model.StatusFailed)
	start := time.Now()
	r.Close(2 * time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Close took %v; expected the hung post to time out first", elapsed)
	}
}

func TestCloseTimeoutCancelsHungPost(t *testing.T) {
	block := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	t.Cleanup(func() { close(block); ts.Close() })
	r := newReporter(t, ts, WithTimeout(10*time.Second))

	r.Report(gitlabRun(model.StatusFailed), model.StatusFailed)
	start := time.Now()
	r.Close(200 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Close took %v; expected it to return near its 200ms timeout", elapsed)
	}
}

func TestReportDoesNotFollowRedirects(t *testing.T) {
	var followed int32
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&followed, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(secondary.Close)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, secondary.URL, http.StatusFound)
	}))
	t.Cleanup(primary.Close)
	r := newReporter(t, primary) // PRIVATE-TOKEN set; must not be forwarded onward

	r.Report(gitlabRun(model.StatusFailed), model.StatusFailed)
	r.Close(2 * time.Second)

	if got := atomic.LoadInt32(&followed); got != 0 {
		t.Errorf("followed a redirect (secondary hit %d times); the token could leak onward", got)
	}
}

func TestReportSuppliedClientStillNoRedirect(t *testing.T) {
	var followed int32
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&followed, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(secondary.Close)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, secondary.URL, http.StatusFound)
	}))
	t.Cleanup(primary.Close)
	// A caller-supplied client that set no CheckRedirect must still not forward
	// the PRIVATE-TOKEN across a redirect — New enforces the no-follow policy.
	r, err := New(
		WithGitLab("tok", primary.URL),
		WithLogger(discardLogger()),
		WithHTTPClient(&http.Client{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	r.Report(gitlabRun(model.StatusFailed), model.StatusFailed)
	r.Close(2 * time.Second)

	if got := atomic.LoadInt32(&followed); got != 0 {
		t.Errorf("supplied client followed a redirect (secondary hit %d times); protection must be enforced", got)
	}
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		base     string
		instance string
		wantErr  bool
		secret   string // must not appear in the error
	}{
		{"empty token", "", "", "", true, ""},
		{"ok", "tok", "https://ci.example.com", "https://gitlab.example.com", false, ""},
		{"base_url userinfo", "tok", "https://u:sekret@ci.example.com", "", true, "sekret"},
		{"gitlab_url query", "tok", "", "https://gitlab.example.com/?t=sekret", true, "sekret"},
		{"gitlab_url ftp", "tok", "", "ftp://nope", true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := []Option{WithLogger(discardLogger())}
			if tc.base != "" {
				opts = append(opts, WithBaseURL(tc.base))
			}
			opts = append(opts, WithGitLab(tc.token, tc.instance))
			_, err := New(opts...)
			if tc.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
			if tc.secret != "" && err != nil && strings.Contains(err.Error(), tc.secret) {
				t.Errorf("error leaks the URL value %q: %v", tc.secret, err)
			}
		})
	}
}

func TestReportOrdering(t *testing.T) {
	// The running post is slowed; if posts weren't serialized per commit, the
	// fast terminal could land first. The ordered worker guarantees running->terminal.
	var mu sync.Mutex
	var order []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var sb statusBody
		_ = json.Unmarshal(b, &sb)
		if sb.State == "running" {
			time.Sleep(100 * time.Millisecond)
		}
		mu.Lock()
		order = append(order, sb.State)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(ts.Close)
	r := newReporter(t, ts)

	run := gitlabRun(model.StatusSuccess)
	r.Report(run, model.StatusRunning)
	r.Report(run, model.StatusSuccess)
	r.Close(3 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "running" || order[1] != "success" {
		t.Fatalf("received order = %v, want [running success]", order)
	}
}

func TestReportIncludesRef(t *testing.T) {
	reqs := make(chan captured, 1)
	ts := recordingServer(t, reqs)
	r := newReporter(t, ts)

	run := gitlabRun(model.StatusSuccess)
	run.Event.Ref = "refs/heads/feature-x" // e.g. an MR source branch
	r.Report(run, model.StatusSuccess)
	r.Close(2 * time.Second)

	got := recv(t, reqs)
	var b statusBody
	_ = json.Unmarshal(got.body, &b)
	if b.Ref != "feature-x" {
		t.Errorf("ref = %q, want feature-x (refs/heads/ stripped)", b.Ref)
	}
}

func TestReportOmitsOverlongRef(t *testing.T) {
	// Event refs are accepted up to 512 bytes, but GitLab caps ref at 255; an
	// over-cap ref is omitted (not truncated to a different branch) so the status
	// still posts, keyed on the SHA.
	reqs := make(chan captured, 1)
	ts := recordingServer(t, reqs)
	r := newReporter(t, ts)

	run := gitlabRun(model.StatusSuccess)
	run.Event.Ref = "refs/heads/" + strings.Repeat("b", 300)
	r.Report(run, model.StatusSuccess)
	r.Close(2 * time.Second)

	got := recv(t, reqs)
	var b statusBody
	_ = json.Unmarshal(got.body, &b)
	if b.Ref != "" {
		t.Errorf("ref = %q, want it omitted for an over-255 branch name", b.Ref)
	}
	if b.State != "success" {
		t.Errorf("status should still post; state = %q", b.State)
	}
}

func TestReportKeepsMultibyteRefUnder255Chars(t *testing.T) {
	// GitLab's cap is 255 characters, not bytes: a 100-CJK-char branch is ~300
	// bytes but only 100 chars, so it must NOT be omitted (byte-counting would).
	branch := strings.Repeat("世", 100) // 100 runes, 300 bytes
	reqs := make(chan captured, 1)
	ts := recordingServer(t, reqs)
	r := newReporter(t, ts)

	run := gitlabRun(model.StatusSuccess)
	run.Event.Ref = "refs/heads/" + branch
	r.Report(run, model.StatusSuccess)
	r.Close(2 * time.Second)

	got := recv(t, reqs)
	var b statusBody
	_ = json.Unmarshal(got.body, &b)
	if b.Ref != branch {
		t.Errorf("ref = %q, want the 100-char multibyte branch kept", b.Ref)
	}
}

func TestReportClipsDescriptionByRunes(t *testing.T) {
	reqs := make(chan captured, 1)
	ts := recordingServer(t, reqs)
	r := newReporter(t, ts)

	run := gitlabRun(model.StatusFailed)
	run.Reason = strings.Repeat("あ", 1000) // multibyte, well over 255 chars
	r.Report(run, model.StatusFailed)
	r.Close(2 * time.Second)

	got := recv(t, reqs)
	var b statusBody
	_ = json.Unmarshal(got.body, &b)
	if n := utf8.RuneCountInString(b.Description); n > 255 {
		t.Errorf("description = %d chars, want <= 255", n)
	}
	if !utf8.ValidString(b.Description) {
		t.Errorf("description is not valid UTF-8 (a rune was split): %q", b.Description)
	}
}

func TestReportCleartextWarning(t *testing.T) {
	tests := []struct {
		name     string
		instance string
		wantWarn bool
	}{
		{"http warns", "http://gitlab.local", true},
		{"https no warning", "https://gitlab.example.com", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			var n int32
			r, err := New(
				WithGitLab("tok", tc.instance),
				WithLogger(logger),
				WithHTTPClient(&http.Client{Transport: okRT{&n}}),
			)
			if err != nil {
				t.Fatal(err)
			}
			r.Report(gitlabRun(model.StatusFailed), model.StatusFailed)
			r.Close(2 * time.Second)
			if warned := strings.Contains(buf.String(), "plaintext http"); warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v; log: %q", warned, tc.wantWarn, buf.String())
			}
		})
	}
}

func TestReportClipsDescriptionAndTargetURL(t *testing.T) {
	reqs := make(chan captured, 1)
	ts := recordingServer(t, reqs)
	// A base URL long enough that <base>/runs/<id> exceeds GitLab's 255 cap.
	longBase := "https://ci.example.com/" + strings.Repeat("a", 300)
	r, err := New(WithGitLab("tok", ts.URL), WithBaseURL(longBase), WithLogger(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	run := gitlabRun(model.StatusFailed)
	run.Reason = strings.Repeat("x", 1000) // long reason -> long description
	r.Report(run, model.StatusFailed)
	r.Close(2 * time.Second)

	got := recv(t, reqs)
	var b statusBody
	_ = json.Unmarshal(got.body, &b)
	if len(b.Description) > 255 {
		t.Errorf("description length = %d, want <= 255", len(b.Description))
	}
	if b.TargetURL != "" {
		t.Errorf("target_url = %q, want it omitted when it would exceed 255", b.TargetURL)
	}
}

func TestReport409Retried(t *testing.T) {
	var n int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(http.StatusConflict) // first attempt: 409
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(ts.Close)
	r := newReporter(t, ts)

	r.Report(gitlabRun(model.StatusFailed), model.StatusFailed)
	r.Close(3 * time.Second) // must exceed the retry backoff

	if got := atomic.LoadInt32(&n); got != 2 {
		t.Errorf("requests = %d, want 2 (409 retried once)", got)
	}
}

func TestReportBodyIsWellFormed(t *testing.T) {
	// Guard the exact struct shape GitLab expects.
	reqs := make(chan captured, 1)
	ts := recordingServer(t, reqs)
	r := newReporter(t, ts)
	r.Report(gitlabRun(model.StatusRunning), model.StatusRunning)
	r.Close(2 * time.Second)

	got := recv(t, reqs)
	var b statusBody
	if err := json.NewDecoder(bytes.NewReader(got.body)).Decode(&b); err != nil {
		t.Fatalf("body: %v", err)
	}
	if b.State != "running" || b.Context != "janus" {
		t.Errorf("body = %+v", b)
	}
}

package status

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hutusi/janus/internal/model"
)

// capturingRT records each request's URL and returns 201 without a network call,
// so a github.com run can be exercised without hitting the real API.
type capturingRT struct{ fn func(*url.URL) }

func (rt capturingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.fn(req.URL)
	return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
}

func githubRun(status model.Status) *model.Run {
	return &model.Run{
		ID:           "run123",
		WorkflowName: "ci",
		Status:       status,
		Reason:       "job build failed",
		Event: model.Event{
			Provider: "github",
			Kind:     model.EventPush,
			RepoURL:  "https://github.example.com/acme/app.git",
			SHA:      testSHA,
			Ref:      "refs/heads/main",
			Branch:   "main",
			RepoSlug: "acme/app",
		},
	}
}

// newGitHubReporter points the GHES web base at ts so posts land on the test
// server (ts host is not github.com, so the /api/v3 path is used).
func newGitHubReporter(t *testing.T, url string, opts ...Option) *Reporter {
	t.Helper()
	all := append([]Option{WithLogger(discardLogger()), WithGitHub("ghtok", url)}, opts...)
	r, err := New(all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestGitHubReportPostsStatus(t *testing.T) {
	reqs := make(chan captured, 1)
	ts := recordingServer(t, reqs)
	r := newGitHubReporter(t, ts.URL, WithBaseURL("https://ci.example.com"))

	r.Report(githubRun(model.StatusFailed), model.StatusFailed)
	r.Close(2 * time.Second)

	got := recv(t, reqs)
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/api/v3/repos/acme/app/statuses/"+testSHA {
		t.Errorf("path = %q", got.path)
	}
	if auth := got.header.Get("Authorization"); auth != "Bearer ghtok" {
		t.Errorf("Authorization = %q, want Bearer ghtok", auth)
	}
	if accept := got.header.Get("Accept"); accept != "application/vnd.github+json" {
		t.Errorf("Accept = %q", accept)
	}
	if tok := got.header.Get("PRIVATE-TOKEN"); tok != "" {
		t.Errorf("PRIVATE-TOKEN = %q, want none (GitHub uses Bearer)", tok)
	}
	var b map[string]any
	if err := json.Unmarshal(got.body, &b); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if b["state"] != "failure" || b["context"] != "janus" {
		t.Errorf("body state/context wrong: %v", b)
	}
	if _, ok := b["ref"]; ok {
		t.Errorf("body carries ref = %v, want it omitted for GitHub", b["ref"])
	}
	if b["target_url"] != "https://ci.example.com/runs/run123" {
		t.Errorf("target_url = %v", b["target_url"])
	}
}

func TestGitHubReportStateMapping(t *testing.T) {
	tests := []struct {
		state model.Status
		want  string // "" = no post expected
	}{
		{model.StatusRunning, "pending"},
		{model.StatusSuccess, "success"},
		{model.StatusFailed, "failure"},
		{model.StatusCancelled, "error"},
		{model.StatusSkipped, ""},
		{model.StatusPending, ""},
	}
	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			reqs := make(chan captured, 1)
			ts := recordingServer(t, reqs)
			r := newGitHubReporter(t, ts.URL)

			r.Report(githubRun(tc.state), tc.state)
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

// TestGitHubReportDerivesPublicAPIBase pins that a github.com clone URL routes to
// api.github.com (a different host, not github.com/api/v3). It can't hit the real
// API, so a capturing RoundTripper records the URL instead.
func TestGitHubReportDerivesPublicAPIBase(t *testing.T) {
	var got atomic.Pointer[url.URL]
	rt := capturingRT{fn: func(u *url.URL) { got.Store(u) }}
	r, err := New(
		WithLogger(discardLogger()),
		WithGitHub("ghtok", ""), // no GHES base: derive from the clone URL
		WithHTTPClient(&http.Client{Transport: rt}),
	)
	if err != nil {
		t.Fatal(err)
	}
	run := githubRun(model.StatusSuccess)
	run.Event.RepoURL = "https://github.com/acme/app.git"

	r.Report(run, model.StatusSuccess)
	r.Close(2 * time.Second)

	u := got.Load()
	if u == nil {
		t.Fatal("no request captured")
	}
	if want := "https://api.github.com/repos/acme/app/statuses/" + testSHA; u.String() != want {
		t.Errorf("endpoint = %q, want %q", u.String(), want)
	}
}

func TestGitHubReportSkips(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*model.Run)
		ghURL  string // WithGitHub base; "" derives from RepoURL
	}{
		{"empty slug", func(r *model.Run) { r.Event.RepoSlug = "" }, "x"},
		{"malformed slug", func(r *model.Run) { r.Event.RepoSlug = "noslash" }, "x"},
		{"nested slug", func(r *model.Run) { r.Event.RepoSlug = "acme/app/extra" }, "x"},
		{"traversal slug", func(r *model.Run) { r.Event.RepoSlug = "../evil" }, "x"},
		{"empty sha", func(r *model.Run) { r.Event.SHA = "" }, "x"},
		{"ssh repo, no github_url", func(r *model.Run) { r.Event.RepoURL = "git@github.com:acme/app.git" }, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reqs := make(chan captured, 1)
			ts := recordingServer(t, reqs)
			ghURL := tc.ghURL
			if ghURL == "x" {
				ghURL = ts.URL
			}
			r := newGitHubReporter(t, ghURL)

			run := githubRun(model.StatusSuccess)
			tc.mutate(run)
			r.Report(run, model.StatusSuccess)
			r.Close(2 * time.Second)

			if len(reqs) != 0 {
				t.Errorf("%s: posted, want no post", tc.name)
			}
		})
	}
}

// TestGitHubReportNoRetryOn409 contrasts with GitLab: GitHub never 409s, so a
// 409 must not trigger the extra retry.
func TestGitHubReportNoRetryOn409(t *testing.T) {
	var n int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusConflict)
	}))
	t.Cleanup(ts.Close)
	r := newGitHubReporter(t, ts.URL)

	r.Report(githubRun(model.StatusFailed), model.StatusFailed)
	r.Close(2 * time.Second)

	if got := atomic.LoadInt32(&n); got != 1 {
		t.Errorf("requests = %d, want 1 (no 409 retry for GitHub)", got)
	}
}

func TestGitHubReportOrdering(t *testing.T) {
	// The pending post is slowed; if posts weren't serialized per commit, the
	// fast terminal could land first. The ordered worker guarantees pending->terminal.
	var mu sync.Mutex
	var order []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var sb statusBody
		_ = json.Unmarshal(b, &sb)
		if sb.State == "pending" {
			time.Sleep(100 * time.Millisecond)
		}
		mu.Lock()
		order = append(order, sb.State)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(ts.Close)
	r := newGitHubReporter(t, ts.URL)

	run := githubRun(model.StatusSuccess)
	r.Report(run, model.StatusRunning) // -> pending
	r.Report(run, model.StatusSuccess)
	r.Close(3 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "pending" || order[1] != "success" {
		t.Fatalf("received order = %v, want [pending success]", order)
	}
}

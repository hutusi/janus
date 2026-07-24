package notify

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hutusi/janus/internal/model"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// captured is one request a recordingServer received.
type captured struct {
	method string
	header http.Header
	body   []byte
}

// recordingServer sends each request it receives to reqs (buffered), then 200s.
func recordingServer(t *testing.T, reqs chan<- captured) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqs <- captured{method: r.Method, header: r.Header.Clone(), body: b}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func newNotifier(t *testing.T, targets []Target, opts ...Option) *Notifier {
	t.Helper()
	n, err := New(targets, append([]Option{WithLogger(discardLogger())}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return n
}

// failedRun is a fully-executed run that finished failed, with credentials in
// its repo URL so redaction can be checked. Reason is left empty — an ordinary
// job failure carries none, so the payload must derive one from the failed jobs.
func failedRun() *model.Run {
	start := time.Now().Add(-5 * time.Second)
	return &model.Run{
		ID:           "abc123",
		WorkflowName: "ci",
		Status:       model.StatusFailed,
		Event: model.Event{
			Provider: "gitlab",
			Kind:     model.EventPush,
			RepoURL:  "https://user:tok@git.example.com/acme/app.git",
			Branch:   "main",
			Ref:      "refs/heads/main",
			SHA:      "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			Title:    "fix the thing",
		},
		CreatedAt:  start.Add(-time.Second),
		StartedAt:  start,
		FinishedAt: start.Add(5 * time.Second),
		Jobs: []*model.JobRun{
			{Name: "build", Status: model.StatusFailed},
			{Name: "test", Status: model.StatusSkipped},
		},
	}
}

func recv(t *testing.T, ch <-chan captured) captured {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a delivery")
		return captured{}
	}
}

func TestNotifyPostsPayload(t *testing.T) {
	reqs := make(chan captured, 1)
	ts := recordingServer(t, reqs)
	n := newNotifier(t, []Target{{URL: ts.URL, On: []string{"failed"}, Secret: "topsecret"}}, WithBaseURL("https://ci.example.com/"))

	n.Notify(failedRun())
	n.Close(2 * time.Second) // drains, so the delivery is guaranteed done

	got := recv(t, reqs)
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if auth := got.header.Get("Authorization"); auth != "Bearer topsecret" {
		t.Errorf("Authorization = %q, want Bearer topsecret", auth)
	}

	var p map[string]any
	if err := json.Unmarshal(got.body, &p); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if p["run_id"] != "abc123" || p["workflow"] != "ci" || p["status"] != "failed" {
		t.Errorf("core fields wrong: %v", p)
	}
	if p["reason"] != "failed jobs: build" {
		t.Errorf("reason = %v, want derived \"failed jobs: build\"", p["reason"])
	}
	if repo, _ := p["repo_url"].(string); strings.Contains(repo, "tok@") || strings.Contains(repo, "user:") {
		t.Errorf("repo_url not redacted: %q", repo)
	}
	if p["url"] != "https://ci.example.com/runs/abc123" {
		t.Errorf("url = %v, want the run link", p["url"])
	}
	if _, ok := p["duration_seconds"]; !ok {
		t.Error("duration_seconds missing for an executed run")
	}
	if p["jobs_total"].(float64) != 2 || p["jobs_failed"].(float64) != 1 {
		t.Errorf("job counts wrong: total=%v failed=%v", p["jobs_total"], p["jobs_failed"])
	}
}

func TestNotifyWithoutSecretHasNoAuthHeaderOrLink(t *testing.T) {
	reqs := make(chan captured, 1)
	ts := recordingServer(t, reqs)
	n := newNotifier(t, []Target{{URL: ts.URL, On: []string{"failed"}}}) // no secret, no base URL

	n.Notify(failedRun())
	n.Close(2 * time.Second)

	got := recv(t, reqs)
	if auth := got.header.Get("Authorization"); auth != "" {
		t.Errorf("Authorization = %q, want none", auth)
	}
	var p map[string]any
	if err := json.Unmarshal(got.body, &p); err != nil {
		t.Fatal(err)
	}
	if _, ok := p["url"]; ok {
		t.Error("url should be omitted without a base_url")
	}
}

func TestNotifyRedactsReason(t *testing.T) {
	// Defense-in-depth: even if a credential-bearing reason reaches the notifier,
	// the outbound payload must not carry it.
	reqs := make(chan captured, 1)
	ts := recordingServer(t, reqs)
	n := newNotifier(t, []Target{{URL: ts.URL, On: []string{"failed"}}})

	run := failedRun()
	run.Reason = "checkout: fatal: unable to access https://user:s3cr3t@host/repo.git"
	n.Notify(run)
	n.Close(2 * time.Second)

	got := recv(t, reqs)
	var p map[string]any
	if err := json.Unmarshal(got.body, &p); err != nil {
		t.Fatal(err)
	}
	if r, _ := p["reason"].(string); strings.Contains(r, "s3cr3t") || strings.Contains(r, "user:") {
		t.Errorf("payload reason still carries credentials: %q", r)
	}
}

func TestNotifyPreExecutionRunOmitsDuration(t *testing.T) {
	reqs := make(chan captured, 1)
	ts := recordingServer(t, reqs)
	n := newNotifier(t, []Target{{URL: ts.URL, On: []string{"failed"}}})

	run := failedRun()
	run.StartedAt = time.Time{} // e.g. a checkout failure — never started executing
	n.Notify(run)
	n.Close(2 * time.Second)

	got := recv(t, reqs)
	var p map[string]any
	if err := json.Unmarshal(got.body, &p); err != nil {
		t.Fatal(err)
	}
	if _, ok := p["duration_seconds"]; ok {
		t.Error("duration_seconds should be omitted when StartedAt is zero")
	}
	if _, ok := p["started_at"]; ok {
		t.Error("started_at should be omitted when zero")
	}
}

func TestNotifyEventFilter(t *testing.T) {
	tests := []struct {
		name   string
		on     []string
		status model.Status
		want   int
	}{
		{"failed-only delivers on failed", []string{"failed"}, model.StatusFailed, 1},
		{"failed-only skips success", []string{"failed"}, model.StatusSuccess, 0},
		{"failed-only skips cancelled", []string{"failed"}, model.StatusCancelled, 0},
		{"default (nil) is failures only", nil, model.StatusFailed, 1},
		{"default (nil) skips success", nil, model.StatusSuccess, 0},
		{"multi delivers on success", []string{"success", "failed"}, model.StatusSuccess, 1},
		{"multi delivers on failed", []string{"success", "failed"}, model.StatusFailed, 1},
		{"multi skips skipped", []string{"success", "failed"}, model.StatusSkipped, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reqs := make(chan captured, 4)
			ts := recordingServer(t, reqs)
			n := newNotifier(t, []Target{{URL: ts.URL, On: tc.on}})

			run := failedRun()
			run.Status = tc.status
			n.Notify(run)
			n.Close(2 * time.Second)

			if got := len(reqs); got != tc.want {
				t.Errorf("delivered %d, want %d", got, tc.want)
			}
		})
	}
}

func TestNotifyMultipleTargets(t *testing.T) {
	reqs := make(chan captured, 2)
	ts1 := recordingServer(t, reqs)
	ts2 := recordingServer(t, reqs)
	n := newNotifier(t, []Target{
		{URL: ts1.URL, On: []string{"failed"}},
		{URL: ts2.URL, On: []string{"failed"}},
	})

	n.Notify(failedRun())
	n.Close(2 * time.Second)

	if got := len(reqs); got != 2 {
		t.Errorf("delivered %d, want 2 (one per target)", got)
	}
}

func TestNotifyErrorStatusNeverFailsOrPanics(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)
	n := newNotifier(t, []Target{{URL: ts.URL, On: []string{"failed"}}})

	n.Notify(failedRun()) // returns immediately; a 500 is logged, not propagated
	n.Close(2 * time.Second)
}

func TestNotifyHangingEndpointDrainsWhenRequestTimesOut(t *testing.T) {
	block := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	t.Cleanup(func() { close(block); ts.Close() })
	// A short per-delivery timeout: the hung POST errors quickly, so drain
	// completes well before Close's own 2s budget.
	n := newNotifier(t, []Target{{URL: ts.URL, On: []string{"failed"}}}, WithTimeout(150*time.Millisecond))

	n.Notify(failedRun())
	start := time.Now()
	n.Close(2 * time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Close took %v; expected it to finish once the request timed out", elapsed)
	}
}

func TestCloseDrainsInFlightDelivery(t *testing.T) {
	release := make(chan struct{})
	landed := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		landed <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	n := newNotifier(t, []Target{{URL: ts.URL, On: []string{"failed"}}})

	n.Notify(failedRun())
	closed := make(chan struct{})
	go func() { n.Close(3 * time.Second); close(closed) }()

	// Close must not return while the delivery is still in flight.
	select {
	case <-closed:
		t.Fatal("Close returned before the in-flight delivery finished")
	case <-time.After(100 * time.Millisecond):
	}

	close(release) // let the delivery complete
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after the delivery finished")
	}
	select {
	case <-landed:
	default:
		t.Error("delivery never reached the server")
	}
}

func TestCloseTimeoutCancelsHungDelivery(t *testing.T) {
	block := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	t.Cleanup(func() { close(block); ts.Close() })
	// A long per-delivery timeout, so only Close's own timeout (+ ctx cancel)
	// can end the hung request.
	n := newNotifier(t, []Target{{URL: ts.URL, On: []string{"failed"}}}, WithTimeout(10*time.Second))

	n.Notify(failedRun())
	start := time.Now()
	n.Close(200 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Close took %v; expected it to return near its 200ms timeout", elapsed)
	}
}

func TestNotifyRunLinkJoinsBaseURLPath(t *testing.T) {
	reqs := make(chan captured, 1)
	ts := recordingServer(t, reqs)
	n := newNotifier(t, []Target{{URL: ts.URL, On: []string{"failed"}}}, WithBaseURL("https://ci.example.com/base"))

	n.Notify(failedRun())
	n.Close(2 * time.Second)

	got := recv(t, reqs)
	var p map[string]any
	if err := json.Unmarshal(got.body, &p); err != nil {
		t.Fatal(err)
	}
	if p["url"] != "https://ci.example.com/base/runs/abc123" {
		t.Errorf("url = %v, want a structural join under /base", p["url"])
	}
}

func TestNotifyDropsWhenSaturated(t *testing.T) {
	release := make(chan struct{})
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	// Cap of one: the first delivery takes the only slot (acquired synchronously
	// in Notify) and blocks in the handler, so the second must be dropped.
	n := newNotifier(t, []Target{{URL: ts.URL, On: []string{"failed"}}}, WithMaxInFlight(1))

	n.Notify(failedRun()) // occupies the single slot
	n.Notify(failedRun()) // slot full → dropped, non-blocking, no panic

	close(release) // let the first delivery complete
	n.Close(2 * time.Second)

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("server received %d deliveries, want 1 (the second must be dropped)", got)
	}
}

func TestValidationErrorsHideURLValues(t *testing.T) {
	// A malformed URL is a startup error printed to stderr; its value may embed a
	// secret (userinfo, or a token in the path/query), so the error must never
	// echo it — only the key/index and reason.
	tests := []struct {
		name    string
		targets []Target
		base    string
		secret  string // substring that must NOT appear in the error
	}{
		{"target scheme with userinfo", []Target{{URL: "ftp://user:pw@host/x"}}, "", "pw"},
		{"target token in query", []Target{{URL: "ftp://host/x?token=sekret"}}, "", "sekret"},
		{"base_url userinfo", nil, "https://user:sekret@host", "sekret"},
		{"base_url token in query", nil, "https://host/?token=sekret", "sekret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := []Option{WithLogger(discardLogger())}
			if tc.base != "" {
				opts = append(opts, WithBaseURL(tc.base))
			}
			_, err := New(tc.targets, opts...)
			if err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			if strings.Contains(err.Error(), tc.secret) {
				t.Errorf("error leaks the URL value %q: %v", tc.secret, err)
			}
		})
	}
}

func TestNotifyPerTargetCapacityDeliversAllTargets(t *testing.T) {
	// Per-target capacity: even with a cap of one, two distinct targets each get
	// their own slot, so a single run delivers to both. (A shared/global cap of
	// one could drop the second.)
	reqsA := make(chan captured, 2)
	reqsB := make(chan captured, 2)
	tsA := recordingServer(t, reqsA)
	tsB := recordingServer(t, reqsB)
	n := newNotifier(t, []Target{
		{URL: tsA.URL, On: []string{"failed"}},
		{URL: tsB.URL, On: []string{"failed"}},
	}, WithMaxInFlight(1))

	n.Notify(failedRun())
	n.Close(2 * time.Second)

	if len(reqsA) != 1 || len(reqsB) != 1 {
		t.Fatalf("deliveries A=%d B=%d, want 1 each (per-target capacity)", len(reqsA), len(reqsB))
	}
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name    string
		targets []Target
		base    string
		wantErr bool
	}{
		{"valid multi-status", []Target{{URL: "https://x/y", On: []string{"failed", "success"}}}, "", false},
		{"valid default on", []Target{{URL: "http://x/y"}}, "", false},
		{"empty url", []Target{{URL: ""}}, "", true},
		{"non-http url", []Target{{URL: "ftp://x/y"}}, "", true},
		{"url without host", []Target{{URL: "http://"}}, "", true},
		{"unknown on value", []Target{{URL: "https://x/y", On: []string{"running"}}}, "", true},
		{"pending is not notifiable", []Target{{URL: "https://x/y", On: []string{"pending"}}}, "", true},
		// Targets are lenient: a webhook URL legitimately carries a path, query, or basic-auth.
		{"target may carry a path and query", []Target{{URL: "https://x/y?token=abc"}}, "", false},
		{"target may carry userinfo", []Target{{URL: "https://u:p@x/y"}}, "", false},
		{"bad base_url scheme", nil, "ftp://nope", true},
		{"good base_url", nil, "https://ci.example.com", false},
		{"base_url with path ok", nil, "https://ci.example.com/base", false},
		// base_url is strict: it is copied into every payload and joined with a path.
		{"base_url with userinfo rejected", nil, "https://u:p@ci.example.com", true},
		{"base_url with query rejected", nil, "https://ci.example.com/?x=1", true},
		{"base_url with fragment rejected", nil, "https://ci.example.com/#f", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := []Option{WithLogger(discardLogger())}
			if tc.base != "" {
				opts = append(opts, WithBaseURL(tc.base))
			}
			_, err := New(tc.targets, opts...)
			if tc.wantErr && err == nil {
				t.Error("New should have returned an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("New returned %v, want nil", err)
			}
		})
	}
}

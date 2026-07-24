// Package status reports a run's lifecycle state to a git host's commit-status
// API, so pass/fail shows natively on the triggering commit or merge request. It
// is Janus's second outbound HTTP user, after internal/notify, and copies that
// package's safety posture: the reporter owns its own context (never the run's),
// a slow/failing/hung host is logged and never fails or blocks a run, redirects
// are not followed (so the API token can't ride a downgrade), and Close drains
// in-flight posts at shutdown. Posts for one commit are serialized (running
// before terminal) so the host never settles on the wrong state, and are
// best-effort by design.
//
// One Reporter fans out across providers: each host's endpoint, auth, state
// vocabulary, and body shape live behind a `dialect`, and Report dispatches by
// the run's provider. The worker pool, ordering, drain, and no-redirect policy
// are shared — a dialect only renders a run+state into a ready-to-send post.
package status

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/hutusi/janus/internal/model"
)

const (
	// defaultTimeout bounds a single status POST.
	defaultTimeout = 10 * time.Second
	// defaultWorkers is the number of ordered worker goroutines. Posts for one
	// commit hash to the same worker, so they run FIFO (running before terminal);
	// unrelated commits spread across workers.
	defaultWorkers = 8
	// workerQueueLen buffers each worker before posts are dropped.
	workerQueueLen = 64
	// statusContext is the status "context" (its grouping key): one row per commit
	// whose lifecycle updates running -> terminal.
	statusContext = "janus"
	// retryBackoff delays the single retry after a host's 409 (concurrent update).
	retryBackoff = 500 * time.Millisecond
)

// dialect renders a run and state into a provider-specific status post, or
// ok=false to skip: a state the host has no row for, an event missing the host's
// addressing fields, or an unresolvable API base. targetURL is the shared
// run-page link (empty when no base_url); each dialect applies its own limits.
type dialect interface {
	build(run *model.Run, state model.Status, targetURL string) (postJob, bool)
}

// Reporter posts commit statuses. Construct it with New; it is safe for
// concurrent use by multiple run goroutines.
type Reporter struct {
	client     *http.Client
	logger     *slog.Logger
	baseRaw    string   // WithBaseURL input, parsed into baseURL by New
	baseURL    *url.URL // public base for run-page links; nil = no link
	timeout    time.Duration
	numWorkers int

	// Per-provider credentials, captured by the With<Provider> options and
	// resolved into dialects by New. A provider with no dialect is simply not
	// reported (Report no-ops for it).
	glToken, glURLRaw string
	dialects          map[string]dialect

	// workers is a fixed pool of ordered FIFO channels; a post is routed to
	// workers[shard(key)], so all posts for one (provider, repo, sha, context) run
	// sequentially through the same worker — running always precedes the terminal,
	// and we never post to one commit concurrently (which some hosts 409).
	workers    []chan postJob
	mu         sync.Mutex // guards closed and sends on the worker channels
	closed     bool
	warnedHTTP sync.Once // warn at most once about cleartext-http posts

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup // worker goroutines
}

// postJob is one enqueued status POST, fully rendered by a dialect.
type postJob struct {
	key      string            // routing key: same key -> same FIFO worker
	endpoint string            // fully-formed status-API URL
	body     []byte            // JSON payload
	header   map[string]string // auth (+ any host header); Content-Type added by post
	runID    string
	label    string
	overHTTP bool // endpoint is plaintext http -> warn once
	retry409 bool // retry a single time on 409 (GitLab; GitHub never 409s)
}

// Option configures a Reporter.
type Option func(*Reporter)

// WithLogger sets the logger (defaults to slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(r *Reporter) {
		if l != nil {
			r.logger = l
		}
	}
}

// WithBaseURL sets the daemon's public base URL; when non-empty, statuses carry
// a run-page link (<base_url>/runs/<id>). Validated by New.
func WithBaseURL(u string) Option { return func(r *Reporter) { r.baseRaw = u } }

// WithGitLab enables GitLab commit-status reporting with an outbound API token
// (personal/project access token, `api` scope) and an optional instance base URL
// (e.g. https://gitlab.example.com). The instance URL overrides deriving the API
// base from the event's clone URL — required for clone_url "ssh" and self-hosted
// instances on a subpath. Both validated by New.
func WithGitLab(token, instanceURL string) Option {
	return func(r *Reporter) { r.glToken, r.glURLRaw = token, instanceURL }
}

// WithHTTPClient overrides the HTTP client (mainly for tests). Its own Timeout
// governs; WithTimeout is ignored.
func WithHTTPClient(c *http.Client) Option {
	return func(r *Reporter) {
		if c != nil {
			r.client = c
		}
	}
}

// WithTimeout sets the per-post timeout for the default client.
func WithTimeout(d time.Duration) Option {
	return func(r *Reporter) {
		if d > 0 {
			r.timeout = d
		}
	}
}

// WithWorkers sets the number of ordered worker goroutines (defaults to
// defaultWorkers).
func WithWorkers(n int) Option {
	return func(r *Reporter) {
		if n > 0 {
			r.numWorkers = n
		}
	}
}

// New validates the configured providers and any URLs and returns a Reporter. It
// rejects a configuration with no provider, an empty token, and a malformed or
// credential-bearing base_url / instance URL — surfaced by the caller as a
// startup error, like allowlist.New.
func New(opts ...Option) (*Reporter, error) {
	r := &Reporter{logger: slog.Default(), timeout: defaultTimeout, numWorkers: defaultWorkers, dialects: make(map[string]dialect)}
	for _, opt := range opts {
		opt(r)
	}
	if r.logger == nil {
		r.logger = slog.Default()
	}
	if r.baseRaw != "" {
		u, err := validateBaseURL(r.baseRaw)
		if err != nil {
			return nil, fmt.Errorf("base_url: %w", err)
		}
		r.baseURL = u
	}
	if r.glToken != "" || r.glURLRaw != "" {
		d, err := newGitLabDialect(r.glToken, r.glURLRaw, r.logger)
		if err != nil {
			return nil, err
		}
		r.dialects["gitlab"] = d
	}
	if len(r.dialects) == 0 {
		return nil, errors.New("no commit-status provider configured")
	}
	if r.client == nil {
		r.client = &http.Client{Timeout: r.timeout}
	}
	// Never follow redirects, regardless of client source: the API token must not
	// be forwarded to a redirect target (e.g. an https->http downgrade). Enforced
	// even on a WithHTTPClient-supplied client so that knob can't silently drop the
	// protection; a client that set its own policy keeps it.
	if r.client.CheckRedirect == nil {
		r.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.workers = make([]chan postJob, r.numWorkers)
	r.wg.Add(r.numWorkers)
	for i := range r.workers {
		r.workers[i] = make(chan postJob, workerQueueLen)
		go r.worker(r.workers[i])
	}
	return r, nil
}

// Report posts run's current lifecycle state to its provider's commit-status
// API. It returns as soon as the post is enqueued, so it never blocks or fails
// the caller. state is the run's current status (StatusRunning for the
// pre-execution post, or the terminal status); the run's provider selects the
// dialect, which maps the state and no-ops for states/events it does not report.
func (r *Reporter) Report(run *model.Run, state model.Status) {
	d := r.dialects[run.Event.Provider]
	if d == nil {
		return // no reporter configured for this provider (or a manual trigger)
	}
	job, ok := d.build(run, state, r.runURL(run.ID))
	if !ok {
		return
	}
	if !r.enqueue(job) {
		r.logger.Warn("commit status dropped: worker queue full", "run_id", run.ID, "target", job.label)
	}
}

// enqueue routes a job to its key's ordered worker, non-blocking. It reports
// false if the queue is full or the reporter is closed.
func (r *Reporter) enqueue(j postJob) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	ch := r.workers[shard(j.key, len(r.workers))]
	select {
	case ch <- j:
		return true
	default:
		return false
	}
}

func (r *Reporter) worker(ch <-chan postJob) {
	defer r.wg.Done()
	for j := range ch {
		r.deliver(j)
	}
}

func (r *Reporter) deliver(j postJob) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("commit status post panicked", "run_id", j.runID, "target", j.label, "panic", rec)
		}
	}()
	if j.overHTTP {
		r.warnedHTTP.Do(func() {
			r.logger.Warn("posting commit status over plaintext http; the API token is sent unencrypted — use https")
		})
	}
	// Some hosts (GitLab) return 409 on a concurrent update to the same commit
	// status and document it as retryable; try once more after a short backoff.
	if code := r.post(j); j.retry409 && code == http.StatusConflict {
		select {
		case <-time.After(retryBackoff):
			r.post(j)
		case <-r.ctx.Done():
		}
	}
}

// post issues one POST and returns the HTTP status code (0 on a transport error
// or a request that couldn't be built).
func (r *Reporter) post(j postJob) int {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodPost, j.endpoint, bytes.NewReader(j.body))
	if err != nil {
		r.logger.Warn("commit status request could not be built", "run_id", j.runID, "target", j.label, "err", err)
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range j.header {
		req.Header.Set(k, v)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		r.logger.Warn("commit status post failed", "run_id", j.runID, "target", j.label, "err", safeError(err))
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		r.logger.Warn("commit status endpoint returned an error status", "run_id", j.runID, "target", j.label, "status", resp.StatusCode)
	}
	return resp.StatusCode
}

// Close stops accepting posts, drains those in flight up to timeout, then
// cancels any that remain. Call it after the runner has shut down.
func (r *Reporter) Close(timeout time.Duration) {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		for _, ch := range r.workers {
			close(ch)
		}
	}
	r.mu.Unlock()

	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
	r.cancel()
}

// runURL is the base-joined run-page link a dialect may use for a status's
// target_url, or "" when no base_url is set. Length limits are the dialect's
// concern (they vary per host).
func (r *Reporter) runURL(runID string) string {
	if r.baseURL == nil {
		return ""
	}
	return r.baseURL.JoinPath("runs", runID).String()
}

// statusBody is the JSON payload. GitLab sets every field; GitHub leaves Ref
// empty (omitempty drops it). Both use the "state/context" pair.
type statusBody struct {
	State       string `json:"state"`
	Context     string `json:"context"`
	Ref         string `json:"ref,omitempty"`
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description,omitempty"`
}

// description is the human-facing status line, shared by all dialects. It keys
// off the reported state (not run.Status, which for the pre-execution "running"
// post is still pending).
func description(run *model.Run, state model.Status) string {
	wf := run.WorkflowName
	if wf == "" {
		wf = statusContext
	}
	switch state {
	case model.StatusRunning:
		return wf + " running"
	case model.StatusSuccess:
		return wf + " passed"
	case model.StatusFailed, model.StatusCancelled:
		verb := "failed"
		if state == model.StatusCancelled {
			verb = "cancelled"
		}
		if run.Reason != "" {
			return wf + ": " + model.RedactURL(run.Reason)
		}
		return wf + " " + verb
	}
	return wf
}

// clip limits s to max characters (code points), so it never over-clips a
// multibyte string nor splits a rune into invalid UTF-8.
func clip(s string, max int) string {
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

// shard maps a key to a worker index. Same key -> same worker -> FIFO order.
func shard(key string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(n)) //nolint:gosec // n is small and positive
}

// deriveBase returns scheme://host of an http/https clone URL (dropping any
// userinfo and path), or nil when the URL is not http/https (e.g. an scp-style
// ssh remote), where an HTTPS API base cannot be inferred.
func deriveBase(repoURL string) *url.URL {
	u, err := url.Parse(repoURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}
}

// validateBaseURL requires an http/https URL with a host and no userinfo, query,
// or fragment (it is joined with a path and, for base_url, copied into the
// run-page link). Errors are value-free: the value may reach a startup log.
func validateBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("must be an http:// or https:// URL")
	}
	if u.Host == "" {
		return nil, errors.New("must include a host")
	}
	if u.User != nil {
		return nil, errors.New("must not contain userinfo")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("must not contain a query or fragment")
	}
	return u, nil
}

// safeError renders err for logging without leaking the request URL (Go wraps
// transport failures in *url.Error whose URL field holds the full endpoint).
func safeError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Err != nil {
			return ue.Op + ": " + ue.Err.Error()
		}
		return ue.Op
	}
	return err.Error()
}

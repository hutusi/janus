// Package status reports a run's lifecycle state to GitLab's Commit Status API
// (POST /api/v4/projects/{id}/statuses/{sha}), so pass/fail shows natively on
// the triggering commit or merge request. It is Janus's second outbound HTTP
// user, after internal/notify, and copies that package's safety posture: the
// reporter owns its own context (never the run's), a slow/failing/hung GitLab is
// logged and never fails or blocks a run, redirects are not followed (so the
// PRIVATE-TOKEN can't ride a downgrade), and Close drains in-flight posts at
// shutdown. Posts for one commit are serialized (running before terminal) so
// GitLab never settles on the wrong state, and are best-effort by design.
package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	// statusContext is the GitLab status "context" (its grouping key): one row
	// per commit whose lifecycle updates running -> terminal.
	statusContext = "janus"
	// GitLab caps description, target_url, and ref at 255 characters; an
	// over-limit field is rejected and the whole status is lost.
	maxDescLen      = 255
	maxTargetURLLen = 255
	maxRefLen       = 255
	// retryBackoff delays the single retry after GitLab's 409 (concurrent update).
	retryBackoff = 500 * time.Millisecond
)

// Reporter posts commit statuses to GitLab. Construct it with New; it is safe
// for concurrent use by multiple run goroutines.
type Reporter struct {
	token       string
	client      *http.Client
	logger      *slog.Logger
	baseRaw     string   // WithBaseURL input, parsed into baseURL by New
	instanceRaw string   // WithInstanceURL input, parsed into instanceURL by New
	baseURL     *url.URL // public base for target_url links; nil = no link
	instanceURL *url.URL // GitLab instance base override; nil = derive per event
	timeout     time.Duration
	numWorkers  int

	// workers is a fixed pool of ordered FIFO channels; a post is routed to
	// workers[fnv(key)%N], so all posts for one (project, sha, context) run
	// sequentially through the same worker — running always precedes the
	// terminal, and we never post to one commit concurrently (which GitLab 409s).
	workers    []chan postJob
	mu         sync.Mutex // guards closed and sends on the worker channels
	closed     bool
	warnedHTTP sync.Once // warn at most once about cleartext-http posts

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup // worker goroutines
}

// postJob is one enqueued status POST.
type postJob struct {
	endpoint string
	body     []byte
	runID    string
	label    string
	overHTTP bool
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
// a target_url link to the run page (<base_url>/runs/<id>). Validated by New.
func WithBaseURL(u string) Option { return func(r *Reporter) { r.baseRaw = u } }

// WithInstanceURL sets the GitLab instance base URL (e.g. https://gitlab.example.com).
// When set it overrides deriving the API base from the event's clone URL —
// required for clone_url "ssh" (an scp-style URL has no derivable HTTPS
// authority) and for self-hosted instances on a subpath. Validated by New.
func WithInstanceURL(u string) Option { return func(r *Reporter) { r.instanceRaw = u } }

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

// New validates the token and any URLs and returns a Reporter. It rejects an
// empty token and a malformed or credential-bearing base_url/gitlab_url —
// surfaced by the caller as a startup error, like allowlist.New.
func New(token string, opts ...Option) (*Reporter, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("gitlab api token is empty")
	}
	r := &Reporter{token: token, logger: slog.Default(), timeout: defaultTimeout, numWorkers: defaultWorkers}
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
	if r.instanceRaw != "" {
		u, err := validateBaseURL(r.instanceRaw)
		if err != nil {
			return nil, fmt.Errorf("gitlab_url: %w", err)
		}
		r.instanceURL = u
	}
	if r.client == nil {
		r.client = &http.Client{
			Timeout: r.timeout,
			// Never follow redirects: the PRIVATE-TOKEN header must not be
			// forwarded to a redirect target (e.g. an https->http downgrade).
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
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

// Report posts run's current lifecycle state to GitLab's commit-status API. It
// returns as soon as the post is enqueued, so it never blocks or fails the
// caller. state is the run's current status (StatusRunning for the
// pre-execution post, or the terminal status); the reporter maps it and no-ops
// for states or events it does not report (pending, skipped, non-GitLab events,
// events without a project id / sha, or an unresolvable API base).
func (r *Reporter) Report(run *model.Run, state model.Status) {
	glState := gitlabState(state)
	if glState == "" {
		return
	}
	ev := run.Event
	if ev.Provider != "gitlab" || ev.ProjectID <= 0 || ev.SHA == "" {
		return
	}
	base := r.instanceURL
	if base == nil {
		base = deriveBase(ev.RepoURL)
	}
	if base == nil {
		r.logger.Debug("commit status skipped: no resolvable GitLab API base", "run_id", run.ID, "project", ev.ProjectID)
		return
	}
	endpoint := base.JoinPath("api", "v4", "projects", strconv.FormatInt(ev.ProjectID, 10), "statuses", ev.SHA).String()

	body, err := json.Marshal(statusBody{
		State:       glState,
		Context:     statusContext,
		Ref:         refName(ev.Ref),
		TargetURL:   r.targetURL(run.ID),
		Description: clip(description(run, state), maxDescLen),
	})
	if err != nil {
		r.logger.Warn("commit status payload could not be encoded", "run_id", run.ID, "err", err)
		return
	}

	label := "gitlab project " + strconv.FormatInt(ev.ProjectID, 10)
	key := label + "|" + ev.SHA + "|" + statusContext
	job := postJob{endpoint: endpoint, body: body, runID: run.ID, label: label, overHTTP: base.Scheme == "http"}
	if !r.enqueue(key, job) {
		r.logger.Warn("commit status dropped: worker queue full", "run_id", run.ID, "target", label)
	}
}

// enqueue routes a job to its key's ordered worker, non-blocking. It reports
// false if the queue is full or the reporter is closed.
func (r *Reporter) enqueue(key string, j postJob) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	ch := r.workers[shard(key, len(r.workers))]
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
			r.logger.Warn("posting commit status over plaintext http; the PRIVATE-TOKEN is sent unencrypted — use https")
		})
	}
	// GitLab returns 409 on a concurrent update to the same commit status and
	// documents it as retryable; try once more after a short backoff.
	if r.post(j) == http.StatusConflict {
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
	req.Header.Set("PRIVATE-TOKEN", r.token)
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

func (r *Reporter) targetURL(runID string) string {
	if r.baseURL == nil {
		return ""
	}
	u := r.baseURL.JoinPath("runs", runID).String()
	if len(u) > maxTargetURLLen {
		return "" // GitLab rejects an over-length target_url; drop it rather than lose the status
	}
	return u
}

type statusBody struct {
	State       string `json:"state"`
	Context     string `json:"context"`
	Ref         string `json:"ref,omitempty"`
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description,omitempty"`
}

// refName strips the refs/heads/ prefix so the status carries the branch name
// GitLab associates the pipeline with — the pushed branch, or an MR's source
// branch (parseGitLabMR sets Ref = "refs/heads/" + source_branch). Without it a
// status can attach to the wrong/null external pipeline and vanish from the MR.
// A ref over GitLab's 255-char cap (event refs are accepted up to 512) is
// omitted rather than truncated — a truncated ref would name a different branch,
// and the status still posts keyed on the SHA.
func refName(ref string) string {
	r := strings.TrimPrefix(ref, "refs/heads/")
	if len(r) > maxRefLen {
		return ""
	}
	return r
}

// shard maps a key to a worker index. Same key -> same worker -> FIFO order.
func shard(key string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(n)) //nolint:gosec // n is small and positive
}

// gitlabState maps a Janus status to a GitLab commit-status state, or "" for
// states GitLab has no equivalent for / that we deliberately don't post
// (pending, skipped). A skipped run means the workflow didn't apply to the
// commit; posting success would green-check code CI never validated.
func gitlabState(s model.Status) string {
	switch s {
	case model.StatusRunning:
		return "running"
	case model.StatusSuccess:
		return "success"
	case model.StatusFailed:
		return "failed"
	case model.StatusCancelled:
		return "canceled" // GitLab's spelling
	}
	return ""
}

// description is the human-facing status line. It keys off the reported state
// (not run.Status, which for the pre-execution "running" post is still pending).
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

func clip(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
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
// target_url link). Errors are value-free: the value may reach a startup log.
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

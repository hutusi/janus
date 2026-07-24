// Package status reports a run's lifecycle state to GitLab's Commit Status API
// (POST /api/v4/projects/{id}/statuses/{sha}), so pass/fail shows natively on
// the triggering commit or merge request. It is Janus's second outbound HTTP
// user, after internal/notify, and copies that package's safety posture: the
// reporter owns its own context (never the run's), each POST runs on its own
// bounded goroutine with a per-request timeout, a slow/failing/hung GitLab is
// logged and never fails or blocks a run, redirects are not followed (so the
// PRIVATE-TOKEN can't ride a downgrade), and Close drains in-flight posts at
// shutdown. It is GitLab-specific and best-effort by design.
package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// defaultMaxInFlight caps concurrent posts so a slow GitLab cannot
	// accumulate goroutines/connections without bound.
	defaultMaxInFlight = 16
	// statusContext is the GitLab status "context" (its grouping key): one row
	// per commit whose lifecycle updates running -> terminal.
	statusContext = "janus"
	// maxDescLen bounds the human-facing description.
	maxDescLen = 512
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
	maxInFlight int

	sem    chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
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

// WithMaxInFlight caps concurrent posts (defaults to defaultMaxInFlight).
func WithMaxInFlight(m int) Option {
	return func(r *Reporter) {
		if m > 0 {
			r.maxInFlight = m
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
	r := &Reporter{token: token, logger: slog.Default(), timeout: defaultTimeout, maxInFlight: defaultMaxInFlight}
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
	r.sem = make(chan struct{}, r.maxInFlight)
	r.ctx, r.cancel = context.WithCancel(context.Background())
	return r, nil
}

// Report posts run's current lifecycle state to GitLab's commit-status API. It
// returns as soon as the post is handed off, so it never blocks or fails the
// caller. state is the run's current status (StatusRunning for the
// pre-execution post, or the terminal status); the reporter maps it and no-ops
// for states or events it does not report (pending, skipped, non-GitLab events,
// events without a project id / sha, or an unresolvable API base).
func (r *Reporter) Report(run *model.Run, state model.Status) {
	glState := gitlabState(state)
	if glState == "" {
		return // pending, skipped, or anything non-terminal-and-non-running
	}
	ev := run.Event
	if ev.Provider != "gitlab" || ev.ProjectID <= 0 || ev.SHA == "" {
		return // not a reportable GitLab run
	}
	base := r.instanceURL
	if base == nil {
		base = deriveBase(ev.RepoURL)
	}
	if base == nil {
		// clone_url "ssh" (or an odd URL) with no gitlab_url override: we can't
		// address the API. Debug, not warn — this is expected in that config.
		r.logger.Debug("commit status skipped: no resolvable GitLab API base", "run_id", run.ID, "project", ev.ProjectID)
		return
	}
	endpoint := base.JoinPath("api", "v4", "projects", strconv.FormatInt(ev.ProjectID, 10), "statuses", ev.SHA).String()

	body, err := json.Marshal(statusBody{
		State:       glState,
		Context:     statusContext,
		TargetURL:   r.targetURL(run.ID),
		Description: description(run, state),
	})
	if err != nil {
		r.logger.Warn("commit status payload could not be encoded", "run_id", run.ID, "err", err)
		return
	}

	label := "gitlab project " + strconv.FormatInt(ev.ProjectID, 10)
	select {
	case r.sem <- struct{}{}:
		r.wg.Add(1)
		go func() {
			defer func() { <-r.sem; r.wg.Done() }()
			r.deliver(endpoint, body, run.ID, label)
		}()
	default:
		r.logger.Warn("commit status dropped: too many posts in flight", "run_id", run.ID, "target", label)
	}
}

func (r *Reporter) deliver(endpoint string, body []byte, runID, label string) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("commit status post panicked", "run_id", runID, "target", label, "panic", rec)
		}
	}()

	req, err := http.NewRequestWithContext(r.ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		r.logger.Warn("commit status request could not be built", "run_id", runID, "target", label, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("PRIVATE-TOKEN", r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		r.logger.Warn("commit status post failed", "run_id", runID, "target", label, "err", safeError(err))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		r.logger.Warn("commit status endpoint returned an error status", "run_id", runID, "target", label, "status", resp.StatusCode)
	}
}

// Close drains in-flight posts, waiting up to timeout, then cancels any that
// remain. Call it after the runner has shut down.
func (r *Reporter) Close(timeout time.Duration) {
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
	return r.baseURL.JoinPath("runs", runID).String()
}

type statusBody struct {
	State       string `json:"state"`
	Context     string `json:"context"`
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description,omitempty"`
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
			return clip(wf+": "+model.RedactURL(run.Reason), maxDescLen)
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

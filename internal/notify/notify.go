// Package notify delivers a JSON summary of a finished run to configured
// outbound webhook endpoints. It is Janus's only outbound HTTP client.
//
// Delivery is best-effort and must never fail or block a run: Notify builds the
// payload synchronously, then hands each matching target off to its own
// goroutine with the notifier's own (not the run's) context, logging failures
// rather than propagating them. A slow, failing, or hung endpoint therefore
// cannot fail a run or stall the runner. Close drains in-flight deliveries at
// shutdown, so a run that finishes just before SIGTERM still gets to notify.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hutusi/janus/internal/model"
)

// defaultTimeout bounds a single delivery so a slow endpoint cannot pin a
// goroutine (and, at shutdown, the drain) indefinitely.
const defaultTimeout = 10 * time.Second

// Target is the wire form of one endpoint passed to New. URL is required and
// must be http/https. On lists the terminal run statuses that deliver to this
// target (any of success/failed/cancelled/skipped); empty means failures only.
// Secret, when set, is sent as an "Authorization: Bearer <secret>" header.
type Target struct {
	URL    string
	On     []string
	Secret string
}

// target is the validated internal form of a Target.
type target struct {
	url    string
	secret string
	on     map[model.Status]bool
}

// Notifier posts run summaries to its targets. Construct it with New; it is safe
// for concurrent use by multiple run goroutines.
type Notifier struct {
	targets []target
	client  *http.Client
	logger  *slog.Logger
	baseURL string
	timeout time.Duration

	// ctx is the notifier's own lifetime, cancelled only by Close — never tied
	// to a run or the runner, so a shutdown/cancel of the run being reported can
	// never abort its own completion notification. wg tracks in-flight
	// deliveries so Close can drain them.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Option configures a Notifier.
type Option func(*Notifier)

// WithLogger sets the logger (defaults to slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(n *Notifier) {
		if l != nil {
			n.logger = l
		}
	}
}

// WithBaseURL sets the daemon's public base URL; when non-empty, payloads carry
// a link to the run page (<base_url>/runs/<id>). Validated by New.
func WithBaseURL(u string) Option { return func(n *Notifier) { n.baseURL = u } }

// WithHTTPClient overrides the HTTP client (mainly for tests). When set, its own
// Timeout governs; WithTimeout is ignored.
func WithHTTPClient(c *http.Client) Option {
	return func(n *Notifier) {
		if c != nil {
			n.client = c
		}
	}
}

// WithTimeout sets the per-delivery timeout used for the default client
// (ignored when WithHTTPClient supplies a client).
func WithTimeout(d time.Duration) Option {
	return func(n *Notifier) {
		if d > 0 {
			n.timeout = d
		}
	}
}

// New validates targets and returns a Notifier. It rejects a non-http(s) URL, an
// unknown status in a target's On list, and a malformed base URL — surfaced by
// the caller as a startup error, exactly like allowlist.New.
func New(targets []Target, opts ...Option) (*Notifier, error) {
	n := &Notifier{logger: slog.Default(), timeout: defaultTimeout}
	for _, opt := range opts {
		opt(n)
	}
	if n.logger == nil {
		n.logger = slog.Default()
	}
	if n.baseURL != "" {
		if err := validateHTTPURL(n.baseURL); err != nil {
			return nil, fmt.Errorf("base_url: %w", err)
		}
		n.baseURL = strings.TrimRight(n.baseURL, "/")
	}
	for i, t := range targets {
		vt, err := validateTarget(t)
		if err != nil {
			return nil, fmt.Errorf("notifications[%d]: %w", i, err)
		}
		n.targets = append(n.targets, vt)
	}
	if n.client == nil {
		n.client = &http.Client{Timeout: n.timeout}
	}
	n.ctx, n.cancel = context.WithCancel(context.Background())
	return n, nil
}

func validateTarget(t Target) (target, error) {
	if err := validateHTTPURL(t.URL); err != nil {
		return target{}, fmt.Errorf("url: %w", err)
	}
	on := make(map[model.Status]bool, len(t.On))
	for _, s := range t.On {
		st := model.Status(s)
		if !notifiable(st) {
			return target{}, fmt.Errorf("on: %q is not a run outcome (use success, failed, cancelled, or skipped)", s)
		}
		on[st] = true
	}
	if len(on) == 0 {
		on[model.StatusFailed] = true // default: failures only
	}
	return target{url: t.URL, secret: t.Secret, on: on}, nil
}

// notifiable reports whether s is a terminal status a target may subscribe to.
// pending/running are not — a notification only fires on a settled run.
func notifiable(s model.Status) bool {
	switch s {
	case model.StatusSuccess, model.StatusFailed, model.StatusCancelled, model.StatusSkipped:
		return true
	}
	return false
}

func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q must be an http:// or https:// URL", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	return nil
}

// Notify delivers a summary of run to every target subscribed to run.Status. It
// returns as soon as the deliveries are handed off — the POSTs happen in the
// background — so it never blocks or fails the caller. run is read only
// synchronously (the payload is built before any goroutine starts), so the
// caller may reuse or mutate it afterward.
func (n *Notifier) Notify(run *model.Run) {
	var matched []target
	for _, t := range n.targets {
		if t.on[run.Status] {
			matched = append(matched, t)
		}
	}
	if len(matched) == 0 {
		return
	}
	body, err := json.Marshal(n.buildPayload(run))
	if err != nil {
		// Should not happen for these field types, but never let it escape.
		n.logger.Warn("notification payload could not be encoded", "run_id", run.ID, "err", err)
		return
	}
	// Add to the WaitGroup synchronously, before spawning, so the hand-off
	// happens-before the caller returns — the ordering Close relies on to drain.
	for _, t := range matched {
		n.wg.Add(1)
		go n.deliver(t, run.ID, body)
	}
}

func (n *Notifier) deliver(t target, runID string, body []byte) {
	defer n.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			// A panic here (e.g. a pathological transport) must stay contained —
			// this goroutine is detached from the run, and the runner's own panic
			// net deliberately does not recover.
			n.logger.Error("notification delivery panicked", "run_id", runID, "target", model.RedactURL(t.url), "panic", r)
		}
	}()

	req, err := http.NewRequestWithContext(n.ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		n.logger.Warn("notification request could not be built", "run_id", runID, "target", model.RedactURL(t.url), "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if t.secret != "" {
		req.Header.Set("Authorization", "Bearer "+t.secret)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		// The URL is echoed in transport errors and may embed credentials.
		n.logger.Warn("notification delivery failed", "run_id", runID, "target", model.RedactURL(t.url), "err", model.RedactURL(err.Error()))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection can be reused
	if resp.StatusCode >= 300 {
		n.logger.Warn("notification endpoint returned an error status", "run_id", runID, "target", model.RedactURL(t.url), "status", resp.StatusCode)
	}
}

// Close drains in-flight deliveries, waiting up to timeout, then cancels any
// that remain (aborting their POSTs). Call it after the runner has shut down, so
// every terminal run has already handed its notification off.
func (n *Notifier) Close(timeout time.Duration) {
	done := make(chan struct{})
	go func() { n.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
	n.cancel()
}

// payload is the JSON body posted to a target. Summary only: per-job status and
// counts, no step detail (additively extensible if that is ever wanted).
type payload struct {
	RunID        string    `json:"run_id"`
	Workflow     string    `json:"workflow"`
	Status       string    `json:"status"`
	Reason       string    `json:"reason,omitempty"`
	Provider     string    `json:"provider"`
	Event        string    `json:"event"`
	RepoURL      string    `json:"repo_url"`
	Branch       string    `json:"branch,omitempty"`
	Ref          string    `json:"ref,omitempty"`
	Commit       string    `json:"commit,omitempty"`
	CommitTitle  string    `json:"commit_title,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	StartedAt    time.Time `json:"started_at,omitzero"`
	FinishedAt   time.Time `json:"finished_at,omitzero"`
	DurationSecs *float64  `json:"duration_seconds,omitempty"`
	JobsTotal    int       `json:"jobs_total"`
	JobsFailed   int       `json:"jobs_failed"`
	Jobs         []job     `json:"jobs"`
	URL          string    `json:"url,omitempty"`
}

type job struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (n *Notifier) buildPayload(run *model.Run) payload {
	p := payload{
		RunID:       run.ID,
		Workflow:    run.WorkflowName,
		Status:      string(run.Status),
		Reason:      run.Reason,
		Provider:    run.Event.Provider,
		Event:       string(run.Event.Kind),
		RepoURL:     model.RedactURL(run.Event.RepoURL), // may embed credentials
		Branch:      run.Event.Branch,
		Ref:         run.Event.Ref,
		Commit:      run.Event.SHA,
		CommitTitle: run.Event.Title,
		CreatedAt:   run.CreatedAt,
		StartedAt:   run.StartedAt,
		FinishedAt:  run.FinishedAt,
		JobsTotal:   len(run.Jobs),
		Jobs:        make([]job, 0, len(run.Jobs)),
	}
	for _, jr := range run.Jobs {
		if jr.Status == model.StatusFailed {
			p.JobsFailed++
		}
		p.Jobs = append(p.Jobs, job{Name: jr.Name, Status: string(jr.Status)})
	}
	// Duration is only meaningful once execution began; pre-execution terminals
	// (checkout/parse/no-match failures) have a zero StartedAt, so omit it there
	// rather than report a nonsensical value.
	if !run.StartedAt.IsZero() {
		d := run.FinishedAt.Sub(run.StartedAt).Seconds()
		p.DurationSecs = &d
	}
	if n.baseURL != "" {
		p.URL = n.baseURL + "/runs/" + run.ID
	}
	return p
}

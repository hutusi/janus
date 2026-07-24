// Package notify delivers a JSON summary of a finished run to configured
// outbound webhook endpoints. It is Janus's only outbound HTTP client.
//
// Delivery is best-effort and must never fail or block a run: Notify builds the
// payload synchronously, then hands each matching target off to its own bounded
// set of delivery goroutines using the notifier's own (not the run's) context,
// logging failures rather than propagating them. A slow, failing, or hung
// endpoint therefore cannot fail a run, stall the runner, or starve the other
// targets; when too many deliveries to one target are already in flight, further
// ones to that target are dropped (and logged) rather than queued unboundedly.
// Close drains in-flight deliveries at shutdown, so a run that finishes just
// before SIGTERM still gets to notify.
package notify

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
	"strings"
	"sync"
	"time"

	"github.com/hutusi/janus/internal/model"
)

const (
	// defaultTimeout bounds a single delivery so a slow endpoint cannot pin a
	// goroutine (and, at shutdown, the drain) indefinitely.
	defaultTimeout = 10 * time.Second
	// defaultMaxInFlight caps concurrent deliveries per target, so a slow
	// endpoint cannot accumulate goroutines/connections/file descriptors without
	// bound or starve other targets.
	defaultMaxInFlight = 16
)

// Target is the wire form of one endpoint passed to New. URL is required and
// must be http/https. On lists the terminal run statuses that deliver to this
// target (any of success/failed/cancelled/skipped); empty means failures only.
// Secret, when set, is sent as an "Authorization: Bearer <secret>" header.
type Target struct {
	URL    string
	On     []string
	Secret string
}

// target is the validated internal form of a Target. label is a log-safe
// identifier — origin plus index, never the full URL, whose path or query may
// carry the endpoint's secret. sem bounds this target's own in-flight
// deliveries, so a slow endpoint fills only its own slots and cannot starve the
// others.
type target struct {
	url    string
	secret string
	on     map[model.Status]bool
	label  string
	sem    chan struct{}
}

// Notifier posts run summaries to its targets. Construct it with New; it is safe
// for concurrent use by multiple run goroutines.
type Notifier struct {
	targets     []target
	client      *http.Client
	logger      *slog.Logger
	baseURL     *url.URL // parsed public base URL for run links; nil = unset
	baseURLRaw  string   // set by WithBaseURL, parsed and validated in New
	timeout     time.Duration
	maxInFlight int

	// Concurrent deliveries are bounded per target (target.sem), so one slow
	// endpoint can't starve the others. ctx is the notifier's own lifetime,
	// cancelled only by Close — never tied to a run or the runner, so a
	// shutdown/cancel of the run being reported can never abort its own
	// completion notification. wg tracks every in-flight delivery (across all
	// targets) so Close can drain them.
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
func WithBaseURL(u string) Option { return func(n *Notifier) { n.baseURLRaw = u } }

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

// WithMaxInFlight caps concurrent deliveries per target (defaults to
// defaultMaxInFlight).
func WithMaxInFlight(m int) Option {
	return func(n *Notifier) {
		if m > 0 {
			n.maxInFlight = m
		}
	}
}

// New validates targets and returns a Notifier. It rejects a non-http(s) URL, an
// unknown status in a target's On list, and a malformed or credential-bearing
// base URL — surfaced by the caller as a startup error, exactly like
// allowlist.New.
func New(targets []Target, opts ...Option) (*Notifier, error) {
	n := &Notifier{logger: slog.Default(), timeout: defaultTimeout, maxInFlight: defaultMaxInFlight}
	for _, opt := range opts {
		opt(n)
	}
	if n.logger == nil {
		n.logger = slog.Default()
	}
	if n.baseURLRaw != "" {
		bu, err := validateBaseURL(n.baseURLRaw)
		if err != nil {
			return nil, fmt.Errorf("base_url: %w", err)
		}
		n.baseURL = bu
	}
	for i, t := range targets {
		vt, err := validateTarget(t, i)
		if err != nil {
			return nil, fmt.Errorf("notifications[%d]: %w", i, err)
		}
		vt.sem = make(chan struct{}, n.maxInFlight) // per-target delivery bound
		n.targets = append(n.targets, vt)
	}
	if n.client == nil {
		n.client = &http.Client{Timeout: n.timeout}
	}
	n.ctx, n.cancel = context.WithCancel(context.Background())
	return n, nil
}

func validateTarget(t Target, i int) (target, error) {
	u, err := parseHTTPURL(t.URL)
	if err != nil {
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
	// Origin + index only: the full URL's path/query may hold the endpoint's
	// secret, so it must never reach a log line.
	return target{
		url:    t.URL,
		secret: t.Secret,
		on:     on,
		label:  fmt.Sprintf("%s://%s#%d", u.Scheme, u.Host, i),
	}, nil
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

// parseHTTPURL parses raw and requires an http/https URL with a host. Path,
// query, and userinfo are allowed — a webhook target legitimately carries them.
// Errors never echo the URL: the value may embed a secret and reaches the
// startup log via main, so the caller's key/index (notifications[i], base_url)
// is what locates the offending entry. url.Parse's own error text embeds the
// URL, so it is deliberately not wrapped.
func parseHTTPURL(raw string) (*url.URL, error) {
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
	return u, nil
}

// validateBaseURL is stricter than parseHTTPURL: the base URL is joined with a
// path and copied into every payload, so userinfo (a credential leak), a query,
// or a fragment (a broken link) are rejected. Errors stay value-free for the
// same reason as parseHTTPURL.
func validateBaseURL(raw string) (*url.URL, error) {
	u, err := parseHTTPURL(raw)
	if err != nil {
		return nil, err
	}
	if u.User != nil {
		return nil, errors.New("must not contain userinfo (it would be copied into every notification link)")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("must not contain a query or fragment")
	}
	return u, nil
}

// Notify delivers a summary of run to every target subscribed to run.Status. It
// returns as soon as the deliveries are handed off — the POSTs happen in the
// background — so it never blocks or fails the caller. run is read only
// synchronously (the payload is built before any goroutine starts), so the
// caller may reuse or mutate it afterward. When a target already has its
// per-target cap of deliveries in flight, further ones to that target are
// dropped and logged rather than queued.
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
	runID := run.ID
	for _, t := range matched {
		// Non-blocking acquire on this target's own semaphore: it bounds
		// concurrent deliveries per target, so a saturated (slow) endpoint drops
		// only its own deliveries and never blocks the caller or starves the
		// others. wg.Add is synchronous, before the goroutine spawns, so the
		// hand-off happens-before the caller returns — the ordering Close relies
		// on to drain.
		select {
		case t.sem <- struct{}{}:
			n.wg.Add(1)
			go func(t target) {
				defer func() { <-t.sem; n.wg.Done() }()
				n.deliver(t, runID, body)
			}(t)
		default:
			n.logger.Warn("notification dropped: too many deliveries in flight for this target", "run_id", runID, "target", t.label)
		}
	}
}

func (n *Notifier) deliver(t target, runID string, body []byte) {
	defer func() {
		if r := recover(); r != nil {
			// A panic here (e.g. a pathological transport) must stay contained —
			// this goroutine is detached from the run, and the runner's own panic
			// net deliberately does not recover.
			n.logger.Error("notification delivery panicked", "run_id", runID, "target", t.label, "panic", r)
		}
	}()

	req, err := http.NewRequestWithContext(n.ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		n.logger.Warn("notification request could not be built", "run_id", runID, "target", t.label, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if t.secret != "" {
		req.Header.Set("Authorization", "Bearer "+t.secret)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		// The transport error echoes the request URL, whose path/query may carry
		// the endpoint's secret; safeError strips it.
		n.logger.Warn("notification delivery failed", "run_id", runID, "target", t.label, "err", safeError(err))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection can be reused
	if resp.StatusCode >= 300 {
		n.logger.Warn("notification endpoint returned an error status", "run_id", runID, "target", t.label, "status", resp.StatusCode)
	}
}

// safeError renders err for logging without leaking the request URL. Go's HTTP
// client wraps transport failures in *url.Error, whose URL field holds the full
// target (path/query may carry a secret); drop it and keep the operation and
// underlying cause.
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
		Reason:      model.RedactURL(run.Reason), // defense-in-depth: the runner already redacts, but this is an external egress
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
	var failed []string
	for _, jr := range run.Jobs {
		if jr.Status == model.StatusFailed {
			p.JobsFailed++
			failed = append(failed, jr.Name)
		}
		p.Jobs = append(p.Jobs, job{Name: jr.Name, Status: string(jr.Status)})
	}
	// A run that failed on its own merits carries no engine Reason (only
	// pre-execution/cancellation/all-skipped outcomes set one), so surface which
	// jobs failed — the notification's headline field is then always meaningful.
	// A Reason set upstream is preserved.
	if p.Reason == "" && run.Status == model.StatusFailed && len(failed) > 0 {
		p.Reason = "failed jobs: " + strings.Join(failed, ", ")
	}
	// Duration is only meaningful once execution began; pre-execution terminals
	// (checkout/parse/no-match failures) have a zero StartedAt, so omit it there
	// rather than report a nonsensical value.
	if !run.StartedAt.IsZero() {
		d := run.FinishedAt.Sub(run.StartedAt).Seconds()
		p.DurationSecs = &d
	}
	// Structural join, so a base URL with a path yields <base>/runs/<id> rather
	// than a broken concatenation.
	if n.baseURL != nil {
		p.URL = n.baseURL.JoinPath("runs", run.ID).String()
	}
	return p
}

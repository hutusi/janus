// Package model holds Janus's core domain types: the immutable pipeline
// specification parsed from YAML (Workflow/Job/Step), the normalized trigger
// Event, and the mutable runtime run state (Run/JobRun/StepRun). It has no
// dependencies so every other package can import it without risking a cycle.
package model

import (
	"encoding/json"
	"time"
)

// Workflow is a parsed and validated pipeline specification (.janus/ci.yml).
// It is immutable once produced by pipeline.Parse.
type Workflow struct {
	Name string
	On   Triggers
	// Concurrency serializes runs of this workflow that share a group. Nil
	// means no grouping: runs execute independently, as if the key were absent.
	Concurrency *Concurrency
	Env         map[string]string
	Jobs        map[string]*Job
}

// Concurrency is the workflow-level `concurrency:` key. Group is a template
// over the closed interpolation set minus sha/short_sha (which would make
// every run its own group); empty means the implicit "<name>-<branch|ref>"
// group. At runtime a group is always scoped to the triggering repo, so
// workflows in different repos never affect each other. Per group at most one
// run executes and at most one waits; a newer trigger supersedes the waiting
// run, and with CancelInProgress it also cancels the executing one.
type Concurrency struct {
	Group            string
	CancelInProgress bool
}

// Triggers describes which events start the workflow. A nil pointer means the
// corresponding trigger was not declared in the YAML.
type Triggers struct {
	Push         *Trigger
	MergeRequest *Trigger
}

// Trigger is one entry under `on:`: a branch filter plus, for push triggers
// only, an optional changed-paths filter (validation rejects `paths` on
// merge_request — an MR's changed set needs a merge base, which the shallow
// checkout deliberately avoids).
type Trigger struct {
	BranchFilter
	Paths *PathFilter
}

// BranchFilter restricts a trigger to a set of branches. Branches is an
// allowlist (empty or nil matches every branch); Ignore is a denylist that
// wins over the allowlist. Validation rejects declaring both in the YAML;
// slice nil-ness (key absent vs present-but-empty) is preserved from the YAML
// so that check can tell them apart.
type BranchFilter struct {
	Branches []string
	Ignore   []string
}

// Matches reports whether branch is allowed by the filter: never when branch
// is in Ignore, otherwise when Branches is empty or contains it.
func (f *BranchFilter) Matches(branch string) bool {
	for _, b := range f.Ignore {
		if b == branch {
			return false
		}
	}
	if len(f.Branches) == 0 {
		return true
	}
	for _, b := range f.Branches {
		if b == branch {
			return true
		}
	}
	return false
}

// Job is a unit of work: a sequence of steps that run on the host, optionally
// after other jobs complete (Needs).
type Job struct {
	Name  string
	Needs []string
	// Filter restricts the job to a set of branches (the job-level
	// `branches`/`branches-ignore` keys, same semantics as an `on:` filter,
	// matched against the event's branch regardless of event kind). Nil means
	// the job runs on every branch. A non-matching job — and any job that
	// needs one — is recorded skipped instead of executed.
	Filter *BranchFilter
	// PathFilter restricts the job to pushes whose changed files match (the
	// job-level `paths`/`paths-ignore` keys). Nil means the job runs
	// regardless of what changed. Like Filter, a non-matching job — and any
	// job that needs one — is recorded skipped. Only push events with a known
	// changed set are constrained; other events (and pushes whose diff could
	// not be determined) run the job — a path filter fails open, never
	// wrongly skipping CI.
	PathFilter *PathFilter
	// WorkingDir is the default working directory for the job's steps,
	// relative to the workspace; a step's own working-directory overrides it.
	// ${{ }} interpolation applies. Empty means the workspace root.
	WorkingDir string
	Env        map[string]string
	Steps      []Step
}

// Step is a single shell command, optionally with its own working directory
// (relative to the workspace), shell, and environment overlay. Shell is empty
// for the OS default (/bin/sh on unix, cmd on Windows); the pipeline validator
// restricts it to the closed set the engine knows how to run.
type Step struct {
	Run        string
	WorkingDir string
	Shell      string
	Env        map[string]string
}

// EventKind is the normalized trigger type. Provider-specific names (GitLab's
// "merge_request", a future GitHub "pull_request") map onto these.
type EventKind string

const (
	EventPush         EventKind = "push"
	EventMergeRequest EventKind = "merge_request"
	EventManual       EventKind = "manual"
)

// Event is a normalized trigger that produced a run, independent of which
// provider (or manual call) created it. It is the value behind ${{ event }}
// and supplies ${{ ref }}/${{ sha }}/${{ branch }}.
type Event struct {
	Provider string    `json:"provider"`        // "gitlab", "manual", ...
	Kind     EventKind `json:"kind"`            // normalized trigger type
	RepoURL  string    `json:"repo_url"`        // clone URL
	Ref      string    `json:"ref,omitempty"`   // e.g. refs/heads/main
	Branch   string    `json:"branch"`          // e.g. main
	SHA      string    `json:"sha,omitempty"`   // commit to check out
	Title    string    `json:"title,omitempty"` // commit/MR title, for display

	// ProjectID is the provider's numeric project identifier (GitLab's
	// project.id), recorded so a run names its project the way the host does.
	// Zero when the provider did not supply one (manual triggers, other
	// providers). Not a secret.
	//
	// Like RepoSlug it is metadata only: it comes from the webhook payload and
	// nothing binds it to RepoURL, so the commit-status reporter does not address
	// a project by it — it derives the NAMESPACE/PROJECT path from RepoURL, the
	// string the allowlist gated, and sends that URL-encoded (GitLab accepts a
	// namespaced path wherever :id appears). See internal/status/gitlab.go.
	ProjectID int64 `json:"project_id,omitempty"`

	// RepoSlug is the provider's "owner/repo" identifier (GitHub's
	// repository.full_name), recorded so a run names its repository the way the
	// host does. Empty for providers that identify by ProjectID (GitLab) and for
	// manual triggers. Not a secret.
	//
	// It is metadata only: it comes from the webhook payload and nothing binds it
	// to RepoURL, so the commit-status reporter deliberately does not address a
	// repository by it — it derives owner/repo from RepoURL, the string the
	// allowlist gated (see internal/status/github.go).
	RepoSlug string `json:"repo_slug,omitempty"`

	// Before is the commit the pushed ref pointed at before this push (the
	// GitLab payload's `before`), used to compute the changed-file set for
	// `paths` filters. Empty when unknown: new branches (all-zeros before),
	// merge requests, and manual triggers — path filters then fail open.
	Before string `json:"before,omitempty"`

	// PipelinePath overrides the pipeline file for this trigger, relative to
	// the configured file's directory, e.g. "release.yml" for
	// .janus/release.yml (the manual API's pipeline_path field, or a
	// webhook URL's ?pipeline_path= query parameter; empty means the
	// configured default).
	PipelinePath string `json:"pipeline_path,omitempty"`
}

// MarshalJSON redacts credentials from RepoURL whenever an Event is serialized —
// the JSON API responses and the flat-file store's run.json — so an embedded
// token (https://user:token@host/…) never leaves the process or lands on disk.
// RepoURL stays raw in memory, where the checkout needs it; the value receiver
// makes this fire for Event embedded in Run/RunSummary too, and RedactURL is a
// no-op on a credential-free URL.
func (e Event) MarshalJSON() ([]byte, error) {
	type alias Event // no MarshalJSON of its own → no recursion, same fields/tags
	a := alias(e)
	a.RepoURL = RedactURL(e.RepoURL)
	return json.Marshal(a)
}

// Status is the lifecycle state of a run, job, or step.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSuccess   Status = "success"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
	StatusSkipped   Status = "skipped"
)

// Terminal reports whether the status is final (no further transitions).
func (s Status) Terminal() bool {
	switch s {
	case StatusSuccess, StatusFailed, StatusCancelled, StatusSkipped:
		return true
	}
	return false
}

// Run is a single execution of a workflow.
type Run struct {
	ID           string `json:"id"`
	WorkflowName string `json:"workflow_name"`
	Event        Event  `json:"event"`
	Status       Status `json:"status"`
	// Reason records why a run reached a terminal state other than by its jobs
	// finishing: a checkout or pipeline-parse failure, an event that did not
	// match the workflow's on: filters, or a cancellation ("cancelled via
	// API", "superseded by run <id>"). Empty for runs that ran to completion.
	Reason string    `json:"reason,omitempty"`
	Jobs   []*JobRun `json:"jobs"`
	// ConcurrencyGroup is the expanded (repo-scoped at runtime, but stored
	// without the repo prefix) concurrency group the run belongs to. Empty for
	// workflows without a concurrency: key.
	ConcurrencyGroup string    `json:"concurrency_group,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	StartedAt        time.Time `json:"started_at,omitzero"`
	FinishedAt       time.Time `json:"finished_at,omitzero"`
	WorkspaceDir     string    `json:"workspace_dir,omitempty"`
}

// MarshalJSON redacts credentials from Reason whenever a Run is serialized,
// mirroring what Event.MarshalJSON does for RepoURL. Reason is redacted before
// it is stored too, so this is belt-and-braces for records written by versions
// that predate that fix: a checkout failure could fill Reason with a git error
// echoing a credentialed clone URL, and those records are still read back by
// GET /api/runs/{id} and rendered on the (unauthenticated) dashboard. Redacting
// on the way out closes that surface without rewriting history on disk.
func (r Run) MarshalJSON() ([]byte, error) {
	type alias Run // no MarshalJSON of its own → no recursion, same fields/tags
	a := alias(r)
	a.Reason = RedactURL(r.Reason)
	return json.Marshal(a)
}

// RunSummary is the compact projection of a Run used for listings: it omits
// the heavy Jobs slice (commands, steps), so listing/pruning/reconciliation can
// scan history without holding every full record in memory. Its json tags match
// Run's, so a run.json partial-unmarshals straight into it.
type RunSummary struct {
	ID           string    `json:"id"`
	WorkflowName string    `json:"workflow_name"`
	Event        Event     `json:"event"`
	Status       Status    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	StartedAt    time.Time `json:"started_at,omitzero"`
	FinishedAt   time.Time `json:"finished_at,omitzero"`
}

// Summary projects a Run to a RunSummary (used by the in-memory store).
func (r *Run) Summary() *RunSummary {
	return &RunSummary{
		ID:           r.ID,
		WorkflowName: r.WorkflowName,
		Event:        r.Event,
		Status:       r.Status,
		CreatedAt:    r.CreatedAt,
		StartedAt:    r.StartedAt,
		FinishedAt:   r.FinishedAt,
	}
}

// JobRun is the runtime state of one job within a run.
type JobRun struct {
	Name       string     `json:"name"`
	Needs      []string   `json:"needs,omitempty"`
	Status     Status     `json:"status"`
	Steps      []*StepRun `json:"steps"`
	StartedAt  time.Time  `json:"started_at,omitzero"`
	FinishedAt time.Time  `json:"finished_at,omitzero"`
}

// StepRun is the runtime state of one step within a job. Logs are streamed to
// the Store keyed by (run, job, step) rather than held here.
type StepRun struct {
	Index      int       `json:"index"`
	Command    string    `json:"command"`
	Status     Status    `json:"status"`
	ExitCode   int       `json:"exit_code"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

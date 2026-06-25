// Package model holds Janus's core domain types: the immutable pipeline
// specification parsed from YAML (Workflow/Job/Step), the normalized trigger
// Event, and the mutable runtime run state (Run/JobRun/StepRun). It has no
// dependencies so every other package can import it without risking a cycle.
package model

import "time"

// Workflow is a parsed and validated pipeline specification (.janus/ci.yml).
// It is immutable once produced by pipeline.Parse.
type Workflow struct {
	Name string
	On   Triggers
	Env  map[string]string
	Jobs map[string]*Job
}

// Triggers describes which events start the workflow. A nil pointer means the
// corresponding trigger was not declared in the YAML.
type Triggers struct {
	Push         *BranchFilter
	MergeRequest *BranchFilter
}

// BranchFilter restricts a trigger to a set of branches. An empty (or nil)
// Branches slice matches every branch.
type BranchFilter struct {
	Branches []string
}

// Matches reports whether branch is allowed by the filter. A filter with no
// branches matches everything.
func (f *BranchFilter) Matches(branch string) bool {
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
	Env   map[string]string
	Steps []Step
}

// Step is a single shell command, optionally with its own working directory
// (relative to the workspace) and environment overlay.
type Step struct {
	Run        string
	WorkingDir string
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
	ID           string    `json:"id"`
	WorkflowName string    `json:"workflow_name"`
	Event        Event     `json:"event"`
	Status       Status    `json:"status"`
	Jobs         []*JobRun `json:"jobs"`
	CreatedAt    time.Time `json:"created_at"`
	StartedAt    time.Time `json:"started_at,omitzero"`
	FinishedAt   time.Time `json:"finished_at,omitzero"`
	WorkspaceDir string    `json:"workspace_dir,omitempty"`
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

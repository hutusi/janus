// Package model holds Janus's core domain types: the immutable pipeline
// specification parsed from YAML (Workflow/Job/Step) and, added later, the
// mutable runtime run state. It has no dependencies so every other package can
// import it without risking an import cycle.
package model

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

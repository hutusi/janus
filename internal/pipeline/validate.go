package pipeline

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hutusi/janus/internal/model"
)

// jobNameRe is the closed charset for job names. Beyond readability, it keeps
// job-derived artifacts injective: the store names each step's log file after
// the job, so two names that sanitize alike ("a/b", "a?b") would silently
// share a log file.
var jobNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Generous structural limits. They sit far above any real pipeline and exist
// only to reject the absurd, so per-run artifacts (the run JSON, the log-file
// set, the dashboard's step table) stay finite for a decidedly-bounded input.
const (
	maxJobs         = 256
	maxStepsPerJob  = 256
	maxJobNameLen   = 256
	maxCommandBytes = 64 << 10
)

// allowedShells is the closed set of step `shell:` values. "" selects the OS
// default (/bin/sh on unix, cmd on Windows); the engine (shellArgv) maps each
// name to an argv prefix and must stay in sync with this set.
var allowedShells = map[string]bool{
	"":           true,
	"sh":         true,
	"bash":       true,
	"cmd":        true,
	"powershell": true,
	"pwsh":       true,
}

// validate performs all semantic checks on an already-decoded workflow. Jobs are
// visited in sorted order so error messages are deterministic.
func validate(wf *model.Workflow) error {
	if strings.TrimSpace(wf.Name) == "" {
		return errors.New("`name` is required")
	}
	if wf.On.Push == nil && wf.On.MergeRequest == nil {
		return errors.New("`on` must declare at least one of `push` or `merge_request`")
	}
	// A trigger takes `branches` (allowlist) or `branches-ignore` (denylist),
	// never both. Slice nil-ness survives toModel, so nil vs non-nil
	// distinguishes "key absent" from "key present" (even `branches: []`).
	for _, tr := range []struct {
		key string
		f   *model.BranchFilter
	}{{"push", wf.On.Push}, {"merge_request", wf.On.MergeRequest}} {
		if tr.f != nil && tr.f.Branches != nil && tr.f.Ignore != nil {
			return fmt.Errorf("`on.%s` cannot set both `branches` and `branches-ignore`", tr.key)
		}
	}
	if len(wf.Jobs) == 0 {
		return errors.New("at least one job is required under `jobs`")
	}
	if len(wf.Jobs) > maxJobs {
		return fmt.Errorf("too many jobs: %d (max %d)", len(wf.Jobs), maxJobs)
	}

	for _, name := range sortedJobNames(wf) {
		job := wf.Jobs[name]
		if !jobNameRe.MatchString(name) {
			return fmt.Errorf("job %q: names may contain only letters, digits, '-' and '_'", name)
		}
		if len(name) > maxJobNameLen {
			return fmt.Errorf("job name too long: %d characters (max %d)", len(name), maxJobNameLen)
		}
		// Same allowlist-or-denylist rule as the on: filters.
		if job.Filter != nil && job.Filter.Branches != nil && job.Filter.Ignore != nil {
			return fmt.Errorf("job %q cannot set both `branches` and `branches-ignore`", name)
		}
		if len(job.Steps) == 0 {
			return fmt.Errorf("job %q: at least one step is required", name)
		}
		if len(job.Steps) > maxStepsPerJob {
			return fmt.Errorf("job %q: too many steps: %d (max %d)", name, len(job.Steps), maxStepsPerJob)
		}
		for i, s := range job.Steps {
			if strings.TrimSpace(s.Run) == "" {
				return fmt.Errorf("job %q step %d: `run` is required and cannot be empty", name, i+1)
			}
			if len(s.Run) > maxCommandBytes {
				return fmt.Errorf("job %q step %d: `run` is too long: %d bytes (max %d)", name, i+1, len(s.Run), maxCommandBytes)
			}
			if !allowedShells[s.Shell] {
				return fmt.Errorf("job %q step %d: unsupported `shell` %q (allowed: sh, bash, cmd, powershell, pwsh)", name, i+1, s.Shell)
			}
		}
		seen := make(map[string]bool, len(job.Needs))
		for _, dep := range job.Needs {
			if dep == name {
				return fmt.Errorf("job %q cannot depend on itself", name)
			}
			if _, ok := wf.Jobs[dep]; !ok {
				return fmt.Errorf("job %q needs unknown job %q", name, dep)
			}
			if seen[dep] {
				return fmt.Errorf("job %q lists %q in `needs` more than once", name, dep)
			}
			seen[dep] = true
		}
	}

	if cycle := findCycle(wf); cycle != nil {
		return fmt.Errorf("`needs` forms a cycle: %s", strings.Join(cycle, " -> "))
	}

	return validateInterpolation(wf)
}

func sortedJobNames(wf *model.Workflow) []string {
	names := make([]string, 0, len(wf.Jobs))
	for n := range wf.Jobs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// findCycle returns a cycle in the needs graph as an ordered slice of job names
// (e.g. ["a","b","a"]), or nil if the graph is acyclic. It uses a three-colour
// DFS (white=unvisited, grey=on the stack, black=done); revisiting a grey node
// means we found a back edge. Jobs are visited in sorted order for determinism.
func findCycle(wf *model.Workflow) []string {
	const (
		white = iota
		grey
		black
	)
	color := make(map[string]int, len(wf.Jobs))
	var stack []string

	var visit func(name string) []string
	visit = func(name string) []string {
		color[name] = grey
		stack = append(stack, name)
		for _, dep := range wf.Jobs[name].Needs {
			switch color[dep] {
			case white:
				if c := visit(dep); c != nil {
					return c
				}
			case grey:
				// Back edge: extract the cycle from the current stack.
				for i, n := range stack {
					if n == dep {
						return append(append([]string{}, stack[i:]...), dep)
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[name] = black
		return nil
	}

	for _, name := range sortedJobNames(wf) {
		if color[name] == white {
			if c := visit(name); c != nil {
				return c
			}
		}
	}
	return nil
}

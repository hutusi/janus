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
	maxJobs           = 256
	maxStepsPerJob    = 256
	maxJobNameLen     = 256
	maxCommandBytes   = 64 << 10
	maxGroupLen       = 256
	maxPathPatterns   = 50
	maxPathPatternLen = 256
)

// validatePathFilter checks one `paths`/`paths-ignore` declaration (trigger or
// job level; where names the owner for error messages). Beyond the shared
// allowlist-or-denylist rule, empty lists are rejected — `paths: []` would
// match nothing and silently skip every push, a trap rather than a feature.
func validatePathFilter(where string, f *model.PathFilter) error {
	if f == nil {
		return nil
	}
	if f.Paths != nil && f.Ignore != nil {
		return fmt.Errorf("%s cannot set both `paths` and `paths-ignore`", where)
	}
	for key, patterns := range map[string][]string{"paths": f.Paths, "paths-ignore": f.Ignore} {
		if err := validatePatterns(where, key, patterns); err != nil {
			return err
		}
	}
	return nil
}

// validateTagFilter checks one `tags`/`tags-ignore` declaration. Tag patterns
// are the same glob syntax and carry the same bounds as path patterns, so only
// the both-keys rule and the key names differ.
func validateTagFilter(where string, f *model.TagFilter) error {
	if f == nil {
		return nil
	}
	if f.Tags != nil && f.Ignore != nil {
		return fmt.Errorf("%s cannot set both `tags` and `tags-ignore`", where)
	}
	for key, patterns := range map[string][]string{"tags": f.Tags, "tags-ignore": f.Ignore} {
		if err := validatePatterns(where, key, patterns); err != nil {
			return err
		}
	}
	return nil
}

// validatePatterns bounds one declared glob list. A nil list is an absent key;
// an empty one is rejected, since `paths: []` or `tags: []` matches nothing and
// would silently skip every event — a trap rather than a feature.
func validatePatterns(where, key string, patterns []string) error {
	if patterns == nil {
		return nil
	}
	if len(patterns) == 0 {
		return fmt.Errorf("%s `%s` must list at least one pattern", where, key)
	}
	if len(patterns) > maxPathPatterns {
		return fmt.Errorf("%s `%s` has too many patterns: %d (max %d)", where, key, len(patterns), maxPathPatterns)
	}
	for _, p := range patterns {
		if p == "" {
			return fmt.Errorf("%s `%s` contains an empty pattern", where, key)
		}
		if len(p) > maxPathPatternLen {
			return fmt.Errorf("%s `%s` pattern is too long: %d characters (max %d)", where, key, len(p), maxPathPatternLen)
		}
	}
	return nil
}

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
	// The same rule applies to `paths`/`paths-ignore` — which are further
	// restricted to push: an MR's changed set needs a merge base against the
	// target branch, history the shallow checkout deliberately avoids.
	for _, tr := range []struct {
		key string
		f   *model.Trigger
	}{{"push", wf.On.Push}, {"merge_request", wf.On.MergeRequest}} {
		if tr.f == nil {
			continue
		}
		if tr.f.Branches != nil && tr.f.Ignore != nil {
			return fmt.Errorf("`on.%s` cannot set both `branches` and `branches-ignore`", tr.key)
		}
		if tr.f.Paths != nil && tr.key != "push" {
			return fmt.Errorf("`on.%s` does not support `paths`/`paths-ignore` — path filters apply to push events only", tr.key)
		}
		if tr.f.Tags != nil && tr.key != "push" {
			return fmt.Errorf("`on.%s` does not support `tags`/`tags-ignore` — a merge request has no tag", tr.key)
		}
		if err := validatePathFilter(fmt.Sprintf("`on.%s`", tr.key), tr.f.Paths); err != nil {
			return err
		}
		if err := validateTagFilter(fmt.Sprintf("`on.%s`", tr.key), tr.f.Tags); err != nil {
			return err
		}
	}
	if wf.Concurrency != nil && len(wf.Concurrency.Group) > maxGroupLen {
		return fmt.Errorf("`concurrency.group` is too long: %d characters (max %d)", len(wf.Concurrency.Group), maxGroupLen)
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
		if err := validatePathFilter(fmt.Sprintf("job %q", name), job.PathFilter); err != nil {
			return err
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

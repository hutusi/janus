package pipeline

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hutusi/janus/internal/model"
)

// validate performs all semantic checks on an already-decoded workflow. Jobs are
// visited in sorted order so error messages are deterministic.
func validate(wf *model.Workflow) error {
	if strings.TrimSpace(wf.Name) == "" {
		return errors.New("`name` is required")
	}
	if wf.On.Push == nil && wf.On.MergeRequest == nil {
		return errors.New("`on` must declare at least one of `push` or `merge_request`")
	}
	if len(wf.Jobs) == 0 {
		return errors.New("at least one job is required under `jobs`")
	}

	for _, name := range sortedJobNames(wf) {
		job := wf.Jobs[name]
		if len(job.Steps) == 0 {
			return fmt.Errorf("job %q: at least one step is required", name)
		}
		for i, s := range job.Steps {
			if strings.TrimSpace(s.Run) == "" {
				return fmt.Errorf("job %q step %d: `run` is required and cannot be empty", name, i+1)
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

package engine

import (
	"fmt"
	"sort"

	"github.com/hutusi/janus/internal/model"
)

// graph is the job dependency DAG derived from a workflow's `needs` edges. It
// carries exactly what the scheduler needs: a stable job order, each job's
// number of unmet dependencies (indegree), and, for each job, the jobs that
// depend on it (dependents).
type graph struct {
	order      []string            // all job names, sorted (deterministic scheduling)
	indegree   map[string]int      // job -> count of jobs it needs
	dependents map[string][]string // job -> jobs that need it (sorted)
}

// buildGraph constructs the DAG. The workflow is expected to be already
// validated (acyclic, needs resolve), but buildGraph re-checks for cycles via
// Kahn's algorithm as a defense-in-depth guard so the scheduler can never
// deadlock waiting on an unsatisfiable dependency.
func buildGraph(wf *model.Workflow) (*graph, error) {
	g := &graph{
		indegree:   make(map[string]int, len(wf.Jobs)),
		dependents: make(map[string][]string),
	}
	for name := range wf.Jobs {
		g.order = append(g.order, name)
	}
	sort.Strings(g.order)

	for _, name := range g.order {
		job := wf.Jobs[name]
		g.indegree[name] = len(job.Needs)
		for _, dep := range job.Needs {
			g.dependents[dep] = append(g.dependents[dep], name)
		}
	}
	for dep := range g.dependents {
		sort.Strings(g.dependents[dep])
	}

	if err := g.assertAcyclic(); err != nil {
		return nil, err
	}
	return g, nil
}

// assertAcyclic runs Kahn's algorithm over a copy of the indegrees: if fewer
// than all jobs can be ordered, a cycle remains.
func (g *graph) assertAcyclic() error {
	indeg := make(map[string]int, len(g.indegree))
	for k, v := range g.indegree {
		indeg[k] = v
	}
	queue := make([]string, 0, len(indeg))
	for _, name := range g.order {
		if indeg[name] == 0 {
			queue = append(queue, name)
		}
	}
	processed := 0
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		processed++
		for _, dep := range g.dependents[name] {
			indeg[dep]--
			if indeg[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}
	if processed != len(g.order) {
		return fmt.Errorf("job dependency cycle detected among %d jobs", len(g.order)-processed)
	}
	return nil
}

package engine

import (
	"testing"

	"github.com/hutusi/janus/internal/model"
)

func TestBuildGraph(t *testing.T) {
	// a -> b, a -> c, b -> d, c -> d   (diamond)
	wf := &model.Workflow{Jobs: map[string]*model.Job{
		"a": {Name: "a"},
		"b": {Name: "b", Needs: []string{"a"}},
		"c": {Name: "c", Needs: []string{"a"}},
		"d": {Name: "d", Needs: []string{"b", "c"}},
	}}
	g, err := buildGraph(wf)
	if err != nil {
		t.Fatalf("buildGraph: %v", err)
	}

	if got := g.order; len(got) != 4 || got[0] != "a" || got[3] != "d" {
		t.Errorf("order = %v, want sorted [a b c d]", got)
	}
	wantIndeg := map[string]int{"a": 0, "b": 1, "c": 1, "d": 2}
	for name, want := range wantIndeg {
		if g.indegree[name] != want {
			t.Errorf("indegree[%s] = %d, want %d", name, g.indegree[name], want)
		}
	}
	if deps := g.dependents["a"]; len(deps) != 2 || deps[0] != "b" || deps[1] != "c" {
		t.Errorf("dependents[a] = %v, want [b c]", deps)
	}
	if deps := g.dependents["b"]; len(deps) != 1 || deps[0] != "d" {
		t.Errorf("dependents[b] = %v, want [d]", deps)
	}
}

func TestBuildGraphCycleGuard(t *testing.T) {
	// Bypass pipeline validation to confirm the engine guards against cycles too.
	wf := &model.Workflow{Jobs: map[string]*model.Job{
		"a": {Name: "a", Needs: []string{"b"}},
		"b": {Name: "b", Needs: []string{"a"}},
	}}
	if _, err := buildGraph(wf); err == nil {
		t.Fatal("expected cycle error, got nil")
	}
}

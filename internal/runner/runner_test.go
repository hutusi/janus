package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hutusi/janus/internal/engine"
	"github.com/hutusi/janus/internal/model"
	"github.com/hutusi/janus/internal/store"
)

func TestSweepRemovesOrphanWorkspaces(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "run-abc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "keep-me"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	r := New(st, engine.New(st), root, ".janus/ci.yml", false, 1)
	if err := r.Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "run-abc")); !os.IsNotExist(err) {
		t.Error("run-* workspace should be swept")
	}
	if _, err := os.Stat(filepath.Join(root, "keep-me")); err != nil {
		t.Error("non run-* directory should be left alone")
	}
}

func TestMatches(t *testing.T) {
	pushMain := &model.Workflow{On: model.Triggers{Push: &model.BranchFilter{Branches: []string{"main"}}}}
	mrMain := &model.Workflow{On: model.Triggers{MergeRequest: &model.BranchFilter{Branches: []string{"main"}}}}
	pushAny := &model.Workflow{On: model.Triggers{Push: &model.BranchFilter{}}}

	tests := []struct {
		name string
		wf   *model.Workflow
		ev   model.Event
		want bool
	}{
		{"manual always matches", pushMain, model.Event{Kind: model.EventManual}, true},
		{"push on listed branch", pushMain, model.Event{Kind: model.EventPush, Branch: "main"}, true},
		{"push on other branch", pushMain, model.Event{Kind: model.EventPush, Branch: "dev"}, false},
		{"push when only MR declared", mrMain, model.Event{Kind: model.EventPush, Branch: "main"}, false},
		{"MR on target branch", mrMain, model.Event{Kind: model.EventMergeRequest, Branch: "main"}, true},
		{"MR on other branch", mrMain, model.Event{Kind: model.EventMergeRequest, Branch: "dev"}, false},
		{"empty filter matches any branch", pushAny, model.Event{Kind: model.EventPush, Branch: "whatever"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := matches(tc.wf, tc.ev); got != tc.want {
				t.Errorf("matches() = %v, want %v", got, tc.want)
			}
		})
	}
}

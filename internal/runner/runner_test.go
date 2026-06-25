package runner

import (
	"testing"

	"github.com/hutusi/janus/internal/model"
)

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

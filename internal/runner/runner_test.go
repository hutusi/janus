package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hutusi/janus/internal/allowlist"
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
	r := New(st, engine.New(st), Options{WSRoot: root, PipelinePath: ".janus/ci.yml", MaxRuns: 1})
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

func TestTriggerRejectsDisallowedRepo(t *testing.T) {
	root := t.TempDir()
	st := store.NewMemory()
	allow, _ := allowlist.New([]string{"https://allowed.example.com"})
	r := New(st, engine.New(st), Options{WSRoot: root, PipelinePath: ".janus/ci.yml", MaxRuns: 1, Allowlist: allow})

	_, err := r.Trigger(context.Background(), model.Event{
		Kind:    model.EventManual,
		RepoURL: "https://evil.example.com/x.git",
		Ref:     "refs/heads/main",
	})
	if !errors.Is(err, ErrRepoNotAllowed) {
		t.Fatalf("err = %v, want ErrRepoNotAllowed", err)
	}
	// Rejected before touching disk: no run-* workspace was created.
	entries, _ := filepath.Glob(filepath.Join(root, "run-*"))
	if len(entries) != 0 {
		t.Errorf("a workspace was created for a disallowed repo: %v", entries)
	}
}

func TestPipelineFile(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "evil.yml")
	def := ".janus/ci.yml"
	tests := []struct {
		name     string
		def      string
		override string
		want     string
		wantErr  bool
	}{
		{"empty falls back to default", def, "", filepath.FromSlash(".janus/ci.yml"), false},
		{"bare name resolves in the pipeline dir", def, "release.yml", filepath.FromSlash(".janus/release.yml"), false},
		{"subdirectory resolves in the pipeline dir", def, "nightly/build.yml", filepath.FromSlash(".janus/nightly/build.yml"), false},
		{"full path resolves under the dir, not from the root", def, ".janus/release.yml", filepath.FromSlash(".janus/.janus/release.yml"), false},
		{"escape from the pipeline dir rejected", def, "../examples/evil.yml", "", true},
		{"nested escape rejected", def, "../../etc/evil.yml", "", true},
		{"the directory itself rejected", def, ".", "", true},
		{"absolute path rejected", def, abs, "", true},
		{"root-level default resolves from the root", "janus.yml", "ci/other.yml", filepath.FromSlash("ci/other.yml"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pipelineFile(tc.def, model.Event{PipelinePath: tc.override})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("pipelineFile(%q, %q) = %q, want error", tc.def, tc.override, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("pipelineFile(%q, %q): %v", tc.def, tc.override, err)
			}
			if got != tc.want {
				t.Errorf("pipelineFile(%q, %q) = %q, want %q", tc.def, tc.override, got, tc.want)
			}
		})
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

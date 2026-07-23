package pipeline

import "github.com/hutusi/janus/internal/model"

// The raw* types mirror the supported pipeline YAML exactly. Decoding uses
// yaml.Decoder.KnownFields(true), so any key absent from these structs — such
// as if, matrix, strategy, uses, with, runs-on, container — is a decode error
// rather than being silently dropped. This is the structural half of "anything
// beyond the spec is a validation error, not a feature".
//
// Env values are map[string]string, so user-chosen variable names (even ones
// like "if" or "uses") never collide with the strict key checking above.

type rawWorkflow struct {
	Name        string            `yaml:"name"`
	On          rawTriggers       `yaml:"on"`
	Concurrency *rawConcurrency   `yaml:"concurrency"`
	Env         map[string]string `yaml:"env"`
	Jobs        map[string]rawJob `yaml:"jobs"`
}

// rawConcurrency accepts only the mapping form; GitHub's bare-string shorthand
// (`concurrency: my-group`) is a decode error, keeping the spec closed. A null
// value (`concurrency:` with nothing under it) decodes to a nil pointer and is
// treated as absent.
type rawConcurrency struct {
	Group            string `yaml:"group"`
	CancelInProgress bool   `yaml:"cancel-in-progress"`
}

type rawTriggers struct {
	Push         *rawTrigger `yaml:"push"`
	MergeRequest *rawTrigger `yaml:"merge_request"`
}

type rawTrigger struct {
	Branches    []string `yaml:"branches"`
	Ignore      []string `yaml:"branches-ignore"`
	Paths       []string `yaml:"paths"`
	PathsIgnore []string `yaml:"paths-ignore"`
}

type rawJob struct {
	Needs       []string          `yaml:"needs"`
	Branches    []string          `yaml:"branches"`
	Ignore      []string          `yaml:"branches-ignore"`
	Paths       []string          `yaml:"paths"`
	PathsIgnore []string          `yaml:"paths-ignore"`
	WorkingDir  string            `yaml:"working-directory"`
	Env         map[string]string `yaml:"env"`
	Steps       []rawStep         `yaml:"steps"`
}

type rawStep struct {
	Run        string            `yaml:"run"`
	WorkingDir string            `yaml:"working-directory"`
	Shell      string            `yaml:"shell"`
	Env        map[string]string `yaml:"env"`
}

// toModel converts the decoded YAML into the immutable domain Workflow,
// populating each job's Name from its map key.
func (r *rawWorkflow) toModel() *model.Workflow {
	wf := &model.Workflow{
		Name: r.Name,
		Env:  r.Env,
		Jobs: make(map[string]*model.Job, len(r.Jobs)),
	}
	wf.On.Push = r.On.Push.toModel()
	wf.On.MergeRequest = r.On.MergeRequest.toModel()
	if r.Concurrency != nil {
		wf.Concurrency = &model.Concurrency{
			Group:            r.Concurrency.Group,
			CancelInProgress: r.Concurrency.CancelInProgress,
		}
	}
	for name, rj := range r.Jobs {
		steps := make([]model.Step, len(rj.Steps))
		for i, rs := range rj.Steps {
			steps[i] = model.Step{Run: rs.Run, WorkingDir: rs.WorkingDir, Shell: rs.Shell, Env: rs.Env}
		}
		job := &model.Job{
			Name:       name,
			Needs:      rj.Needs,
			WorkingDir: rj.WorkingDir,
			Env:        rj.Env,
			Steps:      steps,
		}
		// Preserve key presence (nil vs non-nil), like the on: filters, so
		// validation can reject declaring both keys even when empty.
		if rj.Branches != nil || rj.Ignore != nil {
			job.Filter = &model.BranchFilter{Branches: rj.Branches, Ignore: rj.Ignore}
		}
		job.PathFilter = pathFilter(rj.Paths, rj.PathsIgnore)
		wf.Jobs[name] = job
	}
	return wf
}

func (t *rawTrigger) toModel() *model.Trigger {
	if t == nil {
		return nil
	}
	return &model.Trigger{
		BranchFilter: model.BranchFilter{Branches: t.Branches, Ignore: t.Ignore},
		Paths:        pathFilter(t.Paths, t.PathsIgnore),
	}
}

// pathFilter builds a PathFilter only when a key was present, preserving
// nil-ness like the branch filters.
func pathFilter(paths, ignore []string) *model.PathFilter {
	if paths == nil && ignore == nil {
		return nil
	}
	return &model.PathFilter{Paths: paths, Ignore: ignore}
}

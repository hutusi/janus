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
	Name string            `yaml:"name"`
	On   rawTriggers       `yaml:"on"`
	Env  map[string]string `yaml:"env"`
	Jobs map[string]rawJob `yaml:"jobs"`
}

type rawTriggers struct {
	Push         *rawBranchFilter `yaml:"push"`
	MergeRequest *rawBranchFilter `yaml:"merge_request"`
}

type rawBranchFilter struct {
	Branches []string `yaml:"branches"`
	Ignore   []string `yaml:"branches-ignore"`
}

type rawJob struct {
	Needs      []string          `yaml:"needs"`
	Branches   []string          `yaml:"branches"`
	Ignore     []string          `yaml:"branches-ignore"`
	WorkingDir string            `yaml:"working-directory"`
	Env        map[string]string `yaml:"env"`
	Steps      []rawStep         `yaml:"steps"`
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
	if r.On.Push != nil {
		wf.On.Push = &model.BranchFilter{Branches: r.On.Push.Branches, Ignore: r.On.Push.Ignore}
	}
	if r.On.MergeRequest != nil {
		wf.On.MergeRequest = &model.BranchFilter{Branches: r.On.MergeRequest.Branches, Ignore: r.On.MergeRequest.Ignore}
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
		wf.Jobs[name] = job
	}
	return wf
}

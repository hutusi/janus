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
}

type rawJob struct {
	Needs []string          `yaml:"needs"`
	Env   map[string]string `yaml:"env"`
	Steps []rawStep         `yaml:"steps"`
}

type rawStep struct {
	Run        string            `yaml:"run"`
	WorkingDir string            `yaml:"working-directory"`
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
		wf.On.Push = &model.BranchFilter{Branches: r.On.Push.Branches}
	}
	if r.On.MergeRequest != nil {
		wf.On.MergeRequest = &model.BranchFilter{Branches: r.On.MergeRequest.Branches}
	}
	for name, rj := range r.Jobs {
		steps := make([]model.Step, len(rj.Steps))
		for i, rs := range rj.Steps {
			steps[i] = model.Step{Run: rs.Run, WorkingDir: rs.WorkingDir, Env: rs.Env}
		}
		wf.Jobs[name] = &model.Job{
			Name:  name,
			Needs: rj.Needs,
			Env:   rj.Env,
			Steps: steps,
		}
	}
	return wf
}

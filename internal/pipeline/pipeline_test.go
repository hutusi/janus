package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hutusi/janus/internal/model"
)

// --- Valid pipelines ---------------------------------------------------------

func TestParseValid(t *testing.T) {
	const src = `
name: ci
on:
  push: { branches: [main, release] }
  merge_request: { branches: [main] }
env:
  CI: "true"
jobs:
  build:
    env:
      STAGE: build
    steps:
      - run: npm ci
        working-directory: ./app
  test:
    needs: [build]
    steps:
      - run: npm test
`
	wf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wf.Name != "ci" {
		t.Errorf("name = %q, want ci", wf.Name)
	}
	if wf.On.Push == nil || len(wf.On.Push.Branches) != 2 {
		t.Errorf("push trigger = %+v, want 2 branches", wf.On.Push)
	}
	if wf.On.MergeRequest == nil {
		t.Error("merge_request trigger missing")
	}
	if wf.Env["CI"] != "true" {
		t.Errorf("env CI = %q, want true", wf.Env["CI"])
	}
	build, ok := wf.Jobs["build"]
	if !ok {
		t.Fatal("build job missing")
	}
	if build.Name != "build" {
		t.Errorf("job Name = %q, want build (should be filled from key)", build.Name)
	}
	if build.Steps[0].WorkingDir != "./app" {
		t.Errorf("working-directory = %q, want ./app", build.Steps[0].WorkingDir)
	}
	if build.Env["STAGE"] != "build" {
		t.Errorf("job env STAGE = %q, want build", build.Env["STAGE"])
	}
	if test := wf.Jobs["test"]; test == nil || len(test.Needs) != 1 || test.Needs[0] != "build" {
		t.Errorf("test.needs = %+v, want [build]", test)
	}
}

func TestParseRejectsOversizedPipelines(t *testing.T) {
	var manyJobs strings.Builder
	manyJobs.WriteString("name: ci\non: { push: {} }\njobs:\n")
	for i := 0; i < maxJobs+1; i++ {
		fmt.Fprintf(&manyJobs, "  j%d:\n    steps:\n      - run: echo hi\n", i)
	}

	var manySteps strings.Builder
	manySteps.WriteString("name: ci\non: { push: {} }\njobs:\n  build:\n    steps:\n")
	for i := 0; i < maxStepsPerJob+1; i++ {
		manySteps.WriteString("      - run: echo hi\n")
	}

	longName := "name: ci\non: { push: {} }\njobs:\n  " + strings.Repeat("j", maxJobNameLen+1) + ":\n    steps:\n      - run: echo hi\n"
	longCmd := "name: ci\non: { push: {} }\njobs:\n  build:\n    steps:\n      - run: " + strings.Repeat("x", maxCommandBytes+1) + "\n"
	longGroup := "name: ci\non: { push: {} }\nconcurrency: { group: " + strings.Repeat("g", maxGroupLen+1) + " }\njobs:\n  build:\n    steps:\n      - run: echo hi\n"

	cases := map[string]struct{ src, want string }{
		"too many jobs":  {manyJobs.String(), "too many jobs"},
		"too many steps": {manySteps.String(), "too many steps"},
		"long job name":  {longName, "job name too long"},
		"long command":   {longCmd, "`run` is too long"},
		"long group":     {longGroup, "`concurrency.group` is too long"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	// A generous-but-in-limits pipeline still parses.
	var ok strings.Builder
	ok.WriteString("name: ci\non: { push: {} }\njobs:\n  build:\n    steps:\n")
	for i := 0; i < 50; i++ {
		ok.WriteString("      - run: echo hi\n")
	}
	if _, err := Parse([]byte(ok.String())); err != nil {
		t.Fatalf("a 50-step pipeline should be valid: %v", err)
	}
}

func TestReadFileSizeCap(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "ok.yml")
	if err := os.WriteFile(small, []byte("name: ci\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(small); err != nil {
		t.Errorf("a small file should read: %v", err)
	}

	big := filepath.Join(dir, "big.yml")
	if err := os.WriteFile(big, make([]byte, MaxFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(big); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("a >MaxFileBytes file should be rejected, got %v", err)
	}
}

func TestParseAcceptsFullJobNameCharset(t *testing.T) {
	const src = `
name: ci
on: { push: {} }
jobs:
  build_X-1:
    steps:
      - run: echo hi
`
	if _, err := Parse([]byte(src)); err != nil {
		t.Fatalf("letters/digits/'-'/'_' job names should be accepted: %v", err)
	}
}

func TestParsePushOnlyMatchesAllBranchesWhenEmpty(t *testing.T) {
	const src = `
name: ci
on:
  push: {}
jobs:
  a:
    steps:
      - run: echo hi
`
	wf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wf.On.Push == nil {
		t.Fatal("push should be present even when empty")
	}
	if !wf.On.Push.Matches("anything") {
		t.Error("empty branch filter should match all branches")
	}
	if wf.On.MergeRequest != nil {
		t.Error("merge_request should be nil when not declared")
	}
}

func TestParseBranchesIgnore(t *testing.T) {
	const src = `
name: ci
on:
  push: { branches-ignore: [master] }
jobs:
  a:
    steps:
      - run: echo hi
`
	wf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wf.On.Push.Matches("master") {
		t.Error("ignored branch should not match")
	}
	if !wf.On.Push.Matches("feature/x") {
		t.Error("non-ignored branch should match")
	}
}

func TestParseJobBranchFilter(t *testing.T) {
	const src = `
name: ci
on:
  push: {}
jobs:
  build:
    steps:
      - run: echo build
  deploy:
    needs: [build]
    branches: [master, main]
    steps:
      - run: echo deploy
  nightly:
    branches-ignore: [main]
    steps:
      - run: echo nightly
`
	wf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wf.Jobs["build"].Filter != nil {
		t.Error("job without branches keys should have a nil filter")
	}
	deploy := wf.Jobs["deploy"].Filter
	if deploy == nil || !deploy.Matches("main") || deploy.Matches("feature/x") {
		t.Errorf("deploy filter = %+v, want an allowlist matching only master/main", deploy)
	}
	nightly := wf.Jobs["nightly"].Filter
	if nightly == nil || nightly.Matches("main") || !nightly.Matches("feature/x") {
		t.Errorf("nightly filter = %+v, want a denylist excluding main", nightly)
	}
}

func TestParseJobWorkingDir(t *testing.T) {
	const src = `
name: ci
on:
  push: {}
jobs:
  build:
    working-directory: ./app
    steps:
      - run: npm ci
      - run: npm test
        working-directory: ./app/tests
`
	wf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := wf.Jobs["build"].WorkingDir; got != "./app" {
		t.Errorf("job working-directory = %q, want ./app", got)
	}
	if got := wf.Jobs["build"].Steps[1].WorkingDir; got != "./app/tests" {
		t.Errorf("step working-directory = %q, want ./app/tests", got)
	}
}

func TestParseValidFixtureFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "pipelines", "valid.yml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("valid.yml should parse, got: %v", err)
	}
}

// --- Rejected pipelines ------------------------------------------------------

func TestParseConcurrency(t *testing.T) {
	const src = `
name: deploy
on: { push: {} }
concurrency:
  group: deploy-${{ branch }}
  cancel-in-progress: true
jobs:
  a:
    steps:
      - run: echo hi
`
	wf, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wf.Concurrency == nil {
		t.Fatal("Concurrency = nil, want populated")
	}
	if wf.Concurrency.Group != "deploy-${{ branch }}" {
		t.Errorf("group = %q, want the raw template", wf.Concurrency.Group)
	}
	if !wf.Concurrency.CancelInProgress {
		t.Error("cancel-in-progress = false, want true")
	}

	const jobs = "jobs: { a: { steps: [{ run: echo hi }] } }\n"
	cases := map[string]struct {
		src  string
		want *model.Concurrency
	}{
		"absent key": {"name: ci\non: { push: {} }\n" + jobs, nil},
		// A bare `concurrency:` decodes to null — treated as absent, and the
		// docs call out the footgun.
		"null value":    {"name: ci\non: { push: {} }\nconcurrency:\n" + jobs, nil},
		"empty mapping": {"name: ci\non: { push: {} }\nconcurrency: {}\n" + jobs, &model.Concurrency{}},
		"cancel only":   {"name: ci\non: { push: {} }\nconcurrency: { cancel-in-progress: true }\n" + jobs, &model.Concurrency{CancelInProgress: true}},
		"ref event env tokens": {
			"name: ci\non: { push: {} }\nenv: { STAGE: prod }\nconcurrency: { group: \"${{ ref }}-${{ event }}-${{ env.STAGE }}\" }\n" + jobs,
			&model.Concurrency{Group: "${{ ref }}-${{ event }}-${{ env.STAGE }}"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			wf, err := Parse([]byte(tc.src))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			switch {
			case tc.want == nil:
				if wf.Concurrency != nil {
					t.Fatalf("Concurrency = %+v, want nil", wf.Concurrency)
				}
			case wf.Concurrency == nil:
				t.Fatalf("Concurrency = nil, want %+v", tc.want)
			case *wf.Concurrency != *tc.want:
				t.Fatalf("Concurrency = %+v, want %+v", wf.Concurrency, tc.want)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name      string
		src       string
		wantInErr string
	}{
		{
			name:      "empty document",
			src:       "",
			wantInErr: "empty pipeline",
		},
		{
			name: "uses (third-party action)",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    steps:
      - uses: actions/checkout@v4
`,
			wantInErr: "uses:",
		},
		{
			name: "if conditional",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    if: success()
    steps:
      - run: echo hi
`,
			wantInErr: "if:",
		},
		{
			name: "branches and branches-ignore on push",
			src: `
name: ci
on: { push: { branches: [main], branches-ignore: [dev] } }
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "`on.push` cannot set both",
		},
		{
			name: "branches and branches-ignore on merge_request, empty list still counts",
			src: `
name: ci
on: { merge_request: { branches: [], branches-ignore: [main] } }
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "`on.merge_request` cannot set both",
		},
		{
			name: "tags and tags-ignore on push",
			src: `
name: ci
on: { push: { tags: ["v*"], tags-ignore: ["*-rc*"] } }
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "`on.push` cannot set both `tags` and `tags-ignore`",
		},
		{
			name: "tags on merge_request",
			src: `
name: ci
on: { merge_request: { tags: ["v*"] } }
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "a merge request has no tag",
		},
		{
			name: "empty tags list",
			src: `
name: ci
on: { push: { tags: [] } }
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "`tags` must list at least one pattern",
		},
		{
			name: "empty pattern in tags",
			src: `
name: ci
on: { push: { tags: ["v*", ""] } }
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "contains an empty pattern",
		},
		{
			// Tag filters are a trigger-level key: a job has no tag of its own
			// to filter on, so the strict decode must still reject it there.
			name: "tags on a job",
			src: `
name: ci
on: { push: { tags: ["v*"] } }
jobs:
  a:
    tags: ["v*"]
    steps:
      - run: echo hi
`,
			wantInErr: "tags",
		},
		{
			name: "paths and paths-ignore on push",
			src: `
name: ci
on: { push: { paths: [docs/**], paths-ignore: [README.md] } }
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "`on.push` cannot set both `paths` and `paths-ignore`",
		},
		{
			name: "paths on merge_request",
			src: `
name: ci
on: { merge_request: { paths: [docs/**] } }
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "path filters apply to push events only",
		},
		{
			name: "empty paths list",
			src: `
name: ci
on: { push: { paths: [] } }
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "must list at least one pattern",
		},
		{
			name: "paths and paths-ignore on a job",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    paths: [docs/**]
    paths-ignore: [README.md]
    steps:
      - run: echo hi
`,
			wantInErr: "job \"a\" cannot set both `paths` and `paths-ignore`",
		},
		{
			name: "empty pattern in job paths",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    paths: ["docs/**", ""]
    steps:
      - run: echo hi
`,
			wantInErr: "contains an empty pattern",
		},
		{
			name: "unsupported interpolation in job working-directory",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    working-directory: ${{ nope }}
    steps:
      - run: echo hi
`,
			wantInErr: "unsupported interpolation",
		},
		{
			name: "branches and branches-ignore on a job",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    branches: [main]
    branches-ignore: [dev]
    steps:
      - run: echo hi
`,
			wantInErr: "job \"a\" cannot set both",
		},
		{
			name: "matrix/strategy",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    strategy:
      matrix: { go: [1, 2] }
    steps:
      - run: echo hi
`,
			wantInErr: "strategy:",
		},
		{
			name: "runs-on",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`,
			wantInErr: "runs-on:",
		},
		{
			name: "step with: block",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    steps:
      - run: echo hi
        with: { foo: bar }
`,
			wantInErr: "with:",
		},
		{
			name: "unknown top-level key",
			src: `
name: ci
on: { push: {} }
permissions: write-all
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "permissions:",
		},
		{
			name: "no trigger",
			src: `
name: ci
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "at least one of `push` or `merge_request`",
		},
		{
			name: "missing name",
			src: `
on: { push: {} }
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "`name` is required",
		},
		{
			name: "no jobs",
			src: `
name: ci
on: { push: {} }
jobs: {}
`,
			wantInErr: "at least one job is required",
		},
		{
			name: "job with no steps",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    steps: []
`,
			wantInErr: "at least one step is required",
		},
		{
			name: "empty run",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    steps:
      - run: "   "
`,
			wantInErr: "`run` is required",
		},
		{
			name: "needs unknown job",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    needs: [ghost]
    steps:
      - run: echo hi
`,
			wantInErr: `needs unknown job "ghost"`,
		},
		{
			name: "self dependency",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    needs: [a]
    steps:
      - run: echo hi
`,
			wantInErr: "cannot depend on itself",
		},
		{
			name: "cycle",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    needs: [b]
    steps:
      - run: echo hi
  b:
    needs: [a]
    steps:
      - run: echo hi
`,
			wantInErr: "cycle",
		},
		{
			name: "interpolation: secrets",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    steps:
      - run: deploy --token ${{ secrets.TOKEN }}
`,
			wantInErr: "unsupported interpolation",
		},
		{
			name: "interpolation: expression with operator",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    steps:
      - run: echo ${{ env.X == 'y' }}
`,
			wantInErr: "unsupported interpolation",
		},
		{
			name: "interpolation: unknown context",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    steps:
      - run: echo ${{ github.event.number }}
`,
			wantInErr: "unsupported interpolation",
		},
		{
			name: "interpolation: unterminated placeholder in run",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    steps:
      - run: echo ${{ branch
`,
			wantInErr: "unterminated ${{",
		},
		{
			name: "interpolation: unterminated placeholder in env",
			src: `
name: ci
on: { push: {} }
env: { REF: "${{ ref" }
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "unterminated ${{",
		},
		{
			name: "job name with slash",
			src: `
name: ci
on: { push: {} }
jobs:
  a/b:
    steps:
      - run: echo hi
`,
			wantInErr: "letters, digits",
		},
		{
			name: "job name with space",
			src: `
name: ci
on: { push: {} }
jobs:
  "a b":
    steps:
      - run: echo hi
`,
			wantInErr: "letters, digits",
		},
		{
			name: "job name with dot",
			src: `
name: ci
on: { push: {} }
jobs:
  a.b:
    steps:
      - run: echo hi
`,
			wantInErr: "letters, digits",
		},
		{
			name: "concurrency group with sha",
			src: `
name: ci
on: { push: {} }
concurrency: { group: "ci-${{ sha }}" }
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "would make every run its own group",
		},
		{
			name: "concurrency group with short_sha",
			src: `
name: ci
on: { push: {} }
concurrency: { group: "ci-${{ short_sha }}" }
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "${{ short_sha }} would make every run its own group",
		},
		{
			name: "concurrency group with unknown token",
			src: `
name: ci
on: { push: {} }
concurrency: { group: "${{ github.run_id }}" }
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "unsupported interpolation",
		},
		{
			name: "concurrency group with unterminated placeholder",
			src: `
name: ci
on: { push: {} }
concurrency: { group: "ci-${{ branch" }
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "unterminated ${{",
		},
		{
			name: "concurrency with snake_case typo",
			src: `
name: ci
on: { push: {} }
concurrency: { group: ci, cancel_in_progress: true }
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: `unsupported key "cancel_in_progress"`,
		},
		{
			name: "concurrency string shorthand",
			src: `
name: ci
on: { push: {} }
concurrency: deploy
jobs:
  a:
    steps:
      - run: echo hi
`,
			wantInErr: "cannot unmarshal",
		},
		{
			name: "concurrency on a job",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    concurrency: { group: ci }
    steps:
      - run: echo hi
`,
			wantInErr: "only supported at the workflow (top) level",
		},
		{
			name: "multiple YAML documents",
			src: `
name: ci
on: { push: {} }
jobs:
  a:
    steps:
      - run: echo hi
---
name: second
`,
			wantInErr: "multiple YAML documents",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantInErr)
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantInErr)
			}
		})
	}
}

// --- Interpolation (runtime substitution) ------------------------------------

func TestInterpolate(t *testing.T) {
	ctx := Context{
		Env:      map[string]string{"CI": "true", "STAGE": "build"},
		Ref:      "refs/heads/main",
		SHA:      "abcdef1234567890",
		ShortSHA: "abcdef1",
		Branch:   "main",
		Event:    "push",
	}
	tagCtx := Context{Ref: "refs/tags/v1.0.0", Tag: "v1.0.0", Event: "push"}
	if got, err := tagCtx.Interpolate("release ${{ tag }} from ${{ ref }}", 1<<20); err != nil || got != "release v1.0.0 from refs/tags/v1.0.0" {
		t.Errorf("tag interpolation = %q, %v", got, err)
	}
	tests := []struct {
		in, want string
	}{
		{"plain", "plain"},
		{"echo ${{ branch }}", "echo main"},
		{"sha=${{ sha }} short=${{ short_sha }}", "sha=abcdef1234567890 short=abcdef1"},
		{"on ${{ event }} ref ${{ ref }}", "on push ref refs/heads/main"},
		{"CI=${{ env.CI }} STAGE=${{ env.STAGE }}", "CI=true STAGE=build"},
		{"undef=${{ env.NOPE }}.", "undef=."},
		{"${{branch}}-${{short_sha}}", "main-abcdef1"}, // no inner spaces
		// A branch push has no tag: ${{ tag }} resolves empty rather than
		// failing, the same way an undefined env var does.
		{"tag=[${{ tag }}]", "tag=[]"},
	}
	for _, tc := range tests {
		got, err := ctx.Interpolate(tc.in, 1<<20)
		if err != nil {
			t.Errorf("Interpolate(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Interpolate(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestInterpolateBounded(t *testing.T) {
	ctx := Context{Env: map[string]string{"BIG": strings.Repeat("x", 1000)}}
	// Many references to a large value expand past the cap → error, not OOM.
	cmd := strings.Repeat("${{ env.BIG }}", 100) // ~100 KiB expanded
	if _, err := ctx.Interpolate(cmd, 4096); err == nil {
		t.Error("interpolation exceeding max should error")
	}
	// A literal string longer than max also errors (the tail is counted).
	if _, err := ctx.Interpolate(strings.Repeat("a", 5000), 4096); err == nil {
		t.Error("a literal longer than max should error")
	}
	// Comfortably under the cap → no error, correct output.
	if got, err := ctx.Interpolate("hi ${{ env.BIG }}", 1<<20); err != nil || got != "hi "+strings.Repeat("x", 1000) {
		t.Errorf("under-cap interpolation = %q, %v", got, err)
	}
}

func TestParsePathFilters(t *testing.T) {
	wf, err := Parse([]byte(`
name: ci
on:
  push:
    paths: ["docs/**", "*.md"]
jobs:
  build:
    paths-ignore: [README.md]
    steps:
      - run: echo hi
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pf := wf.On.Push.Paths
	if pf == nil || len(pf.Paths) != 2 || pf.Paths[0] != "docs/**" {
		t.Fatalf("on.push paths filter = %+v, want the two declared patterns", pf)
	}
	jf := wf.Jobs["build"].PathFilter
	if jf == nil || jf.Ignore == nil || len(jf.Ignore) != 1 || jf.Paths != nil {
		t.Fatalf("job paths-ignore filter = %+v, want ignore-only", jf)
	}
	// No paths keys → nil filters, so runs are unconstrained.
	wf2, err := Parse([]byte("name: ci\non: { push: {} }\njobs:\n  a:\n    steps:\n      - run: echo hi\n"))
	if err != nil {
		t.Fatalf("parse plain: %v", err)
	}
	if wf2.On.Push.Paths != nil || wf2.Jobs["a"].PathFilter != nil {
		t.Error("absent paths keys must leave filters nil")
	}
}

func TestParseTagFilters(t *testing.T) {
	wf, err := Parse([]byte(`
name: release
on:
  push:
    tags: ["v*", "release-*"]
jobs:
  build:
    steps:
      - run: echo ${{ tag }}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tf := wf.On.Push.Tags
	if tf == nil || len(tf.Tags) != 2 || tf.Tags[0] != "v*" || tf.Ignore != nil {
		t.Fatalf("on.push tags filter = %+v, want the two declared patterns", tf)
	}

	// tags-ignore alone is the denylist form.
	wf2, err := Parse([]byte("name: ci\non: { push: { tags-ignore: [\"*-rc*\"] } }\njobs:\n  a:\n    steps:\n      - run: echo hi\n"))
	if err != nil {
		t.Fatalf("parse tags-ignore: %v", err)
	}
	if tf := wf2.On.Push.Tags; tf == nil || tf.Tags != nil || len(tf.Ignore) != 1 {
		t.Fatalf("on.push tags filter = %+v, want ignore-only", tf)
	}

	// The nil-ness is what makes a bare `on: push:` branches-only, so an
	// absent key must never materialize a filter.
	wf3, err := Parse([]byte("name: ci\non: { push: {} }\njobs:\n  a:\n    steps:\n      - run: echo hi\n"))
	if err != nil {
		t.Fatalf("parse plain: %v", err)
	}
	if wf3.On.Push.Tags != nil {
		t.Error("an absent tags key must leave the filter nil — that is the opt-in signal")
	}
}

func TestParseTagPatternCaps(t *testing.T) {
	pipe := func(patterns []string) string {
		return "name: ci\non: { push: { tags: [\"" + strings.Join(patterns, "\", \"") + "\"] } }\njobs:\n  a:\n    steps:\n      - run: echo hi\n"
	}
	many := func(n int) []string {
		ps := make([]string, n)
		for i := range ps {
			ps[i] = fmt.Sprintf("v%d.*", i)
		}
		return ps
	}

	if _, err := Parse([]byte(pipe(many(maxPathPatterns)))); err != nil {
		t.Errorf("%d patterns should be accepted: %v", maxPathPatterns, err)
	}
	if _, err := Parse([]byte(pipe(many(maxPathPatterns + 1)))); err == nil || !strings.Contains(err.Error(), "too many patterns") {
		t.Errorf("%d patterns: err = %v, want too-many-patterns", maxPathPatterns+1, err)
	}
	if _, err := Parse([]byte(pipe([]string{strings.Repeat("v", maxPathPatternLen+1)}))); err == nil || !strings.Contains(err.Error(), "pattern is too long") {
		t.Errorf("over-long pattern: err = %v, want pattern-too-long", err)
	}
}

func TestParsePathPatternCaps(t *testing.T) {
	pipe := func(patterns []string) string {
		return "name: ci\non: { push: { paths: [\"" + strings.Join(patterns, "\", \"") + "\"] } }\njobs:\n  a:\n    steps:\n      - run: echo hi\n"
	}
	many := func(n int) []string {
		ps := make([]string, n)
		for i := range ps {
			ps[i] = fmt.Sprintf("p%d/**", i)
		}
		return ps
	}

	// Exactly at the caps: accepted.
	if _, err := Parse([]byte(pipe(many(maxPathPatterns)))); err != nil {
		t.Errorf("%d patterns should be accepted: %v", maxPathPatterns, err)
	}
	if _, err := Parse([]byte(pipe([]string{strings.Repeat("a", maxPathPatternLen)}))); err != nil {
		t.Errorf("a %d-char pattern should be accepted: %v", maxPathPatternLen, err)
	}

	// One past the caps: rejected.
	if _, err := Parse([]byte(pipe(many(maxPathPatterns + 1)))); err == nil || !strings.Contains(err.Error(), "too many patterns") {
		t.Errorf("%d patterns: err = %v, want too-many-patterns", maxPathPatterns+1, err)
	}
	if _, err := Parse([]byte(pipe([]string{strings.Repeat("a", maxPathPatternLen+1)}))); err == nil || !strings.Contains(err.Error(), "pattern is too long") {
		t.Errorf("over-long pattern: err = %v, want pattern-too-long", err)
	}
}

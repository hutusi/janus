package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	}
	for _, tc := range tests {
		if got := ctx.Interpolate(tc.in); got != tc.want {
			t.Errorf("Interpolate(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

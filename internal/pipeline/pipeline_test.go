package pipeline

import (
	"fmt"
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

	cases := map[string]struct{ src, want string }{
		"too many jobs":  {manyJobs.String(), "too many jobs"},
		"too many steps": {manySteps.String(), "too many steps"},
		"long job name":  {longName, "job name too long"},
		"long command":   {longCmd, "`run` is too long"},
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

package config

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "janus.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadEmptyPathReturnsDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":8080" || cfg.PipelinePath != ".janus/ci.yml" || cfg.MaxParallelJobs != 4 {
		t.Errorf("Load(\"\") did not return defaults: %+v", cfg)
	}
}

func TestLoadOverlaysFileOnDefaults(t *testing.T) {
	path := writeFile(t, `
addr: ":9000"
step_timeout: "10m"
max_parallel_runs: 8
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":9000" {
		t.Errorf("addr = %q, want :9000 (from file)", cfg.Addr)
	}
	if time.Duration(cfg.StepTimeout) != 10*time.Minute {
		t.Errorf("step_timeout = %v, want 10m", time.Duration(cfg.StepTimeout))
	}
	if cfg.MaxParallelRuns != 8 {
		t.Errorf("max_parallel_runs = %d, want 8 (from file)", cfg.MaxParallelRuns)
	}
	if cfg.PipelinePath != ".janus/ci.yml" {
		t.Errorf("pipeline_path = %q, want default kept", cfg.PipelinePath)
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	path := writeFile(t, "addr: \":9000\"\nbogus_key: 1\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for unknown key, got nil")
	}
}

func TestLoadRejectsBadDuration(t *testing.T) {
	path := writeFile(t, "step_timeout: \"banana\"\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for bad duration, got nil")
	}
}

func TestLoadMissingFileErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yml")); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadEmptyFileKeepsDefaults(t *testing.T) {
	cfg, err := Load(writeFile(t, ""))
	if err != nil {
		t.Fatalf("empty file should be ok: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("empty file should keep defaults, got addr %q", cfg.Addr)
	}
}

func TestResolve(t *testing.T) {
	// Explicit path always wins, even if it doesn't exist.
	if got := Resolve("/some/explicit.yml"); got != "/some/explicit.yml" {
		t.Errorf("Resolve(explicit) = %q, want it returned verbatim", got)
	}

	// No explicit path, no ./janus.yml → empty (use defaults).
	t.Chdir(t.TempDir())
	if got := Resolve(""); got != "" {
		t.Errorf("Resolve(\"\") with no file = %q, want empty", got)
	}

	// No explicit path, ./janus.yml present → DefaultPath.
	if err := os.WriteFile(DefaultPath, []byte("addr: \":1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Resolve(""); got != DefaultPath {
		t.Errorf("Resolve(\"\") with %s present = %q, want %s", DefaultPath, got, DefaultPath)
	}
}

func TestExampleYAMLParses(t *testing.T) {
	// The shipped starter config must satisfy the strict decoder, so `janus
	// init` never produces a file that `janus serve` then rejects.
	path := writeFile(t, ExampleYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("embedded example.yml does not parse: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("embedded example.yml does not validate: %v", err)
	}
}

func TestValidateWorkspaceStrategy(t *testing.T) {
	tests := []struct {
		value   string
		wantErr bool
	}{
		{"", false},
		{"fresh", false},
		{"persistent", false},
		{"Persistent", true}, // case-sensitive, like every other enum value
		{"incremental", true},
	}
	for _, tc := range tests {
		cfg := Defaults()
		cfg.WorkspaceStrategy = tc.value
		err := cfg.Validate()
		if tc.wantErr && err == nil {
			t.Errorf("Validate(%q) = nil, want error", tc.value)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("Validate(%q) = %v, want nil", tc.value, err)
		}
	}
}

func TestOverlayEnv(t *testing.T) {
	t.Setenv("JANUS_GITLAB_SECRET", "from-env")
	t.Setenv("JANUS_API_TOKEN", "tok-env")
	cfg := Defaults()
	cfg.GitLabSecret = "from-file"
	cfg.OverlayEnv()
	if cfg.GitLabSecret != "from-env" {
		t.Errorf("gitlab secret = %q, want env to override file", cfg.GitLabSecret)
	}
	if cfg.APIToken != "tok-env" {
		t.Errorf("api token = %q, want from env", cfg.APIToken)
	}
}

func TestOverlayFlagsOnlyAppliesSetFlags(t *testing.T) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.String("addr", ":8080", "")
	fs.Int("max-parallel-jobs", 4, "")
	fs.Duration("step-timeout", 0, "")
	fs.String("data-dir", "", "")
	fs.String("workspace-strategy", "fresh", "")
	if err := fs.Parse([]string{"--addr", ":7777", "--step-timeout", "5s", "--workspace-strategy", "persistent"}); err != nil {
		t.Fatal(err)
	}

	cfg := Defaults()
	cfg.DataDir = "/from/file" // not overridden by a flag → must survive
	cfg.MaxParallelJobs = 9    // flag not set → must survive
	cfg.OverlayFlags(fs)

	if cfg.Addr != ":7777" {
		t.Errorf("addr = %q, want :7777 (flag wins)", cfg.Addr)
	}
	if time.Duration(cfg.StepTimeout) != 5*time.Second {
		t.Errorf("step_timeout = %v, want 5s (flag)", time.Duration(cfg.StepTimeout))
	}
	if cfg.DataDir != "/from/file" {
		t.Errorf("data_dir = %q, want unset flag to not clobber file value", cfg.DataDir)
	}
	if cfg.MaxParallelJobs != 9 {
		t.Errorf("max_parallel_jobs = %d, want 9 (unset flag preserved)", cfg.MaxParallelJobs)
	}
	if cfg.WorkspaceStrategy != "persistent" {
		t.Errorf("workspace_strategy = %q, want persistent (flag wins)", cfg.WorkspaceStrategy)
	}
}

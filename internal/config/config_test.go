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
	if cfg.HistoryLimit != 1000 {
		t.Errorf("default history_limit = %d, want 1000", cfg.HistoryLimit)
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

func TestLoadRejectsMultipleDocuments(t *testing.T) {
	path := writeFile(t, "addr: \":9000\"\n---\naddr: \":9001\"\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for a second YAML document, got nil")
	}
}

func TestValidateRejectsNonsenseValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(c *Config)
	}{
		{"negative max_parallel_runs", func(c *Config) { c.MaxParallelRuns = -1 }},
		{"negative max_parallel_jobs", func(c *Config) { c.MaxParallelJobs = -2 }},
		{"negative history_limit", func(c *Config) { c.HistoryLimit = -1 }},
		{"negative step_timeout", func(c *Config) { c.StepTimeout = Duration(-5 * time.Second) }},
		{"empty workspace_root", func(c *Config) { c.WorkspaceRoot = "" }},
		{"empty pipeline_path", func(c *Config) { c.PipelinePath = "  " }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("Validate should reject the value, got nil")
			}
		})
	}

	// Zero caps stay allowed: they mean "use the default" downstream.
	cfg := Defaults()
	cfg.MaxParallelRuns, cfg.MaxParallelJobs = 0, 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("zero caps should be valid (they mean the default): %v", err)
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
		{"mirror", false},
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

func TestValidateCloneURL(t *testing.T) {
	tests := []struct {
		value   string
		wantErr bool
	}{
		{"", false}, // = http
		{"http", false},
		{"ssh", false},
		{"SSH", true}, // case-sensitive, like every other enum value
		{"https", true},
		{"git", true},
	}
	for _, tc := range tests {
		cfg := Defaults()
		cfg.CloneURL = tc.value
		err := cfg.Validate()
		if tc.wantErr && err == nil {
			t.Errorf("Validate(%q) = nil, want error", tc.value)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("Validate(%q) = %v, want nil", tc.value, err)
		}
	}
}

func TestNotificationsDecode(t *testing.T) {
	path := writeFile(t, `
base_url: "https://ci.example.com"
notifications:
  - url: "https://example.com/hook"
    on: [failed, success]
    secret: "s3cr3t"
  - url: "https://other.example.com/hook"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BaseURL != "https://ci.example.com" {
		t.Errorf("base_url = %q, want https://ci.example.com", cfg.BaseURL)
	}
	if len(cfg.Notifications) != 2 {
		t.Fatalf("got %d notification targets, want 2", len(cfg.Notifications))
	}
	first := cfg.Notifications[0]
	if first.URL != "https://example.com/hook" {
		t.Errorf("target[0].url = %q", first.URL)
	}
	if len(first.On) != 2 || first.On[0] != "failed" || first.On[1] != "success" {
		t.Errorf("target[0].on = %v, want [failed success]", first.On)
	}
	if first.Secret != "s3cr3t" {
		t.Errorf("target[0].secret = %q, want s3cr3t", first.Secret)
	}
	// Omitted keys decode to their zero values (notify.New fills the defaults).
	if second := cfg.Notifications[1]; second.On != nil || second.Secret != "" {
		t.Errorf("target[1] = %+v, want empty on/secret", second)
	}
}

func TestNotificationsStrictDecoder(t *testing.T) {
	// An unknown key inside a target must error — the yaml tags are the whole
	// contract, exactly as at the top level.
	path := writeFile(t, "notifications:\n  - url: \"https://x/y\"\n    onn: [failed]\n")
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for unknown target key, got nil")
	}
}

func TestOverlayEnv(t *testing.T) {
	t.Setenv("JANUS_GITLAB_SECRET", "from-env")
	t.Setenv("JANUS_API_TOKEN", "tok-env")
	t.Setenv("JANUS_GITLAB_API_TOKEN", "api-tok-env")
	t.Setenv("JANUS_GITHUB_SECRET", "gh-secret-env")
	t.Setenv("JANUS_GITHUB_API_TOKEN", "gh-tok-env")
	cfg := Defaults()
	cfg.GitLabSecret = "from-file"
	cfg.GitHubSecret = "gh-from-file"
	cfg.OverlayEnv()
	if cfg.GitLabSecret != "from-env" {
		t.Errorf("gitlab secret = %q, want env to override file", cfg.GitLabSecret)
	}
	if cfg.APIToken != "tok-env" {
		t.Errorf("api token = %q, want from env", cfg.APIToken)
	}
	if cfg.GitLabAPIToken != "api-tok-env" {
		t.Errorf("gitlab api token = %q, want from env", cfg.GitLabAPIToken)
	}
	if cfg.GitHubSecret != "gh-secret-env" {
		t.Errorf("github secret = %q, want env to override file", cfg.GitHubSecret)
	}
	if cfg.GitHubAPIToken != "gh-tok-env" {
		t.Errorf("github api token = %q, want from env", cfg.GitHubAPIToken)
	}
}

func TestGitHubStatusConfigDecode(t *testing.T) {
	path := writeFile(t, "github_secret: \"whsec\"\ngithub_api_token: \"ghp_xxx\"\ngithub_url: \"https://github.example.com\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHubSecret != "whsec" {
		t.Errorf("github_secret = %q", cfg.GitHubSecret)
	}
	if cfg.GitHubAPIToken != "ghp_xxx" {
		t.Errorf("github_api_token = %q", cfg.GitHubAPIToken)
	}
	if cfg.GitHubURL != "https://github.example.com" {
		t.Errorf("github_url = %q", cfg.GitHubURL)
	}
}

func TestGitLabStatusConfigDecode(t *testing.T) {
	path := writeFile(t, "gitlab_api_token: \"glpat-xxx\"\ngitlab_url: \"https://gitlab.example.com\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitLabAPIToken != "glpat-xxx" {
		t.Errorf("gitlab_api_token = %q", cfg.GitLabAPIToken)
	}
	if cfg.GitLabURL != "https://gitlab.example.com" {
		t.Errorf("gitlab_url = %q", cfg.GitLabURL)
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

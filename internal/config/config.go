// Package config holds the configuration for `janus serve`. Settings can come
// from a YAML config file, environment variables, and CLI flags, applied in
// increasing order of precedence: defaults < config file < env < flags.
//
// The YAML decoder is strict (KnownFields), so a typo'd key is an error rather
// than being silently ignored — matching the pipeline parser's philosophy.
package config

import (
	"bytes"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPath is the config file auto-loaded by `janus serve` when --config is
// not given and the file exists in the working directory.
const DefaultPath = "janus.yml"

// ExampleYAML is an annotated starter config, written by `janus init` and the
// single source of truth for the example documentation.
//
//go:embed example.yml
var ExampleYAML string

// Resolve returns the config path to load: the explicit path if non-empty;
// otherwise DefaultPath if it exists as a file in the working directory;
// otherwise "" (use built-in defaults, no file).
func Resolve(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if fi, err := os.Stat(DefaultPath); err == nil && !fi.IsDir() {
		return DefaultPath
	}
	return ""
}

// Config is the full set of `janus serve` settings.
type Config struct {
	Addr              string   `yaml:"addr"`
	DataDir           string   `yaml:"data_dir"`
	WorkspaceRoot     string   `yaml:"workspace_root"`
	PipelinePath      string   `yaml:"pipeline_path"`
	MaxParallelJobs   int      `yaml:"max_parallel_jobs"`
	MaxParallelRuns   int      `yaml:"max_parallel_runs"`
	HistoryLimit      int      `yaml:"history_limit"`
	StepTimeout       Duration `yaml:"step_timeout"`
	KeepWorkspaces    bool     `yaml:"keep_workspaces"`
	WorkspaceStrategy string   `yaml:"workspace_strategy"`
	CloneURL          string   `yaml:"clone_url"`
	GitLabSecret      string   `yaml:"gitlab_secret"`
	APIToken          string   `yaml:"api_token"`
	AllowRepos        []string `yaml:"allow_repos"`
}

// Defaults returns the built-in configuration used when nothing overrides it.
func Defaults() Config {
	return Config{
		Addr:              ":8080",
		DataDir:           "",
		WorkspaceRoot:     filepath.Join(os.TempDir(), "janus-workspaces"),
		PipelinePath:      ".janus/ci.yml",
		MaxParallelJobs:   4,
		MaxParallelRuns:   4,
		HistoryLimit:      1000,
		StepTimeout:       0,
		KeepWorkspaces:    false,
		WorkspaceStrategy: "fresh",
		CloneURL:          "http",
	}
}

// Validate rejects setting values that decoding cannot catch structurally.
// Call it after all overlays so flag- and env-supplied values are checked too.
// Zero concurrency caps mean "use the default" and stay allowed; values that
// can only be a mistake (negatives, explicitly-empty paths) are rejected here
// instead of silently reverting to a default downstream.
func (c Config) Validate() error {
	switch c.WorkspaceStrategy {
	case "", "fresh", "persistent", "mirror": // "" = fresh
	default:
		return fmt.Errorf("workspace_strategy: %q is not valid (use \"fresh\", \"persistent\", or \"mirror\")", c.WorkspaceStrategy)
	}
	switch c.CloneURL {
	case "", "http", "ssh": // "" = http
	default:
		return fmt.Errorf("clone_url: %q is not valid (use \"http\" or \"ssh\")", c.CloneURL)
	}
	if c.MaxParallelRuns < 0 {
		return fmt.Errorf("max_parallel_runs: %d is not valid (must be >= 0; 0 means the default)", c.MaxParallelRuns)
	}
	if c.MaxParallelJobs < 0 {
		return fmt.Errorf("max_parallel_jobs: %d is not valid (must be >= 0; 0 means the default)", c.MaxParallelJobs)
	}
	if c.HistoryLimit < 0 {
		return fmt.Errorf("history_limit: %d is not valid (must be >= 0; 0 means unlimited)", c.HistoryLimit)
	}
	if c.StepTimeout < 0 {
		return fmt.Errorf("step_timeout: %q is not valid (must be positive; omit for no timeout)", time.Duration(c.StepTimeout).String())
	}
	if strings.TrimSpace(c.WorkspaceRoot) == "" {
		return errors.New("workspace_root cannot be empty")
	}
	if strings.TrimSpace(c.PipelinePath) == "" {
		return errors.New("pipeline_path cannot be empty")
	}
	return nil
}

// Load returns the defaults overlaid with a YAML config file. An empty path
// returns the defaults unchanged; an empty file is treated as no overrides.
// Unknown keys and malformed values are errors.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return cfg, nil // empty document: keep defaults
		}
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	// One document per file — a second `---` document would be silently
	// ignored, and half-applied configuration is worse than an error.
	if err := dec.Decode(new(Config)); !errors.Is(err, io.EOF) {
		return cfg, fmt.Errorf("parse config %s: multiple YAML documents are not supported", path)
	}
	return cfg, nil
}

// OverlayEnv applies environment variables over the current config. Only the
// sensitive values have env fallbacks, so secrets can stay out of the file.
func (c *Config) OverlayEnv() {
	if v, ok := os.LookupEnv("JANUS_GITLAB_SECRET"); ok {
		c.GitLabSecret = v
	}
	if v, ok := os.LookupEnv("JANUS_API_TOKEN"); ok {
		c.APIToken = v
	}
}

// OverlayFlags applies the CLI flags that were explicitly set (per fs.Visit)
// over the current config — so a flag always wins, but an unset flag never
// clobbers a file/env value with its default. Flag names are the kebab-case
// equivalents of the YAML keys.
func (c *Config) OverlayFlags(fs *flag.FlagSet) {
	fs.Visit(func(f *flag.Flag) {
		g, ok := f.Value.(flag.Getter)
		if !ok {
			return
		}
		switch f.Name {
		case "addr":
			c.Addr = g.Get().(string)
		case "data-dir":
			c.DataDir = g.Get().(string)
		case "workspace-root":
			c.WorkspaceRoot = g.Get().(string)
		case "pipeline-path":
			c.PipelinePath = g.Get().(string)
		case "max-parallel-jobs":
			c.MaxParallelJobs = g.Get().(int)
		case "max-parallel-runs":
			c.MaxParallelRuns = g.Get().(int)
		case "history-limit":
			c.HistoryLimit = g.Get().(int)
		case "step-timeout":
			c.StepTimeout = Duration(g.Get().(time.Duration))
		case "keep-workspaces":
			c.KeepWorkspaces = g.Get().(bool)
		case "workspace-strategy":
			c.WorkspaceStrategy = g.Get().(string)
		case "clone-url":
			c.CloneURL = g.Get().(string)
		case "gitlab-secret":
			c.GitLabSecret = g.Get().(string)
		case "api-token":
			c.APIToken = g.Get().(string)
		case "allow-repos":
			c.AllowRepos = splitCSV(g.Get().(string)) // replaces (does not merge) the file list
		}
	})
}

// splitCSV splits a comma-separated value, trimming and dropping empties.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Duration wraps time.Duration so it decodes from a YAML string like "10m".
type Duration time.Duration

// UnmarshalYAML parses a Go duration string (e.g. "30s", "10m", "1h30m").
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("step_timeout must be a duration string like \"10m\": %w", err)
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid step_timeout %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML renders the duration back to a string (used for example configs).
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

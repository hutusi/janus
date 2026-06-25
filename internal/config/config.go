// Package config holds the configuration for `janus serve`. Settings can come
// from a YAML config file, environment variables, and CLI flags, applied in
// increasing order of precedence: defaults < config file < env < flags.
//
// The YAML decoder is strict (KnownFields), so a typo'd key is an error rather
// than being silently ignored — matching the pipeline parser's philosophy.
package config

import (
	"bytes"
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

// Config is the full set of `janus serve` settings.
type Config struct {
	Addr            string   `yaml:"addr"`
	DataDir         string   `yaml:"data_dir"`
	WorkspaceRoot   string   `yaml:"workspace_root"`
	PipelinePath    string   `yaml:"pipeline_path"`
	MaxParallelJobs int      `yaml:"max_parallel_jobs"`
	MaxParallelRuns int      `yaml:"max_parallel_runs"`
	StepTimeout     Duration `yaml:"step_timeout"`
	KeepWorkspaces  bool     `yaml:"keep_workspaces"`
	GitLabSecret    string   `yaml:"gitlab_secret"`
	APIToken        string   `yaml:"api_token"`
	AllowRepos      []string `yaml:"allow_repos"`
}

// Defaults returns the built-in configuration used when nothing overrides it.
func Defaults() Config {
	return Config{
		Addr:            ":8080",
		DataDir:         "",
		WorkspaceRoot:   filepath.Join(os.TempDir(), "janus-workspaces"),
		PipelinePath:    ".janus/ci.yml",
		MaxParallelJobs: 4,
		MaxParallelRuns: 4,
		StepTimeout:     0,
		KeepWorkspaces:  false,
	}
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
		case "step-timeout":
			c.StepTimeout = Duration(g.Get().(time.Duration))
		case "keep-workspaces":
			c.KeepWorkspaces = g.Get().(bool)
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

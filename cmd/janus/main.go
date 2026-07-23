// Command janus is a minimal, self-hosted CI/CD service that runs pipelines as
// host processes. It is a single binary with a few subcommands:
//
//	janus serve      start the HTTP server (webhooks, manual trigger, dashboard)
//	janus validate   validate a .janus/ci.yml pipeline file
//	janus run        run a pipeline locally against a directory
//	janus version    print the version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/hutusi/janus/internal/allowlist"
	"github.com/hutusi/janus/internal/config"
	"github.com/hutusi/janus/internal/engine"
	"github.com/hutusi/janus/internal/model"
	"github.com/hutusi/janus/internal/pipeline"
	"github.com/hutusi/janus/internal/provider"
	"github.com/hutusi/janus/internal/runner"
	"github.com/hutusi/janus/internal/server"
	"github.com/hutusi/janus/internal/store"
	"github.com/hutusi/janus/internal/workspace"
)

// version (the release tag) and commit (the short hash) are overridden at
// build time via -ldflags "-X main.version=... -X main.commit=...". They are
// stamped separately so the tag survives builds where git tags are absent
// (shallow CI clones), where a single git-describe string would silently
// degrade to a bare commit hash.
var (
	version = "dev"
	commit  = ""
)

// versionString is the human-facing form, e.g. "v0.2.0 (f97e513)" — shown by
// `janus version`, the dashboard header, and /healthz. Builds that bypass the
// Makefile/release ldflags (plain go build/install) fall back to the metadata
// the Go toolchain embeds on its own; only `go run`, which embeds nothing,
// still reports plain "dev".
func versionString() string {
	v, c := version, commit
	if v == "dev" && c == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			if bv, bc := fromBuildInfo(bi); bv != "" || bc != "" {
				if bv != "" {
					v = bv
				}
				c = bc
			}
		}
	}
	if c == "" {
		return v
	}
	return v + " (" + c + ")"
}

// fromBuildInfo derives (version, commit) from Go's embedded build info: since
// Go 1.24 `go build` from a git checkout stamps a tag-derived main-module
// version (the tag itself, or a pseudo-version past it) plus vcs.revision /
// vcs.modified. Unknown values come back empty. A "+dirty" version suffix is
// stripped — dirtiness is reported on the commit, matching the ldflags form.
func fromBuildInfo(bi *debug.BuildInfo) (string, string) {
	v := bi.Main.Version
	if v == "(devel)" {
		v = ""
	}
	v = strings.TrimSuffix(v, "+dirty")
	var rev string
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if rev != "" && dirty {
		rev += "-dirty"
	}
	return v, rev
}

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(args)
	case "init":
		err = runInit(args)
	case "validate":
		err = runValidate(args)
	case "run":
		err = runRun(args)
	case "version", "-version", "--version":
		fmt.Println("janus", versionString())
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "janus: unknown command %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "janus:", err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `janus — minimal self-hosted CI/CD

Usage:
  janus serve [flags]     Start the HTTP server (webhooks, manual trigger, dashboard)
  janus init [flags]      Write a starter janus.yml config file
  janus validate <file>   Validate a .janus/ci.yml pipeline file
  janus run <dir>         Run a pipeline locally against a directory
  janus version           Print the version

Run "janus <command> -h" for command-specific flags.
`)
}

// runServe wires the store, engine, runner, and HTTP server together and serves
// until interrupted. Settings come from defaults, an optional --config YAML
// file, environment variables, and flags — in increasing order of precedence.
func runServe(args []string) error {
	def := config.Defaults()
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", os.Getenv("JANUS_CONFIG"), "path to a YAML config file (default: ./janus.yml if present)")
	fs.String("addr", def.Addr, "HTTP listen address")
	fs.String("data-dir", def.DataDir, "directory for persistent run history (empty = in-memory, lost on restart)")
	fs.String("workspace-root", def.WorkspaceRoot, "directory for per-run workspaces")
	fs.String("pipeline-path", def.PipelinePath, "in-repo path to the pipeline file")
	fs.Int("max-parallel-jobs", def.MaxParallelJobs, "maximum jobs to run concurrently within a run")
	fs.Int("max-parallel-runs", def.MaxParallelRuns, "maximum runs to execute concurrently")
	fs.Int("history-limit", def.HistoryLimit, "maximum terminal runs to retain; oldest (and their logs) are pruned (0 = unlimited)")
	fs.Duration("step-timeout", time.Duration(def.StepTimeout), "fail any step that runs longer than this (0 = no timeout)")
	fs.Bool("keep-workspaces", def.KeepWorkspaces, "do not delete workspaces after runs (debugging)")
	fs.String("workspace-strategy", def.WorkspaceStrategy, `workspace strategy: "fresh" (new dir per run) or "persistent" (one reusable dir per repo)`)
	fs.String("clone-url", def.CloneURL, `which clone URL from the webhook payload to check out: "http" or "ssh"`)
	fs.String("gitlab-secret", "", "GitLab webhook secret token (overrides config/env; enables /webhooks/gitlab)")
	fs.String("api-token", "", "bearer token for /api/* (overrides config/env)")
	fs.String("allow-repos", "", "comma-separated allowed repo URL prefixes; '*' allows all (overrides config)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Precedence: defaults < config file < env < flags. With no --config, fall
	// back to ./janus.yml if it exists.
	cfgPath := config.Resolve(*configPath)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	cfg.OverlayEnv()
	cfg.OverlayFlags(fs)
	if err := cfg.Validate(); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if cfgPath != "" {
		logger.Info("loaded config", "path", cfgPath)
	}

	allow, err := allowlist.New(cfg.AllowRepos)
	if err != nil {
		return fmt.Errorf("allow_repos: %w", err)
	}
	switch {
	case len(allow) == 0:
		logger.Warn("no repos allowed; every webhook and manual trigger will be rejected — set allow_repos (use '*' to allow all)")
	case containsWildcard(allow):
		logger.Warn("allowing ALL repositories (allow_repos contains '*')")
	default:
		logger.Info("repo allowlist active", "entries", len(allow))
	}

	var st store.Store
	if cfg.DataDir != "" {
		fst, err := store.NewFile(cfg.DataDir)
		if err != nil {
			return fmt.Errorf("data-dir: %w", err)
		}
		st = fst
	} else {
		logger.Warn("no data-dir set; run history is in-memory and lost on restart")
		st = store.NewMemory()
	}

	eng := engine.New(st,
		engine.WithMaxParallelJobs(cfg.MaxParallelJobs),
		engine.WithStepTimeout(time.Duration(cfg.StepTimeout)),
		engine.WithLogger(logger),
	)
	rn := runner.New(st, eng, runner.Options{
		WSRoot:       cfg.WorkspaceRoot,
		PipelinePath: cfg.PipelinePath,
		KeepWS:       cfg.KeepWorkspaces,
		MaxRuns:      cfg.MaxParallelRuns,
		HistoryLimit: cfg.HistoryLimit,
		Allowlist:    allow,
		Strategy:     cfg.WorkspaceStrategy,
		Logger:       logger,
	})
	if cfg.WorkspaceStrategy == "persistent" {
		logger.Info("persistent workspaces enabled; builds reuse per-repo directories (not hermetic)")
	}
	if err := rn.Sweep(); err != nil {
		logger.Warn("workspace sweep failed", "err", err)
	}
	if n, err := rn.ReconcileInterrupted(); err != nil {
		// The store rejected a write at startup (e.g. a full or read-only data
		// dir), so a run is still stale and the store is unhealthy. Latch
		// degraded so /healthz reports 503 rather than a false 200 on restart.
		rn.MarkDegraded()
		logger.Warn("reconciling interrupted runs failed; marking unhealthy", "err", err)
	} else if n > 0 {
		logger.Info("marked runs interrupted by the previous process as cancelled", "count", n)
	}
	if n, err := st.Prune(cfg.HistoryLimit); err != nil {
		logger.Warn("pruning run history failed", "err", err)
	} else if n > 0 {
		logger.Info("pruned old runs beyond history_limit at startup", "removed", n, "keep", cfg.HistoryLimit)
	}

	opts := []server.Option{server.WithLogger(logger)}
	if cfg.GitLabSecret != "" {
		ssh := cfg.CloneURL == "ssh"
		opts = append(opts, server.WithProvider(provider.GitLab{SSH: ssh}, cfg.GitLabSecret))
		logger.Info("gitlab webhook enabled at /webhooks/gitlab", "clone_url", cfg.CloneURL)
	} else {
		logger.Warn("no gitlab-secret set; /webhooks/gitlab is disabled")
	}
	if cfg.APIToken != "" {
		opts = append(opts, server.WithAPIToken(cfg.APIToken))
	}

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: server.New(st, rn, versionString(), opts...).Handler(),
		// ReadTimeout bounds reading a request (headers + body), so a
		// slow-loris POST cannot pin a connection open. WriteTimeout must stay
		// unset: the ?follow=1 log stream is a deliberately long-lived
		// response, and a write deadline would sever it mid-run.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("janus listening", "addr", cfg.Addr, "version", versionString(), "workspace_root", cfg.WorkspaceRoot)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down; stopping listener and waiting for in-flight runs")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// The runner must shut down even when HTTP shutdown times out (a
		// lingering log-follow connection is enough to exceed the deadline) —
		// returning early here would exit the process with build process
		// groups still alive.
		err := srv.Shutdown(shutdownCtx)
		// Wait up to 30s for in-flight runs; cancel (kill processes) if they overrun.
		rn.Shutdown(30 * time.Second)
		return err
	}
}

// runInit writes a starter config file, refusing to overwrite unless --force.
func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	out := fs.String("config", config.DefaultPath, "path to write the config file")
	force := fs.Bool("force", false, "overwrite an existing file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*force {
		if _, err := os.Stat(*out); err == nil {
			return fmt.Errorf("%s already exists (use --force to overwrite)", *out)
		}
	}
	if err := os.WriteFile(*out, []byte(config.ExampleYAML), 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s — edit it, then run: janus serve --config %s\n", *out, *out)
	return nil
}

// runValidate parses a pipeline file and reports whether it is valid.
func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: janus validate <file>")
	}
	path := fs.Arg(0)
	data, err := pipeline.ReadFile(path)
	if err != nil {
		return err
	}
	wf, err := pipeline.Parse(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	jobs := make([]string, 0, len(wf.Jobs))
	for name := range wf.Jobs {
		jobs = append(jobs, name)
	}
	sort.Strings(jobs)
	fmt.Printf("ok: workflow %q — %d job(s): %s\n", wf.Name, len(jobs), strings.Join(jobs, ", "))
	return nil
}

// runRun executes a pipeline locally, streaming logs to the terminal. It works
// either against an existing directory (`janus run <dir>`) or by checking out a
// repository at a commit (`janus run --repo <url> --sha <sha>`).
func runRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	file := fs.String("file", ".janus/ci.yml", "pipeline file, relative to the workspace")
	branch := fs.String("branch", "", "value for ${{ branch }}")
	maxJobs := fs.Int("max-parallel-jobs", 4, "maximum jobs to run concurrently")
	stepTimeout := fs.Duration("step-timeout", 0, "fail any step that runs longer than this (0 = no timeout)")
	repo := fs.String("repo", "", "git repo URL to check out (instead of using <dir>)")
	sha := fs.String("sha", "", "commit SHA to check out (with --repo)")
	ref := fs.String("ref", "", "git ref to fetch as a fallback (with --repo)")
	wsRoot := fs.String("workspace-root", "", "directory to create the workspace under (default: temp dir)")
	keep := fs.Bool("keep-workspace", false, "do not delete the workspace after the run")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Ctrl-C must drive the engine's cancellation path (process-group kill,
	// run marked cancelled) rather than just dying with orphaned children.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ev := model.Event{Provider: "manual", Kind: model.EventManual, Branch: *branch}

	var dir string
	if *repo != "" {
		if fs.NArg() != 0 {
			return errors.New("provide either <dir> or --repo, not both")
		}
		root := *wsRoot
		if root == "" {
			root = os.TempDir()
		}
		wsDir, err := os.MkdirTemp(root, "janus-ws-*")
		if err != nil {
			return err
		}
		ws, err := workspace.Checkout(ctx, workspace.Options{
			Dir: wsDir, RepoURL: *repo, SHA: *sha, Ref: *ref, Keep: *keep,
		})
		if err != nil {
			return err
		}
		defer func() { _ = ws.Cleanup() }()
		dir = ws.Dir
		ev.RepoURL, ev.SHA, ev.Ref = *repo, *sha, *ref
		if ws.Head != "" { // the exact commit checked out, not the abbreviation supplied
			ev.SHA = ws.Head
		}
		if ev.Branch == "" {
			ev.Branch = strings.TrimPrefix(*ref, "refs/heads/")
		}
	} else {
		if fs.NArg() != 1 {
			return errors.New("usage: janus run [flags] <dir>  |  janus run --repo <url> --sha <sha>")
		}
		abs, err := filepath.Abs(fs.Arg(0))
		if err != nil {
			return err
		}
		dir = abs
	}

	data, err := pipeline.ReadFile(filepath.Join(dir, *file))
	if err != nil {
		return err
	}
	wf, err := pipeline.Parse(data)
	if err != nil {
		return fmt.Errorf("%s: %w", *file, err)
	}

	st := store.NewMemory()
	eng := engine.New(st, engine.WithMaxParallelJobs(*maxJobs), engine.WithStepTimeout(*stepTimeout), engine.WithTee(os.Stdout))
	run, runErr := eng.Run(ctx, wf, ev, dir)
	if run == nil {
		return runErr // SaveRun failed: no run to summarize
	}
	printRunSummary(run)
	if runErr != nil {
		// The steps ran, but the final state could not be persisted — a
		// distinct failure from an ordinary failed run.
		return fmt.Errorf("run %s: steps completed but persisting the final state failed: %w", run.ID, runErr)
	}
	if run.Status != model.StatusSuccess {
		return fmt.Errorf("run %s finished: %s", run.ID, run.Status)
	}
	return nil
}

// containsWildcard reports whether the allowlist has a "*" (allow-all) entry.
func containsWildcard(a allowlist.Allowlist) bool {
	for _, e := range a {
		if e == "*" {
			return true
		}
	}
	return false
}

// printRunSummary writes a compact per-job/step status report.
func printRunSummary(run *model.Run) {
	fmt.Printf("\n=== run %s: %s ===\n", run.ID, run.Status)
	for _, jr := range run.Jobs {
		fmt.Printf("  %s: %s\n", jr.Name, jr.Status)
		for _, sr := range jr.Steps {
			fmt.Printf("    step %d [%s] exit=%d: %s\n", sr.Index, sr.Status, sr.ExitCode, sr.Command)
		}
	}
}

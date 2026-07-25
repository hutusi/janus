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
	"github.com/hutusi/janus/internal/notify"
	"github.com/hutusi/janus/internal/pipeline"
	"github.com/hutusi/janus/internal/provider"
	"github.com/hutusi/janus/internal/runner"
	"github.com/hutusi/janus/internal/server"
	"github.com/hutusi/janus/internal/status"
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
	c, err := buildServe(args, os.Stderr)
	if err != nil {
		return err
	}
	return serve(c)
}

// serveComponents is everything `janus serve` wires up, built but not yet
// serving. Split out so the wiring — config precedence, which providers and
// status reporters get registered, the startup warnings, and the startup
// sweep/reconcile/prune — can be exercised without binding a listener or
// installing signal handlers.
type serveComponents struct {
	cfg      config.Config
	logger   *slog.Logger
	store    store.Store
	runner   *runner.Runner
	notifier *notify.Notifier
	reporter *status.Reporter
	srv      *http.Server // handler and timeouts set; not listening
}

// buildServe parses args, resolves configuration, and constructs every
// component `janus serve` needs. Diagnostics go to logw. It performs the
// startup sweep, reconciliation, and prune, so the returned components are
// ready to serve.
func buildServe(args []string, logw io.Writer) (*serveComponents, error) {
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
	fs.Int64("log-limit", def.LogLimit, "maximum bytes a single step may write to its log; beyond it the log is truncated with a marker (0 = unlimited)")
	fs.Duration("step-timeout", time.Duration(def.StepTimeout), "fail any step that runs longer than this (0 = no timeout)")
	fs.Bool("keep-workspaces", def.KeepWorkspaces, "do not delete workspaces after runs (debugging)")
	fs.String("workspace-strategy", def.WorkspaceStrategy, `workspace strategy: "fresh" (new dir per run), "persistent" (one reusable dir per repo), or "mirror" (per-repo bare-mirror cache, fresh dir per run)`)
	fs.String("clone-url", def.CloneURL, `which clone URL from the webhook payload to check out: "http" or "ssh"`)
	fs.String("gitlab-secret", "", "GitLab webhook secret token (overrides config/env; enables /webhooks/gitlab)")
	fs.String("gitlab-api-token", "", "GitLab API token (api scope) for commit-status reporting (overrides config/env)")
	fs.String("github-secret", "", "GitHub webhook secret (overrides config/env; enables /webhooks/github)")
	fs.String("github-api-token", "", "GitHub API token (repo:status) for commit-status reporting (overrides config/env)")
	fs.String("gitee-secret", "", "Gitee webhook secret (overrides config/env; enables /webhooks/gitee)")
	fs.String("gitcode-secret", "", "GitCode webhook secret (overrides config/env; enables /webhooks/gitcode)")
	fs.String("api-token", "", "bearer token for /api/* (overrides config/env)")
	fs.String("allow-repos", "", "comma-separated allowed repo URL prefixes; '*' allows all (overrides config)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	// Precedence: defaults < config file < env < flags. With no --config, fall
	// back to ./janus.yml if it exists.
	cfgPath := config.Resolve(*configPath)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	cfg.OverlayEnv()
	cfg.OverlayFlags(fs)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	logger := slog.New(slog.NewTextHandler(logw, nil))
	if cfgPath != "" {
		logger.Info("loaded config", "path", cfgPath)
	}

	allow, err := allowlist.New(cfg.AllowRepos)
	if err != nil {
		return nil, fmt.Errorf("allow_repos: %w", err)
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
			return nil, fmt.Errorf("data-dir: %w", err)
		}
		st = fst
	} else {
		logger.Warn("no data-dir set; run history is in-memory and lost on restart")
		st = store.NewMemory()
	}

	// Outbound notifications are optional; build the notifier only when targets
	// are configured. Leaving it nil (rather than a typed-nil interface) is what
	// keeps the runner's `notifier != nil` guard honest.
	var notifier *notify.Notifier
	if len(cfg.Notifications) > 0 {
		targets := make([]notify.Target, len(cfg.Notifications))
		for i, t := range cfg.Notifications {
			targets[i] = notify.Target{URL: t.URL, On: t.On, Secret: t.Secret}
		}
		notifier, err = notify.New(targets, notify.WithBaseURL(cfg.BaseURL), notify.WithLogger(logger))
		if err != nil {
			return nil, fmt.Errorf("notifications: %w", err)
		}
		logger.Info("outbound notifications enabled", "targets", len(targets))
	}

	// Commit-status reporting is optional; build one reporter for whichever
	// providers have an outbound API token configured (nil, not a typed-nil
	// interface, keeps the runner's guard honest).
	var reporter *status.Reporter
	var stOpts []status.Option
	if cfg.GitLabAPIToken != "" {
		stOpts = append(stOpts, status.WithGitLab(cfg.GitLabAPIToken, cfg.GitLabURL))
	}
	if cfg.GitHubAPIToken != "" {
		stOpts = append(stOpts, status.WithGitHub(cfg.GitHubAPIToken, cfg.GitHubURL))
	}
	if len(stOpts) > 0 {
		stOpts = append(stOpts, status.WithBaseURL(cfg.BaseURL), status.WithLogger(logger))
		reporter, err = status.New(stOpts...)
		if err != nil {
			return nil, fmt.Errorf("commit status: %w", err)
		}
		if cfg.GitLabAPIToken != "" {
			logger.Info("gitlab commit-status reporting enabled")
			if cfg.GitLabSecret == "" {
				logger.Warn("gitlab-api-token is set but gitlab-secret is not; no GitLab webhooks are accepted, so no statuses will be reported")
			}
			if cfg.CloneURL == "ssh" && cfg.GitLabURL == "" {
				logger.Warn("gitlab commit status with clone_url \"ssh\" needs gitlab_url set (an ssh clone URL has no derivable API base); statuses will be skipped")
			}
		}
		if cfg.GitHubAPIToken != "" {
			logger.Info("github commit-status reporting enabled")
			if cfg.GitHubSecret == "" {
				logger.Warn("github-api-token is set but github-secret is not; no GitHub webhooks are accepted, so no statuses will be reported")
			}
			if cfg.CloneURL == "ssh" && cfg.GitHubURL == "" {
				logger.Warn("github commit status with clone_url \"ssh\" needs github_url set (an ssh clone URL has no derivable API base); statuses will be skipped")
			}
		}
	}

	eng := engine.New(st,
		engine.WithMaxParallelJobs(cfg.MaxParallelJobs),
		engine.WithStepTimeout(time.Duration(cfg.StepTimeout)),
		engine.WithLogLimit(cfg.LogLimit),
		engine.WithLogger(logger),
	)
	runnerOpts := runner.Options{
		WSRoot:       cfg.WorkspaceRoot,
		PipelinePath: cfg.PipelinePath,
		KeepWS:       cfg.KeepWorkspaces,
		MaxRuns:      cfg.MaxParallelRuns,
		HistoryLimit: cfg.HistoryLimit,
		Allowlist:    allow,
		Strategy:     cfg.WorkspaceStrategy,
		Logger:       logger,
	}
	if notifier != nil {
		runnerOpts.Notifier = notifier
	}
	if reporter != nil {
		runnerOpts.Reporter = reporter
	}
	rn := runner.New(st, eng, runnerOpts)
	if cfg.WorkspaceStrategy == runner.StrategyPersistent {
		logger.Info("persistent workspaces enabled; builds reuse per-repo directories (not hermetic)")
	}
	if cfg.WorkspaceStrategy == runner.StrategyMirror {
		logger.Info("mirror workspaces enabled; per-repo bare mirrors cached under workspace_root, runs stay hermetic")
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
	ssh := cfg.CloneURL == "ssh"
	if cfg.GitLabSecret != "" {
		opts = append(opts, server.WithProvider(provider.GitLab{SSH: ssh}, cfg.GitLabSecret))
		logger.Info("gitlab webhook enabled at /webhooks/gitlab", "clone_url", cfg.CloneURL)
	}
	if cfg.GitHubSecret != "" {
		opts = append(opts, server.WithProvider(provider.GitHub{SSH: ssh}, cfg.GitHubSecret))
		logger.Info("github webhook enabled at /webhooks/github", "clone_url", cfg.CloneURL)
	}
	if cfg.GiteeSecret != "" {
		opts = append(opts, server.WithProvider(provider.Gitee{SSH: ssh}, cfg.GiteeSecret))
		logger.Info("gitee webhook enabled at /webhooks/gitee", "clone_url", cfg.CloneURL)
	}
	if cfg.GitCodeSecret != "" {
		opts = append(opts, server.WithProvider(provider.GitCode{SSH: ssh}, cfg.GitCodeSecret))
		logger.Info("gitcode webhook enabled at /webhooks/gitcode", "clone_url", cfg.CloneURL)
	}
	if cfg.GitLabSecret == "" && cfg.GitHubSecret == "" && cfg.GiteeSecret == "" && cfg.GitCodeSecret == "" {
		logger.Warn("no webhook provider secret set; all /webhooks/* endpoints are disabled (set gitlab-secret / github-secret / gitee-secret / gitcode-secret)")
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

	return &serveComponents{
		cfg:      cfg,
		logger:   logger,
		store:    st,
		runner:   rn,
		notifier: notifier,
		reporter: reporter,
		srv:      srv,
	}, nil
}

// serve runs the built components until a signal arrives or the listener
// fails, then drains in-flight work.
func serve(c *serveComponents) error {
	cfg, logger, rn, srv := c.cfg, c.logger, c.runner, c.srv
	notifier, reporter := c.notifier, c.reporter

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("janus listening", "addr", cfg.Addr, "version", versionString(), "workspace_root", cfg.WorkspaceRoot)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Drain in-flight work on *every* exit path — a signal or a ListenAndServe
	// error: wait up to 30s for in-flight runs (killing overrunning process
	// groups), then flush queued notifications and commit-status posts. Deferred
	// so the server-error path (errCh) drains too, not just the signal path.
	defer func() {
		rn.Shutdown(30 * time.Second)
		if notifier != nil {
			notifier.Close(5 * time.Second)
		}
		if reporter != nil {
			reporter.Close(5 * time.Second)
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
		// returning early here would exit the process with build process groups
		// still alive; the deferred drain above handles that.
		return shutdownServer(shutdownCtx, srv, logger)
	}
}

// shutdownServer stops srv gracefully and treats an expired deadline as a
// successful — if abrupt — shutdown. Shutdown never cancels an in-flight
// request's context, so a single lingering `?follow=1` log stream is enough to
// exceed the deadline while everything that matters still unwinds: the listener
// is closed either way, and the caller's deferred drain cancels in-flight runs
// and kills their process groups. Reporting that as an error would exit
// non-zero and tell systemd (or Docker) that a clean `systemctl stop` failed.
// Any other error is real and propagates.
func shutdownServer(ctx context.Context, srv *http.Server, logger *slog.Logger) error {
	err := srv.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		logger.Warn("graceful HTTP shutdown timed out with connections still open; exiting anyway", "err", err)
		return nil
	}
	return err
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

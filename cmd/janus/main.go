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
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/hutusi/janus/internal/engine"
	"github.com/hutusi/janus/internal/model"
	"github.com/hutusi/janus/internal/pipeline"
	"github.com/hutusi/janus/internal/provider"
	"github.com/hutusi/janus/internal/runner"
	"github.com/hutusi/janus/internal/server"
	"github.com/hutusi/janus/internal/store"
	"github.com/hutusi/janus/internal/workspace"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

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
	case "validate":
		err = runValidate(args)
	case "run":
		err = runRun(args)
	case "version", "-version", "--version":
		fmt.Println("janus", version)
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
  janus validate <file>   Validate a .janus/ci.yml pipeline file
  janus run <dir>         Run a pipeline locally against a directory
  janus version           Print the version

Run "janus <command> -h" for command-specific flags.
`)
}

// runServe wires the store, engine, runner, and HTTP server together and serves
// until interrupted.
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "HTTP listen address")
	dataDir := fs.String("data-dir", "", "directory for persistent run history (empty = in-memory, lost on restart)")
	wsRoot := fs.String("workspace-root", filepath.Join(os.TempDir(), "janus-workspaces"), "directory for per-run workspaces")
	pipelinePath := fs.String("pipeline-path", ".janus/ci.yml", "in-repo path to the pipeline file")
	maxJobs := fs.Int("max-parallel-jobs", 4, "maximum jobs to run concurrently within a run")
	maxRuns := fs.Int("max-parallel-runs", 4, "maximum runs to execute concurrently")
	stepTimeout := fs.Duration("step-timeout", 0, "fail any step that runs longer than this (0 = no timeout)")
	keepWS := fs.Bool("keep-workspaces", false, "do not delete workspaces after runs (debugging)")
	gitlabSecret := fs.String("gitlab-secret", os.Getenv("JANUS_GITLAB_SECRET"), "GitLab webhook secret token (enables /webhooks/gitlab)")
	apiToken := fs.String("api-token", os.Getenv("JANUS_API_TOKEN"), "if set, require this bearer token on /api/* routes")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	var st store.Store
	if *dataDir != "" {
		fst, err := store.NewFile(*dataDir)
		if err != nil {
			return fmt.Errorf("data-dir: %w", err)
		}
		st = fst
	} else {
		logger.Warn("no --data-dir set; run history is in-memory and lost on restart")
		st = store.NewMemory()
	}

	eng := engine.New(st, engine.WithMaxParallelJobs(*maxJobs), engine.WithStepTimeout(*stepTimeout))
	rn := runner.New(st, eng, *wsRoot, *pipelinePath, *keepWS, *maxRuns)
	if err := rn.Sweep(); err != nil {
		logger.Warn("workspace sweep failed", "err", err)
	}

	opts := []server.Option{server.WithLogger(logger)}
	if *gitlabSecret != "" {
		opts = append(opts, server.WithProvider(provider.GitLab{}, *gitlabSecret))
		logger.Info("gitlab webhook enabled at /webhooks/gitlab")
	} else {
		logger.Warn("no --gitlab-secret set; /webhooks/gitlab is disabled")
	}
	if *apiToken != "" {
		opts = append(opts, server.WithAPIToken(*apiToken))
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           server.New(st, rn, version, opts...).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("janus listening", "addr", *addr, "version", version, "workspace_root", *wsRoot)
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
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		// Give in-flight runs a bounded grace period to finish.
		done := make(chan struct{})
		go func() { rn.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			logger.Warn("timed out waiting for in-flight runs")
		}
		return nil
	}
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
	data, err := os.ReadFile(path)
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

	ctx := context.Background()
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

	data, err := os.ReadFile(filepath.Join(dir, *file))
	if err != nil {
		return err
	}
	wf, err := pipeline.Parse(data)
	if err != nil {
		return fmt.Errorf("%s: %w", *file, err)
	}

	st := store.NewMemory()
	eng := engine.New(st, engine.WithMaxParallelJobs(*maxJobs), engine.WithStepTimeout(*stepTimeout), engine.WithTee(os.Stdout))
	run, err := eng.Run(ctx, wf, ev, dir)
	if err != nil {
		return err
	}
	printRunSummary(run)
	if run.Status != model.StatusSuccess {
		return fmt.Errorf("run %s finished: %s", run.ID, run.Status)
	}
	return nil
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

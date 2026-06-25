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
	"github.com/hutusi/janus/internal/store"
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

// runServe starts the HTTP server and blocks until interrupted.
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "HTTP listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, fmt.Sprintf("{\"status\":\"ok\",\"version\":%q}\n", version))
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("janus listening", "addr", *addr, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
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

// runRun executes a pipeline locally against a directory, streaming logs to the
// terminal. The repository is expected to already be present at <dir> (git
// checkout is added in Phase 3).
func runRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	file := fs.String("file", ".janus/ci.yml", "pipeline file, relative to <dir>")
	branch := fs.String("branch", "", "value for ${{ branch }}")
	maxJobs := fs.Int("max-parallel-jobs", 4, "maximum jobs to run concurrently")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: janus run [flags] <dir>")
	}

	dir, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return err
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
	eng := engine.New(st, engine.WithMaxParallelJobs(*maxJobs), engine.WithTee(os.Stdout))
	ev := model.Event{Provider: "manual", Kind: model.EventManual, Branch: *branch}

	run, err := eng.Run(context.Background(), wf, ev, dir)
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

// writeJSON writes a JSON body with the given status code.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

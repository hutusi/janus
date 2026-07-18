package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/hutusi/janus/internal/model"
	"github.com/hutusi/janus/internal/pipeline"
)

// shellArgv returns the argv prefix used to run a step's `run` string. Using a
// shell is deliberate: pipelines expect `npm ci && npm test`, pipes, and
// globbing. An empty stepShell selects the OS default — /bin/sh on unix, cmd on
// Windows. The pipeline validator restricts stepShell to this closed set, so the
// mapping here must stay in sync with pipeline.allowedShells.
func shellArgv(stepShell string) []string {
	switch stepShell {
	case "sh":
		return []string{"sh", "-c"}
	case "bash":
		return []string{"bash", "-c"}
	case "cmd":
		return []string{"cmd", "/C"}
	case "powershell":
		return []string{"powershell", "-NoProfile", "-NonInteractive", "-Command"}
	case "pwsh":
		return []string{"pwsh", "-NoProfile", "-NonInteractive", "-Command"}
	default: // "" → OS default
		if runtime.GOOS == "windows" {
			return []string{"cmd", "/C"}
		}
		return []string{"/bin/sh", "-c"}
	}
}

// hostEnvAllow is the curated set of host environment variables passed through
// to jobs. The Janus daemon's own environment is otherwise NOT inherited, so
// its configuration/secrets are not handed to builds via the environment.
// That is the extent of the guarantee: jobs run as the same OS user as the
// daemon — no isolation — so anything that user can read stays reachable.
// It lists both unix and Windows names; os.LookupEnv skips those absent on
// the current host.
var hostEnvAllow = []string{
	// unix
	"PATH", "HOME", "LANG", "LC_ALL", "TZ", "TMPDIR",
	// windows (needed for cmd/powershell steps to find system tools)
	"SystemRoot", "ComSpec", "PATHEXT", "USERPROFILE", "HOMEDRIVE", "HOMEPATH",
	"TEMP", "TMP", "APPDATA", "LOCALAPPDATA",
}

// executeJob runs a job's steps sequentially and returns the job's terminal
// status. The first failing step fails the job and skips the rest.
func (e *Engine) executeJob(ctx context.Context, rs *runState, job *model.Job, jr *model.JobRun) model.Status {
	if ctx.Err() != nil {
		return model.StatusSkipped // cancelled before this job ever started
	}
	rs.update(func() {
		jr.Status = model.StatusRunning
		jr.StartedAt = time.Now()
	})

	final := model.StatusSuccess
	for i := range job.Steps {
		if ctx.Err() != nil {
			final = model.StatusCancelled
			break
		}
		if st := e.runStep(ctx, rs, job, jr, jr.Steps[i], job.Steps[i]); st != model.StatusSuccess {
			final = st
			break
		}
	}

	rs.update(func() {
		for _, sr := range jr.Steps {
			if !sr.Status.Terminal() {
				sr.Status = model.StatusSkipped
			}
		}
		jr.Status = final
		jr.FinishedAt = time.Now()
	})
	return final
}

// runStep runs a single step as a host process, streaming combined stdout+stderr
// to the store (and the optional tee).
func (e *Engine) runStep(ctx context.Context, rs *runState, job *model.Job, jr *model.JobRun, sr *model.StepRun, step model.Step) model.Status {
	rs.update(func() {
		sr.Status = model.StatusRunning
		sr.StartedAt = time.Now()
	})

	w, closeLog, err := rs.stepWriter(jr.Name, sr.Index)
	if err != nil {
		rs.logger.Warn("opening log sink failed", "run", rs.run.ID, "job", jr.Name, "step", sr.Index, "err", err)
		rs.update(func() {
			sr.Status = model.StatusFailed
			sr.ExitCode = -1
			sr.FinishedAt = time.Now()
		})
		return model.StatusFailed
	}
	defer func() {
		if cerr := closeLog(); cerr != nil {
			rs.logger.Warn("closing log sink failed", "run", rs.run.ID, "job", jr.Name, "step", sr.Index, "err", cerr)
		}
	}()

	cmdStr, dir, env, err := e.prepare(rs, job, step)
	if err != nil {
		_, _ = fmt.Fprintf(w, "janus: %v\n", err)
		rs.update(func() {
			sr.Status = model.StatusFailed
			sr.ExitCode = -1
			sr.FinishedAt = time.Now()
		})
		return model.StatusFailed
	}

	// A per-step timeout (if configured) cancels only this step; the run ctx
	// stays live so other jobs keep running.
	stepCtx := ctx
	if e.stepTimeout > 0 {
		var cancel context.CancelFunc
		stepCtx, cancel = context.WithTimeout(ctx, e.stepTimeout)
		defer cancel()
	}

	argv := shellArgv(step.Shell)
	cmd := exec.CommandContext(stepCtx, argv[0], append(argv[1:], cmdStr)...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = w
	cmd.Stderr = w
	// Kill the step's whole process group on cancel/timeout; WaitDelay bounds
	// how long Wait blocks draining output if a child still holds the pipe.
	setProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second

	runErr := cmd.Run()
	timedOut := e.stepTimeout > 0 && errors.Is(stepCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	if timedOut {
		_, _ = fmt.Fprintf(w, "\njanus: step timed out after %s\n", e.stepTimeout)
	}
	code, status := classify(ctx, timedOut, runErr)
	rs.update(func() {
		sr.Status = status
		sr.ExitCode = code
		sr.FinishedAt = time.Now()
	})
	return status
}

// classify maps a command's run error to an exit code and status. A run-level
// cancellation is Cancelled; a per-step timeout is a Failed step.
func classify(runCtx context.Context, timedOut bool, runErr error) (int, model.Status) {
	if runErr == nil {
		return 0, model.StatusSuccess
	}
	if runCtx.Err() != nil {
		return -1, model.StatusCancelled
	}
	if timedOut {
		return -1, model.StatusFailed
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		return ee.ExitCode(), model.StatusFailed
	}
	return -1, model.StatusFailed // e.g. command not found
}

// prepare merges environment (curated base → workflow → job → step), resolves
// ${{ ... }} placeholders in the command, working directory, and env values,
// and returns the shell command, absolute working directory, and process env.
func (e *Engine) prepare(rs *runState, job *model.Job, step model.Step) (cmdStr, dir string, env []string, err error) {
	merged := map[string]string{}
	for _, k := range hostEnvAllow {
		if v, ok := os.LookupEnv(k); ok {
			merged[k] = v
		}
	}
	merged["CI"] = "true"
	merged["JANUS_RUN_ID"] = rs.run.ID
	merged["JANUS_EVENT"] = string(rs.event.Kind)
	merged["JANUS_REF"] = rs.event.Ref
	merged["JANUS_SHA"] = rs.event.SHA
	merged["JANUS_BRANCH"] = rs.event.Branch
	for k, v := range rs.wf.Env {
		merged[k] = v
	}
	for k, v := range job.Env {
		merged[k] = v
	}
	for k, v := range step.Env {
		merged[k] = v
	}

	ictx := pipeline.Context{
		Env:      merged,
		Ref:      rs.event.Ref,
		SHA:      rs.event.SHA,
		ShortSHA: shortSHA(rs.event.SHA),
		Branch:   rs.event.Branch,
		Event:    string(rs.event.Kind),
	}

	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env = make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+ictx.Interpolate(merged[k]))
	}

	cmdStr = ictx.Interpolate(step.Run)
	dir, err = resolveDir(rs.workDir, ictx.Interpolate(step.WorkingDir))
	return cmdStr, dir, env, err
}

// resolveDir joins a step's working-directory onto the workspace root and
// rejects paths that would escape the workspace.
func resolveDir(workspace, workingDir string) (string, error) {
	if workingDir == "" {
		return workspace, nil
	}
	joined := filepath.Join(workspace, workingDir)
	rel, err := filepath.Rel(workspace, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("working-directory %q escapes the workspace", workingDir)
	}
	return joined, nil
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// stepWriter builds the combined-output writer for one step: the store's
// append-only log sink, plus (if configured) a job-prefixed mirror to the tee.
// It errors if the log sink can't be opened, so a broken store fails the step
// loudly rather than silently dropping output.
func (rs *runState) stepWriter(job string, stepIndex int) (io.Writer, func() error, error) {
	sink, err := rs.store.LogWriter(rs.run.ID, job, stepIndex)
	if err != nil {
		return nil, nil, err
	}
	writers := []io.Writer{sink}
	closers := []io.Closer{sink}
	if rs.tee != nil {
		lp := newLinePrefixer(&lockedWriter{mu: rs.teeMu, w: rs.tee}, "["+job+"] ")
		writers = append(writers, lp)
		closers = append(closers, lp)
	}
	closeFn := func() error {
		var firstErr error
		for _, c := range closers {
			if cerr := c.Close(); cerr != nil && firstErr == nil {
				firstErr = cerr
			}
		}
		return firstErr
	}
	return io.MultiWriter(writers...), closeFn, nil
}

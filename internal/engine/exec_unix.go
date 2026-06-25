//go:build unix

package engine

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the step's shell in its own process group and, on
// cancellation, kills the whole group. Without this, `exec.CommandContext`
// signals only the direct child (/bin/sh); grandchildren spawned by the step
// (e.g. `sleep` behind `npm test`) would be orphaned and keep running.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative PID targets the whole process group (pgid == leader pid).
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

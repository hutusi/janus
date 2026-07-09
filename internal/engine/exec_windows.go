//go:build windows

package engine

import (
	"context"
	"os/exec"
	"strconv"
	"time"
)

// setProcessGroup arranges for the step's whole process tree to be killed on
// cancel/timeout. Windows has no unix-style process groups; the reliable,
// dependency-free way to terminate a tree is the built-in `taskkill /T /F`,
// which kills the target PID and all of its descendants. This mirrors the unix
// setpgid + kill(-pid) contract without pulling in golang.org/x/sys/windows
// (which the project's yaml.v3-only dependency rule forbids). Without it,
// exec.CommandContext would signal only the direct child (cmd.exe / powershell),
// orphaning grandchildren spawned by the step.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Bound taskkill: unlike the unix kill() syscall it is a spawned
		// process, and Cmd.WaitDelay only starts after Cancel returns — so a
		// hung taskkill would otherwise hang the step's cancellation forever.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		kill := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		return kill.Run()
	}
}

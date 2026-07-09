//go:build !unix && !windows

package engine

import "os/exec"

// setProcessGroup is a no-op on platforms that are neither unix nor Windows
// (both of which have their own implementation). This stub only keeps
// cross-compilation green for any other GOOS.
func setProcessGroup(_ *exec.Cmd) {}

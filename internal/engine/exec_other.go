//go:build !unix

package engine

import "os/exec"

// setProcessGroup is a no-op on non-unix platforms. Janus targets unix hosts;
// this stub only keeps cross-compilation green.
func setProcessGroup(_ *exec.Cmd) {}

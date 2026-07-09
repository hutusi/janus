package engine

import (
	"runtime"
	"slices"
	"testing"
)

// TestShellArgv pins the argv prefix for each named shell (OS-independent) and
// checks the empty default follows the host OS. Keep in sync with the closed set
// enforced by pipeline.allowedShells.
func TestShellArgv(t *testing.T) {
	named := map[string][]string{
		"sh":         {"sh", "-c"},
		"bash":       {"bash", "-c"},
		"cmd":        {"cmd", "/C"},
		"powershell": {"powershell", "-NoProfile", "-NonInteractive", "-Command"},
		"pwsh":       {"pwsh", "-NoProfile", "-NonInteractive", "-Command"},
	}
	for name, want := range named {
		if got := shellArgv(name); !slices.Equal(got, want) {
			t.Errorf("shellArgv(%q) = %v, want %v", name, got, want)
		}
	}

	wantDefault := []string{"/bin/sh", "-c"}
	if runtime.GOOS == "windows" {
		wantDefault = []string{"cmd", "/C"}
	}
	if got := shellArgv(""); !slices.Equal(got, wantDefault) {
		t.Errorf("shellArgv(\"\") on %s = %v, want %v", runtime.GOOS, got, wantDefault)
	}
}

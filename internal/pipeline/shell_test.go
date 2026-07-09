package pipeline

import (
	"strings"
	"testing"
)

func stepWithShell(shell string) string {
	s := `
name: ci
on: { push: {} }
jobs:
  build:
    steps:
      - run: echo hi`
	if shell != "" {
		s += "\n        shell: " + shell
	}
	return s + "\n"
}

func TestShellAccepted(t *testing.T) {
	for _, sh := range []string{"", "sh", "bash", "cmd", "powershell", "pwsh"} {
		if _, err := Parse([]byte(stepWithShell(sh))); err != nil {
			t.Errorf("shell %q: unexpected error: %v", sh, err)
		}
	}
}

func TestShellRejectsUnknown(t *testing.T) {
	_, err := Parse([]byte(stepWithShell("fish")))
	if err == nil {
		t.Fatal("expected an error for an unknown shell, got nil")
	}
	if !strings.Contains(err.Error(), "shell") {
		t.Errorf("error = %v, want it to mention `shell`", err)
	}
}

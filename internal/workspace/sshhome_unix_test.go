//go:build unix

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Real-filesystem cases the fakes only model: a known_hosts FIFO, which would
// block an open forever, and a .ssh the account cannot search.
func TestSSHHomeWarningRealFilesystem(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	account := func(home string) func(context.Context) (string, string, bool) {
		return func(context.Context) (string, string, bool) { return "svc", home, true }
	}

	t.Run("FIFO known_hosts neither blocks nor warns", func(t *testing.T) {
		home := t.TempDir()
		if err := os.Mkdir(filepath.Join(home, ".ssh"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(filepath.Join(home, ".ssh", "known_hosts"), 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		// Generous deadline: the check is that the probe *finishes* (an
		// os.Open of the FIFO would hang and hit it), not merely returns.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan string, 1)
		go func() {
			msg, _ := sshHomeProbe{username: "svc", home: home, stat: os.Stat, open: openClose,
				gitSSHConfigured: func() bool { return false }}.warning()
			done <- msg
		}()
		select {
		case msg := <-done:
			if msg != "" {
				t.Errorf("got warning %q for a FIFO known_hosts", msg)
			}
		case <-ctx.Done():
			t.Fatal("probe blocked on the FIFO known_hosts")
		}
		// And the public path stays bounded regardless.
		if msg, _ := sshHomeWarning(ctx, nil, account(home)); msg != "" {
			t.Errorf("sshHomeWarning = %q, want none", msg)
		}
	})

	t.Run("unsearchable .ssh is reported", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permissions")
		}
		home := t.TempDir()
		dir := filepath.Join(home, ".ssh")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "known_hosts"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o600); err != nil { // readable, not searchable
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		msg, _ := sshHomeWarning(context.Background(), nil, account(home))
		if !strings.Contains(msg, "cannot read its ~/.ssh") {
			t.Errorf("message = %q, want the unreadable-~/.ssh report", msg)
		}
	})
}

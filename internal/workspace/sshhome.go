package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// SSHHomeWarning checks, once at startup, whether ssh can reach the service
// account's ~/.ssh. It returns a log message and slog attributes describing
// why SSH checkouts will fail, or an empty message when nothing looks wrong
// or nothing can be determined.
//
// The trap it catches: OpenSSH resolves ~/.ssh from the account's passwd
// entry, not from $HOME. If the service account's home is under /home and the
// systemd unit sets ProtectHome, ssh cannot even open known_hosts. Every SSH
// checkout then fails with "Host key verification failed", and nothing points
// at the account's home as the cause. Reporting it at startup puts the cause
// in the journal before the first webhook fails.
func SSHHomeWarning(env []string) (string, []any) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return "", nil
	}
	return sshHomeWarning(u.Username, u.HomeDir, env, os.Stat)
}

// sshHomeWarning is SSHHomeWarning with the account and filesystem injected.
func sshHomeWarning(username, home string, env []string, stat func(string) (fs.FileInfo, error)) (string, []any) {
	// An operator-supplied ssh command may point at its own key and
	// known_hosts (-i, -o UserKnownHostsFile=…), so the passwd home proves
	// nothing about it.
	if envHas(env, "GIT_SSH_COMMAND") || envHas(env, "GIT_SSH") {
		return "", nil
	}
	dir := filepath.Join(home, ".ssh")
	_, err := stat(dir)
	if err == nil {
		return "", nil
	}
	attrs := []any{"dir", dir}
	// When $HOME and the passwd home disagree, an operator who provisioned
	// $HOME/.ssh will not see why ssh ignores it, so show both.
	if h, ok := envValue(env, "HOME"); ok && h != "" && filepath.Clean(h) != filepath.Clean(home) {
		attrs = append(attrs, "HOME", h)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return `clone_url is "ssh" but the service account has no ~/.ssh; ssh reads its key and known_hosts from the account's passwd home (not $HOME), so provision them there or SSH checkouts will fail`, attrs
	}
	attrs = append(attrs, "err", err)
	return fmt.Sprintf(`clone_url is "ssh" but the service account's ~/.ssh is not accessible; ssh reads its key and known_hosts from the account's passwd home (not $HOME), so SSH checkouts will fail with "Host key verification failed" — if systemd ProtectHome hides the home, move it out of /home (packaged unit: usermod -d /var/lib/janus %s)`, username), attrs
}

// envValue returns the value of name in env. The first assignment wins, as
// with getenv.
func envValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

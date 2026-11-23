package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
func SSHHomeWarning(ctx context.Context, env []string) (string, []any) {
	username, home, ok := passwdAccount(ctx)
	if !ok {
		return "", nil
	}
	return sshHomeProbe{
		username: username,
		home:     home,
		env:      env,
		stat:     os.Stat,
		open:     openClose,
		// Outside any repository, git config resolves the system and global
		// files (and GIT_CONFIG_* pairs), which is every place a
		// transport override can live that does not depend on the
		// repository being checked out.
		gitSSHConfigured: func() bool { return gitConfigSet(ctx, os.TempDir(), "core.sshCommand") },
	}.warning()
}

// passwdAccount returns the running account's name and home the way ssh
// resolves them: getpwuid, through NSS. It asks getent, which goes through
// NSS, and reads /etc/passwd only where getent is not installed. If neither
// lists the uid, it reports the account as unknown, and the check is skipped
// rather than run against the wrong directory.
//
// os/user can't do this. In cgo-less builds (the Linux release binaries)
// user.Current reads only /etc/passwd. For a uid missing there (an LDAP or
// SSSD account) it falls back to $USER and $HOME, and $HOME is exactly the
// variable ssh ignores. user.LookupId and user.Lookup are no way around it:
// both return the cached user.Current result for the running account.
func passwdAccount(ctx context.Context) (username, home string, ok bool) {
	uid := strconv.Itoa(os.Getuid())
	out, err := exec.CommandContext(ctx, "getent", "passwd", uid).Output()
	if errors.Is(err, exec.ErrNotFound) {
		out, err = os.ReadFile("/etc/passwd")
	}
	if err != nil {
		return "", "", false
	}
	return findPasswdEntry(string(out), uid)
}

// findPasswdEntry returns the name and home directory of uid's entry in db,
// which is in passwd(5) format.
func findPasswdEntry(db, uid string) (username, home string, ok bool) {
	for _, line := range strings.Split(db, "\n") {
		f := strings.Split(line, ":")
		if len(f) == 7 && f[2] == uid && f[5] != "" {
			return f[0], f[5], true
		}
	}
	return "", "", false
}

// sshHomeProbe is SSHHomeWarning with the account, filesystem, and gitconfig
// injected.
type sshHomeProbe struct {
	username, home   string
	env              []string
	stat             func(string) (fs.FileInfo, error)
	open             func(string) error // open for reading, then close
	gitSSHConfigured func() bool
}

const sshHomeWhy = `ssh reads its key and known_hosts from the account's passwd home, not $HOME`

func (p sshHomeProbe) warning() (string, []any) {
	msg, attrs := p.diagnose()
	// An operator-configured ssh command may carry its own key and
	// known_hosts (-i, -o UserKnownHostsFile=…), so an unusable passwd home
	// proves nothing about it. The same overrides gitEnv defers to. Checked
	// only once there is something to report, so a healthy host never runs
	// git at startup.
	if msg == "" || envHas(p.env, "GIT_SSH_COMMAND") || envHas(p.env, "GIT_SSH") || p.gitSSHConfigured() {
		return "", nil
	}
	return msg, attrs
}

func (p sshHomeProbe) diagnose() (string, []any) {
	dir := filepath.Join(p.home, ".ssh")
	attrs := []any{"dir", dir}
	// When $HOME and the passwd home disagree, an operator who provisioned
	// $HOME/.ssh will not see why ssh ignores it, so show both.
	if h, ok := envValue(p.env, "HOME"); ok && h != "" && filepath.Clean(h) != filepath.Clean(p.home) {
		attrs = append(attrs, "HOME", h)
	}

	info, err := p.stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// A gitdir-scoped core.sshCommand (includeIf) only resolves inside a
		// checkout, so it cannot be ruled out here. Hence "unless".
		return `clone_url is "ssh" but the service account has no ~/.ssh; ` + sshHomeWhy +
			`, so SSH checkouts will fail unless a key and known_hosts are provisioned there (or an includeIf-scoped core.sshCommand points elsewhere)`, attrs
	case err == nil && !info.IsDir():
		err = fmt.Errorf("%s is not a directory", dir)
	case err == nil:
		// stat needs search permission only on the parents, but ssh must also
		// enter dir itself. Opening known_hosts tests that, and the file's
		// read permission, the way ssh will. A missing known_hosts is left
		// alone: /etc/ssh/ssh_known_hosts may hold the host key.
		if err = p.open(filepath.Join(dir, "known_hosts")); errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	}
	if err == nil {
		return "", nil
	}
	attrs = append(attrs, "err", err)
	return fmt.Sprintf(`clone_url is "ssh" but the service account cannot read its ~/.ssh; %s, so SSH checkouts will fail with "Host key verification failed" unless an includeIf-scoped core.sshCommand points elsewhere. If systemd ProtectHome hides the home, move it out of /home (packaged unit: usermod -d /var/lib/janus %s); otherwise make %s own the directory and its files`,
		sshHomeWhy, p.username, p.username), attrs
}

// openClose opens path for reading and closes it again.
func openClose(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return f.Close()
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

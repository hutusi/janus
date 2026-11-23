package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// SSHHomeWarning checks, once at startup, whether the service account's
// ~/.ssh is usable. It returns a log message and slog attributes reporting
// what it found wrong, or an empty message when nothing looks wrong or
// nothing can be determined.
//
// The trap it catches: OpenSSH resolves ~/.ssh from the account's passwd
// entry, not from $HOME. If the service account's home is under /home and the
// systemd unit sets ProtectHome, ssh cannot even open known_hosts, and SSH
// checkouts fail with "Host key verification failed" with nothing pointing at
// the account's home. Reporting it at startup puts the cause in the journal
// before the first webhook fails.
//
// It is a diagnostic, so it reports what it observed and never claims more:
// ssh can still succeed through an agent, a global known_hosts, or an
// ssh_config this probe cannot see. It also never holds up startup: it
// returns empty when ctx is done, whatever it is blocked on (a hung network
// home, a slow directory service).
func SSHHomeWarning(ctx context.Context, env []string) (string, []any) {
	return sshHomeWarning(ctx, env, passwdAccount)
}

func sshHomeWarning(ctx context.Context, env []string, lookup func(context.Context) (string, string, bool)) (string, []any) {
	// An operator-configured ssh command may carry its own key and
	// known_hosts (-i, -o UserKnownHostsFile=…), making the home moot. These
	// are the overrides gitEnv defers to. Checked first: they cost nothing and
	// spare every filesystem call.
	if envHas(env, "GIT_SSH_COMMAND") || envHas(env, "GIT_SSH") {
		return "", nil
	}
	type result struct {
		msg   string
		attrs []any
	}
	done := make(chan result, 1) // buffered: an abandoned probe must not leak blocked on send
	go func() {
		username, home, ok := lookup(ctx)
		if !ok {
			done <- result{}
			return
		}
		msg, attrs := sshHomeProbe{
			username: username,
			home:     home,
			env:      env,
			stat:     os.Stat,
			open:     openClose,
			// Outside any repository, git config resolves the system and
			// global files (and GIT_CONFIG_* pairs): every place an override
			// can live that does not depend on the repository being checked
			// out.
			gitSSHConfigured: func() bool { return gitConfigSet(ctx, os.TempDir(), "core.sshCommand") },
			systemdHint:      runtime.GOOS == "linux",
		}.warning()
		done <- result{msg, attrs}
	}()
	select {
	case r := <-done:
		return r.msg, r.attrs
	case <-ctx.Done():
		return "", nil
	}
}

// passwdAccount returns the running account's name and home the way ssh
// resolves them, or ok=false when that can't be established. Then the check is
// skipped rather than run against the wrong directory.
//
//   - Linux and other Unix: getpwuid goes through NSS, so ask getent, which
//     does too, and read /etc/passwd only where getent is not installed.
//     os/user can't be used here. In cgo-less builds (the release binaries)
//     user.Current reads only /etc/passwd and, for a uid missing there (an LDAP
//     or SSSD account), falls back to $USER and $HOME, the variable ssh
//     ignores. user.LookupId and user.Lookup return that same cached result
//     for the running account.
//   - macOS: user.Current calls the native getpwuid_r even without cgo, so
//     it agrees with ssh and has no $HOME fallback.
//   - Windows: skipped. Which home ssh reads depends on which ssh git runs.
//     Git for Windows' bundled ssh follows $HOME, Windows OpenSSH follows the
//     profile directory, so there is no single answer to check. The trap this
//     probe exists for (a systemd sandbox hiding the passwd home) doesn't
//     arise there either.
func passwdAccount(ctx context.Context) (username, home string, ok bool) {
	switch runtime.GOOS {
	case "windows":
		return "", "", false
	case "darwin":
		u, err := user.Current()
		if err != nil || u.HomeDir == "" {
			return "", "", false
		}
		return u.Username, u.HomeDir, true
	}
	return getentAccount(ctx)
}

// getentAccount is passwdAccount's NSS path: `getent passwd <uid>`, falling
// back to /etc/passwd where getent is not installed.
func getentAccount(ctx context.Context) (username, home string, ok bool) {
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

// sshHomeProbe is the filesystem half of SSHHomeWarning, with the account,
// filesystem, and gitconfig injected.
type sshHomeProbe struct {
	username, home   string
	env              []string
	stat             func(string) (fs.FileInfo, error)
	open             func(string) error // open for reading, then close
	gitSSHConfigured func() bool
	systemdHint      bool // add the ProtectHome remedy (Linux)
}

func (p sshHomeProbe) warning() (string, []any) {
	msg, attrs := p.diagnose()
	// Asked only once there is something to report, so a healthy host never
	// spawns git at startup. An includeIf "gitdir:" core.sshCommand only
	// resolves inside a checkout and can't be seen here, which is one reason
	// the messages say what was found and not that checkouts will fail.
	if msg == "" || p.gitSSHConfigured() {
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
		return `clone_url is "ssh" but the service account has no ~/.ssh (ssh reads it from the account's passwd home, not $HOME); SSH checkouts that rely on a key or known_hosts there will fail`, attrs
	case err == nil && !info.IsDir():
		err = fmt.Errorf("%s is not a directory", dir)
	case err == nil:
		err = p.knownHostsReadable(filepath.Join(dir, "known_hosts"))
	}
	if err == nil {
		return "", nil
	}
	attrs = append(attrs, "err", err)
	fix := fmt.Sprintf(`Make sure %s owns the directory and its files.`, p.username)
	if p.systemdHint {
		fix = fmt.Sprintf(`If systemd ProtectHome hides the home, move it out of /home (packaged unit: usermod -d /var/lib/janus %s); otherwise make sure %s owns the directory and its files.`, p.username, p.username)
	}
	return `clone_url is "ssh" but the service account cannot read its ~/.ssh (ssh reads it from the account's passwd home, not $HOME); SSH checkouts that rely on a key or known_hosts there will fail, typically with "Host key verification failed". ` + fix, attrs
}

// knownHostsReadable reports whether ssh could read path. Stat comes first:
// it fails with a permission error when the directory can't be searched,
// which a stat of the directory itself does not catch. A directory is
// reported from its mode. Only a regular file is opened, to test read
// permission. Opening a FIFO with no writer would block, so other special
// files are let through unopened. A missing known_hosts is fine:
// /etc/ssh/ssh_known_hosts may hold the host key.
func (p sshHomeProbe) knownHostsReadable(path string) error {
	info, err := p.stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case info.IsDir():
		// ssh can't load host keys from a directory. Stat alone says so,
		// with no blocking open needed.
		return fmt.Errorf("%s is a directory", path)
	case !info.Mode().IsRegular():
		return nil
	}
	return p.open(path)
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

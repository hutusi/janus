package workspace

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestSSHHomeWarning(t *testing.T) {
	dirInfo, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	statAs := func(info fs.FileInfo, err error) func(string) (fs.FileInfo, error) {
		return func(p string) (fs.FileInfo, error) {
			if err != nil {
				return nil, &fs.PathError{Op: "stat", Path: p, Err: err}
			}
			return info, nil
		}
	}
	openAs := func(err error) func(string) error {
		return func(p string) error {
			if err != nil {
				return &fs.PathError{Op: "open", Path: p, Err: err}
			}
			return nil
		}
	}
	home := filepath.FromSlash("/home/janus")
	sshDir := filepath.Join(home, ".ssh")
	inaccessible := []string{"cannot read its ~/.ssh", "passwd home, not $HOME", "Host key verification failed", "usermod -d /var/lib/janus janus", "make janus own"}

	tests := []struct {
		name      string
		env       []string
		stat      func(string) (fs.FileInfo, error)
		open      func(string) error
		gitSSH    bool
		want      []string // substrings of the message; nil means no warning
		wantAttrs []string
	}{
		{name: "readable known_hosts is silent", stat: statAs(dirInfo, nil), open: openAs(nil)},
		// /etc/ssh/ssh_known_hosts may hold the key; not this probe's call.
		{name: "missing known_hosts is silent", stat: statAs(dirInfo, nil), open: openAs(fs.ErrNotExist)},
		{
			name: "ProtectHome-style permission denied",
			stat: statAs(nil, fs.ErrPermission), open: openAs(nil),
			want: inaccessible, wantAttrs: []string{"dir", "err"},
		},
		{
			// stat succeeds with search permission on the parents alone; ssh
			// still cannot enter a root-owned 0700 ~/.ssh.
			name: "no search permission on ~/.ssh",
			stat: statAs(dirInfo, nil), open: openAs(fs.ErrPermission),
			want: inaccessible, wantAttrs: []string{"dir", "err"},
		},
		{
			name: "~/.ssh is a regular file",
			stat: statAs(fileInfo, nil), open: openAs(nil),
			want: inaccessible, wantAttrs: []string{"dir", "err"},
		},
		{
			name: "missing ~/.ssh asks for provisioning",
			stat: statAs(nil, fs.ErrNotExist), open: openAs(nil),
			want: []string{"has no ~/.ssh", "provisioned there", "includeIf-scoped core.sshCommand"}, wantAttrs: []string{"dir"},
		},
		{
			// The trap itself: an operator points $HOME elsewhere and
			// provisions keys there, but ssh still looks in the passwd home.
			name: "HOME disagreeing with the passwd home is shown",
			env:  []string{"HOME=/var/lib/janus"},
			stat: statAs(nil, fs.ErrPermission), open: openAs(nil),
			want: inaccessible, wantAttrs: []string{"dir", "HOME", "err"},
		},
		{name: "HOME matching the passwd home is not shown", env: []string{"HOME=" + home + "/"},
			stat: statAs(nil, fs.ErrPermission), open: openAs(nil),
			want: inaccessible, wantAttrs: []string{"dir", "err"}},
		{name: "GIT_SSH_COMMAND takes over the transport", env: []string{"GIT_SSH_COMMAND=ssh -i /k"},
			stat: statAs(nil, fs.ErrPermission), open: openAs(nil)},
		{name: "GIT_SSH takes over the transport", env: []string{"GIT_SSH=/usr/bin/plink"},
			stat: statAs(nil, fs.ErrNotExist), open: openAs(nil)},
		{name: "core.sshCommand takes over the transport", gitSSH: true,
			stat: statAs(nil, fs.ErrPermission), open: openAs(nil)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var statted, opened string
			gitProbed := false
			p := sshHomeProbe{
				username: "janus", home: home, env: tc.env,
				stat:             func(s string) (fs.FileInfo, error) { statted = s; return tc.stat(s) },
				open:             func(s string) error { opened = s; return tc.open(s) },
				gitSSHConfigured: func() bool { gitProbed = true; return tc.gitSSH },
			}
			msg, attrs := p.warning()
			if statted != sshDir {
				t.Errorf("stat(%q), want stat(%q): the passwd home, not $HOME", statted, sshDir)
			}
			if opened != "" && opened != filepath.Join(sshDir, "known_hosts") {
				t.Errorf("opened %q, want ~/.ssh/known_hosts", opened)
			}
			if tc.want == nil {
				if msg != "" {
					t.Fatalf("got warning %q, want none", msg)
				}
				return
			}
			if !gitProbed {
				t.Error("warning issued without consulting core.sshCommand")
			}
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("message %q lacks %q", msg, w)
				}
			}
			var keys []string
			for i := 0; i+1 < len(attrs); i += 2 {
				keys = append(keys, attrs[i].(string))
			}
			if strings.Join(keys, ",") != strings.Join(tc.wantAttrs, ",") {
				t.Errorf("attr keys = %v, want %v", keys, tc.wantAttrs)
			}
			if attrs[1] != sshDir {
				t.Errorf("dir attr = %v, want %q", attrs[1], sshDir)
			}
		})
	}
}

// A healthy host must not spawn git at startup just to learn that nothing
// needs reporting.
func TestSSHHomeWarningSkipsGitProbeWhenHealthy(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := sshHomeProbe{
		username: "janus", home: "/home/janus",
		stat:             func(string) (fs.FileInfo, error) { return info, nil },
		open:             func(string) error { return nil },
		gitSSHConfigured: func() bool { t.Error("git probed on a healthy host"); return false },
	}
	if msg, _ := p.warning(); msg != "" {
		t.Fatalf("got warning %q, want none", msg)
	}
}

// stubGetent puts a getent first on PATH that answers `getent passwd <uid>`
// for the running uid with an entry whose home is home — standing in for an
// NSS-backed account database the test cannot otherwise control.
func stubGetent(t *testing.T, home string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub getent is a shell script")
	}
	bin := t.TempDir()
	uid := strconv.Itoa(os.Getuid())
	script := "#!/bin/sh\n[ \"$1\" = passwd ] && [ \"$2\" = " + uid + " ] || exit 2\n" +
		"echo 'svc:x:" + uid + ":" + uid + "::" + home + ":/usr/sbin/nologin'\n"
	if err := os.WriteFile(filepath.Join(bin, "getent"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// End to end through the real lookup and filesystem: a home with no ~/.ssh
// warns, and each operator-configured transport silences it.
func TestSSHHomeWarningEndToEnd(t *testing.T) {
	home := t.TempDir() // exists, but has no .ssh
	stubGetent(t, home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	ctx := context.Background()

	msg, attrs := SSHHomeWarning(ctx, nil)
	if !strings.Contains(msg, "has no ~/.ssh") {
		t.Fatalf("message = %q, want the missing-~/.ssh warning", msg)
	}
	if want := filepath.Join(home, ".ssh"); len(attrs) < 2 || attrs[1] != want {
		t.Errorf("attrs = %v, want dir %q", attrs, want)
	}
	if msg, _ := SSHHomeWarning(ctx, []string{"GIT_SSH_COMMAND=ssh"}); msg != "" {
		t.Errorf("got warning %q with GIT_SSH_COMMAND set", msg)
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[core]\n\tsshCommand = ssh -i /k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	if msg, _ := SSHHomeWarning(ctx, nil); msg != "" {
		t.Errorf("got warning %q with core.sshCommand in the global gitconfig", msg)
	}
}

// The account comes from the account database (getent, so NSS), never from
// $HOME, which is the variable ssh ignores. user.Current breaks this in
// cgo-less builds; reproduce with `go test -tags osusergo`.
func TestPasswdAccountIgnoresHOME(t *testing.T) {
	stubGetent(t, "/srv/from-nss")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USER", "not-the-account")
	name, home, ok := passwdAccount(context.Background())
	if !ok || name != "svc" || home != "/srv/from-nss" {
		t.Fatalf("passwdAccount = %q, %q, %v; want svc, /srv/from-nss from getent", name, home, ok)
	}
}

func TestFindPasswdEntry(t *testing.T) {
	db := "root:x:0:0:root:/root:/bin/bash\n" +
		"# comment\n" +
		"broken:x:999\n" +
		"nohome:x:998:998:::/bin/sh\n" +
		"janus:x:997:997::/var/lib/janus:/usr/sbin/nologin\n"
	for _, tc := range []struct {
		uid, name, home string
		ok              bool
	}{
		{"997", "janus", "/var/lib/janus", true},
		{"0", "root", "/root", true},
		{"999", "", "", false}, // malformed line
		{"998", "", "", false}, // empty home: nothing to check
		{"12345", "", "", false},
	} {
		name, home, ok := findPasswdEntry(db, tc.uid)
		if name != tc.name || home != tc.home || ok != tc.ok {
			t.Errorf("findPasswdEntry(%s) = %q, %q, %v; want %q, %q, %v", tc.uid, name, home, ok, tc.name, tc.home, tc.ok)
		}
	}
}

func TestEnvValue(t *testing.T) {
	env := []string{"A=1", "HOME=/first", "NOEQUALS", "HOME=/second", "EMPTY="}
	if v, ok := envValue(env, "HOME"); !ok || v != "/first" {
		t.Errorf("HOME = %q, %v; want the first assignment, as getenv does", v, ok)
	}
	if v, ok := envValue(env, "EMPTY"); !ok || v != "" {
		t.Errorf("EMPTY = %q, %v; want set-but-empty", v, ok)
	}
	if _, ok := envValue(env, "MISSING"); ok {
		t.Error("MISSING reported as set")
	}
}

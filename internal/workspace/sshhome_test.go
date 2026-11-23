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
	"time"
)

// fakeInfo is a FileInfo of a given mode.
type fakeInfo fs.FileMode

func (f fakeInfo) Name() string       { return "" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return fs.FileMode(f) }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return fs.FileMode(f).IsDir() }
func (f fakeInfo) Sys() any           { return nil }

// fakeEntry is one path of a fakeFS: what stat returns, and what open returns
// for it.
type fakeEntry struct {
	info             fs.FileInfo
	statErr, openErr error
}

// fakeFS serves stat and open from a map, recording every path opened.
type fakeFS struct {
	entries map[string]fakeEntry
	opened  []string
}

func (f *fakeFS) stat(p string) (fs.FileInfo, error) {
	e, ok := f.entries[p]
	switch {
	case !ok:
		return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	case e.statErr != nil:
		return nil, &fs.PathError{Op: "stat", Path: p, Err: e.statErr}
	}
	return e.info, nil
}

func (f *fakeFS) open(p string) error {
	f.opened = append(f.opened, p)
	if e := f.entries[p]; e.openErr != nil {
		return &fs.PathError{Op: "open", Path: p, Err: e.openErr}
	}
	return nil
}

func TestSSHHomeProbe(t *testing.T) {
	home := filepath.FromSlash("/home/janus")
	dir := filepath.Join(home, ".ssh")
	kh := filepath.Join(dir, "known_hosts")
	sshDir := fakeEntry{info: fakeInfo(fs.ModeDir | 0o700)}
	readable := fakeEntry{info: fakeInfo(0o600)}
	inaccessible := []string{"cannot read its ~/.ssh (", "passwd home, not $HOME", "checkouts that rely on a key or known_hosts there will fail", "Host key verification failed"}
	// Only the file is unusable: the key in the directory still works, so
	// the message claims less and offers no re-homing.
	unreadableFile := []string{"cannot read its ~/.ssh/known_hosts", "rely on host keys in that file will fail", "Host key verification failed", "Make sure janus owns it"}
	fileOnlyNot := []string{"cannot read its ~/.ssh (", "a key or known_hosts", "usermod"}

	tests := []struct {
		name       string
		entries    map[string]fakeEntry
		env        []string
		gitSSH     bool
		want       []string // substrings of the message; nil means no warning
		notWant    []string // substrings the message must not contain
		wantAttrs  []string
		wantOpened []string
	}{
		{name: "readable known_hosts is silent", entries: map[string]fakeEntry{dir: sshDir, kh: readable}, wantOpened: []string{kh}},
		// /etc/ssh/ssh_known_hosts may hold the key; not this probe's call.
		{name: "missing known_hosts is silent", entries: map[string]fakeEntry{dir: sshDir}},
		{
			name:      "missing ~/.ssh is reported",
			entries:   map[string]fakeEntry{},
			want:      []string{"has no ~/.ssh", "passwd home, not $HOME", "checkouts that rely on a key or known_hosts there will fail"},
			wantAttrs: []string{"dir"},
		},
		{
			name:      "ProtectHome-style permission denied",
			entries:   map[string]fakeEntry{dir: {statErr: fs.ErrPermission}},
			want:      inaccessible,
			wantAttrs: []string{"dir", "err"},
		},
		{
			// stat of the directory needs search permission only on the
			// parents; ssh must also enter it — a root-owned 0700 .ssh.
			name:      "no search permission on ~/.ssh",
			entries:   map[string]fakeEntry{dir: sshDir, kh: {statErr: fs.ErrPermission}},
			want:      inaccessible,
			wantAttrs: []string{"dir", "err"},
		},
		{
			name:       "unreadable known_hosts",
			entries:    map[string]fakeEntry{dir: sshDir, kh: {info: fakeInfo(0o600), openErr: fs.ErrPermission}},
			want:       unreadableFile,
			notWant:    fileOnlyNot,
			wantAttrs:  []string{"dir", "err"},
			wantOpened: []string{kh},
		},
		{
			name:      "~/.ssh is a regular file",
			entries:   map[string]fakeEntry{dir: readable},
			want:      inaccessible,
			wantAttrs: []string{"dir", "err"},
		},
		{
			// ssh can't load host keys from a directory; the mode says so
			// without opening anything.
			name:      "directory at known_hosts",
			entries:   map[string]fakeEntry{dir: sshDir, kh: sshDir},
			want:      unreadableFile,
			notWant:   fileOnlyNot,
			wantAttrs: []string{"dir", "err"},
		},
		{
			// Opening a FIFO with no writer blocks: never open special files.
			name:    "FIFO known_hosts is not opened",
			entries: map[string]fakeEntry{dir: sshDir, kh: {info: fakeInfo(fs.ModeNamedPipe | 0o600)}},
		},
		{
			// The trap itself: an operator points $HOME elsewhere and
			// provisions keys there, but ssh still looks in the passwd home.
			name:      "HOME disagreeing with the passwd home is shown",
			entries:   map[string]fakeEntry{dir: {statErr: fs.ErrPermission}},
			env:       []string{"HOME=/var/lib/janus"},
			want:      inaccessible,
			wantAttrs: []string{"dir", "HOME", "err"},
		},
		{
			name:      "HOME matching the passwd home is not shown",
			entries:   map[string]fakeEntry{dir: {statErr: fs.ErrPermission}},
			env:       []string{"HOME=" + home + string(filepath.Separator)},
			want:      inaccessible,
			wantAttrs: []string{"dir", "err"},
		},
		{
			name:    "core.sshCommand takes over the transport",
			entries: map[string]fakeEntry{dir: {statErr: fs.ErrPermission}},
			gitSSH:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := &fakeFS{entries: tc.entries}
			gitProbed := false
			p := sshHomeProbe{
				username: "janus", home: home, env: tc.env,
				stat: fsys.stat, open: fsys.open,
				gitSSHConfigured: func() bool { gitProbed = true; return tc.gitSSH },
				systemdHint:      true,
			}
			msg, attrs := p.warning()
			if strings.Join(fsys.opened, ",") != strings.Join(tc.wantOpened, ",") {
				t.Errorf("opened %v, want %v", fsys.opened, tc.wantOpened)
			}
			if tc.want == nil {
				if msg != "" {
					t.Fatalf("got warning %q, want none", msg)
				}
				if gitProbed && !tc.gitSSH {
					t.Error("git probed although there was nothing to report")
				}
				return
			}
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("message %q lacks %q", msg, w)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(msg, w) {
					t.Errorf("message %q should not contain %q", msg, w)
				}
			}
			var keys []string
			for i := 0; i+1 < len(attrs); i += 2 {
				keys = append(keys, attrs[i].(string))
			}
			if strings.Join(keys, ",") != strings.Join(tc.wantAttrs, ",") {
				t.Errorf("attr keys = %v, want %v", keys, tc.wantAttrs)
			}
			if attrs[1] != dir {
				t.Errorf("dir attr = %v, want %q", attrs[1], dir)
			}
		})
	}
}

// The ProtectHome remedy is systemd advice: Linux only. Elsewhere the message
// keeps only the ownership hint.
func TestSSHHomeProbeRemedy(t *testing.T) {
	for _, systemd := range []bool{true, false} {
		fsys := &fakeFS{entries: map[string]fakeEntry{filepath.FromSlash("/h/.ssh"): {statErr: fs.ErrPermission}}}
		p := sshHomeProbe{username: "janus", home: filepath.FromSlash("/h"), stat: fsys.stat, open: fsys.open,
			gitSSHConfigured: func() bool { return false }, systemdHint: systemd}
		msg, _ := p.warning()
		if got := strings.Contains(msg, "usermod -d /var/lib/janus janus"); got != systemd {
			t.Errorf("systemdHint=%v: usermod remedy present = %v in %q", systemd, got, msg)
		}
		if !strings.Contains(strings.ToLower(msg), "make sure janus owns the directory") {
			t.Errorf("systemdHint=%v: ownership hint missing from %q", systemd, msg)
		}
	}
}

// Environment overrides are decided before any lookup or filesystem call.
func TestSSHHomeWarningEnvOverridesComeFirst(t *testing.T) {
	for _, env := range [][]string{{"GIT_SSH_COMMAND=ssh -i /k"}, {"GIT_SSH=/usr/bin/plink"}} {
		lookup := func(context.Context) (string, string, bool) {
			t.Errorf("%v: account looked up despite the override", env)
			return "", "", false
		}
		if msg, _ := sshHomeWarning(context.Background(), env, lookup); msg != "" {
			t.Errorf("%v: got warning %q", env, msg)
		}
	}
}

// A diagnostic must never hold up startup: whatever the probe blocks on, the
// caller gets an answer when its context ends.
func TestSSHHomeWarningNeverBlocksStartup(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	lookup := func(context.Context) (string, string, bool) {
		<-release // a hung network home or directory service
		return "", "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if msg, _ := sshHomeWarning(ctx, nil, lookup); msg != "" {
		t.Errorf("got warning %q from a probe that never finished", msg)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("returned after %v; want it bounded by the context", d)
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

// End to end through getent, the real filesystem, and real git: a home with
// no ~/.ssh is reported, and core.sshCommand silences it.
func TestSSHHomeWarningEndToEnd(t *testing.T) {
	home := t.TempDir() // exists, but has no .ssh
	stubGetent(t, home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	ctx := context.Background()

	msg, attrs := sshHomeWarning(ctx, nil, getentAccount)
	if !strings.Contains(msg, "has no ~/.ssh") {
		t.Fatalf("message = %q, want the missing-~/.ssh report", msg)
	}
	if want := filepath.Join(home, ".ssh"); len(attrs) < 2 || attrs[1] != want {
		t.Errorf("attrs = %v, want dir %q", attrs, want)
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[core]\n\tsshCommand = ssh -i /k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	if msg, _ := sshHomeWarning(ctx, nil, getentAccount); msg != "" {
		t.Errorf("got warning %q with core.sshCommand in the global gitconfig", msg)
	}
}

// On Linux the account comes from getent (NSS), never from $HOME, which is
// the variable ssh ignores. user.Current breaks this in cgo-less Linux builds
// for accounts missing from /etc/passwd; reproduce that failure mode with
// `go test -tags osusergo`.
func TestGetentAccountUsesNSS(t *testing.T) {
	stubGetent(t, "/srv/from-nss")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USER", "not-the-account")
	name, home, ok := getentAccount(context.Background())
	if !ok || name != "svc" || home != "/srv/from-nss" {
		t.Fatalf("getentAccount = %q, %q, %v; want svc, /srv/from-nss from getent", name, home, ok)
	}
}

// Per platform: the native lookup on macOS ignores $HOME, Windows is skipped
// (see passwdAccount), and everything else goes through getent.
func TestPasswdAccountPerPlatform(t *testing.T) {
	ctx := context.Background()
	fake := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		if _, _, ok := passwdAccount(ctx); ok {
			t.Fatal("passwdAccount resolved an account on Windows; the check is meant to be skipped there")
		}
	case "darwin":
		t.Setenv("HOME", fake)
		if _, home, ok := passwdAccount(ctx); !ok || filepath.Clean(home) == filepath.Clean(fake) {
			t.Fatalf("passwdAccount = %q, %v; want the account's real home, not $HOME", home, ok)
		}
	default:
		stubGetent(t, "/srv/from-nss")
		t.Setenv("HOME", fake)
		if _, home, ok := passwdAccount(ctx); !ok || home != "/srv/from-nss" {
			t.Fatalf("passwdAccount = %q, %v; want /srv/from-nss from getent", home, ok)
		}
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

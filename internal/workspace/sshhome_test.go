package workspace

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHHomeWarning(t *testing.T) {
	okDir := t.TempDir()
	statOK := func(string) (fs.FileInfo, error) { return os.Stat(okDir) }
	statErr := func(err error) func(string) (fs.FileInfo, error) {
		return func(p string) (fs.FileInfo, error) {
			return nil, &fs.PathError{Op: "stat", Path: p, Err: err}
		}
	}
	home := filepath.FromSlash("/home/janus")
	sshDir := filepath.Join(home, ".ssh")

	tests := []struct {
		name      string
		env       []string
		stat      func(string) (fs.FileInfo, error)
		want      []string // substrings of the message; nil means no warning
		wantAttrs map[string]bool
	}{
		{name: "reachable ~/.ssh is silent", env: []string{"HOME=" + home}, stat: statOK},
		{
			name:      "ProtectHome-style permission denied",
			env:       []string{"HOME=" + home},
			stat:      statErr(fs.ErrPermission),
			want:      []string{"not accessible", "passwd home (not $HOME)", "Host key verification failed", "usermod -d /var/lib/janus janus"},
			wantAttrs: map[string]bool{"dir": true, "err": true},
		},
		{
			name:      "missing ~/.ssh asks for provisioning",
			env:       []string{"HOME=" + home},
			stat:      statErr(fs.ErrNotExist),
			want:      []string{"has no ~/.ssh", "provision them there"},
			wantAttrs: map[string]bool{"dir": true},
		},
		{
			// The trap itself: an operator points $HOME elsewhere and
			// provisions keys there, but ssh still looks in the passwd home.
			name:      "HOME disagreeing with the passwd home is shown",
			env:       []string{"HOME=/var/lib/janus"},
			stat:      statErr(fs.ErrPermission),
			want:      []string{"not accessible"},
			wantAttrs: map[string]bool{"dir": true, "err": true, "HOME": true},
		},
		{name: "GIT_SSH_COMMAND takes over the transport", env: []string{"GIT_SSH_COMMAND=ssh -i /k"}, stat: statErr(fs.ErrPermission)},
		{name: "GIT_SSH takes over the transport", env: []string{"GIT_SSH=/usr/bin/plink"}, stat: statErr(fs.ErrNotExist)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var statted string
			stat := func(p string) (fs.FileInfo, error) { statted = p; return tc.stat(p) }
			msg, attrs := sshHomeWarning("janus", home, tc.env, stat)
			if tc.want == nil {
				if msg != "" {
					t.Fatalf("got warning %q, want none", msg)
				}
				return
			}
			if statted != sshDir {
				t.Errorf("stat(%q), want stat(%q) — the passwd home, not $HOME", statted, sshDir)
			}
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("message %q lacks %q", msg, w)
				}
			}
			got := map[string]bool{}
			for i := 0; i+1 < len(attrs); i += 2 {
				got[attrs[i].(string)] = true
			}
			for k := range tc.wantAttrs {
				if !got[k] {
					t.Errorf("attrs %v lack %q", attrs, k)
				}
			}
			for k := range got {
				if !tc.wantAttrs[k] {
					t.Errorf("unexpected attr %q in %v", k, attrs)
				}
			}
			if attrs[1] != sshDir {
				t.Errorf("dir attr = %v, want %q", attrs[1], sshDir)
			}
		})
	}
}

// The exported entry point resolves the real account; whatever the machine's
// ~/.ssh looks like, an operator-configured transport must keep it silent.
func TestSSHHomeWarningRespectsConfiguredTransport(t *testing.T) {
	if msg, _ := SSHHomeWarning([]string{"GIT_SSH_COMMAND=ssh"}); msg != "" {
		t.Fatalf("got warning %q with GIT_SSH_COMMAND set", msg)
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

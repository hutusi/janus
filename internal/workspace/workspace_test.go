package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initGitRepo creates a repo with a .janus/ci.yml commit and returns its path
// and HEAD SHA.
func initGitRepo(t *testing.T) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")

	if err := os.MkdirAll(filepath.Join(dir, ".janus"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".janus", "ci.yml"), []byte("name: ci\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "initial")
	return dir, run("rev-parse", "HEAD")
}

// commitFile writes name/content in the repo at dir and commits it, returning
// the new HEAD SHA.
func commitFile(t *testing.T, dir, name, content string) (sha string) {
	t.Helper()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "update "+name)
	return run("rev-parse", "HEAD")
}

func TestCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	src, sha := initGitRepo(t)

	ws, err := Checkout(context.Background(), Options{
		Dir:     filepath.Join(t.TempDir(), "ws"),
		RepoURL: src,
		SHA:     sha,
		Ref:     "refs/heads/main", // fallback if fetch-by-SHA is refused
	})
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	defer func() { _ = ws.Cleanup() }()

	// The pipeline file from the commit is present in the checkout.
	got, err := os.ReadFile(filepath.Join(ws.Dir, ".janus", "ci.yml"))
	if err != nil {
		t.Fatalf("read checked-out pipeline: %v", err)
	}
	if string(got) != "name: ci\n" {
		t.Errorf("checked-out file = %q, want \"name: ci\\n\"", got)
	}

	// HEAD is detached at the requested SHA.
	out, err := exec.Command("git", "-C", ws.Dir, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != sha {
		t.Errorf("checked-out HEAD = %s, want %s", got, sha)
	}
}

func TestCheckoutCleanup(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	src, sha := initGitRepo(t)
	dir := filepath.Join(t.TempDir(), "ws")

	ws, err := Checkout(context.Background(), Options{Dir: dir, RepoURL: src, SHA: sha, Ref: "refs/heads/main"})
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if err := ws.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("workspace dir still exists after Cleanup: %v", err)
	}
}

func TestCheckoutReuseUpdatesInPlace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	src, sha1 := initGitRepo(t)
	dir := filepath.Join(t.TempDir(), "ws")

	ws, err := Checkout(context.Background(), Options{Dir: dir, RepoURL: src, SHA: sha1, Ref: "refs/heads/main", Keep: true, Reuse: true})
	if err != nil {
		t.Fatalf("first Checkout: %v", err)
	}
	// Untracked files (dependency/build caches) must survive the update.
	marker := filepath.Join(ws.Dir, "node_modules_marker")
	if err := os.WriteFile(marker, []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}

	sha2 := commitFile(t, src, "new.txt", "v2\n")
	ws, err = Checkout(context.Background(), Options{Dir: dir, RepoURL: src, SHA: sha2, Ref: "refs/heads/main", Keep: true, Reuse: true})
	if err != nil {
		t.Fatalf("reuse Checkout: %v", err)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Errorf("untracked marker should survive the in-place update: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(ws.Dir, "new.txt")); err != nil || string(got) != "v2\n" {
		t.Errorf("new commit's file = %q, %v; want \"v2\\n\"", got, err)
	}
	out, err := exec.Command("git", "-C", ws.Dir, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != sha2 {
		t.Errorf("HEAD after reuse = %s, want %s", got, sha2)
	}
}

func TestCheckoutReuseForcePush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	src, sha1 := initGitRepo(t)
	dir := filepath.Join(t.TempDir(), "ws")

	if _, err := Checkout(context.Background(), Options{Dir: dir, RepoURL: src, SHA: sha1, Ref: "refs/heads/main", Keep: true, Reuse: true}); err != nil {
		t.Fatalf("first Checkout: %v", err)
	}

	// Rewrite history in the source (as a force-push would).
	amend := exec.Command("git", "-C", src, "commit", "-q", "--amend", "-m", "rewritten")
	if out, err := amend.CombinedOutput(); err != nil {
		t.Fatalf("amend: %v\n%s", err, out)
	}
	out, err := exec.Command("git", "-C", src, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	sha2 := strings.TrimSpace(string(out))

	ws, err := Checkout(context.Background(), Options{Dir: dir, RepoURL: src, SHA: sha2, Ref: "refs/heads/main", Keep: true, Reuse: true})
	if err != nil {
		t.Fatalf("reuse Checkout after rewrite: %v", err)
	}
	head, err := exec.Command("git", "-C", ws.Dir, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse: %v\n%s", err, head)
	}
	if got := strings.TrimSpace(string(head)); got != sha2 {
		t.Errorf("HEAD after rewritten history = %s, want %s", got, sha2)
	}
}

func TestCheckoutRejectsRefDrift(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	src, _ := initGitRepo(t)

	// The event's SHA cannot be fetched (servers may refuse fetch-by-SHA;
	// here the object simply doesn't exist, which fails identically), so the
	// ref fallback runs and lands on the branch tip — which must not be
	// executed while the run records a different SHA.
	bogus := strings.Repeat("beef1234", 5)
	_, err := Checkout(context.Background(), Options{
		Dir: filepath.Join(t.TempDir(), "ws"), RepoURL: src, SHA: bogus, Ref: "refs/heads/main",
	})
	if err == nil {
		t.Fatal("Checkout should fail when the ref does not point at the requested SHA")
	}
	if !strings.Contains(err.Error(), "not the requested SHA") {
		t.Errorf("error should name the SHA mismatch, got: %v", err)
	}
}

func TestCheckoutReuseRejectsRefDrift(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	src, sha1 := initGitRepo(t)
	dir := filepath.Join(t.TempDir(), "ws")
	if _, err := Checkout(context.Background(), Options{Dir: dir, RepoURL: src, SHA: sha1, Ref: "refs/heads/main", Keep: true, Reuse: true}); err != nil {
		t.Fatalf("first Checkout: %v", err)
	}
	commitFile(t, src, "new.txt", "v2\n")

	// A reused checkout already holding the requested commit runs it even when
	// the ref moved on — no drift when the object is available. Drift needs a
	// SHA that cannot be fetched at all: then the ref fallback lands on the tip
	// and verification must fail (in the reuse path and again after the
	// self-heal rebuild) rather than run the tip as if it were the request.
	bogus := strings.Repeat("beef1234", 5)
	_, err := Checkout(context.Background(), Options{Dir: dir, RepoURL: src, SHA: bogus, Ref: "refs/heads/main", Keep: true, Reuse: true})
	if err == nil {
		t.Fatal("reuse Checkout should fail when the requested SHA is unavailable and the ref points elsewhere")
	}
	if !strings.Contains(err.Error(), "not the requested SHA") {
		t.Errorf("error should name the SHA mismatch, got: %v", err)
	}
}

func TestCheckoutShortSHA(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	src, sha := initGitRepo(t)

	// An abbreviated SHA cannot be fetched directly, so the ref fallback runs;
	// verification matches by prefix, so the run proceeds.
	ws, err := Checkout(context.Background(), Options{
		Dir: filepath.Join(t.TempDir(), "ws"), RepoURL: src, SHA: sha[:8], Ref: "refs/heads/main",
	})
	if err != nil {
		t.Fatalf("Checkout with abbreviated SHA: %v", err)
	}
	defer func() { _ = ws.Cleanup() }()
	out, err := exec.Command("git", "-C", ws.Dir, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != sha {
		t.Errorf("HEAD = %s, want %s", got, sha)
	}
}

func TestCheckoutReuseSelfHeals(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	src, sha := initGitRepo(t)

	for _, tc := range []struct {
		name    string
		corrupt func(t *testing.T, dir string)
	}{
		{"gitdir removed", func(t *testing.T, dir string) {
			if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
				t.Fatal(err)
			}
		}},
		{"gitdir replaced by garbage file", func(t *testing.T, dir string) {
			if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("garbage"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "ws")
			if _, err := Checkout(context.Background(), Options{Dir: dir, RepoURL: src, SHA: sha, Ref: "refs/heads/main", Keep: true, Reuse: true}); err != nil {
				t.Fatalf("first Checkout: %v", err)
			}
			marker := filepath.Join(dir, "marker")
			if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			tc.corrupt(t, dir)

			ws, err := Checkout(context.Background(), Options{Dir: dir, RepoURL: src, SHA: sha, Ref: "refs/heads/main", Keep: true, Reuse: true})
			if err != nil {
				t.Fatalf("Checkout should self-heal: %v", err)
			}
			// The directory was rebuilt from scratch: marker gone, HEAD right.
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Errorf("marker should be gone after a rebuild: %v", err)
			}
			head, err := exec.Command("git", "-C", ws.Dir, "rev-parse", "HEAD").CombinedOutput()
			if err != nil {
				t.Fatalf("rev-parse: %v\n%s", err, head)
			}
			if got := strings.TrimSpace(string(head)); got != sha {
				t.Errorf("HEAD after heal = %s, want %s", got, sha)
			}
		})
	}
}

func TestCheckoutReuseFreshDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	src, sha := initGitRepo(t)

	// Reuse into a directory that does not exist yet behaves like a fresh
	// checkout (the first run of a persistent repo).
	ws, err := Checkout(context.Background(), Options{Dir: filepath.Join(t.TempDir(), "ws"), RepoURL: src, SHA: sha, Ref: "refs/heads/main", Keep: true, Reuse: true})
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, ".janus", "ci.yml")); err != nil {
		t.Errorf("fresh checkout via Reuse should materialize the repo: %v", err)
	}
}

func TestValidateTarget(t *testing.T) {
	valid := []struct{ sha, ref string }{
		{"", "refs/heads/main"},
		{"0123456789abcdef0123456789abcdef01234567", ""},
		{"abc1234", "feature/x-y_z"},
		{"", "refs/tags/v1.0"},
		{"", "release@2026"},
	}
	for _, tc := range valid {
		if err := validateTarget(tc.sha, tc.ref); err != nil {
			t.Errorf("validateTarget(%q, %q) = %v, want nil", tc.sha, tc.ref, err)
		}
	}
	bad := []struct {
		name, sha, ref string
	}{
		{"option-injecting SHA", "--upload-pack=/bin/echo", ""},
		{"option-injecting ref", "", "--upload-pack=/bin/echo"},
		{"leading-dash ref", "", "-main"},
		{"traversal ref", "", "refs/../heads/main"},
		{"space in ref", "", "refs/heads/a b"},
		{"lock-suffix ref", "", "refs/heads/x.lock"},
		{"trailing-slash ref", "", "refs/heads/x/"},
		{"reflog ref", "", "main@{0}"},
		{"too-short SHA", "abc12", ""},
		{"non-hex SHA", "zzzzzzz", ""},
		{"over-long SHA", strings.Repeat("a", 65), ""},
	}
	for _, tc := range bad {
		if err := validateTarget(tc.sha, tc.ref); err == nil {
			t.Errorf("%s: validateTarget(%q, %q) = nil, want error", tc.name, tc.sha, tc.ref)
		}
	}
}

func TestGitEnv(t *testing.T) {
	// last returns the final entry for key; exec.Cmd uses the last duplicate.
	last := func(env []string, key string) (string, bool) {
		v, ok := "", false
		for _, kv := range env {
			if strings.HasPrefix(kv, key+"=") {
				v, ok = kv, true
			}
		}
		return v, ok
	}

	env := gitEnv([]string{"PATH=/usr/bin"}, false, false)
	if v, _ := last(env, "GIT_TERMINAL_PROMPT"); v != "GIT_TERMINAL_PROMPT=0" {
		t.Errorf("default: GIT_TERMINAL_PROMPT entry = %q, want GIT_TERMINAL_PROMPT=0", v)
	}
	if v, _ := last(env, "GIT_SSH_COMMAND"); v != "GIT_SSH_COMMAND=ssh -o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=4" {
		t.Errorf("default: GIT_SSH_COMMAND entry = %q, want batch-mode ssh with bounded connect and keepalive", v)
	}
	// Set-but-empty disables askpass resolution entirely; a no-op *program*
	// would echo git's prompt argument back as the credential.
	if v, _ := last(env, "GIT_ASKPASS"); v != "GIT_ASKPASS=" {
		t.Errorf("default: GIT_ASKPASS entry = %q, want set-but-empty", v)
	}

	env = gitEnv([]string{"GIT_SSH_COMMAND=ssh -i /custom/key"}, false, false)
	if v, _ := last(env, "GIT_SSH_COMMAND"); v != "GIT_SSH_COMMAND=ssh -i /custom/key" {
		t.Errorf("operator GIT_SSH_COMMAND not preserved: last entry %q", v)
	}

	env = gitEnv([]string{"GIT_SSH=/usr/local/bin/myssh"}, false, false)
	if v, ok := last(env, "GIT_SSH_COMMAND"); ok {
		t.Errorf("operator GIT_SSH set, but GIT_SSH_COMMAND appended: %q", v)
	}

	env = gitEnv([]string{"GIT_ASKPASS=/usr/bin/mine"}, false, false)
	if v, _ := last(env, "GIT_ASKPASS"); v != "GIT_ASKPASS=/usr/bin/mine" {
		t.Errorf("operator GIT_ASKPASS not preserved: last entry %q", v)
	}

	// gitconfig-configured transport/askpass (core.sshCommand / core.askpass)
	// suppress the defaults exactly like the env vars do.
	env = gitEnv([]string{"PATH=/usr/bin"}, true, false)
	if v, ok := last(env, "GIT_SSH_COMMAND"); ok {
		t.Errorf("core.sshCommand configured, but GIT_SSH_COMMAND appended: %q", v)
	}
	env = gitEnv([]string{"PATH=/usr/bin"}, false, true)
	if v, ok := last(env, "GIT_ASKPASS"); ok {
		t.Errorf("core.askpass configured, but GIT_ASKPASS appended: %q", v)
	}

	env = gitEnv([]string{"GIT_TERMINAL_PROMPT=1"}, false, false)
	if v, _ := last(env, "GIT_TERMINAL_PROMPT"); v != "GIT_TERMINAL_PROMPT=0" {
		t.Errorf("prompt hardening must win over inherited value: last entry %q", v)
	}

	env = gitEnv([]string{"GIT_SSHX=1", "GIT_SSH_COMMANDX=1"}, false, false)
	if _, ok := last(env, "GIT_SSH_COMMAND"); !ok {
		t.Error("similarly-named vars must not suppress the batch-mode default")
	}
}

func TestEnvHasFold(t *testing.T) {
	base := []string{"Git_Ssh_Command=ssh -i /x", "GIT_SSH_COMMANDX=1", "NOEQUALS"}
	if envHasFold(base, "GIT_SSH_COMMAND", false) {
		t.Error("exact matching must not match a differently-cased key")
	}
	if !envHasFold(base, "GIT_SSH_COMMAND", true) {
		t.Error("folded matching must match a differently-cased key (Windows env semantics)")
	}
	if envHasFold(base, "GIT_SSH", true) {
		t.Error("name must match the whole key, not a prefix")
	}
}

func TestGitConfigSet(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// Isolate from the developer's real configuration.
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)

	ctx := context.Background()
	dir := t.TempDir() // not a repository: only global/system scope applies
	if gitConfigSet(ctx, dir, "core.sshCommand") {
		t.Error("empty config: core.sshCommand should not be reported configured")
	}
	if err := os.WriteFile(cfg, []byte("[core]\n\tsshCommand = ssh -i /custom/key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !gitConfigSet(ctx, dir, "core.sshCommand") {
		t.Error("core.sshCommand set in the global gitconfig should be reported configured")
	}
	if gitConfigSet(ctx, dir, "core.askpass") {
		t.Error("core.askpass absent should not be reported configured")
	}

	// The workspace's own local config counts too — the probe runs in the
	// checkout's context, exactly where the fetch's git would resolve it.
	repo := t.TempDir()
	for _, args := range [][]string{
		{"-C", repo, "init", "-q"},
		{"-C", repo, "config", "core.askpass", "/usr/local/bin/my-askpass"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if !gitConfigSet(ctx, repo, "core.askpass") {
		t.Error("core.askpass in the workspace-local config should be reported configured")
	}
}

func TestCheckoutRejectsOptionInjection(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	src, _ := initGitRepo(t)
	for _, reuse := range []bool{false, true} {
		// The payload must be rejected before any git fetch runs — otherwise
		// --upload-pack executes and defeats the SHA verification.
		_, err := Checkout(context.Background(), Options{
			Dir: filepath.Join(t.TempDir(), "ws"), RepoURL: src,
			Ref: "--upload-pack=/bin/echo", Keep: true, Reuse: reuse,
		})
		if err == nil {
			t.Fatalf("reuse=%v: option-injecting ref was not rejected", reuse)
		}
	}
}

func TestCheckoutRecordsResolvedHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	src, sha := initGitRepo(t)
	// Abbreviated SHA + ref fallback: Head must be the full resolved commit.
	ws, err := Checkout(context.Background(), Options{
		Dir: filepath.Join(t.TempDir(), "ws"), RepoURL: src, SHA: sha[:8], Ref: "refs/heads/main",
	})
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	defer func() { _ = ws.Cleanup() }()
	if ws.Head != sha {
		t.Errorf("ws.Head = %q, want the full %q", ws.Head, sha)
	}
}

func TestCheckoutValidation(t *testing.T) {
	if _, err := Checkout(context.Background(), Options{Dir: t.TempDir(), SHA: "abc"}); err == nil {
		t.Error("expected error when RepoURL is empty")
	}
	if _, err := Checkout(context.Background(), Options{Dir: t.TempDir(), RepoURL: "x"}); err == nil {
		t.Error("expected error when neither SHA nor Ref is provided")
	}
}

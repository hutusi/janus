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

func TestCheckoutValidation(t *testing.T) {
	if _, err := Checkout(context.Background(), Options{Dir: t.TempDir(), SHA: "abc"}); err == nil {
		t.Error("expected error when RepoURL is empty")
	}
	if _, err := Checkout(context.Background(), Options{Dir: t.TempDir(), RepoURL: "x"}); err == nil {
		t.Error("expected error when neither SHA nor Ref is provided")
	}
}

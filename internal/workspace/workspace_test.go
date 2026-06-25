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

func TestCheckoutValidation(t *testing.T) {
	if _, err := Checkout(context.Background(), Options{Dir: t.TempDir(), SHA: "abc"}); err == nil {
		t.Error("expected error when RepoURL is empty")
	}
	if _, err := Checkout(context.Background(), Options{Dir: t.TempDir(), RepoURL: "x"}); err == nil {
		t.Error("expected error when neither SHA nor Ref is provided")
	}
}

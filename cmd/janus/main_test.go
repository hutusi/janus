package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/hutusi/janus/internal/config"
	"github.com/hutusi/janus/internal/store"
)

// buildServeArgs returns the flags every wiring test needs: an explicit config
// path (so ./janus.yml in the working directory can never leak in) plus temp
// directories, with extra flags appended.
func buildServeArgs(t *testing.T, configPath string, extra ...string) []string {
	t.Helper()
	args := []string{
		"--config", configPath,
		"--data-dir", t.TempDir(),
		"--workspace-root", t.TempDir(),
	}
	return append(args, extra...)
}

// writeConfig writes a config file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "janus.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Precedence is documented as defaults < file < env < flags, and it is applied
// at the call site in buildServe, so nothing below the CLI can verify it.
func TestBuildServeConfigPrecedence(t *testing.T) {
	cfgPath := writeConfig(t, "addr: \":9001\"\nhistory_limit: 7\ngitlab_secret: from-file\n")

	t.Run("file overrides defaults", func(t *testing.T) {
		c, err := buildServe(buildServeArgs(t, cfgPath), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if c.cfg.Addr != ":9001" || c.cfg.HistoryLimit != 7 || c.cfg.GitLabSecret != "from-file" {
			t.Errorf("cfg = %+v, want the file's values", c.cfg)
		}
	})

	t.Run("env overrides file", func(t *testing.T) {
		t.Setenv("JANUS_GITLAB_SECRET", "from-env")
		c, err := buildServe(buildServeArgs(t, cfgPath), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if c.cfg.GitLabSecret != "from-env" {
			t.Errorf("gitlab_secret = %q, want from-env", c.cfg.GitLabSecret)
		}
	})

	t.Run("flag overrides env and file", func(t *testing.T) {
		t.Setenv("JANUS_GITLAB_SECRET", "from-env")
		c, err := buildServe(buildServeArgs(t, cfgPath, "--gitlab-secret", "from-flag", "--addr", ":9002"), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if c.cfg.GitLabSecret != "from-flag" || c.cfg.Addr != ":9002" {
			t.Errorf("gitlab_secret = %q, addr = %q; want the flag values", c.cfg.GitLabSecret, c.cfg.Addr)
		}
	})

	t.Run("unset flags do not clobber the file", func(t *testing.T) {
		c, err := buildServe(buildServeArgs(t, cfgPath, "--addr", ":9003"), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if c.cfg.HistoryLimit != 7 {
			t.Errorf("history_limit = %d, want 7 — an unset flag must not apply its default", c.cfg.HistoryLimit)
		}
	})
}

// Which /webhooks/* routes exist is decided entirely by which secrets are
// configured. Asserted through the real handler, since the registry is
// unexported: a configured provider rejects a bad signature (401) while an
// unconfigured one does not exist at all (404).
func TestBuildServeRegistersConfiguredProviders(t *testing.T) {
	all := []string{"gitlab", "github", "gitee", "gitcode"}
	for _, name := range all {
		t.Run(name, func(t *testing.T) {
			cfgPath := writeConfig(t, name+"_secret: s3cret\n")
			c, err := buildServe(buildServeArgs(t, cfgPath), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(c.srv.Handler)
			t.Cleanup(ts.Close)

			for _, p := range all {
				resp, err := http.Post(ts.URL+"/webhooks/"+p, "application/json", strings.NewReader("{}"))
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if p == name {
					if resp.StatusCode == http.StatusNotFound {
						t.Errorf("/webhooks/%s should be enabled by %s_secret, got 404", p, name)
					}
				} else if resp.StatusCode != http.StatusNotFound {
					t.Errorf("/webhooks/%s should be disabled, got %d", p, resp.StatusCode)
				}
			}
		})
	}
}

func TestBuildServeNotifierAndReporter(t *testing.T) {
	t.Run("absent by default", func(t *testing.T) {
		c, err := buildServe(buildServeArgs(t, writeConfig(t, "addr: \":0\"\n")), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if c.notifier != nil {
			t.Error("notifier should be nil with no notifications configured")
		}
		if c.reporter != nil {
			t.Error("reporter should be nil with no API token configured")
		}
	})

	t.Run("built when configured", func(t *testing.T) {
		cfgPath := writeConfig(t, "notifications:\n  - url: \"https://hooks.example.com/x\"\ngitlab_api_token: tok\ngitlab_url: \"https://gitlab.example.com\"\n")
		c, err := buildServe(buildServeArgs(t, cfgPath), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if c.notifier == nil {
			t.Error("notifier should be built when notifications are configured")
		}
		if c.reporter == nil {
			t.Error("reporter should be built when gitlab_api_token is set")
		}
	})

	t.Run("a bad notification url is a named startup error", func(t *testing.T) {
		cfgPath := writeConfig(t, "notifications:\n  - url: \"not-a-url\"\n")
		_, err := buildServe(buildServeArgs(t, cfgPath), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "notifications") {
			t.Errorf("err = %v, want it to name notifications", err)
		}
	})
}

func TestBuildServeStoreSelection(t *testing.T) {
	t.Run("data_dir gives a file store", func(t *testing.T) {
		c, err := buildServe(buildServeArgs(t, writeConfig(t, "addr: \":0\"\n")), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := c.store.(*store.File); !ok {
			t.Errorf("store = %T, want *store.File", c.store)
		}
	})

	t.Run("no data_dir warns and uses memory", func(t *testing.T) {
		var logs bytes.Buffer
		c, err := buildServe([]string{
			"--config", writeConfig(t, "addr: \":0\"\n"),
			"--workspace-root", t.TempDir(),
		}, &logs)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := c.store.(*store.Memory); !ok {
			t.Errorf("store = %T, want *store.Memory", c.store)
		}
		if !strings.Contains(logs.String(), "in-memory") {
			t.Errorf("logs = %q, want an in-memory warning", logs.String())
		}
	})
}

// The allowlist is deny-by-default, so an operator who configures nothing gets
// 403 on every trigger — that has to be said out loud at startup.
func TestBuildServeWarnings(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"empty allowlist", nil, "no repos allowed"},
		{"wildcard allowlist", []string{"--allow-repos", "*"}, "allowing ALL repositories"},
		{"no provider secret", []string{"--allow-repos", "*"}, "no webhook provider secret set"},
		// A '*' allowlist plus a derived API base means the commit-status token
		// follows whatever host a webhook's clone URL names.
		{"wildcard steers gitlab status token",
			[]string{"--allow-repos", "*", "--gitlab-secret", "s", "--gitlab-api-token", "tok"},
			"scope allow_repos or set gitlab_url"},
		{"wildcard steers github status token",
			[]string{"--allow-repos", "*", "--github-secret", "s", "--github-api-token", "tok"},
			"scope allow_repos or set github_url"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			if _, err := buildServe(buildServeArgs(t, writeConfig(t, "addr: \":0\"\n"), tc.args...), &logs); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(logs.String(), tc.want) {
				t.Errorf("logs = %q, want them to mention %q", logs.String(), tc.want)
			}
		})
	}
}

// An unreadable run record must not leave the daemon degraded at startup —
// the end-to-end form of the reconciliation fix.
func TestBuildServeStartupSurvivesUnreadableRecord(t *testing.T) {
	dataDir := t.TempDir()
	runDir := filepath.Join(dataDir, "runs", "broken")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	summary := `{"id":"broken","workflow_name":"ci","status":"running","created_at":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(runDir, "summary.json"), []byte(summary), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := buildServe([]string{
		"--config", writeConfig(t, "addr: \":0\"\n"),
		"--data-dir", dataDir,
		"--workspace-root", t.TempDir(),
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.runner.Degraded() {
		t.Error("a single unreadable run record must not latch the daemon degraded")
	}
}

// An operator's `systemctl stop` must not report failure just because someone
// was tailing logs: http.Server.Shutdown never cancels an in-flight request, so
// one long-lived connection always trips the deadline while runs still drain.
func TestShutdownServerSwallowsDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// A handler that stays open exactly as a ?follow=1 log stream would.
	blocked := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(blocked)
		<-r.Context().Done()
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	go func() { _, _ = http.Get("http://" + ln.Addr().String()) }()
	<-blocked // the connection is established and held

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := shutdownServer(ctx, srv, logger); err != nil {
		t.Errorf("a drained shutdown blocked only by an open stream should exit 0, got %v", err)
	}
}

// Anything that is not an expired deadline is a real failure and must
// propagate — only DeadlineExceeded is forgiven.
func TestShutdownServerPropagatesOtherErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(blocked)
		<-r.Context().Done()
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	go func() { _, _ = http.Get("http://" + ln.Addr().String()) }()
	<-blocked

	// Cancelled, not expired: Shutdown reports context.Canceled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := shutdownServer(ctx, srv, logger); err == nil {
		t.Error("a cancelled (not expired) context should surface as an error")
	}
}

func TestRunInit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "janus.yml")

	if err := runInit([]string{"--config", path}); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	if string(got) != config.ExampleYAML {
		t.Error("written config does not match the embedded example")
	}

	// Refuses to overwrite without --force.
	if err := runInit([]string{"--config", path}); err == nil {
		t.Error("runInit overwrote an existing file without --force")
	}

	// --force overwrites.
	if err := runInit([]string{"--config", path, "--force"}); err != nil {
		t.Errorf("runInit --force: %v", err)
	}
}

func TestRunInitDefaultPath(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runInit(nil); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if _, err := os.Stat(config.DefaultPath); err != nil {
		t.Errorf("expected %s to be written: %v", config.DefaultPath, err)
	}
}

// `janus run --tag` supplies the tag a local run would otherwise have no way
// to name, so a release pipeline can be exercised before it is pushed. The
// step writes the value to a file rather than stdout so the assertion does not
// depend on capturing the engine's terminal tee.
func TestRunRunSuppliesTag(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	pipeline := "name: release\non: { push: { tags: [\"v*\"] } }\njobs:\n  publish:\n    steps:\n      - run: printf '%s' \"${{ tag }}/$JANUS_TAG\" > " + out + "\n"
	if err := os.MkdirAll(filepath.Join(dir, ".janus"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".janus", "ci.yml"), []byte(pipeline), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runRun([]string{"--tag", "v1.2.3", dir}); err != nil {
		t.Fatalf("runRun: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read step output: %v", err)
	}
	if string(got) != "v1.2.3/v1.2.3" {
		t.Errorf("step saw %q, want v1.2.3 through both ${{ tag }} and JANUS_TAG", got)
	}
}

// `--ref refs/tags/v1.0.0` fills the tag, and must not also fill the branch:
// strings.TrimPrefix is a no-op on a miss, so the branch default would
// otherwise be the whole ref.
func TestRunRunDerivesTagFromRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	src := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", src}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	out := filepath.Join(t.TempDir(), "out.txt")
	pipeline := "name: release\non: { push: { tags: [\"v*\"] } }\njobs:\n  publish:\n    steps:\n      - run: printf '%s' \"${{ tag }}|${{ branch }}\" > " + out + "\n"
	if err := os.MkdirAll(filepath.Join(src, ".janus"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".janus", "ci.yml"), []byte(pipeline), 0o644); err != nil {
		t.Fatal(err)
	}
	git("init", "-q", "-b", "main", ".")
	git("config", "user.email", "t@e.com")
	git("config", "user.name", "T")
	git("add", ".")
	git("commit", "-q", "-m", "init")
	git("tag", "-a", "v1.0.0", "-m", "release") // annotated: object id != commit id

	if err := runRun([]string{"--repo", src, "--ref", "refs/tags/v1.0.0", "--workspace-root", t.TempDir()}); err != nil {
		t.Fatalf("runRun: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read step output: %v", err)
	}
	if string(got) != "v1.0.0|" {
		t.Errorf("step saw %q, want the tag set and the branch empty", got)
	}
}

// No real event carries a branch and a tag at once, so `janus run` refuses to
// fabricate one: job-level `branches:` filters are applied locally, and a run
// claiming to be both would exercise branch-gated jobs for a tag.
func TestRunRunRejectsBranchAndTag(t *testing.T) {
	dir := t.TempDir()
	if err := runRun([]string{"--branch", "main", "--tag", "v1.0.0", dir}); err == nil {
		t.Error("--branch with --tag was accepted, want an error")
	}
	// The same contradiction reached through a tag-naming --ref.
	err := runRun([]string{"--repo", dir, "--ref", "refs/tags/v1.0.0", "--branch", "main"})
	if err == nil {
		t.Fatal("--ref refs/tags/... with --branch was accepted, want an error")
	}
	if !strings.Contains(err.Error(), "not on a branch") {
		t.Errorf("error = %v, want it to explain the branch/tag conflict", err)
	}
}

func TestVersionString(t *testing.T) {
	origVersion, origCommit := version, commit
	t.Cleanup(func() { version, commit = origVersion, origCommit })

	version, commit = "v0.2.0", ""
	if got := versionString(); got != "v0.2.0" {
		t.Errorf("versionString without commit = %q, want v0.2.0", got)
	}
	version, commit = "v0.2.0", "f97e513"
	if got := versionString(); got != "v0.2.0 (f97e513)" {
		t.Errorf("versionString with commit = %q, want v0.2.0 (f97e513)", got)
	}
	version, commit = "dev", "f97e513-dirty"
	if got := versionString(); got != "dev (f97e513-dirty)" {
		t.Errorf("versionString tagless = %q, want dev (f97e513-dirty)", got)
	}
}

func TestFromBuildInfo(t *testing.T) {
	bi := func(version, rev, modified string) *debug.BuildInfo {
		b := &debug.BuildInfo{}
		b.Main.Version = version
		if rev != "" {
			b.Settings = append(b.Settings, debug.BuildSetting{Key: "vcs.revision", Value: rev})
		}
		if modified != "" {
			b.Settings = append(b.Settings, debug.BuildSetting{Key: "vcs.modified", Value: modified})
		}
		return b
	}
	cases := []struct {
		name         string
		in           *debug.BuildInfo
		wantV, wantC string
	}{
		{"exact tag", bi("v0.2.0", "6c93b86ab1c2d3e4", "false"), "v0.2.0", "6c93b86"},
		{"dirty", bi("v0.2.0+dirty", "6c93b86ab1c2d3e4", "true"), "v0.2.0", "6c93b86-dirty"},
		{"pseudo-version", bi("v0.2.1-0.20260723140000-6c93b86ab1c2", "6c93b86ab1c2d3e4", "false"), "v0.2.1-0.20260723140000-6c93b86ab1c2", "6c93b86"},
		{"devel no vcs", bi("(devel)", "", ""), "", ""},
		{"empty", bi("", "", ""), "", ""},
	}
	for _, c := range cases {
		v, rev := fromBuildInfo(c.in)
		if v != c.wantV || rev != c.wantC {
			t.Errorf("%s: fromBuildInfo = (%q, %q), want (%q, %q)", c.name, v, rev, c.wantV, c.wantC)
		}
	}
}

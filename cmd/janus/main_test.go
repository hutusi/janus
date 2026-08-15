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
		name       string
		args       []string
		cfg        string // extra config-file YAML appended to the base
		want       string
		wantAbsent string
	}{
		{name: "empty allowlist", want: "no repos allowed"},
		{name: "wildcard allowlist", args: []string{"--allow-repos", "*"}, want: "allowing ALL repositories"},
		{name: "no provider secret", args: []string{"--allow-repos", "*"}, want: "no webhook provider secret set"},
		// A '*' allowlist plus a derived API base means the commit-status token
		// follows whatever host a webhook's clone URL names.
		{name: "wildcard steers gitlab status token",
			args: []string{"--allow-repos", "*", "--gitlab-secret", "s", "--gitlab-api-token", "tok"},
			want: "scope allow_repos or set gitlab_url"},
		{name: "wildcard steers github status token",
			args: []string{"--allow-repos", "*", "--github-secret", "s", "--github-api-token", "tok"},
			want: "scope allow_repos or set github_url"},
		// Without the inbound secret the provider accepts no webhooks, so no
		// status can be posted and there is nothing to steer — the steering
		// warning would only contradict the "no statuses" one.
		{name: "no gitlab webhooks means no steering warning",
			args:       []string{"--allow-repos", "*", "--gitlab-api-token", "tok"},
			want:       "no statuses will be reported",
			wantAbsent: "scope allow_repos or set gitlab_url"},
		{name: "no github webhooks means no steering warning",
			args:       []string{"--allow-repos", "*", "--github-api-token", "tok"},
			want:       "no statuses will be reported",
			wantAbsent: "scope allow_repos or set github_url"},
		// The three safe legs of the steering condition, each of which must
		// keep the warning silent: a scoped allowlist (the derived host is one
		// the operator chose), an explicit instance URL (nothing is derived),
		// and ssh mode (statuses are already skipped, with their own warning).
		{name: "scoped allowlist means no steering warning",
			args:       []string{"--allow-repos", "git@gitlab.example.com:acme", "--gitlab-secret", "s", "--gitlab-api-token", "tok"},
			want:       "gitlab commit-status reporting enabled",
			wantAbsent: "scope allow_repos or set gitlab_url"},
		{name: "explicit gitlab_url means no steering warning",
			args:       []string{"--allow-repos", "*", "--gitlab-secret", "s", "--gitlab-api-token", "tok"},
			cfg:        "gitlab_url: \"https://gitlab.example.com\"\n",
			want:       "gitlab commit-status reporting enabled",
			wantAbsent: "scope allow_repos or set gitlab_url"},
		{name: "explicit github_url means no steering warning",
			args:       []string{"--allow-repos", "*", "--github-secret", "s", "--github-api-token", "tok"},
			cfg:        "github_url: \"https://github.example.com\"\n",
			want:       "github commit-status reporting enabled",
			wantAbsent: "scope allow_repos or set github_url"},
		{name: "ssh clone mode means no steering warning",
			args:       []string{"--allow-repos", "*", "--clone-url", "ssh", "--github-secret", "s", "--github-api-token", "tok"},
			want:       "needs github_url set",
			wantAbsent: "scope allow_repos or set github_url"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			if _, err := buildServe(buildServeArgs(t, writeConfig(t, "addr: \":0\"\n"+tc.cfg), tc.args...), &logs); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(logs.String(), tc.want) {
				t.Errorf("logs = %q, want them to mention %q", logs.String(), tc.want)
			}
			if tc.wantAbsent != "" && strings.Contains(logs.String(), tc.wantAbsent) {
				t.Errorf("logs = %q, want no mention of %q", logs.String(), tc.wantAbsent)
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

	// A plain build with no ldflags falls back to the toolchain's embedded
	// build info. What that holds depends on how the test binary was built
	// (a `go test` binary usually has no VCS stamp), so only the shape is
	// asserted: never empty, never a panic.
	version, commit = "dev", ""
	if got := versionString(); got == "" {
		t.Error(`versionString fell back to ""`)
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

// runValidate is the round-trip a pipeline author lives in: a valid file says
// ok, a rejected one names the file, and the argument shape is enforced.
func TestRunValidate(t *testing.T) {
	valid := "name: ci\non: { push: { tags: [\"v*\"] } }\njobs:\n  build:\n    steps:\n      - run: echo ok\n"
	// `if:` is deliberately not a feature; validation must refuse it.
	rejected := "name: ci\non: { push: { tags: [\"v*\"] } }\njobs:\n  build:\n    steps:\n      - run: echo ok\n        if: always()\n"

	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "ci.yml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("valid file", func(t *testing.T) {
		if err := runValidate([]string{write(t, valid)}); err != nil {
			t.Errorf("runValidate = %v, want nil", err)
		}
	})
	t.Run("rejected pipeline names the file", func(t *testing.T) {
		path := write(t, rejected)
		err := runValidate([]string{path})
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("err = %v, want it to name %s", err, path)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if err := runValidate([]string{filepath.Join(t.TempDir(), "absent.yml")}); err == nil {
			t.Error("a missing file validated, want an error")
		}
	})
	t.Run("no argument is a usage error", func(t *testing.T) {
		err := runValidate(nil)
		if err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Errorf("err = %v, want a usage error", err)
		}
	})
	t.Run("two arguments is a usage error", func(t *testing.T) {
		if err := runValidate([]string{"a.yml", "b.yml"}); err == nil {
			t.Error("two files validated, want a usage error")
		}
	})
	t.Run("unknown flag", func(t *testing.T) {
		if err := runValidate([]string{"--no-such-flag"}); err == nil {
			t.Error("an unknown flag was accepted, want an error")
		}
	})
}

// serve's listener-error leg, driven through runServe: when the address is
// already taken, ListenAndServe fails and the error propagates out (after the
// deferred drain) instead of hanging the process.
func TestRunServeReturnsListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	err = runServe(buildServeArgs(t, writeConfig(t, ""), "--addr", ln.Addr().String()))
	if err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("err = %v, want the bind failure", err)
	}
}

// runServe surfaces a buildServe failure as its own error rather than serving.
func TestRunServeReportsBuildError(t *testing.T) {
	err := runServe([]string{"--config", filepath.Join(t.TempDir(), "absent.yml")})
	if err == nil || !strings.Contains(err.Error(), "read config") {
		t.Errorf("err = %v, want the config read failure", err)
	}
}

// Every buildServe failure must be a named startup error, not a panic or a
// half-built server.
func TestBuildServeErrors(t *testing.T) {
	tests := []struct {
		name string
		args func(t *testing.T) []string
		want string // error substring
	}{
		{name: "missing config file",
			args: func(t *testing.T) []string {
				return []string{"--config", filepath.Join(t.TempDir(), "absent.yml")}
			},
			want: "read config"},
		{name: "unknown config key",
			args: func(t *testing.T) []string {
				return buildServeArgs(t, writeConfig(t, "no_such_key: 1\n"))
			},
			want: "no_such_key"},
		{name: "invalid clone_url",
			args: func(t *testing.T) []string {
				return buildServeArgs(t, writeConfig(t, ""), "--clone-url", "bogus")
			},
			want: "clone_url"},
		{name: "invalid allow_repos entry",
			args: func(t *testing.T) []string {
				return buildServeArgs(t, writeConfig(t, ""), "--allow-repos", "gitlab.com/acme")
			},
			want: "allow_repos"},
		{name: "data_dir is a regular file",
			args: func(t *testing.T) []string {
				file := filepath.Join(t.TempDir(), "not-a-dir")
				if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				return []string{
					"--config", writeConfig(t, ""),
					"--data-dir", file,
					"--workspace-root", t.TempDir(),
				}
			},
			want: "data-dir"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildServe(tc.args(t), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The non-hermetic (persistent) and cache-backed (mirror) strategies announce
// themselves at startup — an operator reading the log must be able to tell
// which reuse semantics their builds run under.
func TestBuildServeStrategyNotes(t *testing.T) {
	for strategy, want := range map[string]string{
		"persistent": "persistent workspaces enabled",
		"mirror":     "mirror workspaces enabled",
	} {
		t.Run(strategy, func(t *testing.T) {
			var logs bytes.Buffer
			if _, err := buildServe(buildServeArgs(t, writeConfig(t, ""), "--workspace-strategy", strategy), &logs); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(logs.String(), want) {
				t.Errorf("logs = %q, want them to mention %q", logs.String(), want)
			}
		})
	}
}

func TestRunInitErrors(t *testing.T) {
	t.Run("unknown flag", func(t *testing.T) {
		if err := runInit([]string{"--no-such-flag"}); err == nil {
			t.Error("an unknown flag was accepted, want an error")
		}
	})
	t.Run("unwritable path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "no-such-dir", "janus.yml")
		if err := runInit([]string{"--config", path}); err == nil {
			t.Error("writing under a nonexistent directory succeeded, want an error")
		}
	})
}

func TestRunRunArgumentErrors(t *testing.T) {
	t.Run("dir with --repo", func(t *testing.T) {
		err := runRun([]string{"--repo", "https://example.com/r.git", t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "not both") {
			t.Errorf("err = %v, want the either/or usage error", err)
		}
	})
	t.Run("no dir and no --repo", func(t *testing.T) {
		err := runRun(nil)
		if err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Errorf("err = %v, want a usage error", err)
		}
	})
	t.Run("missing pipeline file", func(t *testing.T) {
		if err := runRun([]string{t.TempDir()}); err == nil {
			t.Error("a dir without a pipeline ran, want an error")
		}
	})
	t.Run("rejected pipeline names the file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".janus"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".janus", "ci.yml"), []byte("jobs: []\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := runRun([]string{dir})
		if err == nil || !strings.Contains(err.Error(), ".janus/ci.yml") {
			t.Errorf("err = %v, want it to name the pipeline file", err)
		}
	})
}

// A checkout that fails must surface git's error, not a run summary.
func TestRunRunCheckoutFailure(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	err := runRun([]string{
		"--repo", filepath.Join(t.TempDir(), "absent-repo"),
		"--workspace-root", t.TempDir(),
	})
	if err == nil {
		t.Error("checking out a nonexistent repo succeeded, want an error")
	}
}

// A pipeline whose step fails must make `janus run` itself fail, carrying the
// run's terminal status — that exit code is what a script wraps.
func TestRunRunFailedRun(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()
	pipeline := "name: ci\non: { push: { tags: [\"v*\"] } }\njobs:\n  build:\n    steps:\n      - run: exit 7\n"
	if err := os.MkdirAll(filepath.Join(dir, ".janus"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".janus", "ci.yml"), []byte(pipeline), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runRun([]string{dir})
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Errorf("err = %v, want the run's failed status", err)
	}
}

// dispatch is the CLI contract: which exit code each path returns and which
// stream it speaks on. Exercised directly — main itself can only os.Exit.
func TestDispatch(t *testing.T) {
	// wantStdout/wantStderr are matched against the *injected* writers, which
	// carry only dispatch's own output — usage, the unknown-command and
	// version lines, and the final "janus: <err>" line. Subcommands print
	// their results to the process streams, which these buffers deliberately
	// do not see; an empty want asserts dispatch itself stayed silent, not
	// that the command produced no output.
	tests := []struct {
		name       string
		argv       []string
		wantCode   int
		wantStdout string // substring; empty means dispatch wrote nothing here
		wantStderr string // substring; empty means dispatch wrote nothing here
	}{
		{name: "no command is usage on stderr",
			argv: []string{"janus"}, wantCode: 2, wantStderr: "Usage:"},
		{name: "unknown command is named on stderr",
			argv: []string{"janus", "frobnicate"}, wantCode: 2, wantStderr: `unknown command "frobnicate"`},
		{name: "help is usage on stdout",
			argv: []string{"janus", "help"}, wantCode: 0, wantStdout: "Usage:"},
		{name: "-h is usage on stdout",
			argv: []string{"janus", "-h"}, wantCode: 0, wantStdout: "Usage:"},
		{name: "version prints one line on stdout",
			argv: []string{"janus", "version"}, wantCode: 0, wantStdout: "janus "},
		{name: "a failing subcommand exits 1 with the error on stderr",
			argv:     []string{"janus", "validate", filepath.Join(t.TempDir(), "absent.yml")},
			wantCode: 1, wantStderr: "janus:"},
		// One routing check per remaining subcommand — each command's own
		// behavior has its focused tests; here only the arm and exit code
		// (init's "wrote …" success line lands on the real os.Stdout, which
		// is exactly why its row expects the injected buffers empty).
		{name: "init routes",
			argv:     []string{"janus", "init", "--config", filepath.Join(t.TempDir(), "janus.yml")},
			wantCode: 0},
		{name: "run routes",
			argv: []string{"janus", "run"}, wantCode: 1, wantStderr: "usage:"},
		{name: "serve routes",
			argv:     []string{"janus", "serve", "--config", filepath.Join(t.TempDir(), "absent.yml")},
			wantCode: 1, wantStderr: "read config"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := dispatch(tc.argv, &stdout, &stderr); code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", code, tc.wantCode)
			}
			if !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tc.wantStdout)
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.wantStderr)
			}
			if tc.wantStdout == "" && stdout.Len() != 0 {
				t.Errorf("stdout = %q, want nothing on it", stdout.String())
			}
			if tc.wantStderr == "" && stderr.Len() != 0 {
				t.Errorf("stderr = %q, want nothing on it", stderr.String())
			}
		})
	}
}

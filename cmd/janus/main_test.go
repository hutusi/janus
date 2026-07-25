package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
	"time"

	"github.com/hutusi/janus/internal/config"
)

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

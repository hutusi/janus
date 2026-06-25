package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hutusi/janus/internal/config"
)

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

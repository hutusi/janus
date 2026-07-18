package store

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hutusi/janus/internal/model"
)

// runIDRe constrains run IDs to a safe charset so a caller-supplied ID (e.g.
// from a URL path) can never traverse outside the data directory.
var runIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func checkRunID(id string) error {
	if !runIDRe.MatchString(id) {
		return fmt.Errorf("invalid run id %q", id)
	}
	return nil
}

// File is a flat-file Store. Each run lives in its own directory:
//
//	<root>/runs/<id>/run.json          (metadata, rewritten atomically on change)
//	<root>/runs/<id>/logs/<job>-<n>.log (append-only combined output per step)
//
// It survives restarts and is trivially inspectable with cat/grep. Run metadata
// is written via a temp file + rename so a reader never sees a partial file.
type File struct {
	root string
}

// NewFile creates a file-backed store rooted at dir, creating it if needed.
func NewFile(dir string) (*File, error) {
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0o700); err != nil {
		return nil, err
	}
	return &File{root: dir}, nil
}

func (f *File) runDir(id string) string { return filepath.Join(f.root, "runs", id) }

func (f *File) SaveRun(run *model.Run) error   { return f.writeRun(run) }
func (f *File) UpdateRun(run *model.Run) error { return f.writeRun(run) }

func (f *File) writeRun(run *model.Run) error {
	if err := checkRunID(run.ID); err != nil {
		return err
	}
	dir := f.runDir(run.ID)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".run-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, "run.json"))
}

func (f *File) GetRun(id string) (*model.Run, error) {
	if err := checkRunID(id); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(f.runDir(id), "run.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("run %q not found", id)
		}
		return nil, err
	}
	var run model.Run
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, fmt.Errorf("run %q: %w", id, err)
	}
	return &run, nil
}

func (f *File) ListRuns(limit int) ([]*model.Run, error) {
	entries, err := os.ReadDir(filepath.Join(f.root, "runs"))
	if err != nil {
		return nil, err
	}
	runs := make([]*model.Run, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		run, err := f.GetRun(e.Name())
		if err != nil {
			continue // skip incomplete/unreadable run dirs
		}
		runs = append(runs, run)
	}
	sort.SliceStable(runs, func(i, j int) bool {
		return runs[i].CreatedAt.After(runs[j].CreatedAt)
	})
	if limit > 0 && len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

func (f *File) logPath(runID, job string, stepIndex int) string {
	name := fmt.Sprintf("%s-%d.log", sanitize(job), stepIndex)
	return filepath.Join(f.runDir(runID), "logs", name)
}

func (f *File) LogWriter(runID, job string, stepIndex int) (io.WriteCloser, error) {
	if err := checkRunID(runID); err != nil {
		return nil, err
	}
	path := f.logPath(runID, job, stepIndex)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

func (f *File) ReadLogs(runID, job string, stepIndex int, offset int64) (io.ReadCloser, error) {
	if err := checkRunID(runID); err != nil {
		return nil, err
	}
	file, err := os.Open(f.logPath(runID, job, stepIndex))
	if err != nil {
		if os.IsNotExist(err) {
			return io.NopCloser(strings.NewReader("")), nil
		}
		return nil, err
	}
	if offset > 0 {
		// Seeking past EOF is fine — reads just return EOF.
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return file, nil
}

// sanitize maps a job name to a safe filename fragment.
func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, name)
}

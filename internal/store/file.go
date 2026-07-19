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

// maxRunFileBytes bounds a run.json read. Records are bounded by the ingestion
// caps (pipeline file, event fields), so 16 MiB is generous headroom; the cap
// only guards against a corrupt or legacy oversized file being loaded whole.
const maxRunFileBytes = 16 << 20

func (f *File) GetRun(id string) (*model.Run, error) {
	if err := checkRunID(id); err != nil {
		return nil, err
	}
	file, err := os.Open(filepath.Join(f.runDir(id), "run.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("run %q not found", id)
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxRunFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxRunFileBytes {
		return nil, fmt.Errorf("run %q metadata is too large (limit %d bytes)", id, maxRunFileBytes)
	}
	var run model.Run
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, fmt.Errorf("run %q: %w", id, err)
	}
	return &run, nil
}

func (f *File) ListRuns(limit, offset int) ([]*model.Run, error) {
	runs, err := f.allRuns()
	if err != nil {
		return nil, err
	}
	return page(runs, limit, offset), nil
}

// allRuns loads every run, newest-first. The flat-file store has no
// time-sorted index, so this reads and decodes each run.json — bounded in
// practice by history_limit retention (see Prune).
func (f *File) allRuns() ([]*model.Run, error) {
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
	return runs, nil
}

func (f *File) Prune(keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	runs, err := f.allRuns()
	if err != nil {
		return 0, err
	}
	// Keep the newest `keep` terminal runs; delete older terminal ones.
	// Non-terminal runs are left untouched and don't count toward the budget.
	kept := 0
	var removed int
	var firstErr error
	for _, run := range runs { // newest-first
		if !run.Status.Terminal() {
			continue
		}
		if kept < keep {
			kept++
			continue
		}
		if err := os.RemoveAll(f.runDir(run.ID)); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	return removed, firstErr
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

func (f *File) ReadLogsTail(runID, job string, stepIndex int, maxBytes int64) (io.ReadCloser, bool, error) {
	if maxBytes <= 0 {
		rc, err := f.ReadLogs(runID, job, stepIndex, 0)
		return rc, false, err
	}
	if err := checkRunID(runID); err != nil {
		return nil, false, err
	}
	file, err := os.Open(f.logPath(runID, job, stepIndex))
	if err != nil {
		if os.IsNotExist(err) {
			return io.NopCloser(strings.NewReader("")), false, nil
		}
		return nil, false, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, false, err
	}
	off, length := int64(0), info.Size()
	truncated := info.Size() > maxBytes
	if truncated {
		off, length = info.Size()-maxBytes, maxBytes
	}
	// SectionReader is bound to the window observed at Stat, so a concurrent
	// append can neither push the read past maxBytes nor turn a whole-file read
	// into a head-of-a-grown-file read — the tail and truncated flag stay
	// coherent as of Stat.
	return readCloser{Reader: io.NewSectionReader(file, off, length), Closer: file}, truncated, nil
}

// readCloser pairs a bounded Reader with the underlying file's Closer.
type readCloser struct {
	io.Reader
	io.Closer
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

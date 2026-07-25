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
	// Enforce the same cap GetRun reads with, so we never persist a record that
	// can then never be read back (which would drop it from list/detail/
	// reconciliation). With the ingestion caps in place this never trips.
	if int64(len(data)) > maxRunFileBytes {
		return fmt.Errorf("run %q metadata is too large to persist (limit %d bytes)", run.ID, maxRunFileBytes)
	}
	// run.json is the source of truth — write it first, then the summary
	// sidecar. That ordering keeps a stale sidecar's lifecycle status <= the
	// record's (pending->running->terminal is monotonic, terminal is final), so
	// a lagging sidecar can never claim "terminal" for a still-running run.
	if err := writeAtomic(dir, "run.json", data, run.Status.Terminal()); err != nil {
		return err
	}
	// The sidecar is a listing cache; a write hiccup must not fail a valid
	// persist — readSummary falls back to run.json (and re-heals) if it is
	// missing.
	_ = writeSummary(dir, run.Summary())
	return nil
}

// writeSummary writes the compact summary sidecar used by ListRuns/Prune. It
// is never synced: it is a listing cache, and readSummary rebuilds it from
// run.json (the source of truth) whenever it is missing or unreadable.
func writeSummary(dir string, s *model.RunSummary) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeAtomic(dir, "summary.json", data, false)
}

// writeAtomic writes data to dir/name via a temp file + rename. The rename
// gives readers atomicity: they see the old file or the new one, never a
// half-written mix.
//
// With sync, the write is additionally made durable — the data is flushed
// before the rename and the parent directory afterwards, so the record
// survives a power loss and not merely a process crash. Durability is not free
// (each flush waits on the disk), and writeRun is called on every step and job
// transition, so only terminal writes ask for it: those are the ones startup
// reconciliation reads back, and a lost intermediate "running" state is
// repaired by that same reconciliation.
func writeAtomic(dir, name string, data []byte, sync bool) error {
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if sync {
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		// Every other failure path clears the temp file; this one used to leak
		// it, leaving .run.json-* debris behind on a full or read-only mount.
		_ = os.Remove(tmpName)
		return err
	}
	if sync {
		syncDir(dir)
	}
	return nil
}

// syncDir flushes a directory entry so a completed rename survives a power
// loss. Best-effort: opening or syncing a directory is not portable (Windows
// refuses it), and the file's own contents are already durable by here, so a
// failure must not fail an otherwise-complete write.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// maxRunFileBytes bounds both a run.json write and read symmetrically. Records
// are bounded by the ingestion caps (pipeline file, event fields), so 16 MiB is
// generous headroom; the cap guards against a corrupt/oversized file being
// written or read whole. A var so tests can shrink it.
var maxRunFileBytes int64 = 16 << 20

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

func (f *File) ListRuns(limit, offset int) ([]*model.RunSummary, error) {
	sums, err := f.allSummaries()
	if err != nil {
		return nil, err
	}
	return page(sums, limit, offset), nil
}

// CountRuns counts via allSummaries so the total always matches what ListRuns
// actually lists (unreadable run dirs are excluded from both).
func (f *File) CountRuns() (int, error) {
	sums, err := f.allSummaries()
	if err != nil {
		return 0, err
	}
	return len(sums), nil
}

// maxSummaryFileBytes bounds a summary.json read. A summary is a handful of
// capped fields, so this is small; it only guards against a corrupt sidecar.
const maxSummaryFileBytes = 64 << 10

// readSummary returns a run's compact summary. It reads the tiny summary.json
// sidecar so listing never parses the full run.json. If the sidecar is missing
// or unreadable (a legacy run, or a failed sidecar write), it falls back to
// deriving the summary from run.json and rewrites the sidecar (self-healing,
// backward-compatible).
func (f *File) readSummary(id string) (*model.RunSummary, error) {
	if data, err := os.ReadFile(filepath.Join(f.runDir(id), "summary.json")); err == nil && int64(len(data)) <= maxSummaryFileBytes {
		var s model.RunSummary
		if err := json.Unmarshal(data, &s); err == nil {
			return &s, nil
		}
	}
	// Fallback: derive from the source of truth and heal the sidecar.
	run, err := f.GetRun(id)
	if err != nil {
		return nil, err
	}
	s := run.Summary()
	_ = writeSummary(f.runDir(id), s)
	return s, nil
}

// allSummaries scans every run directory and returns compact summaries
// newest-first, reading only the tiny summary.json sidecar per run (see
// readSummary) — not the full run.json — so listing stays cheap in both memory
// and disk/CPU regardless of record size. Retention (history_limit) bounds the
// directory scan itself.
func (f *File) allSummaries() ([]*model.RunSummary, error) {
	entries, err := os.ReadDir(filepath.Join(f.root, "runs"))
	if err != nil {
		return nil, err
	}
	sums := make([]*model.RunSummary, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := f.readSummary(e.Name())
		if err != nil {
			continue // skip incomplete/unreadable run dirs
		}
		sums = append(sums, s)
	}
	sort.SliceStable(sums, func(i, j int) bool {
		return sums[i].CreatedAt.After(sums[j].CreatedAt)
	})
	return sums, nil
}

func (f *File) Prune(keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	sums, err := f.allSummaries()
	if err != nil {
		return 0, err
	}
	// Keep the newest `keep` terminal runs; delete older terminal ones.
	// Non-terminal runs are left untouched and don't count toward the budget.
	kept := 0
	var removed int
	var firstErr error
	for _, s := range sums { // newest-first
		if !s.Status.Terminal() {
			continue
		}
		if kept < keep {
			kept++
			continue
		}
		if err := os.RemoveAll(f.runDir(s.ID)); err != nil {
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

package store

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hutusi/janus/internal/model"
)

func sampleRun(id string, created time.Time) *model.Run {
	return &model.Run{
		ID:           id,
		WorkflowName: "ci",
		Status:       model.StatusSuccess,
		CreatedAt:    created,
		Jobs:         []*model.JobRun{{Name: "build", Status: model.StatusSuccess, Steps: []*model.StepRun{{Index: 0, Command: "echo hi", Status: model.StatusSuccess}}}},
	}
}

func TestFileListRunsPagination(t *testing.T) {
	st, _ := NewFile(t.TempDir())
	base := time.Now()
	// r0 newest … r4 oldest.
	for i := 0; i < 5; i++ {
		_ = st.SaveRun(sampleRun("r"+string(rune('0'+i)), base.Add(time.Duration(-i)*time.Minute)))
	}
	ids := func(limit, offset int) []string {
		t.Helper()
		runs, err := st.ListRuns(limit, offset)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, len(runs))
		for i, r := range runs {
			got[i] = r.ID
		}
		return got
	}
	if got := ids(2, 0); len(got) != 2 || got[0] != "r0" || got[1] != "r1" {
		t.Errorf("page 1 = %v, want [r0 r1]", got)
	}
	if got := ids(2, 2); len(got) != 2 || got[0] != "r2" || got[1] != "r3" {
		t.Errorf("page 2 = %v, want [r2 r3]", got)
	}
	if got := ids(10, 4); len(got) != 1 || got[0] != "r4" {
		t.Errorf("last page = %v, want [r4]", got)
	}
	if got := ids(10, 99); len(got) != 0 {
		t.Errorf("offset past end = %v, want empty", got)
	}
}

func TestFilePruneKeepsNewestTerminal(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewFile(dir)
	base := time.Now()
	for i := 0; i < 4; i++ { // r0 newest … r3 oldest, all terminal
		_ = st.SaveRun(sampleRun("r"+string(rune('0'+i)), base.Add(time.Duration(-i)*time.Minute)))
	}
	// A running run older than the cap must survive — never prune non-terminal.
	running := sampleRun("live", base.Add(-10*time.Minute))
	running.Status = model.StatusRunning
	_ = st.SaveRun(running)
	// Give one victim a log file so we can confirm the whole dir goes.
	w, _ := st.LogWriter("r3", "build", 0)
	_, _ = io.WriteString(w, "old output")
	_ = w.Close()

	removed, err := st.Prune(2)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 2 { // r2 and r3 are the terminal runs beyond the newest 2
		t.Fatalf("removed = %d, want 2", removed)
	}
	for _, id := range []string{"r0", "r1", "live"} {
		if _, err := st.GetRun(id); err != nil {
			t.Errorf("run %s should survive: %v", id, err)
		}
	}
	for _, id := range []string{"r2", "r3"} {
		if _, err := st.GetRun(id); err == nil {
			t.Errorf("run %s should have been pruned", id)
		}
	}
	// keep=0 is a no-op.
	if n, _ := st.Prune(0); n != 0 {
		t.Errorf("Prune(0) removed %d, want 0", n)
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	run := sampleRun("abc123", time.Now())
	if err := st.SaveRun(run); err != nil {
		t.Fatal(err)
	}

	w, err := st.LogWriter(run.ID, "build", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, "log line\n")
	_ = w.Close()

	got, err := st.GetRun("abc123")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.WorkflowName != "ci" || len(got.Jobs) != 1 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestFileStoreSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewFile(dir)
	_ = st.SaveRun(sampleRun("r1", time.Now().Add(-time.Minute)))
	_ = st.SaveRun(sampleRun("r2", time.Now()))
	w, _ := st.LogWriter("r2", "build", 0)
	_, _ = io.WriteString(w, "persisted output\n")
	_ = w.Close()

	// A fresh store over the same directory sees everything.
	reopened, err := NewFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := reopened.ListRuns(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("ListRuns = %d runs, want 2", len(runs))
	}
	if runs[0].ID != "r2" { // newest first
		t.Errorf("ListRuns order = %s first, want r2", runs[0].ID)
	}

	rc, _ := reopened.ReadLogs("r2", "build", 0, 0)
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	if string(b) != "persisted output\n" {
		t.Errorf("logs after restart = %q, want persisted output", b)
	}
}

func TestFileReadLogsOffset(t *testing.T) {
	st, _ := NewFile(t.TempDir())
	_ = st.SaveRun(sampleRun("r1", time.Now()))
	w, _ := st.LogWriter("r1", "build", 0)
	_, _ = io.WriteString(w, "hello world")
	_ = w.Close()

	readAt := func(off int64) string {
		t.Helper()
		rc, err := st.ReadLogs("r1", "build", 0, off)
		if err != nil {
			t.Fatalf("ReadLogs(offset=%d): %v", off, err)
		}
		defer func() { _ = rc.Close() }()
		b, _ := io.ReadAll(rc)
		return string(b)
	}
	if got := readAt(6); got != "world" {
		t.Errorf("offset 6 = %q, want world", got)
	}
	if got := readAt(1000); got != "" {
		t.Errorf("offset past end = %q, want empty", got)
	}
}

func TestFileReadLogsTail(t *testing.T) {
	st, _ := NewFile(t.TempDir())
	_ = st.SaveRun(sampleRun("r1", time.Now()))
	w, _ := st.LogWriter("r1", "build", 0)
	_, _ = io.WriteString(w, "hello world")
	_ = w.Close()

	tailOf := func(max int64) (string, bool) {
		t.Helper()
		rc, truncated, err := st.ReadLogsTail("r1", "build", 0, max)
		if err != nil {
			t.Fatalf("ReadLogsTail(%d): %v", max, err)
		}
		defer func() { _ = rc.Close() }()
		b, _ := io.ReadAll(rc)
		return string(b), truncated
	}
	if got, tr := tailOf(5); got != "world" || !tr {
		t.Errorf("tail(5) = %q,%v, want world,true", got, tr)
	}
	if got, tr := tailOf(11); got != "hello world" || tr { // exactly the length: not truncated
		t.Errorf("tail(11) = %q,%v, want the whole log,false", got, tr)
	}
	if got, tr := tailOf(0); got != "hello world" || tr {
		t.Errorf("tail(0) = %q,%v, want the whole log,false", got, tr)
	}
	if got, tr := tailOf(1000); got != "hello world" || tr {
		t.Errorf("tail(oversized) = %q,%v, want the whole log,false", got, tr)
	}
	rc, _, _ := st.ReadLogsTail("r1", "build", 9, 5) // missing step
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if len(b) != 0 {
		t.Errorf("missing-step tail = %q, want empty", b)
	}
}

func TestFileReadLogsTailCoherentUnderGrowth(t *testing.T) {
	st, _ := NewFile(t.TempDir())
	_ = st.SaveRun(sampleRun("g1", time.Now()))
	w, _ := st.LogWriter("g1", "build", 0)
	_, _ = io.WriteString(w, "ab") // below the cap
	_ = w.Close()

	// Open the tail while the file is 2 bytes and under the 5-byte cap.
	rc, truncated, err := st.ReadLogsTail("g1", "build", 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("a sub-cap file should report truncated=false")
	}
	// Now the step keeps writing, growing the file past the cap.
	w2, _ := st.LogWriter("g1", "build", 0)
	_, _ = io.WriteString(w2, "cdefghij")
	_ = w2.Close()

	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	// The snapshot must be the file as of the open (SectionReader), not the
	// grown head — so exactly "ab", never "abcde".
	if string(b) != "ab" {
		t.Errorf("tail under growth = %q, want the coherent snapshot %q", b, "ab")
	}
}

func TestFileGetRunRejectsOversized(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewFile(dir)
	_ = st.SaveRun(sampleRun("r1", time.Now()))
	// Overwrite run.json with an oversized blob (a corrupt/legacy record).
	path := filepath.Join(dir, "runs", "r1", "run.json")
	if err := os.WriteFile(path, make([]byte, maxRunFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetRun("r1"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("GetRun on an oversized record = %v, want a too-large error", err)
	}
}

func TestFileListReadsSidecarNotRunJSON(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewFile(dir)
	if err := st.SaveRun(sampleRun("r1", time.Now())); err != nil {
		t.Fatal(err)
	}
	// The sidecar exists after SaveRun.
	if _, err := os.Stat(filepath.Join(dir, "runs", "r1", "summary.json")); err != nil {
		t.Fatalf("summary.json sidecar should exist after SaveRun: %v", err)
	}
	// Corrupt run.json; listing must still work (it reads the sidecar, not the
	// full record), while GetRun (source of truth) now fails.
	if err := os.WriteFile(filepath.Join(dir, "runs", "r1", "run.json"), []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	sums, err := st.ListRuns(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 1 || sums[0].ID != "r1" {
		t.Fatalf("ListRuns from sidecar = %+v, want [r1]", sums)
	}
	if _, err := st.GetRun("r1"); err == nil {
		t.Error("GetRun should fail on the corrupted run.json")
	}
}

func TestFileListSelfHealsLegacyRun(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewFile(dir)
	// A "legacy" run: run.json only, no sidecar (as if written before this change).
	rd := filepath.Join(dir, "runs", "old")
	if err := os.MkdirAll(rd, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(sampleRun("old", time.Now()))
	if err := os.WriteFile(filepath.Join(rd, "run.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	sums, err := st.ListRuns(0, 0)
	if err != nil || len(sums) != 1 || sums[0].ID != "old" {
		t.Fatalf("ListRuns of a legacy run = %+v, %v", sums, err)
	}
	if _, err := os.Stat(filepath.Join(rd, "summary.json")); err != nil {
		t.Errorf("listing a legacy run should self-heal its sidecar: %v", err)
	}
}

func TestFileListRunsOmitsJobs(t *testing.T) {
	st, _ := NewFile(t.TempDir())
	run := sampleRun("r1", time.Now()) // sampleRun has a build job with a step
	if err := st.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	sums, err := st.ListRuns(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 1 || sums[0].ID != "r1" || sums[0].Status != model.StatusSuccess {
		t.Fatalf("summary = %+v, want r1/success", sums)
	}
	// GetRun still carries the full jobs.
	full, _ := st.GetRun("r1")
	if len(full.Jobs) == 0 {
		t.Error("GetRun should still return the full jobs")
	}
}

func TestFileWriteRunRejectsOversized(t *testing.T) {
	orig := maxRunFileBytes
	maxRunFileBytes = 256 // tiny, so a normal run overflows
	t.Cleanup(func() { maxRunFileBytes = orig })

	st, _ := NewFile(t.TempDir())
	run := sampleRun("r1", time.Now())
	run.Event.Title = strings.Repeat("t", 500) // pushes serialized size past the cap
	if err := st.SaveRun(run); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("SaveRun of an oversized record = %v, want a too-large error (symmetric with GetRun)", err)
	}
}

// A failed rename must not leave its temp file behind: every other error path
// in writeAtomic clears it, and the debris accumulates in the run directory.
func TestFileWriteAtomicCleansUpAfterFailedRename(t *testing.T) {
	dir := t.TempDir()
	// Renaming onto a non-empty directory fails on every supported platform.
	blocker := filepath.Join(dir, "run.json")
	if err := os.MkdirAll(filepath.Join(blocker, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := writeAtomic(dir, "run.json", []byte(`{}`), true); err == nil {
		t.Fatal("writeAtomic onto a non-empty directory should fail")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".run.json-") {
			t.Errorf("temp file %q leaked after a failed rename", e.Name())
		}
	}
}

// A run directory with no readable record used to be skipped everywhere, so it
// was invisible to listing and counting AND unreachable by Prune — its logs
// leaked forever. It must surface as a terminal placeholder that retention can
// then reclaim.
func TestFileUnreadableRunDirIsListedAndPruned(t *testing.T) {
	root := t.TempDir()
	st, err := NewFile(root)
	if err != nil {
		t.Fatal(err)
	}
	// A healthy, newer run that retention should keep.
	if err := st.SaveRun(sampleRun("good", time.Now())); err != nil {
		t.Fatal(err)
	}

	// Debris: both files unreadable, and old enough to be past the grace window.
	debris := filepath.Join(root, "runs", "deadbeef")
	if err := os.MkdirAll(filepath.Join(debris, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"run.json", "summary.json"} {
		if err := os.WriteFile(filepath.Join(debris, name), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * unreadableGrace)
	if err := os.Chtimes(debris, old, old); err != nil {
		t.Fatal(err)
	}

	sums, err := st.ListRuns(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 2 {
		t.Fatalf("ListRuns = %d runs, want 2 (the debris must be visible)", len(sums))
	}
	if n, err := st.CountRuns(); err != nil || n != 2 {
		t.Errorf("CountRuns = %d, %v; want 2, nil", n, err)
	}

	// Retention reclaims it: it is terminal, and older than the run we keep.
	if _, err := st.Prune(1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(debris); !os.IsNotExist(err) {
		t.Errorf("unreadable run dir should have been pruned, stat err = %v", err)
	}
	if _, err := st.GetRun("good"); err != nil {
		t.Errorf("the healthy run must survive: %v", err)
	}
}

// writeRun creates a run's directory before it writes run.json, so a run being
// recorded right now looks briefly unreadable. Treating that as debris would
// let a concurrent Prune delete a live run out from under itself.
func TestFileUnreadableRunDirWithinGraceIsIgnored(t *testing.T) {
	root := t.TempDir()
	st, err := NewFile(root)
	if err != nil {
		t.Fatal(err)
	}
	// A directory that has only just appeared — exactly writeRun's first step.
	fresh := filepath.Join(root, "runs", "inflight")
	if err := os.MkdirAll(filepath.Join(fresh, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}

	sums, err := st.ListRuns(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 0 {
		t.Fatalf("a just-created run dir must not be listed as debris, got %d", len(sums))
	}
	if _, err := st.Prune(0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a just-created run dir must survive Prune: %v", err)
	}
}

func TestFileStoreMissingRun(t *testing.T) {
	st, _ := NewFile(t.TempDir())
	if _, err := st.GetRun("nope"); err == nil {
		t.Error("expected error for missing run")
	}
	// Reading logs for a missing step yields empty, not an error.
	rc, err := st.ReadLogs("nope", "build", 0, 0)
	if err != nil {
		t.Fatalf("ReadLogs missing: %v", err)
	}
	_ = rc.Close()
}

func TestFileCountRuns(t *testing.T) {
	st, _ := NewFile(t.TempDir())
	if n, err := st.CountRuns(); err != nil || n != 0 {
		t.Fatalf("empty CountRuns = %d, %v; want 0, nil", n, err)
	}
	base := time.Now()
	for i := 0; i < 3; i++ {
		_ = st.SaveRun(sampleRun("c"+string(rune('0'+i)), base.Add(time.Duration(-i)*time.Minute)))
	}
	n, err := st.CountRuns()
	if err != nil || n != 3 {
		t.Fatalf("CountRuns = %d, %v; want 3, nil", n, err)
	}
	listed, err := st.ListRuns(0, 0)
	if err != nil || len(listed) != n {
		t.Errorf("CountRuns = %d but ListRuns lists %d (err %v); they must agree", n, len(listed), err)
	}
}

package engine

import (
	"bytes"
	"strings"
	"testing"
)

func TestCappedWriterUnderBudgetPassesThrough(t *testing.T) {
	var buf bytes.Buffer
	c := newCappedWriter(&buf, 100, "MARK")

	n, err := c.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = %d, %v; want 5, nil", n, err)
	}
	if got := buf.String(); got != "hello" {
		t.Errorf("buf = %q, want %q", got, "hello")
	}
}

func TestCappedWriterTruncatesAtBudget(t *testing.T) {
	var buf bytes.Buffer
	c := newCappedWriter(&buf, 4, "MARK")

	// A write that crosses the budget keeps the head and appends the marker.
	if n, err := c.Write([]byte("abcdefgh")); err != nil || n != 8 {
		t.Fatalf("Write = %d, %v; want 8, nil (a short write would fail the step)", n, err)
	}
	if got := buf.String(); got != "abcdMARK" {
		t.Errorf("buf = %q, want %q", got, "abcdMARK")
	}

	// Everything after is dropped, and the marker is not repeated.
	if n, err := c.Write([]byte("ijkl")); err != nil || n != 4 {
		t.Fatalf("post-budget Write = %d, %v; want 4, nil", n, err)
	}
	if got := buf.String(); got != "abcdMARK" {
		t.Errorf("buf = %q, want the marker written exactly once", got)
	}
}

// The budget can be spent exactly, with no room for even one more byte.
func TestCappedWriterExactBudget(t *testing.T) {
	var buf bytes.Buffer
	c := newCappedWriter(&buf, 4, "MARK")

	if _, err := c.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "abcd" {
		t.Errorf("buf = %q, want %q (no marker until something is actually dropped)", got, "abcd")
	}
	if _, err := c.Write([]byte("e")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "abcdMARK" {
		t.Errorf("buf = %q, want the marker once the first byte is dropped", got)
	}
}

// A zero budget truncates immediately rather than passing everything through.
func TestCappedWriterZeroBudget(t *testing.T) {
	var buf bytes.Buffer
	c := newCappedWriter(&buf, 0, "MARK")

	if n, err := c.Write([]byte("abc")); err != nil || n != 3 {
		t.Fatalf("Write = %d, %v; want 3, nil", n, err)
	}
	if got := buf.String(); got != "MARK" {
		t.Errorf("buf = %q, want just the marker", got)
	}
}

// A step is not obliged to ever emit a newline — a progress bar redrawing with
// \r never does. linePrefixer held every such byte until one arrived, and
// `janus run` keeps that buffer for the whole step, so it grew without bound.
func TestLinePrefixerBoundsUnterminatedLines(t *testing.T) {
	var buf bytes.Buffer
	p := newLinePrefixer(&buf, "[build] ")

	// Five times the bound, with no newline anywhere.
	total := 5 * maxPrefixedLineBytes
	chunk := bytes.Repeat([]byte("x"), 4096)
	for written := 0; written < total; written += len(chunk) {
		n, err := p.Write(chunk)
		if err != nil {
			t.Fatalf("Write errored: %v", err)
		}
		if n != len(chunk) {
			t.Fatalf("Write = %d, want %d (a short write would fail the step)", n, len(chunk))
		}
		if len(p.buf) >= 2*maxPrefixedLineBytes {
			t.Fatalf("buffer grew to %d bytes, want it bounded near %d", len(p.buf), maxPrefixedLineBytes)
		}
	}

	// The output was emitted rather than hoarded, and stayed attributable.
	if buf.Len() == 0 {
		t.Fatal("nothing was emitted for an unterminated line")
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("[build] ")) {
		t.Error("emitted chunks should still carry the job prefix")
	}
	if got := bytes.Count(buf.Bytes(), []byte("[build] ")); got < 2 {
		t.Errorf("prefix appears %d times, want it repeated across chunks", got)
	}

	// Close still flushes the remainder.
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if len(p.buf) != 0 {
		t.Errorf("Close left %d bytes buffered", len(p.buf))
	}
}

// A newline-terminated stream must be unaffected by the bound.
func TestLinePrefixerNormalLinesUnchanged(t *testing.T) {
	var buf bytes.Buffer
	p := newLinePrefixer(&buf, "[build] ")
	if _, err := p.Write([]byte("one\ntwo\n")); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "[build] one\n[build] two\n"; got != want {
		t.Errorf("buf = %q, want %q", got, want)
	}
}

// os/exec fails a command on a short write or a write error, so a capped log
// must never report either — no matter how the output is chunked.
func TestCappedWriterNeverShortWrites(t *testing.T) {
	var buf bytes.Buffer
	c := newCappedWriter(&buf, 10, logTruncatedMarker)

	for _, chunk := range []string{"aaa", "bbbb", "cccccccc", "d", ""} {
		n, err := c.Write([]byte(chunk))
		if err != nil {
			t.Fatalf("Write(%q) errored: %v", chunk, err)
		}
		if n != len(chunk) {
			t.Errorf("Write(%q) = %d, want %d", chunk, n, len(chunk))
		}
	}
	if !strings.Contains(buf.String(), "log truncated") {
		t.Errorf("buf = %q, want the truncation marker", buf.String())
	}
}

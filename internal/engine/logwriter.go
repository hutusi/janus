package engine

import (
	"bytes"
	"io"
	"sync"
)

// lockedWriter serializes concurrent writes to a shared underlying writer (the
// CLI's stdout), so output from jobs running in parallel never interleaves
// mid-write. One lockedWriter (one mutex) is shared by all of a run's steps.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// defaultLogLimit caps a single step's log at 10 MiB unless configured
// otherwise. Far above any real step's output, but it bounds an unattended
// daemon out of the box instead of only once an operator thinks to set it.
const defaultLogLimit int64 = 10 << 20

// logTruncatedMarker closes a truncated log. It is written into the log itself,
// so a truncated log can never be mistaken for a complete one — by a reader, or
// by someone grepping it for the string that would have come next.
const logTruncatedMarker = "\njanus: log truncated at the configured log_limit\n"

// cappedWriter bounds how many bytes one step may write to its log sink. Once
// the budget is spent it emits a single marker and drops the rest, keeping the
// *head* of the log: that is where the command and its first error are, a
// runaway log's tail is worthless by definition, and — decisively — the stored
// log is append-only. ReadLogs serves it from a caller-supplied offset and a
// live ?follow=1 stream advances that offset monotonically, so dropping bytes
// from the front would corrupt every reader mid-stream.
//
// Write ALWAYS reports len(p) with a nil error, even past the budget: os/exec
// copies the step's pipe into this writer and treats a short write or an error
// as a failure of the command. Capping a log must never change a step's
// outcome, so the truncation is invisible to the process being run.
type cappedWriter struct {
	w         io.Writer
	remaining int64
	marker    []byte
	truncated bool
}

func newCappedWriter(w io.Writer, limit int64, marker string) *cappedWriter {
	return &cappedWriter{w: w, remaining: limit, marker: []byte(marker)}
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.truncated {
		return len(p), nil
	}
	if int64(len(p)) <= c.remaining {
		n, err := c.w.Write(p)
		c.remaining -= int64(n)
		return len(p), err
	}
	// This write crosses the budget: emit what fits, then the marker, once.
	head := p[:c.remaining]
	c.remaining = 0
	c.truncated = true
	if len(head) > 0 {
		if _, err := c.w.Write(head); err != nil {
			return len(p), err
		}
	}
	_, err := c.w.Write(c.marker)
	return len(p), err
}

// maxPrefixedLineBytes bounds how much of an unterminated line linePrefixer
// will hold. Steps are not obliged to emit newlines — a progress bar redrawing
// with \r, or one enormous line, never does — and `janus run` keeps this buffer
// in memory for the whole step, so without a bound it grows without limit. It
// is the tee's counterpart to log_limit, which bounds only the stored sink.
const maxPrefixedLineBytes = 64 << 10

// linePrefixer prepends a prefix to every complete line, emitting each line as
// a single Write to the underlying writer so a parallel job cannot slip output
// between a line's prefix and its content. Partial lines are buffered until a
// newline arrives (or Close flushes the remainder) — except that a line running
// past maxPrefixedLineBytes is emitted in bounded chunks, each carrying the
// prefix. Repeating the prefix mid-line is the deliberate trade: interleaved
// output from parallel jobs stays attributable, and memory stays bounded.
//
// The buffer never exceeds maxPrefixedLineBytes, including part-way through a
// single Write, however much that Write carries.
type linePrefixer struct {
	w      io.Writer
	prefix []byte
	buf    []byte
}

func newLinePrefixer(w io.Writer, prefix string) *linePrefixer {
	return &linePrefixer{w: w, prefix: []byte(prefix)}
}

// Write buffers b a bounded slice at a time. Consuming the input incrementally
// — rather than appending all of it and trimming afterwards — is what makes the
// bound a property of this writer instead of a property of how the caller
// happens to chunk its writes: os/exec currently hands over at most a pipe
// buffer at a time, but nothing here should depend on that.
//
// It always reports a full write with no error on success. os/exec fails a
// command whose output copy short-writes, and bounding a log must never change
// a step's exit code.
func (p *linePrefixer) Write(b []byte) (int, error) {
	total := len(b)
	for len(b) > 0 {
		// Never take more than keeps the buffer inside its bound.
		n := min(maxPrefixedLineBytes-len(p.buf), len(b))
		p.buf = append(p.buf, b[:n]...)
		b = b[n:]

		for {
			i := bytes.IndexByte(p.buf, '\n')
			if i < 0 {
				break
			}
			if err := p.emit(p.buf[:i+1]); err != nil {
				return 0, err
			}
			p.buf = p.buf[i+1:]
		}
		// Full, with no newline to break it: emit a chunk rather than grow.
		// This also guarantees room on the next pass, so the loop advances.
		if len(p.buf) >= maxPrefixedLineBytes {
			if err := p.emit(p.buf); err != nil {
				return 0, err
			}
			p.buf = p.buf[:0]
		}
	}
	return total, nil
}

// emit writes one prefixed piece as a single Write, so a parallel job cannot
// slip output between a prefix and its content.
func (p *linePrefixer) emit(piece []byte) error {
	out := make([]byte, 0, len(p.prefix)+len(piece))
	out = append(out, p.prefix...)
	out = append(out, piece...)
	_, err := p.w.Write(out)
	return err
}

// Close flushes any buffered final line (one without a trailing newline).
func (p *linePrefixer) Close() error {
	if len(p.buf) == 0 {
		return nil
	}
	line := make([]byte, 0, len(p.prefix)+len(p.buf)+1)
	line = append(line, p.prefix...)
	line = append(line, p.buf...)
	line = append(line, '\n')
	p.buf = nil
	_, err := p.w.Write(line)
	return err
}

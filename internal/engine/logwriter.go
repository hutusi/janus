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

// linePrefixer prepends a prefix to every complete line, emitting each line as
// a single Write to the underlying writer so a parallel job cannot slip output
// between a line's prefix and its content. Partial lines are buffered until a
// newline arrives (or Close flushes the remainder).
type linePrefixer struct {
	w      io.Writer
	prefix []byte
	buf    []byte
}

func newLinePrefixer(w io.Writer, prefix string) *linePrefixer {
	return &linePrefixer{w: w, prefix: []byte(prefix)}
}

func (p *linePrefixer) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		line := make([]byte, 0, len(p.prefix)+i+1)
		line = append(line, p.prefix...)
		line = append(line, p.buf[:i+1]...)
		if _, err := p.w.Write(line); err != nil {
			return 0, err
		}
		p.buf = p.buf[i+1:]
	}
	return len(b), nil
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

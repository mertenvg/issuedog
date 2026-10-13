package agent

import (
	"strings"
	"sync"
	"unicode"
)

// Tail is an io.Writer that keeps only the last bytes written to it, up
// to its size, so a run's stderr stays bounded in memory.
type Tail struct {
	mu    sync.Mutex
	size  int
	buf   []byte
	total int64
}

// NewTail returns a Tail that keeps the last size bytes.
func NewTail(size int) *Tail { return &Tail{size: size} }

// Write keeps the end of p, dropping the oldest bytes held. It never fails.
func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total += int64(len(p))
	if len(p) >= t.size {
		t.buf = append(t.buf[:0], p[len(p)-t.size:]...)
		return len(p), nil
	}
	if over := len(t.buf) + len(p) - t.size; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, p...)
	return len(p), nil
}

// Len is how many bytes it holds.
func (t *Tail) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.buf)
}

// Total is how many bytes were written to it.
func (t *Tail) Total() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.total
}

// Text is what it holds as text: invalid UTF-8, such as a rune cut at the
// start, and control characters other than newlines and tabs become U+FFFD.
func (t *Tail) Text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return unicode.ReplacementChar
		}
		return r
	}, strings.ToValidUTF8(string(t.buf), string(unicode.ReplacementChar)))
}

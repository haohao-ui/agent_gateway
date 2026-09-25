package runner

import (
	"sync"
	"unicode/utf8"
)

// tailBuffer is a sliding window over a byte stream. It consumes every byte it is
// given (so a chatty child never blocks on a full pipe) but retains only the last
// maxBytes of them. Writes and reads may happen from different goroutines.
type tailBuffer struct {
	mu       sync.Mutex
	buf      []byte
	maxBytes int
	total    int64
}

func newTailBuffer(maxBytes int) *tailBuffer {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxOutputBytes
	}
	return &tailBuffer{
		buf:      make([]byte, 0, maxBytes),
		maxBytes: maxBytes,
	}
}

// Write appends p to the sliding window and always reports a full write.
func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := len(p)
	b.total += int64(n)

	switch {
	case n >= b.maxBytes:
		// The chunk alone overflows the window; only its tail can survive.
		b.buf = append(b.buf[:0], p[n-b.maxBytes:]...)
	default:
		overflow := len(b.buf) + n - b.maxBytes
		if overflow > 0 {
			b.buf = append(b.buf[:0], b.buf[overflow:]...)
		}
		b.buf = append(b.buf, p...)
	}
	return n, nil
}

// Result returns the retained tail as valid UTF-8 plus the truncation flag.
func (b *tailBuffer) Result() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return sanitizeTailUTF8(b.buf), b.total > int64(b.maxBytes)
}

// TotalBytes reports how many bytes the child produced in total.
func (b *tailBuffer) TotalBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// sanitizeTailUTF8 makes the retained tail valid UTF-8. A byte window can cut a
// multi-byte rune at either end, and a process killed mid-write can leave an
// incomplete rune at the tail, so both edges are trimmed.
func sanitizeTailUTF8(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}

	// Trim a leading partial rune: continuation bytes or the start of a sequence
	// whose remainder was dropped by the window.
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		if r != utf8.RuneError || size > 1 {
			break
		}
		data = data[1:]
	}

	// Trim a trailing partial rune, which is always the shorter fix.
	for len(data) > 0 {
		r, size := utf8.DecodeLastRune(data)
		if r != utf8.RuneError || size > 1 {
			break
		}
		data = data[:len(data)-1]
	}

	// Anything still invalid came from bytes in the middle of the stream; drop
	// exactly those bytes instead of truncating the whole tail.
	return string(utf8SliceToValid(data))
}

// utf8SliceToValid rebuilds a valid UTF-8 byte slice, skipping every invalid byte.
func utf8SliceToValid(data []byte) []byte {
	if utf8.Valid(data) {
		return data
	}
	out := make([]byte, 0, len(data))
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size <= 1 {
			data = data[1:]
			continue
		}
		out = utf8.AppendRune(out, r)
		data = data[size:]
	}
	return out
}

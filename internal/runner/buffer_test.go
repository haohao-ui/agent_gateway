package runner

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTailBufferRetainsBoundedTail(t *testing.T) {
	buf := newTailBuffer(8)

	if _, err := buf.Write([]byte("abc")); err != nil {
		t.Fatalf("write: %v", err)
	}
	text, truncated := buf.Result()
	if text != "abc" || truncated {
		t.Fatalf("got (%q, %v), want (\"abc\", false)", text, truncated)
	}

	if _, err := buf.Write([]byte("defghij")); err != nil {
		t.Fatalf("write: %v", err)
	}
	text, truncated = buf.Result()
	if text != "cdefghij" || !truncated {
		t.Fatalf("got (%q, %v), want (\"cdefghij\", true)", text, truncated)
	}
	if buf.TotalBytes() != 10 {
		t.Fatalf("total = %d, want 10", buf.TotalBytes())
	}
}

// TestTailBufferHandlesOversizedChunk covers the branch where a single write is
// larger than the whole window.
func TestTailBufferHandlesOversizedChunk(t *testing.T) {
	buf := newTailBuffer(4)

	if _, err := buf.Write([]byte("0123456789")); err != nil {
		t.Fatalf("write: %v", err)
	}
	text, truncated := buf.Result()
	if text != "6789" || !truncated {
		t.Fatalf("got (%q, %v), want (\"6789\", true)", text, truncated)
	}
}

func TestTailBufferEmpty(t *testing.T) {
	buf := newTailBuffer(4)
	text, truncated := buf.Result()
	if text != "" || truncated {
		t.Fatalf("got (%q, %v), want (\"\", false)", text, truncated)
	}
}

// TestSanitizeTailUTF8TrimsCutRunes checks both window edges: a leading partial
// rune (cut at the front) and a trailing partial rune (cut mid-write).
func TestSanitizeTailUTF8TrimsCutRunes(t *testing.T) {
	// "中" is 0xE4 0xB8 0xAD; the window starts one byte into it.
	cutFront := append([]byte{0xB8, 0xAD}, []byte("ok")...)
	if got := sanitizeTailUTF8(cutFront); got != "ok" {
		t.Fatalf("front cut: got %q, want %q", got, "ok")
	}

	cutBack := append([]byte("ok"), 0xE4, 0xB8)
	if got := sanitizeTailUTF8(cutBack); got != "ok" {
		t.Fatalf("back cut: got %q, want %q", got, "ok")
	}
}

// TestSanitizeTailUTF8DropsOnlyInvalidBytes keeps valid text around a broken
// sequence instead of discarding the whole tail.
func TestSanitizeTailUTF8DropsOnlyInvalidBytes(t *testing.T) {
	raw := append([]byte("a"), 0xFF, 0xFE)
	raw = append(raw, []byte("b")...)

	got := sanitizeTailUTF8(raw)
	if !utf8.ValidString(got) {
		t.Fatalf("result is not valid UTF-8: %q", got)
	}
	if got != "ab" {
		t.Fatalf("got %q, want %q", got, "ab")
	}
}

func TestSanitizeTailUTF8KeepsValidText(t *testing.T) {
	in := strings.Repeat("中🙂", 100)
	if got := sanitizeTailUTF8([]byte(in)); got != in {
		t.Fatalf("valid text was modified")
	}
}

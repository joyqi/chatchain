package chat

import (
	"strings"
	"testing"
)

// lineChunker re-lines an endless JSON argument stream into preview rows:
// fixed-width cuts on rune boundaries, partial runes held for the next delta.
func TestLineChunker(t *testing.T) {
	var lines []string
	c := &lineChunker{width: 10, emit: func(l string) { lines = append(lines, l) }}

	c.add(`{"path":"a.txt","conte`)
	c.add(`nt":"hello"}`)
	c.flush()
	joined := strings.Join(lines, "")
	if joined != `{"path":"a.txt","content":"hello"}` {
		t.Fatalf("chunker lost bytes: %q", joined)
	}
	for i, l := range lines[:len(lines)-1] {
		if len(l) > 10 {
			t.Fatalf("line %d exceeds width: %q", i, l)
		}
	}

	// A multi-byte rune straddling the cut is never split.
	lines = nil
	c = &lineChunker{width: 4, emit: func(l string) { lines = append(lines, l) }}
	c.add("ab你") // '你' = 3 bytes crossing the width-4 boundary… fed in halves:
	c.add("好cd")
	c.flush()
	if strings.Join(lines, "") != "ab你好cd" {
		t.Fatalf("rune split: %v", lines)
	}
	for _, l := range lines {
		if !isUTF8Start(l[0]) {
			t.Fatalf("line starts mid-rune: %q", l)
		}
	}

	// Flush with nothing pending emits nothing.
	n := len(lines)
	c.flush()
	if len(lines) != n {
		t.Fatal("empty flush emitted a line")
	}
}

package chat

import (
	"context"
	"io"
	"strings"
	"testing"

	"chatchain/provider"
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

// composeSink records the preview lifecycle including writes.
type composeSink struct{ events []string }

func (s *composeSink) CommitLines(...string) {}
func (s *composeSink) Done()                 {}
func (s *composeSink) BlockPreview(label string) io.WriteCloser {
	s.events = append(s.events, "open:"+label)
	return &composeSinkWriter{s}
}
func (s *composeSink) RelabelPreview(label string) {
	s.events = append(s.events, "relabel:"+label)
}

type composeSinkWriter struct{ s *composeSink }

func (w *composeSinkWriter) Write(p []byte) (int, error) {
	w.s.events = append(w.s.events, "write:"+string(p))
	return len(p), nil
}
func (w *composeSinkWriter) Close() error {
	w.s.events = append(w.s.events, "close")
	return nil
}

// fakeObserverTP is a ToolProvider whose only job is handing back the
// observer callback.
type fakeObserverTP struct{ fn func(name, delta string) }

func (f *fakeObserverTP) SetToolCallObserver(fn func(name, delta string)) { f.fn = fn }
func (f *fakeObserverTP) StreamChatWithTools(context.Context, []provider.Message, []provider.ToolDef, io.Writer, io.WriteCloser) (string, string, []provider.ToolCall, error) {
	return "", "", nil, nil
}

// The staged view mirrors the final display: "[name …]" header updating as
// the name streams in, "⎿"-connected first argument row, a new preview per
// distinct call, everything deferred-closed at cleanup.
func TestWatchToolComposing(t *testing.T) {
	sink := &composeSink{}
	tp := &fakeObserverTP{}
	cleanup := watchToolComposing(newTurnPhases(nil), sink, tp)
	if tp.fn == nil {
		t.Fatal("observer not installed")
	}

	long := strings.Repeat("x", 100)
	tp.fn("write", long)        // opens with the partial name
	tp.fn("write_file", long)   // same call: name grew → relabel
	tp.fn("bash", "small")      // distinct call → fold + new preview
	tp.fn("", "")               // atomic backends: ignored
	cleanup()

	got := strings.Join(sink.events, "\n")
	for _, want := range []string{
		"open:[write …]",
		"write:⎿ " + strings.Repeat("x", 80) + "\n",
		"relabel:[write_file …]",
		"close",
		"open:[bash …]",
		"write:⎿ small\n", // cleanup flushes the pending partial row
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in events:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "close") {
		t.Fatalf("cleanup should deferred-close the last preview:\n%s", got)
	}
	if tp.fn != nil {
		t.Fatal("cleanup should detach the observer")
	}

	// Continuation rows lose the connector, keeping the final display's
	// hanging indent.
	if strings.Count(got, "write:⎿") != 2 {
		t.Fatalf("exactly the first row of each call carries the connector:\n%s", got)
	}
	if !strings.Contains(got, "write:  "+strings.Repeat("x", 20)) {
		t.Fatalf("continuation row missing its indent:\n%s", got)
	}
}

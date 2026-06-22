package promptui

import (
	"io"
	"os"
	"sync"
	"time"
	"unicode/utf8"

	"chatchain/internal/promptui/screenbuf"
	"golang.org/x/term"
)

// StreamView renders streaming text as a header line followed by a rolling
// viewport of the most recent Window wrapped lines, repainted in place via
// screenbuf as bytes arrive. It implements io.Writer — copy a stream into it —
// and Done finalizes the block (clearing it, optionally leaving a summary line).
//
// The header is either a static Header string, or — when Spinner is set — an
// animated spinner frame followed by Label, advanced on a background ticker so
// it keeps spinning even while the stream is idle.
//
// Partial UTF-8 runes split across Writes are buffered until complete. On a
// non-terminal Stdout it degrades to plain passthrough (no cursor control), so
// piped output stays clean.
type StreamView struct {
	// Header is a static header line (used when Spinner is empty). Empty means
	// no header row.
	Header string
	// Spinner, if non-empty, animates through these frames as the header symbol.
	Spinner []string
	// Label is the text shown after the spinner frame in the header.
	Label string
	// Interval is the spinner frame interval (default 100ms).
	Interval time.Duration
	// HeaderStyle decorates the spinner+label header line (e.g. dim).
	HeaderStyle func(string) string
	// Window is how many trailing wrapped lines stay visible (default 3).
	Window int
	// Indent prefixes each viewport line (e.g. "  "); not applied to the header.
	Indent string
	// RuneWidth returns a rune's display width (CJK = 2). Defaults to 1 per rune,
	// which mis-wraps CJK, so callers with CJK content should inject a width func.
	RuneWidth func(rune) int
	// Style optionally decorates each wrapped viewport line (e.g. dim).
	Style func(string) string
	// Stdout is the output sink; defaults to os.Stdout.
	Stdout io.Writer

	out     io.Writer
	tty     bool
	sb      *screenbuf.ScreenBuf
	width   int      // wrap width (terminal width minus the indent)
	lines   []string // completed wrapped lines (kept bounded)
	cur     []rune   // in-progress (not-yet-wrapped) current line
	curW    int      // display width of cur
	pend    []byte   // leftover bytes of a partial UTF-8 rune across Writes
	started bool

	mu      sync.Mutex     // guards renders from Write and the spinner ticker
	frame   int            // spinner frame index
	stop    chan struct{}  // closed by Done to stop the ticker
	wg      sync.WaitGroup // tracks the ticker goroutine
	ticking bool
}

func (v *StreamView) rw(r rune) int {
	if v.RuneWidth != nil {
		return v.RuneWidth(r)
	}
	return 1
}

func (v *StreamView) window() int {
	if v.Window > 0 {
		return v.Window
	}
	return 3
}

func (v *StreamView) interval() time.Duration {
	if v.Interval > 0 {
		return v.Interval
	}
	return 100 * time.Millisecond
}

// start fills defaults, detects the terminal, and launches the spinner ticker.
func (v *StreamView) start() {
	v.started = true
	v.out = v.Stdout
	if v.out == nil {
		v.out = os.Stdout
	}
	tw := 80
	if f, ok := v.out.(*os.File); ok {
		fd := int(f.Fd())
		if term.IsTerminal(fd) {
			v.tty = true
			if w, _, err := term.GetSize(fd); err == nil && w > 0 {
				tw = w
			}
		}
	}
	indentW := 0
	for _, r := range v.Indent {
		indentW += v.rw(r)
	}
	v.width = tw - indentW
	if v.width < 1 {
		v.width = 1
	}
	if !v.tty {
		return
	}
	v.sb = screenbuf.New(v.out)
	if len(v.Spinner) > 0 {
		v.stop = make(chan struct{})
		v.ticking = true
		v.wg.Add(1)
		go v.tick()
	}
}

// tick advances the spinner frame and repaints on a fixed interval, so the
// header keeps animating even when no new bytes arrive.
func (v *StreamView) tick() {
	defer v.wg.Done()
	t := time.NewTicker(v.interval())
	defer t.Stop()
	for {
		select {
		case <-v.stop:
			return
		case <-t.C:
			v.mu.Lock()
			v.frame++
			if v.sb != nil {
				v.render()
			}
			v.mu.Unlock()
		}
	}
}

// Write feeds a chunk of the stream into the viewport.
func (v *StreamView) Write(p []byte) (int, error) {
	if !v.started {
		v.start()
	}
	if !v.tty {
		return v.out.Write(p)
	}
	v.mu.Lock()
	v.feed(p)
	v.render()
	v.mu.Unlock()
	return len(p), nil
}

// feed decodes p (resuming any buffered partial rune) and appends to the
// wrapped-line state. Rendering is separate so it can be unit-tested.
func (v *StreamView) feed(p []byte) {
	data := p
	if len(v.pend) > 0 {
		data = append(v.pend, p...)
		v.pend = nil
	}
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size == 1 {
			if len(data) < utf8.UTFMax { // incomplete trailing rune; wait for more
				v.pend = append(v.pend[:0], data...)
				break
			}
			data = data[1:] // genuine invalid byte; skip it
			continue
		}
		data = data[size:]
		v.feedRune(r)
	}
}

func (v *StreamView) feedRune(r rune) {
	switch r {
	case '\r':
		return
	case '\n':
		v.flushLine()
	default:
		w := v.rw(r)
		if v.curW+w > v.width {
			v.flushLine()
		}
		v.cur = append(v.cur, r)
		v.curW += w
	}
}

func (v *StreamView) flushLine() {
	v.lines = append(v.lines, string(v.cur))
	v.cur = v.cur[:0]
	v.curW = 0
	if max := v.window() + 2; len(v.lines) > max { // only the tail is ever shown
		v.lines = v.lines[len(v.lines)-max:]
	}
}

func (v *StreamView) visibleLines() []string {
	out := make([]string, 0, len(v.lines)+1)
	out = append(out, v.lines...)
	if len(v.cur) > 0 {
		out = append(out, string(v.cur))
	}
	if w := v.window(); len(out) > w {
		out = out[len(out)-w:]
	}
	return out
}

// headerLine builds the current header text (static, or animated spinner+label).
func (v *StreamView) headerLine() string {
	if len(v.Spinner) == 0 {
		return v.Header
	}
	h := v.Spinner[v.frame%len(v.Spinner)]
	if v.Label != "" {
		h += " " + v.Label
	}
	if v.HeaderStyle != nil {
		h = v.HeaderStyle(h)
	}
	return h
}

// render repaints the whole block (header + visible window) in place. screenbuf
// clears the previous frame and buffers this one into a single write. Caller must
// hold v.mu.
func (v *StreamView) render() {
	v.sb.Reset()
	if h := v.headerLine(); h != "" {
		v.sb.WriteString(h)
	}
	for _, ln := range v.visibleLines() {
		if v.Style != nil {
			ln = v.Style(ln)
		}
		v.sb.WriteString(v.Indent + ln)
	}
	v.sb.Flush()
}

// Done finalizes the stream: it stops the spinner, clears the rolling block, then
// prints final as a permanent line if non-empty (e.g. a collapsed summary). A
// no-op if nothing was ever written.
func (v *StreamView) Done(final string) {
	if !v.started {
		return
	}
	if v.ticking {
		close(v.stop)
		v.wg.Wait() // ticker fully stopped; no concurrent renders past here
		v.ticking = false
	}
	if !v.tty {
		io.WriteString(v.out, "\n")
		return
	}
	v.mu.Lock()
	v.sb.Reset()
	v.sb.Clear()
	v.sb.Flush()
	if final != "" {
		io.WriteString(v.out, final+"\n")
	}
	v.mu.Unlock()
}

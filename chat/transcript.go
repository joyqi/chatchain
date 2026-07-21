package chat

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// The transcript is the session's single write surface for the chat area:
// every block that lands in the scrollback — user input, thinking, markdown
// content, tool calls with their results, notices, errors, resume echoes —
// is declared here, and the transcript alone decides the spacing between
// them. Two rules replace the ad-hoc "" commits of the past:
//
//   - every block opens with exactly one blank separator (above the first
//     block sits the pre-Program banner or the previous turn), consecutive
//     same-kind notices/errors grouping into one block;
//   - blank lines INSIDE a block are deferred until more content follows, so
//     no block can export trailing blanks for a neighbor to lean on (the
//     fragility behind the resume-echo regression).
//
// It also owns the lifecycle-widget verbs (thinking, tool calls), so the
// staging view and the settled scrollback are spaced identically by
// construction. Safe for concurrent use (the async MCP reporter interleaves
// with streaming turns).

// blockKind classifies the chat area's logical blocks.
type blockKind uint8

const (
	blockNone blockKind = iota
	blockUser
	blockThinking
	blockContent
	blockToolCall
	blockNotice
	blockError
	blockEcho
	blockImage
)

// transcriptSurface is what the transcript needs from the ui. *ui.UI
// satisfies it; tests substitute a recorder.
type transcriptSurface interface {
	PrintLines(lines ...string)
	UserBlock(display string)
	CallPreview(label string)
	CallDetail(detail string)
	ClosePreview()
}

type transcript struct {
	mu          sync.Mutex
	u           transcriptSurface
	tokens      *tokenCounter // estimator behind the thinking meter
	last        blockKind
	pending     int    // deferred block-interior blank lines
	callUp      bool   // lifecycle widget raised (its block separator already paid)
	thinkingUp  bool   // the thinking widget owns the (single) widget slot
	contentOpen bool   // a markdown content block is streaming (renderer may hold buffered lines)
	pendingCall string // tool call announced while thinking/content streams; raised at close
	orphanSep   bool   // a dropped widget's separator awaits reuse by the next block
}

func newTranscript(u transcriptSurface, tokens *tokenCounter) *transcript {
	return &transcript{u: u, tokens: tokens}
}

// beginLocked opens a new block: the previous block's deferred blanks die,
// one separator is paid — unless a dropped widget left its separator behind
// (the orphaned blank serves as this block's), or this is the session's FIRST
// block: the environment above it (banner, pre-loop prompt, resume echo)
// always ends with exactly one blank of its own.
func (t *transcript) beginLocked(kind blockKind) {
	t.pending = 0
	first := t.last == blockNone
	t.last = kind
	if first || t.orphanSep {
		t.orphanSep = false
		return
	}
	t.u.PrintLines("")
}

// resetTurn clears widget bookkeeping at a turn boundary. A widget dropped
// unsettled (interrupt, stream death — sink.Done discarded it) has already
// paid a separator with nothing under it; mark it for reuse so the next
// block doesn't stack a second blank on top.
func (t *transcript) resetTurn() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.callUp || t.thinkingUp {
		t.orphanSep = true
	}
	t.callUp, t.thinkingUp, t.contentOpen, t.pendingCall = false, false, false, ""
}

// pushLocked commits lines into the current block, deferring interior blank
// lines until more content follows (a block never ends with trailing blanks).
// Entries with embedded newlines (a provider error carrying its JSON body)
// are expanded first: the latch needs line granularity, and downstream row
// accounting (region tail, frame anchor, cursor) assumes one row per line.
func (t *transcript) pushLocked(lines []string) {
	out := make([]string, 0, len(lines)+t.pending)
	for _, chunk := range lines {
		for _, ln := range strings.Split(chunk, "\n") {
			ln = strings.TrimSuffix(ln, "\r")
			if strings.TrimSpace(ln) == "" {
				t.pending++
				continue
			}
			for ; t.pending > 0; t.pending-- {
				out = append(out, "")
			}
			out = append(out, ln)
		}
	}
	if len(out) > 0 {
		t.u.PrintLines(out...)
	}
}

// user renders the submitted input as the ❯ block.
func (t *transcript) user(display string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.beginLocked(blockUser)
	t.u.UserBlock(display)
}

// notice prints a dim one-liner; consecutive notices group into one block.
func (t *transcript) notice(format string, a ...any) {
	t.grouped(blockNotice, DimStyle.Sprintf(format, a...))
}

// error prints a red one-liner; consecutive errors group into one block.
func (t *transcript) error(format string, a ...any) {
	t.grouped(blockError, ErrorStyle.Sprintf(format, a...))
}

func (t *transcript) grouped(kind blockKind, line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last != kind {
		t.beginLocked(kind)
	}
	t.pushLocked([]string{line})
}

// echo replays pre-rendered history (session resume) as one block; the latch
// swallows its trailing blanks, interior spacing passes through.
func (t *transcript) echo(lines []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.beginLocked(blockEcho)
	t.pushLocked(lines)
}

// beginRound clears the streaming-phase guards at the top of a stream round:
// a round that died mid-stream (retry, provider error) may have leaked
// thinkingUp/contentOpen, which would silently defer the next round's widget
// forever.
func (t *transcript) beginRound() {
	t.mu.Lock()
	t.thinkingUp, t.contentOpen = false, false
	t.mu.Unlock()
}

// markContent marks the content block open. The provider's content writer
// calls it on the FIRST content byte — on the stream goroutine, so the mark
// happens-before any tool-call delta that follows on that same goroutine;
// waiting for the turn goroutine's openContent would leave a window where a
// text delta and a tool delta arriving in one network read raise the widget
// ahead of the content (the inversion this deferral exists to prevent).
func (t *transcript) markContent() {
	t.mu.Lock()
	t.contentOpen = true
	t.mu.Unlock()
}

// openContent marks a markdown content block as streaming and returns its
// committer. While it is open, a tool call announced by the stream observer
// is only remembered: the renderer may still hold whole buffered blocks (a
// trailing table flushes only at stream end), and raising the widget first
// would invert block order — the widget's separator lands, then the buffered
// content re-opens after it (seen live as a double blank above the content
// and the widget glued below it). closeContent raises the deferred call.
func (t *transcript) openContent() func(lines ...string) {
	t.mu.Lock()
	t.contentOpen = true
	t.mu.Unlock()
	return t.contentBlock()
}

// closeContent ends the streaming content block (call after the renderer's
// final flush) and raises a tool call deferred while it was open.
func (t *transcript) closeContent() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.contentOpen = false
	t.raisePendingLocked()
}

// contentBlock returns the committer for one markdown content block. The
// block opens lazily on the first line, and re-opens (new separator) if an
// async block — an MCP failure notice — interleaved since.
func (t *transcript) contentBlock() func(lines ...string) {
	opened := false
	return func(lines ...string) {
		t.mu.Lock()
		defer t.mu.Unlock()
		if !opened || t.last != blockContent {
			opened = true
			t.beginLocked(blockContent)
		}
		t.pushLocked(lines)
	}
}

// image commits a rendered image block: the half-block rows, then a dim
// caption ("🖼 saved: <path>") inside the same block, all under a uniform
// two-space left indent so the picture doesn't sit flush against the edge.
// The indent is safe to prepend: every row is SGR-self-contained, so the
// leading spaces render in the terminal's own colors.
func (t *transcript) image(rows []string, caption string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.callUp {
		// An image-generation widget is up: the image IS its result — morph
		// the widget into the image block in place (separator already paid
		// at the raise; the region replaces the preview rows bottom-up).
		t.callUp = false
		t.pending = 0
		t.last = blockImage
		t.u.ClosePreview()
	} else {
		t.beginLocked(blockImage)
	}
	const indent = "  "
	indented := make([]string, len(rows))
	for i, r := range rows {
		indented[i] = indent + r
	}
	t.pushLocked(indented)
	t.pushLocked([]string{indent + DimStyle.Sprint(caption)})
}

// openCall raises the tool-call lifecycle widget ("⠋ [name …]" over the live
// "⎿ elapsed" row), or expands its header in place when already up — the
// separator is paid once per widget. While the thinking widget owns the slot
// (providers can stream tool-call deltas before the reasoning pipe closes),
// the call is only remembered; settleThinking raises it.
func (t *transcript) openCall(label string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.thinkingUp || t.contentOpen {
		t.pendingCall = label
		return
	}
	if !t.callUp {
		t.callUp = true
		t.beginLocked(blockToolCall)
	}
	t.u.CallPreview(label)
}

// settleCall morphs the widget into its final collapsed header; the result
// lines that follow (toolLines) belong to the same block. If an async block
// (an MCP failure) interleaved since the raise, the block re-opens so the
// header doesn't glue to the stranger.
func (t *transcript) settleCall(header string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.callUp = false
	if t.last != blockToolCall {
		t.beginLocked(blockToolCall)
	}
	t.u.ClosePreview()
	t.pushLocked([]string{header})
}

// toolLines commits tool-result lines within the current tool-call block,
// re-opening it if an async block interleaved.
func (t *transcript) toolLines(lines ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last != blockToolCall {
		t.beginLocked(blockToolCall)
	}
	t.pushLocked(lines)
}

// noticeLines commits a pre-rendered multi-line result (e.g. /export output)
// as one grouped notice block.
func (t *transcript) noticeLines(lines ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last != blockNotice {
		t.beginLocked(blockNotice)
	}
	t.pushLocked(lines)
}

// openThinking raises the thinking widget ("⠋ Thinking" over "⎿ 1.2k tokens ·
// 8s · ESC to cancel") and returns the meter that feeds its token count.
func (t *transcript) openThinking() *thinkingMeter {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.callUp = false
	t.thinkingUp = true
	t.beginLocked(blockThinking)
	t.u.CallPreview(DimStyle.Sprint("Thinking"))
	return &thinkingMeter{t: t}
}

// settleThinking folds the widget into the dim "◇ thought for Ns" marker —
// the only trace reasoning leaves in the scrollback — re-opening the block
// first if an async block interleaved. A tool call announced during the
// thinking stream is raised here, in lifecycle order.
func (t *transcript) settleThinking(start time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.thinkingUp = false
	if t.last != blockThinking {
		t.beginLocked(blockThinking)
	}
	t.u.ClosePreview()
	t.pushLocked([]string{dim(fmt.Sprintf("%s thought for %s", reasoningSymbol, reasoningElapsed(start)))})
	t.raisePendingLocked()
}

// raisePendingLocked raises a tool call deferred while the thinking widget or
// a streaming content block owned the slot, in lifecycle order. A no-op while
// the other guard is still up (thinking settles before content opens, but an
// async close must not raise into a live thinking widget).
func (t *transcript) raisePendingLocked() {
	label := t.pendingCall
	if label == "" || t.thinkingUp || t.contentOpen {
		return
	}
	t.pendingCall = ""
	t.callUp = true
	t.beginLocked(blockToolCall)
	t.u.CallPreview(label)
}

// thinkingMeter counts streamed reasoning into the widget's status row:
// tokens estimated per delta (tiktoken; byte-split runes cost ±1 token),
// updates throttled so high-frequency deltas don't flood the program queue.
type thinkingMeter struct {
	t    *transcript
	n    int
	last time.Time
}

func (m *thinkingMeter) Write(p []byte) (int, error) {
	m.add(string(p))
	return len(p), nil
}

func (m *thinkingMeter) add(s string) {
	if s == "" {
		return
	}
	m.n += m.t.tokens.count(s)
	if time.Since(m.last) < 150*time.Millisecond {
		return
	}
	m.last = time.Now()
	m.t.u.CallDetail(formatTokensShort(m.n) + " tokens")
}

// formatTokensShort renders a token count the way the status line renders the
// context figures: k/m units, one decimal, trailing .0 trimmed.
func formatTokensShort(n int) string {
	switch {
	case n >= 1_000_000:
		return trimTokenZero(float64(n)/1e6) + "m"
	case n >= 1_000:
		return trimTokenZero(float64(n)/1e3) + "k"
	default:
		return fmt.Sprintf("%d", n)
	}
}

func trimTokenZero(f float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", f), ".0")
}

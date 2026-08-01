package chat

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"

	"chatchain/tool"
)

// The transcript is the session's single write surface for the chat area:
// every block that lands in the scrollback — user input, activity summaries,
// markdown content, notices, errors, resume echoes — is declared here, and
// the transcript alone decides the spacing between them. Two rules replace
// the ad-hoc "" commits of the past:
//
//   - every block opens with exactly one blank separator (above the first
//     block sits the pre-Program banner or the previous turn), consecutive
//     same-kind notices/errors grouping into one block;
//   - blank lines INSIDE a block are deferred until more content follows, so
//     no block can export trailing blanks for a neighbor to lean on (the
//     fragility behind the resume-echo regression).
//
// It also owns the ACTIVITY GROUP: the run of thinking segments and tool
// calls between two content blocks (or turn boundaries) shares one lifecycle
// widget — completed events scroll through its body — and settles into a
// single summary line ("◇ thought for 15s · ran 4 tools in 12s"). Degenerate
// groups keep the classic forms (a lone tool call = header + result lines,
// thinking only = the "◇ thought for Ns" marker), which is also exactly what
// verbose mode (/debug on) produces by settling after every event. Failed
// calls always break out as their own red rows — aggregation never swallows
// an error. Safe for concurrent use (the async MCP reporter interleaves with
// streaming turns).

// blockKind classifies the chat area's logical blocks.
type blockKind uint8

const (
	blockNone blockKind = iota
	blockUser
	blockActivity
	blockContent
	blockAsk
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
	CallLine(line string)
	ClosePreview()
	PauseClock()
	ResumeClock()
	Width() int
	Height() int
}

// activityGroup is the aggregation state for one activity group. The group
// owns the lifecycle widget for its whole lifetime; a content boundary (or
// resetTurn) settles it. A group that recorded no events yet (a composing
// call whose text spills) survives content — collapsing it would erase a
// call still in flight.
type activityGroup struct {
	up          bool          // widget raised (the group's separator is paid)
	thinkingUp  bool          // a thinking segment owns the widget label
	label       string        // current widget header (restored after a pause)
	paused      bool          // clock frozen while the user is consulted
	thinks      int           // settled thinking segments (a count — a segment can measure 0ns)
	thinkDur    time.Duration // Σ settled thinking segments
	thinkTokens int           // Σ reasoning tokens (meter estimate, live detail)
	tools       int
	fails       int
	toolsDur    time.Duration // Σ tool execution time (human waits excluded)
	firstHeader string        // the lone call's classic form, valid while tools == 1
	firstResult string
	firstErr    bool
	failLines   []string // red breakout rows appended under the summary
}

// hasEvents reports whether anything settled into the group — the guard
// between "a group worth summarizing" and "a raised widget still composing".
func (g *activityGroup) hasEvents() bool { return g.tools > 0 || g.thinks > 0 }

type transcript struct {
	mu     sync.Mutex
	u      transcriptSurface
	tokens *tokenCounter // estimator behind the thinking meter
	// verbose (nil = never) disables aggregation: the group settles after
	// every event, reproducing the classic per-call blocks for debugging.
	verbose     func() bool
	last        blockKind
	pending     int    // deferred block-interior blank lines
	contentOpen bool   // a markdown content block is streaming (renderer may hold buffered lines)
	pendingCall string // tool call announced while thinking/content streams; raised at close
	orphanSep   bool   // a dropped widget's separator awaits reuse by the next block
	grp         activityGroup
}

func newTranscript(u transcriptSurface, tokens *tokenCounter) *transcript {
	return &transcript{u: u, tokens: tokens}
}

func (t *transcript) verboseOn() bool { return t.verbose != nil && t.verbose() }

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

// resetTurn closes out widget bookkeeping at a turn boundary. A group that
// recorded events still settles — the activity happened, and an interrupted
// or errored turn deserves its partial summary (the widget itself is already
// gone: sink.Done dropped it, so the summary commits as plain lines). A
// widget dropped before any event settled (mid-compose interrupt) has paid a
// separator with nothing under it; mark it for reuse so the next block
// doesn't stack a second blank on top.
func (t *transcript) resetTurn() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.grp.hasEvents() {
		t.settleGroupLocked()
	} else if t.grp.up || t.grp.thinkingUp {
		t.orphanSep = true
	}
	t.grp = activityGroup{}
	t.contentOpen, t.pendingCall = false, ""
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

// errorBlock renders a structured error: a red "✗ headline" row over dim
// detail rows in the tool-result idiom ("  ⎿ " on the first, four-space
// indent after). Detail rows are pre-wrapped under the hanging indent —
// wrapping left to the region would restart continuation rows at column
// zero — sized so no produced row reaches the exact screen width (the region
// passes rows ≤ width through untouched, and sanitizeOverflow eats a column
// off exact-width rows). Groups with adjacent error blocks like error().
func (t *transcript) errorBlock(headline string, detail ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last != blockError {
		t.beginLocked(blockError)
	}
	wrapAt := t.u.Width() - 5
	if wrapAt < 20 {
		wrapAt = 20
	}
	lines := []string{ErrorStyle.Sprint("✗ " + headline)}
	indent := "  ⎿ "
	for _, d := range detail {
		for _, ln := range strings.Split(d, "\n") {
			if strings.TrimSpace(ln) == "" {
				continue
			}
			for _, row := range strings.Split(ansi.Wrap(strings.TrimRight(ln, "\r"), wrapAt, ""), "\n") {
				lines = append(lines, DimStyle.Sprint(indent+row))
				indent = "    "
			}
		}
	}
	t.pushLocked(lines)
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
// forever. Group counters deliberately survive — rounds accumulate into one
// group, and a turn-level retry re-running earlier rounds keeps counting the
// calls that really executed.
func (t *transcript) beginRound() {
	t.mu.Lock()
	t.grp.thinkingUp, t.contentOpen = false, false
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
// block opens lazily on the first line — settling the activity group first: a
// content boundary is what closes a group — and re-opens (new separator) if
// an async block (an MCP failure notice) interleaved since.
func (t *transcript) contentBlock() func(lines ...string) {
	opened := false
	return func(lines ...string) {
		t.mu.Lock()
		defer t.mu.Unlock()
		if !opened || t.last != blockContent {
			opened = true
			t.settleGroupLocked()
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
	switch {
	case t.grp.hasEvents():
		// Accumulated activity settles first; the image opens its own block.
		t.settleGroupLocked()
		t.beginLocked(blockImage)
	case t.grp.up:
		// An image-generation widget is up: the image IS its result — morph
		// the widget into the image block in place (separator already paid
		// at the raise; the region replaces the preview rows bottom-up).
		t.grp = activityGroup{}
		t.pending = 0
		t.last = blockImage
		t.u.ClosePreview()
	default:
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

// imageWidget ensures the image-generation widget ahead of progressive
// frames. Accumulated activity settles first — partial frames replace the
// widget body wholesale, so activity rows and refining thumbnails cannot
// share it. While thinking or content owns the slot the raise defers like
// any composing call.
func (t *transcript) imageWidget() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.grp.thinkingUp || t.contentOpen {
		if t.pendingCall == "" {
			t.pendingCall = "image"
		}
		return
	}
	if t.grp.hasEvents() {
		t.settleGroupLocked()
	}
	if !t.grp.up {
		t.ensureWidgetLocked("image")
	}
}

// ensureWidgetLocked raises the group's lifecycle widget (paying the group's
// one separator) or relabels it in place — the clock keeps running.
func (t *transcript) ensureWidgetLocked(label string) {
	if !t.grp.up {
		t.grp.up = true
		t.beginLocked(blockActivity)
	}
	t.grp.label = label
	t.u.CallPreview(label)
}

// openCall raises the tool-call lifecycle widget ("⠋ [name …]" over the live
// "⎿ elapsed" row) into the current group, or relabels it in place. While the
// thinking widget owns the slot (providers can stream tool-call deltas before
// the reasoning pipe closes) or content is streaming, the call is only
// remembered; settleThinking/closeContent raises it.
func (t *transcript) openCall(label string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.grp.thinkingUp || t.contentOpen {
		t.pendingCall = label
		return
	}
	t.ensureWidgetLocked(label)
}

// finishCall records a completed tool call into the group: counters, a body
// row scrolling through the widget, the failure breakout, and — while it is
// the group's only call — the material for the classic degenerate form. In
// verbose mode the group settles immediately, reproducing the classic block.
func (t *transcript) finishCall(header, result string, isError bool, dur time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.grp.up {
		// Defensive: a finish without a raise (never in practice).
		t.ensureWidgetLocked(header)
	}
	t.grp.tools++
	t.grp.toolsDur += dur
	if t.grp.tools == 1 {
		t.grp.firstHeader, t.grp.firstResult, t.grp.firstErr = header, result, isError
	}
	if isError {
		t.grp.fails++
		t.grp.failLines = append(t.grp.failLines, failLine(header, result))
	}
	if t.verboseOn() {
		t.settleGroupLocked()
		return
	}
	t.u.CallLine(eventLine(header, result, isError))
	t.ensureWidgetLocked(dim("Working…"))
	t.callDetailLocked()
}

// openShowcase raises an expanded call's standalone widget (PresentExpanded:
// file mutations showing their diff). An expanded call is a group boundary
// like content — the running group settles first, and whatever follows opens
// a fresh group.
func (t *transcript) openShowcase(header string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pendingCall = ""
	t.settleGroupLocked()
	t.ensureWidgetLocked(header)
}

// settleShowcase morphs the showcase widget into its expanded block: the
// header (with ±row counts) over the rendered diff — or the classic result
// form when the call posted no artifact (errors, declines, tools that had
// nothing to show).
func (t *transcript) settleShowcase(header string, art *tool.Artifact, result string, isError bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var lines []string
	if art != nil && art.Kind == "diff" && len(art.Lines) > 0 && !isError {
		adds, dels := 0, 0
		for _, ln := range art.Lines {
			switch {
			case strings.HasPrefix(ln, "+"):
				adds++
			case strings.HasPrefix(ln, "-"):
				dels++
			}
		}
		lines = append([]string{header + dim(fmt.Sprintf("  +%d -%d", adds, dels))},
			renderDiff(art.Title, art.Lines, t.diffBudgetLocked(), t.u.Width())...)
	} else {
		lines = []string{header}
		lc := &lineCommitter{commit: func(ls ...string) { lines = append(lines, ls...) }}
		printToolResult(lc, result, isError)
		lc.flush()
	}
	t.grp = activityGroup{}
	if t.last != blockActivity {
		t.beginLocked(blockActivity)
	}
	t.u.ClosePreview()
	t.pushLocked(lines)
}

// diffBudgetLocked is the dynamic diff row budget: the live screen height,
// floored at diffMinRows — a taller terminal earns a fuller diff.
func (t *transcript) diffBudgetLocked() int {
	if h := t.u.Height(); h > diffMinRows {
		return h
	}
	return diffMinRows
}

// diffMinRows floors the showcase diff budget on small terminals.
const diffMinRows = 24

// settleGroupLocked folds the group into its settled scrollback form and
// resets it. Groups without recorded events keep their widget (a composing
// call whose text spilled must not be collapsed); a group whose widget was
// already dropped (turn teardown) commits its summary as plain lines.
func (t *transcript) settleGroupLocked() {
	if !t.grp.hasEvents() {
		return
	}
	lines := t.groupLinesLocked()
	wasUp := t.grp.up
	t.grp = activityGroup{}
	if t.last != blockActivity {
		// An async block interleaved since the raise: re-open so the summary
		// doesn't glue to the stranger.
		t.beginLocked(blockActivity)
	}
	if wasUp {
		t.u.ClosePreview()
	}
	t.pushLocked(lines)
}

// groupLinesLocked renders the group's settled form:
//
//   - thinking only        → "◇ thought for 15s" (the classic marker)
//   - a lone untought call → the classic header + result lines
//   - anything else        → one summary line + red breakout rows per failure
func (t *transcript) groupLinesLocked() []string {
	g := &t.grp
	if g.tools == 0 {
		return []string{dim(fmt.Sprintf("%s thought for %s", reasoningSymbol, durElapsed(g.thinkDur)))}
	}
	if g.tools == 1 && g.thinks == 0 {
		lines := []string{g.firstHeader}
		lc := &lineCommitter{commit: func(ls ...string) { lines = append(lines, ls...) }}
		printToolResult(lc, g.firstResult, g.firstErr)
		lc.flush()
		return lines
	}
	ran := fmt.Sprintf("ran %d %s in %s", g.tools, pluralTools(g.tools), durElapsed(g.toolsDur))
	var line string
	if g.thinks > 0 {
		line = dim(fmt.Sprintf("%s thought for %s · %s", reasoningSymbol, durElapsed(g.thinkDur), ran))
	} else {
		line = dim(fmt.Sprintf("%s %s", reasoningSymbol, ran))
	}
	if g.fails > 0 {
		line += ErrorStyle.Sprintf(" · %d failed", g.fails)
	}
	return append([]string{line}, g.failLines...)
}

// eventLine is a completed call's body row inside the widget: a glyph, the
// header, and a snippet of the first result line.
func eventLine(header, result string, isError bool) string {
	glyph := DimStyle.Sprint("✓")
	if isError {
		glyph = ErrorStyle.Sprint("✗")
	}
	line := glyph + " " + header
	if first := firstResultLine(result); first != "" {
		line += dim(" · " + truncateRunes(first, 48))
	}
	return line
}

// failLine is a failed call's red breakout row under the group summary.
func failLine(header, result string) string {
	line := ErrorStyle.Sprint("✗") + " " + header
	if first := firstResultLine(result); first != "" {
		line += ErrorStyle.Sprint(" · " + truncateRunes(first, 64))
	}
	return line
}

// firstResultLine extracts the first non-blank line of a tool result.
func firstResultLine(result string) string {
	for _, ln := range strings.Split(result, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return ln
		}
	}
	return ""
}

func pluralTools(n int) string {
	if n == 1 {
		return "tool"
	}
	return "tools"
}

// durElapsed renders a duration the way the thinking marker always has:
// whole seconds, "<1s" below one.
func durElapsed(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// callDetailLocked refreshes the widget's live status-row prefix from the
// group's counters ("3 tools · 1.2k tokens").
func (t *transcript) callDetailLocked() {
	var parts []string
	if t.grp.tools > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", t.grp.tools, pluralTools(t.grp.tools)))
	}
	if t.grp.thinkTokens > 0 {
		parts = append(parts, formatTokensShort(t.grp.thinkTokens)+" tokens")
	}
	t.u.CallDetail(strings.Join(parts, " · "))
}

// askRecord commits an interactive tool's outcome — the user's own answers —
// as a "?" record block: the tool's surface has closed, and this is its
// scrollback trace. Interactive calls never enter the activity group.
func (t *transcript) askRecord(result string, isError bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.beginLocked(blockAsk)
	style := DimStyle
	if isError {
		style = ErrorStyle
	}
	result = strings.TrimRight(result, "\n")
	if strings.TrimSpace(result) == "" {
		result = "(no answer)"
	}
	var lines []string
	for i, ln := range strings.Split(result, "\n") {
		prefix := "  "
		if i == 0 {
			prefix = "? "
		}
		lines = append(lines, style.Sprint(prefix+ln))
	}
	t.pushLocked(lines)
}

// pauseForInput marks the turn as waiting on the user (an approval prompt, an
// interactive tool's surface): the widget relabels to the reason and the
// elapsed clock freezes — human deliberation must not inflate the group's
// timings. A no-op without a live widget (the surface itself is the display).
func (t *transcript) pauseForInput(reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.grp.up || t.grp.paused {
		return
	}
	t.grp.paused = true
	t.u.CallPreview(dim("⏸ " + reason))
	t.u.PauseClock()
}

// resumeFromInput restores the widget label and restarts the clock.
func (t *transcript) resumeFromInput() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.grp.paused {
		return
	}
	t.grp.paused = false
	t.u.ResumeClock()
	t.u.CallPreview(t.grp.label)
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

// openThinking raises the thinking segment into the group's widget ("⠋
// Thinking" over "⎿ 1.2k tokens · 8s · ESC to cancel") and returns the meter
// that feeds its token count.
func (t *transcript) openThinking() *thinkingMeter {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.grp.thinkingUp = true
	t.ensureWidgetLocked(dim("Thinking"))
	return &thinkingMeter{t: t, base: t.grp.thinkTokens}
}

// settleThinking completes the thinking segment: the group accumulates its
// duration and shows the "◇ thought Ns" body row; in verbose mode the group
// settles immediately, reproducing the classic marker. A tool call announced
// during the thinking stream is raised here, in lifecycle order.
func (t *transcript) settleThinking(start time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.grp.thinkingUp = false
	d := time.Since(start)
	t.grp.thinks++
	t.grp.thinkDur += d
	if t.verboseOn() {
		t.settleGroupLocked()
	} else {
		t.u.CallLine(dim(fmt.Sprintf("%s thought %s", reasoningSymbol, durElapsed(d))))
		if t.pendingCall == "" {
			t.ensureWidgetLocked(dim("Working…"))
		}
		t.callDetailLocked()
	}
	t.raisePendingLocked()
}

// raisePendingLocked raises a tool call deferred while the thinking widget or
// a streaming content block owned the slot, in lifecycle order. A no-op while
// the other guard is still up (thinking settles before content opens, but an
// async close must not raise into a live thinking widget).
func (t *transcript) raisePendingLocked() {
	label := t.pendingCall
	if label == "" || t.grp.thinkingUp || t.contentOpen {
		return
	}
	t.pendingCall = ""
	t.ensureWidgetLocked(label)
}

// thinkingMeter counts streamed reasoning into the widget's status row:
// tokens estimated per delta (tiktoken; byte-split runes cost ±1 token),
// updates throttled so high-frequency deltas don't flood the program queue.
// base carries the group's tokens from earlier segments, so the detail row
// keeps counting up across thinking rounds.
type thinkingMeter struct {
	t    *transcript
	base int
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
	m.t.mu.Lock()
	m.t.grp.thinkTokens = m.base + m.n
	m.t.callDetailLocked()
	m.t.mu.Unlock()
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

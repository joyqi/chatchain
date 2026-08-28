package chat

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/joyqi/iota/internal/tokfmt"
	"github.com/joyqi/iota/provider"
	"github.com/joyqi/iota/tool"

	"github.com/fatih/color"
)

// recSurface records the transcript's surface calls in order.
type recSurface struct{ events []string }

func (s *recSurface) PrintLines(lines ...string) {
	s.events = append(s.events, "print:"+strings.Join(lines, "|"))
}
func (s *recSurface) UserBlock(display string) { s.events = append(s.events, "user:"+display) }
func (s *recSurface) CallPreview(label string) { s.events = append(s.events, "call:"+label) }
func (s *recSurface) CallDetail(detail string) { s.events = append(s.events, "detail:"+detail) }
func (s *recSurface) CallLine(line string)     { s.events = append(s.events, "line:"+line) }
func (s *recSurface) ClosePreview()            { s.events = append(s.events, "settle") }
func (s *recSurface) PauseClock()              { s.events = append(s.events, "pause") }
func (s *recSurface) ResumeClock()             { s.events = append(s.events, "resume") }
func (s *recSurface) Width() int               { return 80 }
func (s *recSurface) Height() int              { return 30 }

func (s *recSurface) joined() string { return strings.Join(s.events, "\n") }

// classicResult renders a result through printToolResult the way the settle
// path does, for test expectations.
func classicResult(result string, isError bool) []string {
	var lines []string
	lc := &lineCommitter{commit: func(ls ...string) { lines = append(lines, ls...) }}
	printToolResult(lc, result, isError)
	lc.flush()
	return lines
}

var working = dim("Working…")

// The aggregation contract: thinking and consecutive tool calls share ONE
// widget (one separator, relabels in place, event rows through the body) and
// a content boundary settles them into a single summary line.
func TestActivityGroupAggregates(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, newTokenCounter())

	tr.user("hello")
	start := time.Now()
	m := tr.openThinking()
	m.add("some reasoning text")
	tr.settleThinking(start)
	tr.openCall("[a …]")
	tr.openCall("[a full]") // header expansion: same widget, no separator
	tr.finishCall("[a full]", "ok", false, 2*time.Second)
	tr.openCall("[b]")
	tr.finishCall("[b]", "out", false, 3*time.Second)
	content := tr.contentBlock()
	content("Done.")

	tokens := tokfmt.Tokens(tokenCount("some reasoning text")) + " tokens"
	want := []string{
		"user:hello",
		"print:", "call:" + DimStyle.Sprint("Thinking"),
		"detail:" + tokens,
		"line:" + dim("◇ thought <1s"),
		"call:" + working,
		"detail:" + tokens,
		"call:[a …]", "call:[a full]",
		"line:" + eventLine("[a full]", "ok", false, ""),
		"call:" + working,
		"detail:1 tool · " + tokens,
		"call:[b]",
		"line:" + eventLine("[b]", "out", false, ""),
		"call:" + working,
		"detail:2 tools · " + tokens,
		"settle",
		"print:" + dim("◇ thought for <1s · ran 2 tools in 5s"),
		"print:", "print:Done.",
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// tokenCount mirrors the meter's estimator for test expectations.
func tokenCount(s string) int { return newTokenCounter().count(s) }

// A lone tool call with no thinking keeps the classic block: header over the
// "⎿" result lines — aggregation must not degrade information density when
// there is nothing to aggregate.
func TestActivityGroupLoneToolClassic(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.user("x")
	tr.openCall("[read_file path:a]")
	tr.finishCall("[read_file path:a]", "line1\nline2", false, time.Second)
	content := tr.contentBlock()
	content("Answer.")

	classic := append([]string{"[read_file path:a]"}, classicResult("line1\nline2", false)...)
	want := []string{
		"user:x",
		"print:", "call:[read_file path:a]",
		"line:" + eventLine("[read_file path:a]", "line1\nline2", false, ""),
		"call:" + working,
		"detail:1 tool",
		"settle",
		"print:" + strings.Join(classic, "|"),
		"print:", "print:Answer.",
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// Thinking with no tool calls settles into the classic "◇ thought for Ns"
// marker at the content boundary.
func TestActivityGroupThinkingOnly(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.openThinking()
	tr.settleThinking(time.Now())
	content := tr.contentBlock()
	content("The reply.")

	want := []string{
		"call:" + DimStyle.Sprint("Thinking"), // first block: no separator
		"line:" + dim("◇ thought <1s"),
		"call:" + working,
		"detail:",
		"settle",
		"print:" + dim("◇ thought for <1s"),
		"print:", "print:The reply.",
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// Failed calls are never swallowed by aggregation: the summary carries a red
// failure count and each failed call breaks out as its own red row.
func TestActivityGroupFailBreakout(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.openCall("[a]")
	tr.finishCall("[a]", "fine", false, time.Second)
	tr.openCall("[bash cmd:x]")
	tr.finishCall("[bash cmd:x]", "exit 1\ndetail", true, time.Second)
	content := tr.contentBlock()
	content("So.")

	summary := dim("◇ ran 2 tools in 2s") + ErrorStyle.Sprintf(" · %d failed", 1)
	want := []string{
		"call:[a]",
		"line:" + eventLine("[a]", "fine", false, ""),
		"call:" + working,
		"detail:1 tool",
		"call:[bash cmd:x]",
		"line:" + eventLine("[bash cmd:x]", "exit 1\ndetail", true, ""),
		"call:" + working,
		"detail:2 tools",
		"settle",
		"print:" + summary + "|" + failLine("[bash cmd:x]", "exit 1\ndetail"),
		"print:", "print:So.",
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// Verbose mode (/debug on) settles the group after every event, reproducing
// the classic per-item blocks — the degenerate forms ARE the legacy shapes.
func TestVerboseSettlesPerEvent(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, newTokenCounter())
	tr.verbose = func() bool { return true }

	tr.user("hello")
	tr.openThinking()
	tr.settleThinking(time.Now())
	content := tr.contentBlock()
	content("The reply.")
	tr.openCall("[a …]")
	tr.openCall("[a full]")
	tr.finishCall("[a full]", "ok", false, time.Second)
	tr.openCall("[b]")
	tr.finishCall("[b]", "", false, time.Second)
	content2 := tr.contentBlock()
	content2("Done.")

	want := []string{
		"user:hello",
		"print:", "call:" + DimStyle.Sprint("Thinking"),
		"settle", "print:" + dim("◇ thought for <1s"),
		"print:", "print:The reply.",
		"print:", "call:[a …]", "call:[a full]",
		"settle", "print:" + strings.Join(append([]string{"[a full]"}, classicResult("ok", false)...), "|"),
		"print:", "call:[b]",
		"settle", "print:" + strings.Join(append([]string{"[b]"}, classicResult("", false)...), "|"),
		"print:", "print:Done.",
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// Interior blanks pass through once more content follows; trailing blanks are
// dropped — a block can never export them for a neighbor to lean on.
func TestTranscriptBlankLatch(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.echo([]string{"❯ hi", "", "line-1", "", "", "line-2", "", ""})
	tr.user("next")

	want := []string{
		"print:❯ hi||line-1|||line-2", // interior blanks intact, trailing dropped
		"print:", "user:next",
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// Consecutive notices (and errors) group into one block; a different kind in
// between starts fresh. Content re-opens after an async interleave.
func TestTranscriptGrouping(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.notice("AGENTS.md reloaded (%d files)", 2)
	tr.notice("Skills reloaded (%d skill(s))", 3)
	tr.error("⚠ MCP %s failed: %s", "srv", "boom")
	tr.notice("Context compacted")

	content := tr.contentBlock()
	content("streaming…")
	tr.error("⚠ MCP %s failed: %s", "other", "late") // async interleave
	content("more content")                          // same closure re-opens

	want := []string{
		"print:" + DimStyle.Sprint("AGENTS.md reloaded (2 files)"),
		"print:" + DimStyle.Sprint("Skills reloaded (3 skill(s))"),
		"print:", "print:" + ErrorStyle.Sprint("⚠ MCP srv failed: boom"),
		"print:", "print:" + DimStyle.Sprint("Context compacted"),
		"print:", "print:streaming…",
		"print:", "print:" + ErrorStyle.Sprint("⚠ MCP other failed: late"),
		"print:", "print:more content",
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// Providers can stream tool-call deltas while the reasoning pipe is still
// open (thinking → tool_use with no text): the observer's openCall must not
// hijack the thinking widget — the call is remembered and raised at settle,
// in lifecycle order, into the same group.
func TestThinkingComposingInterleave(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.user("do it")
	tr.openThinking()
	tr.openCall("[write_file …]") // observer fires mid-thought: queued
	tr.openCall("[bash …]")       // label change while queued: last wins
	tr.settleThinking(time.Now())
	tr.openCall("[bash cmd:ls]") // toolLoop expands the raised widget
	tr.finishCall("[bash cmd:ls]", "ok", false, time.Second)
	content := tr.contentBlock()
	content("Done.")

	classic := append([]string{"[bash cmd:ls]"}, classicResult("ok", false)...)
	_ = classic
	want := []string{
		"user:do it",
		"print:", "call:" + DimStyle.Sprint("Thinking"),
		"line:" + dim("◇ thought <1s"),
		"detail:",
		"call:[bash …]", // the pending call raised at settle, same widget
		"call:[bash cmd:ls]",
		"line:" + eventLine("[bash cmd:ls]", "ok", false, ""),
		"call:" + working,
		"detail:1 tool",
		"settle",
		"print:" + dim("◇ thought for <1s · ran 1 tool in 1s"),
		"print:", "print:Done.",
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// The markdown renderer buffers whole blocks (a trailing table flushes only
// when its stream ends), so the observer's openCall can arrive BEFORE the
// content's final lines commit. The call must defer until closeContent —
// otherwise the widget's separator lands first and the buffered content
// re-opens after it. The group settles at the content's first commit; the
// deferred call then opens the NEXT group.
func TestContentComposingInterleave(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.user("table then tool")
	tr.openThinking()
	tr.settleThinking(time.Now())
	content := tr.openContent()
	content("intro line")
	tr.openCall("[bash …]")       // observer fires; the table is still buffered
	content("| a | b |", "| 1 |") // renderer flush: same block, no re-open
	tr.openCall("[bash cmd:pwd]") // label refresh while deferred: last wins
	tr.closeContent()             // content over → the widget raises NOW

	want := []string{
		"user:table then tool",
		"print:", "call:" + DimStyle.Sprint("Thinking"),
		"line:" + dim("◇ thought <1s"),
		"call:" + working, // no pending call yet at settle time
		"detail:",
		"settle",
		"print:" + dim("◇ thought for <1s"), // the group settles at content
		"print:", "print:intro line",
		"print:| a | b ||| 1 |",         // committed into the SAME content block
		"print:", "call:[bash cmd:pwd]", // raised at closeContent: a new group
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// The race guard: markContent runs on the stream goroutine at the FIRST
// content byte, so an openCall arriving before the turn goroutine's
// openContent still defers — content-before-tools order survives a text
// delta and a tool delta landing in one network read.
func TestMarkContentBeatsObserver(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.beginRound()
	tr.markContent()        // stream goroutine: first content byte
	tr.openCall("[bash …]") // observer, before openContent ran: must defer
	content := tr.openContent()
	content("the text")
	tr.closeContent()

	want := []string{
		"print:the text", // first block: no separator
		"print:", "call:[bash …]",
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// A round that died mid-stream leaks its guards; beginRound clears them so
// the next round's widget is not silently deferred forever.
func TestBeginRoundClearsStaleGuards(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.markContent() // round 1 died after content started
	tr.beginRound()  // round 2 (retry) begins
	tr.openCall("[bash …]")

	want := []string{"call:[bash …]"}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// closeContent with no deferred call is a no-op; openCall after the close
// raises immediately (the tool-call-only round shape).
func TestCloseContentWithoutPendingCall(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	content := tr.openContent()
	content("reply")
	tr.closeContent()
	tr.openCall("[bash …]")

	want := []string{
		"print:reply",
		"print:", "call:[bash …]",
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// A widget dropped before any event settled (mid-compose interrupt) leaves
// its separator with nothing under it; the next block reuses that orphan
// instead of stacking a second blank.
func TestOrphanedWidgetSeparatorReuse(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.user("x")
	tr.openCall("[bash …]") // separator paid, widget raised
	tr.resetTurn()          // turn died; sink.Done dropped the widget
	tr.notice("Interrupted.")
	tr.user("next") // normal turn afterwards pays its own separator again

	want := []string{
		"user:x",
		"print:", "call:[bash …]",
		"print:" + DimStyle.Sprint("Interrupted."), // no extra separator
		"print:", "user:next",
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// A group with recorded events still settles when the turn dies: the
// activity happened, and the partial summary is its trace (the widget itself
// was already dropped by the sink; the summary commits as plain lines).
func TestResetTurnSettlesPartialGroup(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.user("x")
	tr.openCall("[a]")
	tr.finishCall("[a]", "ok", false, time.Second)
	tr.openCall("[b]")
	tr.finishCall("[b]", "ok", false, time.Second)
	tr.resetTurn() // interrupted before any content boundary
	tr.notice("Interrupted.")

	want := []string{
		"user:x",
		"print:", "call:[a]",
		"line:" + eventLine("[a]", "ok", false, ""),
		"call:" + working,
		"detail:1 tool",
		"call:[b]",
		"line:" + eventLine("[b]", "ok", false, ""),
		"call:" + working,
		"detail:2 tools",
		"settle",
		"print:" + dim("◇ ran 2 tools in 2s"),
		"print:", "print:" + DimStyle.Sprint("Interrupted."),
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// An async error interleaving between the raise and the settle forces the
// summary to re-open the block — it never glues to the stranger.
func TestSettleReopensAfterInterleave(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.openCall("[bash …]")
	tr.error("⚠ MCP srv failed: boom") // async reporter mid-execution
	tr.finishCall("[bash cmd:ls]", "ok", false, time.Second)
	content := tr.contentBlock()
	content("Done.")

	classic := append([]string{"[bash cmd:ls]"}, classicResult("ok", false)...)
	want := []string{
		"call:[bash …]",
		"print:", "print:" + ErrorStyle.Sprint("⚠ MCP srv failed: boom"),
		"line:" + eventLine("[bash cmd:ls]", "ok", false, ""),
		"call:" + working,
		"detail:1 tool",
		"print:", "settle",
		"print:" + strings.Join(classic, "|"),
		"print:", "print:Done.",
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// An interactive tool's outcome lands as the "?" record block — the user's
// own answers, outside any activity group.
func TestAskRecord(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.user("pick")
	tr.askRecord("Auth: JWT\nLib: chi", false)
	tr.askRecord("", false)

	want := []string{
		"user:pick",
		"print:", "print:" + DimStyle.Sprint("? Auth: JWT") + "|" + DimStyle.Sprint("  Lib: chi"),
		"print:", "print:" + DimStyle.Sprint("? (no answer)"),
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// pauseForInput relabels the live widget and freezes its clock; resume
// restores the group's label and continues. Without a widget both are
// no-ops — an interactive tool's surface is its own display.
func TestPauseForInput(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.pauseForInput("waiting for your input") // no widget: nothing
	tr.resumeFromInput()

	tr.openCall("[edit_file path:x]")
	tr.pauseForInput("waiting for approval")
	tr.resumeFromInput()

	want := []string{
		"call:[edit_file path:x]",
		"call:" + dim("⏸ waiting for approval"), "pause",
		"resume", "call:[edit_file path:x]",
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// A provider error carries its multi-line JSON body in ONE string; the
// transcript must expand embedded newlines so the blank latch works at line
// granularity and downstream row accounting (frame anchor, cursor) stays
// true — with trailing newlines latched away, not committed.
func TestTranscriptSplitsEmbeddedNewlines(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.error("Error: %s", "400 Bad Request {\n  \"message\": \"bad\",\n  \"code\": \"x\"\n}\n")
	tr.user("next")

	styled := ErrorStyle.Sprint("Error: 400 Bad Request {\n  \"message\": \"bad\",\n  \"code\": \"x\"\n}\n")
	rows := strings.Split(strings.TrimSuffix(styled, "\n"), "\n")
	want := []string{
		"print:" + strings.Join(rows, "|"),
		"print:", "user:next",
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%q\n\nwant:\n%q", got, strings.Join(want, "\n"))
	}
}

// fatih/color's Fprintf places the SGR reset AFTER a trailing newline in the
// format string, so a styled "…\n" write ends with a reset-only line; the
// committer must glue it back so no spurious blank row reaches the transcript
// (seen live as a double blank between a tool result and the next thinking
// marker).
func TestLineCommitterGluesTrailingReset(t *testing.T) {
	var got []string
	lc := &lineCommitter{commit: func(lines ...string) { got = append(got, lines...) }}
	lc.Write([]byte("\x1b[2m  ⎿ /Users/joyqi\n\x1b[0m")) // color.Fprintf's exact shape
	lc.flush()

	want := []string{"\x1b[2m  ⎿ /Users/joyqi\x1b[0m"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("committed %q, want %q", got, want)
	}
}

// fakeObserverTP is a ToolProvider whose only job is handing back the
// observer callback.
type fakeObserverTP struct{ fn func(name, delta string) }

func (f *fakeObserverTP) SetToolCallObserver(fn func(name, delta string)) { f.fn = fn }
func (f *fakeObserverTP) StreamChatWithTools(context.Context, []provider.Message, []provider.ToolDef, io.Writer, io.WriteCloser) (string, string, []provider.ToolCall, error) {
	return "", "", nil, nil
}

// While arguments stream, only the lifecycle widget is raised — once per
// label change, and only for NAMED, non-interactive calls: an anonymous
// delta cannot be classified yet, and an interactive tool's widget would sit
// zombie behind its own surface. Atomic (empty-delta) notifications are
// ignored; cleanup detaches the observer without settling the widget.
func TestWatchToolComposing(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)
	tp := &fakeObserverTP{}
	first := 0
	cleanup := watchToolComposing(newTurnPhases(nil), tr, tp,
		func(name string) bool { return name == "choose" },
		func() { first++ })
	if tp.fn == nil {
		t.Fatal("observer not installed")
	}

	tp.fn("", `{"pa`)          // name unknown yet: no raise, but onFirst fires
	tp.fn("write_file", `th"`) // name arrived → raise via CallPreview
	tp.fn("write_file", `:"a`) // same label → no event
	tp.fn("choose", `{"q`)     // interactive → suppressed
	tp.fn("bash", `{"co`)      // still the same open widget: relabel only
	tp.fn("", "")              // atomic backends: ignored
	cleanup()

	want := []string{
		"call:" + composingLabel("write_file"), // first block: the environment owns the boundary blank
		"call:" + composingLabel("bash"),
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	if first != 1 {
		t.Fatalf("onFirst fired %d times, want once", first)
	}
	if tp.fn != nil {
		t.Fatal("cleanup should detach the observer")
	}
}

// An image block: half-block rows plus a dim caption in ONE block, one
// separator like every other block.
func TestTranscriptImageBlock(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	content := tr.openContent()
	content("Here you go.")
	tr.closeContent()
	tr.image([]string{"ROW1", "ROW2"}, "🖼 saved: /x/y.png")

	want := []string{
		"print:Here you go.",
		"print:", "print:  ROW1|  ROW2", // uniform two-space indent
		"print:  " + DimStyle.Sprint("🖼 saved: /x/y.png"),
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// An image-generation widget (raised via the composing observer) morphs INTO
// the image block: the separator was paid at the raise, so the image pays no
// second one; without a widget the block opens normally.
func TestImageMorphsGenerationWidget(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.user("draw")
	tr.openCall("[image_generation …]")
	tr.image([]string{"ROW"}, "🖼 saved: /p.png")

	want := []string{
		"user:draw",
		"print:", "call:[image_generation …]",
		"settle", "print:  ROW",
		"print:  " + DimStyle.Sprint("🖼 saved: /p.png"),
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// Progressive frames arriving over a group with recorded activity settle the
// group FIRST — refining thumbnails replace the widget body wholesale, and
// the image then morphs a fresh, dedicated widget.
func TestImageWidgetSettlesActivityFirst(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.user("draw with tools")
	tr.openCall("[a]")
	tr.finishCall("[a]", "ok", false, time.Second)
	tr.imageWidget() // first partial frame arrives
	tr.image([]string{"ROW"}, "🖼 saved: /p.png")

	classic := append([]string{"[a]"}, classicResult("ok", false)...)
	want := []string{
		"user:draw with tools",
		"print:", "call:[a]",
		"line:" + eventLine("[a]", "ok", false, ""),
		"call:" + working,
		"detail:1 tool",
		"settle", "print:" + strings.Join(classic, "|"), // the lone call settles classic
		"print:", "call:image", // a fresh widget hosts the frames
		"settle", "print:  ROW", // …and the image morphs it
		"print:  " + DimStyle.Sprint("🖼 saved: /p.png"),
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// An expanded call (PresentExpanded) is a group boundary: the running group
// settles first, the showcase takes its own widget, and the result expands
// into the colored diff block under a ±count header. Whatever follows opens
// a fresh group.
func TestShowcaseSettlesGroupAndExpandsDiff(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.user("edit it")
	tr.openCall("[read_file path:a]")
	tr.finishCall("[read_file path:a]", "ok", false, time.Second)
	art := &tool.Artifact{Kind: "diff", Title: "a",
		Lines: []string{"@@ -1,3 +1,3 @@", " one", "-two", "+2", " three"}}
	tr.openShowcase("[edit_file path:a]")
	tr.settleShowcase("[edit_file path:a]", art, "1 replacement(s) in a", false)
	tr.openCall("[read_file path:b]")

	classic := append([]string{"[read_file path:a]"}, classicResult("ok", false)...)
	diffBlock := append([]string{"[edit_file path:a]" + dim("  +1 -1")},
		renderDiff("a", art.Lines, 30, 80)...)
	want := []string{
		"user:edit it",
		"print:", "call:[read_file path:a]",
		"line:" + eventLine("[read_file path:a]", "ok", false, ""),
		"call:" + working,
		"detail:1 tool",
		"settle", "print:" + strings.Join(classic, "|"), // the group settles first
		"print:", "call:[edit_file path:a]", // showcase widget: own block
		"settle", "print:" + strings.Join(diffBlock, "|"),
		"print:", "call:[read_file path:b]", // a fresh group follows
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// The diff budget follows the live screen height (floored at diffMinRows):
// rows beyond it collapse into the "… +N more lines" tail.
func TestShowcaseDiffBudget(t *testing.T) {
	lines := make([]string, 0, 41)
	lines = append(lines, "@@ -0,0 +1,40 @@")
	for i := 0; i < 40; i++ {
		lines = append(lines, fmt.Sprintf("+row-%d", i))
	}
	s := &recSurface{} // Height() = 30 → budget 30: 29 rows + tail
	tr := newTranscript(s, nil)
	tr.openShowcase("[write_file path:big]")
	tr.settleShowcase("[write_file path:big]", &tool.Artifact{Kind: "diff", Lines: lines}, "ok", false)

	out := s.joined()
	if !strings.Contains(out, "… +11 more lines") {
		t.Fatalf("missing truncation tail:\n%s", out)
	}
	if !strings.Contains(out, "row-28") || strings.Contains(out, "row-29") {
		t.Fatalf("budget must cut after row 28:\n%s", out)
	}
	if strings.Contains(out, "@@") {
		t.Fatalf("hunk headers must translate into line numbers, not render:\n%s", out)
	}
}

// A showcase without an artifact (declines, errors, tools with nothing to
// show) falls back to the classic header + result form.
func TestShowcaseFallsBackClassic(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)
	tr.openShowcase("[edit_file path:x]")
	tr.settleShowcase("[edit_file path:x]", nil, "The user declined this call.", true)

	classic := append([]string{"[edit_file path:x]"}, classicResult("The user declined this call.", true)...)
	want := []string{
		"call:[edit_file path:x]",
		"settle", "print:" + strings.Join(classic, "|"),
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// Overwide diff rows TRUNCATE to the screen width — wrapping would wreck the
// column alignment diffs live by.
func TestRenderDiffTruncatesWideRows(t *testing.T) {
	rows := renderDiff("", []string{"+" + strings.Repeat("x", 200)}, 24, 40)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if w := displayWidth(stripANSI(rows[0])); w > 39 {
		t.Fatalf("row width = %d, must stay under the screen width", w)
	}
}

// Hunk headers translate into the line-number gutter: additions and context
// carry new-file numbers, deletions old-file numbers, and a "⋮" row marks
// the boundary between hunks.
func TestParseDiffRows(t *testing.T) {
	rows := parseDiffRows([]string{
		"@@ -3,2 +3,3 @@",
		" ctx",
		"-gone",
		"+new-a",
		"+new-b",
		"@@ -20,1 +21,1 @@",
		"-old",
		"+fresh",
		"\\ No newline at end of file",
	})
	type want struct {
		kind rune
		num  int
		gap  bool
	}
	wants := []want{
		{' ', 3, false},
		{'-', 4, false},
		{'+', 4, false},
		{'+', 5, false},
		{0, 0, true}, // hunk boundary
		{'-', 20, false},
		{'+', 21, false},
	}
	if len(rows) != len(wants) {
		t.Fatalf("rows = %d, want %d: %+v", len(rows), len(wants), rows)
	}
	for i, w := range wants {
		if rows[i].gap != w.gap || (!w.gap && (rows[i].kind != w.kind || rows[i].num != w.num)) {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], w)
		}
	}
}

// With color on, ± rows carry their background blocks (re-armed after every
// chroma reset so token styling can't cut the block short), the gutter is
// dim, and a fresh-file "-0,0" hunk numbers from 1.
func TestRenderDiffBackgrounds(t *testing.T) {
	old := color.NoColor
	color.NoColor = false
	defer func() { color.NoColor = old }()

	rows := renderDiff("main.go", []string{
		"@@ -0,0 +1,2 @@",
		"+package main",
		"+var x = 1",
	}, 24, 100)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2:\n%q", len(rows), rows)
	}
	bgAdd, _, _, _ := diffShades()
	for i, row := range rows {
		if !strings.Contains(row, bgAdd) {
			t.Errorf("row %d missing the addition background: %q", i, row)
		}
		if !strings.HasSuffix(row, "\x1b[0m") {
			t.Errorf("row %d must end SGR-self-contained: %q", i, row)
		}
	}
	if !strings.Contains(stripANSI(rows[0]), "1 + package main") {
		t.Errorf("gutter numbering wrong: %q", stripANSI(rows[0]))
	}
	if !strings.Contains(stripANSI(rows[1]), "2 + var x = 1") {
		t.Errorf("gutter numbering wrong: %q", stripANSI(rows[1]))
	}
	// chroma emits per-token resets on go code; every reset must re-arm the bg.
	if inner := strings.TrimSuffix(rows[1], "\x1b[0m"); strings.Contains(inner, "\x1b[0m") &&
		!strings.Contains(inner, "\x1b[0m"+bgAdd) {
		t.Errorf("token reset not re-armed with the background: %q", rows[1])
	}
}

// A mid-turn injected user message (steering) is a stronger boundary than
// content: the running group settles first, the ❯ block lands, and the next
// round's activity opens a fresh group.
func TestUserSettlesOpenGroup(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.openCall("[a]")
	tr.finishCall("[a]", "ok", false, time.Second)
	tr.user("also check the tests") // steering injection at the round boundary
	tr.openCall("[b]")

	classic := append([]string{"[a]"}, classicResult("ok", false)...)
	want := []string{
		"call:[a]",
		"line:" + eventLine("[a]", "ok", false, ""),
		"call:" + working,
		"detail:1 tool",
		"settle", "print:" + strings.Join(classic, "|"),
		"print:", "user:also check the tests",
		"print:", "call:[b]", // fresh group, own separator
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// A diff row whose content the lexer cannot parse (CJK prose in a template
// literal) must carry ONLY the block's own background — chroma Error tokens
// are neutralized upstream, never splashing alarm red through the block.
func TestRenderDiffNoAlienBackgrounds(t *testing.T) {
	old := color.NoColor
	color.NoColor = false
	defer func() { color.NoColor = old }()

	rows := renderDiff("prompt.js", []string{
		"@@ -1,1 +1,1 @@",
		"+  重要: 这是发给**开发者**的推荐语（clarity、naturalness）",
	}, 24, 200)
	bgAdd, _, _, _ := diffShades()
	stripped := strings.ReplaceAll(rows[0], bgAdd, "")
	if strings.Contains(stripped, "\x1b[48;") {
		t.Fatalf("alien background survived in diff row:\n%q", rows[0])
	}
}

// The ± block covers the line-number gutter: the row starts with the block
// background right after the indent, and the number + marker wear the row's
// accent color before the code's own chroma foregrounds take over.
func TestRenderDiffGutterInsideBlock(t *testing.T) {
	old := color.NoColor
	color.NoColor = false
	defer func() { color.NoColor = old }()

	rows := renderDiff("main.go", []string{
		"@@ -1,1 +1,2 @@",
		"+package main",
		"-package old",
	}, 24, 100)
	bgAdd, bgDel, fgAdd, fgDel := diffShades()
	if !strings.HasPrefix(rows[0], "  "+bgAdd+fgAdd) {
		t.Errorf("add row must open with block bg + accent fg over the gutter:\n%q", rows[0])
	}
	if !strings.HasPrefix(rows[1], "  "+bgDel+fgDel) {
		t.Errorf("del row must open with block bg + accent fg over the gutter:\n%q", rows[1])
	}
	// The accent yields to the code's own foregrounds after the marker.
	if !strings.Contains(rows[0], "\x1b[39m") {
		t.Errorf("accent fg must reset before the code:\n%q", rows[0])
	}
	if !strings.Contains(stripANSI(rows[0]), "1 + package main") {
		t.Errorf("gutter layout changed: %q", stripANSI(rows[0]))
	}
}

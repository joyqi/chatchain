package chat

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"chatchain/provider"
)

// recSurface records the transcript's surface calls in order.
type recSurface struct{ events []string }

func (s *recSurface) PrintLines(lines ...string) {
	s.events = append(s.events, "print:"+strings.Join(lines, "|"))
}
func (s *recSurface) UserBlock(display string) { s.events = append(s.events, "user:"+display) }
func (s *recSurface) CallPreview(label string) { s.events = append(s.events, "call:"+label) }
func (s *recSurface) CallDetail(detail string) { s.events = append(s.events, "detail:"+detail) }
func (s *recSurface) ClosePreview()            { s.events = append(s.events, "settle") }

func (s *recSurface) joined() string { return strings.Join(s.events, "\n") }

// The spacing contract: every block opens with exactly one blank separator;
// the widget pays it when first raised (header expansion is free, results
// join the widget's block); a settled widget re-raised is a new block.
func TestTranscriptSpacing(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, newTokenCounter())

	tr.user("hello")
	start := time.Now()
	m := tr.openThinking()
	m.add("some reasoning text")
	tr.settleThinking(start)
	content := tr.contentBlock()
	content("The reply.")
	tr.openCall("[a …]")
	tr.openCall("[a full]") // same widget: no extra separator
	tr.settleCall("[a full]")
	tr.toolLines("  ⎿ ok")
	tr.openCall("[b …]") // settled → new block, one separator
	tr.settleCall("[b]")
	content2 := tr.contentBlock()
	content2("Done.")

	want := []string{
		"user:hello",
		"print:", "call:" + DimStyle.Sprint("Thinking"),
		"detail:" + formatTokensShort(tokenCount("some reasoning text")) + " tokens",
		"settle", "print:" + dim("◇ thought for <1s"),
		"print:", "print:The reply.",
		"print:", "call:[a …]", "call:[a full]",
		"settle", "print:[a full]", "print:  ⎿ ok",
		"print:", "call:[b …]",
		"settle", "print:[b]",
		"print:", "print:Done.",
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// tokenCount mirrors the meter's estimator for test expectations.
func tokenCount(s string) int { return newTokenCounter().count(s) }

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
// in lifecycle order, with its own separator and a fresh clock.
func TestThinkingComposingInterleave(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.user("do it")
	tr.openThinking()
	tr.openCall("[write_file …]") // observer fires mid-thought: queued
	tr.openCall("[bash …]")       // label change while queued: last wins
	tr.settleThinking(time.Now())
	tr.openCall("[bash cmd:ls]") // toolLoop expands the raised widget
	tr.settleCall("[bash cmd:ls]")
	tr.toolLines("  ⎿ ok")

	want := []string{
		"user:do it",
		"print:", "call:" + DimStyle.Sprint("Thinking"),
		"settle", "print:" + dim("◇ thought for <1s"),
		"print:", "call:[bash …]", // the pending call raised at settle
		"call:[bash cmd:ls]", // expansion: no separator
		"settle", "print:[bash cmd:ls]", "print:  ⎿ ok",
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// The markdown renderer buffers whole blocks (a trailing table flushes only
// when its stream ends), so the observer's openCall can arrive BEFORE the
// content's final lines commit. The call must defer until closeContent —
// otherwise the widget's separator lands first and the buffered content
// re-opens after it: a double blank above the content and the widget glued
// below it (the live-reported shape).
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
	tr.settleCall("[bash cmd:pwd]")
	tr.toolLines("  ⎿ /root")

	want := []string{
		"user:table then tool",
		"print:", "call:" + DimStyle.Sprint("Thinking"),
		"settle", "print:" + dim("◇ thought for <1s"),
		"print:", "print:intro line",
		"print:| a | b ||| 1 |",         // committed into the SAME content block
		"print:", "call:[bash cmd:pwd]", // raised at closeContent, one separator
		"settle", "print:[bash cmd:pwd]", "print:  ⎿ /root",
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

// A widget dropped unsettled (interrupted turn) leaves its separator with
// nothing under it; the next block reuses that orphan instead of stacking a
// second blank.
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

// An async error interleaving between a widget's raise and its settle forces
// the settle (and result lines) to re-open the block — the header never glues
// to the stranger.
func TestSettleReopensAfterInterleave(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)

	tr.openCall("[bash …]")
	tr.error("⚠ MCP srv failed: boom") // async reporter mid-execution
	tr.settleCall("[bash cmd:ls]")
	tr.toolLines("  ⎿ ok")

	want := []string{
		"call:[bash …]",
		"print:", "print:" + ErrorStyle.Sprint("⚠ MCP srv failed: boom"),
		"print:", "settle", "print:[bash cmd:ls]", "print:  ⎿ ok",
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

func TestFormatTokensShort(t *testing.T) {
	for n, want := range map[int]string{
		0:         "0",
		842:       "842",
		1000:      "1k",
		1234:      "1.2k",
		56400:     "56.4k",
		1_100_000: "1.1m",
	} {
		if got := formatTokensShort(n); got != want {
			t.Errorf("formatTokensShort(%d) = %q, want %q", n, got, want)
		}
	}
}

// fakeObserverTP is a ToolProvider whose only job is handing back the
// observer callback.
type fakeObserverTP struct{ fn func(name, delta string) }

func (f *fakeObserverTP) SetToolCallObserver(fn func(name, delta string)) { f.fn = fn }
func (f *fakeObserverTP) StreamChatWithTools(context.Context, []provider.Message, []provider.ToolDef, io.Writer, io.WriteCloser) (string, string, []provider.ToolCall, error) {
	return "", "", nil, nil
}

// While arguments stream, only the lifecycle widget is raised — separator on
// the first raise, once per label change, no argument text staged; atomic
// (empty-delta) notifications are ignored; cleanup detaches the observer
// without settling the widget (toolLoop owns that).
func TestWatchToolComposing(t *testing.T) {
	s := &recSurface{}
	tr := newTranscript(s, nil)
	tp := &fakeObserverTP{}
	cleanup := watchToolComposing(newTurnPhases(nil), tr, tp, nil)
	if tp.fn == nil {
		t.Fatal("observer not installed")
	}

	tp.fn("", `{"pa`)          // name unknown yet
	tp.fn("write_file", `th"`) // name arrived → relabel via CallPreview
	tp.fn("write_file", `:"a`) // same label → no event
	tp.fn("bash", `{"co`)      // still the same open widget: relabel only
	tp.fn("", "")              // atomic backends: ignored
	cleanup()

	want := []string{
		"call:" + composingLabel(""), // first block: the environment owns the boundary blank
		"call:" + composingLabel("write_file"),
		"call:" + composingLabel("bash"),
	}
	if got := s.joined(); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
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

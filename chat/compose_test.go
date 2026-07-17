package chat

import (
	"context"
	"io"
	"strings"
	"testing"

	"chatchain/provider"
)

// composeSink records the call-preview lifecycle.
type composeSink struct{ events []string }

func (s *composeSink) CommitLines(lines ...string) {
	s.events = append(s.events, "commit:"+strings.Join(lines, "|"))
}
func (s *composeSink) Done() {}
func (s *composeSink) BlockPreview(label string) io.WriteCloser {
	s.events = append(s.events, "open:"+label)
	return nil
}
func (s *composeSink) CallPreview(label string) {
	s.events = append(s.events, "call:"+label)
}
func (s *composeSink) ClosePreview() {
	s.events = append(s.events, "settle")
}

// fakeObserverTP is a ToolProvider whose only job is handing back the
// observer callback.
type fakeObserverTP struct{ fn func(name, delta string) }

func (f *fakeObserverTP) SetToolCallObserver(fn func(name, delta string)) { f.fn = fn }
func (f *fakeObserverTP) StreamChatWithTools(context.Context, []provider.Message, []provider.ToolDef, io.Writer, io.WriteCloser) (string, string, []provider.ToolCall, error) {
	return "", "", nil, nil
}

// While arguments stream, only the lifecycle widget is raised — once per
// label change, no argument text staged; the block separator lands on the
// first raise (staging spacing = settled spacing); atomic (empty-delta)
// notifications are ignored, and cleanup detaches the observer without
// settling the widget (toolLoop owns that).
func TestWatchToolComposing(t *testing.T) {
	sink := &composeSink{}
	tp := &fakeObserverTP{}
	cleanup := watchToolComposing(newTurnPhases(nil), newTurnRender(sink), tp)
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
		"commit:", // the block separator, paid once at the first raise
		"call:" + composingLabel(""),
		"call:" + composingLabel("write_file"),
		"call:" + composingLabel("bash"),
	}
	if got := strings.Join(sink.events, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	if tp.fn != nil {
		t.Fatal("cleanup should detach the observer")
	}
}

// turnRender pins the spacing contract: one separator before every block —
// the tool-call widget pays it when first raised (not at settle), header
// expansion is free, and the result lines join the widget's block.
func TestTurnRenderSpacing(t *testing.T) {
	sink := &composeSink{}
	r := newTurnRender(sink)

	r.openBlock()
	sink.CommitLines("◇ thought for 3s")
	r.openCall("[a …]")
	r.openCall("[a full]") // same open widget: no extra separator
	r.settleCall("[a full]")
	sink.CommitLines("  ⎿ ok")
	r.openCall("[b …]") // widget settled → a new block, one separator
	r.settleCall("[b]")
	r.openBlock() // e.g. the final text reply
	sink.CommitLines("done")

	want := []string{
		"commit:", "commit:◇ thought for 3s",
		"commit:", "call:[a …]", "call:[a full]",
		"settle", "commit:[a full]", "commit:  ⎿ ok",
		"commit:", "call:[b …]",
		"settle", "commit:[b]",
		"commit:", "commit:done",
	}
	if got := strings.Join(sink.events, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

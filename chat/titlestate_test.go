package chat

import (
	"testing"

	"chatchain/provider"
)

// titleProbe wires a sessionTitle to an in-memory writer and records what the
// window sink is told, so a test can assert both halves of every move.
type titleProbe struct {
	sw     *SessionWriter
	window []string
	t      *sessionTitle
}

func newTitleProbe(resumed bool) *titleProbe {
	p := &titleProbe{sw: &SessionWriter{}}
	p.t = newSessionTitle(func() *SessionWriter { return p.sw },
		func(s string) { p.window = append(p.window, s) }, resumed)
	return p
}

func (p *titleProbe) name() string { return p.sw.meta.Title }

func (p *titleProbe) lastWindow() string {
	if len(p.window) == 0 {
		return ""
	}
	return p.window[len(p.window)-1]
}

func userTurn(text string) []provider.Message {
	return []provider.Message{{Role: "user", Content: text}}
}

// TestTitleSeedsBeforeTheReply is the whole point of naming at send time: a
// first turn that spends minutes in tool calls must not leave the session
// poorly named while it works. The placeholder comes from the user's message
// alone, and the same call releases that message for the async model pass —
// nothing here ever waits for the assistant.
func TestTitleSeedsBeforeTheReply(t *testing.T) {
	p := newTitleProbe(false)
	fu, _, ok := p.t.seed(userTurn("refactor the session writer"))

	if !ok || fu != "refactor the session writer" {
		t.Errorf("seed = (%q, %v), want the message released for the model pass", fu, ok)
	}
	if got := p.name(); got != "refactor the session writer" {
		t.Errorf("session title = %q, want the prompt-derived placeholder", got)
	}
	if got := p.lastWindow(); got != "refactor the session writer" {
		t.Errorf("window title = %q, want the placeholder", got)
	}
}

// TestTitleSeedOnlyOnce: later messages never rename a session, and the model
// pass fires only for the seed that newly named it.
func TestTitleSeedOnlyOnce(t *testing.T) {
	p := newTitleProbe(false)
	history := userTurn("first question")
	p.t.seed(history)
	history = append(history,
		provider.Message{Role: "assistant", Content: "sure"},
		provider.Message{Role: "user", Content: "second question"})

	if _, _, ok := p.t.seed(history); ok {
		t.Error("a second seed released another model pass")
	}
	if got := p.name(); got != "first question" {
		t.Errorf("title = %q, want the FIRST message's placeholder", got)
	}
}

// TestTitleLandUpgradesThePlaceholder: the async pass's answer replaces the
// placeholder on both sinks.
func TestTitleLandUpgradesThePlaceholder(t *testing.T) {
	p := newTitleProbe(false)
	_, gen, _ := p.t.seed(userTurn("how do I profile Go allocations"))
	p.t.land(gen, "Profiling Go allocations")

	if got := p.name(); got != "Profiling Go allocations" {
		t.Errorf("title = %q, want the model-written one", got)
	}
	if got := p.lastWindow(); got != "Profiling Go allocations" {
		t.Errorf("window title = %q, want the model-written one", got)
	}
}

// TestTitleLandEmptyKeepsThePlaceholder: a failed pass changes nothing, and
// there is no retry — the placeholder is a complete fallback on its own.
func TestTitleLandEmptyKeepsThePlaceholder(t *testing.T) {
	p := newTitleProbe(false)
	_, gen, _ := p.t.seed(userTurn("a question"))
	p.t.land(gen, "")

	if got := p.name(); got != "a question" {
		t.Errorf("title = %q, want the placeholder kept", got)
	}
}

// TestTitleResumedSessionUntouched: a resumed bundle arrives named; neither
// the placeholder nor a model pass may take that away.
func TestTitleResumedSessionUntouched(t *testing.T) {
	p := newTitleProbe(true)
	p.sw.meta.Title = "an earlier chat"

	if _, _, ok := p.t.seed(userTurn("new question")); ok {
		t.Error("a resumed session released a model pass")
	}
	if got := p.name(); got != "an earlier chat" {
		t.Errorf("title = %q, want the resumed one", got)
	}
	if len(p.window) != 0 {
		t.Errorf("window sink written %v, want untouched", p.window)
	}
}

// TestTitleUnseedOnRollback: a failed or discarded turn takes its user message
// out of the history, and the name was derived from nothing else — so it has
// to go back too, and the pass that was racing the turn must not land late.
func TestTitleUnseedOnRollback(t *testing.T) {
	p := newTitleProbe(false)
	history := userTurn("a question that errored")
	_, gen, _ := p.t.seed(history)
	if p.name() == "" {
		t.Fatal("placeholder not seeded")
	}

	history = history[:0] // the turn rolled back
	p.t.unseed(history)

	if got := p.name(); got != "" {
		t.Errorf("title = %q, want it given back", got)
	}
	if got := p.lastWindow(); got != "" {
		t.Errorf("window title = %q, want it cleared", got)
	}
	// The pass fired for the rolled-back message may still be in flight; its
	// answer names a message the session no longer contains.
	p.t.land(gen, "A Question That Errored")
	if got := p.name(); got != "" {
		t.Errorf("a stale pass landed after the rollback: %q", got)
	}
	// And the NEXT message gets to supply both placeholder and pass.
	fu, gen2, ok := p.t.seed(userTurn("what actually worked"))
	if !ok || fu != "what actually worked" {
		t.Fatalf("re-seed = (%q, %v), want a fresh pass released", fu, ok)
	}
	p.t.land(gen2, "What actually worked")
	if got := p.name(); got != "What actually worked" {
		t.Errorf("title = %q, want the fresh pass landed", got)
	}
}

// TestTitleRollbackRevertsALandedTitle: on a fast pass the model title can
// arrive before the turn fails. The rollback still takes it with it — the
// text it summarized is gone either way.
func TestTitleRollbackRevertsALandedTitle(t *testing.T) {
	p := newTitleProbe(false)
	_, gen, _ := p.t.seed(userTurn("a question"))
	p.t.land(gen, "A Model Title")

	p.t.unseed(nil)

	if got := p.name(); got != "" {
		t.Errorf("title = %q, want the landed title given back too", got)
	}
}

// TestTitleUnseedKeepsASurvivingTurn: an interrupt that kept partial output
// leaves the user message in place, so the name stays.
func TestTitleUnseedKeepsASurvivingTurn(t *testing.T) {
	p := newTitleProbe(false)
	history := userTurn("a question")
	p.t.seed(history)
	history = append(history, provider.Message{Role: "assistant", Content: "partial…"})
	p.t.unseed(history)

	if got := p.name(); got != "a question" {
		t.Errorf("title = %q, want it kept — the message is still there", got)
	}
}

// TestTitleAdoptNameWins: an explicit /save title outranks the model pass —
// one already in flight is dropped on landing — and no rollback strips it.
func TestTitleAdoptNameWins(t *testing.T) {
	p := newTitleProbe(false)
	_, gen, _ := p.t.seed(userTurn("a question"))
	p.t.adoptName("my chosen name")

	p.t.land(gen, "Model Title")
	if got := p.name(); got != "my chosen name" {
		t.Errorf("title = %q, want the chosen one", got)
	}
	p.t.unseed(nil)
	if got := p.name(); got != "my chosen name" {
		t.Errorf("title after rollback = %q, want the chosen one kept", got)
	}
}

// TestTitleSeedWaitsForAWriter covers /save on an ephemeral chat: there was
// no writer to name while the turns ran, so nothing fires until /save mints
// one — and then the same seed call does placeholder and pass both.
func TestTitleSeedWaitsForAWriter(t *testing.T) {
	p := newTitleProbe(false)
	p.sw = nil // ephemeral: nothing is persisting yet
	history := []provider.Message{
		{Role: "user", Content: "a question"},
		{Role: "assistant", Content: "an answer"},
	}
	if _, _, ok := p.t.seed(history); ok {
		t.Fatal("seeded without a writer")
	}
	if len(p.window) != 0 {
		t.Fatalf("window written without a writer: %v", p.window)
	}

	p.sw = &SessionWriter{} // /save mints one
	fu, _, ok := p.t.seed(history)
	if !ok || fu != "a question" {
		t.Fatalf("seed after the writer appeared = (%q, %v)", fu, ok)
	}
	if got := p.name(); got != "a question" {
		t.Errorf("title = %q, want the placeholder", got)
	}
}

// TestTitleFollowsAWriterSwap: /session swaps the writer mid-chat, so the
// state must read the CURRENT one — a captured pointer would name the session
// the user just left.
func TestTitleFollowsAWriterSwap(t *testing.T) {
	p := newTitleProbe(false)
	first := p.sw
	p.t.seed(userTurn("first chat"))

	p.sw = &SessionWriter{} // /session resumed another bundle
	p.t.adopt()
	p.t.seed(userTurn("first chat")) // adopted: no-op either way

	if got := first.meta.Title; got != "first chat" {
		t.Errorf("the original session lost its title: %q", got)
	}
	if got := p.name(); got != "" {
		t.Errorf("the swapped-in session was renamed: %q", got)
	}
}

// TestTitleLandRacesTheLoop pins the locking contract: land arrives from the
// pass goroutine while the loop seeds and unseeds. Run with -race (CI does).
func TestTitleLandRacesTheLoop(t *testing.T) {
	p := newTitleProbe(false)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			p.t.land(i%3, "Racing Title")
		}
	}()
	history := userTurn("a question")
	for i := 0; i < 100; i++ {
		p.t.seed(history)
		p.t.unseed(nil)
	}
	<-done
}

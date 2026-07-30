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

// TestTitleSeedsBeforeTheReply is the whole point of seeding early: a first
// turn that spends minutes in tool calls must not leave the session nameless
// while it works. The placeholder comes from the user's message alone, so it
// is available before the provider is ever called.
func TestTitleSeedsBeforeTheReply(t *testing.T) {
	p := newTitleProbe(false)
	p.t.seed(userTurn("refactor the session writer"))

	if got := p.name(); got != "refactor the session writer" {
		t.Errorf("session title = %q, want the prompt-derived placeholder", got)
	}
	if got := p.lastWindow(); got != "refactor the session writer" {
		t.Errorf("window title = %q, want the placeholder", got)
	}
}

// TestTitleSeedOnlyOnce: later messages never rename a session.
func TestTitleSeedOnlyOnce(t *testing.T) {
	p := newTitleProbe(false)
	history := userTurn("first question")
	p.t.seed(history)
	history = append(history,
		provider.Message{Role: "assistant", Content: "sure"},
		provider.Message{Role: "user", Content: "second question"})
	p.t.seed(history)

	if got := p.name(); got != "first question" {
		t.Errorf("title = %q, want the FIRST message's placeholder", got)
	}
}

// TestTitleResumedSessionUntouched: a resumed bundle arrives named, and
// neither seeding nor upgrading may take that away.
func TestTitleResumedSessionUntouched(t *testing.T) {
	p := newTitleProbe(true)
	p.sw.meta.Title = "an earlier chat"
	history := []provider.Message{
		{Role: "user", Content: "new question"},
		{Role: "assistant", Content: "an answer"},
	}
	p.t.seed(history)
	if _, _, ok := p.t.upgrade(history); ok {
		t.Error("a resumed session asked for a model-written title")
	}
	if got := p.name(); got != "an earlier chat" {
		t.Errorf("title = %q, want the resumed one", got)
	}
	if len(p.window) != 0 {
		t.Errorf("window sink written %v, want untouched", p.window)
	}
}

// TestTitleUnseedOnRollback: a failed or discarded turn takes its user message
// out of the history, and the placeholder was derived from nothing else — so
// the name has to go back too, or the session keeps a name for a message it no
// longer contains.
func TestTitleUnseedOnRollback(t *testing.T) {
	p := newTitleProbe(false)
	history := userTurn("a question that errored")
	p.t.seed(history)
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
	// And the NEXT message gets to supply one.
	p.t.seed(userTurn("what actually worked"))
	if got := p.name(); got != "what actually worked" {
		t.Errorf("title = %q, want the next message's placeholder", got)
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

// TestTitleUnseedNeverTouchesASettledName: once a turn earned a title, a later
// rollback must not strip it.
func TestTitleUnseedNeverTouchesASettledName(t *testing.T) {
	p := newTitleProbe(false)
	history := []provider.Message{
		{Role: "user", Content: "a question"},
		{Role: "assistant", Content: "an answer"},
	}
	p.t.seed(history)
	if _, _, ok := p.t.upgrade(history); !ok {
		t.Fatal("upgrade declined a complete turn")
	}
	p.t.unseed(nil)

	if got := p.name(); got != "a question" {
		t.Errorf("title = %q, want the settled name kept", got)
	}
}

// TestTitleUpgradeWaitsForAReply: with no reply yet the name stays a
// placeholder and NOTHING is settled, so a later turn still gets to upgrade it.
func TestTitleUpgradeWaitsForAReply(t *testing.T) {
	p := newTitleProbe(false)
	history := userTurn("a question")
	p.t.seed(history)
	if _, _, ok := p.t.upgrade(history); ok {
		t.Error("upgrade asked for a title with no reply to summarize")
	}

	history = append(history, provider.Message{Role: "assistant", Content: "an answer"})
	fu, fa, ok := p.t.upgrade(history)
	if !ok || fu != "a question" || fa != "an answer" {
		t.Errorf("upgrade = (%q, %q, %v), want the turn's seeds", fu, fa, ok)
	}
}

// TestTitleUpgradeSeedsWhenSeedingWasSkipped covers /save on an ephemeral
// chat: there was no writer to name while the turns ran, so the upgrade has to
// lay the placeholder down itself.
func TestTitleUpgradeSeedsWhenSeedingWasSkipped(t *testing.T) {
	p := newTitleProbe(false)
	p.sw = nil // ephemeral: nothing is persisting yet
	history := []provider.Message{
		{Role: "user", Content: "a question"},
		{Role: "assistant", Content: "an answer"},
	}
	p.t.seed(history) // no writer: nothing happens
	if len(p.window) != 0 {
		t.Fatalf("seeded without a writer: %v", p.window)
	}

	p.sw = &SessionWriter{} // /save mints one
	if _, _, ok := p.t.upgrade(history); !ok {
		t.Fatal("upgrade declined after the writer appeared")
	}
	if got := p.name(); got != "a question" {
		t.Errorf("title = %q, want the placeholder laid down by upgrade", got)
	}
}

// TestTitleImageReplyIsFinal: a dedicated image provider has no model to
// summarize with — asking it for a title would paint a picture — so the
// prompt-derived placeholder settles as the final name.
func TestTitleImageReplyIsFinal(t *testing.T) {
	p := newTitleProbe(false)
	history := []provider.Message{
		{Role: "user", Content: "draw a cat"},
		{Role: "assistant", Attachments: []provider.Attachment{{Filename: "1.png"}}},
	}
	p.t.seed(history)
	if _, _, ok := p.t.upgrade(history); ok {
		t.Error("an image reply asked for a model-written title")
	}
	if got := p.name(); got != "draw a cat" {
		t.Errorf("title = %q, want the prompt placeholder", got)
	}
	// Settled: a later text turn must not rename it.
	history = append(history,
		provider.Message{Role: "user", Content: "another"},
		provider.Message{Role: "assistant", Content: "text"})
	if _, _, ok := p.t.upgrade(history); ok {
		t.Error("a settled image title was upgraded later")
	}
}

// TestTitleAdoptNameWins: an explicit /save title outranks any model pass.
func TestTitleAdoptNameWins(t *testing.T) {
	p := newTitleProbe(false)
	p.t.adoptName("my chosen name")
	history := []provider.Message{
		{Role: "user", Content: "a question"},
		{Role: "assistant", Content: "an answer"},
	}
	if _, _, ok := p.t.upgrade(history); ok {
		t.Error("a user-chosen title was sent for a model rewrite")
	}
	if got := p.name(); got != "my chosen name" {
		t.Errorf("title = %q, want the chosen one", got)
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

package chat

import (
	"sync"

	"chatchain/provider"
)

// sessionTitle owns a session's name across a turn's lifecycle. Three moves,
// in the order they can occur:
//
//	seed    as soon as the first user message exists — BEFORE the turn runs —
//	        land the prompt-derived placeholder and release that message for
//	        the async model pass
//	land    replace the placeholder with the model-written title when the
//	        pass returns
//	unseed  give the name back when the message it was derived from rolls
//	        back with a failed or discarded turn
//
// Nothing here waits for the assistant. Both the placeholder and the model
// pass are derived from the user's message alone: waiting for the reply left
// a tool-heavy first turn poorly named for as long as the tools ran — in the
// window title and in the session list both. The model pass therefore runs
// WHILE the turn streams, on a dedicated provider instance (run.go's titleP);
// provider per-call state (usage, image results) is not safe for a second
// concurrent request on the main one.
//
// Seeding before the turn means the name can outlive what it was named
// after, so a rolled-back turn gives it back — and because the model pass is
// usually in flight by then, unseed also bumps the generation so a
// late-landing title for the rolled-back message is dropped instead of
// naming the session after text it no longer contains.
type sessionTitle struct {
	// writer resolves the CURRENT session writer on every call rather than
	// capturing one: the loop swaps it (/session resumes another bundle) and
	// mints it late (an ephemeral chat has none until /save), so a captured
	// copy would name the session the user just left, or decide there is
	// none forever. The async pass resolves it at land time too — safe, the
	// loop titleWG.Waits before any input that could swap it.
	writer func() *SessionWriter
	window func(string) // window-title sink

	mu     sync.Mutex // the async pass's land races the loop's moves
	seeded bool       // a placeholder is on the session
	titled bool       // settled: resumed or user-chosen — never overwritten
	gen    int        // seed generation; land drops a pass unseed outlived
}

// newSessionTitle wires the sinks. A resumed session arrives already named, so
// it is left alone.
func newSessionTitle(writer func() *SessionWriter, window func(string), resumed bool) *sessionTitle {
	return &sessionTitle{writer: writer, window: window, seeded: resumed, titled: resumed}
}

// sw is the current writer, or nil when this chat is not persisting yet.
func (t *sessionTitle) sw() *SessionWriter {
	if t.writer == nil {
		return nil
	}
	return t.writer()
}

// set writes a name to both the session and the window. Callers hold mu.
func (t *sessionTitle) set(name string) {
	t.sw().SetTitle(name) // nil-safe
	if t.window != nil {
		t.window(name)
	}
}

// adopt settles the name without touching it — a session resumed mid-chat
// arrives with its own.
func (t *sessionTitle) adopt() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seeded, t.titled = true, true
}

// adoptName settles an explicit name. A title the user chose (/save "…") is
// never overwritten — a model pass still in flight is dropped by land's
// titled check.
func (t *sessionTitle) adoptName(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seeded, t.titled = true, true
	t.set(name)
}

// seed lands the placeholder derived from the first user message and, on the
// one call that newly seeds, releases that message and the generation so the
// caller can fire the model pass.
func (t *sessionTitle) seed(history []provider.Message) (firstUser string, gen int, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seeded || t.sw() == nil {
		return "", 0, false
	}
	firstUser = firstUserText(history)
	if firstUser == "" {
		return "", 0, false
	}
	t.seeded = true
	t.set(titleFrom(firstUser, 40))
	return firstUser, t.gen, true
}

// land applies the model pass's answer — unless the seed it was derived from
// rolled back meanwhile (generation mismatch), the name settled (an explicit
// /save title outranks the pass), or the pass came back empty (the
// placeholder stands; there is no retry).
func (t *sessionTitle) land(gen int, name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if gen != t.gen || t.titled || name == "" {
		return
	}
	t.set(name)
}

// unseed drops a name whose message is no longer in the history — seeding
// before the turn means the name can outlive what it was named after, so a
// rolled-back turn has to give it back, model-written or not: the text it
// summarized is gone either way. A settled title is never touched. The
// generation bump invalidates a pass still in flight.
func (t *sessionTitle) unseed(history []provider.Message) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.titled || !t.seeded {
		return
	}
	if firstUserText(history) != "" {
		return
	}
	t.seeded = false
	t.gen++
	t.set("")
}

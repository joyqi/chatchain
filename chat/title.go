package chat

import "chatchain/provider"

// sessionTitle owns a session's name across a turn's lifecycle. Three moves,
// in the order they can occur:
//
//	seed    the prompt-derived placeholder, as soon as the first user message
//	        exists — BEFORE the turn runs
//	unseed  give that placeholder back when the message rolls back with a
//	        failed or discarded turn
//	upgrade replace it with a model-written title once a reply exists to
//	        summarize
//
// Seeding is deliberately not deferred to the reply. The placeholder is
// derived from the user's message alone, and making it wait left a tool-heavy
// first turn nameless for as long as the tools ran — in the window title and
// in the session list both. Upgrading, by contrast, stays at turn end: text
// emitted mid-tool-loop is usually "let me check that", which makes a worse
// title than the finished answer, and asking for one there would put a second
// request on the provider while the loop is still working.
type sessionTitle struct {
	// writer resolves the CURRENT session writer on every call rather than
	// capturing one: the loop swaps it (/session resumes another bundle) and
	// mints it late (an ephemeral chat has none until /save), so a captured
	// copy would name the wrong session, or decide there is none forever.
	writer func() *SessionWriter
	window func(string) // window-title sink

	seeded bool // a placeholder is on the session
	titled bool // the name is settled: final, or a model pass is in flight
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

// set writes a name to both the session and the window.
func (t *sessionTitle) set(name string) {
	t.sw().SetTitle(name) // nil-safe
	if t.window != nil {
		t.window(name)
	}
}

// adopt settles the name without touching it — a session resumed mid-chat
// arrives with its own.
func (t *sessionTitle) adopt() { t.seeded, t.titled = true, true }

// adoptName settles an explicit name. A title the user chose (/save "…") is
// never overwritten by a model pass.
func (t *sessionTitle) adoptName(name string) {
	t.adopt()
	t.set(name)
}

// seed lands the placeholder derived from the first user message.
func (t *sessionTitle) seed(history []provider.Message) {
	if t.seeded || t.sw() == nil {
		return
	}
	firstUser, _, _ := titleSeeds(history)
	if firstUser == "" {
		return
	}
	t.seeded = true
	t.set(titleFrom(firstUser, 40))
}

// unseed drops a placeholder whose message is no longer in the history —
// seeding before the turn means the name can outlive what it was named after,
// so a rolled-back turn has to give it back. A settled title is never touched:
// by then the turn that earned it succeeded.
func (t *sessionTitle) unseed(history []provider.Message) {
	if t.titled || !t.seeded {
		return
	}
	if firstUser, _, _ := titleSeeds(history); firstUser != "" {
		return
	}
	t.seeded = false
	t.set("")
}

// upgrade settles the name at turn end, returning the seeds for the model pass
// when one is worth making. ok is false when there is nothing to ask for: the
// name is already settled, there is no reply yet (a later turn retries), or the
// reply was an image — a dedicated image provider asked for a title would paint
// a picture, so the placeholder stands as final.
func (t *sessionTitle) upgrade(history []provider.Message) (firstUser, firstAssistant string, ok bool) {
	if t.titled || t.sw() == nil {
		return "", "", false
	}
	firstUser, firstAssistant, imageReply := titleSeeds(history)
	if firstUser == "" {
		return "", "", false
	}
	t.seed(history) // the turn may have been the session's first
	if firstAssistant == "" {
		if imageReply {
			t.titled = true // the placeholder is the final title
		}
		return "", "", false
	}
	t.titled = true
	return firstUser, firstAssistant, true
}

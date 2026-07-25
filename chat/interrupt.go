package chat

import (
	"errors"
	"io"

	"chatchain/provider"
)

// errInterrupted is the sentinel returned by a streaming section when the user
// pressed ESC (or Ctrl+C) mid-stream. Callers treat it as "user stopped it":
// no retry, no error styling, and the partial output is handled by
// finalizeInterrupt. See docs/design/interrupt.md.
var errInterrupted = errors.New("interrupted")

// teeWriteCloser duplicates writes into a secondary writer while preserving
// Close on the underlying pipe (io.MultiWriter alone would lose the Closer the
// provider uses to signal the end of the reasoning stream).
type teeWriteCloser struct {
	io.Writer
	io.Closer
}

// finalizeInterrupt decides what an interrupted turn leaves behind.
// history[watermark:] is this turn's messages (the user message first, then
// any completed tool rounds). It applies the three-state table from
// docs/design/interrupt.md:
//
//   - partial text exists → append an assistant message carrying the partial
//     (Interrupted: true, no raw content — a partial provider blob may be
//     invalid on replay); persist.
//   - no text and only the user message since the watermark → truncate the
//     whole turn back to the watermark; nothing to persist.
//   - no text but completed tool rounds → keep the turn as-is with no trailing
//     assistant message (the tool side effects already happened); persist.
//
// A reasoning-only partial (reasoning without content) counts as "no text".
// Returns the new history, whether the turn should be persisted, and — when
// the turn was discarded whole — the attachments that went down with it, so
// the caller can hand them back to the pending set. Nothing else remembers
// them: they left pendingAttachments when the message was composed, and an
// image turn ALWAYS takes the discard path (it produces no text), which is
// how ESC used to silently strip an /edit canvas or an uploaded file.
func finalizeInterrupt(history []provider.Message, watermark int, partial, partialReasoning string) ([]provider.Message, bool, []provider.Attachment) {
	if partial != "" {
		return append(history, provider.Message{
			Role:        "assistant",
			Content:     partial,
			Reasoning:   partialReasoning,
			Interrupted: true,
		}), true, nil
	}
	if len(history) <= watermark+1 {
		var dropped []provider.Attachment
		if watermark < len(history) {
			dropped = history[watermark].Attachments
		}
		return history[:watermark], false, dropped
	}
	return history, true, nil
}

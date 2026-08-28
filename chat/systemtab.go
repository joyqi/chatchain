package chat

import (
	"strings"

	"github.com/joyqi/iota/internal/agents"
	"github.com/joyqi/iota/internal/ui"
	"github.com/joyqi/iota/provider"
)

// systemPromptPanel builds the /model surface's read-only System tab, or
// reports ok=false when this chat carries no system prompt at all.
//
// The text shown is the prompt AS SENT, not as configured: the -s flag, the
// config `system:`/`system_file:` keys, the interactive -S entry and a resumed
// session's stored prompt all converge on history[0] (run.go), while agent
// mode folds the AGENTS.md overlay in at send time. Composing through the same
// ComposeSendHistory the turn loop uses keeps that assembly rule in one place —
// the tab cannot drift from the wire.
func systemPromptPanel(history []provider.Message, overlay string) (ui.Panel, bool) {
	sent := agents.ComposeSendHistory(history, overlay)
	if len(sent) == 0 || sent[0].Role != "system" || sent[0].Content == "" {
		return ui.Panel{}, false
	}
	return ui.Panel{
		Title: "System", Kind: ui.PanelView, Wrap: true,
		Prompt: "System prompt in effect (read-only)",
		Lines:  strings.Split(sent[0].Content, "\n"),
	}, true
}

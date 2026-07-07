package chat

import (
	"fmt"

	"chatchain/internal/promptui"
	mcpmgr "chatchain/mcp"
	"chatchain/provider"
	"chatchain/tool"
)

// statusItem is one labeled row of the /status panel. Name is the bold left
// column, Value the right column.
type statusItem struct {
	Name  string
	Value string
}

// statusLines builds the detailed /status readout: provider, model, context
// usage and how that count was obtained, last turn's token counts, message and
// attachment counts, MCP wiring, and the session id. Fields degrade gracefully
// when not yet known.
func statusLines(p provider.Provider, b *contextBudget, history []provider.Message, pending int, dispatch tool.Dispatcher, mgr *mcpmgr.Manager, sw *SessionWriter) []statusItem {
	model := p.Model()
	if model == "" {
		model = "(not selected)"
	}
	pct := 0
	if b.window > 0 {
		pct = b.used * 100 / b.window
	}
	source := "estimated (local tokenizer)"
	if b.haveUsage {
		source = "provider-reported"
	}
	last := "(none yet)"
	if ur, ok := p.(provider.UsageReporter); ok {
		if in, out, ok := ur.LastUsage(); ok {
			last = fmt.Sprintf("input %d, output %d", in, out)
		}
	}
	session := "not saved (ephemeral)"
	if id := sw.ID(); id != "" {
		session = id
	}
	mcp := "none configured"
	if mgr != nil {
		if servers := mgr.Servers(); len(servers) > 0 {
			connected := 0
			for _, s := range servers {
				if s.Connected {
					connected++
				}
			}
			mcp = fmt.Sprintf("%d/%d servers connected", connected, len(servers))
		}
	}
	toolsLine := "none"
	if dispatch != nil {
		if n := len(dispatch.Tools()); n > 0 {
			toolsLine = fmt.Sprintf("%d available", n)
		}
	}

	items := []statusItem{
		{"Provider", p.Type()},
		{"Model", model},
	}
	// Tuning knobs (temperature, reasoning effort) when the provider exposes
	// them; "default" means the parameter is omitted from requests.
	if t, ok := p.(provider.Tunable); ok {
		items = append(items,
			statusItem{"Temperature", formatTemperature(t.Temperature())},
			statusItem{"Effort", effortLabel(t.Effort())},
		)
	}
	items = append(items,
		statusItem{"Context", fmt.Sprintf("%d / %d tokens (%d%%)", b.used, b.window, pct)},
		statusItem{"Token count", source},
		statusItem{"Last turn", last},
		statusItem{"Messages", fmt.Sprintf("%d in context", len(history))},
	)
	if pending > 0 {
		items = append(items, statusItem{"Attachments", fmt.Sprintf("%d pending", pending)})
	}
	items = append(items,
		statusItem{"Tools", toolsLine},
		statusItem{"MCP", mcp},
		statusItem{"Session", session},
	)
	return items
}

// showStatus renders the status items as a read-only Viewer panel: each row is a
// bold name + value, dismissed with Esc / q / Ctrl+C, leaving no residue.
func showStatus(items []statusItem) {
	lines := make([]string, len(items))
	for i, it := range items {
		lines[i] = fmt.Sprintf("%s  %s", BoldStyle.Sprintf("%-12s", it.Name), it.Value)
	}
	v := promptui.Viewer{Label: "Status", Lines: lines, Height: 15}
	_ = v.Run()
}

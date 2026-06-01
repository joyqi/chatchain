package chat

import (
	"fmt"
	"os"

	mcpmgr "chatchain/mcp"
	"chatchain/provider"

	"github.com/manifoldco/promptui"
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
func statusLines(p provider.Provider, b *contextBudget, history []provider.Message, pending int, mgr *mcpmgr.Manager, sw *SessionWriter) []statusItem {
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
			mcp = fmt.Sprintf("%d tools · %d/%d servers connected", len(mgr.Tools()), connected, len(servers))
		}
	}

	items := []statusItem{
		{"Provider", p.Type()},
		{"Model", model},
		{"Context", fmt.Sprintf("%d / %d tokens (%d%%)", b.used, b.window, pct)},
		{"Token count", source},
		{"Last turn", last},
		{"Messages", fmt.Sprintf("%d in context", len(history))},
	}
	if pending > 0 {
		items = append(items, statusItem{"Attachments", fmt.Sprintf("%d pending", pending)})
	}
	items = append(items,
		statusItem{"MCP", mcp},
		statusItem{"Session", session},
	)
	return items
}

// showStatus displays the status items as a transient panel via promptui,
// reusing its clean redraw: HideSelected wipes the whole view on dismiss, leaving
// no scrollback residue. It is built on Select (promptui's only widget with that
// clean clear, and whose screen buffer requires one line per item) but rendered
// as a static panel — identical Active/Inactive templates mean no moving pointer
// or highlight, and HideHelp drops the navigation hint, so it reads as a plain
// labeled panel rather than a menu. Each row's name is bold; Enter selects (and
// clears) a row, Esc / Ctrl+C route through escToCancelStdin to the same clean
// cleanup. The selection is irrelevant.
func showStatus(items []statusItem) {
	prompt := promptui.Select{
		Label:        "Status",
		Items:        items,
		Size:         len(items),
		Stdin:        &escToCancelStdin{r: os.Stdin},
		HideHelp:     true,
		HideSelected: true,
		Templates: &promptui.SelectTemplates{
			Label:    `{{ . | bold }}  {{ "Enter/Esc to close" | faint }}`,
			Active:   `  {{ printf "%-12s" .Name | bold }}  {{ .Value }}`,
			Inactive: `  {{ printf "%-12s" .Name | bold }}  {{ .Value }}`,
		},
	}
	_, _, _ = prompt.Run()
}

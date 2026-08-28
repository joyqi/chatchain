package chat

import (
	"fmt"
	"strings"

	"github.com/joyqi/iota/internal/tokfmt"
	mcpmgr "github.com/joyqi/iota/mcp"
	"github.com/joyqi/iota/provider"
	"github.com/joyqi/iota/tool"
)

// imageGenLabel renders an image provider's generation defaults for /status.
func imageGenLabel(g provider.ImageGenParams) string {
	var parts []string
	if g.AspectRatio != "" {
		parts = append(parts, "aspect "+g.AspectRatio)
	}
	if g.ImageSize != "" {
		parts = append(parts, "size "+g.ImageSize)
	}
	if g.NegativePrompt != "" {
		parts = append(parts, "negative "+truncateRunes(g.NegativePrompt, 24))
	}
	if len(parts) == 0 {
		return "default"
	}
	return strings.Join(parts, " · ")
}

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
func statusLines(p provider.Provider, b *contextBudget, total provider.Usage, history []provider.Message, pending int, dispatch tool.Dispatcher, mgr *mcpmgr.Manager, sw *SessionWriter) []statusItem {
	model := p.Model()
	if model == "" {
		model = "(not selected)"
	}
	pct := 0
	if b.window > 0 {
		pct = b.used() * 100 / b.window
	}
	source := "estimated (local tokenizer)"
	if b.haveUsage {
		source = "provider-reported"
	}
	last := "(none yet)"
	if ur, ok := p.(provider.UsageReporter); ok {
		if u, ok := ur.LastUsageFull(); ok {
			last = fmt.Sprintf("input %s, output %s", tokfmt.Tokens(u.Input), tokfmt.Tokens(u.Output))
			if u.Cached() {
				last += fmt.Sprintf(", cache %s (%.1f%%)", tokfmt.Tokens(u.CacheRead), u.CacheHitRate())
			}
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
	// Token rows only for providers that account tokens; image-generation
	// defaults only for dedicated image providers. Same idiom as the tuning
	// knobs above: the assertion is the capability.
	if _, ok := p.(provider.UsageReporter); ok {
		items = append(items,
			statusItem{"Context", fmt.Sprintf("%s / %s tokens (%d%%)",
				tokfmt.Tokens(b.used()), tokfmt.Tokens(b.window), pct)},
			statusItem{"Token count", source},
			statusItem{"Last turn", last},
			statusItem{"Session input", tokfmt.Tokens(total.Input) + " tokens"},
			statusItem{"Session output", tokfmt.Tokens(total.Output) + " tokens"},
		)
		// Cache rows only where caching actually happened: a provider that
		// reports none (or is never asked to cache) shows nothing rather than
		// a row of zeros.
		if total.Cached() {
			items = append(items, statusItem{"Session cache",
				fmt.Sprintf("%s read, %s written (%.1f%% of input)",
					tokfmt.Tokens(total.CacheRead), tokfmt.Tokens(total.CacheWrite),
					total.CacheHitRate())})
		}
	}
	if g, ok := p.(provider.ImageGenTunable); ok {
		items = append(items, statusItem{"Image params", imageGenLabel(g.ImageGenParams())})
	}
	items = append(items,
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

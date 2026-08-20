package chat

import (
	"testing"
)

// hasCmd reports whether the active table advertises a command value.
func hasCmd(v string) bool {
	for _, c := range activeSlashCommands {
		if c.Value == v {
			return true
		}
	}
	return false
}

// TestConditionalCommandVisibility pins the command-table contract: /skills
// joins activeSlashCommands only in agent mode, /save only for sessions that
// started ephemeral, the two toggles compose, and the base table is never
// mutated (the UI's suggestion menu and the dispatch loop both read it).
func TestConditionalCommandVisibility(t *testing.T) {
	t.Cleanup(func() { setActiveCommands(false, false, true, false) })

	setActiveCommands(false, false, true, false)
	if hasCmd("/skills") || hasCmd("/save") {
		t.Fatal("conditional commands visible with both toggles off")
	}
	setActiveCommands(true, false, true, false)
	if !hasCmd("/skills") || hasCmd("/save") {
		t.Fatal("agent-only toggle leaked or missed")
	}
	setActiveCommands(false, true, true, false)
	if hasCmd("/skills") || !hasCmd("/save") {
		t.Fatal("save-only toggle leaked or missed")
	}
	setActiveCommands(true, true, true, false)
	if !hasCmd("/skills") || !hasCmd("/save") {
		t.Fatal("both toggles must compose")
	}
	// A token-less provider (compact=false) drops /compact from the table.
	setActiveCommands(false, false, false, false)
	if hasCmd("/compact") {
		t.Fatal("/compact visible without token accounting")
	}
	setActiveCommands(false, false, true, false)
	if !hasCmd("/compact") {
		t.Fatal("/compact missing for a token-aware provider")
	}
	// /edit exists only on image providers (the typical shape: image=true
	// comes with compact=false).
	setActiveCommands(false, false, false, true)
	if !hasCmd("/edit") || !hasCmd("/redo") {
		t.Fatal("/edit and /redo missing for an image provider")
	}
	setActiveCommands(false, false, true, false)
	if hasCmd("/edit") || hasCmd("/redo") {
		t.Fatal("image commands visible on a text provider")
	}
	// The base table itself is never mutated.
	base := map[string]bool{}
	for _, c := range slashCommands {
		base[c.Value] = true
	}
	if base["/skills"] || base["/save"] || base["/edit"] || !base["/compact"] {
		t.Fatal("base slashCommands polluted")
	}
	// Every base entry carries a description: the list shows it beside the
	// name, and a blank column is the same as not having the feature.
	for _, c := range slashCommands {
		if c.Desc == "" {
			t.Errorf("%s has no description", c.Value)
		}
	}
}

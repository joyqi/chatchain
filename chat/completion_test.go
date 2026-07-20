package chat

import (
	"slices"
	"testing"
)

// TestConditionalCommandVisibility pins the command-table contract: /skills
// joins activeSlashCommands only in agent mode, /save only for sessions that
// started ephemeral, the two toggles compose, and the base table is never
// mutated (the UI's suggestion menu and the dispatch loop both read it).
func TestConditionalCommandVisibility(t *testing.T) {
	t.Cleanup(func() { setActiveCommands(false, false) })

	setActiveCommands(false, false)
	if slices.Contains(activeSlashCommands, "/skills") || slices.Contains(activeSlashCommands, "/save") {
		t.Fatal("conditional commands visible with both toggles off")
	}
	setActiveCommands(true, false)
	if !slices.Contains(activeSlashCommands, "/skills") || slices.Contains(activeSlashCommands, "/save") {
		t.Fatal("agent-only toggle leaked or missed")
	}
	setActiveCommands(false, true)
	if slices.Contains(activeSlashCommands, "/skills") || !slices.Contains(activeSlashCommands, "/save") {
		t.Fatal("save-only toggle leaked or missed")
	}
	setActiveCommands(true, true)
	if !slices.Contains(activeSlashCommands, "/skills") || !slices.Contains(activeSlashCommands, "/save") {
		t.Fatal("both toggles must compose")
	}
	// The base table itself is never mutated.
	if slices.Contains(slashCommands, "/skills") || slices.Contains(slashCommands, "/save") {
		t.Fatal("base slashCommands polluted")
	}
}

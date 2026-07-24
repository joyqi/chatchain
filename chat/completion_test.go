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
	t.Cleanup(func() { setActiveCommands(false, false, true, false) })

	setActiveCommands(false, false, true, false)
	if slices.Contains(activeSlashCommands, "/skills") || slices.Contains(activeSlashCommands, "/save") {
		t.Fatal("conditional commands visible with both toggles off")
	}
	setActiveCommands(true, false, true, false)
	if !slices.Contains(activeSlashCommands, "/skills") || slices.Contains(activeSlashCommands, "/save") {
		t.Fatal("agent-only toggle leaked or missed")
	}
	setActiveCommands(false, true, true, false)
	if slices.Contains(activeSlashCommands, "/skills") || !slices.Contains(activeSlashCommands, "/save") {
		t.Fatal("save-only toggle leaked or missed")
	}
	setActiveCommands(true, true, true, false)
	if !slices.Contains(activeSlashCommands, "/skills") || !slices.Contains(activeSlashCommands, "/save") {
		t.Fatal("both toggles must compose")
	}
	// A token-less provider (compact=false) drops /compact from the table.
	setActiveCommands(false, false, false, false)
	if slices.Contains(activeSlashCommands, "/compact") {
		t.Fatal("/compact visible without token accounting")
	}
	setActiveCommands(false, false, true, false)
	if !slices.Contains(activeSlashCommands, "/compact") {
		t.Fatal("/compact missing for a token-aware provider")
	}
	// /edit exists only on image providers (the typical shape: image=true
	// comes with compact=false).
	setActiveCommands(false, false, false, true)
	if !slices.Contains(activeSlashCommands, "/edit") {
		t.Fatal("/edit missing for an image provider")
	}
	setActiveCommands(false, false, true, false)
	if slices.Contains(activeSlashCommands, "/edit") {
		t.Fatal("/edit visible on a text provider")
	}
	// The base table itself is never mutated.
	if slices.Contains(slashCommands, "/skills") || slices.Contains(slashCommands, "/save") ||
		slices.Contains(slashCommands, "/edit") || !slices.Contains(slashCommands, "/compact") {
		t.Fatal("base slashCommands polluted")
	}
}

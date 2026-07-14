package chat

import (
	"slices"
	"testing"
)

// TestAgentCommandVisibility pins the command-table contract: /skills joins
// activeSlashCommands only in agent mode, and the base table is never mutated
// (the UI's suggestion menu and the dispatch loop both read this table).
func TestAgentCommandVisibility(t *testing.T) {
	t.Cleanup(func() { setAgentCommands(false) })

	setAgentCommands(false)
	if slices.Contains(activeSlashCommands, "/skills") {
		t.Fatal("/skills visible with agent mode off")
	}
	setAgentCommands(true)
	if !slices.Contains(activeSlashCommands, "/skills") {
		t.Fatal("/skills not active with agent mode on")
	}
	// The base table itself is never mutated.
	if slices.Contains(slashCommands, "/skills") {
		t.Fatal("base slashCommands polluted with /skills")
	}
}

package chat

// The slash-command table: the single source for the UI's completion
// suggestions (UI.SetSlashCommands) and the command dispatch in the run
// loop. Names are bare (no trailing space).
var slashCommands = []string{
	"/file", "/session", "/model", "/compact", "/export", "/status", "/tools", "/debug",
}

// agentSlashCommands exist only while agent mode is on; off, they are fully
// invisible — no completion, no highlighting, no dispatch (the input falls
// through as a plain message like any unknown slash text).
var agentSlashCommands = []string{"/skills"}

// activeSlashCommands is the effective command table; the run loop rebinds it
// once at startup via setAgentCommands, before the Program starts.
var activeSlashCommands = slashCommands

// setAgentCommands activates or deactivates the agent-only commands.
func setAgentCommands(enabled bool) {
	if enabled {
		activeSlashCommands = append(append([]string{}, slashCommands...), agentSlashCommands...)
	} else {
		activeSlashCommands = slashCommands
	}
}

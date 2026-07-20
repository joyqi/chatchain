package chat

// The slash-command table: the single source for the UI's completion
// suggestions (UI.SetSlashCommands) and the command dispatch in the run
// loop. Names are bare (no trailing space).
var slashCommands = []string{
	"/file", "/session", "/model", "/compact", "/export", "/status", "/tools", "/debug",
}

// agentSlashCommands exist only while agent mode is on; saveSlashCommands
// only while the session started ephemeral (--no-save / no_save). Inactive
// conditional commands are fully invisible — no completion, no highlighting,
// no dispatch (the input falls through as a plain message like any unknown
// slash text).
var agentSlashCommands = []string{"/skills"}
var saveSlashCommands = []string{"/save"}

// activeSlashCommands is the effective command table; the run loop rebinds it
// once at startup via setActiveCommands, before the Program starts.
var activeSlashCommands = slashCommands

// setActiveCommands activates the conditional command groups.
func setActiveCommands(agent, save bool) {
	cmds := append([]string{}, slashCommands...)
	if save {
		cmds = append(cmds, saveSlashCommands...)
	}
	if agent {
		cmds = append(cmds, agentSlashCommands...)
	}
	activeSlashCommands = cmds
}

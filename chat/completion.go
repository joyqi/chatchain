package chat

import "chatchain/internal/ui"

// The slash-command table: the single source for the composer's completion
// list (UI.SetSlashCommands) and the command dispatch in the run loop. Values
// are bare (no trailing space); the description is what the list shows beside
// each one, so a command's purpose no longer has to be remembered or guessed
// from its name.
var slashCommands = []ui.Suggestion{
	{Value: "/file", Desc: "Attach a file, or browse for one"},
	{Value: "/session", Desc: "Resume or delete a saved session"},
	{Value: "/model", Desc: "Model, context window, effort, temperature"},
	{Value: "/compact", Desc: "Summarize older context to reclaim the window"},
	{Value: "/export", Desc: "Write this chat out as HTML or Markdown"},
	{Value: "/status", Desc: "Provider, tokens, tools, session id"},
	{Value: "/tools", Desc: "Available tools and MCP server state"},
	{Value: "/debug", Desc: "Browse API traffic; toggle request recording"},
}

// agentSlashCommands exist only while agent mode is on; saveSlashCommands
// only while the session started ephemeral (--no-save / no_save); /compact
// only for providers with token accounting (compact=false drops it);
// imageSlashCommands only for dedicated image providers. Inactive
// conditional commands are fully invisible — no completion, no highlighting,
// no dispatch (the input falls through as a plain message like any unknown
// slash text).
var agentSlashCommands = []ui.Suggestion{
	{Value: "/skills", Desc: "List skills, or run one: /skills <name>"},
}
var saveSlashCommands = []ui.Suggestion{
	{Value: "/save", Desc: "Start persisting this ephemeral session"},
}
var imageSlashCommands = []ui.Suggestion{
	{Value: "/edit", Desc: "Edit the last generated image"},
	{Value: "/redo", Desc: "Re-send the last request"},
}

// activeSlashCommands is the effective command table; the run loop rebinds it
// once at startup via setActiveCommands, before the Program starts.
var activeSlashCommands = slashCommands

// commandFlags remembers which conditional groups are on, so the table can be
// rebuilt when the skill catalog changes without the caller re-stating them.
var commandFlags struct{ agent, save, compact, image bool }

// skillCommands are one "/skills <name>" entry per discovered skill. They are
// registered as whole values so the list matches them by prefix like any
// other command, and labelled with the bare name: the "/skills " part is
// already typed, and repeating it on every row would crowd out the
// description that makes a long catalog readable.
var skillCommands []ui.Suggestion

// setActiveCommands activates the conditional command groups.
func setActiveCommands(agent, save, compact, image bool) {
	commandFlags.agent, commandFlags.save = agent, save
	commandFlags.compact, commandFlags.image = compact, image
	rebuildCommands()
}

// setSkillCommands refreshes the per-skill entries; agent mode discovers
// skills at startup and again whenever the catalog changes on disk.
func setSkillCommands(skills []skillEntry) {
	skillCommands = skillCommands[:0]
	for _, sk := range skills {
		skillCommands = append(skillCommands, ui.Suggestion{
			Value: "/skills " + sk.Name,
			Label: sk.Name,
			Desc:  sk.Description,
		})
	}
	rebuildCommands()
}

// skillEntry is the completion-facing view of a discovered skill.
type skillEntry struct{ Name, Description string }

func rebuildCommands() {
	cmds := make([]ui.Suggestion, 0, len(slashCommands)+len(skillCommands)+3)
	for _, c := range slashCommands {
		if c.Value == "/compact" && !commandFlags.compact {
			continue
		}
		cmds = append(cmds, c)
	}
	if commandFlags.image {
		cmds = append(cmds, imageSlashCommands...)
	}
	if commandFlags.save {
		cmds = append(cmds, saveSlashCommands...)
	}
	if commandFlags.agent {
		cmds = append(cmds, agentSlashCommands...)
		cmds = append(cmds, skillCommands...)
	}
	activeSlashCommands = cmds
}

// commandHint renders the startup banner's command line from the active
// table — one source, so a conditional command can never be advertised and
// then not exist (or exist and not be advertised).
func commandNames() []string {
	names := make([]string, 0, len(activeSlashCommands))
	for _, c := range activeSlashCommands {
		if c.Label == "" { // skip the per-skill entries: they are not commands
			names = append(names, c.Value)
		}
	}
	return names
}

package chat

import (
	"fmt"

	"chatchain/internal/agents"
)

// Chat-side glue for agent mode: the Run option struct and the /skills view.
// The overlay itself (AGENTS.md chain + skills catalog) lives in
// internal/agents, shared with the agent toolset's load_skill.

// AgentOptions configures agent mode for Run. The zero value keeps agent mode
// off, and Run behaves exactly as it does without the feature.
type AgentOptions struct {
	Enabled bool
	Root    string // project root: the git root of the cwd, or the cwd itself
}

// agentCommandHint appends /skills to the startup command line only when
// agent mode is on (the command exists but answers with a notice otherwise).
func agentCommandHint(o *agents.Overlay) string {
	if o == nil {
		return ""
	}
	return ", /skills"
}

// skillsStatusLines renders the /skills view: each discovered skill with its
// source tag and description, then any invalid-skill warnings.
func skillsStatusLines(sks []agents.Skill, warnings []string, root string) []string {
	if len(sks) == 0 && len(warnings) == 0 {
		lines := []string{DimStyle.Sprint("No skills discovered. Searched:")}
		for _, d := range agents.SkillRoots(root) {
			lines = append(lines, DimStyle.Sprintf("  %s", d))
		}
		return lines
	}
	var lines []string
	for _, sk := range sks {
		lines = append(lines, fmt.Sprintf("%s  %s",
			BoldStyle.Sprint(sk.Name), YellowStyle.Sprintf("[%s]", agents.SkillSourceTag(sk.Path, root))))
		lines = append(lines, DimStyle.Sprintf("  %s", sk.Description))
	}
	if len(warnings) > 0 {
		lines = append(lines, "")
		lines = append(lines, ErrorStyle.Sprint("Skipped (invalid):"))
		for _, warning := range warnings {
			lines = append(lines, DimStyle.Sprintf("  %s", warning))
		}
	}
	return lines
}

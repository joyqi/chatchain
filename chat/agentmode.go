package chat

import (
	"errors"
	"fmt"
	"os"
	"strings"

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

// skillEntries projects discovered skills onto the completion table: the
// description is what makes a long catalog navigable, so it travels with the
// name.
func skillEntries(sks []agents.Skill) []skillEntry {
	out := make([]skillEntry, 0, len(sks))
	for _, sk := range sks {
		out = append(out, skillEntry{Name: sk.Name, Description: sk.Description})
	}
	return out
}

// findSkill resolves a catalog name, case-insensitively — the user is typing
// it, not the model reading it back from a prompt.
func findSkill(sks []agents.Skill, name string) (agents.Skill, bool) {
	for _, sk := range sks {
		if strings.EqualFold(sk.Name, name) {
			return sk, true
		}
	}
	return agents.Skill{}, false
}

// expandSkill turns "/skill <name> [text]" into the message that is actually
// sent: the skill's instructions in a tagged block, followed by whatever else
// the user typed on the line.
//
// It is an INPUT EXPANSION, not a command — the result travels as an ordinary
// user message, so the model side needs no new concept and mid-turn steering
// carries it like any other typed line. The tag and the stated directory are
// what let the model resolve the skill's relative references (the same two
// facts load_skill hands it on activation).
func expandSkill(sks []agents.Skill, arg string) (text, name string, err error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", "", errors.New("usage: /skills <name> [instructions]")
	}
	name, extra, _ := strings.Cut(arg, " ")
	sk, ok := findSkill(sks, name)
	if !ok {
		return "", name, fmt.Errorf("no skill named %q — /skills lists what is available", name)
	}
	data, rerr := os.ReadFile(sk.Path)
	if rerr != nil {
		return "", sk.Name, fmt.Errorf("cannot read skill %q: %w", sk.Name, rerr)
	}
	body, berr := agents.SkillBody(data)
	if berr != nil {
		// Discovery validated this file's frontmatter; reaching here means it
		// changed since, and half a manifest is worse than an error.
		return "", sk.Name, fmt.Errorf("skill %q: %w", sk.Name, berr)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<skill name=%q location=%q>\n", sk.Name, sk.Path)
	fmt.Fprintf(&b, "References are relative to %s.\n\n", sk.Dir())
	b.WriteString(strings.TrimSpace(body))
	b.WriteString("\n</skill>")
	if extra = strings.TrimSpace(extra); extra != "" {
		b.WriteString("\n\n" + extra)
	}
	return b.String(), sk.Name, nil
}

// byteSize renders a payload size for the skill-loaded notice.
func byteSize(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/1024)
}

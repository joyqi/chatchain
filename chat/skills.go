package chat

import (
	"fmt"
	"strings"

	"chatchain/internal/skills"
)

// Agent-mode skills, chat-side (docs/design/agent-mode.md): discovery and
// validation live in internal/skills; this file renders the level-1 catalog
// into the volatile system overlay and the /skills view. Activation is the
// model calling the load_skill built-in tool (the agent toolset) with a
// skill's name.

// skillsCatalogInstruction prefaces the catalog block: how the model turns a
// catalog entry into an active skill.
const skillsCatalogInstruction = "To use a skill, call the load_skill tool with the skill's name and follow " +
	"the instructions it returns; read files the skill references by calling load_skill again with the " +
	"\"file\" argument, and run its bundled scripts with run_command."

// skillsCatalog renders the level-1 skills catalog injected into the system
// overlay (modelled on the reference `skills-ref to-prompt` output): one
// instruction sentence and an <available_skills> block listing each skill's
// name and description (paths stay encapsulated behind load_skill).
func skillsCatalog(sks []skills.Skill) string {
	if len(sks) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(skillsCatalogInstruction)
	b.WriteString("\n\n<available_skills>\n")
	omitted := 0
	for _, sk := range sks {
		// Every field is XML-escaped: descriptions come from arbitrary (possibly
		// cloned) skill files and land inside the system prompt — unescaped
		// angle brackets could close the catalog block and plant text outside
		// its structural boundary (prompt injection).
		entry := fmt.Sprintf("<skill>\n<name>%s</name>\n<description>%s</description>\n</skill>\n",
			xmlEscape(sk.Name), xmlEscape(sk.Description))
		if b.Len()+len(entry) > skillsCatalogCap {
			omitted++
			continue
		}
		b.WriteString(entry)
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "<note>%d more skill(s) omitted: catalog size cap reached</note>\n", omitted)
	}
	b.WriteString("</available_skills>")
	return b.String()
}

// skillsCatalogCap bounds the rendered catalog like agentsChainCap bounds the
// AGENTS.md chain: the number of discovered skills is unbounded, the system
// prompt must not be.
const skillsCatalogCap = 32 << 10

// xmlEscape neutralizes markup characters in catalog fields.
var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func xmlEscape(s string) string { return xmlEscaper.Replace(s) }

// skillsStatusLines renders the /skills view: each discovered skill with its
// source tag and description, then any invalid-skill warnings.
func skillsStatusLines(sks []skills.Skill, warnings []string, root string) []string {
	if len(sks) == 0 && len(warnings) == 0 {
		lines := []string{DimStyle.Sprint("No skills discovered. Searched:")}
		for _, d := range skills.Roots(root) {
			lines = append(lines, DimStyle.Sprintf("  %s", d))
		}
		return lines
	}
	var lines []string
	for _, sk := range sks {
		lines = append(lines, fmt.Sprintf("%s  %s",
			BoldStyle.Sprint(sk.Name), YellowStyle.Sprintf("[%s]", skills.SourceTag(sk.Path, root))))
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

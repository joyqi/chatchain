package chat

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"chatchain/internal/promptui"

	"gopkg.in/yaml.v3"
)

// Agent-mode skills (docs/design/agent-mode.md, per the Agent Skills spec,
// https://agentskills.io/specification): a skill is a directory holding a
// SKILL.md with YAML frontmatter. Discovery renders a level-1 catalog into the
// volatile system overlay; activation is the model reading the SKILL.md via
// the read_file built-in tool.

// skillFileName is the manifest each skill directory must contain.
const skillFileName = "SKILL.md"

// skillNameRe matches valid skill names: lowercase alphanumerics separated by
// single hyphens (no leading/trailing/double hyphen).
var skillNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

const (
	skillNameMaxLen = 64   // characters
	skillDescMaxLen = 1024 // characters
)

// agentSkill is one discovered skill — exactly what the level-1 catalog needs.
type agentSkill struct {
	Name        string
	Description string
	Path        string // absolute path of the skill's SKILL.md
}

// skillsRoots returns the skill discovery directories for a project root,
// precedence high→low: the project's skills, then the chatchain-native and
// cross-client user directories.
func skillsRoots(root string) []string {
	dirs := []string{filepath.Join(root, ".agents", "skills")}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs,
			filepath.Join(home, ".chatchain", "skills"),
			filepath.Join(home, ".agents", "skills"))
	}
	return dirs
}

// discoverSkills scans the given directories (precedence high→low) for skills.
// Invalid skills are skipped with a warning naming the reason — never fatal;
// on a name collision the higher-precedence directory wins. Results keep the
// deterministic scan order (precedence-major, directory-name-minor), so the
// rendered catalog is byte-stable across turns.
func discoverSkills(dirs []string) (skills []agentSkill, warnings []string) {
	seen := make(map[string]bool)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // absent root: nothing to discover
		}
		for _, e := range entries {
			sub := filepath.Join(dir, e.Name())
			if fi, serr := os.Stat(sub); serr != nil || !fi.IsDir() {
				continue // plain files (and dangling symlinks) are not candidates
			}
			path := filepath.Join(sub, skillFileName)
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				continue // a directory without SKILL.md is not a skill
			}
			sk, perr := parseSkill(data, e.Name(), path)
			if perr != nil {
				warnings = append(warnings, fmt.Sprintf("skill %s: %v (skipped)", path, perr))
				continue
			}
			if seen[sk.Name] {
				continue // shadowed by a higher-precedence directory
			}
			seen[sk.Name] = true
			skills = append(skills, sk)
		}
	}
	return skills, warnings
}

// parseSkill validates one SKILL.md: required frontmatter fields name (which
// must equal the directory name) and description. Optional fields (license,
// compatibility, metadata, allowed-tools, …) are tolerated and ignored in P1.
func parseSkill(data []byte, dirName, path string) (agentSkill, error) {
	front, err := splitFrontmatter(data)
	if err != nil {
		return agentSkill{}, err
	}
	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal(front, &meta); err != nil {
		return agentSkill{}, fmt.Errorf("invalid frontmatter YAML: %v", err)
	}
	switch {
	case meta.Name == "":
		return agentSkill{}, errors.New(`frontmatter is missing required field "name"`)
	case len(meta.Name) > skillNameMaxLen:
		return agentSkill{}, fmt.Errorf("name exceeds %d characters", skillNameMaxLen)
	case !skillNameRe.MatchString(meta.Name):
		return agentSkill{}, fmt.Errorf("invalid name %q (want lowercase alphanumerics separated by single hyphens)", meta.Name)
	case meta.Name != dirName:
		return agentSkill{}, fmt.Errorf("name %q does not match directory name %q", meta.Name, dirName)
	case meta.Description == "":
		return agentSkill{}, errors.New(`frontmatter is missing required field "description"`)
	case utf8.RuneCountInString(meta.Description) > skillDescMaxLen:
		return agentSkill{}, fmt.Errorf("description exceeds %d characters", skillDescMaxLen)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return agentSkill{Name: meta.Name, Description: meta.Description, Path: abs}, nil
}

// splitFrontmatter extracts the YAML frontmatter from a SKILL.md: the document
// must open with a "---" line, and the frontmatter runs until the next one.
func splitFrontmatter(data []byte) ([]byte, error) {
	const delim = "---"
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, delim+"\n") && text != delim {
		return nil, errors.New("missing YAML frontmatter (file must start with ---)")
	}
	body := strings.TrimPrefix(strings.TrimPrefix(text, delim), "\n")
	if i := strings.Index(body, "\n"+delim+"\n"); i >= 0 {
		return []byte(body[:i]), nil
	}
	if strings.HasSuffix(body, "\n"+delim) {
		return []byte(strings.TrimSuffix(body, "\n"+delim)), nil
	}
	return nil, errors.New("unterminated YAML frontmatter (missing closing ---)")
}

// skillsCatalogInstruction prefaces the catalog block: how the model turns a
// catalog entry into an active skill.
const skillsCatalogInstruction = "To use a skill, read its SKILL.md with the read_file tool and follow its " +
	"instructions, reading any files it references with read_file and running its bundled scripts with run_command."

// skillsCatalog renders the level-1 skills catalog injected into the system
// overlay (modelled on the reference `skills-ref to-prompt` output): one
// instruction sentence and an <available_skills> block listing each skill's
// name, description, and the absolute path of its SKILL.md.
func skillsCatalog(skills []agentSkill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(skillsCatalogInstruction)
	b.WriteString("\n\n<available_skills>\n")
	omitted := 0
	for _, sk := range skills {
		// Every field is XML-escaped: descriptions come from arbitrary (possibly
		// cloned) skill files and land inside the system prompt — unescaped
		// angle brackets could close the catalog block and plant text outside
		// its structural boundary (prompt injection).
		entry := fmt.Sprintf("<skill>\n<name>%s</name>\n<description>%s</description>\n<path>%s</path>\n</skill>\n",
			xmlEscape(sk.Name), xmlEscape(sk.Description), xmlEscape(sk.Path))
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

// skillsProbe stats everything skill freshness depends on, without reading
// content: the discovery roots (adding/removing a skill updates its root's
// mtime) AND each discovered skill's SKILL.md (so editing a description is
// detected too). Returns parallel path/mtime slices for exact comparison.
func skillsProbe(dirs []string, skills []agentSkill) ([]string, []time.Time) {
	var paths []string
	var stamps []time.Time
	for _, dir := range dirs {
		fi, err := os.Stat(dir)
		if err != nil || !fi.IsDir() {
			continue
		}
		paths = append(paths, dir)
		stamps = append(stamps, fi.ModTime())
	}
	for _, sk := range skills {
		fi, err := os.Stat(sk.Path)
		if err != nil || !fi.Mode().IsRegular() {
			continue // vanished skill: the shrunken path list flags the change
		}
		paths = append(paths, sk.Path)
		stamps = append(stamps, fi.ModTime())
	}
	return paths, stamps
}

// skillSourceTag labels where a skill was discovered, matched against the
// discovery roots (project first, then the user-level directories).
func skillSourceTag(path, root string) string {
	home, _ := os.UserHomeDir()
	switch {
	case strings.HasPrefix(path, filepath.Join(root, ".agents", "skills")+string(filepath.Separator)):
		return "project"
	case home != "" && strings.HasPrefix(path, filepath.Join(home, ".chatchain", "skills")+string(filepath.Separator)):
		return "user"
	case home != "" && strings.HasPrefix(path, filepath.Join(home, ".agents", "skills")+string(filepath.Separator)):
		return "user (.agents)"
	default:
		return filepath.Dir(path)
	}
}

// skillsStatusLines renders the /skills view: each discovered skill with its
// source tag and description, then any invalid-skill warnings.
func skillsStatusLines(skills []agentSkill, warnings []string, root string) []string {
	if len(skills) == 0 && len(warnings) == 0 {
		lines := []string{DimStyle.Sprint("No skills discovered. Searched:")}
		for _, d := range skillsRoots(root) {
			lines = append(lines, DimStyle.Sprintf("  %s", d))
		}
		return lines
	}
	var lines []string
	for _, sk := range skills {
		lines = append(lines, fmt.Sprintf("%s  %s",
			BoldStyle.Sprint(sk.Name), YellowStyle.Sprintf("[%s]", skillSourceTag(sk.Path, root))))
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

// showSkills opens a read-only viewer over the discovered skills — the user
// side of the level-1 catalog the model sees. Refreshes discovery first so
// the view matches the next send.
func showSkills(overlay *systemOverlay, root string) {
	overlay.refresh()
	skills := overlay.skillList()
	v := promptui.Viewer{
		Label:  fmt.Sprintf("Skills (%d)", len(skills)),
		Lines:  skillsStatusLines(skills, overlay.warnings(), root),
		Wrap:   true,
		Height: 15,
	}
	_ = v.Run()
}

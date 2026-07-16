package agents

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Agent Skills per the spec (https://agentskills.io/specification): a skill
// is a directory holding a SKILL.md with YAML frontmatter. Discovery feeds
// the Overlay's level-1 catalog; the load_skill built-in tool resolves a
// catalog name back to the skill's files for activation.

// SkillFileName is the manifest each skill directory must contain.
const SkillFileName = "SKILL.md"

// skillNameRe matches valid skill names: lowercase alphanumerics separated by
// single hyphens (no leading/trailing/double hyphen).
var skillNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

const (
	skillNameMaxLen = 64   // characters
	skillDescMaxLen = 1024 // characters
)

// Skill is one discovered skill.
type Skill struct {
	Name        string
	Description string
	Path        string // absolute path of the skill's SKILL.md
}

// Dir returns the skill's directory — the root for its referenced files and
// bundled scripts.
func (s Skill) Dir() string { return filepath.Dir(s.Path) }

// SkillRoots returns the skill discovery directories for a project root,
// precedence high→low: the project's skills, then the chatchain-native and
// cross-client user directories.
func SkillRoots(root string) []string {
	dirs := []string{filepath.Join(root, ".agents", "skills")}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs,
			filepath.Join(home, ".chatchain", "skills"),
			filepath.Join(home, ".agents", "skills"))
	}
	return dirs
}

// DiscoverSkills scans the given directories (precedence high→low) for skills.
// Invalid skills are skipped with a warning naming the reason — never fatal;
// on a name collision the higher-precedence directory wins. Results keep the
// deterministic scan order (precedence-major, directory-name-minor), so the
// rendered catalog is byte-stable across turns.
func DiscoverSkills(dirs []string) (skills []Skill, warnings []string) {
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
			path := filepath.Join(sub, SkillFileName)
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
func parseSkill(data []byte, dirName, path string) (Skill, error) {
	front, _, err := splitFrontmatter(data)
	if err != nil {
		return Skill{}, err
	}
	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal(front, &meta); err != nil {
		return Skill{}, fmt.Errorf("invalid frontmatter YAML: %v", err)
	}
	switch {
	case meta.Name == "":
		return Skill{}, errors.New(`frontmatter is missing required field "name"`)
	case len(meta.Name) > skillNameMaxLen:
		return Skill{}, fmt.Errorf("name exceeds %d characters", skillNameMaxLen)
	case !skillNameRe.MatchString(meta.Name):
		return Skill{}, fmt.Errorf("invalid name %q (want lowercase alphanumerics separated by single hyphens)", meta.Name)
	case meta.Name != dirName:
		return Skill{}, fmt.Errorf("name %q does not match directory name %q", meta.Name, dirName)
	case meta.Description == "":
		return Skill{}, errors.New(`frontmatter is missing required field "description"`)
	case utf8.RuneCountInString(meta.Description) > skillDescMaxLen:
		return Skill{}, fmt.Errorf("description exceeds %d characters", skillDescMaxLen)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return Skill{Name: meta.Name, Description: meta.Description, Path: abs}, nil
}

// SkillBody strips the frontmatter from SKILL.md content and returns the
// instruction body — what load_skill hands to the model on activation.
func SkillBody(data []byte) (string, error) {
	_, body, err := splitFrontmatter(data)
	if err != nil {
		return "", err
	}
	return strings.TrimLeft(body, "\n"), nil
}

// splitFrontmatter extracts the YAML frontmatter and the remaining body from
// a SKILL.md: the document must open with a "---" line, and the frontmatter
// runs until the next one.
func splitFrontmatter(data []byte) (front []byte, body string, err error) {
	const delim = "---"
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, delim+"\n") && text != delim {
		return nil, "", errors.New("missing YAML frontmatter (file must start with ---)")
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(text, delim), "\n")
	if i := strings.Index(rest, "\n"+delim+"\n"); i >= 0 {
		return []byte(rest[:i]), rest[i+len(delim)+2:], nil
	}
	if strings.HasSuffix(rest, "\n"+delim) {
		return []byte(strings.TrimSuffix(rest, "\n"+delim)), "", nil
	}
	return nil, "", errors.New("unterminated YAML frontmatter (missing closing ---)")
}

// probeSkills stats everything skill freshness depends on, without reading
// content: the discovery roots (adding/removing a skill updates its root's
// mtime) AND each discovered skill's SKILL.md (so editing a description is
// detected too). Returns parallel path/mtime slices for exact comparison.
func probeSkills(dirs []string, skills []Skill) ([]string, []time.Time) {
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

// SkillSourceTag labels where a skill was discovered, matched against the
// discovery roots (project first, then the user-level directories).
func SkillSourceTag(path, root string) string {
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

// skillsCatalogInstruction prefaces the catalog block: how the model turns a
// catalog entry into an active skill.
const skillsCatalogInstruction = "To use a skill, call the load_skill tool with the skill's name and follow " +
	"the instructions it returns; read files the skill references by calling load_skill again with the " +
	"\"file\" argument, and run its bundled scripts with the bash tool."

// skillsCatalogCap bounds the rendered catalog like agentsChainCap bounds the
// AGENTS.md chain: the number of discovered skills is unbounded, the system
// prompt must not be.
const skillsCatalogCap = 32 << 10

// skillsCatalog renders the level-1 skills catalog injected into the system
// overlay (modelled on the reference `skills-ref to-prompt` output): one
// instruction sentence and an <available_skills> block listing each skill's
// name and description (paths stay encapsulated behind load_skill).
func skillsCatalog(skills []Skill) string {
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

// xmlEscape neutralizes markup characters in catalog fields.
var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func xmlEscape(s string) string { return xmlEscaper.Replace(s) }

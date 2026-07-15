// Package skills implements agent-mode skill discovery per the Agent Skills
// spec (https://agentskills.io/specification): a skill is a directory holding
// a SKILL.md with YAML frontmatter. The chat layer renders discovered skills
// into the level-1 catalog of the volatile system overlay; the load_skill
// built-in tool resolves a skill name back to its files for activation. Both
// sit on this package (docs/design/agent-mode.md).
package skills

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

// FileName is the manifest each skill directory must contain.
const FileName = "SKILL.md"

// nameRe matches valid skill names: lowercase alphanumerics separated by
// single hyphens (no leading/trailing/double hyphen).
var nameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

const (
	nameMaxLen = 64   // characters
	descMaxLen = 1024 // characters
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

// Roots returns the skill discovery directories for a project root,
// precedence high→low: the project's skills, then the chatchain-native and
// cross-client user directories.
func Roots(root string) []string {
	dirs := []string{filepath.Join(root, ".agents", "skills")}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs,
			filepath.Join(home, ".chatchain", "skills"),
			filepath.Join(home, ".agents", "skills"))
	}
	return dirs
}

// Discover scans the given directories (precedence high→low) for skills.
// Invalid skills are skipped with a warning naming the reason — never fatal;
// on a name collision the higher-precedence directory wins. Results keep the
// deterministic scan order (precedence-major, directory-name-minor), so the
// rendered catalog is byte-stable across turns.
func Discover(dirs []string) (skills []Skill, warnings []string) {
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
			path := filepath.Join(sub, FileName)
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
	case len(meta.Name) > nameMaxLen:
		return Skill{}, fmt.Errorf("name exceeds %d characters", nameMaxLen)
	case !nameRe.MatchString(meta.Name):
		return Skill{}, fmt.Errorf("invalid name %q (want lowercase alphanumerics separated by single hyphens)", meta.Name)
	case meta.Name != dirName:
		return Skill{}, fmt.Errorf("name %q does not match directory name %q", meta.Name, dirName)
	case meta.Description == "":
		return Skill{}, errors.New(`frontmatter is missing required field "description"`)
	case utf8.RuneCountInString(meta.Description) > descMaxLen:
		return Skill{}, fmt.Errorf("description exceeds %d characters", descMaxLen)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return Skill{Name: meta.Name, Description: meta.Description, Path: abs}, nil
}

// Body strips the frontmatter from SKILL.md content and returns the
// instruction body — what load_skill hands to the model on activation.
func Body(data []byte) (string, error) {
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

// Probe stats everything skill freshness depends on, without reading content:
// the discovery roots (adding/removing a skill updates its root's mtime) AND
// each discovered skill's SKILL.md (so editing a description is detected
// too). Returns parallel path/mtime slices for exact comparison.
func Probe(dirs []string, skills []Skill) ([]string, []time.Time) {
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

// SourceTag labels where a skill was discovered, matched against the
// discovery roots (project first, then the user-level directories).
func SourceTag(path, root string) string {
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

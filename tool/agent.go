package tool

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"chatchain/internal/agents"
	"chatchain/provider"

	"gopkg.in/yaml.v3"
)

// loadSkillMaxBytes caps how much of a skill file is read from disk (mirroring
// the attachment cap in chat); anything past it is never loaded.
const loadSkillMaxBytes = 20 * 1024 * 1024 // 20MB

// loadSkillMaxOutput caps the text returned to the model; longer windows are
// cut with a marker telling the model how to continue via offset/limit.
const loadSkillMaxOutput = 64 * 1024 // 64KB

// newAgentSet builds the "agent" toolset — the tools agent mode runs on. It
// takes no configuration yet: the shared node is tolerated and ignored so a
// `tools: {agent: ...}` entry has room to grow settings.
func newAgentSet(env Env, _ yaml.Node) ([]Tool, error) {
	return []Tool{&loadSkill{root: env.ProjectRoot}}, nil
}

// loadSkill activates agent-mode skills: it resolves a catalog name back to
// the skill's directory and serves its SKILL.md body (frontmatter consumed)
// or, via "file", a file bundled inside that directory. Reads are confined to
// the skill's directory — this replaced the general-purpose read_file tool,
// which could read anything on the machine.
type loadSkill struct {
	root string // project root anchoring skill discovery ("" = cwd at call time)
}

func (l *loadSkill) Def() provider.ToolDef {
	return provider.ToolDef{
		Name: "load_skill",
		Description: "Load a skill by name to activate it: returns the skill's instructions and its directory " +
			"(the base for its bundled files and scripts). Available skills are listed in the system prompt. " +
			"Pass the optional \"file\" to read a file the skill's instructions reference, as a path relative " +
			"to the skill's directory. Long content is windowed by \"offset\"/\"limit\" lines.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"skill": map[string]any{
					"type":        "string",
					"description": "Skill name exactly as listed in the available skills catalog.",
				},
				"file": map[string]any{
					"type":        "string",
					"description": "Optional file to read instead of the skill's instructions, relative to the skill's directory (e.g. \"references/api.md\").",
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "1-based line number to start reading from (default 1).",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of lines to return (default: all remaining lines).",
				},
			},
			"required": []any{"skill"},
		},
	}
}

func (l *loadSkill) Call(_ context.Context, args map[string]any) (string, bool, error) {
	name, _ := args["skill"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return "missing required argument: skill", true, nil
	}

	sk, errText := l.resolve(name)
	if errText != "" {
		return errText, true, nil
	}

	if file, _ := args["file"].(string); strings.TrimSpace(file) != "" {
		return l.serveFile(sk, strings.TrimSpace(file), args)
	}
	return l.serveInstructions(sk, args)
}

// resolve re-discovers skills (discovery is a few readdirs — cheap, and always
// consistent with the catalog the model just saw) and finds name.
func (l *loadSkill) resolve(name string) (agents.Skill, string) {
	root := l.root
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = cwd
		}
	}
	sks, _ := agents.DiscoverSkills(agents.SkillRoots(root))
	for _, sk := range sks {
		if sk.Name == name {
			return sk, ""
		}
	}
	if len(sks) == 0 {
		return agents.Skill{}, fmt.Sprintf("unknown skill %q: no skills are installed", name)
	}
	names := make([]string, 0, len(sks))
	for _, sk := range sks {
		names = append(names, sk.Name)
	}
	sort.Strings(names)
	return agents.Skill{}, fmt.Sprintf("unknown skill %q; available skills: %s", name, strings.Join(names, ", "))
}

// serveInstructions returns the skill's SKILL.md body prefixed with a header
// naming the skill and its directory — the model needs the directory to run
// bundled scripts through run_command and to name files for "file" reads.
func (l *loadSkill) serveInstructions(sk agents.Skill, args map[string]any) (string, bool, error) {
	data, errText := readCapped(sk.Path)
	if errText != "" {
		return errText, true, nil
	}
	body, err := agents.SkillBody(data)
	if err != nil {
		// Discovery validated the frontmatter; reaching this means the file
		// changed since. Surface it rather than serving half a manifest.
		return fmt.Sprintf("skill %q: %v", sk.Name, err), true, nil
	}
	out, errText := windowLines(body, args, fmt.Sprintf("instructions of skill %q", sk.Name))
	if errText != "" {
		return errText, true, nil
	}
	return fmt.Sprintf("skill: %s\ndirectory: %s\n\n%s", sk.Name, sk.Dir(), out), false, nil
}

// serveFile returns a file bundled inside the skill's directory. The path is
// jailed to that directory: absolute paths and ".." escapes are rejected.
// (Symlinks pointing outside are accepted until the P2 workspace-trust pass —
// still strictly tighter than the machine-wide read_file this replaced.)
func (l *loadSkill) serveFile(sk agents.Skill, file string, args map[string]any) (string, bool, error) {
	clean := filepath.Clean(filepath.FromSlash(file))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Sprintf("file must be a relative path inside the skill's directory, got %q", file), true, nil
	}
	path := filepath.Join(sk.Dir(), clean)

	data, errText := readCapped(path)
	if errText != "" {
		return errText, true, nil
	}
	out, errText := windowLines(string(data), args, fmt.Sprintf("%s of skill %q", clean, sk.Name))
	if errText != "" {
		return errText, true, nil
	}
	return out, false, nil
}

// readCapped reads a regular file up to loadSkillMaxBytes, returning a
// model-facing error string on failure.
func readCapped(path string) ([]byte, string) {
	fi, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return nil, fmt.Sprintf("file does not exist: %s", path)
	case err != nil:
		return nil, fmt.Sprintf("cannot access %s: %v", path, err)
	case fi.IsDir():
		return nil, fmt.Sprintf("%s is a directory, not a file", path)
	case !fi.Mode().IsRegular():
		return nil, fmt.Sprintf("%s is not a regular file", path)
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Sprintf("cannot open %s: %v", path, err)
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, loadSkillMaxBytes))
	if err != nil {
		return nil, fmt.Sprintf("cannot read %s: %v", path, err)
	}
	if fi.Size() > loadSkillMaxBytes {
		data = append(data, fmt.Sprintf("\n[file is %d bytes; only the first %d MB was read]",
			fi.Size(), loadSkillMaxBytes/(1024*1024))...)
	}
	return data, ""
}

// windowLines applies the offset/limit line window and the output cap to
// content; what names the content in the continuation markers. A non-empty
// errText is a model-facing error.
func windowLines(content string, args map[string]any, what string) (out, errText string) {
	lines := splitLines(content)
	total := len(lines)
	if total == 0 {
		return "[content is empty]", ""
	}

	start := intArg(args, "offset")
	if start < 1 {
		start = 1
	}
	if start > total {
		return "", fmt.Sprintf("offset %d is past the end of the %s (%d lines)", start, what, total)
	}
	end := total
	if limit := intArg(args, "limit"); limit > 0 && start-1+limit < end {
		end = start - 1 + limit
	}

	out = strings.Join(lines[start-1:end], "\n")
	var marks []string
	if len(out) > loadSkillMaxOutput {
		out = truncateToRuneBoundary(out, loadSkillMaxOutput)
		// The last surviving line is likely partial; a follow-up call resumes on it.
		last := start + strings.Count(out, "\n")
		marks = append(marks, fmt.Sprintf(
			"[output truncated at %d KB — showing lines %d-%d of %d; call load_skill again with offset=%d and a limit to continue]",
			loadSkillMaxOutput/1024, start, last, total, last))
	} else if start > 1 || end < total {
		marks = append(marks, fmt.Sprintf("[showing lines %d-%d of %d]", start, end, total))
	}
	if len(marks) > 0 {
		out += "\n" + strings.Join(marks, "\n")
	}
	return out, ""
}

// splitLines splits content into lines, treating a trailing newline as a line
// terminator rather than the start of an empty final line.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if n := len(lines); lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// intArg reads an integer argument that may arrive as float64 (JSON decoding),
// int, or int64 depending on the provider SDK. Missing or other types → 0.
func intArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// truncateToRuneBoundary cuts s at max bytes, backing up to a rune boundary so
// the cut never splits a UTF-8 sequence.
func truncateToRuneBoundary(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

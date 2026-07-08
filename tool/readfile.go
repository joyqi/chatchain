package tool

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"chatchain/provider"

	"gopkg.in/yaml.v3"
)

// readFileMaxBytes caps how much of a file is read from disk (mirroring the
// attachment cap in chat); anything past it is never loaded.
const readFileMaxBytes = 20 * 1024 * 1024 // 20MB

// readFileMaxOutput caps the text returned to the model; longer windows are
// cut with a marker telling the model how to continue via offset/limit.
const readFileMaxOutput = 64 * 1024 // 64KB

// readFile reads text files from the user's machine — the read-only built-in
// that powers agent-mode skills (a skill is activated by reading its SKILL.md).
type readFile struct{}

// newReadFile builds the tool. read_file takes no configuration in P1: any
// config value is tolerated and ignored, so agent mode can auto-register it
// with an empty node while a `tools:` entry may still declare it explicitly.
func newReadFile(yaml.Node) (Tool, error) { return &readFile{}, nil }

func (f *readFile) Def() provider.ToolDef {
	return provider.ToolDef{
		Name: "read_file",
		Description: "Read a text file from the user's machine and return its content, windowed by the " +
			"optional \"offset\" (1-based first line) and \"limit\" (line count). Paths may be absolute, " +
			"relative to the current directory, or ~-prefixed. This is also how skills are used: read a " +
			"skill's SKILL.md to activate it, then read any files it references the same way.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path (absolute, relative to the current directory, or ~-prefixed).",
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
			"required": []any{"path"},
		},
	}
}

func (f *readFile) Call(_ context.Context, args map[string]any) (string, bool, error) {
	path, _ := args["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		return "missing required argument: path", true, nil
	}

	resolved := expandHome(path)
	fi, err := os.Stat(resolved)
	switch {
	case os.IsNotExist(err):
		return fmt.Sprintf("file does not exist: %s", path), true, nil
	case err != nil:
		return fmt.Sprintf("cannot access %s: %v", path, err), true, nil
	case fi.IsDir():
		return fmt.Sprintf("%s is a directory, not a file", path), true, nil
	case !fi.Mode().IsRegular():
		return fmt.Sprintf("%s is not a regular file", path), true, nil
	}

	fh, err := os.Open(resolved)
	if err != nil {
		return fmt.Sprintf("cannot open %s: %v", path, err), true, nil
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, readFileMaxBytes))
	if err != nil {
		return fmt.Sprintf("cannot read %s: %v", path, err), true, nil
	}

	lines := splitLines(string(data))
	total := len(lines)
	if total == 0 {
		return "[file is empty]", false, nil
	}

	start := intArg(args, "offset")
	if start < 1 {
		start = 1
	}
	if start > total {
		return fmt.Sprintf("offset %d is past the end of %s (%d lines)", start, path, total), true, nil
	}
	end := total
	if limit := intArg(args, "limit"); limit > 0 && start-1+limit < end {
		end = start - 1 + limit
	}

	out := strings.Join(lines[start-1:end], "\n")
	var marks []string
	if len(out) > readFileMaxOutput {
		out = truncateToRuneBoundary(out, readFileMaxOutput)
		// The last surviving line is likely partial; a follow-up call resumes on it.
		last := start + strings.Count(out, "\n")
		marks = append(marks, fmt.Sprintf(
			"[output truncated at %d KB — showing lines %d-%d of %d; call read_file again with offset=%d and a limit to continue]",
			readFileMaxOutput/1024, start, last, total, last))
	} else if start > 1 || end < total {
		marks = append(marks, fmt.Sprintf("[showing lines %d-%d of %d]", start, end, total))
	}
	if fi.Size() > readFileMaxBytes {
		marks = append(marks, fmt.Sprintf("[file is %d bytes; only the first %d MB was read]",
			fi.Size(), readFileMaxBytes/(1024*1024)))
	}
	if len(marks) > 0 {
		out += "\n" + strings.Join(marks, "\n")
	}
	return out, false, nil
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

// expandHome resolves a leading ~ to the user's home directory.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
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

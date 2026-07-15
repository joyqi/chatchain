package tool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"chatchain/provider"

	"gopkg.in/yaml.v3"
)

// commandTimeout is the hard cap on a single run_command execution. A call can
// also be cancelled earlier (ESC) via the context passed to Call.
const commandTimeout = 10 * time.Minute

// runCommand executes commands the user has allow-listed by program name. It
// runs argv directly with NO shell, so shell metacharacters (| & ; > < $ * etc.)
// are never interpreted — an allowed program can't be used to smuggle a second
// command. allow holds globs matched against argv[0] and its basename; an empty
// allow list permits any program.
type runCommand struct {
	allow []string
}

// newCommandSet builds the "command" toolset. Its shared config is the allow
// list of program globs, consumed by run_command (the set's only tool so far).
func newCommandSet(_ Env, node yaml.Node) ([]Tool, error) {
	rc, err := newRunCommand(node)
	if err != nil {
		return nil, err
	}
	return []Tool{rc}, nil
}

// newRunCommand builds the tool from its config: a list of allowed program
// globs. An empty/null config permits any program.
func newRunCommand(node yaml.Node) (Tool, error) {
	var allow []string
	if !node.IsZero() {
		if err := node.Decode(&allow); err != nil {
			return nil, fmt.Errorf("config must be a list of allowed program globs: %w", err)
		}
	}
	return &runCommand{allow: allow}, nil
}

func (c *runCommand) Def() provider.ToolDef {
	desc := "Run a command on the user's machine and return its combined stdout/stderr. " +
		"The command runs directly with NO shell: pipes, redirects, globbing, variable " +
		"expansion and chaining (| > < * $VAR ; &&) are NOT interpreted — pass a single " +
		"program with its arguments. Use the optional \"stdin\" to feed standard input and " +
		"\"cwd\" to set the working directory."
	if len(c.allow) > 0 {
		desc += " Only these programs are permitted: " + strings.Join(c.allow, ", ") + "."
	} else {
		desc += " Any program is permitted."
	}
	return provider.ToolDef{
		Name:        "run_command",
		Description: desc,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Full command line, e.g. 'git log --oneline -5'.",
				},
				"stdin": map[string]any{
					"type":        "string",
					"description": "Optional data written to the command's standard input.",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Optional working directory; defaults to the current directory.",
				},
			},
			"required": []any{"command"},
		},
	}
}

func (c *runCommand) Call(ctx context.Context, args map[string]any) (string, bool, error) {
	command, _ := args["command"].(string)
	command = strings.TrimSpace(command)
	if command == "" {
		return "missing required argument: command", true, nil
	}

	argv, err := splitArgs(command)
	if err != nil {
		return fmt.Sprintf("could not parse command: %v", err), true, nil
	}
	if len(argv) == 0 {
		return "empty command", true, nil
	}
	if !c.allowed(argv[0]) {
		return fmt.Sprintf("command not allowed: %q is not in the permitted list (%s)",
			argv[0], strings.Join(c.allow, ", ")), true, nil
	}

	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if cwd, ok := args["cwd"].(string); ok && strings.TrimSpace(cwd) != "" {
		cmd.Dir = cwd
	}
	if stdin, ok := args["stdin"].(string); ok && stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	out := buf.String()

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Sprintf("%s\n[command timed out after %s]", out, commandTimeout), true, nil
	case errors.Is(ctx.Err(), context.Canceled):
		return fmt.Sprintf("%s\n[command cancelled]", out), true, nil
	}

	if runErr != nil {
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			return fmt.Sprintf("%s\n[exit code %d]", out, exit.ExitCode()), true, nil
		}
		return fmt.Sprintf("%s\n[failed to run: %v]", out, runErr), true, nil
	}
	if strings.TrimSpace(out) == "" {
		out = "[command produced no output]"
	}
	return out, false, nil
}

// allowed reports whether prog (argv[0]) passes the allow list. An empty list
// permits any program. Each entry is a glob matched against both the full
// argv[0] and its basename, so "git" matches "git" and "/usr/bin/git".
func (c *runCommand) allowed(prog string) bool {
	if len(c.allow) == 0 {
		return true
	}
	base := filepath.Base(prog)
	for _, pat := range c.allow {
		if globMatch(pat, prog) || globMatch(pat, base) {
			return true
		}
	}
	return false
}

// globMatch reports whether s matches a glob pattern supporting * (any run,
// including path separators) and ? (any single character). The match is
// anchored to the whole string. Unlike filepath.Match, * spans "/".
func globMatch(pattern, s string) bool {
	var b strings.Builder
	b.WriteByte('^')
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteByte('.')
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteByte('$')
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

// splitArgs splits a command line into argv using POSIX-ish quoting rules but
// WITHOUT any expansion: no variable, glob, or command substitution. Shell
// control operators (| & ; > <) are NOT special — they become literal bytes of
// whatever word contains them. Combined with shell-free exec, this means an
// allow-listed program can never be used to launch a second command.
func splitArgs(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			args = append(args, cur.String())
			cur.Reset()
			inWord = false
		}
	}

	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
			i++
		case c == '\'':
			inWord = true
			i++
			for i < len(s) && s[i] != '\'' {
				cur.WriteByte(s[i])
				i++
			}
			if i >= len(s) {
				return nil, errors.New("unterminated single quote")
			}
			i++ // closing '
		case c == '"':
			inWord = true
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
					cur.WriteByte(s[i+1])
					i += 2
					continue
				}
				cur.WriteByte(s[i])
				i++
			}
			if i >= len(s) {
				return nil, errors.New("unterminated double quote")
			}
			i++ // closing "
		case c == '\\':
			if i+1 >= len(s) {
				return nil, errors.New("trailing backslash")
			}
			inWord = true
			cur.WriteByte(s[i+1])
			i += 2
		default:
			inWord = true
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return args, nil
}

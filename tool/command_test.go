package tool

import (
	"context"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSplitArgs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"simple", "git status", []string{"git", "status"}},
		{"flags", "git log --oneline -5", []string{"git", "log", "--oneline", "-5"}},
		{"extra spaces", "  ls   -la  ", []string{"ls", "-la"}},
		{"double quotes", `grep "hello world" file`, []string{"grep", "hello world", "file"}},
		{"single quotes", `echo 'a b c'`, []string{"echo", "a b c"}},
		{"escaped space", `cat a\ b`, []string{"cat", "a b"}},
		// Injection neutralization: shell operators are literal, never separators.
		{"semicolon", "git log; rm -rf ~", []string{"git", "log;", "rm", "-rf", "~"}},
		{"pipe", "cat x | sh", []string{"cat", "x", "|", "sh"}},
		{"and", "true && rm x", []string{"true", "&&", "rm", "x"}},
		{"subshell", "echo $(rm -rf /)", []string{"echo", "$(rm", "-rf", "/)"}},
		{"redirect", "echo hi > /etc/passwd", []string{"echo", "hi", ">", "/etc/passwd"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := splitArgs(tt.in)
			if err != nil {
				t.Fatalf("splitArgs(%q) error: %v", tt.in, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitArgs(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestSplitArgsErrors(t *testing.T) {
	for _, in := range []string{`echo "unterminated`, `echo 'unterminated`, `echo trailing\`} {
		if _, err := splitArgs(in); err == nil {
			t.Errorf("splitArgs(%q) expected error, got nil", in)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{"git", "git", true},
		{"git", "github", false},
		{"git*", "github", true},
		{"py*", "python3", true},
		{"py*", "ruby", false},
		{"*", "anything", true},
		{"ssh", "ssh", true},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		// '*' spans path separators (unlike filepath.Match).
		{"*git", "/usr/bin/git", true},
	}
	for _, tt := range tests {
		if got := globMatch(tt.pattern, tt.s); got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.s, got, tt.want)
		}
	}
}

func TestAllowed(t *testing.T) {
	tests := []struct {
		name  string
		allow []string
		prog  string
		want  bool
	}{
		{"empty allows all", nil, "rm", true},
		{"exact match", []string{"git", "ssh"}, "git", true},
		{"not listed", []string{"git", "ssh"}, "rm", false},
		{"basename of abs path", []string{"git"}, "/usr/bin/git", true},
		{"glob program", []string{"py*"}, "python3", true},
		{"glob no match", []string{"py*"}, "node", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &runCommand{allow: tt.allow}
			if got := c.allowed(tt.prog); got != tt.want {
				t.Errorf("allowed(%q) with %v = %v, want %v", tt.prog, tt.allow, got, tt.want)
			}
		})
	}
}

func TestRunCommandCall(t *testing.T) {
	ctx := context.Background()

	t.Run("missing command", func(t *testing.T) {
		c := &runCommand{}
		out, isErr, err := c.Call(ctx, map[string]any{})
		if err != nil || !isErr || !strings.Contains(out, "missing required argument") {
			t.Fatalf("got (%q, %v, %v)", out, isErr, err)
		}
	})

	t.Run("not allowed", func(t *testing.T) {
		c := &runCommand{allow: []string{"git"}}
		out, isErr, err := c.Call(ctx, map[string]any{"command": "rm -rf /"})
		if err != nil || !isErr || !strings.Contains(out, "not allowed") {
			t.Fatalf("got (%q, %v, %v)", out, isErr, err)
		}
	})

	t.Run("runs allowed program", func(t *testing.T) {
		// `go` is guaranteed present during `go test`.
		c := &runCommand{allow: []string{"go"}}
		out, isErr, err := c.Call(ctx, map[string]any{"command": "go version"})
		if err != nil || isErr || !strings.Contains(out, "go version") {
			t.Fatalf("got (%q, %v, %v)", out, isErr, err)
		}
	})

	t.Run("non-zero exit is an error result", func(t *testing.T) {
		c := &runCommand{allow: []string{"go"}}
		_, isErr, err := c.Call(ctx, map[string]any{"command": "go thisisnotacommand"})
		if err != nil || !isErr {
			t.Fatalf("expected isError result, got isErr=%v err=%v", isErr, err)
		}
	})

	if runtime.GOOS == "windows" {
		return
	}

	t.Run("stdin is fed", func(t *testing.T) {
		if _, err := exec.LookPath("cat"); err != nil {
			t.Skip("cat not available")
		}
		c := &runCommand{allow: []string{"cat"}}
		out, isErr, err := c.Call(ctx, map[string]any{"command": "cat", "stdin": "hello-stdin"})
		if err != nil || isErr || !strings.Contains(out, "hello-stdin") {
			t.Fatalf("got (%q, %v, %v)", out, isErr, err)
		}
	})

	t.Run("cwd is honored", func(t *testing.T) {
		if _, err := exec.LookPath("pwd"); err != nil {
			t.Skip("pwd not available")
		}
		dir := t.TempDir()
		c := &runCommand{allow: []string{"pwd"}}
		out, isErr, err := c.Call(ctx, map[string]any{"command": "pwd", "cwd": dir})
		// macOS /tmp is a symlink to /private/tmp, so match the basename.
		if err != nil || isErr || !strings.Contains(out, lastPath(dir)) {
			t.Fatalf("got (%q, %v, %v) want dir %q", out, isErr, err, dir)
		}
	})
}

func lastPath(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// rawTools decodes a YAML snippet (the body of a provider's `tools:` block) into
// the map Build expects.
func rawTools(t *testing.T, y string) map[string]yaml.Node {
	t.Helper()
	var m map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(y), &m); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	return m
}

func TestBuildRegistry(t *testing.T) {
	t.Run("absent key disables set", func(t *testing.T) {
		r := Build(Env{}, rawTools(t, "agent:\n"), nil)
		if defs := r.Tools(); len(defs) != 1 || defs[0].Name != "load_skill" {
			t.Fatalf("expected only the agent set (load_skill), got %v", defs)
		}
	})

	t.Run("present empty enables with allow-all", func(t *testing.T) {
		var warned []string
		r := Build(Env{}, rawTools(t, "command:\n"), func(f string, a ...any) {
			warned = append(warned, f)
		})
		if len(r.Tools()) != 1 || r.Tools()[0].Name != "run_command" {
			t.Fatalf("expected run_command enabled, got %v", r.Tools())
		}
		out, isErr, err := r.CallTool(context.Background(), "run_command", map[string]any{"command": "go version"})
		if err != nil || isErr || !strings.Contains(out, "go version") {
			t.Fatalf("allow-all should run: (%q, %v, %v)", out, isErr, err)
		}
		if len(warned) != 0 {
			t.Fatalf("unexpected warnings: %v", warned)
		}
	})

	t.Run("populated list restricts", func(t *testing.T) {
		r := Build(Env{}, rawTools(t, "command:\n  - git\n  - ssh\n"), nil)
		out, isErr, _ := r.CallTool(context.Background(), "run_command", map[string]any{"command": "rm -rf /"})
		if !isErr || !strings.Contains(out, "not allowed") {
			t.Fatalf("rm should be rejected, got (%q, %v)", out, isErr)
		}
	})

	t.Run("unknown set warns and is skipped", func(t *testing.T) {
		var warned int
		r := Build(Env{}, rawTools(t, "bogus_set:\n"), func(string, ...any) { warned++ })
		if len(r.Tools()) != 0 || warned != 1 {
			t.Fatalf("expected skip+1 warning, got tools=%v warned=%d", r.Tools(), warned)
		}
	})
}

func TestMerge(t *testing.T) {
	reg := Build(Env{}, rawTools(t, "command:\n"), nil)
	merged := Merge(reg, nil)
	if len(merged.Tools()) != 1 {
		t.Fatalf("merge should expose run_command, got %v", merged.Tools())
	}
	out, isErr, err := merged.CallTool(context.Background(), "run_command", map[string]any{"command": "go version"})
	if err != nil || isErr || !strings.Contains(out, "go version") {
		t.Fatalf("merged routing failed: (%q, %v, %v)", out, isErr, err)
	}
	if _, _, err := merged.CallTool(context.Background(), "nope", nil); err == nil {
		t.Fatalf("expected error for unknown tool")
	}
}

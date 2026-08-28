package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joyqi/iota/internal/shell"

	"gopkg.in/yaml.v3"
)

// newBash builds the shell set over a temp project root with the given YAML
// config (the value of the `shell:` key) and returns root + tool.
func newBash(t *testing.T, cfgYAML string) (string, Tool) {
	t.Helper()
	root := t.TempDir()
	tools, err := newShellSet(Env{ProjectRoot: root}, rawNode(t, cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Def().Name != "bash" {
		t.Fatalf("shell set = %v, want the bash tool", tools)
	}
	return root, tools[0]
}

func TestBashCall(t *testing.T) {
	root, bash := newBash(t, "sandbox: off\n")

	out, isErr := call(t, bash, map[string]any{"command": "echo hello | tr a-z A-Z"})
	if isErr || !strings.Contains(out, "HELLO") {
		t.Fatalf("pipe failed: (%q, %v)", out, isErr)
	}

	// cwd defaults to the project root; a relative cwd resolves against it.
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, _ = call(t, bash, map[string]any{"command": "pwd"})
	if !strings.Contains(out, filepath.Base(root)) {
		t.Errorf("default cwd = %q, want the project root", out)
	}
	out, _ = call(t, bash, map[string]any{"command": "pwd", "cwd": "sub"})
	if !strings.HasSuffix(strings.TrimSpace(out), "/sub") {
		t.Errorf("relative cwd = %q, want .../sub", out)
	}

	// Exit codes and empty output are reported model-facing.
	out, isErr = call(t, bash, map[string]any{"command": "exit 3"})
	if !isErr || !strings.Contains(out, "[exit code 3]") {
		t.Fatalf("exit code = (%q, %v)", out, isErr)
	}
	out, isErr = call(t, bash, map[string]any{"command": "true"})
	if isErr || out != "[command produced no output]" {
		t.Fatalf("empty output = (%q, %v)", out, isErr)
	}
	out, isErr = call(t, bash, map[string]any{})
	if !isErr || !strings.Contains(out, "missing required argument") {
		t.Fatalf("missing command = (%q, %v)", out, isErr)
	}
}

func TestBashApprovalMatrix(t *testing.T) {
	approval := func(cfg string) bool {
		_, bash := newBash(t, cfg)
		a, ok := bash.(approver)
		if !ok {
			t.Fatal("bash should implement approver")
		}
		return a.RequiresApproval()
	}
	if !approval("sandbox: off\n") {
		t.Error("unsandboxed bash should require approval")
	}
	if approval("sandbox: off\nauto_run: true\n") {
		t.Error("auto_run should waive approval")
	}
	// auto: approval exactly when no sandbox is available.
	if got := approval(""); got != !shell.Available() {
		t.Errorf("auto approval = %v, shell.Available = %v", got, shell.Available())
	}
}

func TestShellSetConfigErrors(t *testing.T) {
	// The pre-bash allow-list shape is no longer valid: warn and skip the set.
	var warned int
	r := Build(Env{}, rawTools(t, "shell:\n  - git\n"), func(string, ...any) { warned++ })
	if len(r.Tools()) != 0 || warned != 1 {
		t.Fatalf("legacy list config: tools=%v warned=%d, want skip+1 warning", r.Tools(), warned)
	}

	if _, err := newShellSet(Env{}, rawNode(t, "sandbox: bogus\n")); err == nil {
		t.Fatal("invalid sandbox mode should error")
	}
}

// rawNode decodes a YAML snippet into a single node (a set's config value).
// An empty snippet yields a zero node (set defaults).
func rawNode(t *testing.T, y string) yaml.Node {
	t.Helper()
	var n yaml.Node
	if y == "" {
		return n
	}
	if err := yaml.Unmarshal([]byte(y), &n); err != nil {
		t.Fatal(err)
	}
	return n
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

	t.Run("shell set enables bash", func(t *testing.T) {
		var warned []string
		r := Build(Env{}, rawTools(t, "shell:\n  sandbox: off\n"), func(f string, a ...any) {
			warned = append(warned, f)
		})
		if len(r.Tools()) != 1 || r.Tools()[0].Name != "bash" {
			t.Fatalf("expected bash enabled, got %v", r.Tools())
		}
		out, isErr, err := r.CallTool(context.Background(), "bash", map[string]any{"command": "echo registry"})
		if err != nil || isErr || !strings.Contains(out, "registry") {
			t.Fatalf("bash via registry: (%q, %v, %v)", out, isErr, err)
		}
		if len(warned) != 0 {
			t.Fatalf("unexpected warnings: %v", warned)
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
	reg := Build(Env{}, rawTools(t, "shell:\n  sandbox: off\n"), nil)
	merged := Merge(reg, nil)
	if len(merged.Tools()) != 1 {
		t.Fatalf("merge should expose bash, got %v", merged.Tools())
	}
	out, isErr, err := merged.CallTool(context.Background(), "bash", map[string]any{"command": "echo merged"})
	if err != nil || isErr || !strings.Contains(out, "merged") {
		t.Fatalf("merged routing failed: (%q, %v, %v)", out, isErr, err)
	}
	if _, _, err := merged.CallTool(context.Background(), "nope", nil); err == nil {
		t.Fatalf("expected error for unknown tool")
	}
}

// Every bash call is its own `bash -c`, so a helper defined in one call is
// gone by the next. The description must SAY so: skill and README instructions
// routinely tell a model to define a function first and use it afterwards
// ("brain() { node …/brain.mjs \"$@\"; }"), which fails a turn later with
// "command not found" — far from the call that appeared to work.
func TestBashDescriptionStatesShellStateContract(t *testing.T) {
	for _, b := range []*bashTool{{root: "/proj"}, {root: "/proj", sandboxed: true}} {
		desc := b.Def().Description
		for _, want := range []string{"FRESH shell", "functions", "do not carry over"} {
			if !strings.Contains(desc, want) {
				t.Fatalf("sandboxed=%v: description missing %q:\n%s", b.sandboxed, want, desc)
			}
		}
	}
}

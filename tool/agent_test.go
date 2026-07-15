package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newSkillProject lays out a project root with one installed skill and
// isolates $HOME so the user-level discovery dirs never leak into the test.
// Returns the project root and the skill's directory.
func newSkillProject(t *testing.T, name, body string) (root, dir string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root = t.TempDir()
	dir = filepath.Join(root, ".agents", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + name + "\ndescription: a test skill\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, dir
}

// callLoadSkill runs load_skill rooted at root and returns (text, isError).
func callLoadSkill(t *testing.T, root string, args map[string]any) (string, bool) {
	t.Helper()
	ls := &loadSkill{root: root}
	out, isErr, err := ls.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call() hard error: %v", err)
	}
	return out, isErr
}

func TestLoadSkillInstructions(t *testing.T) {
	root, dir := newSkillProject(t, "demo", "\n# Do the thing\n\nStep one.\n")

	out, isErr := callLoadSkill(t, root, map[string]any{"skill": "demo"})
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	for _, want := range []string{"skill: demo", "directory: " + dir, "# Do the thing", "Step one."} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The frontmatter is consumed, not served.
	if strings.Contains(out, "description: a test skill") {
		t.Errorf("frontmatter leaked into the output:\n%s", out)
	}
}

func TestLoadSkillUnknown(t *testing.T) {
	root, _ := newSkillProject(t, "demo", "body\n")

	out, isErr := callLoadSkill(t, root, map[string]any{"skill": "nope"})
	if !isErr || !strings.Contains(out, `unknown skill "nope"`) || !strings.Contains(out, "demo") {
		t.Fatalf("want unknown-skill error listing %q, got (%q, %v)", "demo", out, isErr)
	}

	// No skills installed at all: say so instead of listing nothing.
	t.Setenv("HOME", t.TempDir())
	out, isErr = callLoadSkill(t, t.TempDir(), map[string]any{"skill": "demo"})
	if !isErr || !strings.Contains(out, "no skills are installed") {
		t.Fatalf("want no-skills error, got (%q, %v)", out, isErr)
	}

	// Missing argument.
	out, isErr = callLoadSkill(t, root, map[string]any{})
	if !isErr || !strings.Contains(out, "missing required argument") {
		t.Fatalf("want missing-argument error, got (%q, %v)", out, isErr)
	}
}

func TestLoadSkillFile(t *testing.T) {
	root, dir := newSkillProject(t, "demo", "see references/api.md\n")
	refDir := filepath.Join(dir, "references")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refDir, "api.md"), []byte("API DETAILS\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file outside the skill dir that a jail escape would reach.
	if err := os.WriteFile(filepath.Join(root, ".agents", "skills", "secret.txt"), []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, isErr := callLoadSkill(t, root, map[string]any{"skill": "demo", "file": "references/api.md"})
	if isErr || !strings.Contains(out, "API DETAILS") {
		t.Fatalf("bundled file read failed: (%q, %v)", out, isErr)
	}

	// The jail: ".." escapes and absolute paths are rejected.
	for _, file := range []string{"../secret.txt", "..", filepath.Join(root, ".agents", "skills", "secret.txt")} {
		out, isErr = callLoadSkill(t, root, map[string]any{"skill": "demo", "file": file})
		if !isErr || !strings.Contains(out, "relative path inside the skill") {
			t.Errorf("file %q should be rejected, got (%q, %v)", file, out, isErr)
		}
	}

	// A missing bundled file is a model-facing error, not a hard failure.
	out, isErr = callLoadSkill(t, root, map[string]any{"skill": "demo", "file": "references/nope.md"})
	if !isErr || !strings.Contains(out, "does not exist") {
		t.Fatalf("missing file should error, got (%q, %v)", out, isErr)
	}
}

func TestLoadSkillWindow(t *testing.T) {
	root, dir := newSkillProject(t, "demo", "body\n")
	if err := os.WriteFile(filepath.Join(dir, "long.txt"), []byte("l1\nl2\nl3\nl4\nl5\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, isErr := callLoadSkill(t, root, map[string]any{
		"skill": "demo", "file": "long.txt", "offset": float64(2), "limit": float64(2)})
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	if !strings.HasPrefix(out, "l2\nl3") || !strings.Contains(out, "[showing lines 2-3 of 5]") {
		t.Fatalf("window wrong:\n%s", out)
	}

	out, isErr = callLoadSkill(t, root, map[string]any{"skill": "demo", "file": "long.txt", "offset": float64(99)})
	if !isErr || !strings.Contains(out, "past the end") {
		t.Fatalf("offset past end should error, got (%q, %v)", out, isErr)
	}
}

// Agent mode auto-registers the agent set; a `tools:` entry that already
// enabled it keeps its configured instance (no duplicates either way).
func TestEnableAgentSet(t *testing.T) {
	root, _ := newSkillProject(t, "demo", "body\n")
	env := Env{ProjectRoot: root}

	reg := Build(env, nil, nil)
	reg.EnableSet(env, "agent", nil)
	if defs := reg.Tools(); len(defs) != 1 || defs[0].Name != "load_skill" {
		t.Fatalf("Tools() = %+v, want load_skill alone", defs)
	}

	// Config already enabled the set: EnableSet must not duplicate it.
	reg = Build(env, rawTools(t, "agent:\n"), nil)
	reg.EnableSet(env, "agent", nil)
	if defs := reg.Tools(); len(defs) != 1 {
		t.Fatalf("Tools() = %+v, want a single load_skill", defs)
	}

	out, isErr, err := reg.CallTool(context.Background(), "load_skill", map[string]any{"skill": "demo"})
	if err != nil || isErr || !strings.Contains(out, "skill: demo") {
		t.Fatalf("registry routing failed: (%q, %v, %v)", out, isErr, err)
	}

	// Unknown set name still warns.
	var warned int
	reg.EnableSet(env, "bogus", func(string, ...any) { warned++ })
	if warned != 1 {
		t.Fatalf("expected a warning for unknown set, got %d", warned)
	}
}

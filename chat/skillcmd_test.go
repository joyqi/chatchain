package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chatchain/internal/agents"
)

func writeSkill(t *testing.T, dir, name, body string) agents.Skill {
	t.Helper()
	sd := filepath.Join(dir, name)
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sd, agents.SkillFileName)
	content := "---\nname: " + name + "\ndescription: does " + name + "\n---\n\n" + body + "\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return agents.Skill{Name: name, Description: "does " + name, Path: p}
}

// /skill is an input EXPANSION: the sent message carries the skill's
// instructions plus whatever else was typed, tagged with the name and the
// directory its relative references resolve against.
func TestExpandSkill(t *testing.T) {
	dir := t.TempDir()
	sk := writeSkill(t, dir, "brain-page", "Read the page, then write it back.")
	sks := []agents.Skill{sk}

	got, name, err := expandSkill(sks, " brain-page  now do the thing ")
	if err != nil {
		t.Fatalf("expandSkill: %v", err)
	}
	if name != "brain-page" {
		t.Fatalf("name = %q", name)
	}
	for _, want := range []string{
		`<skill name="brain-page"`,
		sk.Path,
		"References are relative to " + sk.Dir(),
		"Read the page, then write it back.",
		"</skill>",
		"now do the thing",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expansion missing %q:\n%s", want, got)
		}
	}
	// The frontmatter is consumed, never forwarded.
	if strings.Contains(got, "description: does brain-page") {
		t.Fatalf("frontmatter leaked into the message:\n%s", got)
	}
	// The user's own text follows the block, not inside it.
	if strings.Index(got, "now do the thing") < strings.Index(got, "</skill>") {
		t.Fatalf("trailing text landed inside the skill block:\n%s", got)
	}
}

// Without extra text the block stands alone.
func TestExpandSkillNameOnly(t *testing.T) {
	dir := t.TempDir()
	sks := []agents.Skill{writeSkill(t, dir, "solo", "Just do it.")}
	got, _, err := expandSkill(sks, " solo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "</skill>") {
		t.Fatalf("expansion should end at the block:\n%s", got)
	}
}

// A name the catalog does not have is an error, not a message: sending the
// literal "/skill nope" to the model would waste a turn on a typo.
func TestExpandSkillErrors(t *testing.T) {
	dir := t.TempDir()
	sks := []agents.Skill{writeSkill(t, dir, "known", "body")}

	if _, _, err := expandSkill(sks, ""); err == nil {
		t.Fatal("bare /skill accepted; want a usage error")
	}
	if _, _, err := expandSkill(sks, " nope"); err == nil {
		t.Fatal("unknown skill accepted")
	}
	// The user types the name, so matching tolerates their casing.
	if _, name, err := expandSkill(sks, " KNOWN"); err != nil || name != "known" {
		t.Fatalf("case-insensitive lookup failed: name=%q err=%v", name, err)
	}
}

// Each skill registers a whole "/skills <name>" entry, so the completion row
// suggests them by prefix like any other command — and they appear only in
// agent mode.
func TestSkillCommandsCompletion(t *testing.T) {
	t.Cleanup(func() { setSkillCommands(nil); setActiveCommands(false, false, true, false) })

	setActiveCommands(true, false, true, false)
	setSkillCommands([]skillEntry{{Name: "brain-page", Description: "brain pages"}, {Name: "code-review"}})
	var joined string
	for _, c := range activeSlashCommands {
		joined += c.Value + " "
	}
	for _, want := range []string{"/skills", "/skills brain-page", "/skills code-review"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("command table missing %q: %v", want, joined)
		}
	}
	// The row shows the bare name, not the already-typed prefix.
	for _, c := range activeSlashCommands {
		if c.Value == "/skills brain-page" && c.Label != "brain-page" {
			t.Fatalf("skill entry label = %q, want the bare name", c.Label)
		}
	}

	// Outside agent mode neither the command nor its skills exist.
	setActiveCommands(false, false, true, false)
	for _, c := range activeSlashCommands {
		if strings.HasPrefix(c.Value, "/skill") {
			t.Fatalf("skill commands leaked outside agent mode: %q", c.Value)
		}
	}
}

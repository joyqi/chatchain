package chat

import (
	"strings"
	"testing"

	"github.com/joyqi/iota/internal/agents"
)

// Discovery, catalog rendering, and overlay freshness are tested in
// internal/agents; this file covers the chat-side /skills view.

func TestSkillsStatusLines(t *testing.T) {
	root := "/tmp/proj"
	sks := []agents.Skill{
		{Name: "commit-helper", Description: "Write commit messages", Path: "/tmp/proj/.agents/skills/commit-helper/SKILL.md"},
	}
	lines := skillsStatusLines(sks, []string{"bad-skill: name does not match directory"}, root)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"commit-helper", "[project]", "Write commit messages", "Skipped (invalid)", "bad-skill"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	// Empty discovery explains where it looked.
	empty := strings.Join(skillsStatusLines(nil, nil, root), "\n")
	if !strings.Contains(empty, "No skills discovered") || !strings.Contains(empty, ".agents/skills") {
		t.Fatalf("empty view unhelpful:\n%s", empty)
	}
}

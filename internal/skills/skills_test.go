package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill writes root/dir/SKILL.md (creating dir), returning the file path.
func writeSkill(t *testing.T, root, dir, content string) string {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(d, FileName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// skillMD renders a minimal valid SKILL.md for name/description.
func skillMD(name, desc string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n# Instructions\n"
}

func TestDiscoverPrecedence(t *testing.T) {
	project := t.TempDir()
	userNative := t.TempDir()
	userShared := t.TempDir()

	writeSkill(t, project, "alpha", skillMD("alpha", "project alpha"))
	writeSkill(t, userNative, "alpha", skillMD("alpha", "user alpha")) // shadowed by project
	writeSkill(t, userNative, "beta", skillMD("beta", "native beta"))
	writeSkill(t, userShared, "beta", skillMD("beta", "shared beta")) // shadowed by native
	writeSkill(t, userShared, "gamma", skillMD("gamma", "shared gamma"))

	skills, warnings := Discover([]string{project, userNative, userShared})
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(skills) != 3 {
		t.Fatalf("len(skills) = %d, want 3: %+v", len(skills), skills)
	}
	want := map[string]string{"alpha": "project alpha", "beta": "native beta", "gamma": "shared gamma"}
	for _, sk := range skills {
		if want[sk.Name] != sk.Description {
			t.Errorf("skill %s: description = %q, want %q (precedence violated)", sk.Name, sk.Description, want[sk.Name])
		}
		if !filepath.IsAbs(sk.Path) || filepath.Base(sk.Path) != FileName {
			t.Errorf("skill %s: path = %q, want an absolute SKILL.md path", sk.Name, sk.Path)
		}
		if sk.Dir() != filepath.Dir(sk.Path) {
			t.Errorf("skill %s: Dir() = %q, want %q", sk.Name, sk.Dir(), filepath.Dir(sk.Path))
		}
	}

	// Missing roots contribute nothing (and never error).
	skills, warnings = Discover([]string{filepath.Join(project, "no-such-dir")})
	if len(skills) != 0 || len(warnings) != 0 {
		t.Errorf("missing root: skills=%v warnings=%v, want none", skills, warnings)
	}
}

func TestDiscoverInvalid(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "good", skillMD("good", "a fine skill"))
	writeSkill(t, root, "Bad_Dir", skillMD("Bad_Dir", "invalid name")) // rejected with a warning
	// A directory without SKILL.md is not a candidate — silently ignored.
	if err := os.MkdirAll(filepath.Join(root, "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A stray plain file in the skills root is ignored too.
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	skills, warnings := Discover([]string{root})
	if len(skills) != 1 || skills[0].Name != "good" {
		t.Errorf("skills = %+v, want only %q", skills, "good")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "invalid name") {
		t.Errorf("warnings = %v, want one naming the invalid name", warnings)
	}
}

func TestParseSkillValidation(t *testing.T) {
	longName := strings.Repeat("a", nameMaxLen+1)
	longDesc := strings.Repeat("d", descMaxLen+1)
	cases := []struct {
		name    string
		dir     string
		content string
		wantErr string // "" = valid
	}{
		{"valid", "my-skill", skillMD("my-skill", "does things"), ""},
		{"optional fields tolerated", "extra",
			"---\nname: extra\ndescription: ok\nlicense: MIT\ncompatibility: \">=1\"\nallowed-tools:\n  - load_skill\nmetadata:\n  author: x\n---\nbody",
			""},
		{"uppercase name", "Bad", skillMD("Bad", "x"), "invalid name"},
		{"underscore in name", "bad_name", skillMD("bad_name", "x"), "invalid name"},
		{"leading hyphen", "-bad", skillMD("-bad", "x"), "invalid name"},
		{"trailing hyphen", "bad-", skillMD("bad-", "x"), "invalid name"},
		{"double hyphen", "a--b", skillMD("a--b", "x"), "invalid name"},
		{"name over max length", longName, skillMD(longName, "x"), "exceeds 64"},
		{"name mismatches directory", "real-dir", skillMD("other-name", "x"), "does not match directory"},
		{"missing name", "nameless", "---\ndescription: x\n---\n", `missing required field "name"`},
		{"missing description", "no-desc", "---\nname: no-desc\n---\n", `missing required field "description"`},
		{"description over max length", "long-desc", skillMD("long-desc", longDesc), "exceeds 1024"},
		{"no frontmatter", "plain", "# just markdown\n", "missing YAML frontmatter"},
		{"unterminated frontmatter", "open", "---\nname: open\ndescription: x\n", "unterminated"},
		{"broken YAML", "broken", "---\nname: [\n---\n", "invalid frontmatter YAML"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sk, err := parseSkill([]byte(tc.content), tc.dir, filepath.Join(os.TempDir(), tc.dir, FileName))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("parseSkill() error = %v, want valid", err)
				}
				if sk.Name != tc.dir {
					t.Errorf("Name = %q, want %q", sk.Name, tc.dir)
				}
				return
			}
			if err == nil {
				t.Fatalf("parseSkill() = %+v, want error containing %q", sk, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// Body returns the instruction text with the frontmatter consumed — what
// load_skill hands to the model on activation.
func TestBody(t *testing.T) {
	body, err := Body([]byte(skillMD("my-skill", "does things")))
	if err != nil {
		t.Fatalf("Body() error = %v", err)
	}
	if body != "# Instructions\n" {
		t.Errorf("Body() = %q, want the frontmatter stripped", body)
	}

	// Frontmatter-only file: empty body, no error.
	body, err = Body([]byte("---\nname: x\ndescription: y\n---"))
	if err != nil || body != "" {
		t.Errorf("Body(frontmatter only) = %q, %v; want empty and nil", body, err)
	}

	// CRLF endings are normalized before splitting.
	body, err = Body([]byte("---\r\nname: x\r\n---\r\nline\r\n"))
	if err != nil || !strings.Contains(body, "line") {
		t.Errorf("Body(CRLF) = %q, %v; want the body line", body, err)
	}

	if _, err = Body([]byte("no frontmatter")); err == nil {
		t.Error("Body() on a file without frontmatter should error")
	}
}

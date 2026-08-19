package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The header path ladder: each rung exists for a case the one above cannot
// serve. Pinned because a wrong rung silently degrades every file call's
// header into something the user cannot type back.
func TestHeaderPath(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "sub")
	_ = root

	home, herr := os.UserHomeDir()

	for _, tc := range []struct {
		name, in, want string
		skip           bool
	}{
		{name: "relative stays verbatim", in: "internal/ui/model.go", want: "internal/ui/model.go"},
		{name: "relative is cleaned but not rebased", in: "./a/../b.go", want: "b.go"},
		{name: "under cwd", in: filepath.Join(cwd, "a", "b.go"), want: "a/b.go"},
		{name: "cwd itself", in: cwd, want: "."},
		{name: "elsewhere in the project walks up", in: filepath.Join(root, "other", "x.go"), want: "../other/x.go"},
		{
			name: "under home outside the project",
			in:   filepath.Join(home, "elsewhere", "y.go"),
			want: "~/elsewhere/y.go",
			skip: herr != nil || home == "" || strings.HasPrefix(root, home),
		},
		{name: "empty", in: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skip {
				t.Skip("home directory unavailable or contains the temp root")
			}
			if got := headerPath(tc.in, cwd, root); got != tc.want {
				t.Fatalf("headerPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A path outside both the project and home has nowhere to be relative to.
func TestHeaderPathFallsBackToAbsolute(t *testing.T) {
	if got := headerPath("/etc/hosts", "/proj/sub", "/proj"); got != "/etc/hosts" {
		t.Fatalf("headerPath = %q, want the absolute path", got)
	}
}

// Long paths lose their HEAD: the basename identifies the file, so it is the
// part that must survive.
func TestHeaderPathElidesFromTheFront(t *testing.T) {
	long := "a/very/deeply/nested/tree/of/directories/that/keeps/going/model.go"
	got := headerPath(long, "", "")
	if len([]rune(got)) > headerPathMax {
		t.Fatalf("not elided: %q (%d cols)", got, len([]rune(got)))
	}
	if !strings.HasSuffix(got, "model.go") {
		t.Fatalf("basename lost: %q", got)
	}
	if !strings.HasPrefix(got, ".../") {
		t.Fatalf("elision marker missing: %q", got)
	}

	// One oversized segment has no separator to cut at — the tail still wins.
	huge := strings.Repeat("x", 200) + ".go"
	got = headerPath(huge, "", "")
	if len([]rune(got)) > headerPathMax {
		t.Fatalf("oversized segment not cut: %d cols", len([]rune(got)))
	}
	if !strings.HasSuffix(got, ".go") {
		t.Fatalf("extension lost: %q", got)
	}
}

// The capability is what switches the digest off, and an absent path yields a
// bare name rather than a digest of the remaining arguments — edit_file's
// new_string must never reach a header.
func TestFileToolHeaderSummary(t *testing.T) {
	cs := &codeSet{root: "/proj", cwd: "/proj"}
	edit := &codeEditFile{cs}

	if got := edit.HeaderSummary(map[string]any{
		"path":       "internal/ui/model.go",
		"old_string": "before",
		"new_string": strings.Repeat("code\n", 500),
	}); got != "internal/ui/model.go" {
		t.Fatalf("summary = %q, want the path alone", got)
	}

	if got := edit.HeaderSummary(map[string]any{"new_string": "x"}); got != "" {
		t.Fatalf("summary without a path = %q, want empty", got)
	}
}

// Registry.HeaderSummary reports capability presence, so the chat layer can
// tell "no summary" from "empty summary".
func TestRegistryHeaderSummaryCapability(t *testing.T) {
	cs := &codeSet{root: "/proj", cwd: "/proj"}
	r := &Registry{index: map[string]Tool{
		"edit_file": &codeEditFile{cs},
		"glob":      &codeGlob{cs},
	}}

	if got, ok := r.HeaderSummary("edit_file", map[string]any{"path": "a.go"}); !ok || got != "a.go" {
		t.Fatalf("edit_file = (%q, %v), want (\"a.go\", true)", got, ok)
	}
	if _, ok := r.HeaderSummary("glob", map[string]any{"pattern": "*.go"}); ok {
		t.Fatal("glob declares no summary; want ok=false so the digest applies")
	}
	if _, ok := r.HeaderSummary("nope", nil); ok {
		t.Fatal("unknown tool reported a summary")
	}
}

// For bash the command IS the call: no "command:" label, a width budget that
// fits a real pipeline, first line only, and an explicit cwd folded into the
// shell idiom for it.
func TestBashHeaderSummary(t *testing.T) {
	b := &bashTool{root: "/proj", cwd: "/proj"}

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"plain", map[string]any{"command": "git status"}, "git status"},
		{
			name: "a real pipeline survives the old 24-column budget",
			args: map[string]any{"command": "go test ./... 2>&1 | tail -20"},
			want: "go test ./... 2>&1 | tail -20",
		},
		{"trimmed", map[string]any{"command": "  ls -la  "}, "ls -la"},
		{"missing", map[string]any{}, ""},
		{
			name: "cwd folds into cd",
			args: map[string]any{"command": "make release", "cwd": "/proj/deploy"},
			want: "cd deploy && make release",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := b.HeaderSummary(tc.args); got != tc.want {
				t.Fatalf("HeaderSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

// A multi-line script cannot be read on one row: keep the first line and say
// so, rather than flattening the whole thing into a smear.
func TestHeaderCommandFirstLineAndWidth(t *testing.T) {
	got := headerCommand("npm run build\nnpm test\nnpm publish")
	if got != "npm run build …" {
		t.Fatalf("multi-line = %q", got)
	}

	long := "for f in $(find . -name '*.go'); do echo checking $f; gofmt -l $f; done"
	got = headerCommand(long)
	if w := len([]rune(got)); w > headerCmdMax {
		t.Fatalf("not truncated: %d cols", w)
	}
	if !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, "for f in") {
		t.Fatalf("truncated from the wrong end: %q", got)
	}
}

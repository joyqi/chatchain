package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// newCodeProject builds a code set over a temp project root and returns the
// root plus a tool index by name.
func newCodeProject(t *testing.T, cfgYAML string) (string, map[string]Tool) {
	t.Helper()
	root := t.TempDir()
	var node yaml.Node
	if cfgYAML != "" {
		if err := yaml.Unmarshal([]byte(cfgYAML), &node); err != nil {
			t.Fatal(err)
		}
	}
	tools, err := newCodeSet(Env{ProjectRoot: root}, node)
	if err != nil {
		t.Fatal(err)
	}
	index := make(map[string]Tool, len(tools))
	for _, tl := range tools {
		index[tl.Def().Name] = tl
	}
	return root, index
}

// writeProjectFile writes root/rel (creating parents) and returns its path.
func writeProjectFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func call(t *testing.T, tl Tool, args map[string]any) (string, bool) {
	t.Helper()
	out, isErr, err := tl.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call() hard error: %v", err)
	}
	return out, isErr
}

func TestCodePathJail(t *testing.T) {
	root, tools := newCodeProject(t, "")
	writeProjectFile(t, root, "a.txt", "hello\n")
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{outside, "../outside.txt", "a/../../outside.txt"} {
		out, isErr := call(t, tools["read_file"], map[string]any{"path": path})
		if !isErr || !strings.Contains(out, "outside the project root") {
			t.Errorf("path %q should be jailed, got (%q, %v)", path, out, isErr)
		}
	}

	// Relative and absolute-inside paths both resolve.
	if out, isErr := call(t, tools["read_file"], map[string]any{"path": "a.txt"}); isErr || !strings.Contains(out, "hello") {
		t.Fatalf("relative read failed: (%q, %v)", out, isErr)
	}
	if out, isErr := call(t, tools["read_file"], map[string]any{"path": filepath.Join(root, "a.txt")}); isErr || !strings.Contains(out, "hello") {
		t.Fatalf("absolute-inside read failed: (%q, %v)", out, isErr)
	}
}

func TestCodeGlob(t *testing.T) {
	root, tools := newCodeProject(t, "")
	old := writeProjectFile(t, root, "pkg/old.go", "package pkg\n")
	writeProjectFile(t, root, "pkg/new.go", "package pkg\n")
	writeProjectFile(t, root, "docs/readme.md", "hi\n")
	writeProjectFile(t, root, "vendor/dep.go", "package dep\n")
	writeProjectFile(t, root, ".gitignore", "vendor/\n")
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	// Bare pattern matches at any depth; gitignored vendor/ is excluded;
	// newest file first.
	out, isErr := call(t, tools["glob"], map[string]any{"pattern": "*.go"})
	if isErr {
		t.Fatalf("glob error: %s", out)
	}
	if strings.Contains(out, "vendor/dep.go") {
		t.Errorf("gitignored file leaked into glob:\n%s", out)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 2 || lines[0] != "pkg/new.go" || lines[1] != "pkg/old.go" {
		t.Errorf("glob order/content wrong: %v", lines)
	}

	// Path-anchored pattern.
	out, _ = call(t, tools["glob"], map[string]any{"pattern": "docs/*.md"})
	if out != "docs/readme.md" {
		t.Errorf("anchored glob = %q", out)
	}

	out, isErr = call(t, tools["glob"], map[string]any{"pattern": "*.rs"})
	if isErr || !strings.Contains(out, "no files match") {
		t.Errorf("no-match glob = (%q, %v)", out, isErr)
	}
}

func TestCodeGrep(t *testing.T) {
	root, tools := newCodeProject(t, "")
	writeProjectFile(t, root, "main.go", "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n")
	writeProjectFile(t, root, "notes.md", "the main idea\n")
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), []byte("ma\x00in"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, isErr := call(t, tools["grep"], map[string]any{"pattern": `func main`})
	if isErr || !strings.Contains(out, "main.go:3: func main() {") {
		t.Fatalf("grep basic = (%q, %v)", out, isErr)
	}

	// include filter narrows by basename glob; context adds - lines.
	out, _ = call(t, tools["grep"], map[string]any{"pattern": "main", "include": "*.md"})
	if strings.Contains(out, "main.go") || !strings.Contains(out, "notes.md:1: the main idea") {
		t.Errorf("include filter failed:\n%s", out)
	}
	out, _ = call(t, tools["grep"], map[string]any{"pattern": `println`, "context": float64(1)})
	if !strings.Contains(out, "main.go:3- func main() {") || !strings.Contains(out, "main.go:4: \tprintln") {
		t.Errorf("context lines missing:\n%s", out)
	}

	// Binary files are skipped silently; bad regex is a model-facing error.
	out, _ = call(t, tools["grep"], map[string]any{"pattern": "ma.?in"})
	if strings.Contains(out, "blob.bin") {
		t.Errorf("binary file leaked into grep:\n%s", out)
	}
	out, isErr = call(t, tools["grep"], map[string]any{"pattern": "("})
	if !isErr || !strings.Contains(out, "invalid regular expression") {
		t.Errorf("bad regex = (%q, %v)", out, isErr)
	}
}

func TestCodeListDir(t *testing.T) {
	root, tools := newCodeProject(t, "")
	writeProjectFile(t, root, "pkg/a.go", "x")
	writeProjectFile(t, root, "top.txt", "12345")

	out, isErr := call(t, tools["list_dir"], map[string]any{})
	if isErr || !strings.Contains(out, "pkg/") || !strings.Contains(out, "top.txt (5 B)") {
		t.Fatalf("list_dir root = (%q, %v)", out, isErr)
	}
	out, isErr = call(t, tools["list_dir"], map[string]any{"path": "nope"})
	if !isErr || !strings.Contains(out, "not a directory") {
		t.Fatalf("missing dir = (%q, %v)", out, isErr)
	}
}

func TestCodeReadFileWindow(t *testing.T) {
	root, tools := newCodeProject(t, "")
	writeProjectFile(t, root, "f.txt", "l1\nl2\nl3\nl4\nl5\n")

	out, isErr := call(t, tools["read_file"], map[string]any{"path": "f.txt"})
	if isErr || !strings.Contains(out, "     1\tl1") || !strings.Contains(out, "     5\tl5") {
		t.Fatalf("numbered read = (%q, %v)", out, isErr)
	}

	out, _ = call(t, tools["read_file"], map[string]any{"path": "f.txt", "offset": float64(2), "limit": float64(2)})
	if !strings.Contains(out, "     2\tl2") || strings.Contains(out, "l4") || !strings.Contains(out, "[showing lines 2-3 of 5]") {
		t.Fatalf("window wrong:\n%s", out)
	}

	out, isErr = call(t, tools["read_file"], map[string]any{"path": "f.txt", "offset": float64(9)})
	if !isErr || !strings.Contains(out, "past the end") {
		t.Fatalf("offset past end = (%q, %v)", out, isErr)
	}

	writeProjectFile(t, root, "empty.txt", "")
	if out, isErr := call(t, tools["read_file"], map[string]any{"path": "empty.txt"}); isErr || out != "[file is empty]" {
		t.Fatalf("empty read = (%q, %v)", out, isErr)
	}

	if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte("a\x00b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, isErr := call(t, tools["read_file"], map[string]any{"path": "bin.dat"}); !isErr || !strings.Contains(out, "binary") {
		t.Fatalf("binary read = (%q, %v)", out, isErr)
	}
}

func TestCodeEditFile(t *testing.T) {
	root, tools := newCodeProject(t, "")
	writeProjectFile(t, root, "f.go", "aaa\nbbb\naaa\n")

	// Read-before-edit: unread file is rejected.
	out, isErr := call(t, tools["edit_file"], map[string]any{"path": "f.go", "old_string": "bbb", "new_string": "BBB"})
	if !isErr || !strings.Contains(out, "read it with read_file") {
		t.Fatalf("unread edit = (%q, %v)", out, isErr)
	}
	call(t, tools["read_file"], map[string]any{"path": "f.go"})

	// Ambiguous old_string needs replace_all or more context.
	out, isErr = call(t, tools["edit_file"], map[string]any{"path": "f.go", "old_string": "aaa", "new_string": "AAA"})
	if !isErr || !strings.Contains(out, "appears 2 times") {
		t.Fatalf("ambiguous edit = (%q, %v)", out, isErr)
	}

	// Unique replacement succeeds, reports a numbered snippet, stays fresh.
	out, isErr = call(t, tools["edit_file"], map[string]any{"path": "f.go", "old_string": "bbb", "new_string": "BBB"})
	if isErr || !strings.Contains(out, "1 replacement(s) in f.go") || !strings.Contains(out, "2\tBBB") {
		t.Fatalf("edit = (%q, %v)", out, isErr)
	}
	data, _ := os.ReadFile(filepath.Join(root, "f.go"))
	if string(data) != "aaa\nBBB\naaa\n" {
		t.Fatalf("file content = %q", data)
	}

	// replace_all handles both occurrences without a re-read (the edit itself
	// refreshed the ledger).
	out, isErr = call(t, tools["edit_file"], map[string]any{"path": "f.go", "old_string": "aaa", "new_string": "xxx", "replace_all": true})
	if isErr || !strings.Contains(out, "2 replacement(s)") {
		t.Fatalf("replace_all = (%q, %v)", out, isErr)
	}

	// Not-found and no-op edits are model-facing errors.
	out, isErr = call(t, tools["edit_file"], map[string]any{"path": "f.go", "old_string": "zzz", "new_string": "y"})
	if !isErr || !strings.Contains(out, "not found") {
		t.Fatalf("not-found edit = (%q, %v)", out, isErr)
	}
	out, isErr = call(t, tools["edit_file"], map[string]any{"path": "f.go", "old_string": "xxx", "new_string": "xxx"})
	if !isErr || !strings.Contains(out, "identical") {
		t.Fatalf("no-op edit = (%q, %v)", out, isErr)
	}

	// External modification after the read invalidates the ledger.
	path := filepath.Join(root, "f.go")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	out, isErr = call(t, tools["edit_file"], map[string]any{"path": "f.go", "old_string": "xxx", "new_string": "y"})
	if !isErr || !strings.Contains(out, "changed on disk") {
		t.Fatalf("stale edit = (%q, %v)", out, isErr)
	}
}

func TestCodeWriteFile(t *testing.T) {
	root, tools := newCodeProject(t, "")

	// New file: no prior read needed, parents created.
	out, isErr := call(t, tools["write_file"], map[string]any{"path": "new/dir/f.txt", "content": "hello"})
	if isErr || !strings.Contains(out, "created") {
		t.Fatalf("create = (%q, %v)", out, isErr)
	}
	data, err := os.ReadFile(filepath.Join(root, "new", "dir", "f.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("written content = %q, %v", data, err)
	}

	// Overwrite without reading first is rejected; the write above counts as
	// the read (the ledger tracks the tool's own writes).
	writeProjectFile(t, root, "other.txt", "x")
	out, isErr = call(t, tools["write_file"], map[string]any{"path": "other.txt", "content": "y"})
	if !isErr || !strings.Contains(out, "read it with read_file") {
		t.Fatalf("unread overwrite = (%q, %v)", out, isErr)
	}
	out, isErr = call(t, tools["write_file"], map[string]any{"path": "new/dir/f.txt", "content": "hello2"})
	if isErr || !strings.Contains(out, "overwritten") {
		t.Fatalf("tracked overwrite = (%q, %v)", out, isErr)
	}

	out, isErr = call(t, tools["write_file"], map[string]any{"path": "z.txt"})
	if !isErr || !strings.Contains(out, "missing required argument: content") {
		t.Fatalf("missing content = (%q, %v)", out, isErr)
	}
}

// The mutating tools require approval by default; auto_write waives it. The
// read-only tools never require it.
func TestCodeApproval(t *testing.T) {
	reg := Build(Env{ProjectRoot: t.TempDir()}, rawTools(t, "code:\n"), nil)
	for name, want := range map[string]bool{
		"edit_file": true, "write_file": true,
		"read_file": false, "glob": false, "grep": false, "list_dir": false,
	} {
		if got := reg.RequiresApproval(name); got != want {
			t.Errorf("RequiresApproval(%s) = %v, want %v", name, got, want)
		}
	}

	auto := Build(Env{ProjectRoot: t.TempDir()}, rawTools(t, "code:\n  auto_write: true\n"), nil)
	if auto.RequiresApproval("write_file") || auto.RequiresApproval("edit_file") {
		t.Error("auto_write should waive approval")
	}

	// The capability routes through Merge, and unknown tools never require it.
	merged := Merge(reg, nil)
	ar, ok := merged.(ApprovalReporter)
	if !ok {
		t.Fatal("merged dispatcher should implement ApprovalReporter")
	}
	if !ar.RequiresApproval("write_file") || ar.RequiresApproval("read_file") || ar.RequiresApproval("nope") {
		t.Error("merged approval routing wrong")
	}
}

// edit_file and write_file post their unified diff through the artifact side
// channel — display-only, never part of the model-facing result text.
func TestMutationsPostDiffArtifact(t *testing.T) {
	root, tools := newCodeProject(t, "")
	writeProjectFile(t, root, "a.txt", "one\ntwo\nthree\n")

	call(t, tools["read_file"], map[string]any{"path": "a.txt"}) // edit needs a fresh read
	ctx, collect := WithArtifact(context.Background())
	out, isErr, err := tools["edit_file"].Call(ctx, map[string]any{
		"path": "a.txt", "old_string": "two", "new_string": "2",
	})
	if err != nil || isErr {
		t.Fatalf("edit failed: %q %v", out, err)
	}
	art := collect()
	if art == nil || art.Kind != "diff" || art.Title != "a.txt" {
		t.Fatalf("edit_file must post a diff artifact, got %+v", art)
	}
	joined := strings.Join(art.Lines, "\n")
	if !strings.Contains(joined, "-two") || !strings.Contains(joined, "+2") {
		t.Fatalf("diff content wrong:\n%s", joined)
	}
	if strings.Contains(joined, "--- ") || strings.Contains(joined, "+++ ") {
		t.Fatalf("file header rows must be stripped:\n%s", joined)
	}
	if strings.Contains(out, "@@") {
		t.Fatalf("the diff must not leak into the model-facing result:\n%s", out)
	}

	// Creating a new file diffs against empty content: all additions.
	ctx, collect = WithArtifact(context.Background())
	out, isErr, err = tools["write_file"].Call(ctx, map[string]any{
		"path": "new.txt", "content": "alpha\nbeta\n",
	})
	if err != nil || isErr {
		t.Fatalf("write failed: %q %v", out, err)
	}
	if art = collect(); art == nil {
		t.Fatal("write_file must post a diff artifact for a new file")
	}
	joined = strings.Join(art.Lines, "\n")
	if !strings.Contains(joined, "+alpha") || !strings.Contains(joined, "+beta") {
		t.Fatalf("creation diff must be all additions:\n%s", joined)
	}

	// Overwriting: the diff runs old → new.
	call(t, tools["read_file"], map[string]any{"path": "new.txt"})
	ctx, collect = WithArtifact(context.Background())
	if out, isErr, err = tools["write_file"].Call(ctx, map[string]any{
		"path": "new.txt", "content": "alpha\ngamma\n",
	}); err != nil || isErr {
		t.Fatalf("overwrite failed: %q %v", out, err)
	}
	joined = strings.Join(collect().Lines, "\n")
	if !strings.Contains(joined, "-beta") || !strings.Contains(joined, "+gamma") {
		t.Fatalf("overwrite diff wrong:\n%s", joined)
	}
}

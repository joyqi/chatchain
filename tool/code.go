package tool

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"chatchain/provider"

	"github.com/aymanbagabas/go-udiff"
	"github.com/bmatcuk/doublestar/v4"
	ignore "github.com/sabhiram/go-gitignore"
	"gopkg.in/yaml.v3"
)

// The "code" toolset: the locate → read → edit loop for programming
// (docs/design/code-toolset.md). Verification runs through the command set's
// bash — this set never executes anything. All paths are jailed to the
// project root; the mutating tools (edit_file, write_file) require
// interactive approval unless auto_write is configured, and refuse to touch a
// file the model has not read in its current on-disk state.

const (
	codeMaxFileBytes     = 20 * 1024 * 1024 // read/edit cap per file
	codeMaxOutput        = 64 * 1024        // output cap per call
	codeMaxGlobResults   = 200              // glob matches returned (newest first)
	codeGlobCollectCap   = 10000            // glob matches examined before sorting
	codeMaxGrepMatches   = 100              // grep matching lines returned
	codeGrepMaxFileBytes = 10 * 1024 * 1024 // grep skips files larger than this
	codeGrepMaxLineLen   = 500              // grep truncates longer lines
	codeMaxGrepContext   = 10               // grep context lines per side
	codeMaxDirEntries    = 500              // list_dir entries returned
	codeBinarySniffLen   = 8000             // leading bytes checked for NUL
)

// codeConfig is the set's shared config.
type codeConfig struct {
	// AutoWrite skips the interactive approval for edit_file/write_file (and
	// permits them in non-interactive -m runs).
	AutoWrite bool `yaml:"auto_write"`
}

// newCodeSet builds the "code" toolset.
func newCodeSet(env Env, node yaml.Node) ([]Tool, error) {
	var cfg codeConfig
	if !node.IsZero() {
		if err := node.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("config must be a mapping (auto_write): %w", err)
		}
	}
	cwd, _ := os.Getwd()
	cs := &codeSet{root: env.Root(), cwd: cwd, autoWrite: cfg.AutoWrite, reads: make(map[string]time.Time)}
	return []Tool{
		&codeGlob{cs},
		&codeGrep{cs},
		&codeListDir{cs},
		&codeReadFile{cs},
		&codeEditFile{cs},
		&codeWriteFile{cs},
	}, nil
}

// codeSet is the state shared by the set's tools for one session: the jail
// root and the read ledger backing the read-before-edit rule.
type codeSet struct {
	root string
	// cwd is where the process was started, captured once: the display
	// anchor for header paths. It is NOT the jail (root is) — a run started
	// in a subdirectory of the project shows paths the way the user would
	// type them, while still reaching the whole project.
	cwd       string
	autoWrite bool

	mu    sync.Mutex
	reads map[string]time.Time // abs path → mtime when the model last read it
}

// resolve jails a model-supplied path: relative paths resolve against the
// project root, absolute paths must stay inside it.
func (cs *codeSet) resolve(arg, argName string) (string, string) {
	p := strings.TrimSpace(arg)
	if p == "" {
		return "", "missing required argument: " + argName
	}
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(cs.root, p)
	}
	p = filepath.Clean(p)
	rel, err := filepath.Rel(cs.root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Sprintf("path is outside the project root (%s): %s", cs.root, arg)
	}
	return p, ""
}

// display renders an absolute path root-relative for model-facing output.
func (cs *codeSet) display(abs string) string {
	rel, err := filepath.Rel(cs.root, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

// noteRead records the file's current mtime as "read by the model".
func (cs *codeSet) noteRead(abs string) {
	if fi, err := os.Stat(abs); err == nil {
		cs.mu.Lock()
		cs.reads[abs] = fi.ModTime()
		cs.mu.Unlock()
	}
}

// requireFreshRead enforces read-before-edit: the file must have been read in
// this session AND be unchanged on disk since. Returns a model-facing error
// string, empty when the write may proceed.
func (cs *codeSet) requireFreshRead(abs string) string {
	cs.mu.Lock()
	stamp, ok := cs.reads[abs]
	cs.mu.Unlock()
	if !ok {
		return fmt.Sprintf("%s has not been read in this session — read it with read_file before modifying it", cs.display(abs))
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return fmt.Sprintf("cannot access %s: %v", cs.display(abs), err)
	}
	if !fi.ModTime().Equal(stamp) {
		return fmt.Sprintf("%s changed on disk after it was read — read it again before modifying it", cs.display(abs))
	}
	return ""
}

// ignoreMatcher compiles the project root's .gitignore, re-read per call so
// edits to it apply immediately. Nil when absent or unreadable.
func (cs *codeSet) ignoreMatcher() *ignore.GitIgnore {
	gi, err := ignore.CompileIgnoreFile(filepath.Join(cs.root, ".gitignore"))
	if err != nil {
		return nil
	}
	return gi
}

// walkFiles walks base, skipping .git and root-.gitignore matches, and calls
// visit for every file. visit returns false to stop the walk (cap reached).
func (cs *codeSet) walkFiles(base string, visit func(abs, relRoot string, d fs.DirEntry) bool) {
	gi := cs.ignoreMatcher()
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil // unreadable entries are skipped, never fatal
		}
		rel, rerr := filepath.Rel(cs.root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			if p != base && gi != nil && gi.MatchesPath(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if gi != nil && gi.MatchesPath(rel) {
			return nil
		}
		if !visit(p, rel, d) {
			return filepath.SkipAll
		}
		return nil
	})
}

// looksBinary sniffs for a NUL byte in the leading bytes, the same heuristic
// grep/ripgrep use.
func looksBinary(data []byte) bool {
	n := len(data)
	if n > codeBinarySniffLen {
		n = codeBinarySniffLen
	}
	return bytes.IndexByte(data[:n], 0) >= 0
}

// byteCount renders a compact human size.
func byteCount(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// ---- glob ----

type codeGlob struct{ cs *codeSet }

func (t *codeGlob) Def() provider.ToolDef {
	return provider.ToolDef{
		Name: "glob",
		Description: "Find files by name pattern under the project root. Patterns match root-relative paths " +
			"and support * ? and ** (a pattern without \"/\" matches at any depth, e.g. \"*.go\"). Results " +
			"are newest-first. .git and root-.gitignore matches are excluded.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Glob pattern, e.g. \"**/*.go\" or \"cmd/*.go\".",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Optional directory to search, relative to the project root (default: the root).",
				},
			},
			"required": []any{"pattern"},
		},
	}
}

func (t *codeGlob) Call(_ context.Context, args map[string]any) (string, bool, error) {
	pattern, _ := args["pattern"].(string)
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "missing required argument: pattern", true, nil
	}
	if !strings.Contains(pattern, "/") {
		pattern = "**/" + pattern
	}
	if !doublestar.ValidatePattern(pattern) {
		return fmt.Sprintf("invalid glob pattern: %s", pattern), true, nil
	}
	base, errText := t.baseDir(args)
	if errText != "" {
		return errText, true, nil
	}

	type match struct {
		rel   string
		mtime time.Time
	}
	var matches []match
	t.cs.walkFiles(base, func(abs, relRoot string, d fs.DirEntry) bool {
		if ok, _ := doublestar.Match(pattern, relRoot); ok {
			var mtime time.Time
			if fi, err := d.Info(); err == nil {
				mtime = fi.ModTime()
			}
			matches = append(matches, match{rel: relRoot, mtime: mtime})
		}
		return len(matches) < codeGlobCollectCap
	})

	if len(matches) == 0 {
		return fmt.Sprintf("no files match %s", pattern), false, nil
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].mtime.After(matches[j].mtime) })
	shown := matches
	if len(shown) > codeMaxGlobResults {
		shown = shown[:codeMaxGlobResults]
	}
	var b strings.Builder
	for _, m := range shown {
		b.WriteString(m.rel)
		b.WriteByte('\n')
	}
	if len(matches) > len(shown) {
		fmt.Fprintf(&b, "[showing the %d newest of %d matches; narrow the pattern to see the rest]", len(shown), len(matches))
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

// baseDir resolves the optional "path" argument to the search base.
func (t *codeGlob) baseDir(args map[string]any) (string, string) {
	return codeBaseDir(t.cs, args)
}

func codeBaseDir(cs *codeSet, args map[string]any) (string, string) {
	p, _ := args["path"].(string)
	if strings.TrimSpace(p) == "" {
		return cs.root, ""
	}
	abs, errText := cs.resolve(p, "path")
	if errText != "" {
		return "", errText
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.IsDir() {
		return "", fmt.Sprintf("not a directory: %s", p)
	}
	return abs, ""
}

// ---- grep ----

type codeGrep struct{ cs *codeSet }

func (t *codeGrep) Def() provider.ToolDef {
	return provider.ToolDef{
		Name: "grep",
		Description: "Search file contents under the project root with a Go regular expression (RE2). " +
			"Output lines are \"path:line: text\" (context lines use \"-\" instead of \":\"). Binary files, " +
			".git, and root-.gitignore matches are skipped.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Regular expression to search for (Go/RE2 syntax).",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Optional directory to search, relative to the project root (default: the root).",
				},
				"include": map[string]any{
					"type":        "string",
					"description": "Optional filename glob filter, e.g. \"*.go\" or \"cmd/**\".",
				},
				"context": map[string]any{
					"type":        "integer",
					"description": "Lines of context to show around each match (0-10, default 0).",
				},
			},
			"required": []any{"pattern"},
		},
	}
}

func (t *codeGrep) Call(_ context.Context, args map[string]any) (string, bool, error) {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return "missing required argument: pattern", true, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Sprintf("invalid regular expression: %v", err), true, nil
	}
	base, errText := codeBaseDir(t.cs, args)
	if errText != "" {
		return errText, true, nil
	}
	include := strings.TrimSpace(func() string { s, _ := args["include"].(string); return s }())
	if include != "" && !doublestar.ValidatePattern(include) {
		return fmt.Sprintf("invalid include pattern: %s", include), true, nil
	}
	ctxLines := intArg(args, "context")
	if ctxLines < 0 {
		ctxLines = 0
	}
	if ctxLines > codeMaxGrepContext {
		ctxLines = codeMaxGrepContext
	}

	var b strings.Builder
	total := 0
	capped := false
	t.cs.walkFiles(base, func(abs, relRoot string, d fs.DirEntry) bool {
		if include != "" && !matchInclude(include, relRoot) {
			return true
		}
		if fi, ierr := d.Info(); ierr != nil || fi.Size() > codeGrepMaxFileBytes {
			return true
		}
		data, _, rerrText := readFileLimited(abs, codeGrepMaxFileBytes)
		if rerrText != "" || looksBinary(data) {
			return true
		}
		lines := strings.Split(string(data), "\n")
		var hits []int
		for i, line := range lines {
			if re.MatchString(line) {
				hits = append(hits, i)
				if total+len(hits) >= codeMaxGrepMatches {
					break
				}
			}
		}
		if len(hits) == 0 {
			return true
		}
		total += len(hits)
		emitGrepFile(&b, relRoot, lines, hits, ctxLines)
		if total >= codeMaxGrepMatches || b.Len() > codeMaxOutput {
			capped = true
			return false
		}
		return true
	})

	if total == 0 {
		return fmt.Sprintf("no matches for %s", pattern), false, nil
	}
	out := strings.TrimRight(b.String(), "\n")
	if capped {
		out += fmt.Sprintf("\n[stopped after %d matches; refine the pattern or use include to narrow the search]", total)
	}
	return out, false, nil
}

// matchInclude applies the grep include filter: a pattern without "/" matches
// the basename, otherwise the root-relative path.
func matchInclude(pattern, relRoot string) bool {
	target := relRoot
	if !strings.Contains(pattern, "/") {
		target = relRoot[strings.LastIndex(relRoot, "/")+1:]
	}
	ok, _ := doublestar.Match(pattern, target)
	return ok
}

// emitGrepFile prints one file's matches with optional context, rg-style.
func emitGrepFile(b *strings.Builder, rel string, lines []string, hits []int, ctxLines int) {
	isHit := make(map[int]bool, len(hits))
	show := make(map[int]bool)
	for _, h := range hits {
		isHit[h] = true
		for i := h - ctxLines; i <= h+ctxLines; i++ {
			if i >= 0 && i < len(lines) {
				show[i] = true
			}
		}
	}
	order := make([]int, 0, len(show))
	for i := range show {
		order = append(order, i)
	}
	sort.Ints(order)
	for _, i := range order {
		line := lines[i]
		if len(line) > codeGrepMaxLineLen {
			line = truncateToRuneBoundary(line, codeGrepMaxLineLen) + "…"
		}
		sep := "-"
		if isHit[i] {
			sep = ":"
		}
		fmt.Fprintf(b, "%s:%d%s %s\n", rel, i+1, sep, line)
	}
}

// ---- list_dir ----

type codeListDir struct{ cs *codeSet }

func (t *codeListDir) Def() provider.ToolDef {
	return provider.ToolDef{
		Name: "list_dir",
		Description: "List one directory level under the project root: directories with a trailing \"/\", " +
			"files with their size. Defaults to the project root itself.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Directory to list, relative to the project root (default: the root).",
				},
			},
		},
	}
}

func (t *codeListDir) Call(_ context.Context, args map[string]any) (string, bool, error) {
	base, errText := codeBaseDir(t.cs, args)
	if errText != "" {
		return errText, true, nil
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return fmt.Sprintf("cannot list %s: %v", t.cs.display(base), err), true, nil
	}
	if len(entries) == 0 {
		return "[directory is empty]", false, nil
	}
	var b strings.Builder
	shown := 0
	for _, e := range entries {
		if shown == codeMaxDirEntries {
			fmt.Fprintf(&b, "[showing %d of %d entries]", shown, len(entries))
			break
		}
		if e.IsDir() {
			b.WriteString(e.Name() + "/\n")
		} else if fi, ierr := e.Info(); ierr == nil {
			fmt.Fprintf(&b, "%s (%s)\n", e.Name(), byteCount(fi.Size()))
		} else {
			b.WriteString(e.Name() + "\n")
		}
		shown++
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

// ---- read_file ----

type codeReadFile struct{ cs *codeSet }

func (t *codeReadFile) Def() provider.ToolDef {
	return provider.ToolDef{
		Name: "read_file",
		Description: "Read a text file inside the project root and return its content with line numbers, " +
			"windowed by the optional \"offset\" (1-based first line) and \"limit\" (line count). Paths are " +
			"relative to the project root. Reading a file is required before editing it.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path relative to the project root.",
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

func (t *codeReadFile) Call(_ context.Context, args map[string]any) (string, bool, error) {
	arg, _ := args["path"].(string)
	abs, errText := t.cs.resolve(arg, "path")
	if errText != "" {
		return errText, true, nil
	}
	data, size, errText := readFileLimited(abs, codeMaxFileBytes)
	if errText != "" {
		return errText, true, nil
	}
	if looksBinary(data) {
		return fmt.Sprintf("%s looks like a binary file (%s); read_file only serves text", t.cs.display(abs), byteCount(size)), true, nil
	}
	t.cs.noteRead(abs)
	if len(data) == 0 {
		return "[file is empty]", false, nil
	}

	out, errText := numberedWindow(string(data), args, t.cs.display(abs))
	if errText != "" {
		return errText, true, nil
	}
	if size > codeMaxFileBytes {
		out += fmt.Sprintf("\n[file is %s; only the first %d MB was read]", byteCount(size), codeMaxFileBytes/(1024*1024))
	}
	return out, false, nil
}

// numberedWindow renders content as "   N\tline" rows through the offset/limit
// window, keeping the output under codeMaxOutput with a continuation marker.
func numberedWindow(content string, args map[string]any, display string) (string, string) {
	lines := splitLines(content)
	total := len(lines)
	start := intArg(args, "offset")
	if start < 1 {
		start = 1
	}
	if start > total {
		return "", fmt.Sprintf("offset %d is past the end of %s (%d lines)", start, display, total)
	}
	end := total
	if limit := intArg(args, "limit"); limit > 0 && start-1+limit < end {
		end = start - 1 + limit
	}

	var b strings.Builder
	last := start - 1 // last line number actually emitted
	for i := start - 1; i < end; i++ {
		row := fmt.Sprintf("%6d\t%s\n", i+1, lines[i])
		if len(row) > codeMaxOutput {
			// A single pathological line: serve what fits rather than nothing.
			row = truncateToRuneBoundary(row, codeMaxOutput) + "…\n"
		}
		if b.Len()+len(row) > codeMaxOutput {
			break
		}
		b.WriteString(row)
		last = i + 1
	}
	out := strings.TrimRight(b.String(), "\n")
	if last < end {
		out += fmt.Sprintf("\n[output truncated — showing lines %d-%d of %d; call read_file with offset=%d to continue]",
			start, last, total, last+1)
	} else if start > 1 || end < total {
		out += fmt.Sprintf("\n[showing lines %d-%d of %d]", start, end, total)
	}
	return out, ""
}

// ---- edit_file ----

type codeEditFile struct{ cs *codeSet }

// SupportsParallel: the read-only file tools touch nothing and ask nothing,
// so a round that reads four files can read them at once. edit_file and
// write_file are deliberately absent — they write — and so is glob's and
// grep's mutating sibling set, which does not exist.
//
// These four answer the same for every call, so the arguments go unread. The
// parameter exists for tools whose calls differ in kind (see parallelizer);
// reading it here would only invite a per-call exception none of them wants.
func (t *codeReadFile) SupportsParallel(map[string]any) bool { return true }
func (t *codeGlob) SupportsParallel(map[string]any) bool     { return true }
func (t *codeGrep) SupportsParallel(map[string]any) bool     { return true }
func (t *codeListDir) SupportsParallel(map[string]any) bool  { return true }

// HeaderSummary: the path IS the call for the file tools — the header reads
// "[edit_file internal/ui/model.go]". edit_file and write_file especially
// must own this: their new_string / content arguments are whole files, and
// the generic digest would paste one into the header.
func (t *codeReadFile) HeaderSummary(args map[string]any) string  { return t.cs.headerArg(args) }
func (t *codeEditFile) HeaderSummary(args map[string]any) string  { return t.cs.headerArg(args) }
func (t *codeWriteFile) HeaderSummary(args map[string]any) string { return t.cs.headerArg(args) }
func (t *codeListDir) HeaderSummary(args map[string]any) string   { return t.cs.headerArg(args) }

// headerArg renders the "path" argument for a header. A missing path (a
// malformed call) yields "" — a bare "[read_file]" — rather than falling
// back to a digest of whatever else the model sent.
func (cs *codeSet) headerArg(args map[string]any) string {
	p, _ := args["path"].(string)
	return headerPath(p, cs.cwd, cs.root)
}

func (t *codeEditFile) RequiresApproval() bool { return !t.cs.autoWrite }

// Presentation: a mutation's outcome deserves the expanded standalone block —
// the posted diff is what the user reviews.
func (t *codeEditFile) Presentation() Presentation { return PresentExpanded }

// postDiff computes the unified diff between old and new content and posts
// it as the call's display artifact. Hunks only: the ---/+++ file header
// would duplicate the title, and the display is for the user's eyes — the
// model-facing result text stays untouched (a full diff there costs tokens).
func postDiff(ctx context.Context, display, old, new string) {
	unified := udiff.Unified(display, display, old, new)
	lines := splitLines(unified)
	for len(lines) > 0 && (strings.HasPrefix(lines[0], "--- ") || strings.HasPrefix(lines[0], "+++ ")) {
		lines = lines[1:]
	}
	if len(lines) == 0 {
		return
	}
	PostArtifact(ctx, Artifact{Kind: "diff", Title: display, Lines: lines})
}

func (t *codeEditFile) Def() provider.ToolDef {
	return provider.ToolDef{
		Name: "edit_file",
		Description: "Replace an exact string in a file inside the project root. \"old_string\" must match " +
			"the file content exactly (including whitespace and indentation) and must be unique in the file " +
			"unless \"replace_all\" is set — extend it with surrounding lines to disambiguate. The file must " +
			"have been read with read_file first.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path relative to the project root.",
				},
				"old_string": map[string]any{
					"type":        "string",
					"description": "Exact text to replace.",
				},
				"new_string": map[string]any{
					"type":        "string",
					"description": "Replacement text.",
				},
				"replace_all": map[string]any{
					"type":        "boolean",
					"description": "Replace every occurrence instead of requiring uniqueness (default false).",
				},
			},
			"required": []any{"path", "old_string", "new_string"},
		},
	}
}

func (t *codeEditFile) Call(ctx context.Context, args map[string]any) (string, bool, error) {
	arg, _ := args["path"].(string)
	abs, errText := t.cs.resolve(arg, "path")
	if errText != "" {
		return errText, true, nil
	}
	oldStr, _ := args["old_string"].(string)
	newStr, _ := args["new_string"].(string)
	replaceAll := boolArg(args, "replace_all", false)
	display := t.cs.display(abs)
	switch {
	case oldStr == "":
		return "old_string must not be empty (use write_file to create or replace a whole file)", true, nil
	case oldStr == newStr:
		return "old_string and new_string are identical", true, nil
	}
	if errText := t.cs.requireFreshRead(abs); errText != "" {
		return errText, true, nil
	}

	fi, err := os.Stat(abs)
	if err != nil {
		return fmt.Sprintf("cannot access %s: %v", display, err), true, nil
	}
	if fi.Size() > codeMaxFileBytes {
		return fmt.Sprintf("%s is too large to edit (%s)", display, byteCount(fi.Size())), true, nil
	}
	data, _, errText := readFileLimited(abs, codeMaxFileBytes)
	if errText != "" {
		return errText, true, nil
	}
	content := string(data)

	count := strings.Count(content, oldStr)
	switch {
	case count == 0:
		return fmt.Sprintf("old_string not found in %s — it must match the file content exactly, whitespace included; read the file again if unsure", display), true, nil
	case count > 1 && !replaceAll:
		return fmt.Sprintf("old_string appears %d times in %s; extend it with surrounding context to make it unique, or set replace_all", count, display), true, nil
	}

	n := 1
	if replaceAll {
		n = -1
	}
	updated := strings.Replace(content, oldStr, newStr, n)
	if err := os.WriteFile(abs, []byte(updated), fi.Mode().Perm()); err != nil {
		return fmt.Sprintf("cannot write %s: %v", display, err), true, nil
	}
	t.cs.noteRead(abs)
	postDiff(ctx, display, content, updated)

	done := count
	if !replaceAll {
		done = 1
	}
	// The first replacement lands at old_string's original offset.
	line := strings.Count(content[:strings.Index(content, oldStr)], "\n") + 1
	return fmt.Sprintf("%d replacement(s) in %s\n\n%s", done, display, editSnippet(updated, line)), false, nil
}

// editSnippet shows a few numbered lines around the first change so the model
// can verify the edit landed as intended.
func editSnippet(content string, line int) string {
	lines := splitLines(content)
	from := line - 3
	if from < 1 {
		from = 1
	}
	to := line + 3
	if to > len(lines) {
		to = len(lines)
	}
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i, lines[i-1])
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---- write_file ----

type codeWriteFile struct{ cs *codeSet }

func (t *codeWriteFile) RequiresApproval() bool { return !t.cs.autoWrite }

// Presentation: see codeEditFile.Presentation.
func (t *codeWriteFile) Presentation() Presentation { return PresentExpanded }

func (t *codeWriteFile) Def() provider.ToolDef {
	return provider.ToolDef{
		Name: "write_file",
		Description: "Create or overwrite a whole file inside the project root (parent directories are " +
			"created). Overwriting an existing file requires reading it with read_file first — prefer " +
			"edit_file for changes inside an existing file.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path relative to the project root.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "Full file content to write.",
				},
			},
			"required": []any{"path", "content"},
		},
	}
}

func (t *codeWriteFile) Call(ctx context.Context, args map[string]any) (string, bool, error) {
	arg, _ := args["path"].(string)
	abs, errText := t.cs.resolve(arg, "path")
	if errText != "" {
		return errText, true, nil
	}
	content, ok := args["content"].(string)
	if !ok {
		return "missing required argument: content", true, nil
	}
	display := t.cs.display(abs)

	perm := os.FileMode(0o644)
	created := true
	old := ""
	oldOK := true // false: the previous content is unknowable, skip the diff
	if fi, err := os.Stat(abs); err == nil {
		if fi.IsDir() {
			return fmt.Sprintf("%s is a directory", display), true, nil
		}
		if !fi.Mode().IsRegular() {
			return fmt.Sprintf("%s is not a regular file", display), true, nil
		}
		if errText := t.cs.requireFreshRead(abs); errText != "" {
			return errText, true, nil
		}
		perm = fi.Mode().Perm()
		created = false
		// The previous content only feeds the display diff; a file too large
		// to read whole would produce a lying diff, so it produces none.
		if fi.Size() <= codeMaxFileBytes {
			if data, _, errText := readFileLimited(abs, codeMaxFileBytes); errText == "" {
				old = string(data)
			} else {
				oldOK = false
			}
		} else {
			oldOK = false
		}
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Sprintf("cannot create parent directory for %s: %v", display, err), true, nil
	}
	if err := os.WriteFile(abs, []byte(content), perm); err != nil {
		return fmt.Sprintf("cannot write %s: %v", display, err), true, nil
	}
	t.cs.noteRead(abs)
	if oldOK {
		postDiff(ctx, display, old, content)
	}

	verb := "created"
	if !created {
		verb = "overwritten"
	}
	return fmt.Sprintf("wrote %s to %s (%s)", byteCount(int64(len(content))), display, verb), false, nil
}

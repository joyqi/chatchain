package chat

import (
	"strings"
	"testing"
	"time"

	"chatchain/provider"
)

func TestDetectExportFormat(t *testing.T) {
	tests := []struct {
		in     string
		path   string
		format exportFormat
	}{
		{"notes.md", "notes.md", exportMarkdown},
		{"NOTES.MD", "NOTES.MD", exportMarkdown},
		{"log.markdown", "log.markdown", exportMarkdown},
		{"Log.Markdown", "Log.Markdown", exportMarkdown},
		{"report", "report.html", exportHTML},
		{"page.html", "page.html", exportHTML},
		{"Page.HTML", "Page.HTML", exportHTML},
		{"data.txt", "data.txt", exportHTML}, // unknown extension keeps the name, exports HTML
		{"dir/nested", "dir/nested.html", exportHTML},
	}
	for _, tt := range tests {
		path, format := detectExportFormat(tt.in)
		if path != tt.path || format != tt.format {
			t.Errorf("detectExportFormat(%q) = (%q, %v), want (%q, %v)", tt.in, path, format, tt.path, tt.format)
		}
	}
}

func TestSlugify(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Hello, World!", "hello-world"},
		{"  spaces   and\tpunct?! ", "spaces-and-punct"},
		{"already-fine", "already-fine"},
		{"...", ""},
		{"", ""},
		{"中文 标题", "中文-标题"}, // unicode letters survive
	}
	for _, tt := range tests {
		if got := slugify(tt.in); got != tt.want {
			t.Errorf("slugify(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}

	// Long titles are capped (~40 runes) with no trailing dash.
	long := slugify(strings.Repeat("word ", 20))
	if n := len([]rune(long)); n > exportSlugMax {
		t.Errorf("slug length %d exceeds cap %d", n, exportSlugMax)
	}
	if strings.HasSuffix(long, "-") {
		t.Errorf("capped slug has a trailing dash: %q", long)
	}
}

func TestExportFileName(t *testing.T) {
	now := time.Date(2026, 7, 7, 9, 30, 15, 0, time.UTC)

	if got := exportFileName("Fix the Build!", "k7qz3xv9m2ht", exportHTML, now); got != "chatchain-fix-the-build-20260707-093015.html" {
		t.Errorf("title slug: got %q", got)
	}
	// Empty title falls back to the session id.
	if got := exportFileName("", "k7qz3xv9m2ht", exportMarkdown, now); got != "chatchain-k7qz3xv9m2ht-20260707-093015.md" {
		t.Errorf("id fallback: got %q", got)
	}
	// Both empty fall back to "session".
	if got := exportFileName("", "", exportHTML, now); got != "chatchain-session-20260707-093015.html" {
		t.Errorf("session fallback: got %q", got)
	}
}

// exportTestHistory is a two-round history exercising every export feature:
// system prompt, attachment, reasoning, a tool round, and an interruption.
func exportTestHistory() []provider.Message {
	return []provider.Message{
		{Role: "system", Content: "be terse"},
		{Role: "user", Content: "read the file",
			Attachments: []provider.Attachment{{Filename: "a.txt", MimeType: "text/plain", Data: []byte("x")}}},
		{Role: "assistant", Reasoning: "let me think about this",
			ToolCalls: []provider.ToolCall{{ID: "c1", Name: "load_skill", Arguments: map[string]any{"path": "a.txt"}}}},
		{Role: "tool", Content: "file contents", ToolCallID: "c1", ToolCallName: "load_skill"},
		{Role: "assistant", Content: "It says **x**."},
		{Role: "user", Content: "thanks"},
		{Role: "assistant", Content: "partial…", Interrupted: true},
	}
}

func TestBuildExportMarkdown(t *testing.T) {
	meta := exportMeta{Title: "My Chat", SessionID: "abc123", Model: "gpt-x", Date: time.Date(2026, 7, 7, 9, 30, 0, 0, time.UTC)}
	out := buildExportMarkdown(meta, exportTestHistory())

	for _, want := range []string{
		"# My Chat",
		"> Session abc123 · gpt-x · 2026-07-07 09:30",
		"<summary>System prompt</summary>",
		"be terse",
		"## User",
		"## Assistant",
		"(attachment: a.txt)",
		"> ⚙ 1 tool call(s)",
		"It says **x**.", // assistant content verbatim
		"\n\n---\n\n",    // rule between rounds
		"(interrupted)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "let me think") {
		t.Errorf("markdown must skip reasoning:\n%s", out)
	}
	if strings.Count(out, "---") != 1 {
		t.Errorf("expected exactly one round separator, got %d", strings.Count(out, "---"))
	}
}

func TestBuildExportHTML(t *testing.T) {
	meta := exportMeta{Title: "My Chat", SessionID: "abc123", Model: "gpt-x", Date: time.Now()}
	msgs := []provider.Message{
		{Role: "user", Content: "<script>alert(1)</script>"},
		{Role: "assistant", Content: "**bold** and\n\n```go\nfunc main() {}\n```"},
	}
	out, err := buildExportHTML(meta, msgs)
	if err != nil {
		t.Fatalf("buildExportHTML: %v", err)
	}

	// User content is escaped — never interpreted as HTML.
	if strings.Contains(out, "<script>alert") {
		t.Error("user <script> passed through unescaped")
	}
	if !strings.Contains(out, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("escaped user content missing")
	}
	// Assistant markdown is rendered.
	if !strings.Contains(out, "<strong>bold</strong>") {
		t.Error("assistant markdown not rendered")
	}
	// Code blocks carry chroma classes (class mode), styled for both themes.
	if !strings.Contains(out, `class="chroma"`) {
		t.Error("chroma class-mode output missing")
	}
	// Dark mode: media query plus the toggle hook.
	if !strings.Contains(out, "prefers-color-scheme") {
		t.Error("prefers-color-scheme CSS missing")
	}
	if !strings.Contains(out, `id="theme-toggle"`) || !strings.Contains(out, "data-theme") {
		t.Error("theme toggle hook missing")
	}
	if !strings.Contains(out, `html[data-theme="dark"] .chroma`) {
		t.Error("forced-dark chroma scope missing")
	}
}

// Reasoning and tool activity collapse into <details>; user bubbles, tool
// arguments, and tool results are all escaped.
func TestBuildExportHTMLDetails(t *testing.T) {
	meta := exportMeta{Title: "T", Date: time.Now()}
	out, err := buildExportHTML(meta, exportTestHistory())
	if err != nil {
		t.Fatalf("buildExportHTML: %v", err)
	}
	for _, want := range []string{
		"<summary>System prompt</summary>",
		"<summary>Reasoning</summary>",
		"let me think about this",
		"<summary>⚙ load_skill</summary>",
		"file contents", // tool result
		"(attachment: a.txt)",
		`<p class="interrupted">(interrupted)</p>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("html missing %q", want)
		}
	}
}

func TestValidateExportTarget(t *testing.T) {
	for _, bad := range []string{"mydir/", ".", "..", ".md", ".html", "dir/.markdown"} {
		if err := validateExportTarget(bad); err == nil {
			t.Errorf("validateExportTarget(%q) = nil, want error", bad)
		}
	}
	for _, good := range []string{"chat.html", "notes.md", "dir/chat", "a.tar.gz"} {
		if err := validateExportTarget(good); err != nil {
			t.Errorf("validateExportTarget(%q) = %v, want nil", good, err)
		}
	}
}

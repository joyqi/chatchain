package chat

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"chatchain/provider"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	chromastyles "github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
)

// /export renders the session to a single self-contained HTML or Markdown
// file. Exports read the FULL on-disk log (LoadFullHistory), so compaction
// never hides archived rounds. See docs/design/export.md.

// exportFormat selects the output document type.
type exportFormat int

const (
	exportHTML exportFormat = iota
	exportMarkdown
)

// ext returns the canonical file extension for the format.
func (f exportFormat) ext() string {
	if f == exportMarkdown {
		return "md"
	}
	return "html"
}

// validateExportTarget rejects targets without a usable file name: a trailing
// separator (which would silently create a hidden ".html" inside the
// directory), a dot-only base, or a name that is nothing but an extension
// (e.g. ".md").
func validateExportTarget(path string) error {
	if strings.HasSuffix(path, string(filepath.Separator)) || strings.HasSuffix(path, "/") {
		return fmt.Errorf("%q is a directory path; give a file name", path)
	}
	base := filepath.Base(path)
	if base == "" || base == "." || base == ".." || base == filepath.Ext(base) {
		return fmt.Errorf("%q has no usable file name", path)
	}
	return nil
}

// detectExportFormat maps a target filename to its export format, adjusting
// the name when needed: ".md"/".markdown" select Markdown; a name with no
// extension gets ".html" appended; any other extension keeps the given name
// but exports HTML (the default). Extension matching is case-insensitive.
func detectExportFormat(path string) (string, exportFormat) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return path, exportMarkdown
	case "":
		return path + ".html", exportHTML
	default:
		return path, exportHTML
	}
}

// exportSlugMax caps the slug length so generated filenames stay readable.
const exportSlugMax = 40

// slugify lowercases s and folds every run of non-alphanumeric characters into
// a single "-", trimming leading/trailing dashes and capping the length.
func slugify(s string) string {
	var b []rune
	dash := false
	for _, r := range strings.ToLower(s) {
		if len(b) >= exportSlugMax {
			break
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if dash {
				b = append(b, '-')
			}
			dash = false
			b = append(b, r)
		} else if len(b) > 0 {
			dash = true
		}
	}
	if len(b) > exportSlugMax {
		b = b[:exportSlugMax]
	}
	return strings.TrimRight(string(b), "-")
}

// exportFileName generates the no-argument /export target name:
// "chatchain-<slug>-<YYYYMMDD-HHMMSS>.<ext>". The slug is derived from the
// session title, falling back to the session id and then to "session".
func exportFileName(title, id string, format exportFormat, now time.Time) string {
	slug := slugify(title)
	if slug == "" {
		slug = slugify(id)
	}
	if slug == "" {
		slug = "session"
	}
	return fmt.Sprintf("chatchain-%s-%s.%s", slug, now.Format("20060102-150405"), format.ext())
}

// exportMeta carries the session fields shown in export headers.
type exportMeta struct {
	Title     string
	SessionID string
	Model     string
	Date      time.Time
}

// exportTitle returns the display title, with a fallback for untitled sessions.
func exportTitle(meta exportMeta) string {
	if t := strings.TrimSpace(meta.Title); t != "" {
		return t
	}
	return "ChatChain session"
}

// exportMetaLine joins the available header fields (session id, model, date,
// and optionally the message count) into one " · "-separated line, skipping
// fields the session doesn't have.
func exportMetaLine(meta exportMeta, count int, withCount bool) string {
	var parts []string
	if meta.SessionID != "" {
		parts = append(parts, "Session "+meta.SessionID)
	}
	if meta.Model != "" {
		parts = append(parts, meta.Model)
	}
	parts = append(parts, meta.Date.Format("2006-01-02 15:04"))
	if withCount {
		parts = append(parts, fmt.Sprintf("%d messages", count))
	}
	return strings.Join(parts, " · ")
}

// conversationCount is the number of exported conversation messages (system
// prompts are rendered as a header section, not a turn, so they don't count).
func conversationCount(msgs []provider.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role != "system" {
			n++
		}
	}
	return n
}

// exportRound groups one conversation exchange: the user message that opened
// it plus every assistant/tool message before the next user message.
type exportRound struct {
	user    provider.Message // Role == "" when the round has no user opener
	replies []provider.Message
}

// splitRounds separates system messages from conversation rounds. A leading
// assistant/tool message without a user opener (possible in unusual logs)
// starts a round with an empty user part.
func splitRounds(msgs []provider.Message) (system []provider.Message, rounds []exportRound) {
	for _, m := range msgs {
		switch m.Role {
		case "system":
			system = append(system, m)
		case "user":
			rounds = append(rounds, exportRound{user: m})
		default: // assistant / tool
			if len(rounds) == 0 {
				rounds = append(rounds, exportRound{})
			}
			r := &rounds[len(rounds)-1]
			r.replies = append(r.replies, m)
		}
	}
	return system, rounds
}

// ---- Markdown builder ----

// buildExportMarkdown renders the history as a Markdown document: "# <title>",
// a quoted metadata line, "## User"/"## Assistant" headings with "---" between
// rounds. Assistant content is embedded verbatim (it already is Markdown);
// reasoning is skipped; tool activity collapses to one quoted marker per round;
// attachments and interruption are noted inline. The system prompt, if any,
// becomes a collapsed <details> section at the top, not a conversation turn.
func buildExportMarkdown(meta exportMeta, msgs []provider.Message) string {
	system, rounds := splitRounds(msgs)

	var blocks []string
	blocks = append(blocks, "# "+exportTitle(meta))
	blocks = append(blocks, "> "+exportMetaLine(meta, 0, false))
	for _, sys := range system {
		blocks = append(blocks,
			"<details>\n<summary>System prompt</summary>\n\n"+strings.TrimSpace(sys.Content)+"\n\n</details>")
	}

	for i, r := range rounds {
		if i > 0 {
			blocks = append(blocks, "---")
		}
		if r.user.Role == "user" {
			blocks = append(blocks, "## User")
			for _, att := range r.user.Attachments {
				blocks = append(blocks, "(attachment: "+att.Filename+")")
			}
			if c := strings.TrimSpace(r.user.Content); c != "" {
				blocks = append(blocks, c)
			}
		}

		toolCalls := 0
		for _, m := range r.replies {
			toolCalls += len(m.ToolCalls)
		}
		// Image providers reply with attachments and no text — those turns
		// still have an Assistant side to export.
		hasReply := toolCalls > 0
		for _, m := range r.replies {
			if m.Role == "assistant" && (strings.TrimSpace(m.Content) != "" || len(m.Attachments) > 0) {
				hasReply = true
			}
		}
		if !hasReply {
			continue
		}

		blocks = append(blocks, "## Assistant")
		if toolCalls > 0 {
			blocks = append(blocks, fmt.Sprintf("> ⚙ %d tool call(s)", toolCalls))
		}
		for _, m := range r.replies {
			if m.Role != "assistant" {
				continue
			}
			if c := strings.TrimSpace(m.Content); c != "" {
				blocks = append(blocks, c)
			}
			for _, att := range m.Attachments {
				blocks = append(blocks, "(image: "+att.Filename+")")
			}
			if m.Interrupted {
				blocks = append(blocks, "(interrupted)")
			}
		}
	}
	return strings.Join(blocks, "\n\n") + "\n"
}

// ---- HTML builder ----

// exportGoldmark renders assistant Markdown for the HTML export. It stays in
// goldmark's default SAFE mode (raw HTML in model output is omitted, never
// passed through); GFM adds the tables/strikethrough models commonly emit; the
// highlighting extension drives the already-vendored chroma in CLASS mode so
// one stylesheet can carry both the light and dark code palettes (see
// exportChromaCSS).
var exportGoldmark = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
		highlighting.NewHighlighting(
			highlighting.WithFormatOptions(chromahtml.WithClasses(true)),
		),
	),
)

// exportChromaCSS builds the code-highlighting stylesheet for both themes.
//
// Approach: chroma runs in CLASS mode (WithClasses), so the rendered HTML
// carries theme-neutral token classes and a single inline stylesheet provides
// BOTH palettes — the light theme's rules verbatim, and the dark theme's rules
// twice under higher-precedence scopes: inside the prefers-color-scheme media
// query guarded by html:not([data-theme="light"]) (system dark, unless the
// toggle forces light), and under html[data-theme="dark"] (toggle forces dark
// on a light system). Inline per-token styles would lock the export to a
// single palette (or require rendering every code block twice), so classes are
// the only approach that yields correct light AND dark code colors from one
// rendering. CSS comments are disabled so each rule is exactly one
// "selector { styles }" line, which keeps the scope prefixing trivial.
func exportChromaCSS() (string, error) {
	formatter := chromahtml.New(chromahtml.WithClasses(true), chromahtml.WithCSSComments(false))
	var light, dark bytes.Buffer
	if err := formatter.WriteCSS(&light, chromastyles.Get("github")); err != nil {
		return "", err
	}
	if err := formatter.WriteCSS(&dark, chromastyles.Get("github-dark")); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(light.String())
	b.WriteString("@media (prefers-color-scheme: dark) {\n")
	b.WriteString(prefixCSSSelectors(dark.String(), `html:not([data-theme="light"])`))
	b.WriteString("}\n")
	b.WriteString(prefixCSSSelectors(dark.String(), `html[data-theme="dark"]`))
	return b.String(), nil
}

// prefixCSSSelectors prepends prefix to the selector of every one-line
// "selector { styles }" rule in css (the shape chroma's WriteCSS emits with
// comments disabled).
func prefixCSSSelectors(css, prefix string) string {
	var b strings.Builder
	for _, line := range strings.Split(css, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		b.WriteString(prefix + " " + line + "\n")
	}
	return b.String()
}

// exportDarkVars is the dark page palette (GitHub-dark-ish, matching the
// github-dark chroma theme). Emitted twice — under the media query and under
// the data-theme override — since CSS cannot merge those two scopes.
const exportDarkVars = `  color-scheme: dark;
  --bg: #0d1117;
  --fg: #e6edf3;
  --muted: #8b949e;
  --border: #30363d;
  --bubble-bg: #161b22;
  --bubble-border: #30363d;
  --code-bg: #161b22;
  --accent: #58a6ff;`

// exportPageCSS is the static page stylesheet (everything but the chroma
// token rules). Dark mode mirrors exportChromaCSS's precedence scheme.
const exportPageCSS = `:root {
  color-scheme: light;
  --bg: #ffffff;
  --fg: #1f2328;
  --muted: #6a737d;
  --border: #d8dee4;
  --bubble-bg: #eef4fb;
  --bubble-border: #cfe0f4;
  --code-bg: #f6f8fa;
  --accent: #0969da;
}
@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) {
` + exportDarkVars + `
  }
}
:root[data-theme="dark"] {
` + exportDarkVars + `
}
* { box-sizing: border-box; }
body {
  margin: 0 auto;
  padding: 2rem 1.25rem 4rem;
  max-width: 48rem;
  background: var(--bg);
  color: var(--fg);
  font-family: system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
  line-height: 1.6;
}
header { position: relative; border-bottom: 1px solid var(--border); padding-bottom: 1rem; margin-bottom: 1rem; }
header h1 { margin: 0 0 0.35rem; font-size: 1.5rem; padding-right: 6rem; }
p.meta { margin: 0; color: var(--muted); font-size: 0.85rem; }
#theme-toggle {
  position: absolute; top: 0.25rem; right: 0;
  padding: 0.25rem 0.6rem; font-size: 0.8rem;
  color: var(--muted); background: transparent;
  border: 1px solid var(--border); border-radius: 6px; cursor: pointer;
}
#theme-toggle:hover { color: var(--fg); }
section.round { border-bottom: 1px solid var(--border); padding: 1.25rem 0; }
section.round:last-child { border-bottom: none; }
div.bubble {
  background: var(--bubble-bg);
  border: 1px solid var(--bubble-border);
  border-radius: 0.75rem;
  padding: 0.7rem 1rem;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
p.attachment, p.interrupted { color: var(--muted); font-size: 0.85rem; font-style: italic; margin: 0.35rem 0; }
details.muted { color: var(--muted); font-size: 0.9rem; margin: 0.75rem 0; }
details.muted summary { cursor: pointer; }
p.tool-label { margin: 0.5rem 0 0.25rem; font-size: 0.8rem; }
a { color: var(--accent); }
pre, code {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace;
}
code { font-size: 0.9em; }
:not(pre) > code { background: var(--code-bg); padding: 0.15em 0.35em; border-radius: 4px; }
pre { background: var(--code-bg); padding: 0.75rem 1rem; border-radius: 8px; overflow-x: auto; }
blockquote { margin: 0.75rem 0; padding: 0 1rem; color: var(--muted); border-left: 3px solid var(--border); }
table { border-collapse: collapse; margin: 0.75rem 0; display: block; overflow-x: auto; }
th, td { border: 1px solid var(--border); padding: 0.35rem 0.7rem; }
img { max-width: 100%; }
`

// exportToggleJS applies a stored theme choice on load and flips it on click;
// html[data-theme] then overrides the prefers-color-scheme defaults above.
const exportToggleJS = `(function () {
  var root = document.documentElement;
  var stored = localStorage.getItem("chatchain-theme");
  if (stored) root.setAttribute("data-theme", stored);
  document.getElementById("theme-toggle").addEventListener("click", function () {
    var sysDark = window.matchMedia("(prefers-color-scheme: dark)").matches;
    var cur = root.getAttribute("data-theme") || (sysDark ? "dark" : "light");
    var next = cur === "dark" ? "light" : "dark";
    root.setAttribute("data-theme", next);
    localStorage.setItem("chatchain-theme", next);
  });
})();`

// buildExportHTML renders the history as one self-contained HTML document: all
// CSS inline, no external references. Assistant Markdown goes through goldmark
// (safe mode); user content is HTML-escaped into bubble blocks (user text is
// never interpreted); reasoning and tool calls (name, arguments, result — all
// escaped) collapse into <details>; the system prompt becomes a muted
// collapsed section at the top. Dark mode follows prefers-color-scheme, with
// a toggle button persisted to localStorage.
func buildExportHTML(meta exportMeta, msgs []provider.Message) (string, error) {
	codeCSS, err := exportChromaCSS()
	if err != nil {
		return "", err
	}
	system, rounds := splitRounds(msgs)

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	fmt.Fprintf(&b, "<title>%s</title>\n", html.EscapeString(exportTitle(meta)))
	b.WriteString("<style>\n" + exportPageCSS + codeCSS + "</style>\n</head>\n<body>\n")

	b.WriteString("<header>\n")
	fmt.Fprintf(&b, "<h1>%s</h1>\n", html.EscapeString(exportTitle(meta)))
	fmt.Fprintf(&b, "<p class=\"meta\">%s</p>\n", html.EscapeString(exportMetaLine(meta, conversationCount(msgs), true)))
	b.WriteString("<button id=\"theme-toggle\" type=\"button\">Toggle theme</button>\n</header>\n<main>\n")

	for _, sys := range system {
		b.WriteString("<details class=\"muted system\"><summary>System prompt</summary><pre>")
		b.WriteString(html.EscapeString(sys.Content))
		b.WriteString("</pre></details>\n")
	}

	for _, r := range rounds {
		b.WriteString("<section class=\"round\">\n")
		if r.user.Role == "user" {
			for _, att := range r.user.Attachments {
				fmt.Fprintf(&b, "<p class=\"attachment\">(attachment: %s)</p>\n", html.EscapeString(att.Filename))
			}
			fmt.Fprintf(&b, "<div class=\"bubble\">%s</div>\n", html.EscapeString(r.user.Content))
		}

		// Tool results are matched to their calls by id.
		results := make(map[string]provider.Message)
		for _, m := range r.replies {
			if m.Role == "tool" {
				results[m.ToolCallID] = m
			}
		}

		for _, m := range r.replies {
			if m.Role != "assistant" {
				continue
			}
			if m.Reasoning != "" {
				b.WriteString("<details class=\"muted\"><summary>Reasoning</summary><pre>")
				b.WriteString(html.EscapeString(m.Reasoning))
				b.WriteString("</pre></details>\n")
			}
			// Chronological order matches the live stream: the message's text is
			// produced first, then its tool calls run — so content renders above
			// the tool blocks it announced.
			if m.Content != "" {
				var out bytes.Buffer
				if cerr := exportGoldmark.Convert([]byte(m.Content), &out); cerr != nil {
					return "", cerr
				}
				b.WriteString("<div class=\"assistant\">\n")
				b.Write(out.Bytes())
				b.WriteString("</div>\n")
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "<details class=\"muted tool\"><summary>⚙ %s</summary>\n", html.EscapeString(displayToolName(tc.Name)))
				if len(tc.Arguments) > 0 {
					args, jerr := json.MarshalIndent(tc.Arguments, "", "  ")
					if jerr != nil {
						args = []byte(fmt.Sprintf("%v", tc.Arguments))
					}
					fmt.Fprintf(&b, "<pre>%s</pre>\n", html.EscapeString(string(args)))
				}
				if res, ok := results[tc.ID]; ok {
					label := "Result"
					if res.IsError {
						label = "Error"
					}
					fmt.Fprintf(&b, "<p class=\"tool-label\">%s</p><pre>%s</pre>\n", label, html.EscapeString(res.Content))
				}
				b.WriteString("</details>\n")
			}
			// Generated images embed as data URIs — the export stays a single
			// self-contained file even after the session bundle is deleted.
			for _, att := range m.Attachments {
				if strings.HasPrefix(att.MimeType, "image/") {
					fmt.Fprintf(&b, "<img alt=\"%s\" src=\"data:%s;base64,%s\">\n",
						html.EscapeString(att.Filename), html.EscapeString(att.MimeType),
						base64.StdEncoding.EncodeToString(att.Data))
				} else {
					fmt.Fprintf(&b, "<p class=\"attachment\">(attachment: %s)</p>\n", html.EscapeString(att.Filename))
				}
			}
			if m.Interrupted {
				b.WriteString("<p class=\"interrupted\">(interrupted)</p>\n")
			}
		}
		b.WriteString("</section>\n")
	}

	b.WriteString("</main>\n<script>\n" + exportToggleJS + "\n</script>\n</body>\n</html>\n")
	return b.String(), nil
}

// ---- /export command handler ----

// expandHome resolves a leading "~/" against the user's home directory.
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// exportChat implements /export: resolve the target path and format (asking
// for the format and generating a name when no argument is given), load the
// full session log — falling back to the in-memory history for ephemeral or
// not-yet-persisted sessions — build the document, and write it. An existing
// file is an error (no silent overwrite).
func exportChat(w io.Writer, arg string, sw *SessionWriter, history []provider.Message, p provider.Provider) {
	var path string
	var format exportFormat
	if arg == "" {
		return // the format prompt lives in the UI loop (see /export in runv2)
	}
	{
		expanded := expandHome(arg)
		if verr := validateExportTarget(expanded); verr != nil {
			ErrorStyle.Fprintf(w, "Error: %v\n", verr)
			return
		}
		path, format = detectExportFormat(expanded)
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}

	msgs := history
	if sw.onDisk() {
		full, err := LoadFullHistory(sw.ID(), p)
		if err != nil {
			ErrorStyle.Fprintf(w, "Error: %v\n", err)
			return
		}
		msgs = full
	}
	if conversationCount(msgs) == 0 {
		DimStyle.Fprintln(w, "Nothing to export yet.")
		return
	}

	meta := exportMeta{Title: sw.Title(), SessionID: sw.ID(), Model: p.Model(), Date: time.Now()}
	var doc string
	var err error
	if format == exportMarkdown {
		doc = buildExportMarkdown(meta, msgs)
	} else {
		doc, err = buildExportHTML(meta, msgs)
		if err != nil {
			ErrorStyle.Fprintf(w, "Error: %v\n", err)
			return
		}
	}
	// O_EXCL makes the no-overwrite guarantee atomic: creation fails if the
	// target appeared between any earlier check and this write.
	f, ferr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if ferr != nil {
		if os.IsExist(ferr) {
			ErrorStyle.Fprintf(w, "Error: %s already exists\n", path)
		} else {
			ErrorStyle.Fprintf(w, "Error: %v\n", ferr)
		}
		return
	}
	_, werr := f.WriteString(doc)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		ErrorStyle.Fprintf(w, "Error: %v\n", werr)
		return
	}
	DimStyle.Fprintf(w, "Exported %d messages → %s\n", conversationCount(msgs), path)
}

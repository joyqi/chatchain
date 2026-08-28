# /export — render a session to a single HTML or Markdown file

## Goal

Export the conversation for reading and archiving:

- `/export <filename>` — format from the extension: `.md`/`.markdown` →
  Markdown; anything else → HTML (the default). A name with no extension gets
  `.html` appended; other unknown extensions keep the given name but export
  HTML. `~` expands; relative paths resolve against the working directory.
  An existing file is an error (no silent overwrite).
- `/export` — a selector picks HTML or Markdown; the filename is generated as
  `iota-<title-slug-or-id>-<YYYYMMDD-HHMMSS>.<ext>` in the working
  directory. The full path is printed on success.

## Scope of the exported history

**The full on-disk log**, not the compacted in-memory view: exports are for
archiving, so compaction must not hide older rounds. A raw loader reads every
message record from `messages.jsonl` and ignores compaction markers (the
scanning is shared with `loadLog`; only the view-weaving differs). Ephemeral
sessions (`--no-save`, nil writer) fall back to the in-memory history.

## HTML format

A **single self-contained file**: all CSS inline, no external references.

- Assistant markdown is rendered server-side by **goldmark** (the industry
  standard; Hugo's renderer) with **goldmark-highlighting**, which drives the
  already-vendored chroma for code blocks. goldmark stays in its default
  safe mode (raw HTML in model output is not passed through).
- **Dark mode**: `prefers-color-scheme` follows the system, plus a small
  toggle button (a few lines of inline JS, persisted to localStorage).
- Layout: a header with metadata (title, session id, model, export date,
  message count); user messages as highlighted bubble blocks (content
  HTML-escaped — user text is never interpreted); assistant replies as body
  text; reasoning and tool calls (name, arguments, result) collapsed into
  `<details>`; interrupted replies and attachments marked.

## Markdown format

- `# <title>` then a quoted metadata line (session id, model, date).
- Each turn separated: `## User` / `## Assistant` headings with a `---` rule
  between rounds. Assistant content is embedded verbatim (it already is
  Markdown). Reasoning is skipped; tool activity collapses to one quoted
  marker line per round; attachments and interruption noted inline.

## Non-goals

- No export of provider raw-content blobs.
- No pagination/splitting; one file per export.
- No PDF (out of scope; print the HTML instead).

## Dependencies

`github.com/yuin/goldmark` + `github.com/yuin/goldmark-highlighting/v2`
(new; both standard), reusing the existing `alecthomas/chroma/v2`.

## Testing

- Format/extension detection and filename generation (slug fallback to id).
- Raw loader returns pre-compaction rounds that the view hides.
- Markdown builder structure (headings, separators, tool markers).
- HTML builder: user `<script>` input arrives escaped; assistant markdown
  renders; dark-mode CSS present. No terminal emulation.

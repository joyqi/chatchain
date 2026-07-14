# Tabbed Select — a tabbed selector component

> **Superseded (implementation only).** The readline/promptui implementation
> this document designed (`internal/promptui/tabbed.go`) was deleted together
> with the rest of the readline UI stack; the component now lives at
> `internal/ui/tabbed.go` as a surface engine inside the bubbletea inline
> Program (panel kinds `PanelList` / `PanelMulti` / `PanelSlider` /
> `PanelBrowser` / `PanelView`, opened via `UI.Tabbed` in-REPL or
> `ui.RunSurface` one-shot). The **UX contract is unchanged**: Tab switches
> focus, Enter commits all tabs, q/Esc cancels, same key map. The goal,
> requirements, and UX sections below remain the live spec; the
> implementation-mechanics sections are kept as a historical record and are
> marked as such.

## Goal

Merge paired commands into single commands, carried by a tabbed selector: once
open, the user switches between tabs, and each tab is an independent
single-select / multi-select / directory-browser panel.

- `/file` + `/files` → unified `/file`
- `/session` + `/sessions` → unified `/session`
- `/tools` + `/mcp` → unified `/tools` (a read-only tabbed viewer)
- `/model` + `/context` → unified `/model` (a four-tab questionnaire; see
  `docs/design/model-settings.md`)

The component is designed to be **reusable**: a future "ask several choice
questions at once" flow (AskUserQuestion-style — multiple questions, each with a
set of options) reuses the same component; only the caller's interpretation of
the result differs.

## Behavior before the merge (historical)

The paired commands this design replaced, as they existed on the readline
stack. These pickers are gone (`PickSession` survives as a function, but its
promptui select was replaced by a one-shot ui surface — see Command migration):

| Command | Behavior | Impl (replaced) |
|---|---|---|
| `/file` | no arg → directory browser to attach a file; `/file <path>` → direct attach | `pickFile()` `chat/file.go` |
| `/files` | multi-select removal of attached files | `cleanAttachments()` `chat/file.go` |
| `/session` | single-select resume | `PickSession()` `chat/session.go` |
| `/sessions` | multi-select deletion of non-current sessions | `cleanSessions()` `chat/sessions_clean.go` |

## Original component design (`internal/promptui/tabbed.go` — deleted; historical)

> **Historical.** Everything in this section describes the deleted
> readline/promptui implementation. The current implementation is
> `internal/ui/tabbed.go`: panels are declarative `Panel` specs (`Title` +
> `PanelKind` + kind-specific fields) in a `TabbedSpec`; the ui model routes
> keys into the open surface (`surfaceKey`) and the bubbletea renderer paints
> it below the composer — no readline loop, no `screenbuf`, no injected
> `RuneWidth` (width comes from `internal/textwidth`). Enter commits **all**
> tabs: `TabbedResult` carries every panel's `PanelResult` plus the focused
> index. The panel semantics, key map, and render layout designed below carried
> over as-is.

The original plan reused the readline infrastructure, **no reinvention**:
readline `Listener` + `screenbuf` in-place repaint, `FuncGetSize` for the
terminal size, an injected `RuneWidth` for CJK width. The container owned a
single readline loop; panels were passive "render + handle key" objects
(unlike `Select`, where each instance ran its own `rl.Readline()`).

### Panel interface

```go
// Panel is one tab in a Tabbed selector. The container owns the terminal loop;
// panels only render their body and react to keys.
type Panel interface {
    Title() string
    // Render returns the panel body's lines (already fitted to width, may carry
    // ANSI); height is the number of body rows available.
    Render(width, height int) []string
    // HandleKey processes one key; consumed=true means the panel handled it and
    // the container should not interpret it further.
    HandleKey(key rune) (consumed bool)
}
```

### Concrete panels

- **`ListPanel`** (single and multi)
  - Fields: `TitleText`, `Items`, `Multi`.
  - Internal: cursor, checked map (multi only), scroll offset.
  - Render: active row `▸` + cyan; multi mode prefixes `[x]`/`[ ]`.
  - Keys: `↑↓` (vim `jk`) navigate, `←→` (vim `hl`) page, `g`/`G` top/bottom,
    `Space` toggle (multi) — mirrors Select's keymap.
  - Enter: **always returns consumed=false** → the container commits.
  - Read via `Selected() []int` (multi = all checked; single = the cursor row as
    a 1-element slice) and `Cursor() int`.
- **`BrowserPanel`** (`/file`'s "Add")
  - Fields: `TitleText`, `Dir` (start directory, defaults to cwd).
  - Internal: current dir, entries (`..` + subdirs + files), cursor, scroll.
    Reuses `pickFile`'s directory-reading rules.
  - Keys: `↑↓` navigate, `←→` (vim `hl`) page.
  - Enter: on a directory → descend, returns **consumed=true** (no submit); on a
    file → record the choice, returns **consumed=false** → container submits.
  - Read via `Chosen() string` (absolute path of the chosen file, empty if none).
- **`ViewPanel`** (read-only, `/tools`'s "Tools" / "MCP" tabs)
  - Fields: `TitleText`, `Lines` (may carry ANSI), `Wrap`.
  - Mirrors `Viewer`: by default it clips lines and pans horizontally with `h`/`l`;
    set `Wrap` to soft-wrap to the width instead (ANSI-aware, reusing Viewer's
    `wrapLine`, pan disabled). CJK width approximated as 1, matching Viewer.
  - Keys: `↑↓` scroll, `←→` / `Space` / `b` page, `h`/`l` pan (unless `Wrap`),
    `g`/`G` top/bottom. Implements `HelpHint()` so the container shows
    viewer-specific help instead of the selector help.
  - Enter: not consumed → the container closes the view (read-only, nothing to
    submit).

### Container `Tabbed`

```go
type Tabbed struct {
    Panels    []Panel
    RuneWidth func(rune) int // CJK width; nil means 1 per rune
    Stdin     io.ReadCloser  // defaults to os.Stdin (injected in tests)
    Stdout    io.WriteCloser // defaults to os.Stdout
    Size      int            // body-row cap before a panel scrolls (default 15)
}

// Run shows the selector and blocks until submit or cancel. It returns the tab
// focused at submission. All selection state stays in the panel objects, read
// by the caller (focused tab for a command, every tab for a questionnaire).
// Cancel (Ctrl+C / Esc) returns ErrInterrupt.
func (t *Tabbed) Run() (focused int, err error)
```

### Key dispatch order (container)

1. `Tab` / `Shift+Tab` → switch the focused tab (panels never see it).
2. `Ctrl+C` / `Esc` / `q` → cancel (`ErrInterrupt`), matching Select (`q` maps to
   `CharInterrupt` via the readline input filter, since the listener can't end the run).
3. Otherwise → `focusedPanel.HandleKey(key)`.
4. If the key is `Enter` and the panel did **not** consume it → commit (return
   focused).
5. Otherwise repaint.

`←→` are PageUp/PageDown in the existing Select, `↑↓` navigate, `Space` toggles,
so `Tab`/`Shift+Tab` are free for tab switching — no conflict. (Note: readline
skips the listener when Enter submits, so Enter is handled once in the container
loop, not via the listener — no double dispatch.)

### Render layout (screenbuf in-place repaint)

```
 [Attached] · Add        ← tab bar: active tab = cyan-bg chip (fg/bg swapped), rest dim
 ▸ [x] a.png (image/png, 1234 bytes)
   [ ] notes.md (text/markdown, 88 bytes)
 Tab switch · ↑↓ move · ←→ page · Space toggle · Enter confirm · q/Esc cancel  ← dim help
```

Tab-bar widths and row truncation all use the injected `RuneWidth`.

## Command migration

Executed as planned; the command dispatch now lives in `chat/run.go` and opens
the surfaces via `u.Tabbed(...)` with `ui.Panel` specs, but the shape of each
command is as designed here.

- **`/file`** (now `chat/run.go`): no arg →
  `Tabbed{ ListPanel("Attached", attachmentLabels, Multi:true), BrowserPanel("Add", cwd) }`.
  On submit: `focused==0` → remove the checked items from `pendingAttachments`;
  `focused==1` → attach `BrowserPanel.Chosen()`. `/file <path>` direct attach is
  unchanged.
- **`/session`** (now `chat/run.go`):
  `Tabbed{ ListPanel("Resume", sessionLabels, Multi:false), ListPanel("Delete", deletableLabels, Multi:true) }`.
  The Delete tab excludes the current session. On submit: `focused==0` →
  `ResumeSession(cursor)`; `focused==1` → delete the checked ones (reuse the
  `cleanSessions` deletion logic).
- **`/tools`** (now `chat/run.go`): opens a read-only
  `Tabbed{ ViewPanel("Tools", toolStatusLines), ViewPanel("MCP", mcpStatusLines) }`.
  `printToolStatus`/`printMCPStatus` were refactored into `toolStatusLines` /
  `mcpStatusLines` (which return `[]string` and now double as `Refresh`
  callbacks, live-updating the open surface as MCP servers connect). `/mcp` is
  removed from `slashCommands` and folded into the MCP tab. The Tools tab clips +
  pans (like the old `/tools`); the MCP tab sets `Wrap` (like the old `/mcp`).
- **`/files`, `/sessions`** removed from `slashCommands` (`chat/completion.go`)
  and from the `Run()` dispatch.
- Fold now-dead wrappers into panel construction and delete them (avoid dead
  code). `runSelect` and `SelectModel` are gone with the readline stack (the
  in-REPL model pick is a plain `u.Select`, a one-panel Tabbed).
  `PickSession` survives for `--resume` at launch, backed by a one-shot ui
  surface (`ui.RunSurface`, a surface-only Program that releases the terminal
  before the REPL starts); the old context-window picker is gone with
  `/context` (folded into `/model`'s Context tab).
- Update `/help`, the `slashCommands` table, and the docs.

## Future reuse: several questions at once

N list panels (each = one question). The current API already commits all tabs:
`TabbedResult.Panels` carries every panel's state, so a questionnaire caller
reads all of them instead of just `Focused` — the same component, no mode flag
needed (`/model`'s four-tab questionnaire works exactly this way).

## Testing

Avoid repeating the abandoned custom-terminal-renderer rabbit hole: **no full
terminal emulation**.
- Panel-level logic tests: feed key sequences and assert cursor/checked/chosen
  state, directory-descend vs file-choose.
- Container-level: a few key-dispatch / tab-switch assertions.
- CJK width: assert the tab bar and row truncation stay aligned.

(The mechanics moved with the port: the promptui plan injected
`Stdin`/`Stdout` and `RuneWidth`; the bubbletea implementation feeds `tea.Key`
events to the ui model and asserts on the rendered view instead. The testing
principles are unchanged.)

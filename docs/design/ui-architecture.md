# UI Architecture: bubbletea v2 inline migration

Status: shipped through P5 (2026-07-14) — the bubbletea UI is the only interactive path; P6 (lipgloss major convergence) remains · Brain: `ui-bubbletea-v2`

## Decision

Migrate the interactive surface from the vendored readline+promptui REPL to a
**bubbletea v2 INLINE program**: a pinned bottom frame (separator + live status
+ real-cursor composer) with history flowing above it into native scrollback
via `Println`. The terminal's own scrolling is never taken over.

Gate results that made the call: real-cursor IME verified acceptable by hand
(Ghostty, streaming); the spike (`spikes/bubbletea-inline`) proved
selector-in-frame-while-streaming, the live region (spinner + rolling
raw-source preview + one-shot commit, matching StreamView semantics), a
multi-row wrapping composer, and app-side fixes for all four v2 inline warts
(insertAbove cursor desync → view-changing bump; top-anchored shrink → deferred
re-anchor; textarea viewport snap after growth; resize reflow leakage →
accepted, do nothing during the desync window).

This supersedes `readline-repl-over-charm` and the Surface-lite plan
(`typeahead-surface-lite`). The vendored-ui-stack philosophy survives with a
new substrate: we own our UI layer, now as bubbletea components in
`internal/ui`.

## Package layout

```
internal/ui/          the ONLY importer of the charm.land v2 UI stack
                      (bubbletea/bubbles). Runs the tea.Program on its own
                      goroutine; exposes a synchronous facade to logic.
internal/markdown/    the streaming markdown renderer (moved from
                      chat/markdown.go), PURE: no terminal, no globals,
                      no bubbletea. Emits via its own Sink interface.
internal/mathtext/    unchanged.
chat/                 business logic only. Keeps the imperative Run loop;
                      every terminal touch becomes a ui call.
```

Amended isolation rule (the original "ui is the only charm importer" was
false the moment markdown moved): **`internal/ui` is the only importer of
bubbletea/bubbles (the event-loop/TUI stack). lipgloss — a string styler — is
permitted in `internal/markdown` (v1 today) and `internal/ui` (v2 via
bubbles).** Two lipgloss majors coexist during the migration window (distinct
module paths; legal); converge markdown to v2 after P5. The v2 stack requires
a Go toolchain bump (1.25 → 1.26.x): update go.mod and the release workflow
image at P2.

`internal/` is the right home: this is a binary-only module; the UI layer is
an implementation detail, import-restricted by construction, and consistent
with where the vendored stack already lives.

## The facade (internal/ui)

Synchronous service API; methods marshal into the Program and (only where a
result is required) await a reply.

```go
type UI interface {
    // Input. Blocks until submit. The composer stays live while a turn
    // renders (type-ahead); submits during a turn queue INSIDE ui, and
    // ReadInput drains the queue. Input separates Display (pastes expanded
    // but bounded, for the user block) from Text (expanded in full, for
    // sending); the composer itself keeps the tags.
    ReadInput(ctx context.Context, opts ...InputOpt) (Input, error)

    // History (append-only scrollback, Println path). Fire-and-forget;
    // ordering guaranteed by the Program mailbox + the anchor manager.
    PrintLines(lines ...string)
    UserBlock(display string)

    // Streaming. The sink is bound to the turn's cancel scope: ui routes
    // ESC/Ctrl+C to `cancel` directly (logic stays blocked in its turn).
    StartStream(cancel context.CancelFunc) StreamSink

    Busy(label string) (stop func())   // frame spinner; ui appends elapsed
                                       // time + "(ESC to cancel)" itself
    SetStatus(StatusData)              // model · ctx used/window (pct) …
    SetTitle(string)                   // tea View.WindowTitle

    // Modal-ish surfaces, rendered in the frame.
    Select(SelectSpec) (SelectResult, error)
    View(ViewSpec) error               // read-only viewer (wrap/height/refresh)
    Confirm(label string, choices ...string) (int, error)

    Close() error
}

type StreamSink interface {
    CommitLines(lines []string)                 // final ANSI lines → scrollback
    BlockPreview(label string) io.WriteCloser   // live region: rolling window
    Done()                                      // turn finished
}
```

Facade shape decisions from the adversarial review:

- **Await only input-acquiring calls** (ReadInput/Select/View/Confirm).
  PrintLines/SetStatus/StreamSink are fire-and-forget sends — a stream
  goroutine's throughput must not be coupled to render latency.
- **Reply channels are buffered(1)**; every blocking call selects on
  {reply, program-done, caller-ctx}; Program shutdown fails all outstanding
  waiters. No unbuffered sends from inside Update (deadlock).
- **SelectSpec** must express the verified surfaces: tabbed panels with kinds
  (single list, multi-select list, slider, boolean switch, one-line text input, directory browser, preview picker, view), Enter
  commits ALL tabs (the /model questionnaire), per-panel `Refresh func()
  []string` + `RefreshEvery` (the /debug and /tools live views). Drill-down
  loops (debug detail) stay logic-side: Select → View → Select.
- **BlockPreview returns an io.WriteCloser** (stream-shaped, not
  snapshot-shaped): the old path backs it directly with promptui.StreamView
  (perfect fidelity in P1); the new ui backs it with a frame live-region model
  that owns the 3-line window + spinner. Close = Done/clear, and the caller's
  flush ordering (close preview BEFORE committing the rendered block) is
  preserved by construction.
- **Frame stacking order is fixed and ui-owned** (user-confirmed final):
  `[staging tail] [stream preview] [queue] ─sep─ [composer] ─sep─
  [status | suggestions | surface]`. The composer is wrapped by two
  separators; the bottom zone below the lower separator holds exactly one
  of the status line, the slash-suggestion row, or an open interaction
  surface (surface > suggestions > status) — swapping them is a content
  change plus below-composer growth, never a composer move. Content-side
  transients (stream preview) morph in place through the staging window;
  the busy indicator is a status-line segment. Layout laws: above the
  composer the frame only grows (staging window + queue; shrink happens by
  in-place morph); the status row is the permanent carrier for transient
  indicators; below-composer shrink dies at the screen bottom and
  self-heals from subsequent output. A side benefit: the composer never
  sits on the physical bottom row (status pads it), removing the
  Terminal.app bottom-row CJK crash surface.

## internal/markdown (P1)

`chat/markdown.go` moves with a purity refactor. The review killed the
static-config shape — three signals are LIVE today and must stay live:

- **Width**: re-read per block flush (mid-stream resize changes the next
  block). → `Sink` (or a `Layout` provider) carries `Width() int`; ui answers
  from its last WindowSizeMsg; tests/pipes return a fixed width.
- **color.NoColor**: re-read before every lipgloss render (`syncMDRenderer`).
  → keep reading the global (it is process-wide by design).
- **Code theme**: today `detectCodeTheme` runs an OSC background query at
  startup (a terminal interaction!). The query moves to ui/cmd; the result is
  injected as a getter.

The five in-band block previews (table/code/list/quote/math) stop
type-asserting `w.(*os.File)` and instead call `Sink.BlockPreview`; a no-op
sink restores today's piped/non-TTY degradation. P1 must also re-plumb the
call sites that hardcode `os.Stdout` (`newMarkdownWriter(os.Stdout)` at
streamResponse/streamToolRound ×4, `reasoningStream` ×1) — the extraction is
not done until every constructor takes the sink.

Reasoning fit the same shape in P1 (a BlockPreview rolling window folding to
the "◇ thought for Ns" marker); since 2026-07-17 it renders as the transcript
layer's lifecycle widget instead — see "The transcript layer" below.

**P1 lands against the OLD UI** (a StreamView-backed sink), zero behavior
change, tests green — it is valuable standalone and is the executable proof
of the seam.

## The transcript layer (2026-07-17; activity groups 2026-08-01)

`chat/transcript.go` is the single writer to the chat area — every block
(user input, activity, markdown content, notices, errors, resume echoes)
declares itself there, and the transcript alone spaces them:

- **one blank separator opens every block**, paid at block OPEN, so the
  staging view of a lifecycle widget is spaced exactly like the settled
  scrollback it morphs into; consecutive notices/errors group into one block;
- **interior blanks defer through a pending latch** and a block's trailing
  blanks are dropped — no block can export trailing blanks for a neighbor to
  lean on (the fragility class behind the resume-echo regression);
- it owns the **activity group**: the run of thinking segments and tool
  calls between two content blocks (or turn boundaries) shares ONE lifecycle
  widget. The header shows the current activity ("⠋ Thinking", "⠋ [name …]"
  expanding in place, dim "Working…" between events); completed events
  scroll through the widget body as compact rows ("◇ thought 4s",
  "✓ [read_file …] · first result line"); the "⎿" status row carries group
  counters (3 tools · 1.2k tokens · 24s · ESC to cancel). Reasoning text
  itself never renders.

**Settling.** A content boundary (the content block's first committed line)
folds the group into one summary line — "◇ thought for 15s · ran 4 tools in
12s" — and the next activity opens a new group. Degenerate groups keep the
classic forms: thinking alone = "◇ thought for Ns", a lone untought call =
the header + "⎿" result block; `/debug on` (verbose) settles after every
event, which reproduces exactly those classic per-item blocks. Aggregation
never swallows failures (red "· N failed" on the summary plus one red
breakout row per failed call) and an interrupted/errored turn still settles
its partial summary via resetTurn. Timing: the tool figure sums execution
time only; the live clock pauses (region pausedAt + since compensation)
while the user is consulted — approval prompts, ask surfaces. Counters are
event counts, never durations (a real segment can measure 0ns on Apple
Silicon's ~41ns clock tick).

**Presentation classes** (tool.PresentationReporter, mirroring
ApprovalReporter) route each call's display. PresentSurface (the ask set)
never enters the panel: the tabbed surface IS the display, the host
presenter carries StateNeedsInput (plus an unfocused notification),
composing deltas never raise the widget (the raise waits for a NAMED,
groupable delta), and the outcome lands as its own "?" record block — the
user's answers, replayed the same way on resume. PresentExpanded (file
mutations: edit_file/write_file) is a group boundary like content: the
running group settles, the call takes a standalone widget, and the result
expands into a colored unified diff (± rows, cyan @@ hunks) under a
"header · +A -R" line. The diff arrives through the tool.Artifact context
side channel — display-only, never in the model-facing result text (a full
diff there costs tokens) — generated with go-udiff; the row budget follows
the live screen height floored at 24, overwide rows truncate (wrapping
would wreck diff alignment), and artifact-less settles (declines, errors)
fall back to the classic form. Diffs are live-only: messages.jsonl carries
only the result text, so resume echoes fold expanded calls into the count.

The status line keeps only the pre-output phases (Sending request with
upload progress, Waiting for the model). `ui.UI` exposes the widget verbs
(CallPreview/CallDetail/CallLine/ClosePreview, PauseClock/ResumeClock);
`StreamSink` shrank to the turn-scope handle plus the markdown preview seam
(BlockPreview/Done). Direct `u.PrintLines` writes from chat code are an
architecture violation — everything goes through the transcript.

Two supporting contracts:

- **committed lines are SGR-self-contained.** fatih/color's Fprintf wraps
  the reset AROUND a trailing newline ("\x1b[2m…\n\x1b[0m"), so naive line
  splitting yields reset-only "lines" that render as spurious blank rows
  (seen live as a double blank between a tool result and the next thinking
  marker). `lineCommitter.flush` glues escape-only lines back onto their
  predecessor; the transcript's blank latch can then stay display-naive.
- **`CHATCHAIN_DEBUG_REGION=<file>`** traces every region op (commit /
  openCallPreview / closePreview / dropPreview + overflow batches). Spacing
  faults that sit in a producer or the renderer are invisible to region unit
  tests — the live op trace is how you localize which layer emitted a stray
  row.

## Concurrency contract

The review's blocker: "spawn the turn + immediately ReadInput + queue at
logic level" has a dequeue-then-wait state where ESC has no receiver, plus
shared turn-state races. Adopted instead:

**Synchronous turns + ui-owned queue.**

- The logic loop is strictly sequential: `in := ui.ReadInput()` → process the
  whole turn (send → stream → tools → persist) → loop. Logic never holds a
  dequeued-but-undispatchable input.
- Type-ahead is a ui affair: the composer is always live; submits while a
  turn runs are queued inside ui (rendered as dim "queued" lines under the
  composer); the next ReadInput drains them in order. On Ctrl+C/ESC mid-turn,
  ui atomically clears the queue back into the composer draft.
- **Interrupt routing is injected, not surfaced**: `StartStream(cancel)` /
  `Busy` register cancel scopes on a ui-side stack (turn > tool). ESC cancels
  the top scope, Ctrl+C the turn; the partial-output tee/finalize logic stays
  where it is (logic level) — it survives unchanged because cancellation
  still arrives as a provider ctx cancel. `startCancelWatch`/termios die with
  the old stack (the Program owns stdin).
- **Mid-turn command dispatch is scoped OUT of v1.** Every real command
  mutates state the in-flight turn uses (/model ↔ provider, /compact ↔
  history, /session ↔ session writer). Commands queue like messages and run
  between turns. The selector-during-stream capability remains proven ui
  tech; revisit for read-only viewers once snapshot feeds exist.
- **No re-anchor machinery**: the below-composer interaction area makes every
  frame shrink self-healing (vacated rows die at the screen bottom; output
  walks the frame back down; history stays contiguous). The spike's earlier
  scheduleShrink / pendingBlanks / deferred-filler state machine — and the
  concurrency hazards the review found in its arithmetic — were deleted
  outright, and no frame-at-bottom detection is needed. A closing surface
  commits only a one-line outcome record ("model switched to X") into
  scrollback.
- **Cursor-restore bump**: the spike's fix relies on the view changing per
  insert. Production ties it to a streaming activity glyph in the status line
  that advances per committed insert (also a nice activity indicator); this
  keeps viewEquals false per insert by design, not by accident. (Upstream
  issue to file: insertAbove should restore the cursor itself.)
- **Type-ahead queue rendering**: queued submits show as dim "»" lines above
  the separator (content side — inputs about to become history), one row
  each (ANSI-aware truncation), capped at 3 shown + "+N more"; commands keep
  their highlight. On interrupt the queue joins (newline-separated) into the
  composer draft, ahead of any half-typed text; when input history (↑↓)
  lands in P3, queued items are also pushed there individually so message
  boundaries stay recoverable.

## Non-TTY / Once

`chat.Once` (-m) never constructs the ui facade — plain fmt output, as today.
The markdown renderer's no-preview sink + NoColor keeps piped output clean.
Interactive mode on a non-terminal exits with an error pointing at
-m/--message (the readline path that used to serve pipes is gone).

## Migration phases (amended; P0–P5 done, P6 open)

P5 landed 2026-07-14: the default flipped, `--ui` was removed, and the old
stack was deleted (`internal/readline`, `internal/promptui`, the readline Run
loop, composer chrome, paste filter, cancel watch, spinner). Pre-REPL
interactions moved into the Program: the startup model pick and the -S
system-prompt read run as pre-loop interactions in chat/run.go; the --resume
picker is a one-shot surface-only Program (`ui.RunSurface`). Ported contracts:
window-title sanitize/truncate lives in ui.SetTitle (the renderer emits raw
OSC); title/status fallbacks and status-field hues re-pinned in tests.

- **P0**: add a CI test workflow (`go vet && go test -race ./...`) — the
  whole plan leans on "tests stay green" and nothing enforces it today.
- **P1**: extract `internal/markdown` with the Sink seam, old-UI StreamView
  sink, re-plumb the five hardcoded stdout call sites. Zero behavior change.
- **P2**: `internal/ui` facade + Program behind `--ui=v2`, with **minimal
  versions of EVERY in-loop surface** (plain list selectors, static viewers,
  Busy, stream sink, status, title, user block, queue, interrupts). Rationale:
  once the Program owns stdin, any surviving old-stack call on the v2 path is
  a second raw-mode owner — P2 is not shippable until the v2 path never
  touches promptui/readline/spinner. Pre-REPL interactions (system prompt,
  model pick, --resume picker) start the Program first and use the facade.
  Run loop: fork `RunV2` but share all pure logic helpers; freeze feature
  work on the old loop during the window; keep the window short.
- **P3**: fidelity ports — tabbed selectors (multi-select, slider,
  commit-all), directory browser, live-refresh viewers, slash auto-popup
  completion menu, paste tags via tea's paste events, input history ↑↓.
  Port the assertions from completion_test.go / inputchrome_test.go as each
  contract lands (those tests are the executable spec; they die with the old
  stack in P5, their assertions must not).
- **P4**: edge integration — ESC/Ctrl+C matrix, partial-output capture,
  session-resume replay echo, MCP failure reports, compaction UI, export
  prompts, agent-mode surfaces.
- **P5**: parity checklist sign-off → flip the default → delete
  `internal/readline`, `internal/promptui`, the flag, and the old loop.
- **P6** (cleanup, non-blocking): converge `internal/markdown` from lipgloss
  v1 to `charm.land/lipgloss/v2`, eliminating the dual-major coexistence.
  Gated by the markdown test suite plus emoji/CJK table re-validation: v2's
  internal width ruler changed (go-runewidth → displaywidth), which is
  exactly the two-rulers trap recorded in the terminal-emoji-width lesson.
  Until P6, the dual majors cost only binary size — functionally inert.

## Parity checklist (P5 gate, ranked) — PASSED 2026-07-14

Signed off through dogfooding (one report → one fix, until dry). Notes:
Terminal.app bottom-row CJK IME resolved by construction — the wrapped-composer
layout keeps the composer off the physical bottom row (the status line pads
it); interrupt matrix and queue-restore are pinned in internal/ui/model_test.go;
resize reflow orphans remain an accepted cost (below). Original ranking:

Tier 1 (crash/corruption): Terminal.app bottom-row CJK IME (the old
reserveBottomLines protection is gone — must retest composer-at-bottom with a
wrapping candidate window); resize mid-stream orphan-row budget; interrupt
matrix (ESC vs Ctrl+C at every state); queued-input recovery on cancel.
Tier 2 (fidelity): paste tags round-trip, slash menu popup+filter+highlight,
multi-row composer wrap/collapse, status truncation, session replay echo,
piped/non-TTY output byte-parity, /model slider semantics, /debug live
refresh + drill-down, title set/restore.
Tier 3 (polish): spinner elapsed labels, activity glyph, queued-line styling.

## Accepted costs

- Resize reflow orphans on rewrapping terminals (do nothing during desync).
- Cursor flicker window per insert (upstream fix pending).
- Commands wait for turn completion (v1 scope).
- Dual lipgloss majors + toolchain bump during the window.
- The composer floats above the screen bottom transiently after a surface
  closes or the input collapses, until the next output refills the vacated
  rows (an idle session keeps the float — it reads as normal scroll state).

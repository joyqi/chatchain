# ESC interrupt — cancel a streaming turn, persist what's worth keeping

## Goal

While the assistant is streaming (thinking or emitting text), ESC — or Ctrl+C —
interrupts the turn immediately, leaves the terminal clean, and the session
bundle records exactly what the user actually saw.

Scope: interactive `Run` only. `Once` (non-interactive, `chat.Once` →
`executeWithTools`) keeps its uncancellable behavior — no interrupt handling.
The old Windows caveat is gone: cancellation is now key routing inside the
bubbletea Program, which is platform-neutral (no raw-mode watcher, no build
tags).

## Mechanism

The bubbletea Program (`internal/ui`) owns stdin for the whole session, so
there is no separate raw-mode watcher — ESC and Ctrl+C are ordinary key events
routed by the ui model (`internal/ui/model.go`, `updateKey`). The model keeps a
**cancel-scope stack**:

- `chat/run.go` wraps each turn in `context.WithCancel` and registers the
  turn's cancel as the bottom scope via `u.StartStream(cancelTurn)` (which also
  opens the turn's output sink); `sink.Done()` pops it when the turn ends.
- During tool execution, `toolLoop` wraps each tool call in its own
  `context.WithCancel` (a child of the turn context) and pushes it as an inner
  scope via `u.PushCancelScope(cancel)`, popped right after the call returns.
- **ESC fires the innermost (top) scope**: while a tool runs, it cancels only
  that tool — the tool's error result is fed back to the model and the loop
  continues; while only the turn scope is on the stack (streaming), it cancels
  the turn.
- **Ctrl+C fires the bottom (turn) scope**: the turn context is cancelled, and
  any running tool's child context with it — `toolLoop` sees the turn context
  cancelled and returns `errInterrupted`.
- At idle (empty stack), Ctrl+C/Ctrl+D end the session (`ErrInterrupted` from
  `ReadInput`); ESC does nothing.

A fired cancel reaches the provider as a context cancellation. The streaming
sections (`streamTurn`, `streamToolRound` in chat/run.go) map any stream error
observed after our context was cancelled to the `errInterrupted` sentinel
(chat/interrupt.go), so the caller can distinguish "user stopped it" from a
real failure (no retry — `isRetryable` excludes it — no error styling).

Type-ahead is not dropped: the composer stays live during a turn, and Enter
queues submits inside the ui (drained in order by `ReadInput`). On any fired
cancel, `fireCancel` atomically folds the queued submits back into the
composer draft — an interrupt means "the situation changed", so nothing
auto-sends.

Cleanup is owned by the Program, not the success path: the reasoning rolling
window's `finish()` is deferred and done-guarded, the `Busy` spinner's stop
runs on all paths, and `sink.Done()` closes any leaked preview when it pops
the turn scope. After cleanup, `interruptTurn` prints a dim `Interrupted.`
line into scrollback.

## Capturing the partial reply

`streamTurn` / `streamToolRound` return the text from the provider call's
completion value, so an interrupted stream would yield nothing on its own.
Both pipes are teed into buffers (`io.MultiWriter`; `teeWriteCloser` preserves
`Close` on the reasoning pipe) as they are rendered; on interruption the
buffered partial content/reasoning is returned alongside `errInterrupted` and
handed to `interruptTurn`.

## Persistence rules (the design decision)

| state at interrupt | in-memory history | disk |
|---|---|---|
| partial text exists | user msg + completed tool rounds + partial assistant msg (`Interrupted: true`, **no raw content**) | same — persisted |
| no text, no completed tool rounds (still thinking) | whole turn dropped (user msg rolled back) | nothing persisted |
| no text, but completed tool rounds (side effects happened) | user msg + tool rounds kept, no trailing assistant msg | same — persisted |

Rationale:

- A kept partial lets the user type "continue" next turn.
- A zero-yield turn is dropped entirely: a dangling user message would break
  providers with role-alternation constraints, and there is nothing of value.
- Completed tool rounds are never hidden: their side effects (e.g.
  `run_command`) already happened; hiding them from the model is worse than an
  odd history shape. Implementations must verify each provider's request
  builder tolerates a history ending in tool results followed by a new user
  message (merge if needed).
- The interrupted assistant message carries **no raw content**: a partial
  provider-specific blob (thought signatures, half-open tool_use blocks) may
  be invalid on replay; plain text is always safe.

In-memory history and the persistence watermark move together — after an
interrupt they are always consistent (what's in memory is exactly what's on
disk plus the not-yet-persisted nothing).

## Format changes

- `provider.Message` gains `Interrupted bool`.
- `sessionMessage` gains `interrupted,omitempty` — old readers ignore unknown
  fields, old files simply lack it; fully compatible both ways.
- `loadLog`/resume replays interrupted messages as ordinary history (their
  content is the partial text the user saw).

## Non-goals

- No partial raw-content persistence.
- No resume-time "regenerate the interrupted reply" affordance.
- No SIGINT handler: the Program keeps the terminal in raw mode, so Ctrl+C is
  a key event, routed by scope state — cancel mid-turn, end the session at
  idle.

## Testing

- The persist/rollback decision is factored into a pure function
  (`finalizeInterrupt`: history + watermark + partials in → new history +
  to-persist out) and unit tested for all three table rows.
- Session round-trip test: `interrupted` flag survives append → load.
- No terminal emulation (project decision).

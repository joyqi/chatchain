# ESC interrupt — cancel a streaming turn, persist what's worth keeping

## Goal

While the assistant is streaming (thinking or emitting text), ESC — or Ctrl+C —
interrupts the turn immediately, leaves the terminal clean, and the session
bundle records exactly what the user actually saw.

Scope: interactive `Run` only. `Once` (non-interactive) and Windows keep
today's behavior (the cancel watch is already a no-op on Windows, matching
tool cancellation).

## Mechanism

- Each streaming section — `streamResponse`, and the streaming part of each
  `executeWithTools` round — wraps its provider call in
  `context.WithCancel` and runs `startCancelWatch(cancel)`
  (chat/cancelwatch_unix.go) for its duration, stopped via defer. The watch's
  raw mode is input-only — it keeps OPOST, like the vendored readline term
  package — so streamed output renders normally while the watch is active.
- ESC (0x1b) or Ctrl+C (0x03) → `cancel()` → the SDK stream returns a
  context-cancellation error → mapped to the `errInterrupted` sentinel
  (detected via `errors.Is(err, context.Canceled)` when our cancel fired), so
  the caller can distinguish "user stopped it" from a real failure (no retry,
  no error styling).
- The **tool-execution phase keeps its existing behavior**: `callTool` runs its
  own watch; ESC there cancels only the running tool and feeds the error back
  to the model. The streaming watch is never active at the same time (scoped
  per section), so two watchers never fight over stdin.
- Keystrokes other than a lone ESC / Ctrl+C are consumed and dropped while a
  watch is active (decided trade-off: blind type-ahead during streaming is
  lost, same as during tool execution today). Escape sequences — arrow keys,
  paste markers, terminal replies — also start with 0x1b and are recognized
  and dropped, not misread as ESC (a trailing 0x1b is disambiguated by a short
  peek for its continuation).
- Terminal cleanup is deferred, not success-path-only: the reasoning
  viewport's `finish()` and the spinner's stop run on the interrupt path too
  (this also fixes the pre-existing leak where a mid-stream error left the
  ticker running). After cleanup, print a dim `Interrupted.` line.

## Capturing the partial reply

`streamResponse` returns the text from `StreamChat`'s completion value, so an
interrupted stream previously yielded nothing. Both pipes are now teed into
buffers (`io.MultiWriter`) as they are rendered; on interruption the buffered
partial content/reasoning is returned alongside `errInterrupted`.

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
- No signal (SIGINT) handler outside the watched sections; at the readline
  prompt Ctrl+C keeps its readline meaning.

## Testing

- The persist/rollback decision is factored into a pure function
  (history + watermark + partials in → new history + to-persist out) and unit
  tested for all three table rows.
- Session round-trip test: `interrupted` flag survives append → load.
- No terminal emulation (project decision).

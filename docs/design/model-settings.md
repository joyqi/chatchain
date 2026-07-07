# Model Settings — merged /model command (model · context · effort · temperature)

## Goal

Merge `/model` and `/context` into a single tabbed `/model` command that owns all
per-session model tuning, add two new knobs (reasoning effort, temperature), and
make every knob persist to the session bundle and replay on resume.

Tabs (a `Tabbed` from `docs/design/tabbed-select.md`):

1. **Model** — pick the model (existing `/model` behavior)
2. **Context** — pick the context window (existing `/context` behavior)
3. **Effort** — reasoning effort: `default · low · medium · high · xhigh · max`
   (`default` = the parameter is not sent at all)
4. **Temperature** — a slider; `default` = the parameter is not sent at all

`/context` is removed from `slashCommands` (same retirement as `/files`,
`/sessions`, `/mcp`). `--context-window` and config `context_window` stay.

## Commit semantics: questionnaire mode

Unlike `/file`/`/session` (act on the focused tab), `/model` applies **all four
tabs** on Enter. Every tab opens pre-seeded with the current value, so untouched
tabs are no-ops. To keep that invariant, each list tab **inserts the current
value as a row when it is missing** from the stock rows: a non-preset window, an
unknown effort level, and — critically — a current model absent from the
freshly fetched list (delisted model, different naming scheme, or no model
selected yet, which gets a `(not selected)` row representing ""); otherwise a
plain Enter would silently adopt row 0. This exercises the Tabbed component's "read every panel"
questionnaire mode. After commit, print one line per knob that actually changed.
Esc/q/Ctrl+C cancels everything.

## Pre-merge behavior being replaced

- `/model` (chat.go): FetchModels (spinner) → SelectModel → `p.SetModel` +
  `sw.SetModel`. Aborts if the model list fetch fails — keep that.
- `/context` (chat.go): presets `8k 32k 128k 200k 256k 1m` via
  `pickContextWindow`, or `/context <size>` parsed by `ParseWindowSize`;
  runtime-only `budget.setWindow`, never persisted.

## New promptui pieces (`internal/promptui/tabbed.go`)

- **`ListPanel.SetCursor(i int)`** — pre-position the highlight (current model /
  current preset / current effort).
- **`SliderPanel`** — a numeric slider panel:
  - Fields: `TitleText`, `Min, Max, Step float64`, `Default` label handling,
    value state `*float64` (nil = default/unset), `RuneWidth`.
  - Keys: `←`/`h` decrease, `→`/`l` increase; stepping below `Min` lands on
    **default** (nil); from default, `→` enters at `Min`. `g` jumps to default,
    `G` to `Max`. Enter is not consumed (container commits).
  - Render: header value line (`default (provider decides)` or the number),
    plus a bar like `0.0 ├────●──────┤ 2.0`.
  - `Value() *float64` reads the result. Implements `HelpHint()` with
    slider-specific help.

## Provider layer (optional-capability style)

`baseProvider` already stores `temperature *float64` (nil = omit — exactly the
"default" semantics we need). Add:

```go
// Tunable adjusts sampling/reasoning parameters after construction.
// All providers implement it via baseProvider.
type Tunable interface {
    SetTemperature(t *float64) // nil = provider default (omit the parameter)
    Temperature() *float64
    SetEffort(level string)    // "" = default (omit); low|medium|high|xhigh|max
    Effort() string
}
```

`baseProvider` gains the `effort string` field plus the four methods.

**No level mapping.** The level string is passed **verbatim** to each
provider's effort-shaped parameter; if a model doesn't support a given value,
the API returns a visible, recoverable error and the user picks another level
(or `default`). Every parameter below is a string-typed enum in its SDK, so
verbatim pass-through compiles and reaches the wire. `""` (default) → the
parameter is not sent at all, keeping the request byte-identical to today's.
Parameter choice verified against the SDK versions locked in go.mod
(openai-go v3.29.0, anthropic-sdk-go v1.27.1, genai v1.51.0):

| provider | parameter | value sent |
|---|---|---|
| openai | `ChatCompletionNewParams.ReasoningEffort` (`reasoning_effort`) | level verbatim |
| openresponses | `shared.ReasoningParam{Effort}` (`reasoning.effort`) | level verbatim |
| anthropic | `MessageNewParams.OutputConfig.Effort` (`output_config.effort`, message.go:4135, stable API) | level verbatim |
| gemini / vertexai | `ThinkingConfig.ThinkingLevel` (`thinkingConfig.thinkingLevel`, types.go:2331) + `IncludeThoughts: true` | `strings.ToUpper(level)` (casing convention, not a mapping) |

A dedicated reasoning on/off switch was considered and **rejected**: no
provider has a uniform boolean (openai spells off as `reasoning_effort:
"none"`, anthropic as `thinking: {type: "disabled"}`, gemini as
`thinkingBudget: 0`), so an `off` level would be per-provider translation —
not worth the exception. `default` (send nothing) covers the need: the model's
own default behavior applies.

Notes:

- SDK enum constants existing today: openai `none minimal low medium high
  xhigh`; anthropic `low medium high max`; genai `MINIMAL LOW MEDIUM HIGH`.
  Values outside a model's supported set (e.g. `max` on openai, `xhigh` on
  older claude models, anything on gemini-2.5 which predates thinkingLevel)
  fail loudly at the API — by design. Document in README.
- anthropic `output_config.effort` was chosen over `thinking.budget_tokens`
  (a numeric budget can't carry a level verbatim, and budget brings side
  constraints: `max_tokens > budget`, temperature omission). `MaxTokens: 4096`
  and temperature handling stay untouched.
- gemini `thinkingLevel` was chosen over `thinkingBudget` for the same reason
  (numeric, would require a mapping).
- Temperature range differs: anthropic caps at 1.0, others at 2.0. The chat
  layer picks the slider `Max` by `p.Type()`.

## Session persistence (`chat/session.go`)

`sessionMeta` gains:

```go
ContextWindow int    `json:"context_window,omitempty"`
Effort        string `json:"effort,omitempty"`
```

(`temperature` already exists as `*float64,omitempty`.)

New setters mirroring `SetModel` (update meta in memory; `writeMeta()` only if
the bundle exists; nil-`sw`-safe the same way `SetModel` is — verify and match):
`SetTemperature(*float64)`, `SetEffort(string)`, `SetContextWindow(int)`.

The effective context window is recorded for **brand-new sessions only** (a
guarded `sw.SetContextWindow` at `Run` start, skipped when the writer was
resumed): a one-off `--context-window` override or the config default must not
be baked into an existing bundle, and rewriting meta.json on every resume would
bump `UpdatedAt` and reorder the session picker.

## Resume replay (both paths — this fixes the current gap)

Pre-merge, only **model** was replayed on resume; temperature sat in meta.json
unused, and the context window wasn't even stored.

- **`/session` in-chat resume** (chat.go): after the existing model replay,
  also apply (guarded by `sess.Meta.Provider == p.Type()` like model):
  temperature → `Tunable.SetTemperature`, effort → `SetEffort`,
  `ContextWindow > 0` → `budget.setWindow`.
- **`--resume` at launch** (cmd/root.go): same, with precedence **explicit flag
  > session meta > config > default** — mirror the existing `if model == ""`
  guard: only apply meta temperature when `-t` wasn't passed, only apply meta
  context window when `--context-window` wasn't passed. Effort has no flag, so
  meta always applies.

## /status

Add two lines: `Temperature: default | <value>` and `Effort: default | <level>`
(read via the `Tunable` assertion).

## Testing

- SliderPanel: step/clamp/default transitions, `Value()` nil vs number, render
  snapshot of the bar (logic-level, feed `HandleKey`).
- ListPanel.SetCursor.
- sessionMeta round-trip: write meta with effort/context_window/temperature,
  reload, assert fields survive.
- Resume replay: unit-test the applied values against a fake provider
  implementing `Tunable`.
- No terminal emulation (per project decision).

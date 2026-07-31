# Terminal host integration (internal/host)

Status: **Shipped** · Depends on: internal/ui (the ANSI fallback wraps its facade)

chatchain signals its lifecycle to whatever terminal environment ("host") it
runs inside: a progress indicator while a turn works, a warning state while
it is blocked on the user, and attention pings when something finishes or
needs approval while the user is away. Different hosts speak different
protocols — the plain terminal reads ANSI escapes, cmux ignores them and
listens to its own CLI — so the signals are abstracted once and adapted per
host.

## Shape

```
chat/run.go ──▶ host.Presenter ──▶ Host adapters (capability interfaces)
                                    ├── cmux   (detected: CMUX_SURFACE_ID + CLI on PATH)
                                    └── ANSI   (always present, always last)
```

- **Semantic layer** (`host.go`): `State` (Idle / Busy / NeedsInput / Error)
  and `Event` (an attention ping whose `Text` is always a content digest —
  the answer's first line, the error headline, the tool asking — never a
  fixed phrase). The chat loop emits these at its turn anchors and knows
  nothing about escapes or CLIs.
- **Capability interfaces**: a detected `Host` advertises what it can display
  by implementing `StateReporter`, `Notifier`, and/or `io.Closer` — the same
  optional-interface pattern the provider layer uses (`UsageReporter`,
  `Tunable`, …).
- **Per-capability fallback**: the `Presenter` resolves each signal
  independently — first detected host implementing the capability wins, the
  ANSI fallback covers the rest. cmux implements only `StateReporter`, so
  inside cmux the sidebar shows state while notifications still ride the
  ANSI OSC 9 path (which cmux's ingress surfaces as desktop notifications).
- **Detection registry**: `detectors` in `host.go`, most specific first,
  probing through the injectable `Env` seam (`Getenv`/`LookPath`). Adding a
  host = one file + one registry entry.

## Hosts

**ANSI** (`ansi.go`) — the default. State rides the ConEmu OSC 9;4 progress
protocol through bubbletea's `View.ProgressBar` (the renderer diffs states
and clears on exit; Ghostty 1.2+, Windows Terminal, iTerm2 3.6.6+, kitty
0.47+ render it). Busy → indeterminate, NeedsInput → warning at 100, Error →
error at 100. Notifications are one write of OSC 9 (desktop notification)
plus a BEL, emitted only while the terminal is UNFOCUSED — focus reporting
(`View.ReportFocus`) is an ANSI-host mechanism, so the gate lives in the ui
model.

**cmux** (`cmux.go`) — detected via the `CMUX_SURFACE_ID` env var cmux
injects into every pane plus its CLI on PATH. State drives the sidebar
through the public CLI: `set-status chatchain … --icon … --color …` (the
icons/colors mirror what cmux's own Claude Code integration sends, so the
row reads identically) and `workspace loading on|off --id chatchain` for the
spinner. A single worker goroutine executes commands off the chat loop; a
capacity-1 last-wins mailbox coalesces bursts so a stale state never lands
late; every call is best-effort with a 2s timeout — sidebar dressing must
never slow or fail a turn. `Close` clears the row and spinner (they would
outlive the process) and flushes bounded.

## Contracts

- The `Presenter` is driven from the chat-loop goroutine only; hosts own any
  internal concurrency. It dedups repeated states (command dispatch
  re-asserts Idle liberally; a host may pay per update).
- The config `notify: false` switch is Presenter policy: it silences every
  host's `Notify`. Presence policy ("is anyone watching") is HOST-local: a
  host implementing `Notifier` takes over the whole attention decision —
  ANSI gates by terminal focus, a config-hooks host would not gate at all.
- Notification text carries no program prefix — the host names the sender
  (macOS shows Ghostty/cmux, cmux attributes by pane), and banner space is
  scarce. The OSC 9 `4;`-prefix collision (such a payload parses as a
  progress report) is defused at the MECHANISM: the ui model prepends a
  space, so arbitrary content digests stay safe by construction.

## Future hosts the design anticipates

- **tmux**: OSC must be wrapped in DCS passthrough to cross tmux — a tmux
  host implementing BOTH `StateReporter` and `Notifier` with wrapped
  sequences (bypassing tea's ProgressBar field) is the only place that fix
  can live.
- **User-config hooks**: "on needs-input run this command" is a Host adapter
  built from config, slotting into the same registry.
- **cmux deep integration**: registering as a cmux vault agent unlocks real
  lifecycle (`set_agent_lifecycle` over the control socket: hibernation,
  needs-input aggregation) — at that point cmux suppresses raw OSC
  notifications, and its adapter must implement `Notifier` via `cmux notify`.

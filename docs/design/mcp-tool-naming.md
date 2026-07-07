# MCP Tool-Name Namespacing

## Problem

MCP tools used to be advertised to models under their raw server-side names.
Two configured servers exposing the same tool name (e.g. two servers both
offering `search`) silently collided: `Manager.toolIndex` mapped the raw name
to a session index, so a later server overwrote an earlier one's entry and
every call for that name was routed to the last server registered.

## Wire name

Every MCP tool is now advertised to models — and registered for dispatch — as

```
mcp__<segment>__<tool>
```

where `<segment>` is a wire-name segment the manager assigns to the server at
registration time and `<tool>` is the server-side tool name, sanitized.

### Sanitization

`mcp.sanitizeNameSegment` maps a name to a wire-safe segment: every character
outside `[A-Za-z0-9_]` becomes `_` (hyphens too — Gemini's
`functionDeclaration` name pattern forbids them, while OpenAI and Anthropic
allow `[a-zA-Z0-9_-]{1,64}`), runs of `_` collapse to a single `_`, and
leading/trailing `_` are trimmed; a name that sanitizes to nothing falls back
to `srv`. Collapse + trim guarantee a segment never contains `__`, so the
first `__` after the `mcp__` prefix is always the server/tool separator —
`WireToolName("a", "b__c")` and `WireToolName("a__b", "c")` can no longer
compose the same name.

### Segment assignment

The manager derives each connected server's segment from its name via
`sanitizeNameSegment` and, when that segment is already held by an earlier
server, appends `_2`, `_3`, … deterministically in connect (config) order
(`Manager.assignSegment`). Same-named servers are a reachable config —
repeated `--mcp` command flags are all named by `argv[0]` (e.g. `npx`) — and
so are names that differ only in sanitized characters (`my-server` vs
`my_server`). The assigned segment is exposed as `ServerStatus.Segment` so
callers (the `/tools` list) can recompose exactly what was registered.

### Composition

`mcp.ComposeWireName(segment, tool)` is a pure function: it sanitizes only
the tool part and joins `mcp__<segment>__<tool>`. When sanitizing changes the
tool segment (hyphenated tool names are common in the wild), the name gets
the disambiguating hash suffix described below, so distinct raw names that
sanitize identically stay distinct. `mcp.WireToolName(server, tool)` remains
as `sanitizeNameSegment` + `ComposeWireName` for single-server contexts and
tests; the manager itself always registers through its assigned segments.

### Uniqueness

Wire names are unique because:

- segments are unique per manager (assignment above);
- a segment never contains `__`, so the composition parses unambiguously;
- tool names are unique within one server per the MCP spec;
- lossy or truncated tool segments carry a hash of the unsanitized
  composition.

Built-in tools (`run_command`) are **not** prefixed; the `mcp__` prefix
guarantees they can never collide with MCP tools, so `tool.Merge`'s
first-wins policy stays untouched. As a guardrail, `Manager.addServer` still
checks `toolIndex` before registering: a duplicate wire name (unreachable
through normal registration) is skipped — never overwritten — and reported
through the verbose log.

### 64-char limit

All supported providers cap tool names at 64 characters. If the composed wire
name exceeds 64, it is truncated and suffixed with `_` plus an 8-char (32-bit)
lowercase hex hash of the unsanitized composition (`mcp.ComposeWireName`),
keeping the result unique and within the limit. The truncation eats into the
tool segment (the tail of the composed name), preserving as much of the
prefix as possible. `ComposeWireName` is a pure function, so recomposing a
wire name from `ServerStatus.Segment` plus a raw tool name (as the `/tools`
list does) always matches what was registered.

## Dispatch

`Manager.toolIndex` maps each wire name to a `toolTarget{session, raw}`.
`Manager.CallTool` accepts the wire name and translates it back to the
server's raw tool name for the actual `CallToolParams` — servers never see
the namespaced form. `ServerStatus.Tools` keeps raw names, since the `/tools`
MCP tab lists them grouped under their server where raw is more readable.

## Display name

User-facing sites never show the wire form. `chat.displayToolName` parses it
back to `server:tool` by splitting on the **first** `__` after the `mcp__`
prefix — always the real boundary, because segments never contain `__`; the
tool part after it is preserved verbatim. Any name without the prefix is
returned unchanged. The displayed segments are the sanitized ones
(underscores instead of hyphens, plus the hash suffix on lossy tool names),
which is accepted as-is. Applied at: the tool-call header, the spinner label,
the `/tools` list, the `/compact` summary text, and the HTML export summary
(escaped after formatting).

## Old-session compatibility

Session records store whatever `ToolCall.Name` was at the time, so sessions
saved before this change carry raw names. No migration is needed: history is
not validated on replay, and `displayToolName` passes un-prefixed names
through unchanged.

# Deferred tool loading (`defer`)

Tool schemas are the expensive payload of an agentic request: 200–800
tokens each, paid on EVERY request, and long tool lists measurably degrade
tool selection. The index — names, one-line summaries — is 15–25 tokens per
tool and can be paid once. Deferred loading moves everything it can from
the payload bucket into the index bucket.

## Opt-in: the switch is the summary

```yaml
mcp_servers:
  github:
    command: github-mcp
    defer: "GitHub repos, issues, PRs, code search"
```

`defer` is a string, not a bool, by design: opting in forces writing the
one-line summary the search manifest shows — the retrieval corpus the whole
scheme depends on. Absent = fully advertised (the default, unchanged);
present but blank = loud warning, not deferred. Built-in toolsets never
defer. `--mcp` flag servers have no defer syntax.

## How the model discovers hidden tools

Three tiers, cheapest first:

1. **Group lines inside `search_tools`' own description** — one line per
   deferred server (`github (12 tools): <summary>`), under a hard character
   budget (provider description limits are tight and underdocumented; we
   impose our own ~800 chars). Long summaries clamp per line; overflowing
   groups fold into a `+N more` tail; in the extreme the description
   degrades to bare counts. The description is paid per request, so only
   the bounded group tier lives there — never per-tool listings.
2. **The empty-query catalog** — `search_tools("")` always returns EVERY
   group line with its full summary, then the per-tool index (name +
   clamped one-liner; names only past 50 tools — the result lands in
   history for the session and must stay bounded).
3. **Keyword search** — `search_tools("create pull request")` scores hits
   over the corpus the official implementations use: tool name (double
   weight), description, and parameter names/descriptions (recursively
   flattened — "giftwrap" finds the tool whose option mentions it). The
   **top 5** load (the industry-consensus cap; Anthropic's server-side
   search is fixed at 5) — a generic query cannot dump a whole group into
   the tools array; the result names the overflow and the escape hatches
   (refine, empty-query catalog, exact-name call — the implicit-load path
   is deliberately uncapped). Loaded tools stay advertised for the rest of
   the session (runtime state, not persisted — a resumed session
   re-defers).

## The implicit-load safety net

A model that reads the manifest and CALLS a hidden tool directly — skipping
the search — must not hit "unknown tool". The wrapper treats a direct call
to a hidden-but-known name as an implicit search-and-call: enable, then
execute. This requires routing to see hidden names through `tool.Merge`,
which the `Owner` capability provides (ownership beyond the advertised
list); approval and presentation queries route the same way. Weaker models
degrade to a few wasted tokens, never to a missing capability.

## Live tool set

`search_tools` results must be callable on the very next request, not the
next turn: both tool loops (interactive `toolLoop`, non-interactive
`executeWithTools`) re-query `dispatch.Tools()` at each round boundary.
Side benefit: MCP servers that finish connecting mid-turn now join the
advertised set a round later instead of waiting for the next turn.

Wire-name prefixes (`mcp__<segment>__`) resolve lazily through a closure —
segments are assigned at connect time, so a static mapping at construction
would always be empty. A group whose server has not connected shows as
`(connecting…)` in the manifest and its tools cannot match a search yet.

## defer_mode: the protocol is pluggable

`defer_mode` on the PROVIDER selects which protocol realizes the deferral —
orthogonal to which servers defer. Modes live in a registry
(`tool/defermode.go`, one implementation + one table line per mode);
unknown or unsupported modes warn and fall back to `normal`, and runtime
degradation is part of every future mode's contract (model-level
requirements cannot be judged client-side).

| mode | status | protocol |
|---|---|---|
| `normal` (default) | shipped | client-side search_tools + tools-array growth; every dialect, model, relay; one cache bust per search |
| `reference` | shipped (live-verified: official Anthropic + claude-sonnet-4-5 — the server-side regex search runs and expands references in-response; note Kimi's anthropic-COMPAT endpoint silently ignores `defer_loading`, deferring there simply advertises nothing) | Anthropic `defer_loading`: deferred tools carry the flag plus the server-side `tool_search_tool_regex` entry; search + `tool_reference` expansion happen WITHIN a response, so search-and-call completes in one round with no client round trip (anthropic dialect, Claude 4.5+). Known limitation: the server search blocks are not yet replayed across rounds, so later rounds re-search server-side (cheap; block replay is a planned optimization) |
| `tool-search` | shipped (live-verified: official OpenAI + gpt-5.5 end-to-end; zenmux rejects the tool type with a loud 400) | OpenAI Responses `tool_search` with `execution: "client"`: the provider answers `tool_search_call` items with `tool_search_output` (top-5 via the shared scorer) inside the same logical call — search legs are invisible upstream; the call+output pair joins the raw replay blob so mounted tools persist via history (openresponses dialect, gpt-5.4+) |
| `system-tools` | shipped (live-verified: kimi-k3 on api.kimi.com/coding/v1 end-to-end — the mount message is accepted and the model reads full mounted schemas) | Kimi K3 dynamically-loaded tools: the normal search flow with a FROZEN tools array — loads queue behind PendingLoader and the chat loop appends them to history as system messages carrying `tools` and no `content` (400 otherwise), append-only per the K3 cache contract; chatcomp dialect against Moonshot directly, kimi-k3 only (other models fail server-side with "tokenization failed") |

The shared design law all three vendor protocols follow — and the reason a
plain-text schema mode is NOT on this list: schemas must stay in the
declaration channel, only their PLACEMENT moves out of the cached prefix.
Our A/B experiment (12 models × 3 schema tiers) showed text-only schemas
hard-fail on OpenAI models and Kimi K3 (infinite re-search) and on
Anthropic models (announce-then-stall); they only work on
declaration-lenient models (Gemini, DeepSeek, Llama).

## Costs and when to use it

Enabling a tool changes the `tools` array and busts the provider's prompt
cache once per search; a first use of any deferred capability costs one
extra round trip. With prompt caching available and a SMALL static tool
set, a fully-advertised list can be cheaper — defer pays off for servers
with many tools, relays without caching, or short sessions. That judgment
is left to the user, per server: that is exactly what the per-server
string opt-in is.

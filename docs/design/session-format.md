# Session Persistence Format Design

Status: **Draft** · Target: v1.9

## 1. Background & Motivation

iota's current session persistence (`SaveHistory` / `ImportHistory` in `chat/file.go`) is a human-readable Markdown transcript. It's fine for "glance at the history", but as an **agent session archive it is badly lossy** — it cannot support the core ability to **fully resume a half-finished agent session**.

As the chat evolves toward an agent (multi-step tool loops, reasoning chains, context-budget management), we need a **lossless, resumable, crash-safe** on-disk session format. This document defines that format and the related interface changes.

### 1.1 What the current format loses

| Info | Today | Consequence |
|---|---|---|
| Attachment bytes | Only writes `[Attached: foo.png (image/png)]`, data discarded; import skips the line | Conversations with images/PDFs can't be restored |
| `tool_calls` | Tool-call messages with empty content are skipped entirely | Agent tool trail lost |
| Tool results | Not written at all | Tool round-trips can't be rebuilt |
| `RawContent` | Never serialized | thought-signature / reasoning items lost → reasoning chain breaks on resume |
| `Reasoning` | Written as text but skipped on import | Round-trips to nothing |
| `ToolCallID/Name/IsError` | Lost with the tool message | — |
| Session metadata | No provider/model/temperature/time/version | Resume doesn't know what model to continue with |

Also, import uses line prefixes `You> ` / `Assistant> ` / `System> ` / `Reasoning> ` as separators, so **message bodies containing such lines (a pasted transcript, a markdown quote) get split incorrectly** — it can't even round-trip plain text losslessly.

## 2. Goals & Non-goals

**Goals**
1. Losslessly persist **every field** of `provider.Message`, including `RawContent`.
2. Support full **resume**: after loading under the same provider/model, continue the agent loop seamlessly with an unbroken reasoning chain.
3. **Crash-safe**: if the process dies at any moment, what's already on disk is still valid and loadable.
4. Store attachment bytes losslessly, with dedup.
5. Forward-evolvable: the format carries a version number for future migration.

**Non-goals**
- Don't require `RawContent` to be portable across providers (it's provider-specific by nature). Degrade gracefully when resuming under a different provider.
- Don't persist any **secret** (API keys never hit disk).
- **No human-readable Markdown export.** Since every session is auto-persisted losslessly and resumable at any time, a lossy one-way export has no value. The `/save` `/import` `/export` commands are all removed.
- This doc does not design context budgeting / compaction (separate doc), but §9 explains how the two decouple.

## 3. Overall layout: directory bundle + append-only JSONL

A session is a **directory** on disk:

```
~/.iota/sessions/<session-id>/
  meta.json          # session metadata (small file, rewritten whole)
  messages.jsonl     # one message per line, strictly append-only
  attachments/
    <sha256>         # attachment bytes, content-addressed → natural dedup
```

- `session-id`: a **ULID** (timestamp prefix + randomness, 26 chars, lexicographic order = creation order). Sortable and unique, but **not human-readable** — lists are distinguished by `meta.title` (§4.4); the ULID is only the dir name / internal id. Depends on `github.com/oklog/ulid/v2`.
- `messages.jsonl`: **append-only**. Each new message appends one line and `fsync`s; never a full rewrite.
- `attachments/<sha256>`: messages only reference the hash (content-addressed); the same file referenced by multiple messages is stored once.

**Why append-only JSONL and not one big JSON**: agent sessions can be long and may crash at any time. Append-only guarantees (a) cheap writes (no full rewrite), (b) the file stays valid after a crash (at most the last line is lost), (c) a natural fit for streamed output. (Claude Code's own transcript is `.jsonl` for the same reasons.)

## 4. File schemas

### 4.1 `meta.json`

```jsonc
{
  "v": 1,                                       // schema version
  "id": "01JZ8QK3V9XYZABCDEFGHJKMNP",          // ULID
  "created_at": "2026-05-31T15:30:12+08:00",   // RFC3339
  "updated_at": "2026-05-31T15:42:08+08:00",
  "provider": "vertexai",          // provider type tag, used to match RawContent
  "model": "gemini-2.5-pro",       // last-used model (/model updates it)
  "temperature": 0.7,              // optional
  "base_url": "",                  // optional; non-secret, helps reconstruct the provider
  "title": "Refactor the JSON parser in Go"  // primary human-readable list label, auto-generated (§4.4)
}
```

> Security: `meta.json` **never** stores an API key. `base_url` is usually non-secret; if a user embeds credentials in the URL that's on them (redaction can be added later).

### 4.2 `messages.jsonl` (one line each)

Mirrors every field of `provider.Message`. The session layer uses its own DTOs (no json tags forced onto the `provider` package, keeping it unaware of persistence):

```jsonc
// role=user, with an attachment
{ "role": "user",
  "content": "look at this chart",
  "attachments": [ { "filename": "chart.png", "mime": "image/png", "data_ref": "sha256:9f86d0..." } ] }

// role=assistant, with a tool call + raw reasoning state
{ "role": "assistant",
  "content": "",
  "reasoning": "the user wants the trend analyzed…",
  "tool_calls": [ { "id": "call_1", "name": "analyze", "arguments": { "x": 1 } } ],
  "raw": { "provider": "vertexai", "blob": { /* provider-serialized opaque JSON */ } } }

// role=tool, tool result
{ "role": "tool", "tool_call_id": "call_1", "tool_call_name": "analyze", "is_error": false, "content": "trend is up" }

// role=assistant, final text reply — with what the API call that produced it cost
{ "role": "assistant", "content": "the chart shows…", "reasoning": "…",
  "usage": { "in": 14200, "out": 320 } }
```

Empty values are dropped via `omitempty` to keep lines compact.

**`usage`** rides the assistant message the call produced (and the compaction
marker, §4.5 — the summary pass is a billed call too). It is local bookkeeping:
no dialect sends it upstream. Summing it over the WHOLE log — compacted-away
rounds included, since those were paid for — is how a resumed session restores
the cumulative ↑/↓ token figures on the status line instead of restarting at
zero. Sessions written before the field existed contribute nothing and load
unchanged.

### 4.3 DTO definitions (Go)

```go
// chat/session.go (new file)

type sessionMessage struct {
    Role         string              `json:"role"`
    Content      string              `json:"content,omitempty"`
    Reasoning    string              `json:"reasoning,omitempty"`
    Attachments  []sessionAttachment `json:"attachments,omitempty"`
    ToolCalls    []sessionToolCall   `json:"tool_calls,omitempty"`
    ToolCallID   string              `json:"tool_call_id,omitempty"`
    ToolCallName string              `json:"tool_call_name,omitempty"`
    IsError      bool                `json:"is_error,omitempty"`
    Raw          *sessionRaw         `json:"raw,omitempty"`
    Usage        *sessionUsage       `json:"usage,omitempty"`
}

type sessionUsage struct {
    Input      int `json:"in"`
    Output     int `json:"out"`
    CacheRead  int `json:"cache_read,omitempty"`
    CacheWrite int `json:"cache_write,omitempty"`
    Total      int `json:"total,omitempty"`  // the provider's own total, when reported
}

type sessionToolCall struct {
    ID        string         `json:"id"`
    Name      string         `json:"name"`
    Arguments map[string]any `json:"arguments"`
}

type sessionAttachment struct {
    Filename string `json:"filename"`
    MimeType string `json:"mime"`
    DataRef  string `json:"data_ref"` // "sha256:<hex>" → attachments/<hex>
}

type sessionRaw struct {
    Provider string          `json:"provider"` // producing provider type
    Blob     json.RawMessage `json:"blob"`
}
```

### 4.4 Title generation

`meta.title` is the **primary human-readable info** that distinguishes sessions in the `/session` / `--resume` list (the ULID is unreadable). ULID + date alone don't tell the user which is which, so title quality matters. Two-stage:

1. **Immediate placeholder**: the moment the first user message is appended, `title` = that message truncated (first 40 runes). Guarantees a name at all times (even a session abandoned before the first reply has one).
2. **LLM summary upgrade**: fired at the same moment, **asynchronously** (a goroutine racing the turn — not after the reply: a tool-heavy first turn would hold the name hostage for minutes). One `p.Chat()` call asking for a short title ("≤6 words, no quotes, match the message's language"); input is the **first user message only** — the assistant reply is structurally excluded, matching OpenCode/Claude Code/Crush. On success → overwrite `meta.title`; on failure → keep the placeholder (no retry).

Key points: non-blocking; the pass rides a **dedicated provider instance** (`titleP`, cmd-constructed clone) because provider per-call state (usage fields, `lastImages`) is not safe for a request concurrent with the streaming turn; a rolled-back first turn takes the name with it, and a generation counter drops a pass that lands after the rollback (`chat/title.go`); dedicated image providers get no pass (asked for a title they would paint) — their placeholder is the final name; tiny cost (one small call per session, once); reuses `Chat` (no tools, no new interface); ephemeral mode (`--no-save`) names the WINDOW only — nothing persists, and `/save` reapplies the name to the bundle it mints; a disable switch (`session.autotitle: false`) is left for later, on by default.

## 5. RawContent serialization (key design)

`RawContent any` is a provider-specific opaque type (`*genai.Content` / `*openResponsesRawOutput` / openai's raw JSON string, etc.) that generic JSON can't round-trip. Let **the provider own** marshal/unmarshal:

```go
// provider/provider.go — extend the existing RawContentProvider
type RawContentProvider interface {
    LastRawContent() any
    MarshalRawContent(v any) ([]byte, error)      // new: provider raw type → JSON
    UnmarshalRawContent(data []byte) (any, error)  // new: JSON → provider raw type
}

// provider/provider.go — new: used for the meta tag and RawContent matching
type Provider interface {
    // ... existing methods
    Type() string  // "openai" / "anthropic" / "vertexai" / ...
}
```

Per-provider implementation cost (mostly free):
- **openresponses**: raw is `*openResponsesRawOutput{items []json.RawMessage}` — marshal the items array.
- **openai**: raw is already a raw JSON string — store/load verbatim.
- **vertexai / gemini**: raw is `*genai.Content` with JSON tags — `json.Marshal` / `Unmarshal` directly.
- **anthropic**: doesn't implement `RawContentProvider`; without it `raw` is empty and the message degrades to text + tool_calls.

**Write**: for a message with `RawContent != nil`, call the active provider's `MarshalRawContent` and store `{"provider": p.Type(), "blob": <bytes>}`.

**Load (invariant)**:
1. Read the blob's tagged provider.
2. If it equals the loading `provider.Type()` → `UnmarshalRawContent(blob)` restores `Message.RawContent`, reasoning chain intact.
3. If it **doesn't match** (resumed under a different provider) → **drop `raw`**, degrade to `content` + `tool_calls`. Deliberate trade-off: `RawContent` is meaningless across providers.

## 6. Attachments: content-addressed + dedup

- Write: `sha256` the attachment `Data`, write to `attachments/<hex>` (skip if it exists), record `data_ref: "sha256:<hex>"` in the message.
- Read: resolve `data_ref` → read `attachments/<hex>` back into `Data`.
- Dedup: one file referenced by many messages is stored once.
- The `maxFileSize` cap (20MB) is retained.

## 7. Write & crash-safety semantics

The session writer appends to disk at the **same points** history grows (the existing history-append sites in `chat.go`: user message, assistant + tool_calls, tool result, final assistant). Each `Append` `fsync`s so a crash never loses a written line. `meta.json` is small and rewritten whole on each change. On load, `messages.jsonl` is parsed line by line with `json.Unmarshal`, **tolerating and skipping a truncated trailing line** (crash residue) so one bad line never kills the whole session.

## 8. Load / Resume semantics

```go
func LoadSession(id string, p provider.Provider) (*Session, error)
// read meta.json; read messages.jsonl → []provider.Message (ignore a bad last line);
// resolve attachments by data_ref; restore raw if the provider matches, else drop.
func ListSessions() ([]SessionInfo, error)
// scan ~/.iota/sessions/, read each meta.json, sort by updated_at desc.
// SessionInfo: { ID, Title, Model, Provider, UpdatedAt, MessageCount }
```

### 8.1 `--resume` — command-line / startup entry

A flag on each provider subcommand, using cobra's `NoOptDefVal` to support both forms:

- `iota openai --resume` (no value) → at startup, pop the `ListSessions` picker (`chat.PickSession`: a one-shot, surface-only list rendered via `ui.RunSurface` before the chat Program starts — arrow keys to move, Enter picks, Esc cancels); load the selection and enter chat.
- `iota openai --resume=<id>` → load that id directly. **Must use `=`** — with `NoOptDefVal` set, the space form `--resume <id>` would treat `<id>` as the provider positional arg, not the flag value.

Model selection: if `-M` is also given, use it; otherwise **default to the session's `meta.model`** and skip the picker (no need to re-pick a model on resume).

### 8.2 `/session` — in-chat entry

For switching/resuming mid-conversation. `/session` opens a tabbed ui surface with two tabs: **Resume** — a single-select list of all sessions (`title · model · relative time · N messages`) — and **Delete** — multi-select checkboxes (Space toggles; the current session is not listed). Enter commits the focused tab: on Resume, the selection is loaded and **replaces the in-memory history**, continuing the chat; on Delete, the checked session bundles are removed. Switching away from the current session is safe — it's already auto-persisted.

### 8.3 Shared rules

- The bound provider is decided by the `iota <provider>` subcommand; resume **continues under the current provider** and does not rebuild from the session's origin provider (there's no key to rebuild with anyway).
- If the loaded session's `meta.provider == the current provider.Type()` → `RawContent` is restored; otherwise `raw` is dropped per §5.

## 9. Decoupling from context budgeting / compaction (important property)

The session file is the **complete, untruncated source of truth / audit log**; compaction (summarizing old messages to save context window) is a **send-time transform over the in-memory history** that **does not touch disk**.

> **Persist everything; compact at send time.**

This fully decouples the two features: disk is always the complete record (reviewable, replayable, re-runnable under a larger-window model), while compaction only decides "how much to feed the API this turn".

## 10. Command surface changes

Once persistence is **fully automatic**, every "manual save/load" command loses its reason to exist:

| Command | Change |
|---|---|
| Interactive session | **Auto** created under `~/.iota/sessions/<id>/` and continuously appended — no manual action |
| `/save` | **Removed** — everything is already persisted |
| `/import` | **Removed** — replaced by the `/session` picker |
| `/export` | **Not added** — no Markdown export |
| `/session` | **New** — list and resume a session (§8) |
| `/model` | **New** — switch the model mid-chat (§10.1) |

Unchanged commands: `/file`, `/files`, `/clear`, `/mcp`, `/exit`. Old `.md` transcripts are no longer importable; migrate via a one-off script if needed.

### 10.1 `/model` — runtime model switching

Today the model is baked into the provider struct at construction with no runtime setter. `/model` needs the `Provider` interface to gain `Model() string` and `SetModel(model string)` (each provider just reads/writes its own model field — trivial). The command opens a four-tab ui surface — **Model / Context / Effort / Temperature** (the last a slider) — listing the provider's models in the Model tab; Enter commits **all tabs at once**, and a changed model selection is applied via `p.SetModel(selected)`.

- **Same provider, different model**: `RawContent` stays valid (the §5/§8 invariant keys on `provider.Type()`, not the model), so the reasoning chain is unaffected.
- After switching, update the session's `meta.model` — its meaning is "last-used model", which is exactly the `--resume` default when `-M` is omitted (§8.1).
- Different messages in one session may be produced by different models; that's allowed (`messages.jsonl` is provider-format and model-agnostic).

### 10.2 Surface cancel behavior

All interactive surfaces (`/model`, `/session`, the startup pickers) support **cancel**: `q`/`Esc` or `Ctrl+C` closes the surface with **no changes applied**. Keys are handled by the bubbletea event loop — there is no custom ESC stdin wrapper.

- `PickSession` returns an **empty value, not an error**, on cancel, so callers distinguish "cancelled" from "real error".
- **In-chat `/model` `/session`**: cancel → back to the prompt, no side effects.
- **Startup model selection (no `-M`)**: cancel = **lazy selection** — enter chat without a model; the first real message re-opens the picker (`ensureModel`); cancelling again skips the turn.

## 11. Change list

- `provider/provider.go`: add `Type()` / `Model()` / `SetModel()` to `Provider`; add `MarshalRawContent` / `UnmarshalRawContent` to `RawContentProvider`.
- Each provider: implement `Type()` / `Model()` / `SetModel()`; the four with `RawContentProvider` (openai/gemini/vertexai/openresponses) add marshal/unmarshal; anthropic degrades (no raw).
- `chat/session.go` (new): DTOs, `SessionWriter`, `LoadSession`, `ListSessions`/`SessionInfo`, attachment hash store, session-id generation, dir management.
- `chat/chat.go`: `Run` creates a `SessionWriter` and appends at the history-append points; delete `/save`/`/import`, add `/session` and `/model`; drop the `/import` handling in `ReadSystemPrompt` and the `importCompleter`; update `chatCompleter`; ESC cancel + `ensureModel` lazy selection.
- `chat/file.go`: **delete** `SaveHistory` / `ImportHistory`; keep `ReadAttachment`, `DetectMimeType`, `FormatAttachmentList`.
- `cmd/`: add `--resume[=<id>]` (cobra `NoOptDefVal`) and `--no-save` (ephemeral) flags; on resume without `-M`, use the session's `meta.model`; update help text.
- `go.mod`: add `github.com/oklog/ulid/v2`.
- README / docs: update commands and the persistence description.

## 12. Decisions & follow-ups

**Decided:**
- Auto-persist by default + `--no-save` (ephemeral) switch (no disk, no title, no autotitle call).
- ULID session ids; lists distinguished by the auto-generated `meta.title` (§4.4).
- Two-stage title: immediate placeholder + async LLM summary of the first user message, both at first send.

**Follow-up / orthogonal:**
- Per-message token usage / timing (cost auditing) — optional field, addable later.
- `session.autotitle: false` config to disable autotitling — on by default for v1.
- Anthropic-thinking gap: real anthropic-thinking agent resume needs anthropic to implement `RawContentProvider`, enable thinking, and capture signed thinking blocks. Separate work item (the format already reserves the `raw` field).
- Concurrent writes: single-process single-session needs no lock; future multi-process sharing would need a file lock.

# internal/llm — minimal provider API client (SDK replacement)

Status: SHIPPED (2026-07-16) — all four dialects live, SDKs removed; binary 81.5MB → 27.8MB
Research: 7-agent SDK/wire inventory + adversarial critic · Brain: `internal-llm-client`

## Goal

Replace the three official SDKs — `openai-go/v3`, `anthropic-sdk-go`, `google.golang.org/genai` —
with a hand-rolled client under `internal/llm`. We use, per provider, exactly: one chat endpoint
(streaming + non-streaming), one model-list endpoint, and (Responses API) one variant chat
endpoint. The SDKs cost 18MB of binary growth per upgrade cycle (generated API surfaces +
metadata amplification), ~185 packages on the google side alone (grpc/protobuf/s2a/opencensus,
none used on the wire), version churn, and an error taxonomy we can only parse out of strings.

`provider/` keeps its public contract unchanged (`Provider`, `ToolProvider`, `UsageReporter`,
`Tunable`, `RawContentProvider`, `New(...)`) — only its bottom half changes: SDK calls become
`internal/llm` calls. `chat/`, `cmd/`, sessions, and /debug notice nothing.

## Shape

```
internal/llm/
  client.go     — Client{HTTP *http.Client, BaseURL, headers}; do/doStream; retry; errors
  sse.go        — SSE reader: data:/event: fields, one-space strip, comment lines,
                  [DONE] sentinel (drain past it), in-band error surfacing
  chatcomp.go   — OpenAI chat-completions dialect (also every compatible server: deepseek, kimi…)
  responses.go  — OpenAI Responses dialect
  anthropic.go  — Anthropic messages dialect
  google.go     — Gemini/Vertex generateContent dialect (both backends, express auth;
                  see Auth — the planned googleauth.go turned out unnecessary)
```

Dialect clients expose wire-shaped request/response structs (exact JSON tags) plus a streaming
callback surface; `provider/*.go` maps `provider.Message` ↔ wire structs, exactly as it maps to
SDK types today (same code volume, no third representation).

## Wire facts each dialect must implement

Extracted from the SDK sources (versions: openai v3.43.0, anthropic v1.57.0, genai v1.63.0).

Cross-dialect (provider layer, not internal/llm): reasoning models behind relays
that don't parse thinking leak the chat template's inline `<think>…</think>` into
plain content on ANY text dialect (observed: kimi via zenmux on both chatcomp and
responses). Every text provider routes content deltas through `thinkTagSplitter`
(provider/thinktag.go): only a block OPENING the stream counts as reasoning
(a literal tag mid-reply passes through, so replies discussing the tag are safe);
an unclosed block (tool-call round, max_tokens, cancel) is implicitly all
reasoning — `</think>` marks the think→content transition, not end-of-round;
tags split across deltas are held back until resolved. Returned content/reasoning
are clean; raw replay stays verbatim (openai keeps tags inline in rawMsg content
and never duplicates tag-extracted think into the `reasoning` field;
responses/google record verbatim items/parts anyway). The non-streaming Chat
paths (titles, /compact, -m) split the same way.

### chat-completions (openai + compatibles)
- `POST {base}/chat/completions`, `GET {base}/models`; `Authorization: Bearer <key>`.
- Body: model, temperature (omit when nil), reasoning_effort (omit when ""), messages
  (system/user/assistant/tool; user attachments: `image_url` data-URL parts for image/*,
  `file` parts `{file_data:<bare b64>, filename}` otherwise, text part LAST), tools
  (`{type:function, function:{name,description,parameters}}`), stream, stream_options
  `{include_usage:true}`.
- Assistant messages with RawContent replay the recorded JSON VERBATIM (kimi `reasoning`
  round-trip); tool_calls otherwise reconstructed `{id, type:function, function:{name,
  arguments:<json string>}}`.
- Streaming: `data:` chunks; `choices[].delta.content` → content writer; nonstandard
  thinking deltas under `reasoning` AND `reasoning_content` → reasoning writer; tool-call
  deltas accumulate BY `index` (id/name arrive once, arguments concatenate); usage on the
  final chunk (any chunk with total_tokens>0 wins); `[DONE]` ends, keep draining; finish_reason
  captured. A `stream:true` request answered with a plain JSON body (broken compat server) must
  surface a diagnosable error, not silent emptiness.

### responses (OpenAI Responses API)
- `POST {base}/responses`; same auth; no `[DONE]` — terminal events end the stream.
- Event dispatch by the `type` INSIDE the data JSON. `response.output_text.delta` → content;
  `response.reasoning_summary_text.delta` → reasoning; completed output items recorded for
  verbatim replay (reasoning items included — compatibility test case before cutover);
  usage from `response.completed`.
- FIXED in the port: `response.failed` / `response.incomplete` / `error` events map to
  structured `*llm.RespFailure` errors (event name + code + message). Pre-port they fell
  through silently and surfaced as bare `io.EOF`. Pinned by TestOpenResponsesTerminalEvents;
  the provider still tolerates a late error after content already streamed (old behavior).

### anthropic (messages)
- `POST {base}/v1/messages`, `GET {base}/v1/models` (paginate: `after_id=<last_id>` until
  `has_more=false`); headers `x-api-key: <key>`, `anthropic-version: 2023-06-01`.
- Body: model, max_tokens (required — keep today's value logic), system top-level, messages
  with text/image/document(PDF) blocks, tools, temperature, thinking config (budget_tokens).
- Streaming SSE event grammar: message_start / content_block_start / content_block_delta
  (text_delta | thinking_delta | input_json_delta) / content_block_stop / message_delta
  (usage, stop_reason) / ping / error. FIXED in the port: content blocks accumulate BY INDEX
  (the pre-port single-accumulator scheme silently corrupted interleaved parallel tool_use;
  pinned by an interleaved-blocks transcript test).
- Error envelope `{"error":{"type":...,"message":...}}`; 529 overloaded = retryable like 5xx.

### google (generateContent, both backends)
- Gemini: `POST https://generativelanguage.googleapis.com/v1beta/models/{m}:streamGenerateContent?alt=sse`,
  auth `x-goog-api-key`. Vertex (ADC): `POST https://{loc}-aiplatform.googleapis.com/v1beta1/
  projects/{p}/locations/{l}/publishers/google/models/{m}:streamGenerateContent?alt=sse`,
  `Authorization: Bearer`; Vertex express (API key) uses the global host without the
  project/location segment. Same body both backends.
- Body: contents/parts (`text`, `inlineData{data,mimeType}`, `functionCall{id,args,name}`,
  `functionResponse`), systemInstruction, tools `{functionDeclarations}`, generationConfig
  (temperature, thinkingConfig). Vertex service rejects functionCall/Response `id` — keep the
  existing `toolCallIDs=false` switch.
- Streaming: plain SSE (`?alt=sse`), whole `GenerateContentResponse` per event; parts split
  text vs `thought:true`; `thoughtSignature` (base64) preserved OPAQUELY and replayed verbatim
  next round — sessions already persist this Content JSON.
- Usage from `usageMetadata`.

### Auth (Vertex) — resolved: no OAuth needed at all
Implementation finding: cmd/root.go REQUIRES an API key for every provider, so the genai
SDK's ADC/OAuth path was unreachable in chatchain — Vertex has only ever run in "express"
mode (x-goog-api-key on aiplatform.googleapis.com). The port keeps exactly that; the
planned ~200-line stdlib ADC token source was never needed. If ADC-based Vertex auth ever
becomes a requirement, that plan (refresh-token + RS256 JWT-bearer grants, GCE metadata)
is the shape to build. Dropping genai deleted the entire grpc/protobuf/s2a/opencensus tail.

## Cross-cutting contracts (from the critic — each is load-bearing)

1. **Interrupt = ctx cancellation mid-stream.** Every request is built with
   `http.NewRequestWithContext`; the SSE loop exits on body-read error (not just between
   events); deltas are written to the tee incrementally (they ARE the interrupt partials).
2. **Errors become structured**: `llm.StatusError{Status int, Method, URL, Body}` with
   `Error()` embedding the numeric status (keeps chat/chat.go isRetryable working), then
   isRetryable upgraded to type-assert instead of string-parsing. Zero-event streams get a
   named error (today's bare io.EOF identity is load-bearing for non-retryability — preserve
   classification, improve the message).
3. **One retry layer, uniform**: in llm.Client, matching SDK semantics (2 retries; 408/409/
   429/≥5xx + transport errors; `x-should-retry` override; Retry-After[-Ms] honored; exp
   backoff 0.5s·2ⁿ capped 8s with 25% jitter; streams retry only before the first byte).
   Sits ABOVE the injected reqlog client → every attempt appears in /debug. This also gives
   Once/-l retries (today SDK-provided) and google retries (today none).
4. **Timeouts**: never `http.Client.Timeout` (kills streams). When the caller passes no
   client, default transport = `http.DefaultTransport` clone (keeps HTTPS_PROXY + HTTP/2)
   with `ResponseHeaderTimeout` 2min. Follow-up (separate commit): ctx cancel scopes for
   /compact and model fetch — today they hang forever on a dead gateway.
5. **Body close hygiene**: `defer resp.Body.Close()` on every path; drain past `[DONE]`.
6. **Env vars**: we honor ONLY our own key envs (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`,
   `GOOGLE_API_KEY`) + Vertex ADC files. SDK-specific envs (`OPENAI_BASE_URL`,
   `ANTHROPIC_BASE_URL`, `OPENAI_ORG_ID`, `GOOGLE_GEMINI_BASE_URL`, …) STOP working —
   base URLs come from config/-u. Documented breaking change.
7. **RawContent compatibility**: google's persisted blobs are `json.Marshal(*genai.Content)`;
   our replacement Content struct keeps identical JSON tags so existing session blobs decode.
   Pinned by a fixture test built from a real pre-migration session.
8. **JSON encoding**: encoding/json defaults (HTML-escaping on) — two of three SDKs already
   escape; semantically neutral.

## What we consciously lose

- Anthropic's client-side "streaming required over 10min" guard and computed unary timeouts
  (replaced by the uniform header timeout + retry layer).
- WIF/impersonation Vertex auth (ADC files + metadata server only).
- SDK env-var overrides (see table above).
- Automatic wire additions from SDK upgrades (that is the point: we add fields when WE need them).

## Payoff

- go.mod: −3 SDKs and their tails (google alone ≈185 pkgs incl. grpc+protobuf+opencensus);
  expected binary −15–20MB of the current 81MB (and the growth TREND flattens).
- Structured errors end the isRetryable string parsing; /debug sees every retry attempt.
- Two latent streaming bugs fixed by construction (anthropic index accumulation, responses
  terminal events).
- Note: dropping anthropic-sdk removes go.yaml.in/yaml/v4 from the tree — the earlier yaml
  unification question inverts (gopkg.in/yaml.v3 becomes the only engine; migrate by choice,
  not for dedup). Do THIS before the yaml decision.

## Test & rollout plan

- Golden wire tests per dialect: httptest servers assert exact request JSON (fixtures) and
  replay recorded SSE transcripts (happy path, tool rounds, thinking, usage, mid-stream error,
  zero-event stream, plain-JSON-on-stream); tool-call index interleaving case for anthropic.
- RawContent fixtures from real existing sessions (deepseek reasoning replay, vertex thought
  signatures) must round-trip byte-identically.
- Port order, one provider per commit, SDK stays until its provider is swapped and green:
  ① chatcomp (highest usage + compatible-server risk — dogfood against deepseek immediately),
  ② responses, ③ anthropic, ④ google (auth last). Final commit drops the SDK deps.
- Live smoke per provider before its SDK removal (user-run: real keys).

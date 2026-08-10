package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"chatchain/internal/llm"
)

// Compile-time check that AnthropicProvider implements ToolProvider.
var _ ToolProvider = (*AnthropicProvider)(nil)
var _ UsageReporter = (*AnthropicProvider)(nil)
var _ Tunable = (*AnthropicProvider)(nil)
var _ RawContentProvider = (*AnthropicProvider)(nil)

const anthropicDefaultBaseURL = "https://api.anthropic.com"

type AnthropicProvider struct {
	baseProvider
	client llm.Anthropic
	// lastServerBlocks holds the server search blocks (server_tool_use +
	// tool_search_tool_result) of the last response, in arrival order.
	// Replaying them keeps the tool_reference schema expansion alive across
	// rounds; without them the model blind-calls deferred tools with guessed
	// arguments (live-verified 2026-08-05). Capture and replay are gated on
	// the request carrying defer_loading tools (defer_mode "reference") —
	// the blocks belong to that protocol, so the gate needs no endpoint
	// guessing and switching to a non-reference provider strips them.
	lastServerBlocks []json.RawMessage
}

func NewAnthropic(apiKey, baseURL, model string, temperature *float64, httpClient *http.Client) *AnthropicProvider {
	if baseURL == "" {
		baseURL = anthropicDefaultBaseURL
	}
	c := llm.New(baseURL, httpClient)
	c.Header.Set("x-api-key", apiKey)
	c.Header.Set("anthropic-version", "2023-06-01")
	return &AnthropicProvider{
		baseProvider: baseProvider{providerType: "anthropic", model: model, temperature: temperature},
		client:       llm.Anthropic{Client: c},
	}
}

// anthropicRawBlocks is the RawContent payload: the response's server search
// blocks, replayed at the front of the assistant message they came from.
type anthropicRawBlocks struct {
	Blocks []json.RawMessage
}

func (p *AnthropicProvider) LastRawContent() any {
	if len(p.lastServerBlocks) == 0 {
		return nil
	}
	return &anthropicRawBlocks{Blocks: p.lastServerBlocks}
}

func (p *AnthropicProvider) MarshalRawContent(v any) ([]byte, error) {
	rb, ok := v.(*anthropicRawBlocks)
	if !ok || rb == nil || len(rb.Blocks) == 0 {
		return nil, nil
	}
	return json.Marshal(rb.Blocks)
}

func (p *AnthropicProvider) UnmarshalRawContent(data []byte) (any, error) {
	var blocks []json.RawMessage
	if err := json.Unmarshal(data, &blocks); err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return nil, nil
	}
	return &anthropicRawBlocks{Blocks: blocks}, nil
}

func (p *AnthropicProvider) ListModels(ctx context.Context) ([]string, error) {
	models, err := p.client.Models(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	return models, nil
}

func (p *AnthropicProvider) buildRequest(messages []Message, replayServerBlocks bool) *llm.AnthropicRequest {
	req := &llm.AnthropicRequest{
		Model:       p.model,
		MaxTokens:   4096,
		Temperature: p.temperature,
		TopP:        p.topP,
	}
	if p.effort != "" {
		req.OutputConfig = &llm.AnthropicOutputConfig{Effort: p.effort}
	}

	// Buffer for coalescing consecutive tool result messages into one user message
	var pendingToolResults []any

	flushToolResults := func() {
		if len(pendingToolResults) > 0 {
			req.Messages = append(req.Messages, llm.AnthropicMsg{Role: "user", Content: pendingToolResults})
			pendingToolResults = nil
		}
	}

	for _, msg := range messages {
		// User messages merge into any pending tool results instead of flushing
		// them first: an interrupted turn can leave the history ending in tool
		// results directly followed by the next user message (state 3 in
		// docs/design/interrupt.md), and the API requires user/assistant roles
		// to alternate — two consecutive user messages are rejected.
		if msg.Role != "tool" && msg.Role != "user" {
			flushToolResults()
		}
		switch msg.Role {
		case "system":
			if len(msg.Tools) > 0 {
				continue // a system-tools mount (K3 wire shape): chatcomp-only, skip here
			}
			req.System = append(req.System, llm.AnthropicTextBlock{Type: "text", Text: msg.Content})
		case "user":
			var blocks []any
			for _, att := range msg.Attachments {
				switch {
				case strings.HasPrefix(att.MimeType, "image/"):
					blocks = append(blocks, llm.AnthropicImageBlock{Type: "image", Source: llm.AnthropicSource{
						Type:      "base64",
						MediaType: att.MimeType,
						Data:      base64.StdEncoding.EncodeToString(att.Data),
					}})
				case att.MimeType == "application/pdf":
					blocks = append(blocks, llm.AnthropicDocumentBlock{Type: "document", Source: llm.AnthropicSource{
						Type:      "base64",
						MediaType: "application/pdf",
						Data:      base64.StdEncoding.EncodeToString(att.Data),
					}})
				default:
					// Text files: inline as text block
					blocks = append(blocks, llm.AnthropicTextBlock{Type: "text", Text: "[File: " + att.Filename + "]\n" + string(att.Data)})
				}
			}
			blocks = append(blocks, llm.AnthropicTextBlock{Type: "text", Text: msg.Content})
			// Tool results (if any) come first in the merged user message.
			pendingToolResults = append(pendingToolResults, blocks...)
			flushToolResults()
		case "assistant":
			var blocks []any
			// Server search blocks replay ahead of the reconstructed content
			// (search precedes speech; the result block must follow its
			// server_tool_use).
			if raw, ok := msg.RawContent.(*anthropicRawBlocks); ok && raw != nil && replayServerBlocks {
				for _, b := range raw.Blocks {
					blocks = append(blocks, b)
				}
			}
			if len(msg.ToolCalls) > 0 {
				if msg.Content != "" {
					blocks = append(blocks, llm.AnthropicTextBlock{Type: "text", Text: msg.Content})
				}
				for _, tc := range msg.ToolCalls {
					input := tc.Arguments
					if input == nil {
						input = map[string]any{}
					}
					blocks = append(blocks, llm.AnthropicToolUseBlock{Type: "tool_use", ID: tc.ID, Input: input, Name: tc.Name})
				}
			} else if msg.Content != "" || len(blocks) == 0 {
				blocks = append(blocks, llm.AnthropicTextBlock{Type: "text", Text: msg.Content})
			}
			req.Messages = append(req.Messages, llm.AnthropicMsg{Role: "assistant", Content: blocks})
		case "tool":
			// Coalesce consecutive tool results into a single user message
			pendingToolResults = append(pendingToolResults, llm.AnthropicToolResultBlock{
				Type:      "tool_result",
				ToolUseID: msg.ToolCallID,
				Content:   []llm.AnthropicTextBlock{{Type: "text", Text: msg.Content}},
				IsError:   msg.IsError,
			})
		}
	}
	flushToolResults()

	return req
}

func (p *AnthropicProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	p.lastUsageOK = false // this call owns the figures from here (see LastUsage)
	resp, err := p.client.Message(ctx, p.buildRequest(messages, false))
	if err != nil {
		return "", fmt.Errorf("chat error: %w", err)
	}
	if resp.Usage != nil {
		p.setUsage(anthropicUsage(*resp.Usage))
	}
	var result string
	for _, block := range resp.Content {
		if block.Type == "text" {
			result += block.Text
		}
	}
	content, _ := splitInlineThink(result)
	return content, nil
}

func (p *AnthropicProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer, reasoningW io.WriteCloser) (string, string, error) {
	content, reasoning, _, err := p.streamChatInternal(ctx, messages, nil, w, reasoningW)
	return content, reasoning, err
}

func (p *AnthropicProvider) StreamChatWithTools(ctx context.Context, messages []Message, tools []ToolDef, w io.Writer, reasoningW io.WriteCloser) (string, string, []ToolCall, error) {
	return p.streamChatInternal(ctx, messages, tools, w, reasoningW)
}

func (p *AnthropicProvider) streamChatInternal(ctx context.Context, messages []Message, tools []ToolDef, w io.Writer, reasoningW io.WriteCloser) (string, string, []ToolCall, error) {
	// Deferred tools (defer_mode "reference") carry defer_loading and summon
	// the server-side search tool: the API expands matching defs as
	// tool_reference blocks WITHIN a response, so search+call complete in
	// one round. Cross-round reuse works by replaying the captured server
	// blocks (see anthropicRawBlocks); without them the model blind-calls
	// with guessed arguments. anyDeferred is the protocol gate for both
	// capture and replay.
	anyDeferred := false
	for _, t := range tools {
		if t.Deferred {
			anyDeferred = true
			break
		}
	}
	req := p.buildRequest(messages, anyDeferred)
	for _, t := range tools {
		schema := &llm.AnthropicToolSchema{
			Type:       "object",
			Properties: t.InputSchema["properties"],
		}
		if reqd, ok := t.InputSchema["required"].([]any); ok {
			for _, r := range reqd {
				if s, ok := r.(string); ok {
					schema.Required = append(schema.Required, s)
				}
			}
		}
		req.Tools = append(req.Tools, llm.AnthropicTool{
			Name:         t.Name,
			Description:  t.Description,
			InputSchema:  schema,
			DeferLoading: t.Deferred,
		})
	}
	if anyDeferred {
		req.Tools = append(req.Tools, llm.AnthropicTool{
			Type: "tool_search_tool_regex_20251119",
			Name: "tool_search_tool_regex",
		})
	}

	p.lastUsageOK = false
	p.lastServerBlocks = nil // an interrupted stream must not leak stale blocks
	stream, err := p.client.StreamMessage(ctx, req)
	if err != nil {
		return "", "", nil, fmt.Errorf("stream error: %w", err)
	}
	defer stream.Close()

	reasoningClosed := false
	closeReasoning := func() {
		if !reasoningClosed {
			reasoningW.Close()
			reasoningClosed = true
		}
	}
	defer closeReasoning()
	split := newStreamThinkSplitter(w, reasoningW, closeReasoning)

	// Content blocks accumulate BY INDEX: content_block_delta events address
	// blocks by `index` and can interleave across open blocks (parallel
	// tool_use), so a single "current block" accumulator would mix fragments
	// of different blocks. Assembly walks the indexes in order at the end.
	type blockAcc struct {
		kind    string          // "text" | "thinking" | "tool_use" | server block types
		id      string          // tool_use / server_tool_use
		name    string          // tool_use / server_tool_use
		content strings.Builder // text/thinking deltas
		args    strings.Builder // input_json deltas
		raw     json.RawMessage // start-event JSON (server result blocks replay verbatim)
	}
	blocks := make(map[int]*blockAcc)
	block := func(idx int, kind string) *blockAcc {
		acc := blocks[idx]
		if acc == nil {
			acc = &blockAcc{kind: kind}
			blocks[idx] = acc
		}
		return acc
	}
	assemble := func() (full, think string, toolCalls []ToolCall, serverBlocks []json.RawMessage) {
		idxs := make([]int, 0, len(blocks))
		for i := range blocks {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		var text, thinking strings.Builder
		for _, i := range idxs {
			acc := blocks[i]
			switch acc.kind {
			case "text":
				text.WriteString(acc.content.String())
			case "thinking":
				thinking.WriteString(acc.content.String())
			case "tool_use":
				args := map[string]any{}
				if argsStr := acc.args.String(); argsStr != "" {
					json.Unmarshal([]byte(argsStr), &args)
				}
				toolCalls = append(toolCalls, ToolCall{ID: acc.id, Name: acc.name, Arguments: args})
			case "server_tool_use":
				// input streams via input_json_delta; recompose the block.
				input := json.RawMessage("{}")
				if s := acc.args.String(); json.Valid([]byte(s)) && s != "" {
					input = json.RawMessage(s)
				}
				b, _ := json.Marshal(struct {
					Type  string          `json:"type"`
					ID    string          `json:"id"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				}{"server_tool_use", acc.id, acc.name, input})
				serverBlocks = append(serverBlocks, b)
			case "tool_search_tool_result":
				// Arrives complete in content_block_start; replays verbatim.
				if len(acc.raw) > 0 {
					serverBlocks = append(serverBlocks, acc.raw)
				}
			}
		}
		return text.String(), thinking.String(), toolCalls, serverBlocks
	}

	var stopReason string
	// Usage arrives in two events: input (plus cache counts) at message_start,
	// the cumulative output at message_delta. Accumulate here and publish once
	// the output figure lands.
	var usage Usage
	for {
		evt, cerr := stream.Next()
		if cerr == io.EOF {
			break
		}
		if cerr != nil {
			split.flush()
			_, think, _, _ := assemble()
			return split.content.String(), think + split.think.String(), nil, fmt.Errorf("stream error: %w", cerr)
		}
		switch evt.Type {
		case "message_start":
			if evt.Message != nil {
				usage = anthropicUsage(evt.Message.Usage)
			}
		case "content_block_start":
			if evt.ContentBlock != nil {
				blocks[evt.Index] = &blockAcc{
					kind: evt.ContentBlock.Type,
					id:   evt.ContentBlock.ID,
					name: evt.ContentBlock.Name,
					raw:  evt.ContentBlock.Raw,
				}
			}
		case "content_block_delta":
			if evt.Delta == nil {
				continue
			}
			switch evt.Delta.Type {
			case "thinking_delta":
				fmt.Fprint(reasoningW, evt.Delta.Thinking)
				block(evt.Index, "thinking").content.WriteString(evt.Delta.Thinking)
			case "text_delta":
				split.write(evt.Delta.Text)
				block(evt.Index, "text").content.WriteString(evt.Delta.Text)
			case "input_json_delta":
				closeReasoning() // thinking is over once tool args stream
				acc := block(evt.Index, "tool_use")
				acc.args.WriteString(evt.Delta.PartialJSON)
				// Server search args are not a client tool call — raising the
				// lifecycle widget for them would leave it dangling (no
				// CallTool ever settles it).
				if acc.kind == "tool_use" {
					p.notifyToolDelta(acc.name, evt.Delta.PartialJSON)
				}
			}
		case "message_delta":
			if evt.Delta != nil {
				stopReason = evt.Delta.StopReason
			}
			if evt.Usage != nil {
				usage.Output = evt.Usage.OutputTokens // cumulative
				p.setUsage(usage)
			}
		}
	}
	split.flush()
	closeReasoning()

	_, thinkFull, toolCalls, serverBlocks := assemble()
	if anyDeferred {
		// Only reference-protocol responses replay their server blocks; other
		// server tools' blocks (e.g. a future web_search) must not be dragged
		// into history as orphans.
		p.lastServerBlocks = serverBlocks
	}
	full := split.content.String()
	thinkFull += split.think.String()

	// If model requested tool calls, return them (assembled in index order)
	if stopReason == "tool_use" && len(toolCalls) > 0 {
		return full, thinkFull, toolCalls, nil
	}

	return full, thinkFull, nil, nil
}

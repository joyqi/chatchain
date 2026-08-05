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

const anthropicDefaultBaseURL = "https://api.anthropic.com"

type AnthropicProvider struct {
	baseProvider
	client llm.Anthropic
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

func (p *AnthropicProvider) ListModels(ctx context.Context) ([]string, error) {
	models, err := p.client.Models(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	return models, nil
}

func (p *AnthropicProvider) buildRequest(messages []Message) *llm.AnthropicRequest {
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
			if len(msg.ToolCalls) > 0 {
				var blocks []any
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
				req.Messages = append(req.Messages, llm.AnthropicMsg{Role: "assistant", Content: blocks})
			} else {
				req.Messages = append(req.Messages, llm.AnthropicMsg{Role: "assistant", Content: []any{llm.AnthropicTextBlock{Type: "text", Text: msg.Content}}})
			}
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
	resp, err := p.client.Message(ctx, p.buildRequest(messages))
	if err != nil {
		return "", fmt.Errorf("chat error: %w", err)
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
	req := p.buildRequest(messages)

	// Add tool definitions if provided. Deferred tools (defer_mode
	// "reference") carry defer_loading and summon the server-side search
	// tool: the API expands matching defs as tool_reference blocks WITHIN a
	// response, so search+call complete in one round. (Cross-round reuse
	// would need replaying the server blocks — a planned optimization; until
	// then the model re-searches server-side, which costs no client round
	// trip.)
	anyDeferred := false
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
		if t.Deferred {
			anyDeferred = true
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
		kind    string          // "text" | "thinking" | "tool_use"
		id      string          // tool_use
		name    string          // tool_use
		content strings.Builder // text/thinking deltas
		args    strings.Builder // input_json deltas
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
	assemble := func() (full, think string, toolCalls []ToolCall) {
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
			}
		}
		return text.String(), thinking.String(), toolCalls
	}

	var stopReason string
	for {
		evt, cerr := stream.Next()
		if cerr == io.EOF {
			break
		}
		if cerr != nil {
			split.flush()
			_, think, _ := assemble()
			return split.content.String(), think + split.think.String(), nil, fmt.Errorf("stream error: %w", cerr)
		}
		switch evt.Type {
		case "message_start":
			if evt.Message != nil {
				p.lastInput = evt.Message.Usage.InputTokens
			}
		case "content_block_start":
			if evt.ContentBlock != nil {
				blocks[evt.Index] = &blockAcc{
					kind: evt.ContentBlock.Type,
					id:   evt.ContentBlock.ID,
					name: evt.ContentBlock.Name,
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
				p.notifyToolDelta(acc.name, evt.Delta.PartialJSON)
			}
		case "message_delta":
			if evt.Delta != nil {
				stopReason = evt.Delta.StopReason
			}
			if evt.Usage != nil {
				p.lastOutput = evt.Usage.OutputTokens // cumulative
				p.lastUsageOK = true
			}
		}
	}
	split.flush()
	closeReasoning()

	_, thinkFull, toolCalls := assemble()
	full := split.content.String()
	thinkFull += split.think.String()

	// If model requested tool calls, return them (assembled in index order)
	if stopReason == "tool_use" && len(toolCalls) > 0 {
		return full, thinkFull, toolCalls, nil
	}

	return full, thinkFull, nil, nil
}

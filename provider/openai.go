package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"chatchain/internal/llm"
)

// Compile-time check that OpenAIProvider implements ToolProvider.
var _ ToolProvider = (*OpenAIProvider)(nil)
var _ RawContentProvider = (*OpenAIProvider)(nil)
var _ UsageReporter = (*OpenAIProvider)(nil)
var _ Tunable = (*OpenAIProvider)(nil)

const openAIDefaultBaseURL = "https://api.openai.com/v1"

type OpenAIProvider struct {
	baseProvider
	client               llm.ChatComp
	lastAssistantRawJSON string // raw JSON of last assistant message with tool calls
}

func (p *OpenAIProvider) LastRawContent() any {
	if p.lastAssistantRawJSON == "" {
		return nil
	}
	return p.lastAssistantRawJSON
}

// MarshalRawContent stores the raw assistant JSON (already valid JSON) verbatim.
func (p *OpenAIProvider) MarshalRawContent(v any) ([]byte, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("openai: unexpected raw content type %T", v)
	}
	return []byte(s), nil
}

func (p *OpenAIProvider) UnmarshalRawContent(data []byte) (any, error) {
	return string(data), nil
}

func NewOpenAI(apiKey, baseURL, model string, temperature *float64, httpClient *http.Client) *OpenAIProvider {
	if baseURL == "" {
		baseURL = openAIDefaultBaseURL
	}
	c := llm.New(baseURL, httpClient)
	c.Header.Set("Authorization", "Bearer "+apiKey)
	return &OpenAIProvider{
		baseProvider: baseProvider{providerType: "openai", model: model, temperature: temperature},
		client:       llm.ChatComp{Client: c},
	}
}

func (p *OpenAIProvider) ListModels(ctx context.Context) ([]string, error) {
	models, err := p.client.Models(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	return models, nil
}

func (p *OpenAIProvider) buildRequest(messages []Message) *llm.ChatCompRequest {
	req := &llm.ChatCompRequest{
		Model:           p.model,
		Temperature:     p.temperature,
		TopP:            p.topP,
		ReasoningEffort: p.effort,
	}
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			if len(msg.Tools) > 0 {
				// Dynamically loaded tools (defer_mode system-tools): the
				// K3 wire shape — tools, no content.
				tm := llm.ChatToolsMsg{Role: "system"}
				for _, t := range msg.Tools {
					tm.Tools = append(tm.Tools, llm.ChatTool{Type: "function", Function: llm.ChatToolFunction{
						Name:        t.Name,
						Description: t.Description,
						Parameters:  t.InputSchema,
					}})
				}
				req.Messages = append(req.Messages, tm)
				continue
			}
			req.Messages = append(req.Messages, llm.ChatMsg{Role: "system", Content: msg.Content})
		case "user":
			if len(msg.Attachments) > 0 {
				var parts []any
				for _, att := range msg.Attachments {
					if strings.HasPrefix(att.MimeType, "image/") {
						dataURL := "data:" + att.MimeType + ";base64," + base64.StdEncoding.EncodeToString(att.Data)
						parts = append(parts, llm.ChatImagePart{Type: "image_url", ImageURL: llm.ChatImageURL{URL: dataURL}})
					} else {
						parts = append(parts, llm.ChatFilePart{Type: "file", File: llm.ChatFileData{
							FileData: base64.StdEncoding.EncodeToString(att.Data),
							Filename: att.Filename,
						}})
					}
				}
				parts = append(parts, llm.ChatTextPart{Type: "text", Text: msg.Content})
				req.Messages = append(req.Messages, llm.ChatMsg{Role: "user", Content: parts})
			} else {
				req.Messages = append(req.Messages, llm.ChatMsg{Role: "user", Content: msg.Content})
			}
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				// If raw assistant JSON is available, replay it verbatim.
				// This preserves provider-specific fields (e.g. kimi reasoning).
				if rawJSON, ok := msg.RawContent.(string); ok && rawJSON != "" {
					req.Messages = append(req.Messages, json.RawMessage(rawJSON))
					continue
				}
				assistant := llm.ChatMsg{Role: "assistant", Content: msg.Content}
				for _, tc := range msg.ToolCalls {
					argsJSON, _ := json.Marshal(tc.Arguments)
					assistant.ToolCalls = append(assistant.ToolCalls, llm.ChatToolCall{
						ID:   tc.ID,
						Type: "function",
						Function: llm.ChatToolCallFunc{
							Name:      tc.Name,
							Arguments: string(argsJSON),
						},
					})
				}
				req.Messages = append(req.Messages, assistant)
			} else {
				req.Messages = append(req.Messages, llm.ChatMsg{Role: "assistant", Content: msg.Content})
			}
		case "tool":
			req.Messages = append(req.Messages, llm.ChatMsg{Role: "tool", Content: msg.Content, ToolCallID: msg.ToolCallID})
		}
	}
	return req
}

func (p *OpenAIProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	resp, err := p.client.Complete(ctx, p.buildRequest(messages))
	if err != nil {
		return "", fmt.Errorf("chat error: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("no response choices")
	}
	content, _ := splitInlineThink(resp.Choices[0].Message.Content)
	return content, nil
}

func (p *OpenAIProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer, reasoningW io.WriteCloser) (string, string, error) {
	content, reasoning, _, err := p.streamChatInternal(ctx, messages, nil, w, reasoningW)
	return content, reasoning, err
}

func (p *OpenAIProvider) StreamChatWithTools(ctx context.Context, messages []Message, tools []ToolDef, w io.Writer, reasoningW io.WriteCloser) (string, string, []ToolCall, error) {
	return p.streamChatInternal(ctx, messages, tools, w, reasoningW)
}

func (p *OpenAIProvider) streamChatInternal(ctx context.Context, messages []Message, tools []ToolDef, w io.Writer, reasoningW io.WriteCloser) (string, string, []ToolCall, error) {
	req := p.buildRequest(messages)
	p.lastUsageOK = false
	for _, t := range tools {
		req.Tools = append(req.Tools, llm.ChatTool{Type: "function", Function: llm.ChatToolFunction{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		}})
	}

	stream, err := p.client.StreamCompletion(ctx, req)
	if err != nil {
		return "", "", nil, fmt.Errorf("stream error: %w", err)
	}
	defer stream.Close()

	var thinkFull, rawContent string
	reasoningClosed := false
	closeReasoning := func() {
		if !reasoningClosed {
			reasoningW.Close()
			reasoningClosed = true
		}
	}
	defer closeReasoning()
	split := newStreamThinkSplitter(w, reasoningW, closeReasoning)

	// Accumulate tool calls by stream index (id/name arrive once, arguments
	// concatenate across deltas).
	type toolCallAcc struct {
		id   string
		name string
		args strings.Builder
	}
	toolCallMap := make(map[int]*toolCallAcc)
	var finishReason string

	for {
		chunk, cerr := stream.Next()
		if cerr == io.EOF {
			break
		}
		if cerr != nil {
			split.flush()
			return split.content.String(), thinkFull + split.think.String(), nil, fmt.Errorf("stream error: %w", cerr)
		}
		// The final chunk (include_usage) carries token usage and no choices.
		if chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
			p.lastInput = chunk.Usage.PromptTokens
			p.lastOutput = chunk.Usage.CompletionTokens
			p.lastUsageOK = true
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}

			// Thinking deltas: aggregators send `reasoning`, deepseek's own
			// API sends `reasoning_content` — accept either, prefer the first.
			think := choice.Delta.Reasoning
			if think == "" {
				think = choice.Delta.ReasoningContent
			}
			if think != "" {
				fmt.Fprint(reasoningW, think)
				thinkFull += think
			}

			for _, tc := range choice.Delta.ToolCalls {
				idx := tc.Index
				if idx < 0 {
					idx = 0
				}
				acc, ok := toolCallMap[idx]
				if !ok {
					acc = &toolCallAcc{}
					toolCallMap[idx] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name += tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					closeReasoning() // thinking is over once tool args stream
					acc.args.WriteString(tc.Function.Arguments)
					p.notifyToolDelta(acc.name, tc.Function.Arguments)
				}
			}

			if choice.Delta.Content != "" {
				// Verbatim for raw replay (inline think tags included); the
				// splitter routes the display/history copy.
				rawContent += choice.Delta.Content
				split.write(choice.Delta.Content)
			}
		}
	}
	split.flush()
	closeReasoning()
	full := split.content.String()
	reasoning := thinkFull + split.think.String()

	// If the model requested tool calls, parse and return them.
	if finishReason == "tool_calls" && len(toolCallMap) > 0 {
		var toolCalls []ToolCall
		var rawTCs []llm.ChatToolCall
		for i := 0; i < len(toolCallMap); i++ {
			acc, ok := toolCallMap[i]
			if !ok {
				continue
			}
			args := map[string]any{}
			argsStr := acc.args.String()
			if argsStr != "" {
				json.Unmarshal([]byte(argsStr), &args)
			}
			toolCalls = append(toolCalls, ToolCall{ID: acc.id, Name: acc.name, Arguments: args})
			rawTCs = append(rawTCs, llm.ChatToolCall{
				ID:       acc.id,
				Type:     "function",
				Function: llm.ChatToolCallFunc{Name: acc.name, Arguments: argsStr},
			})
		}

		// Save raw assistant message JSON (preserves reasoning for kimi etc.).
		rawMsg := map[string]any{
			"role":       "assistant",
			"content":    rawContent,
			"tool_calls": rawTCs,
		}
		// Field reasoning only: tag-extracted think text is already inline
		// in the verbatim content.
		if thinkFull != "" {
			rawMsg["reasoning"] = thinkFull
		}
		rawJSON, _ := json.Marshal(rawMsg)
		p.lastAssistantRawJSON = string(rawJSON)

		return full, reasoning, toolCalls, nil
	}
	p.lastAssistantRawJSON = ""

	return full, reasoning, nil, nil
}

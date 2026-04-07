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

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Compile-time check that AnthropicProvider implements ToolProvider.
var _ ToolProvider = (*AnthropicProvider)(nil)

type AnthropicProvider struct {
	client      *anthropic.Client
	model       string
	temperature *float64
}

func NewAnthropic(apiKey, baseURL, model string, temperature *float64, httpClient *http.Client) *AnthropicProvider {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	if httpClient != nil {
		opts = append(opts, option.WithHTTPClient(httpClient))
	}

	client := anthropic.NewClient(opts...)
	return &AnthropicProvider{
		client:      &client,
		model:       model,
		temperature: temperature,
	}
}

func (p *AnthropicProvider) ListModels(ctx context.Context) ([]string, error) {
	page, err := p.client.Models.List(ctx, anthropic.ModelListParams{})
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	var models []string
	for _, m := range page.Data {
		models = append(models, m.ID)
	}

	sort.Strings(models)
	return models, nil
}

func (p *AnthropicProvider) buildParams(messages []Message) (anthropic.MessageNewParams, []anthropic.MessageParam) {
	var msgs []anthropic.MessageParam
	var system []anthropic.TextBlockParam
	// Buffer for coalescing consecutive tool result messages into one user message
	var pendingToolResults []anthropic.ContentBlockParamUnion

	flushToolResults := func() {
		if len(pendingToolResults) > 0 {
			msgs = append(msgs, anthropic.NewUserMessage(pendingToolResults...))
			pendingToolResults = nil
		}
	}

	for _, msg := range messages {
		if msg.Role != "tool" {
			flushToolResults()
		}
		switch msg.Role {
		case "system":
			system = append(system, anthropic.TextBlockParam{Text: msg.Content})
		case "user":
			if len(msg.Attachments) > 0 {
				var blocks []anthropic.ContentBlockParamUnion
				for _, att := range msg.Attachments {
					switch {
					case strings.HasPrefix(att.MimeType, "image/"):
						blocks = append(blocks, anthropic.NewImageBlockBase64(att.MimeType, base64.StdEncoding.EncodeToString(att.Data)))
					case att.MimeType == "application/pdf":
						blocks = append(blocks, anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{
							Data: base64.StdEncoding.EncodeToString(att.Data),
						}))
					default:
						// Text files: inline as text block
						blocks = append(blocks, anthropic.NewTextBlock("[File: "+att.Filename+"]\n"+string(att.Data)))
					}
				}
				blocks = append(blocks, anthropic.NewTextBlock(msg.Content))
				msgs = append(msgs, anthropic.NewUserMessage(blocks...))
			} else {
				msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(msg.Content)))
			}
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				var blocks []anthropic.ContentBlockParamUnion
				if msg.Content != "" {
					blocks = append(blocks, anthropic.NewTextBlock(msg.Content))
				}
				for _, tc := range msg.ToolCalls {
					blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, tc.Arguments, tc.Name))
				}
				msgs = append(msgs, anthropic.NewAssistantMessage(blocks...))
			} else {
				msgs = append(msgs, anthropic.NewAssistantMessage(anthropic.NewTextBlock(msg.Content)))
			}
		case "tool":
			// Coalesce consecutive tool results into a single user message
			pendingToolResults = append(pendingToolResults, anthropic.NewToolResultBlock(msg.ToolCallID, msg.Content, msg.IsError))
		}
	}
	flushToolResults()

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: 4096,
		Messages:  msgs,
		System:    system,
	}
	if p.temperature != nil {
		params.Temperature = anthropic.Float(*p.temperature)
	}
	return params, msgs
}

func (p *AnthropicProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	params, _ := p.buildParams(messages)
	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return "", fmt.Errorf("chat error: %w", err)
	}
	var result string
	for _, block := range resp.Content {
		if block.Type == "text" {
			result += block.Text
		}
	}
	return result, nil
}

func (p *AnthropicProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer, reasoningW io.WriteCloser) (string, string, error) {
	content, reasoning, _, err := p.streamChatInternal(ctx, messages, nil, w, reasoningW)
	return content, reasoning, err
}

func (p *AnthropicProvider) StreamChatWithTools(ctx context.Context, messages []Message, tools []ToolDef, w io.Writer, reasoningW io.WriteCloser) (string, string, []ToolCall, error) {
	return p.streamChatInternal(ctx, messages, tools, w, reasoningW)
}

func (p *AnthropicProvider) streamChatInternal(ctx context.Context, messages []Message, tools []ToolDef, w io.Writer, reasoningW io.WriteCloser) (string, string, []ToolCall, error) {
	params, _ := p.buildParams(messages)

	// Add tool definitions if provided
	if len(tools) > 0 {
		for _, t := range tools {
			schema := anthropic.ToolInputSchemaParam{
				Properties: t.InputSchema["properties"],
			}
			if req, ok := t.InputSchema["required"].([]any); ok {
				for _, r := range req {
					if s, ok := r.(string); ok {
						schema.Required = append(schema.Required, s)
					}
				}
			}
			toolParam := anthropic.ToolUnionParamOfTool(schema, t.Name)
			toolParam.OfTool.Description = anthropic.String(t.Description)
			params.Tools = append(params.Tools, toolParam)
		}
	}

	stream := p.client.Messages.NewStreaming(ctx, params)

	var full, thinkFull string
	reasoningClosed := false
	closeReasoning := func() {
		if !reasoningClosed {
			reasoningW.Close()
			reasoningClosed = true
		}
	}

	// Track tool use blocks during streaming
	type toolUseAcc struct {
		id      string
		name    string
		argsJSON strings.Builder
	}
	var currentToolUse *toolUseAcc
	var toolUseBlocks []toolUseAcc
	var stopReason string

	for stream.Next() {
		evt := stream.Current()
		switch evt.Type {
		case "content_block_start":
			if evt.ContentBlock.Type == "tool_use" {
				currentToolUse = &toolUseAcc{
					id:   evt.ContentBlock.ID,
					name: evt.ContentBlock.Name,
				}
			}
		case "content_block_delta":
			if evt.Delta.Type == "thinking_delta" {
				fmt.Fprint(reasoningW, evt.Delta.Thinking)
				thinkFull += evt.Delta.Thinking
			} else if evt.Delta.Type == "text_delta" {
				closeReasoning()
				fmt.Fprint(w, evt.Delta.Text)
				full += evt.Delta.Text
			} else if evt.Delta.Type == "input_json_delta" && currentToolUse != nil {
				currentToolUse.argsJSON.WriteString(evt.Delta.PartialJSON)
			}
		case "content_block_stop":
			if currentToolUse != nil {
				toolUseBlocks = append(toolUseBlocks, *currentToolUse)
				currentToolUse = nil
			}
		case "message_delta":
			stopReason = string(evt.Delta.StopReason)
		}
	}
	closeReasoning()
	if err := stream.Err(); err != nil {
		return full, thinkFull, nil, fmt.Errorf("stream error: %w", err)
	}

	// If model requested tool calls, parse and return them
	if stopReason == "tool_use" && len(toolUseBlocks) > 0 {
		var toolCalls []ToolCall
		for _, tb := range toolUseBlocks {
			var args map[string]any
			if argsStr := tb.argsJSON.String(); argsStr != "" {
				json.Unmarshal([]byte(argsStr), &args)
			}
			toolCalls = append(toolCalls, ToolCall{
				ID:        tb.id,
				Name:      tb.name,
				Arguments: args,
			})
		}
		return full, thinkFull, toolCalls, nil
	}

	return full, thinkFull, nil, nil
}

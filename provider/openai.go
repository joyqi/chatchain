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

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"
)

// Compile-time check that OpenAIProvider implements ToolProvider.
var _ ToolProvider = (*OpenAIProvider)(nil)
var _ RawContentProvider = (*OpenAIProvider)(nil)

type OpenAIProvider struct {
	client               *openai.Client
	model                string
	temperature          *float64
	lastAssistantRawJSON string // raw JSON of last assistant message with tool calls
}

func (p *OpenAIProvider) LastRawContent() any {
	if p.lastAssistantRawJSON == "" {
		return nil
	}
	return p.lastAssistantRawJSON
}

func NewOpenAI(apiKey, baseURL, model string, temperature *float64, httpClient *http.Client) *OpenAIProvider {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	if httpClient != nil {
		opts = append(opts, option.WithHTTPClient(httpClient))
	}

	client := openai.NewClient(opts...)
	return &OpenAIProvider{
		client:      &client,
		model:       model,
		temperature: temperature,
	}
}

func (p *OpenAIProvider) ListModels(ctx context.Context) ([]string, error) {
	page, err := p.client.Models.List(ctx)
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

func (p *OpenAIProvider) buildParams(messages []Message) openai.ChatCompletionNewParams {
	params := openai.ChatCompletionNewParams{
		Model: p.model,
	}
	if p.temperature != nil {
		params.Temperature = openai.Float(*p.temperature)
	}
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			params.Messages = append(params.Messages, openai.SystemMessage(msg.Content))
		case "user":
			if len(msg.Attachments) > 0 {
				var parts []openai.ChatCompletionContentPartUnionParam
				for _, att := range msg.Attachments {
					if strings.HasPrefix(att.MimeType, "image/") {
						dataURL := "data:" + att.MimeType + ";base64," + base64.StdEncoding.EncodeToString(att.Data)
						parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
							URL: dataURL,
						}))
					} else {
						b64 := base64.StdEncoding.EncodeToString(att.Data)
						parts = append(parts, openai.FileContentPart(openai.ChatCompletionContentPartFileFileParam{
							FileData: openai.String(b64),
							Filename: openai.String(att.Filename),
						}))
					}
				}
				parts = append(parts, openai.TextContentPart(msg.Content))
				params.Messages = append(params.Messages, openai.UserMessage(parts))
			} else {
				params.Messages = append(params.Messages, openai.UserMessage(msg.Content))
			}
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				// If raw assistant JSON is available, replay it verbatim.
				// This preserves provider-specific fields (e.g. kimi reasoning).
				if rawJSON, ok := msg.RawContent.(string); ok && rawJSON != "" {
					params.Messages = append(params.Messages, param.Override[openai.ChatCompletionMessageParamUnion](json.RawMessage(rawJSON)))
				} else {
					assistant := openai.AssistantMessage(msg.Content)
					for _, tc := range msg.ToolCalls {
						argsJSON, _ := json.Marshal(tc.Arguments)
						assistant.OfAssistant.ToolCalls = append(assistant.OfAssistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
							OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
								ID: tc.ID,
								Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
									Name:      tc.Name,
									Arguments: string(argsJSON),
								},
							},
						})
					}
					params.Messages = append(params.Messages, assistant)
				}
			} else {
				params.Messages = append(params.Messages, openai.AssistantMessage(msg.Content))
			}
		case "tool":
			params.Messages = append(params.Messages, openai.ToolMessage(msg.Content, msg.ToolCallID))
		}
	}
	return params
}

func (p *OpenAIProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	resp, err := p.client.Chat.Completions.New(ctx, p.buildParams(messages))
	if err != nil {
		return "", fmt.Errorf("chat error: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("no response choices")
	}
	return resp.Choices[0].Message.Content, nil
}

func (p *OpenAIProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer, reasoningW io.WriteCloser) (string, string, error) {
	content, reasoning, _, err := p.streamChatInternal(ctx, messages, nil, w, reasoningW)
	return content, reasoning, err
}

func (p *OpenAIProvider) StreamChatWithTools(ctx context.Context, messages []Message, tools []ToolDef, w io.Writer, reasoningW io.WriteCloser) (string, string, []ToolCall, error) {
	return p.streamChatInternal(ctx, messages, tools, w, reasoningW)
}

func (p *OpenAIProvider) streamChatInternal(ctx context.Context, messages []Message, tools []ToolDef, w io.Writer, reasoningW io.WriteCloser) (string, string, []ToolCall, error) {
	params := p.buildParams(messages)

	// Add tool definitions if provided
	if len(tools) > 0 {
		for _, t := range tools {
			params.Tools = append(params.Tools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
				Name:        t.Name,
				Description: openai.String(t.Description),
				Parameters:  shared.FunctionParameters(t.InputSchema),
			}))
		}
	}

	stream := p.client.Chat.Completions.NewStreaming(ctx, params)
	var full, thinkFull string
	reasoningClosed := false
	closeReasoning := func() {
		if !reasoningClosed {
			reasoningW.Close()
			reasoningClosed = true
		}
	}

	// Accumulate tool calls: index → {id, name, arguments}
	type toolCallAcc struct {
		id   string
		name string
		args strings.Builder
	}
	toolCallMap := make(map[int]*toolCallAcc)
	var finishReason string

	for stream.Next() {
		evt := stream.Current()
		for _, choice := range evt.Choices {
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}

			// Check for reasoning field in raw JSON (DeepSeek and compatible models)
			var extra struct {
				Reasoning *string `json:"reasoning"`
			}
			if err := json.Unmarshal([]byte(choice.Delta.RawJSON()), &extra); err == nil && extra.Reasoning != nil && *extra.Reasoning != "" {
				fmt.Fprint(reasoningW, *extra.Reasoning)
				thinkFull += *extra.Reasoning
			}

			// Accumulate tool call deltas
			for _, tc := range choice.Delta.ToolCalls {
				idx := int(tc.Index)
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
					acc.args.WriteString(tc.Function.Arguments)
				}
			}

			chunk := choice.Delta.Content
			if chunk != "" {
				closeReasoning()
				fmt.Fprint(w, chunk)
				full += chunk
			}
		}
	}
	closeReasoning()
	if err := stream.Err(); err != nil {
		return full, thinkFull, nil, fmt.Errorf("stream error: %w", err)
	}

	// If model requested tool calls, parse and return them
	if finishReason == "tool_calls" && len(toolCallMap) > 0 {
		var toolCalls []ToolCall
		// Build raw tool_calls array for replay
		type rawToolCall struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		var rawTCs []rawToolCall

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
			toolCalls = append(toolCalls, ToolCall{
				ID:        acc.id,
				Name:      acc.name,
				Arguments: args,
			})
			rawTCs = append(rawTCs, rawToolCall{
				ID:   acc.id,
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: acc.name, Arguments: argsStr},
			})
		}

		// Save raw assistant message JSON (preserves reasoning for kimi etc.)
		rawMsg := map[string]any{
			"role":       "assistant",
			"content":    full,
			"tool_calls": rawTCs,
		}
		if thinkFull != "" {
			rawMsg["reasoning"] = thinkFull
		}
		rawJSON, _ := json.Marshal(rawMsg)
		p.lastAssistantRawJSON = string(rawJSON)

		return full, thinkFull, toolCalls, nil
	}
	p.lastAssistantRawJSON = ""

	return full, thinkFull, nil, nil
}

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
	"github.com/openai/openai-go/v3/responses"
)

var _ ToolProvider = (*OpenResponsesProvider)(nil)
var _ RawContentProvider = (*OpenResponsesProvider)(nil)

// openResponsesRawOutput stores the raw output items from a response.completed event.
// These are replayed verbatim as input items in the next round to preserve
// provider-specific fields like reasoning content that the SDK doesn't expose.
type openResponsesRawOutput struct {
	items []json.RawMessage
}

type OpenResponsesProvider struct {
	client           *openai.Client
	model            string
	temperature      *float64
	lastRawOutput    *openResponsesRawOutput
}

func NewOpenResponses(apiKey, baseURL, model string, temperature *float64, httpClient *http.Client) *OpenResponsesProvider {
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
	return &OpenResponsesProvider{
		client:      &client,
		model:       model,
		temperature: temperature,
	}
}

func (p *OpenResponsesProvider) LastRawContent() any {
	return p.lastRawOutput
}

func (p *OpenResponsesProvider) ListModels(ctx context.Context) ([]string, error) {
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

func (p *OpenResponsesProvider) buildParams(messages []Message) responses.ResponseNewParams {
	params := responses.ResponseNewParams{
		Model: p.model,
	}
	if p.temperature != nil {
		params.Temperature = openai.Float(*p.temperature)
	}
	var input responses.ResponseInputParam
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			params.Instructions = openai.String(msg.Content)
		case "user":
			if len(msg.Attachments) > 0 {
				var content responses.ResponseInputMessageContentListParam
				for _, att := range msg.Attachments {
					if strings.HasPrefix(att.MimeType, "image/") {
						dataURL := "data:" + att.MimeType + ";base64," + base64.StdEncoding.EncodeToString(att.Data)
						content = append(content, responses.ResponseInputContentUnionParam{
							OfInputImage: &responses.ResponseInputImageParam{
								ImageURL: param.NewOpt(dataURL),
								Detail:   responses.ResponseInputImageDetailAuto,
							},
						})
					} else {
						b64 := base64.StdEncoding.EncodeToString(att.Data)
						content = append(content, responses.ResponseInputContentUnionParam{
							OfInputFile: &responses.ResponseInputFileParam{
								FileData: param.NewOpt(b64),
								Filename: param.NewOpt(att.Filename),
							},
						})
					}
				}
				content = append(content, responses.ResponseInputContentParamOfInputText(msg.Content))
				input = append(input, responses.ResponseInputItemParamOfMessage(content, responses.EasyInputMessageRoleUser))
			} else {
				input = append(input, responses.ResponseInputItemParamOfMessage(msg.Content, responses.EasyInputMessageRoleUser))
			}
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				// If raw output items are available, replay them verbatim.
				// This preserves provider-specific fields (e.g. kimi reasoning items).
				// Skip "message" type items — some APIs (kimi) reject them on replay.
				if raw, ok := msg.RawContent.(*openResponsesRawOutput); ok && raw != nil {
					for _, item := range raw.items {
						var peek struct{ Type string `json:"type"` }
						if json.Unmarshal(item, &peek) == nil && peek.Type == "message" {
							continue
						}
						input = append(input, param.Override[responses.ResponseInputItemUnionParam](item))
					}
				} else {
					if msg.Content != "" {
						input = append(input, responses.ResponseInputItemParamOfMessage(msg.Content, responses.EasyInputMessageRoleAssistant))
					}
					for _, tc := range msg.ToolCalls {
						argsJSON, _ := json.Marshal(tc.Arguments)
						input = append(input, responses.ResponseInputItemParamOfFunctionCall(string(argsJSON), tc.ID, tc.Name))
					}
				}
			} else {
				input = append(input, responses.ResponseInputItemParamOfMessage(msg.Content, responses.EasyInputMessageRoleAssistant))
			}
		case "tool":
			input = append(input, responses.ResponseInputItemParamOfFunctionCallOutput(msg.ToolCallID, msg.Content))
		}
	}
	params.Input.OfInputItemList = input
	return params
}

func (p *OpenResponsesProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	resp, err := p.client.Responses.New(ctx, p.buildParams(messages))
	if err != nil {
		return "", fmt.Errorf("chat error: %w", err)
	}
	return resp.OutputText(), nil
}

func (p *OpenResponsesProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer, reasoningW io.WriteCloser) (string, string, error) {
	content, reasoning, _, err := p.streamChatInternal(ctx, messages, nil, w, reasoningW)
	return content, reasoning, err
}

func (p *OpenResponsesProvider) StreamChatWithTools(ctx context.Context, messages []Message, tools []ToolDef, w io.Writer, reasoningW io.WriteCloser) (string, string, []ToolCall, error) {
	return p.streamChatInternal(ctx, messages, tools, w, reasoningW)
}

func (p *OpenResponsesProvider) streamChatInternal(ctx context.Context, messages []Message, tools []ToolDef, w io.Writer, reasoningW io.WriteCloser) (string, string, []ToolCall, error) {
	params := p.buildParams(messages)

	if len(tools) > 0 {
		for _, t := range tools {
			params.Tools = append(params.Tools, responses.ToolUnionParam{
				OfFunction: &responses.FunctionToolParam{
					Name:        t.Name,
					Description: param.NewOpt(t.Description),
					Parameters:  t.InputSchema,
					Strict:      param.NewOpt(false),
				},
			})
		}
	}

	stream := p.client.Responses.NewStreaming(ctx, params)
	var full, thinkFull string
	reasoningClosed := false
	closeReasoning := func() {
		if !reasoningClosed {
			reasoningW.Close()
			reasoningClosed = true
		}
	}

	// Track function calls: itemID → accumulated data
	type fnCallAcc struct {
		callID string // the actual call_id (from output_item.done)
		name   string
		args   strings.Builder
	}
	fnCalls := make(map[string]*fnCallAcc)
	// fnCallOrder is populated strictly from response.output_item.done so it
	// stays in lock-step with rawOutputItems. This guarantees the tool_result
	// messages we append later are in the same order as the replayed
	// function_call items, which Anthropic-via-OpenAI-Responses gateways
	// require ("tool_use ids were found without tool_result blocks
	// immediately after").
	var fnCallOrder []string
	var rawOutputItems []json.RawMessage

	for stream.Next() {
		evt := stream.Current()
		switch evt.Type {
		case "response.reasoning_summary_text.delta":
			fmt.Fprint(reasoningW, evt.Delta)
			thinkFull += evt.Delta
		case "response.output_text.delta":
			closeReasoning()
			fmt.Fprint(w, evt.Delta)
			full += evt.Delta
		case "response.function_call_arguments.delta":
			delta := evt.AsResponseFunctionCallArgumentsDelta()
			acc, ok := fnCalls[delta.ItemID]
			if !ok {
				acc = &fnCallAcc{}
				fnCalls[delta.ItemID] = acc
			}
			acc.args.WriteString(delta.Delta)
		case "response.function_call_arguments.done":
			done := evt.AsResponseFunctionCallArgumentsDone()
			acc, ok := fnCalls[done.ItemID]
			if !ok {
				acc = &fnCallAcc{}
				fnCalls[done.ItemID] = acc
			}
			acc.name = done.Name
			acc.args.Reset()
			acc.args.WriteString(done.Arguments)
		case "response.output_item.done":
			item := evt.AsResponseOutputItemDone()
			// Capture every completed output item as raw JSON for replay
			rawOutputItems = append(rawOutputItems, json.RawMessage(item.Item.RawJSON()))
			if item.Item.Type == "function_call" {
				acc, ok := fnCalls[item.Item.ID]
				if !ok {
					acc = &fnCallAcc{}
					fnCalls[item.Item.ID] = acc
				}
				acc.callID = item.Item.CallID
				if item.Item.Name != "" {
					acc.name = item.Item.Name
				}
				fnCallOrder = append(fnCallOrder, item.Item.ID)
			}
		case "response.completed":
			// Stream complete
		default:
			if evt.Delta != "" && evt.Type == "" {
				closeReasoning()
				fmt.Fprint(w, evt.Delta)
				full += evt.Delta
			}
		}
	}
	closeReasoning()
	if err := stream.Err(); err != nil {
		if full != "" || thinkFull != "" || len(fnCalls) > 0 {
			// Ignore stream close errors if we got content
		} else {
			return full, thinkFull, nil, fmt.Errorf("stream error: %w", err)
		}
	}

	if len(fnCalls) > 0 {
		// Save raw output for replay
		p.lastRawOutput = &openResponsesRawOutput{items: rawOutputItems}
		var toolCalls []ToolCall
		for _, itemID := range fnCallOrder {
			acc := fnCalls[itemID]
			args := map[string]any{}
			if argsStr := acc.args.String(); argsStr != "" {
				json.Unmarshal([]byte(argsStr), &args)
			}
			id := acc.callID
			if id == "" {
				id = itemID
			}
			toolCalls = append(toolCalls, ToolCall{
				ID:        id,
				Name:      acc.name,
				Arguments: args,
			})
		}
		return full, thinkFull, toolCalls, nil
	}

	p.lastRawOutput = nil
	return full, thinkFull, nil, nil
}

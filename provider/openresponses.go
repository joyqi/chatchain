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

	// Track function calls. Key by call_id (always unique per call) rather
	// than item.ID: Bedrock-backed gateways (zenmux) collapse parallel tool
	// calls into a single output item, reusing the same `id` across them
	// while giving each a distinct `call_id`. Keying by item.ID would
	// silently collapse multiple calls into one entry and desync from the
	// raw output.
	type fnCallAcc struct {
		callID string
		name   string
		args   strings.Builder
	}
	fnCalls := make(map[string]*fnCallAcc)           // keyed by call_id
	pendingArgs := make(map[string]*strings.Builder) // item_id → accumulated args, flushed on output_item.done
	var fnCallOrder []string
	var rawOutputItems []json.RawMessage

	getPendingArgs := func(itemID string) *strings.Builder {
		b, ok := pendingArgs[itemID]
		if !ok {
			b = &strings.Builder{}
			pendingArgs[itemID] = b
		}
		return b
	}

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
			getPendingArgs(delta.ItemID).WriteString(delta.Delta)
		case "response.function_call_arguments.done":
			done := evt.AsResponseFunctionCallArgumentsDone()
			b := getPendingArgs(done.ItemID)
			b.Reset()
			b.WriteString(done.Arguments)
		case "response.output_item.done":
			item := evt.AsResponseOutputItemDone()
			// Capture every completed output item as raw JSON for replay
			rawOutputItems = append(rawOutputItems, json.RawMessage(item.Item.RawJSON()))
			if item.Item.Type == "function_call" {
				callID := item.Item.CallID
				if callID == "" {
					callID = item.Item.ID
				}
				if _, exists := fnCalls[callID]; !exists {
					acc := &fnCallAcc{
						callID: item.Item.CallID,
						name:   item.Item.Name,
					}
					// Prefer the authoritative arguments from the completed
					// item; fall back to whatever we accumulated via deltas.
					if argsStr := item.Item.Arguments.OfString; argsStr != "" {
						acc.args.WriteString(argsStr)
					} else if b, ok := pendingArgs[item.Item.ID]; ok {
						acc.args.WriteString(b.String())
					}
					fnCalls[callID] = acc
					fnCallOrder = append(fnCallOrder, callID)
				}
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
		// fnCallOrder is keyed by call_id, so each entry is guaranteed
		// unique even when the upstream (zenmux → Bedrock) collapses
		// parallel calls under a shared item.id.
		var toolCalls []ToolCall
		for _, callID := range fnCallOrder {
			acc := fnCalls[callID]
			args := map[string]any{}
			if argsStr := acc.args.String(); argsStr != "" {
				json.Unmarshal([]byte(argsStr), &args)
			}
			toolCalls = append(toolCalls, ToolCall{
				ID:        callID,
				Name:      acc.name,
				Arguments: args,
			})
		}
		// Rewrite function_call items in the raw replay so each has a
		// unique `id`. Bedrock reuses the same id for parallel calls,
		// which some downstream validators reject as duplicate blocks.
		// Substituting id := call_id keeps ids unique while preserving
		// the call_id used to match function_call_output.
		fixedRaw := make([]json.RawMessage, 0, len(rawOutputItems))
		for _, item := range rawOutputItems {
			var peek struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(item, &peek) == nil && peek.Type == "function_call" {
				var obj map[string]json.RawMessage
				if json.Unmarshal(item, &obj) == nil {
					if callIDRaw, ok := obj["call_id"]; ok {
						obj["id"] = callIDRaw
						if reencoded, err := json.Marshal(obj); err == nil {
							fixedRaw = append(fixedRaw, reencoded)
							continue
						}
					}
				}
			}
			fixedRaw = append(fixedRaw, item)
		}
		p.lastRawOutput = &openResponsesRawOutput{items: fixedRaw}
		return full, thinkFull, toolCalls, nil
	}

	p.lastRawOutput = nil
	return full, thinkFull, nil, nil
}

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

var _ ToolProvider = (*OpenResponsesProvider)(nil)
var _ RawContentProvider = (*OpenResponsesProvider)(nil)
var _ UsageReporter = (*OpenResponsesProvider)(nil)
var _ Tunable = (*OpenResponsesProvider)(nil)

// openResponsesRawOutput stores the raw output items from a response.completed
// round. These are replayed verbatim as input items in the next round to
// preserve provider-specific fields like reasoning content.
type openResponsesRawOutput struct {
	items []json.RawMessage
}

type OpenResponsesProvider struct {
	baseProvider
	client        llm.Responses
	lastRawOutput *openResponsesRawOutput
}

func NewOpenResponses(apiKey, baseURL, model string, temperature *float64, httpClient *http.Client) *OpenResponsesProvider {
	if baseURL == "" {
		baseURL = openAIDefaultBaseURL
	}
	c := llm.New(baseURL, httpClient)
	c.Header.Set("Authorization", "Bearer "+apiKey)
	return &OpenResponsesProvider{
		baseProvider: baseProvider{providerType: "openresponses", model: model, temperature: temperature},
		client:       llm.Responses{Client: c},
	}
}

func (p *OpenResponsesProvider) LastRawContent() any {
	return p.lastRawOutput
}

func (p *OpenResponsesProvider) MarshalRawContent(v any) ([]byte, error) {
	r, ok := v.(*openResponsesRawOutput)
	if !ok || r == nil {
		return nil, fmt.Errorf("openresponses: unexpected raw content type %T", v)
	}
	return json.Marshal(r.items)
}

func (p *OpenResponsesProvider) UnmarshalRawContent(data []byte) (any, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, err
	}
	return &openResponsesRawOutput{items: items}, nil
}

func (p *OpenResponsesProvider) ListModels(ctx context.Context) ([]string, error) {
	models, err := p.client.Models(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	return models, nil
}

func (p *OpenResponsesProvider) buildRequest(messages []Message) *llm.RespRequest {
	req := &llm.RespRequest{
		Model:       p.model,
		Temperature: p.temperature,
	}
	if p.effort != "" {
		req.Reasoning = &llm.RespReasoning{Effort: p.effort}
	}
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			// System prompts ride in instructions, never as input items; the
			// last system message wins.
			instructions := msg.Content
			req.Instructions = &instructions
		case "user":
			if len(msg.Attachments) > 0 {
				var parts []any
				for _, att := range msg.Attachments {
					if strings.HasPrefix(att.MimeType, "image/") {
						dataURL := "data:" + att.MimeType + ";base64," + base64.StdEncoding.EncodeToString(att.Data)
						parts = append(parts, llm.RespImagePart{Type: "input_image", ImageURL: dataURL, Detail: "auto"})
					} else {
						parts = append(parts, llm.RespFilePart{Type: "input_file", FileData: base64.StdEncoding.EncodeToString(att.Data), Filename: att.Filename})
					}
				}
				parts = append(parts, llm.RespTextPart{Type: "input_text", Text: msg.Content})
				req.Input = append(req.Input, llm.RespMsg{Role: "user", Content: parts})
			} else {
				req.Input = append(req.Input, llm.RespMsg{Role: "user", Content: msg.Content})
			}
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				// If raw output items are available, replay them verbatim.
				// This preserves provider-specific fields (e.g. kimi reasoning items).
				// Skip "message" type items — some APIs (kimi) reject them on replay.
				if raw, ok := msg.RawContent.(*openResponsesRawOutput); ok && raw != nil {
					for _, item := range raw.items {
						var peek struct {
							Type string `json:"type"`
						}
						if json.Unmarshal(item, &peek) == nil && peek.Type == "message" {
							continue
						}
						req.Input = append(req.Input, item)
					}
				} else {
					if msg.Content != "" {
						req.Input = append(req.Input, llm.RespMsg{Role: "assistant", Content: msg.Content})
					}
					for _, tc := range msg.ToolCalls {
						argsJSON, _ := json.Marshal(tc.Arguments)
						req.Input = append(req.Input, llm.RespFunctionCall{
							Type:      "function_call",
							CallID:    tc.ID,
							Name:      tc.Name,
							Arguments: string(argsJSON),
						})
					}
				}
			} else {
				req.Input = append(req.Input, llm.RespMsg{Role: "assistant", Content: msg.Content})
			}
		case "tool":
			req.Input = append(req.Input, llm.RespFunctionCallOutput{Type: "function_call_output", CallID: msg.ToolCallID, Output: msg.Content})
		}
	}
	return req
}

func (p *OpenResponsesProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	resp, err := p.client.Create(ctx, p.buildRequest(messages))
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
	req := p.buildRequest(messages)
	for _, t := range tools {
		req.Tools = append(req.Tools, llm.RespTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
			Strict:      false,
		})
	}

	p.lastUsageOK = false
	stream, err := p.client.StreamResponse(ctx, req)
	if err != nil {
		return "", "", nil, fmt.Errorf("stream error: %w", err)
	}
	defer stream.Close()

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

	var streamErr error
	for {
		evt, cerr := stream.Next()
		if cerr == io.EOF {
			break
		}
		if cerr != nil {
			streamErr = cerr
			break
		}
		switch evt.Type {
		case "response.reasoning_summary_text.delta":
			fmt.Fprint(reasoningW, evt.Delta)
			thinkFull += evt.Delta
		case "response.output_text.delta":
			closeReasoning()
			fmt.Fprint(w, evt.Delta)
			full += evt.Delta
		case "response.function_call_arguments.delta":
			getPendingArgs(evt.ItemID).WriteString(evt.Delta)
		case "response.function_call_arguments.done":
			b := getPendingArgs(evt.ItemID)
			b.Reset()
			b.WriteString(evt.Arguments)
		case "response.output_item.done":
			// Capture every completed output item as raw JSON for replay.
			rawOutputItems = append(rawOutputItems, evt.Item)
			var item llm.RespOutputItem
			if json.Unmarshal(evt.Item, &item) == nil && item.Type == "function_call" {
				callID := item.CallID
				if callID == "" {
					callID = item.ID
				}
				if _, exists := fnCalls[callID]; !exists {
					acc := &fnCallAcc{
						callID: item.CallID,
						name:   item.Name,
					}
					// Prefer the authoritative arguments from the completed
					// item; fall back to whatever we accumulated via deltas.
					if argsStr := item.ArgumentsString(); argsStr != "" {
						acc.args.WriteString(argsStr)
					} else if b, ok := pendingArgs[item.ID]; ok {
						acc.args.WriteString(b.String())
					}
					fnCalls[callID] = acc
					fnCallOrder = append(fnCallOrder, callID)
				}
			}
		case "response.completed":
			var usage llm.RespUsage
			if evt.Response != nil && evt.Response.Usage != nil {
				usage = *evt.Response.Usage
			}
			p.lastInput = usage.InputTokens
			p.lastOutput = usage.OutputTokens
			p.lastUsageOK = true
		default:
			if evt.Delta != "" && evt.Type == "" {
				closeReasoning()
				fmt.Fprint(w, evt.Delta)
				full += evt.Delta
			}
		}
	}
	closeReasoning()
	if streamErr != nil {
		if full != "" || thinkFull != "" || len(fnCalls) > 0 {
			// Ignore stream close errors if we got content
		} else {
			return full, thinkFull, nil, fmt.Errorf("stream error: %w", streamErr)
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

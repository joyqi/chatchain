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
	imageOutput   bool
	imagePartial  func(data []byte)
	client        llm.Responses
	lastRawOutput *openResponsesRawOutput
	lastImages    []Attachment
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
		TopP:        p.topP,
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
				// function_call items replay WITHOUT their `id`: OpenAI
				// validates the prefix ("Expected an ID that begins with
				// 'fc'"), Bedrock-backed gateways reuse one id across
				// parallel calls (duplicate-block rejections), and blobs
				// persisted by older builds carry a call_-prefixed rewrite —
				// the optional field only causes harm; call_id alone pairs
				// the call with its function_call_output.
				if raw, ok := msg.RawContent.(*openResponsesRawOutput); ok && raw != nil {
					for _, item := range raw.items {
						var peek struct {
							Type string `json:"type"`
						}
						if json.Unmarshal(item, &peek) != nil {
							req.Input = append(req.Input, item)
							continue
						}
						switch peek.Type {
						case "message":
							continue
						case "function_call":
							var obj map[string]json.RawMessage
							if json.Unmarshal(item, &obj) == nil {
								delete(obj, "id")
								if reencoded, err := json.Marshal(obj); err == nil {
									req.Input = append(req.Input, json.RawMessage(reencoded))
									continue
								}
							}
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
	if p.imageOutput {
		// The image switch advertises the server-side built-in on EVERY
		// request path (streaming and unary alike).
		req.Tools = append(req.Tools, llm.RespBuiltinTool{Type: "image_generation", PartialImages: 1})
	}
	return req
}

func (p *OpenResponsesProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	p.lastImages = nil
	resp, err := p.client.Create(ctx, p.buildRequest(messages))
	if err != nil {
		return "", fmt.Errorf("chat error: %w", err)
	}
	// The unary path surfaces generated images too (-m single-shot runs).
	for _, item := range resp.Output {
		if item.Type != "image_generation_call" || item.Result == "" {
			continue
		}
		if data, derr := base64.StdEncoding.DecodeString(item.Result); derr == nil && len(data) > 0 {
			mime := "image/png"
			if item.Format != "" {
				mime = "image/" + item.Format
			}
			p.lastImages = append(p.lastImages, Attachment{MimeType: mime, Data: data})
		}
	}
	content, _ := splitInlineThink(resp.OutputText())
	return content, nil
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
	p.lastImages = nil
	stream, err := p.client.StreamResponse(ctx, req)
	if err != nil {
		return "", "", nil, fmt.Errorf("stream error: %w", err)
	}
	defer stream.Close()

	var thinkFull string
	reasoningClosed := false
	closeReasoning := func() {
		if !reasoningClosed {
			reasoningW.Close()
			reasoningClosed = true
		}
	}
	split := newStreamThinkSplitter(w, reasoningW, closeReasoning)

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
			split.write(evt.Delta)
		case "response.image_generation_call.in_progress", "response.image_generation_call.generating":
			// Generation started: raise the lifecycle widget through the
			// composing observer (the same channel function calls use).
			closeReasoning()
			p.notifyToolDelta("image_generation", "…")
		case "response.image_generation_call.partial_image":
			if p.imagePartial != nil && evt.PartialImageB64 != "" {
				if data, derr := base64.StdEncoding.DecodeString(evt.PartialImageB64); derr == nil && len(data) > 0 {
					p.imagePartial(data)
				}
			}
		case "response.function_call_arguments.delta":
			closeReasoning() // thinking is over once tool args stream
			getPendingArgs(evt.ItemID).WriteString(evt.Delta)
			p.notifyToolDelta("", evt.Delta)
		case "response.function_call_arguments.done":
			b := getPendingArgs(evt.ItemID)
			b.Reset()
			b.WriteString(evt.Arguments)
		case "response.output_item.done":
			// Capture every completed output item as raw JSON for replay.
			var peeked llm.RespOutputItem
			peekOK := json.Unmarshal(evt.Item, &peeked) == nil
			if peekOK && peeked.Type == "image_generation_call" {
				// The generated image: base64 in `result`. Surface it via
				// LastImages; the raw replay keeps the item WITHOUT the
				// payload (megabytes of base64 — the id alone carries the
				// multiturn context server-side).
				if data, derr := base64.StdEncoding.DecodeString(peeked.Result); derr == nil && len(data) > 0 {
					mime := "image/png"
					if peeked.OutputFormat != "" {
						mime = "image/" + peeked.OutputFormat
					}
					p.lastImages = append(p.lastImages, Attachment{MimeType: mime, Data: data})
				}
				var obj map[string]json.RawMessage
				if json.Unmarshal(evt.Item, &obj) == nil {
					delete(obj, "result")
					if reencoded, rerr := json.Marshal(obj); rerr == nil {
						rawOutputItems = append(rawOutputItems, reencoded)
					}
				}
				continue
			}
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
				split.write(evt.Delta)
			}
		}
	}
	split.flush()
	closeReasoning()
	full := split.content.String()
	reasoning := thinkFull + split.think.String()
	if streamErr != nil {
		if full != "" || reasoning != "" || len(fnCalls) > 0 {
			// Ignore stream close errors if we got content
		} else {
			return full, reasoning, nil, fmt.Errorf("stream error: %w", streamErr)
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
		// Raw items are recorded VERBATIM; the id hygiene for replay
		// (stripping function_call ids) happens in buildRequest, so it also
		// heals blobs persisted by older builds that rewrote id := call_id.
		p.lastRawOutput = &openResponsesRawOutput{items: rawOutputItems}
		return full, reasoning, toolCalls, nil
	}

	// An image round also keeps its raw items: the image_generation_call id
	// (payload stripped) carries the server-side multiturn context.
	if len(p.lastImages) > 0 && len(rawOutputItems) > 0 {
		p.lastRawOutput = &openResponsesRawOutput{items: rawOutputItems}
		return full, reasoning, nil, nil
	}
	p.lastRawOutput = nil
	return full, reasoning, nil, nil
}

// SetImageOutput / ImageOutput implement ImageTunable: the switch advertises
// the image_generation built-in tool on requests.
func (p *OpenResponsesProvider) SetImageOutput(on bool) { p.imageOutput = on }
func (p *OpenResponsesProvider) ImageOutput() bool      { return p.imageOutput }

// LastImages implements ImageOutputProvider.
func (p *OpenResponsesProvider) LastImages() []Attachment { return p.lastImages }

// SetImagePartialObserver registers the progressive-frame callback: each
// response.image_generation_call.partial_image event delivers one decoded
// preview frame (a complete low-detail image, later frames refine it). nil
// detaches. Called from the stream goroutine.
func (p *OpenResponsesProvider) SetImagePartialObserver(fn func(data []byte)) {
	p.imagePartial = fn
}

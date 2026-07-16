package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
)

// Anthropic is the Anthropic messages dialect: POST {base}/v1/messages
// (streaming and non-streaming) and GET {base}/v1/models (paginated). The
// static headers (x-api-key, anthropic-version) are set by the provider
// constructor via Client.Header.
type Anthropic struct{ *Client }

// --- request wire shapes (JSON tags are the protocol) ---

// AnthropicMsg is one conversation turn. Content is always an array of typed
// block structs (AnthropicTextBlock, AnthropicImageBlock, ...).
type AnthropicMsg struct {
	Role    string `json:"role"` // "user" | "assistant"
	Content []any  `json:"content"`
}

type AnthropicTextBlock struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

type AnthropicImageBlock struct {
	Type   string          `json:"type"` // "image"
	Source AnthropicSource `json:"source"`
}

type AnthropicDocumentBlock struct {
	Type   string          `json:"type"` // "document"
	Source AnthropicSource `json:"source"`
}

// AnthropicSource is the base64 source of an image or document block.
type AnthropicSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // e.g. "image/png", "application/pdf"
	Data      string `json:"data"`       // base64-encoded bytes
}

// AnthropicToolUseBlock replays an assistant tool call in history.
type AnthropicToolUseBlock struct {
	Type  string         `json:"type"` // "tool_use"
	ID    string         `json:"id"`
	Input map[string]any `json:"input"`
	Name  string         `json:"name"`
}

// AnthropicToolResultBlock answers a tool call. is_error is always emitted,
// even when false (SDK parity).
type AnthropicToolResultBlock struct {
	Type      string               `json:"type"` // "tool_result"
	ToolUseID string               `json:"tool_use_id"`
	Content   []AnthropicTextBlock `json:"content"`
	IsError   bool                 `json:"is_error"`
}

type AnthropicTool struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	InputSchema AnthropicToolSchema `json:"input_schema"`
}

type AnthropicToolSchema struct {
	Type       string   `json:"type"` // "object"
	Properties any      `json:"properties,omitempty"`
	Required   []string `json:"required,omitempty"`
}

type AnthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"` // low|medium|high|xhigh|max
}

type AnthropicRequest struct {
	Model        string                 `json:"model"`
	MaxTokens    int                    `json:"max_tokens"` // required by the API; never omitted
	Messages     []AnthropicMsg         `json:"messages"`
	System       []AnthropicTextBlock   `json:"system,omitempty"` // top-level, not a message role
	Temperature  *float64               `json:"temperature,omitempty"`
	OutputConfig *AnthropicOutputConfig `json:"output_config,omitempty"`
	Tools        []AnthropicTool        `json:"tools,omitempty"`
	Stream       bool                   `json:"stream,omitempty"`
}

// --- response wire shapes ---

type AnthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// AnthropicEvent is one streaming event. The SSE grammar: message_start /
// content_block_start / content_block_delta (text_delta | thinking_delta |
// input_json_delta | signature_delta) / content_block_stop / message_delta
// (usage, stop_reason) / message_stop / ping / error. ping is skipped and
// error becomes the stream error inside Next; neither surfaces as an event.
// content_block_* events address blocks by Index — deltas and stops can
// interleave across open blocks (parallel tool_use), so consumers must
// accumulate per index, never into a single "current block".
type AnthropicEvent struct {
	Type         string                 `json:"type"`
	Index        int                    `json:"index"`         // content_block_*
	Message      *AnthropicEventMessage `json:"message"`       // message_start
	ContentBlock *AnthropicContentBlock `json:"content_block"` // content_block_start
	Delta        *AnthropicDelta        `json:"delta"`         // content_block_delta / message_delta
	Usage        *AnthropicUsage        `json:"usage"`         // message_delta (output_tokens cumulative)
	Error        json.RawMessage        `json:"error"`         // error event envelope
}

type AnthropicEventMessage struct {
	Usage AnthropicUsage `json:"usage"`
}

type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type AnthropicContentBlock struct {
	Type string `json:"type"` // text | thinking | tool_use | ...
	ID   string `json:"id"`   // tool_use
	Name string `json:"name"` // tool_use
}

type AnthropicDelta struct {
	Type        string `json:"type"`         // text_delta | thinking_delta | input_json_delta | signature_delta
	Text        string `json:"text"`         // text_delta
	Thinking    string `json:"thinking"`     // thinking_delta
	PartialJSON string `json:"partial_json"` // input_json_delta
	StopReason  string `json:"stop_reason"`  // message_delta
}

// Message is the non-streaming POST /v1/messages.
func (a Anthropic) Message(ctx context.Context, req *AnthropicRequest) (*AnthropicResponse, error) {
	req.Stream = false
	var out AnthropicResponse
	if err := a.Do(ctx, "POST", "/v1/messages", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StreamMessage is the streaming POST /v1/messages.
func (a Anthropic) StreamMessage(ctx context.Context, req *AnthropicRequest) (*AnthropicStream, error) {
	req.Stream = true
	sse, err := a.Stream(ctx, "POST", "/v1/messages", req)
	if err != nil {
		return nil, err
	}
	return &AnthropicStream{sse: sse}, nil
}

// Models is GET /v1/models, paginated forward (after_id=<last_id> until
// has_more=false), IDs sorted.
func (a Anthropic) Models(ctx context.Context) ([]string, error) {
	var models []string
	afterID := ""
	for {
		path := "/v1/models"
		if afterID != "" {
			path += "?after_id=" + url.QueryEscape(afterID)
		}
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := a.Do(ctx, "GET", path, nil, &out); err != nil {
			return nil, err
		}
		for _, m := range out.Data {
			models = append(models, m.ID)
		}
		if !out.HasMore || out.LastID == "" {
			break
		}
		afterID = out.LastID
	}
	sort.Strings(models)
	return models, nil
}

// AnthropicStream yields events until io.EOF (clean end after message_stop —
// there is no [DONE] sentinel in this dialect). A stream that ends without a
// single event returns ErrNoEvents.
type AnthropicStream struct{ sse *SSE }

func (s *AnthropicStream) Next() (*AnthropicEvent, error) {
	for {
		evt, err := s.sse.Next()
		if err == io.EOF && !s.sse.SawEvent() {
			return nil, ErrNoEvents
		}
		if err != nil {
			return nil, err
		}
		var out AnthropicEvent
		if uerr := json.Unmarshal(evt.Data, &out); uerr != nil {
			return nil, fmt.Errorf("llm: malformed stream event: %w", uerr)
		}
		if out.Type == "" {
			out.Type = evt.Type // fall back to the event: field
		}
		switch out.Type {
		case "ping":
			continue
		case "error":
			// In-band stream error (e.g. overloaded_error arrives on a 200
			// stream, not as HTTP 529): surface the error envelope.
			detail := json.RawMessage(evt.Data)
			if len(out.Error) > 0 {
				detail = out.Error
			}
			return nil, fmt.Errorf("received error while streaming: %s", detail)
		}
		return &out, nil
	}
}

func (s *AnthropicStream) Close() error { return s.sse.Close() }

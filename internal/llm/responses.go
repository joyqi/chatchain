package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Responses is the OpenAI Responses API dialect: POST {base}/responses. The
// endpoint sends no [DONE] sentinel — streams end at EOF after a terminal
// event (response.completed on success; response.failed / response.incomplete
// / error on failure, which Next maps to *RespFailure), and events are
// dispatched by the "type" field INSIDE the data JSON (the SSE event: line is
// redundant and ignored).
type Responses struct{ *Client }

// --- request wire shapes (JSON tags are the protocol) ---

// RespMsg is a message input item ({"role":...,"content":...}; the optional
// "type":"message" discriminator is never sent, matching the SDK).
type RespMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string, or []any of input content parts
}

type RespTextPart struct {
	Type string `json:"type"` // "input_text"
	Text string `json:"text"`
}

// RespImagePart carries the data URL as a plain string — unlike
// chat-completions' nested image_url object.
type RespImagePart struct {
	Type     string `json:"type"`      // "input_image"
	ImageURL string `json:"image_url"` // data URL: data:<mime>;base64,<b64>
	Detail   string `json:"detail"`    // "auto"
}

type RespFilePart struct {
	Type     string `json:"type"`      // "input_file"
	FileData string `json:"file_data"` // bare base64 (not a data URL)
	Filename string `json:"filename"`
}

// RespFunctionCall replays an assistant tool call as an input item.
type RespFunctionCall struct {
	Type      string `json:"type"` // "function_call"
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded string
}

// RespFunctionCallOutput is a tool result input item.
type RespFunctionCallOutput struct {
	Type   string `json:"type"` // "function_call_output"
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// RespTool is a function tool definition. Responses tools are FLAT (no nested
// "function" object like chat-completions); description and strict are always
// emitted, matching the SDK-era wire bytes.
type RespTool struct {
	Type        string         `json:"type"` // "function"
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      bool           `json:"strict"`
	// DeferLoading opts the tool into deferred loading via the tool-search
	// tool (emitted only when true).
	DeferLoading bool `json:"defer_loading,omitempty"`
}

// RespBuiltinTool declares a server-side built-in tool by bare type
// ("image_generation") — no name/description/strict fields.
type RespBuiltinTool struct {
	Type string `json:"type"`
	// PartialImages asks image_generation for progressive preview frames
	// (0–3) via response.image_generation_call.partial_image events.
	PartialImages int `json:"partial_images,omitempty"`
}

type RespReasoning struct {
	Effort string `json:"effort"`
}

// RespToolSearch declares the tool-search server tool with client
// execution: the model emits tool_search_call items and the CLIENT answers
// them with tool_search_output items carrying the loaded subset — deferred
// schemas mount at the end of the context window, preserving the prefix
// cache (the documented guarantee).
type RespToolSearch struct {
	Type      string `json:"type"`      // "tool_search"
	Execution string `json:"execution"` // "client"
	// Description and Parameters are REQUIRED for client execution (the
	// live API rejects their absence): they are what the model sees.
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// RespToolSearchOutput answers one tool_search_call.
type RespToolSearchOutput struct {
	Type      string     `json:"type"` // "tool_search_output"
	CallID    string     `json:"call_id"`
	Status    string     `json:"status"`    // "completed"
	Execution string     `json:"execution"` // "client"
	Tools     []RespTool `json:"tools"`
}

type RespRequest struct {
	Model string `json:"model"`
	// Instructions carries the system prompt (never an input item). A pointer
	// so that an explicitly empty system message still serializes as "".
	Instructions *string        `json:"instructions,omitempty"`
	Input        []any          `json:"input"` // RespMsg / RespFunctionCall / RespFunctionCallOutput / json.RawMessage (verbatim replay)
	Temperature  *float64       `json:"temperature,omitempty"`
	TopP         *float64       `json:"top_p,omitempty"`
	Reasoning    *RespReasoning `json:"reasoning,omitempty"`
	Tools        []any          `json:"tools,omitempty"` // RespTool / RespBuiltinTool
	Stream       bool           `json:"stream,omitempty"`
}

// --- response wire shapes ---

type RespUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type RespError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RespResponse is the Response object: the non-streaming POST body and the
// "response" field of response.* stream events.
type RespResponse struct {
	Status string `json:"status"`
	Output []struct {
		Type    string `json:"type"`
		Result  string `json:"result"`        // image_generation_call: b64 image
		Format  string `json:"output_format"` // image_generation_call: "png", …
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage             *RespUsage `json:"usage"`
	Error             *RespError `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

// OutputText concatenates every output_text content part across output items
// (SDK Response.OutputText parity).
func (r *RespResponse) OutputText() string {
	var b strings.Builder
	for _, item := range r.Output {
		for _, c := range item.Content {
			if c.Type == "output_text" {
				b.WriteString(c.Text)
			}
		}
	}
	return b.String()
}

// RespEvent is one stream event, flattened: every field any event type
// carries, discriminated by Type (the data JSON's own "type" field).
type RespEvent struct {
	Type      string          `json:"type"`
	Delta     string          `json:"delta"`     // *.delta events
	ItemID    string          `json:"item_id"`   // function_call_arguments.*
	Arguments string          `json:"arguments"` // function_call_arguments.done
	Item      json.RawMessage `json:"item"`      // output_item.* (verbatim, for replay)
	// PartialImageB64 carries image_generation_call.partial_image frames.
	PartialImageB64 string        `json:"partial_image_b64"`
	Response        *RespResponse `json:"response"` // response.completed/failed/incomplete
	// "error" terminal events carry these at the top level.
	Code    string `json:"code"`
	Message string `json:"message"`
	// In-band error envelope some gateways emit as event data (SDK parity).
	Error json.RawMessage `json:"error,omitempty"`
}

// RespOutputItem is the typed view of a completed output item (RespEvent.Item)
// — the subset needed to extract function calls; replay uses the raw JSON.
type RespOutputItem struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	// Arguments is normally a JSON-encoded string but kept raw: some gateways
	// send an object instead (see ArgumentsString).
	Arguments json.RawMessage `json:"arguments"`
	// Result carries an image_generation_call's base64 image payload.
	Result string `json:"result"`
	// Query carries a tool_search_call's search query.
	Query string `json:"query"`
	// OutputFormat is the image_generation_call's format ("png", "webp", …).
	OutputFormat string `json:"output_format"`
}

// ArgumentsString returns the arguments when they are a JSON string, else ""
// (callers fall back to delta-accumulated arguments — SDK OfString parity).
func (i *RespOutputItem) ArgumentsString() string {
	var s string
	if json.Unmarshal(i.Arguments, &s) == nil {
		return s
	}
	return ""
}

// RespFailure is a terminal failure event on a Responses stream:
// response.failed, response.incomplete, or error. Before this mapping these
// events fell through silently and the stream looked like a clean EOF.
type RespFailure struct {
	Event   string // the event type
	Code    string // error code, or the incomplete reason
	Message string
}

func (e *RespFailure) Error() string {
	detail := e.Message
	if detail == "" {
		detail = e.Code
	} else if e.Code != "" {
		detail += " (" + e.Code + ")"
	}
	if detail == "" {
		detail = "no detail provided"
	}
	return e.Event + ": " + detail
}

// Create is the non-streaming POST /responses.
func (c Responses) Create(ctx context.Context, req *RespRequest) (*RespResponse, error) {
	req.Stream = false
	var out RespResponse
	if err := c.Do(ctx, "POST", "/responses", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StreamResponse is the streaming POST /responses.
func (c Responses) StreamResponse(ctx context.Context, req *RespRequest) (*RespStream, error) {
	req.Stream = true
	sse, err := c.Stream(ctx, "POST", "/responses", req)
	if err != nil {
		return nil, err
	}
	return &RespStream{sse: sse}, nil
}

// Models is GET /models — Responses servers share the endpoint (and wire
// shape) with chat-completions.
func (c Responses) Models(ctx context.Context) ([]string, error) {
	return ChatComp{Client: c.Client}.Models(ctx)
}

// RespStream yields events until io.EOF (clean end, after the terminal
// response.completed). Terminal failure events surface as *RespFailure; a
// stream that ends without a single event returns ErrNoEvents.
type RespStream struct{ sse *SSE }

func (s *RespStream) Next() (*RespEvent, error) {
	evt, err := s.sse.Next()
	if err == io.EOF && !s.sse.SawEvent() {
		return nil, ErrNoEvents
	}
	if err != nil {
		return nil, err
	}
	var e RespEvent
	if uerr := json.Unmarshal(evt.Data, &e); uerr != nil {
		return nil, fmt.Errorf("llm: malformed stream event: %w", uerr)
	}
	if len(e.Error) > 0 {
		return nil, fmt.Errorf("received error while streaming: %s", e.Error)
	}
	if ferr := e.failure(); ferr != nil {
		return nil, ferr
	}
	return &e, nil
}

func (s *RespStream) Close() error { return s.sse.Close() }

// failure maps terminal failure events to *RespFailure, carrying the event's
// message/code (or the incomplete reason).
func (e *RespEvent) failure() error {
	switch e.Type {
	case "error":
		return &RespFailure{Event: e.Type, Code: e.Code, Message: e.Message}
	case "response.failed":
		f := &RespFailure{Event: e.Type}
		if e.Response != nil && e.Response.Error != nil {
			f.Code, f.Message = e.Response.Error.Code, e.Response.Error.Message
		}
		return f
	case "response.incomplete":
		f := &RespFailure{Event: e.Type}
		if e.Response != nil && e.Response.IncompleteDetails != nil {
			f.Code = e.Response.IncompleteDetails.Reason
		}
		return f
	}
	return nil
}

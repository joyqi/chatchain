package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// ChatComp is the OpenAI chat-completions dialect — also the wire shape of
// every OpenAI-compatible server (deepseek, kimi, local gateways). Exact
// compatibility with those servers is a hard requirement.
type ChatComp struct{ *Client }

// --- request wire shapes (JSON tags are the protocol) ---

type ChatMsg struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"` // string, or []any of content parts
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type ChatTextPart struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

type ChatImagePart struct {
	Type     string       `json:"type"` // "image_url"
	ImageURL ChatImageURL `json:"image_url"`
}

type ChatImageURL struct {
	URL string `json:"url"` // data URL: data:<mime>;base64,<b64>
}

type ChatFilePart struct {
	Type string       `json:"type"` // "file"
	File ChatFileData `json:"file"`
}

type ChatFileData struct {
	FileData string `json:"file_data"` // bare base64 (not a data URL)
	Filename string `json:"filename"`
}

type ChatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"
	Function ChatToolCallFunc `json:"function"`
}

type ChatToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded string
}

type ChatTool struct {
	Type     string           `json:"type"` // "function"
	Function ChatToolFunction `json:"function"`
}

type ChatToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type ChatCompRequest struct {
	Model           string             `json:"model"`
	Messages        []any              `json:"messages"` // ChatMsg or json.RawMessage (verbatim replay)
	Temperature     *float64           `json:"temperature,omitempty"`
	TopP            *float64           `json:"top_p,omitempty"`
	ReasoningEffort string             `json:"reasoning_effort,omitempty"`
	Tools           []ChatTool         `json:"tools,omitempty"`
	Stream          bool               `json:"stream,omitempty"`
	StreamOptions   *ChatStreamOptions `json:"stream_options,omitempty"`
}

type ChatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// --- response wire shapes ---

type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ChatCompResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

type ChatChunk struct {
	Choices []ChatChunkChoice `json:"choices"`
	Usage   *ChatUsage        `json:"usage"`
	Error   json.RawMessage   `json:"error,omitempty"` // in-band stream error
}

type ChatChunkChoice struct {
	Delta        ChatDelta `json:"delta"`
	FinishReason string    `json:"finish_reason"`
}

type ChatDelta struct {
	Content string `json:"content"`
	// Reasoning carries thinking deltas on OpenAI-compatible servers;
	// deepseek's official field is reasoning_content, aggregators use
	// reasoning. Callers should prefer Reasoning, else ReasoningContent.
	Reasoning        string          `json:"reasoning"`
	ReasoningContent string          `json:"reasoning_content"`
	ToolCalls        []ChatToolDelta `json:"tool_calls"`
}

type ChatToolDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Complete is the non-streaming POST /chat/completions.
func (c ChatComp) Complete(ctx context.Context, req *ChatCompRequest) (*ChatCompResponse, error) {
	req.Stream = false
	req.StreamOptions = nil
	var out ChatCompResponse
	if err := c.Do(ctx, "POST", "/chat/completions", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StreamCompletion is the streaming POST /chat/completions; the final chunk
// carries usage (stream_options.include_usage).
func (c ChatComp) StreamCompletion(ctx context.Context, req *ChatCompRequest) (*ChatCompStream, error) {
	req.Stream = true
	req.StreamOptions = &ChatStreamOptions{IncludeUsage: true}
	sse, err := c.Stream(ctx, "POST", "/chat/completions", req)
	if err != nil {
		return nil, err
	}
	return &ChatCompStream{sse: sse}, nil
}

// Models is GET /models, IDs sorted.
func (c ChatComp) Models(ctx context.Context) ([]string, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.Do(ctx, "GET", "/models", nil, &out); err != nil {
		return nil, err
	}
	var models []string
	for _, m := range out.Data {
		models = append(models, m.ID)
	}
	sort.Strings(models)
	return models, nil
}

// ChatCompStream yields chunks until io.EOF (clean end). A stream that ends
// without a single event returns ErrNoEvents (a compat server that answered
// a stream request with a plain body).
type ChatCompStream struct{ sse *SSE }

func (s *ChatCompStream) Next() (*ChatChunk, error) {
	evt, err := s.sse.Next()
	if err == io.EOF && !s.sse.SawEvent() {
		return nil, ErrNoEvents
	}
	if err != nil {
		return nil, err
	}
	var chunk ChatChunk
	if uerr := json.Unmarshal(evt.Data, &chunk); uerr != nil {
		return nil, fmt.Errorf("llm: malformed stream chunk: %w", uerr)
	}
	if len(chunk.Error) > 0 {
		return nil, fmt.Errorf("received error while streaming: %s", chunk.Error)
	}
	return &chunk, nil
}

func (s *ChatCompStream) Close() error { return s.sse.Close() }

package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

type Attachment struct {
	Filename string // basename
	MimeType string // e.g. "image/png"
	Data     []byte // raw file bytes
}

// ToolDef describes a tool available from an MCP server.
type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any // JSON Schema forwarded to AI provider
}

// ToolCall represents a model requesting a tool invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}

type Message struct {
	Role         string // "system", "user", "assistant", or "tool"
	Content      string
	Reasoning    string       // thinking/reasoning text (display/save only)
	Attachments  []Attachment // nil when no files
	ToolCalls    []ToolCall   // assistant messages requesting tool use
	ToolCallID   string       // tool result messages: which call this answers
	ToolCallName string       // tool result messages: function name
	IsError      bool         // tool result messages: whether the call failed
	Interrupted  bool         // set on assistant messages cut short by the user; replayed as ordinary history
	RawContent   any          // provider-specific raw content (e.g. *genai.Content for thought signatures)
}

type Provider interface {
	ListModels(ctx context.Context) ([]string, error)
	Chat(ctx context.Context, messages []Message) (string, error)
	// StreamChat streams content to w and reasoning to reasoning.
	// The provider MUST close reasoning when thinking is done (before first content write).
	// Returns (content, reasoning_text, error).
	StreamChat(ctx context.Context, messages []Message, w io.Writer, reasoning io.WriteCloser) (string, string, error)
	// Type returns the provider type string (e.g. "openai", "anthropic").
	// Used to tag persisted sessions and match RawContent on resume.
	Type() string
	// Model returns the current model; SetModel switches it at runtime
	// (used by the /model command and session resume).
	Model() string
	SetModel(model string)
}

// ToolProvider is an optional interface for providers that support tool calling.
type ToolProvider interface {
	StreamChatWithTools(ctx context.Context, messages []Message, tools []ToolDef,
		w io.Writer, reasoning io.WriteCloser) (content string, reasoningText string, toolCalls []ToolCall, err error)
}

// UsageReporter is an optional interface for providers that report token usage.
// LastUsage returns the input/output token counts from the most recent API call
// (ok=false when the provider didn't report usage — callers fall back to a local
// tokenizer). Used to drive context-window accounting / compaction.
type UsageReporter interface {
	LastUsage() (input int, output int, ok bool)
}

// Tunable is an optional interface for providers whose sampling/reasoning
// parameters can be adjusted after construction (the /model command and session
// resume). All providers implement it via baseProvider. Unset values (nil
// temperature, "" effort) mean the parameter is omitted from requests entirely,
// leaving the provider's own default in effect.
type Tunable interface {
	SetTemperature(t *float64) // nil = provider default (omit the parameter)
	Temperature() *float64
	SetEffort(level string) // "" = default (omit); low|medium|high|xhigh|max
	Effort() string
}

// ValidEffort reports whether level is a recognized reasoning-effort value
// ("" = provider default). The canonical list — the /model Effort tab and the
// config's effort key both validate against it.
func ValidEffort(level string) bool {
	switch level {
	case "", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

// ToolCallStreamObserver is an optional interface: providers report tool-call
// arguments while the model is still streaming them, so the chat layer can
// show progress (a large write_file call is otherwise dead air — tool calls
// render only once complete). name is the call's function name when known
// ("" otherwise); nil clears the observer. All providers implement it via
// baseProvider; backends that deliver calls atomically notify once.
type ToolCallStreamObserver interface {
	SetToolCallObserver(fn func(name, argsDelta string))
}

// RawContentProvider is an optional interface for providers that need to preserve
// raw model response content (e.g. Vertex AI thought signatures) across tool call rounds.
//
// Marshal/UnmarshalRawContent (de)serialize that provider-specific raw value for
// session persistence. The blob is opaque and only meaningful to the same provider
// type; the session layer tags each blob with the provider type and drops it on
// resume under a different provider.
type RawContentProvider interface {
	LastRawContent() any
	MarshalRawContent(v any) ([]byte, error)
	UnmarshalRawContent(data []byte) (any, error)
}

func New(providerType, apiKey, baseURL, model string, temperature *float64, httpClient *http.Client) (Provider, error) {
	switch providerType {
	case "openai":
		return NewOpenAI(apiKey, baseURL, model, temperature, httpClient), nil
	case "anthropic":
		return NewAnthropic(apiKey, baseURL, model, temperature, httpClient), nil
	case "gemini":
		return NewGemini(apiKey, baseURL, model, temperature, httpClient), nil
	case "openresponses":
		return NewOpenResponses(apiKey, baseURL, model, temperature, httpClient), nil
	case "vertexai":
		return NewVertexAI(apiKey, baseURL, model, temperature, httpClient), nil
	default:
		return nil, fmt.Errorf("unknown provider type: %s (supported: openai, anthropic, gemini, vertexai, openresponses)", providerType)
	}
}

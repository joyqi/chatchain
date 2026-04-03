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

type Message struct {
	Role        string // "user" or "assistant"
	Content     string
	Reasoning   string       // thinking/reasoning text (display/save only)
	Attachments []Attachment // nil when no files
}

type Provider interface {
	ListModels(ctx context.Context) ([]string, error)
	Chat(ctx context.Context, messages []Message) (string, error)
	// StreamChat streams content to w and reasoning to reasoning.
	// The provider MUST close reasoning when thinking is done (before first content write).
	// Returns (content, reasoning_text, error).
	StreamChat(ctx context.Context, messages []Message, w io.Writer, reasoning io.WriteCloser) (string, string, error)
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
	case "openclaw":
		return NewOpenClaw(apiKey, baseURL, model, httpClient != nil), nil
	default:
		return nil, fmt.Errorf("unknown provider type: %s (supported: openai, anthropic, gemini, vertexai, openresponses, openclaw)", providerType)
	}
}

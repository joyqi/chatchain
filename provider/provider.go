package provider

import (
	"context"
	"fmt"
	"io"
)

type Message struct {
	Role    string // "user" or "assistant"
	Content string
}

type Provider interface {
	ListModels(ctx context.Context) ([]string, error)
	Chat(ctx context.Context, messages []Message) (string, error)
	StreamChat(ctx context.Context, messages []Message, w io.Writer) (string, error)
}

func New(providerType, apiKey, baseURL, model string, temperature *float64) (Provider, error) {
	switch providerType {
	case "openai":
		return NewOpenAI(apiKey, baseURL, model, temperature), nil
	case "anthropic":
		return NewAnthropic(apiKey, baseURL, model, temperature), nil
	case "gemini":
		return NewGemini(apiKey, baseURL, model, temperature), nil
	case "openresponses":
		return NewOpenResponses(apiKey, baseURL, model, temperature), nil
	case "vertexai":
		return NewVertexAI(apiKey, baseURL, model, temperature), nil
	default:
		return nil, fmt.Errorf("unknown provider type: %s (supported: openai, anthropic, gemini, vertexai, openresponses)", providerType)
	}
}

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
	StreamChat(ctx context.Context, messages []Message, w io.Writer) (string, error)
}

func New(providerType, apiKey, baseURL, model string) (Provider, error) {
	switch providerType {
	case "openai":
		return NewOpenAI(apiKey, baseURL, model), nil
	case "anthropic":
		return NewAnthropic(apiKey, baseURL, model), nil
	default:
		return nil, fmt.Errorf("unknown provider type: %s (supported: openai, anthropic)", providerType)
	}
}

package provider

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type AnthropicProvider struct {
	client *anthropic.Client
	model  string
}

func NewAnthropic(apiKey, baseURL, model string) *AnthropicProvider {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}

	client := anthropic.NewClient(opts...)
	return &AnthropicProvider{
		client: &client,
		model:  model,
	}
}

func (p *AnthropicProvider) ListModels(ctx context.Context) ([]string, error) {
	page, err := p.client.Models.List(ctx, anthropic.ModelListParams{})
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

func (p *AnthropicProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer) (string, error) {
	var msgs []anthropic.MessageParam
	for _, msg := range messages {
		switch msg.Role {
		case "user":
			msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(msg.Content)))
		case "assistant":
			msgs = append(msgs, anthropic.NewAssistantMessage(anthropic.NewTextBlock(msg.Content)))
		}
	}

	stream := p.client.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: 4096,
		Messages:  msgs,
	})

	var full string
	for stream.Next() {
		evt := stream.Current()
		switch evt.Type {
		case "content_block_delta":
			if evt.Delta.Type == "text_delta" {
				fmt.Fprint(w, evt.Delta.Text)
				full += evt.Delta.Text
			}
		}
	}
	if err := stream.Err(); err != nil {
		return full, fmt.Errorf("stream error: %w", err)
	}

	return full, nil
}

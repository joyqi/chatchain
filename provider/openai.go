package provider

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

type OpenAIProvider struct {
	client *openai.Client
	model  string
}

func NewOpenAI(apiKey, baseURL, model string) *OpenAIProvider {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}

	client := openai.NewClient(opts...)
	return &OpenAIProvider{
		client: &client,
		model:  model,
	}
}

func (p *OpenAIProvider) ListModels(ctx context.Context) ([]string, error) {
	page, err := p.client.Models.List(ctx)
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

func (p *OpenAIProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer) (string, error) {
	params := openai.ChatCompletionNewParams{
		Model: p.model,
	}

	for _, msg := range messages {
		switch msg.Role {
		case "user":
			params.Messages = append(params.Messages, openai.UserMessage(msg.Content))
		case "assistant":
			params.Messages = append(params.Messages, openai.AssistantMessage(msg.Content))
		}
	}

	stream := p.client.Chat.Completions.NewStreaming(ctx, params)
	var full string

	for stream.Next() {
		evt := stream.Current()
		for _, choice := range evt.Choices {
			chunk := choice.Delta.Content
			if chunk != "" {
				fmt.Fprint(w, chunk)
				full += chunk
			}
		}
	}
	if err := stream.Err(); err != nil {
		return full, fmt.Errorf("stream error: %w", err)
	}

	return full, nil
}

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
	client      *anthropic.Client
	model       string
	temperature *float64
}

func NewAnthropic(apiKey, baseURL, model string, temperature *float64) *AnthropicProvider {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}

	client := anthropic.NewClient(opts...)
	return &AnthropicProvider{
		client:      &client,
		model:       model,
		temperature: temperature,
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

func (p *AnthropicProvider) buildParams(messages []Message) (anthropic.MessageNewParams, []anthropic.MessageParam) {
	var msgs []anthropic.MessageParam
	for _, msg := range messages {
		switch msg.Role {
		case "user":
			msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(msg.Content)))
		case "assistant":
			msgs = append(msgs, anthropic.NewAssistantMessage(anthropic.NewTextBlock(msg.Content)))
		}
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: 4096,
		Messages:  msgs,
	}
	if p.temperature != nil {
		params.Temperature = anthropic.Float(*p.temperature)
	}
	return params, msgs
}

func (p *AnthropicProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	params, _ := p.buildParams(messages)
	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return "", fmt.Errorf("chat error: %w", err)
	}
	var result string
	for _, block := range resp.Content {
		if block.Type == "text" {
			result += block.Text
		}
	}
	return result, nil
}

func (p *AnthropicProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer) (string, error) {
	params, _ := p.buildParams(messages)
	stream := p.client.Messages.NewStreaming(ctx, params)

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

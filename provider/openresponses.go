package provider

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

type OpenResponsesProvider struct {
	client      *openai.Client
	model       string
	temperature *float64
}

func NewOpenResponses(apiKey, baseURL, model string, temperature *float64) *OpenResponsesProvider {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}

	client := openai.NewClient(opts...)
	return &OpenResponsesProvider{
		client:      &client,
		model:       model,
		temperature: temperature,
	}
}

func (p *OpenResponsesProvider) ListModels(ctx context.Context) ([]string, error) {
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

func (p *OpenResponsesProvider) buildParams(messages []Message) responses.ResponseNewParams {
	params := responses.ResponseNewParams{
		Model: p.model,
	}
	if p.temperature != nil {
		params.Temperature = openai.Float(*p.temperature)
	}
	var input responses.ResponseInputParam
	for _, msg := range messages {
		switch msg.Role {
		case "user":
			input = append(input, responses.ResponseInputItemParamOfMessage(msg.Content, responses.EasyInputMessageRoleUser))
		case "assistant":
			input = append(input, responses.ResponseInputItemParamOfMessage(msg.Content, responses.EasyInputMessageRoleAssistant))
		}
	}
	params.Input.OfInputItemList = input
	return params
}

func (p *OpenResponsesProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	resp, err := p.client.Responses.New(ctx, p.buildParams(messages))
	if err != nil {
		return "", fmt.Errorf("chat error: %w", err)
	}
	return resp.OutputText(), nil
}

func (p *OpenResponsesProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer) (string, error) {
	stream := p.client.Responses.NewStreaming(ctx, p.buildParams(messages))
	var full string

	for stream.Next() {
		evt := stream.Current()
		if evt.Delta != "" {
			fmt.Fprint(w, evt.Delta)
			full += evt.Delta
		}
	}
	if err := stream.Err(); err != nil {
		return full, fmt.Errorf("stream error: %w", err)
	}

	return full, nil
}

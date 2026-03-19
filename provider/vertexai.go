package provider

import (
	"context"
	"fmt"
	"io"
	"sort"

	"google.golang.org/genai"
)

type VertexAIProvider struct {
	client      *genai.Client
	model       string
	temperature *float64
}

func NewVertexAI(apiKey, baseURL, model string, temperature *float64) *VertexAIProvider {
	cfg := &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendVertexAI,
	}
	if baseURL != "" {
		cfg.HTTPOptions = genai.HTTPOptions{BaseURL: baseURL, APIVersion: "v1"}
	}
	client, err := genai.NewClient(context.Background(), cfg)
	if err != nil {
		panic(fmt.Sprintf("failed to create VertexAI client: %v", err))
	}

	return &VertexAIProvider{
		client:      client,
		model:       model,
		temperature: temperature,
	}
}

func (p *VertexAIProvider) ListModels(ctx context.Context) ([]string, error) {
	page, err := p.client.Models.List(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}

	var models []string
	for {
		for _, m := range page.Items {
			models = append(models, m.Name)
		}
		if page.NextPageToken == "" {
			break
		}
		page, err = page.Next(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list models: %w", err)
		}
	}

	sort.Strings(models)
	return models, nil
}

func (p *VertexAIProvider) buildContents(messages []Message) ([]*genai.Content, *genai.Content) {
	var contents []*genai.Content
	var system *genai.Content
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			system = genai.NewContentFromText(msg.Content, "user")
		case "user":
			contents = append(contents, genai.NewContentFromText(msg.Content, "user"))
		case "assistant":
			contents = append(contents, genai.NewContentFromText(msg.Content, "model"))
		}
	}
	return contents, system
}

func (p *VertexAIProvider) config(system *genai.Content) *genai.GenerateContentConfig {
	cfg := &genai.GenerateContentConfig{}
	if p.temperature != nil {
		temp := float32(*p.temperature)
		cfg.Temperature = &temp
	}
	if system != nil {
		cfg.SystemInstruction = system
	}
	return cfg
}

func (p *VertexAIProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	contents, system := p.buildContents(messages)
	resp, err := p.client.Models.GenerateContent(ctx, p.model, contents, p.config(system))
	if err != nil {
		return "", fmt.Errorf("chat error: %w", err)
	}
	return resp.Text(), nil
}

func (p *VertexAIProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer) (string, error) {
	contents, system := p.buildContents(messages)
	var full string
	for resp, err := range p.client.Models.GenerateContentStream(ctx, p.model, contents, p.config(system)) {
		if err != nil {
			return full, fmt.Errorf("stream error: %w", err)
		}
		chunk := resp.Text()
		if chunk != "" {
			fmt.Fprint(w, chunk)
			full += chunk
		}
	}
	return full, nil
}

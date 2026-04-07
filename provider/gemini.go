package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"

	"google.golang.org/genai"
)

var _ ToolProvider = (*GeminiProvider)(nil)

type GeminiProvider struct {
	client      *genai.Client
	model       string
	temperature *float64
}

func NewGemini(apiKey, baseURL, model string, temperature *float64, httpClient *http.Client) *GeminiProvider {
	cfg := &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	}
	if baseURL != "" {
		cfg.HTTPOptions = genai.HTTPOptions{BaseURL: baseURL}
	}
	if httpClient != nil {
		cfg.HTTPClient = httpClient
	}
	client, err := genai.NewClient(context.Background(), cfg)
	if err != nil {
		panic(fmt.Sprintf("failed to create Gemini client: %v", err))
	}

	return &GeminiProvider{
		client:      client,
		model:       model,
		temperature: temperature,
	}
}

func (p *GeminiProvider) ListModels(ctx context.Context) ([]string, error) {
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

func (p *GeminiProvider) buildContents(messages []Message) ([]*genai.Content, *genai.Content) {
	var contents []*genai.Content
	var system *genai.Content
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			system = genai.NewContentFromText(msg.Content, "user")
		case "user":
			if len(msg.Attachments) > 0 {
				var parts []*genai.Part
				for _, att := range msg.Attachments {
					parts = append(parts, genai.NewPartFromBytes(att.Data, att.MimeType))
				}
				parts = append(parts, genai.NewPartFromText(msg.Content))
				contents = append(contents, genai.NewContentFromParts(parts, "user"))
			} else {
				contents = append(contents, genai.NewContentFromText(msg.Content, "user"))
			}
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				var parts []*genai.Part
				if msg.Content != "" {
					parts = append(parts, genai.NewPartFromText(msg.Content))
				}
				for _, tc := range msg.ToolCalls {
					parts = append(parts, &genai.Part{
						FunctionCall: &genai.FunctionCall{
							ID:   tc.ID,
							Name: tc.Name,
							Args: tc.Arguments,
						},
					})
				}
				contents = append(contents, genai.NewContentFromParts(parts, "model"))
			} else {
				contents = append(contents, genai.NewContentFromText(msg.Content, "model"))
			}
		case "tool":
			resp := map[string]any{"output": msg.Content}
			if msg.IsError {
				resp = map[string]any{"error": msg.Content}
			}
			contents = append(contents, genai.NewContentFromParts([]*genai.Part{
				{FunctionResponse: &genai.FunctionResponse{
					ID:       msg.ToolCallID,
					Name:     msg.ToolCallName,
					Response: resp,
				}},
			}, "user"))
		}
	}
	return contents, system
}

func (p *GeminiProvider) config(system *genai.Content) *genai.GenerateContentConfig {
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

func (p *GeminiProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	contents, system := p.buildContents(messages)
	resp, err := p.client.Models.GenerateContent(ctx, p.model, contents, p.config(system))
	if err != nil {
		return "", fmt.Errorf("chat error: %w", err)
	}
	return resp.Text(), nil
}

func (p *GeminiProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer, reasoningW io.WriteCloser) (string, string, error) {
	content, reasoning, _, err := p.streamChatInternal(ctx, messages, nil, w, reasoningW)
	return content, reasoning, err
}

func (p *GeminiProvider) StreamChatWithTools(ctx context.Context, messages []Message, tools []ToolDef, w io.Writer, reasoningW io.WriteCloser) (string, string, []ToolCall, error) {
	return p.streamChatInternal(ctx, messages, tools, w, reasoningW)
}

func (p *GeminiProvider) streamChatInternal(ctx context.Context, messages []Message, tools []ToolDef, w io.Writer, reasoningW io.WriteCloser) (string, string, []ToolCall, error) {
	contents, system := p.buildContents(messages)
	cfg := p.config(system)

	if len(tools) > 0 {
		var decls []*genai.FunctionDeclaration
		for _, t := range tools {
			decls = append(decls, &genai.FunctionDeclaration{
				Name:                 t.Name,
				Description:          t.Description,
				ParametersJsonSchema: t.InputSchema,
			})
		}
		cfg.Tools = []*genai.Tool{{FunctionDeclarations: decls}}
	}

	var full, thinkFull string
	var toolCalls []ToolCall
	reasoningClosed := false
	closeReasoning := func() {
		if !reasoningClosed {
			reasoningW.Close()
			reasoningClosed = true
		}
	}

	for resp, err := range p.client.Models.GenerateContentStream(ctx, p.model, contents, cfg) {
		if err != nil {
			closeReasoning()
			return full, thinkFull, nil, fmt.Errorf("stream error: %w", err)
		}
		if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
			for _, part := range resp.Candidates[0].Content.Parts {
				if part.Thought {
					fmt.Fprint(reasoningW, part.Text)
					thinkFull += part.Text
				} else if part.FunctionCall != nil {
					closeReasoning()
					args := part.FunctionCall.Args
					if args == nil {
						args = make(map[string]any)
					}
					id := part.FunctionCall.ID
					if id == "" {
						// Gemini may not return an ID; generate one
						id = fmt.Sprintf("call_%s_%d", part.FunctionCall.Name, len(toolCalls))
					}
					toolCalls = append(toolCalls, ToolCall{
						ID:        id,
						Name:      part.FunctionCall.Name,
						Arguments: args,
					})
				} else if part.Text != "" {
					closeReasoning()
					fmt.Fprint(w, part.Text)
					full += part.Text
				}
			}
		}
	}
	closeReasoning()

	if len(toolCalls) > 0 {
		return full, thinkFull, toolCalls, nil
	}
	return full, thinkFull, nil, nil
}


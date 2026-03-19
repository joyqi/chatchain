package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

type OpenAIProvider struct {
	client      *openai.Client
	model       string
	temperature *float64
}

func NewOpenAI(apiKey, baseURL, model string, temperature *float64, httpClient *http.Client) *OpenAIProvider {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	if httpClient != nil {
		opts = append(opts, option.WithHTTPClient(httpClient))
	}

	client := openai.NewClient(opts...)
	return &OpenAIProvider{
		client:      &client,
		model:       model,
		temperature: temperature,
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

func (p *OpenAIProvider) buildParams(messages []Message) openai.ChatCompletionNewParams {
	params := openai.ChatCompletionNewParams{
		Model: p.model,
	}
	if p.temperature != nil {
		params.Temperature = openai.Float(*p.temperature)
	}
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			params.Messages = append(params.Messages, openai.SystemMessage(msg.Content))
		case "user":
			if len(msg.Attachments) > 0 {
				var parts []openai.ChatCompletionContentPartUnionParam
				for _, att := range msg.Attachments {
					if strings.HasPrefix(att.MimeType, "image/") {
						dataURL := "data:" + att.MimeType + ";base64," + base64.StdEncoding.EncodeToString(att.Data)
						parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
							URL: dataURL,
						}))
					} else {
						b64 := base64.StdEncoding.EncodeToString(att.Data)
						parts = append(parts, openai.FileContentPart(openai.ChatCompletionContentPartFileFileParam{
							FileData: openai.String(b64),
							Filename: openai.String(att.Filename),
						}))
					}
				}
				parts = append(parts, openai.TextContentPart(msg.Content))
				params.Messages = append(params.Messages, openai.UserMessage(parts))
			} else {
				params.Messages = append(params.Messages, openai.UserMessage(msg.Content))
			}
		case "assistant":
			params.Messages = append(params.Messages, openai.AssistantMessage(msg.Content))
		}
	}
	return params
}

func (p *OpenAIProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	resp, err := p.client.Chat.Completions.New(ctx, p.buildParams(messages))
	if err != nil {
		return "", fmt.Errorf("chat error: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("no response choices")
	}
	return resp.Choices[0].Message.Content, nil
}

func (p *OpenAIProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer, reasoningW io.WriteCloser) (string, string, error) {
	stream := p.client.Chat.Completions.NewStreaming(ctx, p.buildParams(messages))
	var full, thinkFull string
	reasoningClosed := false
	closeReasoning := func() {
		if !reasoningClosed {
			reasoningW.Close()
			reasoningClosed = true
		}
	}

	for stream.Next() {
		evt := stream.Current()
		for _, choice := range evt.Choices {
			// Check for reasoning field in raw JSON (DeepSeek and compatible models)
			var extra struct {
				Reasoning *string `json:"reasoning"`
			}
			if err := json.Unmarshal([]byte(choice.Delta.RawJSON()), &extra); err == nil && extra.Reasoning != nil && *extra.Reasoning != "" {
				fmt.Fprint(reasoningW, *extra.Reasoning)
				thinkFull += *extra.Reasoning
			}

			chunk := choice.Delta.Content
			if chunk != "" {
				closeReasoning()
				fmt.Fprint(w, chunk)
				full += chunk
			}
		}
	}
	closeReasoning()
	if err := stream.Err(); err != nil {
		return full, thinkFull, fmt.Errorf("stream error: %w", err)
	}

	return full, thinkFull, nil
}

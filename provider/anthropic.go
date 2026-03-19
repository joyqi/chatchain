package provider

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type AnthropicProvider struct {
	client      *anthropic.Client
	model       string
	temperature *float64
}

func NewAnthropic(apiKey, baseURL, model string, temperature *float64, httpClient *http.Client) *AnthropicProvider {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	if httpClient != nil {
		opts = append(opts, option.WithHTTPClient(httpClient))
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
	var system []anthropic.TextBlockParam
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			system = append(system, anthropic.TextBlockParam{Text: msg.Content})
		case "user":
			if len(msg.Attachments) > 0 {
				var blocks []anthropic.ContentBlockParamUnion
				for _, att := range msg.Attachments {
					switch {
					case strings.HasPrefix(att.MimeType, "image/"):
						blocks = append(blocks, anthropic.NewImageBlockBase64(att.MimeType, base64.StdEncoding.EncodeToString(att.Data)))
					case att.MimeType == "application/pdf":
						blocks = append(blocks, anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{
							Data: base64.StdEncoding.EncodeToString(att.Data),
						}))
					default:
						// Text files: inline as text block
						blocks = append(blocks, anthropic.NewTextBlock("[File: "+att.Filename+"]\n"+string(att.Data)))
					}
				}
				blocks = append(blocks, anthropic.NewTextBlock(msg.Content))
				msgs = append(msgs, anthropic.NewUserMessage(blocks...))
			} else {
				msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(msg.Content)))
			}
		case "assistant":
			msgs = append(msgs, anthropic.NewAssistantMessage(anthropic.NewTextBlock(msg.Content)))
		}
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: 4096,
		Messages:  msgs,
		System:    system,
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

func (p *AnthropicProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer, reasoningW io.WriteCloser) (string, string, error) {
	params, _ := p.buildParams(messages)
	stream := p.client.Messages.NewStreaming(ctx, params)

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
		switch evt.Type {
		case "content_block_delta":
			if evt.Delta.Type == "thinking_delta" {
				fmt.Fprint(reasoningW, evt.Delta.Thinking)
				thinkFull += evt.Delta.Thinking
			} else if evt.Delta.Type == "text_delta" {
				closeReasoning()
				fmt.Fprint(w, evt.Delta.Text)
				full += evt.Delta.Text
			}
		}
	}
	closeReasoning()
	if err := stream.Err(); err != nil {
		return full, thinkFull, fmt.Errorf("stream error: %w", err)
	}

	return full, thinkFull, nil
}

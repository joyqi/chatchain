package provider

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

type OpenResponsesProvider struct {
	client      *openai.Client
	model       string
	temperature *float64
}

func NewOpenResponses(apiKey, baseURL, model string, temperature *float64, httpClient *http.Client) *OpenResponsesProvider {
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
		case "system":
			params.Instructions = openai.String(msg.Content)
		case "user":
			if len(msg.Attachments) > 0 {
				var content responses.ResponseInputMessageContentListParam
				for _, att := range msg.Attachments {
					if strings.HasPrefix(att.MimeType, "image/") {
						dataURL := "data:" + att.MimeType + ";base64," + base64.StdEncoding.EncodeToString(att.Data)
						content = append(content, responses.ResponseInputContentUnionParam{
							OfInputImage: &responses.ResponseInputImageParam{
								ImageURL: param.NewOpt(dataURL),
								Detail:   responses.ResponseInputImageDetailAuto,
							},
						})
					} else {
						b64 := base64.StdEncoding.EncodeToString(att.Data)
						content = append(content, responses.ResponseInputContentUnionParam{
							OfInputFile: &responses.ResponseInputFileParam{
								FileData: param.NewOpt(b64),
								Filename: param.NewOpt(att.Filename),
							},
						})
					}
				}
				content = append(content, responses.ResponseInputContentParamOfInputText(msg.Content))
				input = append(input, responses.ResponseInputItemParamOfMessage(content, responses.EasyInputMessageRoleUser))
			} else {
				input = append(input, responses.ResponseInputItemParamOfMessage(msg.Content, responses.EasyInputMessageRoleUser))
			}
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

func (p *OpenResponsesProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer, reasoningW io.WriteCloser) (string, string, error) {
	stream := p.client.Responses.NewStreaming(ctx, p.buildParams(messages))
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
		case "response.reasoning_summary_text.delta":
			fmt.Fprint(reasoningW, evt.Delta)
			thinkFull += evt.Delta
		case "response.output_text.delta":
			closeReasoning()
			fmt.Fprint(w, evt.Delta)
			full += evt.Delta
		case "response.completed":
			// Stream complete — stop reading
		default:
			if evt.Delta != "" && evt.Type == "" {
				// Fallback for untyped deltas
				closeReasoning()
				fmt.Fprint(w, evt.Delta)
				full += evt.Delta
			}
		}
	}
	closeReasoning()
	if err := stream.Err(); err != nil {
		// Some providers close the stream without [DONE] after response.completed,
		// causing JSON parse errors. Ignore if we already got content or reasoning.
		if full != "" || thinkFull != "" {
			return full, thinkFull, nil
		}
		return full, thinkFull, fmt.Errorf("stream error: %w", err)
	}

	return full, thinkFull, nil
}

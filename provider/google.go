package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"google.golang.org/genai"
)

var _ ToolProvider = (*GoogleProvider)(nil)
var _ RawContentProvider = (*GoogleProvider)(nil)
var _ UsageReporter = (*GoogleProvider)(nil)
var _ Tunable = (*GoogleProvider)(nil)

// GoogleProvider serves both Google backends through the unified genai SDK:
// the Gemini Developer API ("gemini") and Vertex AI ("vertexai"). The request
// schema is shared; the SDK handles the endpoint/auth split via Backend, and
// the remaining field-level differences are parameterized here — Vertex AI
// does not accept FunctionCall/FunctionResponse IDs and strictly validates
// that every part has its data oneof set.
type GoogleProvider struct {
	baseProvider
	client           *genai.Client
	lastModelContent *genai.Content // preserves thought signatures for tool call rounds
	toolCallIDs      bool           // whether the backend accepts FunctionCall/FunctionResponse IDs
}

func NewGemini(apiKey, baseURL, model string, temperature *float64, httpClient *http.Client) *GoogleProvider {
	return newGoogle("gemini", genai.BackendGeminiAPI, "", true, apiKey, baseURL, model, temperature, httpClient)
}

func NewVertexAI(apiKey, baseURL, model string, temperature *float64, httpClient *http.Client) *GoogleProvider {
	return newGoogle("vertexai", genai.BackendVertexAI, "v1", false, apiKey, baseURL, model, temperature, httpClient)
}

func newGoogle(providerType string, backend genai.Backend, apiVersion string, toolCallIDs bool,
	apiKey, baseURL, model string, temperature *float64, httpClient *http.Client) *GoogleProvider {
	cfg := &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: backend,
	}
	if baseURL != "" {
		cfg.HTTPOptions = genai.HTTPOptions{BaseURL: baseURL, APIVersion: apiVersion}
	}
	if httpClient != nil {
		cfg.HTTPClient = httpClient
	}
	client, err := genai.NewClient(context.Background(), cfg)
	if err != nil {
		panic(fmt.Sprintf("failed to create %s client: %v", providerType, err))
	}

	return &GoogleProvider{
		baseProvider: baseProvider{providerType: providerType, model: model, temperature: temperature},
		client:       client,
		toolCallIDs:  toolCallIDs,
	}
}

func (p *GoogleProvider) LastRawContent() any {
	return p.lastModelContent
}

func (p *GoogleProvider) MarshalRawContent(v any) ([]byte, error) {
	c, ok := v.(*genai.Content)
	if !ok || c == nil {
		return nil, fmt.Errorf("%s: unexpected raw content type %T", p.providerType, v)
	}
	return json.Marshal(c)
}

func (p *GoogleProvider) UnmarshalRawContent(data []byte) (any, error) {
	var c genai.Content
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (p *GoogleProvider) ListModels(ctx context.Context) ([]string, error) {
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

// partHasData reports whether the part's data oneof is set. A zero-value part
// (e.g. the empty text part that can trail a streamed response) marshals to {}
// and Vertex AI rejects the whole request with "required oneof field 'data'
// must have one initialized field".
func partHasData(p *genai.Part) bool {
	return p.Text != "" ||
		p.InlineData != nil ||
		p.FileData != nil ||
		p.FunctionCall != nil ||
		p.FunctionResponse != nil ||
		p.ExecutableCode != nil ||
		p.CodeExecutionResult != nil ||
		p.ToolCall != nil ||
		p.ToolResponse != nil
}

// sanitizeContent drops data-less parts before content is replayed to the
// API, returning nil when nothing remains. Sessions persisted before this
// filter existed can still carry such parts, so history replay must clean
// them too, not just fresh stream output.
func sanitizeContent(c *genai.Content) *genai.Content {
	kept := make([]*genai.Part, 0, len(c.Parts))
	for _, part := range c.Parts {
		if partHasData(part) {
			kept = append(kept, part)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	if len(kept) == len(c.Parts) {
		return c
	}
	return &genai.Content{Role: c.Role, Parts: kept}
}

// appendUserContent appends a user-role content, folding it into a trailing
// user-role content when present: Gemini multiturn requests alternate
// user/model roles, and an interrupted turn can leave tool results (user-role
// function responses) directly followed by the next user message — see
// docs/design/interrupt.md (persistence state 3).
func appendUserContent(contents []*genai.Content, c *genai.Content) []*genai.Content {
	if n := len(contents); n > 0 && contents[n-1].Role == "user" {
		contents[n-1].Parts = append(contents[n-1].Parts, c.Parts...)
		return contents
	}
	return append(contents, c)
}

func (p *GoogleProvider) buildContents(messages []Message) ([]*genai.Content, *genai.Content) {
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
				contents = appendUserContent(contents, genai.NewContentFromParts(parts, "user"))
			} else {
				contents = appendUserContent(contents, genai.NewContentFromText(msg.Content, "user"))
			}
		case "assistant":
			// Use raw content if available (preserves thought signatures)
			if raw, ok := msg.RawContent.(*genai.Content); ok && raw != nil {
				if c := sanitizeContent(raw); c != nil {
					contents = append(contents, c)
				}
			} else if len(msg.ToolCalls) > 0 {
				var parts []*genai.Part
				if msg.Content != "" {
					parts = append(parts, genai.NewPartFromText(msg.Content))
				}
				for _, tc := range msg.ToolCalls {
					fc := &genai.FunctionCall{
						Name: tc.Name,
						Args: tc.Arguments,
					}
					if p.toolCallIDs {
						fc.ID = tc.ID
					}
					parts = append(parts, &genai.Part{FunctionCall: fc})
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
			fr := &genai.FunctionResponse{
				Name:     msg.ToolCallName,
				Response: resp,
			}
			if p.toolCallIDs {
				fr.ID = msg.ToolCallID
			}
			contents = appendUserContent(contents, genai.NewContentFromParts([]*genai.Part{
				{FunctionResponse: fr},
			}, "user"))
		}
	}
	return contents, system
}

func (p *GoogleProvider) config(system *genai.Content) *genai.GenerateContentConfig {
	cfg := &genai.GenerateContentConfig{}
	if p.temperature != nil {
		temp := float32(*p.temperature)
		cfg.Temperature = &temp
	}
	if p.effort != "" {
		cfg.ThinkingConfig = &genai.ThinkingConfig{
			IncludeThoughts: true,
			ThinkingLevel:   genai.ThinkingLevel(strings.ToUpper(p.effort)),
		}
	}
	if system != nil {
		cfg.SystemInstruction = system
	}
	return cfg
}

func (p *GoogleProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	contents, system := p.buildContents(messages)
	resp, err := p.client.Models.GenerateContent(ctx, p.model, contents, p.config(system))
	if err != nil {
		return "", fmt.Errorf("chat error: %w", err)
	}
	return resp.Text(), nil
}

func (p *GoogleProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer, reasoningW io.WriteCloser) (string, string, error) {
	content, reasoning, _, err := p.streamChatInternal(ctx, messages, nil, w, reasoningW)
	return content, reasoning, err
}

func (p *GoogleProvider) StreamChatWithTools(ctx context.Context, messages []Message, tools []ToolDef, w io.Writer, reasoningW io.WriteCloser) (string, string, []ToolCall, error) {
	return p.streamChatInternal(ctx, messages, tools, w, reasoningW)
}

func (p *GoogleProvider) streamChatInternal(ctx context.Context, messages []Message, tools []ToolDef, w io.Writer, reasoningW io.WriteCloser) (string, string, []ToolCall, error) {
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
	var rawParts []*genai.Part // accumulate all parts to preserve thought signatures
	reasoningClosed := false
	closeReasoning := func() {
		if !reasoningClosed {
			reasoningW.Close()
			reasoningClosed = true
		}
	}
	p.lastUsageOK = false

	for resp, err := range p.client.Models.GenerateContentStream(ctx, p.model, contents, cfg) {
		if err != nil {
			closeReasoning()
			return full, thinkFull, nil, fmt.Errorf("stream error: %w", err)
		}
		if resp.UsageMetadata != nil {
			p.lastInput = int(resp.UsageMetadata.PromptTokenCount)
			p.lastOutput = int(resp.UsageMetadata.CandidatesTokenCount)
			p.lastUsageOK = true
		}
		if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
			for _, part := range resp.Candidates[0].Content.Parts {
				if partHasData(part) {
					rawParts = append(rawParts, part)
				}
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
		p.lastModelContent = genai.NewContentFromParts(rawParts, "model")
		return full, thinkFull, toolCalls, nil
	}
	p.lastModelContent = nil
	return full, thinkFull, nil, nil
}

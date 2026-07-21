package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"chatchain/internal/llm"
)

var _ ToolProvider = (*GoogleProvider)(nil)
var _ RawContentProvider = (*GoogleProvider)(nil)
var _ UsageReporter = (*GoogleProvider)(nil)
var _ Tunable = (*GoogleProvider)(nil)

const (
	geminiDefaultBaseURL = "https://generativelanguage.googleapis.com"
	vertexDefaultBaseURL = "https://aiplatform.googleapis.com"
)

// GoogleProvider serves both Google backends through the shared
// generateContent dialect (internal/llm): the Gemini Developer API ("gemini")
// and Vertex AI ("vertexai", express/API-key mode — chatchain always has a
// key, so the SDK's ADC path was never reachable). The request schema is
// shared; the remaining field-level differences are parameterized here —
// Vertex AI does not accept FunctionCall/FunctionResponse IDs and strictly
// validates that every part has its data oneof set.
type GoogleProvider struct {
	baseProvider
	imageOutput      bool
	lastImages       []Attachment
	client           llm.Google
	lastModelContent *llm.GContent // preserves thought signatures for tool call rounds
	toolCallIDs      bool          // whether the backend accepts FunctionCall/FunctionResponse IDs
}

func NewGemini(apiKey, baseURL, model string, temperature *float64, httpClient *http.Client) *GoogleProvider {
	if baseURL == "" {
		baseURL = geminiDefaultBaseURL
	}
	return newGoogle("gemini", false, "v1beta", true, apiKey, baseURL, model, temperature, httpClient)
}

func NewVertexAI(apiKey, baseURL, model string, temperature *float64, httpClient *http.Client) *GoogleProvider {
	// Parity with the genai constructor: the default endpoint speaks v1beta1;
	// a custom base URL historically switched to v1.
	version := "v1beta1"
	if baseURL == "" {
		baseURL = vertexDefaultBaseURL
	} else {
		version = "v1"
	}
	return newGoogle("vertexai", true, version, false, apiKey, baseURL, model, temperature, httpClient)
}

func newGoogle(providerType string, vertex bool, version string, toolCallIDs bool,
	apiKey, baseURL, model string, temperature *float64, httpClient *http.Client) *GoogleProvider {
	c := llm.New(baseURL, httpClient)
	c.Header.Set("x-goog-api-key", apiKey)
	return &GoogleProvider{
		baseProvider: baseProvider{providerType: providerType, model: model, temperature: temperature},
		client:       llm.Google{Client: c, Vertex: vertex, Version: version},
		toolCallIDs:  toolCallIDs,
	}
}

func (p *GoogleProvider) LastRawContent() any {
	return p.lastModelContent
}

func (p *GoogleProvider) MarshalRawContent(v any) ([]byte, error) {
	c, ok := v.(*llm.GContent)
	if !ok || c == nil {
		return nil, fmt.Errorf("%s: unexpected raw content type %T", p.providerType, v)
	}
	return json.Marshal(c)
}

func (p *GoogleProvider) UnmarshalRawContent(data []byte) (any, error) {
	var c llm.GContent
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (p *GoogleProvider) ListModels(ctx context.Context) ([]string, error) {
	models, err := p.client.Models(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	return models, nil
}

// sanitizeContent drops data-less parts before content is replayed to the
// API, returning nil when nothing remains. Sessions persisted before this
// filter existed can still carry such parts, so history replay must clean
// them too, not just fresh stream output.
func sanitizeContent(c *llm.GContent) *llm.GContent {
	kept := make([]*llm.GPart, 0, len(c.Parts))
	for _, part := range c.Parts {
		if part.HasData() {
			kept = append(kept, part)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	if len(kept) == len(c.Parts) {
		return c
	}
	return &llm.GContent{Role: c.Role, Parts: kept}
}

// appendUserContent appends a user-role content, folding it into a trailing
// user-role content when present: Gemini multiturn requests alternate
// user/model roles, and an interrupted turn can leave tool results (user-role
// function responses) directly followed by the next user message — see
// docs/design/interrupt.md (persistence state 3).
func appendUserContent(contents []*llm.GContent, c *llm.GContent) []*llm.GContent {
	if n := len(contents); n > 0 && contents[n-1].Role == "user" {
		contents[n-1].Parts = append(contents[n-1].Parts, c.Parts...)
		return contents
	}
	return append(contents, c)
}

func textContent(role, text string) *llm.GContent {
	return &llm.GContent{Role: role, Parts: []*llm.GPart{{Text: text}}}
}

func (p *GoogleProvider) buildContents(messages []Message) ([]*llm.GContent, *llm.GContent) {
	var contents []*llm.GContent
	var system *llm.GContent
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			system = textContent("user", msg.Content)
		case "user":
			if len(msg.Attachments) > 0 {
				var parts []*llm.GPart
				for _, att := range msg.Attachments {
					parts = append(parts, &llm.GPart{InlineData: &llm.GBlob{Data: att.Data, MimeType: att.MimeType}})
				}
				parts = append(parts, &llm.GPart{Text: msg.Content})
				contents = appendUserContent(contents, &llm.GContent{Role: "user", Parts: parts})
			} else {
				contents = appendUserContent(contents, textContent("user", msg.Content))
			}
		case "assistant":
			// Use raw content if available (preserves thought signatures)
			if raw, ok := msg.RawContent.(*llm.GContent); ok && raw != nil {
				if c := sanitizeContent(raw); c != nil {
					contents = append(contents, c)
				}
			} else if len(msg.ToolCalls) > 0 {
				var parts []*llm.GPart
				if msg.Content != "" {
					parts = append(parts, &llm.GPart{Text: msg.Content})
				}
				for _, tc := range msg.ToolCalls {
					fc := &llm.GFunctionCall{Name: tc.Name, Args: tc.Arguments}
					if p.toolCallIDs {
						fc.ID = tc.ID
					}
					parts = append(parts, &llm.GPart{FunctionCall: fc})
				}
				contents = append(contents, &llm.GContent{Role: "model", Parts: parts})
			} else if len(msg.Attachments) > 0 {
				// A generated image round-trips as a model inlineData part so
				// follow-ups ("make the sky blue") edit it in place.
				var parts []*llm.GPart
				if msg.Content != "" {
					parts = append(parts, &llm.GPart{Text: msg.Content})
				}
				for _, att := range msg.Attachments {
					parts = append(parts, &llm.GPart{InlineData: &llm.GBlob{Data: att.Data, MimeType: att.MimeType}})
				}
				contents = append(contents, &llm.GContent{Role: "model", Parts: parts})
			} else {
				contents = append(contents, textContent("model", msg.Content))
			}
		case "tool":
			resp := map[string]any{"output": msg.Content}
			if msg.IsError {
				resp = map[string]any{"error": msg.Content}
			}
			fr := &llm.GFunctionResp{Name: msg.ToolCallName, Response: resp}
			if p.toolCallIDs {
				fr.ID = msg.ToolCallID
			}
			contents = appendUserContent(contents, &llm.GContent{Role: "user", Parts: []*llm.GPart{{FunctionResponse: fr}}})
		}
	}
	return contents, system
}

func (p *GoogleProvider) buildRequest(messages []Message) *llm.GenerateRequest {
	contents, system := p.buildContents(messages)
	req := &llm.GenerateRequest{Contents: contents, SystemInstruction: system}
	cfg := &llm.GGenerationConfig{}
	if p.temperature != nil {
		temp := float32(*p.temperature)
		cfg.Temperature = &temp
	}
	if p.effort != "" {
		cfg.ThinkingConfig = &llm.GThinkingConfig{
			IncludeThoughts: true,
			ThinkingLevel:   strings.ToUpper(p.effort),
		}
	}
	if p.imageOutput {
		// The explicit opt-in (config image: true / the /model Image tab):
		// official-API models that need responseModalities to emit images.
		// Off by default — relays like zenmux generate without it, and text
		// models reject the IMAGE modality.
		cfg.ResponseModalities = []string{"TEXT", "IMAGE"}
	}
	if cfg.Temperature != nil || cfg.ThinkingConfig != nil || cfg.ResponseModalities != nil {
		req.GenerationConfig = cfg
	}
	return req
}

func (p *GoogleProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	p.lastImages = nil
	resp, err := p.client.Generate(ctx, p.model, p.buildRequest(messages))
	if err != nil {
		return "", fmt.Errorf("chat error: %w", err)
	}
	// The unary path surfaces generated images too (-m single-shot runs).
	if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
		for _, part := range resp.Candidates[0].Content.Parts {
			if part.InlineData != nil {
				p.lastImages = append(p.lastImages, Attachment{
					MimeType: part.InlineData.MimeType,
					Data:     part.InlineData.Data,
				})
			}
		}
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
	req := p.buildRequest(messages)
	if len(tools) > 0 {
		var decls []*llm.GFunctionDeclaration
		for _, t := range tools {
			decls = append(decls, &llm.GFunctionDeclaration{
				Name:                 t.Name,
				Description:          t.Description,
				ParametersJsonSchema: t.InputSchema,
			})
		}
		req.Tools = []*llm.GTool{{FunctionDeclarations: decls}}
	}

	var full, thinkFull string
	var toolCalls []ToolCall
	p.lastImages = nil
	var rawParts []*llm.GPart // accumulate all parts to preserve thought signatures
	reasoningClosed := false
	closeReasoning := func() {
		if !reasoningClosed {
			reasoningW.Close()
			reasoningClosed = true
		}
	}
	defer closeReasoning()
	p.lastUsageOK = false

	stream, err := p.client.StreamGenerate(ctx, p.model, req)
	if err != nil {
		return "", "", nil, fmt.Errorf("stream error: %w", err)
	}
	defer stream.Close()

	for {
		resp, serr := stream.Next()
		if serr == io.EOF {
			break
		}
		if serr != nil {
			return full, thinkFull, nil, fmt.Errorf("stream error: %w", serr)
		}
		if resp.UsageMetadata != nil {
			p.lastInput = resp.UsageMetadata.PromptTokenCount
			p.lastOutput = resp.UsageMetadata.CandidatesTokenCount
			p.lastUsageOK = true
		}
		if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
			continue
		}
		for _, part := range resp.Candidates[0].Content.Parts {
			if part.HasData() && part.InlineData == nil {
				// Generated images ride Message.Attachments (and the session
				// bundle) — duplicating their base64 into the raw replay blob
				// would double-store megabytes per image.
				rawParts = append(rawParts, part)
			}
			switch {
			case part.InlineData != nil:
				closeReasoning()
				p.lastImages = append(p.lastImages, Attachment{
					MimeType: part.InlineData.MimeType,
					Data:     part.InlineData.Data,
				})
			case part.Thought:
				fmt.Fprint(reasoningW, part.Text)
				thinkFull += part.Text
			case part.FunctionCall != nil:
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
				toolCalls = append(toolCalls, ToolCall{ID: id, Name: part.FunctionCall.Name, Arguments: args})
				// generateContent delivers calls atomically: one notification.
				p.notifyToolDelta(part.FunctionCall.Name, "")
			case part.Text != "":
				closeReasoning()
				fmt.Fprint(w, part.Text)
				full += part.Text
			}
		}
	}
	closeReasoning()

	if len(toolCalls) > 0 {
		p.lastModelContent = &llm.GContent{Role: "model", Parts: rawParts}
		return full, thinkFull, toolCalls, nil
	}
	p.lastModelContent = nil
	return full, thinkFull, nil, nil
}

// LastImages implements ImageOutputProvider: the images generated during the
// most recent stream.
func (p *GoogleProvider) LastImages() []Attachment { return p.lastImages }

// SetImageOutput / ImageOutput implement ImageTunable: the switch adds the
// responseModalities opt-in to requests.
func (p *GoogleProvider) SetImageOutput(on bool) { p.imageOutput = on }
func (p *GoogleProvider) ImageOutput() bool      { return p.imageOutput }

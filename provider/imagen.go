package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/joyqi/iota/internal/llm"
)

// ImagenProvider speaks the dedicated Imagen :predict dialect: every turn is
// a text→image (or references+text→image) generation. There is no text
// output, no tool calling, no token accounting — the type deliberately does
// NOT implement Tunable or UsageReporter, so the chat layer's capability
// gates hide the effort/temperature/context machinery for these sessions
// (docs: brain page image-providers).
//
// Requests are stateless-per-message, matching the raw API: the prompt is
// the final user message and the reference images are THAT MESSAGE's image
// attachments — nothing is mined from earlier history. Editing is explicit:
// the chat layer's /edit command materializes the previous result into the
// outgoing message as an attachment (persisted, so resume replays exactly).
type ImagenProvider struct {
	model      string
	client     llm.Google
	gen        ImageGenParams
	lastImages []Attachment
}

// NewImagen builds the imagen provider. An empty baseURL targets the official
// Gemini Developer API (models/imagen-…:predict on v1beta); a custom baseURL
// targets a vertexai-compatible endpoint (publishers/{vendor}/models/… on v1
// — the relay-station convention, e.g. zenmux's /api/vertex-ai).
func NewImagen(apiKey, baseURL, model string, httpClient *http.Client) *ImagenProvider {
	vertex := baseURL != ""
	version := "v1"
	if baseURL == "" {
		baseURL = geminiDefaultBaseURL
		version = "v1beta"
	}
	c := llm.New(baseURL, httpClient)
	c.Header.Set("x-goog-api-key", apiKey)
	// No transport-level retries: a :predict is a billed, non-idempotent
	// generation, and its response arrives only when the work is done — a
	// relay 5xx after upstream completion would silently re-bill. Failures
	// surface immediately; the user decides whether to spend again.
	c.Retries = 0
	return &ImagenProvider{
		model:  model,
		client: llm.Google{Client: c, Vertex: vertex, Version: version},
	}
}

func (p *ImagenProvider) Type() string      { return "imagen" }
func (p *ImagenProvider) Model() string     { return p.model }
func (p *ImagenProvider) SetModel(m string) { p.model = m }

func (p *ImagenProvider) SetImageGenParams(g ImageGenParams) { p.gen = g }
func (p *ImagenProvider) ImageGenParams() ImageGenParams     { return p.gen }

// ImageGenOptions lists what the Imagen :predict dialect understands across
// backends (seedream's advertised ratios ∪ official Imagen's); a size tier a
// given backend lacks is ignored or rejected server-side.
func (p *ImagenProvider) ImageGenOptions() ImageGenOptions {
	return ImageGenOptions{
		AspectRatios:   []string{"1:1", "2:3", "3:2", "3:4", "4:3", "9:16", "16:9", "21:9"},
		ImageSizes:     []string{"1K", "2K", "4K"},
		NegativePrompt: true,
	}
}

func (p *ImagenProvider) LastImages() []Attachment { return p.lastImages }

// ListModels reuses the Google listing (relay fallback included) and keeps
// only models whose METADATA says they generate images: outputModalities
// containing "image" (relay form) or supportedGenerationMethods containing
// "predict" (official form). Entries with no metadata at all are kept — the
// user judges, and a wrong pick surfaces as a clear API error (the standing
// no-name-heuristics rule).
func (p *ImagenProvider) ListModels(ctx context.Context) ([]string, error) {
	models, err := p.client.Models(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	var names []string
	for _, m := range models {
		if imageCapable(m) {
			names = append(names, m.Name)
		}
	}
	return names, nil
}

func imageCapable(m llm.GModelInfo) bool {
	if len(m.Methods) == 0 && len(m.Output) == 0 {
		return true // no metadata: cannot judge, keep
	}
	for _, meth := range m.Methods {
		if meth == "predict" {
			return true
		}
	}
	for _, out := range m.Output {
		if strings.EqualFold(out, "image") {
			return true
		}
	}
	return false
}

// lastUserTurn is the whole input of a stateless image-provider turn: the
// trailing user message's content and image attachments. Earlier history is
// record, not input — editing re-attaches explicitly (the /edit command).
// Shared by every dedicated image dialect (imagen, images).
func lastUserTurn(messages []Message) (prompt string, refs []Attachment) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content, imageAttachments(messages[i].Attachments)
		}
	}
	return "", nil
}

// buildRequest derives the predict call from the trailing user message alone
// (lastUserTurn), reference images numbered 1..n.
func (p *ImagenProvider) buildRequest(messages []Message) (*llm.PredictRequest, error) {
	prompt, refs := lastUserTurn(messages)
	if strings.TrimSpace(prompt) == "" {
		return nil, &PermanentError{Err: fmt.Errorf("imagen: empty prompt")}
	}
	inst := &llm.GImageInstance{Prompt: prompt}
	for i, a := range refs {
		inst.ReferenceImages = append(inst.ReferenceImages, &llm.GReferenceImage{
			ReferenceType:  "REFERENCE_TYPE_RAW",
			ReferenceID:    i + 1,
			ReferenceImage: &llm.GImageBlob{BytesBase64Encoded: a.Data, MimeType: a.MimeType},
		})
	}
	// sampleCount is pinned to 1: official Imagen defaults to FOUR images per
	// call otherwise — 4x the cost, and every follow-up turn would then drag
	// four canvases along as reference images.
	return &llm.PredictRequest{
		Instances: []*llm.GImageInstance{inst},
		Parameters: &llm.GImageParams{
			SampleCount:    1,
			AspectRatio:    p.gen.AspectRatio,
			ImageSize:      p.gen.ImageSize,
			NegativePrompt: p.gen.NegativePrompt,
		},
	}, nil
}

func imageAttachments(atts []Attachment) []Attachment {
	var out []Attachment
	for _, a := range atts {
		if strings.HasPrefix(a.MimeType, "image/") {
			out = append(out, a)
		}
	}
	return out
}

// Chat runs one generation. The returned text is always empty — the images
// land in LastImages, which the chat layer saves, renders, and attaches to
// the assistant message (the established image-only response path).
func (p *ImagenProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	p.lastImages = nil
	req, err := p.buildRequest(messages)
	if err != nil {
		return "", err
	}
	resp, err := p.client.Predict(ctx, p.model, req)
	if err != nil {
		return "", err
	}
	filtered := ""
	for i, pred := range resp.Predictions {
		data, mime := pred.BytesBase64Encoded, pred.MimeType
		if len(data) == 0 {
			if pred.RAIFilteredReason != "" {
				filtered = pred.RAIFilteredReason
				continue
			}
			if pred.GcsURI == "" {
				continue
			}
			// Referenced payload: relays answer with a signed https link that
			// EXPIRES, so it is fetched now rather than remembered. (Official
			// Vertex would answer gs://, which needs Google credentials we
			// never hold — fetchImage rejects the scheme with a clear error.)
			var ferr error
			if data, mime, ferr = fetchImage(ctx, p.client.HTTP, pred.GcsURI); ferr != nil {
				return "", fmt.Errorf("imagen: fetching result image: %w", ferr)
			}
		}
		mime = imageMime(mime, data)
		p.lastImages = append(p.lastImages, Attachment{
			Filename: fmt.Sprintf("image-%d%s", i+1, extForMime(mime)),
			MimeType: mime,
			Data:     data,
		})
	}
	if len(p.lastImages) == 0 {
		// Deterministic outcomes: retrying would bill again and fail again.
		if filtered != "" {
			return "", &PermanentError{Err: fmt.Errorf("imagen: all candidates were safety-filtered: %s", filtered)}
		}
		return "", &PermanentError{Err: fmt.Errorf(
			"imagen: response contained no images (%d prediction(s), none carried inline bytes or a URI)",
			len(resp.Predictions))}
	}
	return "", nil
}

// StreamChat adapts the unary predict to the streaming interface: there is
// nothing to stream (the endpoint has no streaming form), so reasoning closes
// immediately and the call resolves with empty text once the images arrive.
func (p *ImagenProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer, reasoning io.WriteCloser) (string, string, error) {
	reasoning.Close()
	_, err := p.Chat(ctx, messages)
	return "", "", err
}

func extForMime(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

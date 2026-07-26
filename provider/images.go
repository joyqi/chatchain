package provider

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"chatchain/internal/llm"
)

// ImagesProvider speaks the OpenAI Images dialect (/images/generations and
// /images/edits) — gpt-image models and the openai-compatible relays that
// mirror the endpoints. Same session shape as ImagenProvider: every turn is
// a stateless generation from the trailing user message, editing is the
// /edit command re-attaching the canvas, and the type is deliberately NOT
// Tunable or UsageReporter so the chat layer's capability gates apply.
//
// The dialect has no aspect-ratio or negative-prompt parameters — dimensions
// live in the single size knob — so ImageGenOptions offers only sizes and
// the /model surface renders only the Size tab.
type ImagesProvider struct {
	model        string
	client       llm.Images
	http         *http.Client // URL-form results (DALL·E) are fetched with this
	gen          ImageGenParams
	jsonEdits    bool // POST /images/edits as JSON instead of multipart (xAI)
	imagePartial func([]byte)
	lastImages   []Attachment
}

// NewImages builds the images provider; an empty baseURL targets the
// official OpenAI API.
func NewImages(apiKey, baseURL, model string, httpClient *http.Client) *ImagesProvider {
	if baseURL == "" {
		baseURL = openAIDefaultBaseURL
	}
	c := llm.New(baseURL, httpClient)
	c.Header.Set("Authorization", "Bearer "+apiKey)
	// Billed, non-idempotent generations: no transport retries (see NewImagen).
	c.Retries = 0
	return &ImagesProvider{model: model, client: llm.Images{Client: c}, http: c.HTTP}
}

func (p *ImagesProvider) Type() string      { return "images" }
func (p *ImagesProvider) Model() string     { return p.model }
func (p *ImagesProvider) SetModel(m string) { p.model = m }

func (p *ImagesProvider) SetImageGenParams(g ImageGenParams) { p.gen = g }
func (p *ImagesProvider) ImageGenParams() ImageGenParams     { return p.gen }

func (p *ImagesProvider) SetJSONEdits(on bool) { p.jsonEdits = on }
func (p *ImagesProvider) JSONEdits() bool      { return p.jsonEdits }

// SetImagePartialObserver installs (or clears) the progressive-preview sink.
// Its presence is what asks the backend to stream: an unwatched turn (-m,
// a quiet round) posts the plain unary request instead.
func (p *ImagesProvider) SetImagePartialObserver(fn func([]byte)) { p.imagePartial = fn }

func (p *ImagesProvider) ImageGenOptions() ImageGenOptions {
	return ImageGenOptions{
		ImageSizes: []string{"auto", "1024x1024", "1536x1024", "1024x1536", "1792x1024", "1024x1792"},
	}
}

func (p *ImagesProvider) LastImages() []Attachment { return p.lastImages }

// ListModels keeps models by METADATA only: relays attach output_modalities
// (keep "image"), entries without metadata are kept — the user judges, never
// name heuristics (official OpenAI lists everything unannotated).
func (p *ImagesProvider) ListModels(ctx context.Context) ([]string, error) {
	models, err := p.client.Models(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	var names []string
	for _, m := range models {
		if len(m.Output) == 0 {
			names = append(names, m.ID)
			continue
		}
		for _, out := range m.Output {
			if strings.EqualFold(out, "image") {
				names = append(names, m.ID)
				break
			}
		}
	}
	return names, nil
}

// Chat runs one generation (no references) or edit (references present).
// The returned text is always empty — images land in LastImages.
func (p *ImagesProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	p.lastImages = nil
	prompt, refs := lastUserTurn(messages)
	if strings.TrimSpace(prompt) == "" {
		return "", &PermanentError{Err: fmt.Errorf("images: empty prompt")}
	}
	// One partial frame: enough to watch the composition settle without
	// redrawing the block on every tick (the openresponses preview budget).
	onPartial := p.imagePartial
	stream, partials := onPartial != nil, 0
	if stream {
		partials = 1
	}
	var resp *llm.ImagesResponse
	var err error
	if len(refs) == 0 {
		resp, err = p.client.Generate(ctx, &llm.ImagesRequest{
			Model: p.model, Prompt: prompt, N: 1, Size: p.gen.ImageSize,
			Stream: stream, PartialImages: partials,
		}, onPartial)
	} else {
		req := &llm.ImagesEditRequest{Model: p.model, Prompt: prompt, N: 1, Size: p.gen.ImageSize,
			Stream: stream, PartialImages: partials}
		for _, a := range refs {
			req.Images = append(req.Images, llm.ImageFile{Name: a.Filename, Mime: a.MimeType, Data: a.Data})
		}
		if p.jsonEdits {
			resp, err = p.client.EditJSON(ctx, req, onPartial)
		} else {
			resp, err = p.client.Edit(ctx, req, onPartial)
		}
	}
	if err != nil {
		return "", err
	}
	for i, d := range resp.Data {
		data, mime := []byte(d.B64JSON), imageMime(d.Type(), d.B64JSON)
		if len(data) == 0 && d.URL != "" {
			// DALL·E's default response form: a short-lived link, fetched
			// immediately (unauthenticated — the URL itself is the token).
			if data, mime, err = fetchImage(ctx, p.http, d.URL); err != nil {
				return "", fmt.Errorf("images: fetching result: %w", err)
			}
		}
		if len(data) == 0 {
			continue
		}
		p.lastImages = append(p.lastImages, Attachment{
			Filename: fmt.Sprintf("image-%d%s", i+1, extForMime(mime)),
			MimeType: mime,
			Data:     data,
		})
	}
	if len(p.lastImages) == 0 {
		return "", &PermanentError{Err: fmt.Errorf("images: response contained no images")}
	}
	return "", nil
}

// StreamChat adapts the unary call, like ImagenProvider.
func (p *ImagesProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer, reasoning io.WriteCloser) (string, string, error) {
	reasoning.Close()
	_, err := p.Chat(ctx, messages)
	return "", "", err
}

// imageMime settles an inline payload's type: the backend's own declaration
// when it makes one (parameters stripped — the extension maps and the edit
// multipart replay match on the bare type), otherwise the bytes themselves.
// Sniffing is what keeps a JPEG from being filed as .png: OpenAI declares
// nothing and its default is png, but relays and output_format:jpeg|webp
// break that assumption.
func imageMime(declared string, data []byte) string {
	if declared != "" {
		if mt, _, err := mime.ParseMediaType(declared); err == nil {
			declared = mt
		}
		if strings.HasPrefix(declared, "image/") {
			return declared
		}
	}
	if sniffed := http.DetectContentType(data); strings.HasPrefix(sniffed, "image/") {
		return sniffed
	}
	return "image/png"
}

// maxImageFetch bounds a URL-form result download — far above any legitimate
// generation, but a broken relay cannot stream unbounded bytes into memory.
const maxImageFetch = 64 << 20

// fetchImage downloads a result URL, returning the bytes and the mime type
// from the response header (default image/png). The URL comes from the
// server: only http(s) is followed, the read is bounded, and no auth header
// rides along (the link itself is the token).
func fetchImage(ctx context.Context, hc *http.Client, rawURL string) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, "", fmt.Errorf("unsupported image url %q", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("image url returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageFetch+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxImageFetch {
		return nil, "", fmt.Errorf("image exceeds %d MB", maxImageFetch>>20)
	}
	// Strip content-type parameters ("image/jpeg; charset=binary"): the bare
	// type feeds the extension maps and the /edit multipart replay.
	ct := resp.Header.Get("Content-Type")
	if mt, _, perr := mime.ParseMediaType(ct); perr == nil {
		ct = mt
	}
	if !strings.HasPrefix(ct, "image/") {
		ct = "image/png"
	}
	return data, ct, nil
}

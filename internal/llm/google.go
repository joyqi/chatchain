package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Google is the generateContent dialect serving both backends in "express"
// auth mode (x-goog-api-key): the Gemini Developer API and Vertex AI.
// iota always has an API key (cmd requires one), so the genai SDK's
// ADC/OAuth path was unreachable here — express is full parity. The request
// body is identical across backends; only the URL form differs.
type Google struct {
	*Client
	Vertex  bool   // publisher-model paths (Vertex express) vs models/ (Gemini)
	Version string // API version path segment ("v1beta", "v1beta1", "v1")
}

// --- wire shapes (JSON tags are the protocol AND the persisted RawContent
// format: GContent must decode the blobs sessions saved as genai.Content) ---

type GContent struct {
	Parts []*GPart `json:"parts,omitempty"`
	Role  string   `json:"role,omitempty"`
}

// GPart models the part one-of. Fields iota never touches are kept as
// raw JSON so persisted session content (and future server additions) survive
// the round-trip verbatim.
type GPart struct {
	Text             string          `json:"text,omitempty"`
	Thought          bool            `json:"thought,omitempty"`
	ThoughtSignature []byte          `json:"thoughtSignature,omitempty"` // base64 on the wire
	InlineData       *GBlob          `json:"inlineData,omitempty"`
	FileData         json.RawMessage `json:"fileData,omitempty"`
	FunctionCall     *GFunctionCall  `json:"functionCall,omitempty"`
	FunctionResponse *GFunctionResp  `json:"functionResponse,omitempty"`

	// Opaque pass-through one-ofs (never produced by iota).
	ExecutableCode      json.RawMessage `json:"executableCode,omitempty"`
	CodeExecutionResult json.RawMessage `json:"codeExecutionResult,omitempty"`
	VideoMetadata       json.RawMessage `json:"videoMetadata,omitempty"`
}

// HasData reports whether the part's data one-of is set. Vertex rejects
// requests containing empty {} parts ("required oneof field 'data' must have
// one initialized field"); streamed responses can trail one.
func (p *GPart) HasData() bool {
	return p.Text != "" ||
		p.InlineData != nil ||
		len(p.FileData) > 0 ||
		p.FunctionCall != nil ||
		p.FunctionResponse != nil ||
		len(p.ExecutableCode) > 0 ||
		len(p.CodeExecutionResult) > 0
}

type GBlob struct {
	Data     []byte `json:"data,omitempty"` // base64 on the wire
	MimeType string `json:"mimeType,omitempty"`
}

type GFunctionCall struct {
	ID   string         `json:"id,omitempty"`
	Args map[string]any `json:"args,omitempty"`
	Name string         `json:"name,omitempty"`
}

type GFunctionResp struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name,omitempty"`
	Response map[string]any `json:"response,omitempty"`
}

type GThinkingConfig struct {
	IncludeThoughts bool   `json:"includeThoughts,omitempty"`
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
}

type GGenerationConfig struct {
	Temperature        *float32         `json:"temperature,omitempty"`
	TopP               *float32         `json:"topP,omitempty"`
	ThinkingConfig     *GThinkingConfig `json:"thinkingConfig,omitempty"`
	ResponseModalities []string         `json:"responseModalities,omitempty"`
}

type GFunctionDeclaration struct {
	Name                 string         `json:"name,omitempty"`
	Description          string         `json:"description,omitempty"`
	ParametersJsonSchema any            `json:"parametersJsonSchema,omitempty"`
	Parameters           map[string]any `json:"parameters,omitempty"`
}

type GTool struct {
	FunctionDeclarations []*GFunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type GenerateRequest struct {
	Contents          []*GContent        `json:"contents"`
	SystemInstruction *GContent          `json:"systemInstruction,omitempty"`
	GenerationConfig  *GGenerationConfig `json:"generationConfig,omitempty"`
	Tools             []*GTool           `json:"tools,omitempty"`
}

type GUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"` // EXCLUDES thoughts
	// ThoughtsTokenCount is billed reasoning that CandidatesTokenCount leaves
	// out; TotalTokenCount does include it, which is why context math prefers
	// the total.
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
}

type GenerateResponse struct {
	Candidates []struct {
		Content *GContent `json:"content"`
	} `json:"candidates"`
	UsageMetadata *GUsageMetadata `json:"usageMetadata"`
	Error         json.RawMessage `json:"error,omitempty"` // in-band stream error
}

// Text concatenates candidate 0's non-thought text parts (genai's
// Response.Text() parity — the unary Chat path).
func (r *GenerateResponse) Text() string {
	if len(r.Candidates) == 0 || r.Candidates[0].Content == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range r.Candidates[0].Content.Parts {
		if !p.Thought {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// modelPath resolves the model resource path — genai tModel parity. Vertex:
// known resource prefixes pass through; "vendor/model" names (the relay-
// station convention, e.g. zenmux's "bytedance/doubao-…") become
// publishers/{vendor}/models/{model}; bare names get publishers/google/.
// Gemini: models/ and tunedModels/ pass through, everything else (slashes
// included) is prefixed with models/. Names carrying path metacharacters are
// rejected upstream by the server; keep them out of the path here.
func (g Google) modelPath(model string) (string, error) {
	if strings.Contains(model, "?") || strings.Contains(model, "&") || strings.Contains(model, "..") {
		return "", fmt.Errorf("llm: invalid model name %q", model)
	}
	if g.Vertex {
		switch {
		case strings.HasPrefix(model, "projects/") || strings.HasPrefix(model, "models/") || strings.HasPrefix(model, "publishers/"):
			// pass through
		case strings.Contains(model, "/"):
			parts := strings.SplitN(model, "/", 2)
			model = "publishers/" + parts[0] + "/models/" + parts[1]
		default:
			model = "publishers/google/models/" + model
		}
	} else if !strings.HasPrefix(model, "models/") && !strings.HasPrefix(model, "tunedModels/") {
		model = "models/" + model
	}
	return "/" + g.Version + "/" + model, nil
}

// Generate is the non-streaming :generateContent call.
func (g Google) Generate(ctx context.Context, model string, req *GenerateRequest) (*GenerateResponse, error) {
	path, err := g.modelPath(model)
	if err != nil {
		return nil, err
	}
	var out GenerateResponse
	if err := g.Do(ctx, "POST", path+":generateContent", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StreamGenerate is :streamGenerateContent?alt=sse; each SSE event carries a
// whole GenerateResponse.
func (g Google) StreamGenerate(ctx context.Context, model string, req *GenerateRequest) (*GoogleStream, error) {
	path, err := g.modelPath(model)
	if err != nil {
		return nil, err
	}
	sse, err := g.Stream(ctx, "POST", path+":streamGenerateContent?alt=sse", req)
	if err != nil {
		return nil, err
	}
	return &GoogleStream{sse: sse}, nil
}

// GModelInfo is one listed model plus the capability metadata backends
// attach, when they do: official Gemini fills SupportedGenerationMethods
// (e.g. "generateContent", "predict"), relay stations (zenmux-style) fill
// OutputModalities (e.g. "text", "image"). Either — or both — may be empty;
// callers must treat absence as "unknown", not "unsupported".
type GModelInfo struct {
	Name    string
	Methods []string // supportedGenerationMethods
	Output  []string // outputModalities
}

// Models lists models (paginated; sorted by name). Gemini lists /models.
// Vertex tries the official publisher-model listing first, then falls back to
// the Gemini-style /v1beta/models that vertexai-compatible relay stations
// expose (probed: zenmux serves ONLY that form; the official path 404s there
// or redirects to a landing page, which surfaces as a decode error). Response
// keys accepted: models | publisherModels | data(id) — relays vary.
func (g Google) Models(ctx context.Context) ([]GModelInfo, error) {
	base := "/" + g.Version + "/models"
	if g.Vertex {
		base = "/" + g.Version + "/publishers/google/models"
	}
	models, err := g.listModels(ctx, base)
	if g.Vertex && err != nil && isListFallbackErr(err) {
		var ferr error
		if models, ferr = g.listModels(ctx, "/v1beta/models"); ferr == nil {
			return models, nil
		}
	}
	return models, err
}

// isListFallbackErr reports errors that mean "this endpoint shape does not
// exist here": a 404/405, or a non-JSON body (a relay redirecting the unknown
// path to its landing page).
func isListFallbackErr(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status == http.StatusNotFound || se.Status == http.StatusMethodNotAllowed
	}
	var je *json.SyntaxError
	return errors.As(err, &je)
}

func (g Google) listModels(ctx context.Context, base string) ([]GModelInfo, error) {
	type entry struct {
		Name    string   `json:"name"`
		ID      string   `json:"id"` // openai-style relays
		Methods []string `json:"supportedGenerationMethods"`
		Output  []string `json:"outputModalities"`
	}
	var models []GModelInfo
	pageToken := ""
	for {
		path := base
		if pageToken != "" {
			path += "?pageToken=" + url.QueryEscape(pageToken)
		}
		var out struct {
			Models          []entry `json:"models"`
			PublisherModels []entry `json:"publisherModels"`
			Data            []entry `json:"data"`
			NextPageToken   string  `json:"nextPageToken"`
		}
		if err := g.Do(ctx, "GET", path, nil, &out); err != nil {
			return nil, err
		}
		for _, list := range [][]entry{out.Models, out.PublisherModels, out.Data} {
			for _, m := range list {
				name := m.Name
				if name == "" {
					name = m.ID
				}
				models = append(models, GModelInfo{Name: name, Methods: m.Methods, Output: m.Output})
			}
		}
		if out.NextPageToken == "" {
			break
		}
		pageToken = out.NextPageToken
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models, nil
}

// --- Imagen :predict dialect (genai generate_images / edit_image parity) ---
// The dedicated image-generation endpoint: same model-path resolution and
// auth as generateContent, different envelope (instances/parameters in,
// predictions out). Official Imagen models and relay-hosted image models
// (zenmux's seedream, qwen-image, …) speak exactly this.

// GImageBlob is the image payload of the predict envelope. NOTE: the key is
// "bytesBase64Encoded" (matching the predictions output), NOT the
// generateContent inlineData form's "data" — verified live against zenmux.
type GImageBlob struct {
	BytesBase64Encoded []byte `json:"bytesBase64Encoded,omitempty"`
	MimeType           string `json:"mimeType,omitempty"`
}

// GReferenceImage is one reference image for image+text→image editing — the
// genai RawReferenceImage wire form.
type GReferenceImage struct {
	ReferenceType  string      `json:"referenceType,omitempty"` // "REFERENCE_TYPE_RAW"
	ReferenceID    int         `json:"referenceId,omitempty"`
	ReferenceImage *GImageBlob `json:"referenceImage,omitempty"`
}

type GImageInstance struct {
	Prompt          string             `json:"prompt"`
	ReferenceImages []*GReferenceImage `json:"referenceImages,omitempty"`
}

// GImageParams carries the generation knobs iota exposes; zero values
// are omitted so the server default applies. Keys follow what the genai SDK
// converters EMIT on the wire — notably sampleImageSize, not the SDK-level
// config name imageSize.
type GImageParams struct {
	SampleCount    int    `json:"sampleCount,omitempty"`
	AspectRatio    string `json:"aspectRatio,omitempty"`
	NegativePrompt string `json:"negativePrompt,omitempty"`
	ImageSize      string `json:"sampleImageSize,omitempty"`
}

type PredictRequest struct {
	Instances  []*GImageInstance `json:"instances"`
	Parameters *GImageParams     `json:"parameters,omitempty"`
}

// GPrediction is one generated image. The payload is either inline bytes or
// a URI the caller fetches — official Vertex answers with a gs:// path when
// the request asked for storage output, and relay stations reuse the field
// for a signed https link (expiring, so it must be fetched right away). A
// safety-filtered candidate carries RAIFilteredReason and neither.
type GPrediction struct {
	BytesBase64Encoded []byte `json:"bytesBase64Encoded,omitempty"`
	GcsURI             string `json:"gcsUri,omitempty"`
	MimeType           string `json:"mimeType,omitempty"`
	RAIFilteredReason  string `json:"raiFilteredReason,omitempty"`
}

type PredictResponse struct {
	Predictions []*GPrediction `json:"predictions"`
}

// Predict is the unary :predict call (no streaming variant exists).
func (g Google) Predict(ctx context.Context, model string, req *PredictRequest) (*PredictResponse, error) {
	path, err := g.modelPath(model)
	if err != nil {
		return nil, err
	}
	var out PredictResponse
	if err := g.Do(ctx, "POST", path+":predict", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GoogleStream yields GenerateResponse events until io.EOF.
type GoogleStream struct{ sse *SSE }

func (s *GoogleStream) Next() (*GenerateResponse, error) {
	evt, err := s.sse.Next()
	if err == io.EOF && !s.sse.SawEvent() {
		return nil, ErrNoEvents
	}
	if err != nil {
		return nil, err
	}
	var resp GenerateResponse
	if uerr := json.Unmarshal(evt.Data, &resp); uerr != nil {
		return nil, fmt.Errorf("llm: malformed stream chunk: %w", uerr)
	}
	if len(resp.Error) > 0 {
		return nil, fmt.Errorf("received error while streaming: %s", resp.Error)
	}
	return &resp, nil
}

func (s *GoogleStream) Close() error { return s.sse.Close() }

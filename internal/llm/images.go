package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
)

// Images is the OpenAI Images dialect — the dedicated image endpoints
// (/images/generations, /images/edits) that openai-compatible backends
// expose. Generation is JSON; editing is multipart (file uploads).
type Images struct{ *Client }

type ImagesRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	N      int    `json:"n,omitempty"`
	Size   string `json:"size,omitempty"`
	// response_format is deliberately omitted: gpt-image models reject the
	// parameter (always b64), relays default to b64, and DALL·E's default
	// url form is handled by the caller fetching the link.
}

// ImageDatum is one generated image: b64 payload (gpt-image, relays) or a
// short-lived URL (DALL·E's default response form). OpenAI states no type
// for the inline form (png unless output_format asked otherwise), while
// relays may declare one — OpenRouter answers JPEG bytes with media_type.
type ImageDatum struct {
	B64JSON   []byte `json:"b64_json,omitempty"`
	URL       string `json:"url,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

type ImagesResponse struct {
	Data []*ImageDatum `json:"data"`
}

// Generate is the unary POST /images/generations call.
func (i Images) Generate(ctx context.Context, req *ImagesRequest) (*ImagesResponse, error) {
	var out ImagesResponse
	if err := i.Do(ctx, "POST", "/images/generations", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ImageFile is one uploaded reference image of an edit call.
type ImageFile struct {
	Name string
	Mime string
	Data []byte
}

type ImagesEditRequest struct {
	Model  string
	Prompt string
	N      int
	Size   string
	Images []ImageFile
}

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

// Edit is the multipart POST /images/edits call. A single reference uploads
// as the `image` field, several as `image[]` entries — mirroring what the
// official SDKs put on the wire for the two cases.
//
// TEXT FIELDS COME FIRST. Relay gateways sniff `model` in the form's leading
// bytes to route the request; with a multi-MB image part first, the field
// falls outside the sniff window and zenmux answers 404 "Requested model is
// not valid" (reproduced live — small images masked the bug). The official
// SDKs order fields-then-files too.
func (i Images) Edit(ctx context.Context, req *ImagesEditRequest) (*ImagesResponse, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("model", req.Model)
	w.WriteField("prompt", req.Prompt)
	if req.N > 0 {
		w.WriteField("n", strconv.Itoa(req.N))
	}
	if req.Size != "" {
		w.WriteField("size", req.Size)
	}
	field := "image"
	if len(req.Images) > 1 {
		field = "image[]"
	}
	for _, f := range req.Images {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
			field, quoteEscaper.Replace(f.Name)))
		h.Set("Content-Type", f.Mime)
		part, err := w.CreatePart(h)
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", i.BaseURL+"/images/edits", &buf)
	if err != nil {
		return nil, err
	}
	for k, vs := range i.Header {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	httpReq.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := i.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The package-wide error shaping: bounded read, envelope extraction,
		// snippet truncation (client.go newStatusError).
		return nil, newStatusError(httpReq, resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out ImagesResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("llm: malformed images response: %w", err)
	}
	return &out, nil
}

// imagesEditJSONImage is one reference image of a JSON edit request: the
// xAI shape, a typed object carrying a data URI (a public URL or file id
// would fit the same field, but chatchain always has the bytes in hand).
type imagesEditJSONImage struct {
	Type string `json:"type"` // "image_url"
	URL  string `json:"url"`
}

type imagesEditJSONRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	// Image is one object for a single reference, an array for several —
	// the documented shape is the single object; the array is the natural
	// generalization and a backend that dislikes it says so explicitly.
	Image any    `json:"image"`
	Size  string `json:"size,omitempty"`
}

// EditJSON is /images/edits with a JSON body instead of multipart. Some
// backends (xAI) implement ONLY this form — their docs say the OpenAI SDK's
// images.edit() cannot be used because it posts multipart. The response
// shape is the shared one, so results flow through unchanged.
func (i Images) EditJSON(ctx context.Context, req *ImagesEditRequest) (*ImagesResponse, error) {
	images := make([]imagesEditJSONImage, 0, len(req.Images))
	for _, f := range req.Images {
		mime := f.Mime
		if mime == "" {
			mime = "image/png"
		}
		images = append(images, imagesEditJSONImage{
			Type: "image_url",
			URL:  "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(f.Data),
		})
	}
	body := &imagesEditJSONRequest{Model: req.Model, Prompt: req.Prompt, Size: req.Size}
	if len(images) == 1 {
		body.Image = images[0]
	} else {
		body.Image = images
	}
	var out ImagesResponse
	if err := i.Do(ctx, "POST", "/images/edits", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ImagesModel is one /models entry plus the output-modality metadata relay
// stations attach (official OpenAI attaches none).
type ImagesModel struct {
	ID     string   `json:"id"`
	Output []string `json:"output_modalities"`
}

// Models lists the backend's models with whatever modality metadata exists.
func (i Images) Models(ctx context.Context) ([]ImagesModel, error) {
	var out struct {
		Data []ImagesModel `json:"data"`
	}
	if err := i.Do(ctx, "GET", "/models", nil, &out); err != nil {
		return nil, err
	}
	models := out.Data
	sort.Slice(models, func(a, b int) bool { return models[a].ID < models[b].ID })
	return models, nil
}

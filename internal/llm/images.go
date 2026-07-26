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
	// Stream asks for progressive frames; PartialImages (0–3) caps how many
	// arrive before the finished picture. A backend that ignores the flags
	// simply answers with the unary JSON body — see consumeImages.
	Stream        bool `json:"stream,omitempty"`
	PartialImages int  `json:"partial_images,omitempty"`
	// response_format is deliberately omitted: gpt-image models reject the
	// parameter (always b64), relays default to b64, and DALL·E's default
	// url form is handled by the caller fetching the link.
}

// ImageDatum is one generated image: b64 payload (gpt-image, relays) or a
// short-lived URL (DALL·E's default response form). OpenAI states no type
// for the inline form (png unless output_format asked otherwise), while
// relays may declare one — OpenRouter answers JPEG bytes with media_type.
type ImageDatum struct {
	B64JSON []byte `json:"b64_json,omitempty"`
	URL     string `json:"url,omitempty"`
	// Backends spell the type declaration three ways: media_type (OpenRouter),
	// mime_type (xAI), output_format on streaming events. Type() folds them.
	MediaType string `json:"media_type,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
}

// Type returns the declared mime type, "" when the payload declares none
// (the caller then sniffs the bytes).
func (d *ImageDatum) Type() string {
	if d.MediaType != "" {
		return d.MediaType
	}
	return d.MimeType
}

type ImagesResponse struct {
	Data []*ImageDatum `json:"data"`
}

// Generate is POST /images/generations. With req.Stream set, partial frames
// are handed to onPartial as they arrive; onPartial may be nil.
func (i Images) Generate(ctx context.Context, req *ImagesRequest, onPartial func([]byte)) (*ImagesResponse, error) {
	resp, err := i.send(ctx, "POST", "/images/generations", req)
	if err != nil {
		return nil, err
	}
	return consumeImages(resp, onPartial)
}

// imagesStreamEvent is one SSE frame of a streaming images call. Event names
// are matched by SUFFIX: the Images API says image_generation.partial_image
// while the Responses API wraps the same payload as
// response.image_generation_call.partial_image.
type imagesStreamEvent struct {
	Type    string `json:"type"`
	B64JSON []byte `json:"b64_json,omitempty"`
	// The type declaration differs from the unary body's: streaming events
	// name a bare format ("png", "jpeg", "webp"), relays reuse media_type.
	OutputFormat string          `json:"output_format,omitempty"`
	MediaType    string          `json:"media_type,omitempty"`
	Error        json.RawMessage `json:"error,omitempty"`
}

// mediaType folds an event's type declaration into a mime type ("" when it
// declares none — the caller then sniffs the bytes).
func (e imagesStreamEvent) mediaType() string {
	if e.MediaType != "" {
		return e.MediaType
	}
	switch e.OutputFormat {
	case "png", "jpeg", "webp", "gif":
		return "image/" + e.OutputFormat
	case "jpg":
		return "image/jpeg"
	}
	return ""
}

// consumeImages interprets an images response either way: as an SSE stream
// when the server honored stream:true (partials go to onPartial, the
// completed frame is the result), or as the plain JSON body when it did not.
// Relays commonly ignore the flag, and asking again "properly" would bill a
// second generation — so the Content-Type decides, not the request.
func consumeImages(resp *http.Response, onPartial func([]byte)) (*ImagesResponse, error) {
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var out ImagesResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("llm: malformed images response: %w", err)
		}
		return &out, nil
	}

	sse := newSSE(resp.Body)
	var final *ImageDatum
	for {
		evt, err := sse.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		var e imagesStreamEvent
		if uerr := json.Unmarshal(evt.Data, &e); uerr != nil {
			continue // a keepalive or a frame shape we do not model
		}
		if len(e.Error) > 0 {
			return nil, fmt.Errorf("received error while streaming: %s", e.Error)
		}
		switch {
		case strings.HasSuffix(e.Type, ".partial_image"):
			if onPartial != nil && len(e.B64JSON) > 0 {
				onPartial(e.B64JSON)
			}
		case strings.HasSuffix(e.Type, ".completed"):
			if len(e.B64JSON) > 0 {
				final = &ImageDatum{B64JSON: e.B64JSON, MediaType: e.mediaType()}
			}
		}
	}
	if final == nil {
		return nil, fmt.Errorf("llm: image stream ended without a completed image")
	}
	return &ImagesResponse{Data: []*ImageDatum{final}}, nil
}

// ImageFile is one uploaded reference image of an edit call.
type ImageFile struct {
	Name string
	Mime string
	Data []byte
}

type ImagesEditRequest struct {
	Model         string
	Prompt        string
	N             int
	Size          string
	Stream        bool
	PartialImages int
	Images        []ImageFile
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
func (i Images) Edit(ctx context.Context, req *ImagesEditRequest, onPartial func([]byte)) (*ImagesResponse, error) {
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
	if req.Stream {
		w.WriteField("stream", "true")
		w.WriteField("partial_images", strconv.Itoa(req.PartialImages))
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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The package-wide error shaping: bounded read, envelope extraction,
		// snippet truncation (client.go newStatusError).
		return nil, newStatusError(httpReq, resp)
	}
	return consumeImages(resp, onPartial)
}

// imagesEditJSONImage is one reference image of a JSON edit request: the
// xAI shape, a typed object carrying a data URI (a public URL or file id
// would fit the same field, but chatchain always has the bytes in hand).
type imagesEditJSONImage struct {
	Type string `json:"type"` // "image_url"
	URL  string `json:"url"`
}

type imagesEditJSONRequest struct {
	Model         string `json:"model"`
	Prompt        string `json:"prompt"`
	Stream        bool   `json:"stream,omitempty"`
	PartialImages int    `json:"partial_images,omitempty"`
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
func (i Images) EditJSON(ctx context.Context, req *ImagesEditRequest, onPartial func([]byte)) (*ImagesResponse, error) {
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
	body := &imagesEditJSONRequest{Model: req.Model, Prompt: req.Prompt, Size: req.Size,
		Stream: req.Stream, PartialImages: req.PartialImages}
	if len(images) == 1 {
		body.Image = images[0]
	} else {
		body.Image = images
	}
	resp, err := i.send(ctx, "POST", "/images/edits", body)
	if err != nil {
		return nil, err
	}
	return consumeImages(resp, onPartial)
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

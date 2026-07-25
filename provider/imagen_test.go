package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chatchain/internal/llm"
)

// TestImagenGoldenRequest pins the :predict request wire for the relay
// (vertex-form) backend: publishers/{vendor}/models path on v1, x-goog-api-key
// auth, the instances/parameters envelope, and the stateless-per-message
// reference derivation — ONLY the final user message's image attachments
// become referenceImages (earlier history, including the previous assistant
// image, is record, not input; /edit re-attaches explicitly).
func TestImagenGoldenRequest(t *testing.T) {
	var path, key string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		key = r.Header.Get("x-goog-api-key")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Write([]byte(`{"predictions":[{"bytesBase64Encoded":"CQ==","mimeType":"image/jpeg"},{"bytesBase64Encoded":"Cg=="}]}`))
	}))
	defer srv.Close()

	p := NewImagen("k", srv.URL, "bytedance/doubao-seedream-5.0-pro", srv.Client())
	p.SetImageGenParams(ImageGenParams{AspectRatio: "3:2", ImageSize: "2K", NegativePrompt: "blurry"})
	text, err := p.Chat(context.Background(), []Message{
		{Role: "user", Content: "a cat"},
		{Role: "assistant", Attachments: []Attachment{{Filename: "image-1.png", MimeType: "image/png", Data: []byte{1}}}},
		{Role: "user", Content: "add a robot",
			Attachments: []Attachment{
				{Filename: "ref.png", MimeType: "image/png", Data: []byte{2}},
				{Filename: "notes.txt", MimeType: "text/plain", Data: []byte("x")}, // non-image: dropped
			}},
	})
	if err != nil || text != "" {
		t.Fatalf("Chat: %q %v", text, err)
	}
	if path != "/v1/publishers/bytedance/models/doubao-seedream-5.0-pro:predict" {
		t.Fatalf("path = %s", path)
	}
	if key != "k" {
		t.Fatalf("key = %q", key)
	}

	instances := got["instances"].([]any)
	inst := instances[0].(map[string]any)
	if inst["prompt"] != "add a robot" {
		t.Fatalf("prompt = %v", inst["prompt"])
	}
	refs := inst["referenceImages"].([]any)
	if len(refs) != 1 {
		t.Fatalf("referenceImages = %d, want ONLY this message's image attachment (history is not mined)", len(refs))
	}
	r0 := refs[0].(map[string]any)
	if r0["referenceType"] != "REFERENCE_TYPE_RAW" || r0["referenceId"] != float64(1) {
		t.Fatalf("ref[0] = %v", r0)
	}
	// The predict envelope's image key is bytesBase64Encoded (NOT inlineData's
	// "data") — pinned here because the live API silently ignores unknown keys.
	if img := r0["referenceImage"].(map[string]any); img["bytesBase64Encoded"] != "Ag==" { // []byte{2} — the user's attachment
		t.Fatalf("ref[0] image = %v", img)
	}
	params := got["parameters"].(map[string]any)
	// sampleCount pinned to 1 (official Imagen would default to 4); the size
	// key is sampleImageSize on the wire (genai converter parity), NOT the
	// SDK-level config name imageSize.
	if params["aspectRatio"] != "3:2" || params["sampleImageSize"] != "2K" ||
		params["negativePrompt"] != "blurry" || params["sampleCount"] != float64(1) {
		t.Fatalf("parameters = %v", params)
	}

	// Outputs land in LastImages: mime respected, default png, extensions.
	imgs := p.LastImages()
	if len(imgs) != 2 {
		t.Fatalf("LastImages = %d", len(imgs))
	}
	if imgs[0].MimeType != "image/jpeg" || imgs[0].Filename != "image-1.jpg" {
		t.Fatalf("imgs[0] = %+v", imgs[0])
	}
	if imgs[1].MimeType != "image/png" || imgs[1].Filename != "image-2.png" {
		t.Fatalf("imgs[1] = %+v", imgs[1])
	}
}

// The official Gemini Developer API form: models/{model}:predict on v1beta.
func TestImagenOfficialPathForm(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Write([]byte(`{"predictions":[{"bytesBase64Encoded":"CQ=="}]}`))
	}))
	defer srv.Close()
	c := llm.New(srv.URL, srv.Client())
	p := &ImagenProvider{model: "imagen-4.0-generate-001", client: llm.Google{Client: c, Vertex: false, Version: "v1beta"}}
	if _, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "a cat"}}); err != nil {
		t.Fatal(err)
	}
	if path != "/v1beta/models/imagen-4.0-generate-001:predict" {
		t.Fatalf("path = %s", path)
	}
}

func TestImagenSafetyFiltered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"predictions":[{"raiFilteredReason":"unsafe prompt"}]}`))
	}))
	defer srv.Close()
	p := NewImagen("k", srv.URL, "m", srv.Client())
	_, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "x"}})
	if err == nil || !strings.Contains(err.Error(), "unsafe prompt") {
		t.Fatalf("err = %v, want the filter reason surfaced", err)
	}
	// Deterministic failures are PermanentError: the chat retry loop must not
	// re-bill a call that will fail identically.
	var pe *PermanentError
	if !errors.As(err, &pe) {
		t.Fatalf("safety filter must surface as PermanentError, got %T", err)
	}
	_, err = p.Chat(context.Background(), []Message{{Role: "assistant", Content: "no user turn"}})
	if err == nil || !errors.As(err, &pe) {
		t.Fatalf("empty prompt must be a PermanentError before hitting the API, got %v", err)
	}
}

// ListModels keeps models by METADATA only: outputModalities containing image
// (relay form) or supportedGenerationMethods containing predict (official
// form); entries with no metadata at all are kept — never name heuristics.
func TestImagenListModelsFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models" {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{"models":[
			{"name":"chat-model","outputModalities":["text"]},
			{"name":"image-model","outputModalities":["image"]},
			{"name":"imagen-x","supportedGenerationMethods":["predict"]},
			{"name":"gen-model","supportedGenerationMethods":["generateContent"]},
			{"name":"bare-model"}
		]}`))
	}))
	defer srv.Close()
	p := NewImagen("k", srv.URL, "m", srv.Client())
	names, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bare-model", "image-model", "imagen-x"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

type closeRecorder struct{ closed bool }

func (c *closeRecorder) Write(p []byte) (int, error) { return len(p), nil }
func (c *closeRecorder) Close() error                { c.closed = true; return nil }

// StreamChat adapts the unary predict: reasoning closes up front (the chat
// layer requires it before content), nothing is written, images arrive via
// LastImages exactly like the conversational image providers.
func TestImagenStreamChatAdapts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"predictions":[{"bytesBase64Encoded":"CQ=="}]}`))
	}))
	defer srv.Close()
	p := NewImagen("k", srv.URL, "m", srv.Client())
	var out strings.Builder
	rec := &closeRecorder{}
	content, reasoning, err := p.StreamChat(context.Background(), []Message{{Role: "user", Content: "a cat"}}, &out, rec)
	if err != nil || content != "" || reasoning != "" || out.Len() != 0 {
		t.Fatalf("StreamChat: %q %q %v (wrote %q)", content, reasoning, err, out.String())
	}
	if !rec.closed {
		t.Fatal("reasoning must be closed before returning")
	}
	if len(p.LastImages()) != 1 {
		t.Fatal("images missing after stream adaptation")
	}
}

// The type stays deliberately un-Tunable and token-less: the chat layer's
// capability gates (effort/temperature tabs, compaction, ctx meter) key off
// these assertions.
func TestImagenCapabilitySurface(t *testing.T) {
	var p Provider = NewImagen("k", "", "m", nil)
	if _, ok := p.(Tunable); ok {
		t.Fatal("imagen must not be Tunable")
	}
	if _, ok := p.(UsageReporter); ok {
		t.Fatal("imagen must not be a UsageReporter")
	}
	if _, ok := p.(ImageOutputProvider); !ok {
		t.Fatal("imagen must surface LastImages")
	}
	if tun, ok := p.(ImageGenTunable); !ok {
		t.Fatal("imagen must expose its generation params")
	} else if o := tun.ImageGenOptions(); len(o.AspectRatios) == 0 || len(o.ImageSizes) == 0 {
		t.Fatal("imagen must offer choice lists for the /model tabs")
	}
}

// Relay-hosted models answer with a signed URL instead of inline bytes
// (predictions[].gcsUri). It is fetched immediately — the link expires — with
// no API key attached (the signature IS the credential), and the response
// header decides the mime.
func TestImagenGcsURIFetched(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/blob.png", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "" || r.Header.Get("Authorization") != "" {
			t.Error("signed-URL fetch must not carry credentials")
		}
		w.Header().Set("Content-Type", "image/jpeg; charset=binary")
		w.Write([]byte{7, 7, 7})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"predictions":[{"gcsUri":"` + srv.URL + `/blob.png?Expires=1&Signature=x"}]}`))
	})

	p := NewImagen("k", srv.URL, "klingai/kling-v2", srv.Client())
	if _, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "a cat"}}); err != nil {
		t.Fatal(err)
	}
	imgs := p.LastImages()
	if len(imgs) != 1 || len(imgs[0].Data) != 3 {
		t.Fatalf("LastImages = %+v", imgs)
	}
	// Mime params are stripped, so the extension maps still match exactly.
	if imgs[0].MimeType != "image/jpeg" || imgs[0].Filename != "image-1.jpg" {
		t.Fatalf("fetched attachment = %+v", imgs[0])
	}
}

// A gs:// URI needs Google credentials chatchain never holds: fail loudly
// instead of reporting "no images".
func TestImagenGcsURIUnfetchable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"predictions":[{"gcsUri":"gs://bucket/out/image-1.png"}]}`))
	}))
	defer srv.Close()
	p := NewImagen("k", srv.URL, "m", srv.Client())
	_, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "x"}})
	if err == nil || !strings.Contains(err.Error(), "gs://bucket/out/image-1.png") {
		t.Fatalf("err = %v, want the unsupported URI surfaced", err)
	}
}

// An empty prediction set still says how many came back — the diagnosis that
// exposed the gcsUri gap needed /debug to make.
func TestImagenNoImagesErrorIsDiagnostic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"predictions":[{},{}]}`))
	}))
	defer srv.Close()
	p := NewImagen("k", srv.URL, "m", srv.Client())
	_, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "x"}})
	if err == nil || !strings.Contains(err.Error(), "2 prediction(s)") {
		t.Fatalf("err = %v, want the prediction count", err)
	}
}

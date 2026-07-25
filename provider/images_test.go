package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestImagesGoldenGenerate pins the /images/generations wire: bearer auth,
// n pinned to 1, size passthrough, b64 response into LastImages.
func TestImagesGoldenGenerate(t *testing.T) {
	var path, auth string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Write([]byte(`{"data":[{"b64_json":"CQ=="}]}`))
	}))
	defer srv.Close()

	p := NewImages("k", srv.URL, "gpt-image-1", srv.Client())
	p.SetImageGenParams(ImageGenParams{ImageSize: "1024x1024"})
	text, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "a cat"}})
	if err != nil || text != "" {
		t.Fatalf("Chat: %q %v", text, err)
	}
	if path != "/images/generations" || auth != "Bearer k" {
		t.Fatalf("path=%s auth=%q", path, auth)
	}
	if got["model"] != "gpt-image-1" || got["prompt"] != "a cat" || got["n"] != float64(1) || got["size"] != "1024x1024" {
		t.Fatalf("body = %v", got)
	}
	if _, ok := got["response_format"]; ok {
		t.Fatal("response_format must be omitted (gpt-image rejects it)")
	}
	imgs := p.LastImages()
	if len(imgs) != 1 || imgs[0].MimeType != "image/png" || imgs[0].Filename != "image-1.png" {
		t.Fatalf("LastImages = %+v", imgs)
	}
}

// References switch the call to multipart /images/edits: one ref uploads as
// `image`, several as `image[]`, with per-part content types and the same
// form fields.
func TestImagesGoldenEdit(t *testing.T) {
	var path, prompt, model, n, size string
	var fields map[string][]*struct {
		name, mime string
		data       []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		var raw bytes.Buffer
		r.Body = io.NopCloser(io.TeeReader(r.Body, &raw))
		r.ParseMultipartForm(1 << 20)
		// Text fields must precede the file parts: relay gateways sniff
		// `model` in the form's leading bytes (a multi-MB image first pushed
		// it out of the window → 404 invalid_model, reproduced live).
		if i, j := bytes.Index(raw.Bytes(), []byte(`name="model"`)), bytes.Index(raw.Bytes(), []byte(`name="image`)); i < 0 || j < 0 || i > j {
			t.Errorf("model field must precede file parts (model@%d image@%d)", i, j)
		}
		prompt, model = r.FormValue("prompt"), r.FormValue("model")
		n, size = r.FormValue("n"), r.FormValue("size")
		fields = map[string][]*struct {
			name, mime string
			data       []byte
		}{}
		for field, files := range r.MultipartForm.File {
			for _, fh := range files {
				f, _ := fh.Open()
				data, _ := io.ReadAll(f)
				f.Close()
				fields[field] = append(fields[field], &struct {
					name, mime string
					data       []byte
				}{fh.Filename, fh.Header.Get("Content-Type"), data})
			}
		}
		w.Write([]byte(`{"data":[{"b64_json":"CQ=="}]}`))
	}))
	defer srv.Close()

	p := NewImages("k", srv.URL, "gpt-image-1", srv.Client())
	ref := Attachment{Filename: "image-1.png", MimeType: "image/png", Data: []byte{7}}
	if _, err := p.Chat(context.Background(), []Message{
		{Role: "user", Content: "add a robot", Attachments: []Attachment{ref}},
	}); err != nil {
		t.Fatal(err)
	}
	if path != "/images/edits" || prompt != "add a robot" || model != "gpt-image-1" || n != "1" || size != "" {
		t.Fatalf("form: path=%s prompt=%q model=%q n=%q size=%q", path, prompt, model, n, size)
	}
	files := fields["image"]
	if len(files) != 1 || files[0].name != "image-1.png" || files[0].mime != "image/png" || files[0].data[0] != 7 {
		t.Fatalf("single ref must upload as `image`: %+v", fields)
	}

	// Two references: the field name becomes image[].
	if _, err := p.Chat(context.Background(), []Message{
		{Role: "user", Content: "merge them", Attachments: []Attachment{ref,
			{Filename: "b.jpg", MimeType: "image/jpeg", Data: []byte{8}}}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(fields["image[]"]) != 2 {
		t.Fatalf("multiple refs must upload as image[]: %+v", fields)
	}
}

// DALL·E's default response form is a short-lived URL — fetched immediately,
// mime taken from the response header.
func TestImagesURLFallback(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/blob", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("blob fetch must not carry the API key")
		}
		// Parameterful content type: the stored mime must come back as the
		// bare type (extension maps and the /edit replay exact-match it).
		w.Header().Set("Content-Type", "image/jpeg; charset=binary")
		w.Write([]byte{9, 9})
	})
	mux.HandleFunc("/images/generations", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"url":"` + srv.URL + `/blob"}]}`))
	})

	p := NewImages("k", srv.URL, "dall-e-3", srv.Client())
	if _, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "a cat"}}); err != nil {
		t.Fatal(err)
	}
	imgs := p.LastImages()
	if len(imgs) != 1 || imgs[0].MimeType != "image/jpeg" || len(imgs[0].Data) != 2 {
		t.Fatalf("LastImages = %+v", imgs)
	}
}

func TestImagesListModelsFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{"data":[
			{"id":"chat-model","output_modalities":["text"]},
			{"id":"image-model","output_modalities":["image"]},
			{"id":"bare-model"}
		]}`))
	}))
	defer srv.Close()
	p := NewImages("k", srv.URL, "m", srv.Client())
	names, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "bare-model,image-model" {
		t.Fatalf("names = %v", names)
	}
}

func TestImagesCapabilitySurface(t *testing.T) {
	var p Provider = NewImages("k", "", "m", nil)
	if _, ok := p.(Tunable); ok {
		t.Fatal("images must not be Tunable")
	}
	if _, ok := p.(UsageReporter); ok {
		t.Fatal("images must not be a UsageReporter")
	}
	tun, ok := p.(ImageGenTunable)
	if !ok {
		t.Fatal("images must expose generation params")
	}
	o := tun.ImageGenOptions()
	if len(o.ImageSizes) == 0 || len(o.AspectRatios) != 0 || o.NegativePrompt {
		t.Fatalf("options = %+v: the dialect has sizes only", o)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	pe := NewImages("k", srv.URL, "m", srv.Client())
	var perm *PermanentError
	if _, err := pe.Chat(context.Background(), []Message{{Role: "user", Content: "x"}}); !errors.As(err, &perm) {
		t.Fatalf("empty data must be a PermanentError, got %v", err)
	}
	if _, err := pe.Chat(context.Background(), nil); !errors.As(err, &perm) {
		t.Fatalf("empty prompt must be a PermanentError, got %v", err)
	}
}

// JSON edits: some backends (xAI) accept ONLY a JSON body on /images/edits
// and reject multipart. The switch routes there; a single reference is the
// documented object form, several become an array; results parse the same.
func TestImagesJSONEdits(t *testing.T) {
	var ct string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/edits" {
			t.Errorf("path = %s", r.URL.Path)
		}
		ct = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		got = map[string]any{}
		json.Unmarshal(body, &got)
		w.Write([]byte(`{"data":[{"b64_json":"CQ=="}]}`))
	}))
	defer srv.Close()

	p := NewImages("k", srv.URL, "grok-imagine-image-quality", srv.Client())
	p.SetJSONEdits(true)
	ref := Attachment{Filename: "a.png", MimeType: "image/png", Data: []byte{1}}
	if _, err := p.Chat(context.Background(), []Message{
		{Role: "user", Content: "add a hat", Attachments: []Attachment{ref}},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	if got["model"] != "grok-imagine-image-quality" || got["prompt"] != "add a hat" {
		t.Fatalf("body = %v", got)
	}
	img, ok := got["image"].(map[string]any)
	if !ok {
		t.Fatalf("single reference must be an object, got %T", got["image"])
	}
	if img["type"] != "image_url" || img["url"] != "data:image/png;base64,AQ==" {
		t.Fatalf("image = %v", img)
	}
	if len(p.LastImages()) != 1 {
		t.Fatalf("response parsing broke: %+v", p.LastImages())
	}

	// Several references generalize to an array.
	if _, err := p.Chat(context.Background(), []Message{
		{Role: "user", Content: "merge", Attachments: []Attachment{ref,
			{Filename: "b.jpg", MimeType: "image/jpeg", Data: []byte{2}}}},
	}); err != nil {
		t.Fatal(err)
	}
	arr, ok := got["image"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("multiple references must be an array, got %T %v", got["image"], got["image"])
	}
	if second := arr[1].(map[string]any); second["url"] != "data:image/jpeg;base64,Ag==" {
		t.Fatalf("array entry = %v", second)
	}

	// The switch is off by default: OpenAI and its mirrors want multipart.
	if NewImages("k", srv.URL, "gpt-image-1", srv.Client()).JSONEdits() {
		t.Fatal("json edits must default to off")
	}
}

// Inline payload types: a declared media_type wins (parameters stripped),
// otherwise the bytes are sniffed — OpenAI declares nothing and defaults to
// png, but relays answer JPEG and output_format can ask for webp, so a
// hardcoded png would file the wrong extension and replay the wrong part
// type on the next edit.
func TestImageMimeResolution(t *testing.T) {
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 0, 0, 0, 0, 0, 0, 0, 0}
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}

	for name, tc := range map[string]struct {
		declared string
		data     []byte
		want     string
	}{
		"declared wins":           {"image/jpeg", png, "image/jpeg"},
		"declared params strip":   {"image/jpeg; charset=binary", png, "image/jpeg"},
		"sniffed when undeclared": {"", jpeg, "image/jpeg"},
		"sniffed png":             {"", png, "image/png"},
		"non-image declaration":   {"application/octet-stream", jpeg, "image/jpeg"},
		"unknown bytes":           {"", []byte{0, 1, 2, 3}, "image/png"},
	} {
		if got := imageMime(tc.declared, tc.data); got != tc.want {
			t.Errorf("%s: imageMime(%q, …) = %q, want %q", name, tc.declared, got, tc.want)
		}
	}
}

// The relay shape end to end: JPEG bytes announced by media_type must reach
// LastImages as image/jpeg with a .jpg name (OpenRouter's answer).
func TestImagesMediaTypeHonored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"b64_json":"/9j/4AAQ","media_type":"image/jpeg"}]}`))
	}))
	defer srv.Close()
	p := NewImages("k", srv.URL, "google/gemini-3.1-flash-lite-image", srv.Client())
	if _, err := p.Chat(context.Background(), []Message{{Role: "user", Content: "a cat"}}); err != nil {
		t.Fatal(err)
	}
	imgs := p.LastImages()
	if len(imgs) != 1 || imgs[0].MimeType != "image/jpeg" || imgs[0].Filename != "image-1.jpg" {
		t.Fatalf("LastImages = %+v", imgs)
	}
}

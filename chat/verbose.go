package chat

import (
	"bytes"
	"io"
	"net/http"
	"os"
)

// VerboseTransport wraps an http.RoundTripper and logs request/response bodies.
type VerboseTransport struct {
	Transport http.RoundTripper
}

func (t *VerboseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	transport := t.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	// Log request
	DimStyle.Fprintf(os.Stderr, "→ %s %s\n", req.Method, req.URL)
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err == nil {
			req.Body = io.NopCloser(bytes.NewReader(body))
			DimStyle.Fprintf(os.Stderr, "→ %s\n", body)
		}
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		DimStyle.Fprintf(os.Stderr, "← error: %v\n", err)
		return resp, err
	}

	// Log response
	DimStyle.Fprintf(os.Stderr, "← %s\n", resp.Status)
	if resp.Body != nil {
		body, readErr := io.ReadAll(resp.Body)
		if readErr == nil {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			DimStyle.Fprintf(os.Stderr, "← %s\n", body)
		}
	}

	return resp, nil
}

// NewVerboseHTTPClient returns an *http.Client that logs request/response bodies.
func NewVerboseHTTPClient() *http.Client {
	return &http.Client{
		Transport: &VerboseTransport{},
	}
}


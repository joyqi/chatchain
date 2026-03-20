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

	// Log response status, then tee body so streaming is preserved
	DimStyle.Fprintf(os.Stderr, "← %s\n", resp.Status)
	if resp.Body != nil {
		resp.Body = &verboseBody{
			rc:  resp.Body,
			out: os.Stderr,
		}
	}

	return resp, nil
}

// verboseBody wraps a response body, buffering partial lines so that
// each SSE event (data: {...}) is logged as a complete line.
type verboseBody struct {
	rc     io.ReadCloser
	out    io.Writer
	logBuf []byte
}

func (v *verboseBody) Read(p []byte) (int, error) {
	n, err := v.rc.Read(p)
	if n > 0 {
		v.logBuf = append(v.logBuf, p[:n]...)
		// Flush complete lines
		for {
			idx := bytes.IndexByte(v.logBuf, '\n')
			if idx < 0 {
				break
			}
			line := v.logBuf[:idx]
			v.logBuf = v.logBuf[idx+1:]
			if len(line) > 0 {
				DimStyle.Fprintf(v.out, "← %s\n", line)
			}
		}
	}
	// Flush remaining buffer on EOF/error
	if err != nil && len(v.logBuf) > 0 {
		DimStyle.Fprintf(v.out, "← %s\n", v.logBuf)
		v.logBuf = nil
	}
	return n, err
}

func (v *verboseBody) Close() error {
	return v.rc.Close()
}

// NewVerboseHTTPClient returns an *http.Client that logs request/response bodies.
func NewVerboseHTTPClient() *http.Client {
	return &http.Client{
		Transport: &VerboseTransport{},
	}
}


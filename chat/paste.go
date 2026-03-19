package chat

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
)

var (
	pasteStartSeq = []byte("\033[200~")
	pasteEndSeq   = []byte("\033[201~")
)

// pasteFilter wraps an io.Reader (typically os.Stdin) to handle
// bracketed paste mode. When multi-line text is pasted, the full
// content is captured and stored, and a short preview tag is fed to
// readline instead:
//
//	[#1 first few chars... 5 lines]
//
// The calling code retrieves the stored paste via ConsumePaste and
// replaces the tag in the readline result with the real content.
type pasteFilter struct {
	r        io.Reader
	outBuf   bytes.Buffer // processed bytes waiting to be returned to readline
	inPaste  bool
	pasteBuf bytes.Buffer // accumulates raw bytes during a paste

	mu      sync.Mutex
	pastes  []string // stored paste blocks, indexed from 0
	lastTag string   // the tag string of the most recent paste
}

func (pf *pasteFilter) Close() error {
	if c, ok := pf.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// ConsumePaste returns the stored paste text for the given tag and
// removes it. Returns empty string if tag is not found.
func (pf *pasteFilter) ConsumePaste(tag string) string {
	pf.mu.Lock()
	defer pf.mu.Unlock()

	var idx int
	if _, err := fmt.Sscanf(tag, "#%d ", &idx); err != nil {
		return ""
	}
	idx-- // tags are 1-based
	if idx < 0 || idx >= len(pf.pastes) {
		return ""
	}
	text := pf.pastes[idx]
	pf.pastes[idx] = "" // free memory
	return text
}

func (pf *pasteFilter) Read(p []byte) (int, error) {
	// Return any buffered processed bytes first.
	if pf.outBuf.Len() > 0 {
		return pf.outBuf.Read(p)
	}

	// Keep reading from underlying reader until we have output bytes.
	// This is important: during paste accumulation, processChunk may
	// return empty output. We must NOT return (0, io.EOF) to readline
	// in that case — instead keep reading until paste ends.
	for {
		tmp := make([]byte, 4096)
		n, err := pf.r.Read(tmp)
		if n == 0 {
			return 0, err
		}

		out := pf.processChunk(tmp[:n])
		if len(out) > 0 {
			pf.outBuf.Write(out)
			return pf.outBuf.Read(p)
		}
		// out is empty — still accumulating paste data, loop.
	}
}

// processChunk handles bracketed paste detection and content substitution.
func (pf *pasteFilter) processChunk(data []byte) []byte {
	var out []byte

	for len(data) > 0 {
		if pf.inPaste {
			if idx := bytes.Index(data, pasteEndSeq); idx >= 0 {
				pf.pasteBuf.Write(data[:idx])
				data = data[idx+len(pasteEndSeq):]
				pf.inPaste = false

				// Paste ended — process captured content.
				raw := pf.pasteBuf.String()
				pf.pasteBuf.Reset()

				// Normalize line endings.
				raw = strings.ReplaceAll(raw, "\r\n", "\n")
				raw = strings.ReplaceAll(raw, "\r", "\n")
				raw = strings.TrimRight(raw, "\n")

				if strings.Contains(raw, "\n") {
					// Multi-line: store and emit preview tag.
					preview := pf.storePaste(raw)
					out = append(out, []byte(preview)...)
				} else {
					// Single line: pass through as-is.
					out = append(out, []byte(raw)...)
				}
			} else {
				// Haven't seen paste-end yet; accumulate everything.
				pf.pasteBuf.Write(data)
				data = nil
			}
		} else {
			if idx := bytes.Index(data, pasteStartSeq); idx >= 0 {
				// Pass through any bytes before the paste start.
				out = append(out, data[:idx]...)
				data = data[idx+len(pasteStartSeq):]
				pf.inPaste = true
				pf.pasteBuf.Reset()
			} else {
				// No paste sequence, pass through.
				out = append(out, data...)
				data = nil
			}
		}
	}

	return out
}

// storePaste saves the pasted text and returns a preview tag like:
//
//	[#1 Hello world... 10 lines]
func (pf *pasteFilter) storePaste(text string) string {
	pf.mu.Lock()
	defer pf.mu.Unlock()

	pf.pastes = append(pf.pastes, text)
	id := len(pf.pastes) // 1-based

	lines := strings.Split(text, "\n")
	lineCount := len(lines)

	// Take first line, truncate if needed.
	first := strings.TrimSpace(lines[0])
	runes := []rune(first)
	if len(runes) > 20 {
		first = string(runes[:20])
	}

	tag := fmt.Sprintf("#%d ", id)
	pf.lastTag = tag
	return fmt.Sprintf("[%s%s... %d lines]", tag, first, lineCount)
}

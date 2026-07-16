package llm

import (
	"bufio"
	"bytes"
	"io"
)

// Event is one server-sent event: the event: field (often empty — OpenAI
// dispatches by a "type" INSIDE the data JSON) and the joined data: payload.
type Event struct {
	Type string
	Data []byte
}

// SSE reads server-sent events off a response body. Parsing rules match the
// SDKs': fields split at the first ':', one optional leading space stripped
// from the value, ':' comment lines skipped, multiple data: lines joined with
// '\n', events dispatched on blank lines. A "[DONE]" data sentinel marks the
// logical end; reading continues (drain, for connection reuse) until EOF.
type SSE struct {
	body     io.ReadCloser
	r        *bufio.Reader // ReadBytes, not Scanner: data lines can be huge
	done     bool          // saw the [DONE] sentinel
	sawEvent bool
}

func newSSE(body io.ReadCloser) *SSE {
	return &SSE{body: body, r: bufio.NewReader(body)}
}

// Next returns the next event. io.EOF means the stream ended (cleanly when
// Done() or after terminal events; see ErrNoEvents for the none-at-all case,
// which the dialects surface). A ctx cancel aborts the underlying body read,
// surfacing here as its read error — that is the interrupt path.
func (s *SSE) Next() (Event, error) {
	var evt Event
	var data bytes.Buffer
	haveData := false
	for {
		line, err := s.r.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 && err == nil {
			// Blank line: dispatch if an event accumulated.
			if !haveData {
				evt.Type = ""
				continue
			}
			d := data.Bytes()
			if bytes.HasPrefix(d, []byte("[DONE]")) {
				s.done = true
				data.Reset()
				haveData = false
				evt.Type = ""
				continue // drain the remainder
			}
			s.sawEvent = true
			return Event{Type: evt.Type, Data: append([]byte(nil), d...)}, nil
		}
		if len(line) > 0 {
			if line[0] == ':' { // comment
				if err == nil {
					continue
				}
			} else {
				field, val := string(line), []byte(nil)
				if i := bytes.IndexByte(line, ':'); i >= 0 {
					field = string(line[:i])
					val = line[i+1:]
					if len(val) > 0 && val[0] == ' ' {
						val = val[1:]
					}
				}
				switch field {
				case "data":
					if haveData {
						data.WriteByte('\n')
					}
					data.Write(val)
					haveData = true
				case "event":
					evt.Type = string(val)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				// A final event without a trailing blank line still counts.
				if haveData && !bytes.HasPrefix(data.Bytes(), []byte("[DONE]")) {
					s.sawEvent = true
					return Event{Type: evt.Type, Data: append([]byte(nil), data.Bytes()...)}, nil
				}
				return Event{}, io.EOF
			}
			return Event{}, err
		}
	}
}

// SawEvent reports whether at least one event (or the [DONE] sentinel) was
// parsed — the discriminator for ErrNoEvents.
func (s *SSE) SawEvent() bool { return s.sawEvent || s.done }

// Done reports whether the [DONE] sentinel was seen.
func (s *SSE) Done() bool { return s.done }

// Close releases the connection.
func (s *SSE) Close() error { return s.body.Close() }

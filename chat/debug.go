package chat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const summaryColWidth = 30

// requestRows renders each captured round-trip as one styled list row — columns
// distinguished by STYLE, not separators (colors are washed out by the row
// selection highlight, so each column gets a distinct treatment): dim time,
// cyan action, UNDERLINED summary (the important bit), color-by-outcome status
// (green 2xx / red ≥400), dim duration. No raw method/URL — they carry no
// meaning to a reader scanning the log.
func requestRows(entries []*RequestEntry) []string {
	rows := make([]string, len(entries))
	for i, e := range entries {
		at := DimStyle.Sprint(e.Time.Format("15:04:05"))
		action := CodeStyle.Sprint(fmt.Sprintf("%-6s", actionFromURL(e.URL)))
		dur := ""
		if e.Duration > 0 {
			dur = DimStyle.Sprint(e.Duration.Round(time.Millisecond).String())
		}
		rows[i] = fmt.Sprintf("%s  %s  %s  %s  %s", at, action, summaryCol(e), statusCol(e), dur)
	}
	return rows
}

// summaryCol renders the underlined last-user-message summary, padded (with
// plain spaces, so the underline covers only the text) to a fixed visible width.
func summaryCol(e *RequestEntry) string {
	text := strings.Join(strings.Fields(lastUserText(e.ReqBody)), " ") // collapse whitespace
	if text == "" {
		text = "—" // e.g. a model listing carries no user text
		return DimStyle.Sprint(text) + strings.Repeat(" ", summaryColWidth-displayWidth(text))
	}
	text = truncateWidth(text, summaryColWidth)
	pad := summaryColWidth - displayWidth(text)
	if pad < 0 {
		pad = 0
	}
	return UnderlineStyle.Sprint(text) + strings.Repeat(" ", pad)
}

// statusCol renders the response status code colored by outcome: green for 2xx,
// red for a client/server error (≥400) or a transport failure, yellow for other
// codes, dim for a still-pending request.
func statusCol(e *RequestEntry) string {
	code, style := "…", DimStyle
	switch {
	case e.Err != "":
		code, style = "ERR", ErrorStyle
	case e.Status != "":
		if f := strings.Fields(e.Status); len(f) > 0 {
			code = f[0] // "200 OK" → "200"
		}
		if n, err := strconv.Atoi(code); err == nil {
			switch {
			case n >= 200 && n < 300:
				style = CodeBlockStyle // green
			case n >= 400:
				style = ErrorStyle // red
			default:
				style = YellowStyle
			}
		}
	}
	return style.Sprint(fmt.Sprintf("%-3s", code))
}

// truncateWidth clips s to at most w visible columns (CJK-aware), appending "…"
// (one column) when it cut.
func truncateWidth(s string, w int) string {
	if displayWidth(s) <= w {
		return s
	}
	col := 0
	var b strings.Builder
	for _, r := range s {
		rw := runeWidth(r)
		if col+rw > w-1 {
			break
		}
		b.WriteRune(r)
		col += rw
	}
	b.WriteString("…")
	return b.String()
}

// actionFromURL maps an API endpoint to a short action name, provider-agnostic.
// The Chat cases are checked before Models because Gemini's generateContent path
// also contains "/models".
func actionFromURL(rawURL string) string {
	path := rawURL
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		path = u.Path
	}
	lower := strings.ToLower(path)
	switch {
	case strings.Contains(lower, "chat/completions"),
		strings.Contains(lower, "/messages"),
		strings.Contains(lower, "/responses"),
		strings.Contains(lower, "generatecontent"):
		return "Chat"
	case strings.Contains(lower, "/models"):
		return "Models"
	}
	if segs := strings.Split(strings.Trim(path, "/"), "/"); len(segs) > 0 && segs[len(segs)-1] != "" {
		return segs[len(segs)-1]
	}
	return "Request"
}

// lastUserText digs the most recent user-authored text out of a request body,
// across the provider shapes: OpenAI/Anthropic `messages[]`, OpenAI Responses
// `input`, and Gemini `contents[].parts[].text`. Returns "" for bodyless or
// unrecognized requests (e.g. a model listing).
func lastUserText(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	if msgs, ok := m["messages"].([]any); ok {
		for i := len(msgs) - 1; i >= 0; i-- {
			mm, _ := msgs[i].(map[string]any)
			if mm == nil {
				continue
			}
			if role, _ := mm["role"].(string); role != "" && role != "user" {
				continue
			}
			if t := contentText(mm["content"]); t != "" {
				return t
			}
		}
	}
	if s, ok := m["input"].(string); ok {
		return s
	}
	if arr, ok := m["input"].([]any); ok {
		for i := len(arr) - 1; i >= 0; i-- {
			if mm, ok := arr[i].(map[string]any); ok {
				if t := contentText(mm["content"]); t != "" {
					return t
				}
			}
		}
	}
	if cs, ok := m["contents"].([]any); ok {
		for i := len(cs) - 1; i >= 0; i-- {
			if mm, ok := cs[i].(map[string]any); ok {
				if t := contentText(mm["parts"]); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

// contentText extracts text from a message "content" (or Gemini "parts") that is
// either a plain string or an array of parts carrying a "text" field.
func contentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if txt, ok := pm["text"].(string); ok && txt != "" {
					return txt
				}
			}
		}
	}
	return ""
}

func requestDetailLines(e *RequestEntry) []string {
	head := []string{
		DimStyle.Sprintf("%s %s", e.Method, e.URL),
		DimStyle.Sprint(e.Time.Format("2006-01-02 15:04:05")),
		"",
	}
	return append(head, prettyBodyLines(e.ReqBody)...)
}

func responseDetailLines(e *RequestEntry) []string {
	status := e.Status
	if e.Err != "" {
		status = "error: " + e.Err
	}
	if status == "" {
		status = "(pending)"
	}
	head := []string{DimStyle.Sprintf("%s   %s", status, e.Duration.Round(time.Millisecond)), ""}
	return append(head, prettyBodyLines(e.RespBody)...)
}

// prettyBodyLines pretty-prints a captured body: a single JSON document is
// indented; anything else (e.g. an SSE stream of data: lines) is shown verbatim.
func prettyBodyLines(body []byte) []string {
	if len(body) == 0 {
		return []string{DimStyle.Sprint("(empty)")}
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "", "  ") == nil {
		return strings.Split(strings.TrimRight(pretty.String(), "\n"), "\n")
	}
	return strings.Split(strings.TrimRight(string(body), "\n"), "\n")
}

package chat

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"chatchain/provider"
)

func TestLastRounds(t *testing.T) {
	u := func(s string) provider.Message { return provider.Message{Role: "user", Content: s} }
	a := func(s string) provider.Message { return provider.Message{Role: "assistant", Content: s} }
	sys := provider.Message{Role: "system", Content: "sys"}
	tool := provider.Message{Role: "tool", Content: "result"}

	tests := []struct {
		name    string
		history []provider.Message
		n       int
		want    []provider.Message
	}{
		{"empty", nil, 3, nil},
		{"zero-rounds", []provider.Message{u("q"), a("r")}, 0, nil},
		{
			"fewer-than-n-returns-all",
			[]provider.Message{u("q1"), a("r1"), u("q2"), a("r2")},
			3,
			[]provider.Message{u("q1"), a("r1"), u("q2"), a("r2")},
		},
		{
			"exactly-n",
			[]provider.Message{u("q1"), a("r1"), u("q2"), a("r2"), u("q3"), a("r3")},
			3,
			[]provider.Message{u("q1"), a("r1"), u("q2"), a("r2"), u("q3"), a("r3")},
		},
		{
			"more-than-n-keeps-last-n-starting-at-user",
			[]provider.Message{u("q1"), a("r1"), u("q2"), a("r2"), u("q3"), a("r3"), u("q4"), a("r4")},
			3,
			[]provider.Message{u("q2"), a("r2"), u("q3"), a("r3"), u("q4"), a("r4")},
		},
		{
			"system-excluded",
			[]provider.Message{sys, u("q1"), a("r1")},
			3,
			[]provider.Message{u("q1"), a("r1")},
		},
		{
			"trailing-tool-results-kept",
			[]provider.Message{u("q1"), a("r1"), u("q2"), {Role: "assistant", ToolCalls: []provider.ToolCall{{Name: "f"}}}, tool, tool},
			1,
			[]provider.Message{u("q2"), {Role: "assistant", ToolCalls: []provider.ToolCall{{Name: "f"}}}, tool, tool},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lastRounds(tt.history, tt.n); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("lastRounds(..., %d) = %#v, want %#v", tt.n, got, tt.want)
			}
		})
	}
}

func TestEchoRounds(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "first question", Attachments: []provider.Attachment{{Filename: "a.png"}, {Filename: "b.pdf"}}},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{Name: "bash"}}},
		{Role: "tool", Content: "output 1"},
		{Role: "tool", Content: "output 2"},
		{Role: "assistant", Content: "final answer", Reasoning: "secret thinking"},
		{Role: "user", Content: "second question"},
		{Role: "assistant", Content: "partial reply", Interrupted: true},
	}
	var buf bytes.Buffer
	echoRounds(&buf, msgs, "", nil)
	out := buf.String()

	for _, want := range []string{
		"first question",
		"📎 a.png",
		"📎 b.pdf",
		"◇ ran 2 tools",
		"final answer",
		"second question",
		"partial reply",
		"(interrupted)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("echoRounds output missing %q\noutput:\n%s", want, out)
		}
	}
	// Tool bodies and reasoning must not be replayed.
	for _, forbidden := range []string{"output 1", "output 2", "secret thinking"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("echoRounds output leaked %q\noutput:\n%s", forbidden, out)
		}
	}
	// The tool line must precede the round's final reply.
	if strings.Index(out, "◇ ran 2 tools") > strings.Index(out, "final answer") {
		t.Errorf("tool call line should precede the round's reply\noutput:\n%s", out)
	}
}

func TestEchoRoundsTrailingToolResults(t *testing.T) {
	// A history ending in tool results (no trailing assistant reply) still gets
	// its aggregated tool line at the end of the replay.
	msgs := []provider.Message{
		{Role: "user", Content: "do the thing"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{Name: "f"}}},
		{Role: "tool", Content: "out"},
	}
	var buf bytes.Buffer
	echoRounds(&buf, msgs, "", nil)
	if !strings.Contains(buf.String(), "◇ ran 1 tool") {
		t.Errorf("expected trailing tool call line, got:\n%s", buf.String())
	}
}

// A replayed assistant image renders as half-blocks with a filename caption —
// not just the name (the pre-imagen placeholder); undecodable data falls back
// to the caption line alone.
func TestEchoRoundsRendersImages(t *testing.T) {
	var pngBuf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	echoRounds(&out, []provider.Message{
		{Role: "user", Content: "draw"},
		{Role: "assistant", Attachments: []provider.Attachment{
			{Filename: "image-1.png", MimeType: "image/png", Data: pngBuf.Bytes()}}},
	}, "", nil)
	s := out.String()
	if !strings.Contains(s, "▀") {
		t.Fatalf("no half-block rows in echo:\n%q", s)
	}
	if !strings.Contains(s, "🖼 image-1.png") {
		t.Fatalf("caption missing:\n%q", s)
	}

	out.Reset()
	echoRounds(&out, []provider.Message{
		{Role: "assistant", Attachments: []provider.Attachment{
			{Filename: "broken.png", MimeType: "image/png", Data: []byte{1, 2}}}},
	}, "", nil)
	if s := out.String(); strings.Contains(s, "▀") || !strings.Contains(s, "🖼 broken.png") {
		t.Fatalf("broken image must fall back to the caption line:\n%q", s)
	}
}

// With the session's images directory supplied and the file still on disk,
// the caption becomes the live turn's clickable path (OSC 8 file:// link);
// a missing file falls back to the bare filename.
func TestEchoImageLinkedCaption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "image-1.png")
	if err := os.WriteFile(path, []byte{9}, 0o644); err != nil {
		t.Fatal(err)
	}
	att := provider.Attachment{Filename: "image-1.png", MimeType: "image/png", Data: []byte{1}} // undecodable: caption only

	// markdown.Hyperlink degrades to the bare text without a TTY, so the
	// pinned contract is the full path in the caption (the link target).
	var out bytes.Buffer
	echoImage(&out, att, 100, dir)
	if s := out.String(); !strings.Contains(s, path) {
		t.Fatalf("caption must carry the on-disk path:\n%q", s)
	}

	out.Reset()
	echoImage(&out, provider.Attachment{Filename: "gone.png", MimeType: "image/png", Data: []byte{1}}, 100, dir)
	if s := out.String(); strings.Contains(s, dir) || !strings.Contains(s, "gone.png") {
		t.Fatalf("missing file must fall back to the bare name:\n%q", s)
	}
}

// The /edit canvas dedup: a user attachment sharing its name with an
// assistant image in the echoed window is suppressed (that picture already
// rendered above); when the source round fell outside the window the marker
// shows — then it is the only trace of the reference.
func TestEchoRoundsCanvasDedup(t *testing.T) {
	img := provider.Attachment{Filename: "gen-1.png", MimeType: "image/png", Data: []byte{1}}

	var buf bytes.Buffer
	echoRounds(&buf, []provider.Message{
		{Role: "user", Content: "a cat"},
		{Role: "assistant", Attachments: []provider.Attachment{img}},
		{Role: "user", Content: "add a hat", Attachments: []provider.Attachment{img}}, // /edit turn
		{Role: "assistant", Attachments: []provider.Attachment{
			{Filename: "gen-2.png", MimeType: "image/png", Data: []byte{2}}}},
	}, "", nil)
	if s := buf.String(); strings.Contains(s, "📎") {
		t.Fatalf("canvas copy must be suppressed:\n%q", s)
	}

	// Same /edit turn, but the source round is outside the window.
	buf.Reset()
	echoRounds(&buf, []provider.Message{
		{Role: "user", Content: "add a hat", Attachments: []provider.Attachment{img}},
		{Role: "assistant", Attachments: []provider.Attachment{
			{Filename: "gen-2.png", MimeType: "image/png", Data: []byte{2}}}},
	}, "", nil)
	if s := buf.String(); !strings.Contains(s, "📎 gen-1.png") {
		t.Fatalf("cut-off canvas must keep its marker:\n%q", s)
	}
}

// Every row of a replayed multi-line message is a FULL-WIDTH block with the
// prompt gutter on the first row and an aligned indent on the rest: embedded
// newlines start rows, they are not runes to measure (a single "row" carrying
// its own line breaks reset the terminal to column 0 and left the background
// stopping at the text).
func TestPrintUserBlockMultiline(t *testing.T) {
	var out bytes.Buffer
	printUserBlock(&out, "first line\nsecond line\nthird")

	rows := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want one per line:\n%q", len(rows), out.String())
	}
	width := 0
	for i, row := range rows {
		plain := stripANSI(row)
		if strings.Contains(plain, "\r") {
			t.Fatalf("row %d carries a control char: %q", i, plain)
		}
		want := "  "
		if i == 0 {
			want = "❯ "
		}
		if !strings.HasPrefix(plain, want) {
			t.Fatalf("row %d = %q, want the %q gutter", i, plain, want)
		}
		w := displayWidth(plain)
		if i == 0 {
			width = w
		} else if w != width {
			t.Fatalf("row %d width = %d, want %d (padding must reach the full block width)", i, w, width)
		}
	}
}

// Interactive tool results (the ask set) replay as their "?" record block —
// the user's own answers — instead of folding into the tool count.
func TestEchoRoundsAskRecord(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "pick for me"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{Name: "choose"}, {Name: "bash"}}},
		{Role: "tool", ToolCallName: "choose", Content: "Auth: JWT\nLib: chi"},
		{Role: "tool", ToolCallName: "bash", Content: "out"},
		{Role: "assistant", Content: "done"},
	}
	var buf bytes.Buffer
	echoRounds(&buf, msgs, "", func(name string) bool { return name == "choose" })
	out := buf.String()

	for _, want := range []string{"? Auth: JWT", "  Lib: chi", "◇ ran 1 tool"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "2 tools") {
		t.Errorf("the ask result must not count as a tool:\n%s", out)
	}
}

// The replayed "?" record uses the live block's exact renderer: an errored
// interactive result keeps its error emphasis and an empty one falls back to
// "(no answer)" instead of a blank line.
func TestEchoRoundsAskRecordParity(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "pick"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{Name: "choose"}, {Name: "confirm"}}},
		{Role: "tool", ToolCallName: "choose", Content: "boom", IsError: true},
		{Role: "tool", ToolCallName: "confirm", Content: "  \n"},
	}
	var buf bytes.Buffer
	echoRounds(&buf, msgs, "", func(string) bool { return true })
	out := buf.String()

	if want := askRecordLines("boom", true)[0]; !strings.Contains(out, want) {
		t.Errorf("errored ask must replay with error styling (%q), got:\n%s", want, out)
	}
	if !strings.Contains(out, "? (no answer)") {
		t.Errorf("empty ask must replay as \"(no answer)\", got:\n%s", out)
	}
}

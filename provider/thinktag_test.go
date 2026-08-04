package provider

import (
	"strings"
	"testing"
)

func feedSplitter(deltas []string) (content, think string) {
	var c, th strings.Builder
	sp := &thinkTagSplitter{
		think:   func(s string) { th.WriteString(s) },
		content: func(s string) { c.WriteString(s) },
	}
	for _, d := range deltas {
		sp.write(d)
	}
	sp.flush()
	return c.String(), th.String()
}

func TestThinkTagSplitter(t *testing.T) {
	cases := []struct {
		name        string
		deltas      []string
		wantContent string
		wantThink   string
	}{
		{"no tags", []string{"hello ", "world"}, "hello world", ""},
		{"single delta", []string{"<think>reason</think>answer"}, "answer", "reason"},
		{"tags split across deltas", []string{"<th", "ink>rea", "son</th", "ink>ans", "wer"}, "answer", "reason"},
		{"leading whitespace before tag", []string{"\n <think>r</think>c"}, "c", "r"},
		{"unclosed block is all reasoning", []string{"<think>only thoughts"}, "", "only thoughts"},
		{"dangling close prefix stays think", []string{"<think>x</thi"}, "", "x</thi"},
		{"template padding after close trimmed", []string{"<think>r</think>", "\n\n", "answer"}, "answer", "r"},
		{"literal tag mid-content passes through", []string{"see the <think> tag"}, "see the <think> tag", ""},
		{"close without open passes through", []string{"plain</think>text"}, "plain</think>text", ""},
		{"open prefix that mismatches flushes raw", []string{"<th", "ing else"}, "<thing else", ""},
		{"second block after close passes through", []string{"<think>r</think>a<think>b</think>"}, "a<think>b</think>", "r"},
		{"whitespace-only stream", []string{"  "}, "  ", ""},
		{"empty stream", nil, "", ""},
		{"partial open at EOF flushes raw", []string{"<thi"}, "<thi", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content, think := feedSplitter(tc.deltas)
			if content != tc.wantContent || think != tc.wantThink {
				t.Fatalf("content=%q think=%q, want %q / %q", content, think, tc.wantContent, tc.wantThink)
			}
		})
	}
}

// Byte-at-a-time chunking must resolve identically to whole-string feeding.
func TestThinkTagSplitterByteAtATime(t *testing.T) {
	in := "\n<think>deep\nthought</think>\n\nThe <think> tag explained."
	var deltas []string
	for i := 0; i < len(in); i++ {
		deltas = append(deltas, in[i:i+1])
	}
	content, think := feedSplitter(deltas)
	if think != "deep\nthought" {
		t.Fatalf("think = %q", think)
	}
	if content != "The <think> tag explained." {
		t.Fatalf("content = %q", content)
	}
}

func TestSplitInlineThink(t *testing.T) {
	c, th := splitInlineThink("<think>pondering</think>done")
	if c != "done" || th != "pondering" {
		t.Fatalf("got %q / %q", c, th)
	}
	c, th = splitInlineThink("no tags here")
	if c != "no tags here" || th != "" {
		t.Fatalf("got %q / %q", c, th)
	}
}

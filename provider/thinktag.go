package provider

import (
	"fmt"
	"io"
	"strings"
)

const (
	thinkOpenTag  = "<think>"
	thinkCloseTag = "</think>"
)

// thinkTagSplitter splits a streamed content sequence that may open with an
// inline <think>…</think> block. Reasoning models behind relays that don't
// parse thinking (vLLM without a reasoning parser, passthrough aggregators)
// leak the chat template's tags into plain content on any text dialect; this
// routes that block to the reasoning channel so the rest of the pipeline
// sees the same shape as a parsed reasoning stream.
//
// Only a block that opens the stream (after optional whitespace) counts as
// reasoning — once the opening bytes mismatch, the splitter passes
// everything through verbatim for the rest of the round, so replies that
// merely discuss the tag are safe. A reply whose first bytes are the literal
// tag is byte-identical to a real leak and degrades to showing as reasoning.
// An unclosed block (round ended by tool calls, max_tokens, or cancel) is
// implicitly all reasoning: </think> marks the transition to visible
// content, not end-of-generation, and interleaved-thinking models
// legitimately end tool rounds mid-think.
//
// Tags may be split across deltas, so partial matches are held back until
// resolved. Feed each delta with write; call flush at end of stream.
type thinkTagSplitter struct {
	think   func(string) // receives reasoning-bound text
	content func(string) // receives visible-bound text

	started  bool   // left the sniffing state: the opening-tag question is settled
	inThink  bool   // inside the think block, routing to think until </think>
	held     string // sniff: raw prefix still matching <think>; think: suffix that prefixes </think>
	afterTag bool   // just closed the block: trim the template's padding once
}

func (t *thinkTagSplitter) write(s string) {
	if s == "" {
		return
	}
	switch {
	case !t.started:
		t.sniff(s)
	case t.inThink:
		t.feedThink(s)
	default:
		t.feedContent(s)
	}
}

// flush resolves any held partial match at end of stream.
func (t *thinkTagSplitter) flush() {
	held := t.held
	t.held = ""
	if held == "" {
		return
	}
	if t.inThink {
		// A dangling prefix of </think> that never completed is think text.
		t.think(held)
		return
	}
	// Stream ended while it still looked like an opening tag: it wasn't one.
	t.started = true
	t.content(held)
}

// sniff decides whether the stream opens with <think>, holding bytes until
// the question is settled either way.
func (t *thinkTagSplitter) sniff(s string) {
	t.held += s
	trimmed := strings.TrimLeft(t.held, " \t\r\n")
	if trimmed == "" {
		return // pure whitespace so far, keep waiting
	}
	if strings.HasPrefix(trimmed, thinkOpenTag) {
		rest := trimmed[len(thinkOpenTag):]
		t.held = ""
		t.started = true
		t.inThink = true
		if rest != "" {
			t.feedThink(rest)
		}
		return
	}
	if len(trimmed) < len(thinkOpenTag) && strings.HasPrefix(thinkOpenTag, trimmed) {
		return // still a viable tag prefix, keep holding
	}
	out := t.held
	t.held = ""
	t.started = true
	t.content(out)
}

// feedThink routes text to the think channel until </think> completes.
func (t *thinkTagSplitter) feedThink(s string) {
	s = t.held + s
	t.held = ""
	if i := strings.Index(s, thinkCloseTag); i >= 0 {
		if i > 0 {
			t.think(s[:i])
		}
		t.inThink = false
		t.afterTag = true
		if rest := s[i+len(thinkCloseTag):]; rest != "" {
			t.feedContent(rest)
		}
		return
	}
	if k := longestSuffixPrefix(s, thinkCloseTag); k > 0 {
		t.held = s[len(s)-k:]
		s = s[:len(s)-k]
	}
	if s != "" {
		t.think(s)
	}
}

func (t *thinkTagSplitter) feedContent(s string) {
	if t.afterTag {
		s = strings.TrimLeft(s, " \t\r\n")
		if s == "" {
			return // templates pad </think> with blank lines; swallow them
		}
		t.afterTag = false
	}
	t.content(s)
}

// longestSuffixPrefix reports the length of the longest suffix of s that is
// a proper prefix of tag (a partial tag possibly split across deltas).
func longestSuffixPrefix(s, tag string) int {
	max := len(tag) - 1
	if len(s) < max {
		max = len(s)
	}
	for k := max; k > 0; k-- {
		if s[len(s)-k:] == tag[:k] {
			return k
		}
	}
	return 0
}

// streamThinkSplitter adapts thinkTagSplitter to the provider streaming
// contract: think text feeds the reasoning writer, visible text closes
// reasoning (thinking ends at first content, matching the field and block
// dialect paths) and feeds the content writer. Both sides accumulate for
// the stream's return values — content holds the clean visible text, think
// the tag-extracted reasoning.
type streamThinkSplitter struct {
	sp      thinkTagSplitter
	content strings.Builder
	think   strings.Builder
}

func newStreamThinkSplitter(w io.Writer, reasoningW io.Writer, closeReasoning func()) *streamThinkSplitter {
	s := &streamThinkSplitter{}
	s.sp.think = func(t string) {
		fmt.Fprint(reasoningW, t)
		s.think.WriteString(t)
	}
	s.sp.content = func(c string) {
		closeReasoning()
		fmt.Fprint(w, c)
		s.content.WriteString(c)
	}
	return s
}

func (s *streamThinkSplitter) write(delta string) { s.sp.write(delta) }
func (s *streamThinkSplitter) flush()             { s.sp.flush() }

// splitInlineThink partitions a complete (non-streamed) response into
// visible content and inline-tagged reasoning.
func splitInlineThink(s string) (content, think string) {
	var c, th strings.Builder
	sp := &thinkTagSplitter{
		think:   func(s string) { th.WriteString(s) },
		content: func(s string) { c.WriteString(s) },
	}
	sp.write(s)
	sp.flush()
	return c.String(), th.String()
}

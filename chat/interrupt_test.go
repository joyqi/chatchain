package chat

import (
	"testing"

	"github.com/joyqi/iota/provider"
)

// TestFinalizeInterrupt covers the three-state persistence table from
// docs/design/interrupt.md plus the reasoning-only case (reasoning without
// content counts as "no text").
func TestFinalizeInterrupt(t *testing.T) {
	system := provider.Message{Role: "system", Content: "sys"}
	user := provider.Message{Role: "user", Content: "question"}
	toolUse := provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c1", Name: "t"}}, RawContent: "raw"}
	toolResult := provider.Message{Role: "tool", Content: "out", ToolCallID: "c1", ToolCallName: "t"}

	t.Run("partial text is kept as an interrupted assistant message", func(t *testing.T) {
		history := []provider.Message{system, user}
		got, persist, _ := finalizeInterrupt(history, 1, "partial answer", "partial thinking")
		if !persist {
			t.Fatal("expected persist=true when partial text exists")
		}
		if len(got) != 3 {
			t.Fatalf("history length: got %d want 3", len(got))
		}
		last := got[2]
		if last.Role != "assistant" || last.Content != "partial answer" || last.Reasoning != "partial thinking" {
			t.Errorf("unexpected assistant message: %+v", last)
		}
		if !last.Interrupted {
			t.Error("assistant message not marked Interrupted")
		}
		if last.RawContent != nil {
			t.Errorf("interrupted assistant message must carry no raw content, got %#v", last.RawContent)
		}
	})

	t.Run("no text and no tool rounds drops the whole turn", func(t *testing.T) {
		history := []provider.Message{system, user}
		got, persist, _ := finalizeInterrupt(history, 1, "", "")
		if persist {
			t.Fatal("expected persist=false when the turn yielded nothing")
		}
		if len(got) != 1 || got[0].Role != "system" {
			t.Fatalf("expected turn rolled back to watermark, got %+v", got)
		}
	})

	t.Run("no text but completed tool rounds are kept as-is", func(t *testing.T) {
		history := []provider.Message{system, user, toolUse, toolResult}
		got, persist, _ := finalizeInterrupt(history, 1, "", "")
		if !persist {
			t.Fatal("expected persist=true when tool rounds completed")
		}
		if len(got) != 4 {
			t.Fatalf("history length: got %d want 4", len(got))
		}
		if last := got[len(got)-1]; last.Role != "tool" {
			t.Errorf("expected no trailing assistant message, got role %q", last.Role)
		}
	})

	t.Run("reasoning-only partial counts as no text", func(t *testing.T) {
		history := []provider.Message{system, user}
		got, persist, _ := finalizeInterrupt(history, 1, "", "only thinking so far")
		if persist {
			t.Fatal("expected persist=false for a reasoning-only partial")
		}
		if len(got) != 1 {
			t.Fatalf("expected turn rolled back, got %+v", got)
		}
	})

	t.Run("partial text after tool rounds appends the assistant message", func(t *testing.T) {
		history := []provider.Message{user, toolUse, toolResult}
		got, persist, _ := finalizeInterrupt(history, 0, "final partial", "")
		if !persist {
			t.Fatal("expected persist=true")
		}
		if len(got) != 4 {
			t.Fatalf("history length: got %d want 4", len(got))
		}
		if last := got[3]; !last.Interrupted || last.Content != "final partial" {
			t.Errorf("unexpected trailing message: %+v", last)
		}
	})
}

// Cancelling a send must not strip the message's attachments: the discarded
// turn hands them back so the next message still carries the /edit canvas
// (image turns ALWAYS take this path — they produce no text).
func TestFinalizeInterruptReturnsDroppedAttachments(t *testing.T) {
	canvas := provider.Attachment{Filename: "gen-1.png", MimeType: "image/png", Data: []byte{1}}
	history := []provider.Message{
		{Role: "user", Content: "a cat"},
		{Role: "assistant", Attachments: []provider.Attachment{canvas}},
		{Role: "user", Content: "add a hat", Attachments: []provider.Attachment{canvas}},
	}

	got, persist, dropped := finalizeInterrupt(history, 2, "", "")
	if len(got) != 2 || persist {
		t.Fatalf("discard path broken: len=%d persist=%v", len(got), persist)
	}
	if len(dropped) != 1 || dropped[0].Filename != "gen-1.png" {
		t.Fatalf("dropped = %+v, want the canvas back", dropped)
	}

	// A turn that survives keeps its attachments in history — nothing to hand
	// back (they already reached the model).
	if _, _, d := finalizeInterrupt(history, 2, "partial text", ""); d != nil {
		t.Fatalf("kept turn must not return attachments: %+v", d)
	}
	// Completed tool rounds keep the turn too.
	withTool := append(history, provider.Message{Role: "tool", Content: "out"})
	if _, _, d := finalizeInterrupt(withTool, 2, "", ""); d != nil {
		t.Fatalf("tool-round turn must not return attachments: %+v", d)
	}
	// A watermark at the end (nothing composed yet) is not a crash.
	if _, _, d := finalizeInterrupt(history, len(history), "", ""); d != nil {
		t.Fatalf("empty turn = %+v", d)
	}
}

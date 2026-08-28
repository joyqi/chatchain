package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/joyqi/iota/provider"
)

// gatedDispatch owns one tool that always needs approval, and records whether
// it was ever actually executed.
type gatedDispatch struct{ ran int }

func (d *gatedDispatch) Tools() []provider.ToolDef {
	return []provider.ToolDef{{Name: "write_file"}}
}
func (d *gatedDispatch) RequiresApproval(string) bool { return true }
func (d *gatedDispatch) CallTool(context.Context, string, map[string]any) (string, bool, error) {
	d.ran++
	return "written", false, nil
}

// writingProvider asks for the gated tool once, then answers.
type writingProvider struct{ calls int }

func (p *writingProvider) StreamChatWithTools(ctx context.Context, msgs []provider.Message, tools []provider.ToolDef, w io.Writer, reasoning io.WriteCloser) (string, string, []provider.ToolCall, error) {
	reasoning.Close()
	p.calls++
	if p.calls == 1 {
		return "", "", []provider.ToolCall{{ID: "c1", Name: "write_file", Arguments: map[string]any{}}}, nil
	}
	// The result of the gated call is the last thing in history; echo it so
	// the test can assert on what the model was actually told.
	last := msgs[len(msgs)-1]
	return "saw: " + last.Content, "", nil, nil
}

func runGated(t *testing.T, host quietHost) (string, *gatedDispatch, error) {
	t.Helper()
	d := &gatedDispatch{}
	history := []provider.Message{{Role: "user", Content: "go"}}
	reply, _, err := executeWithTools(context.Background(), &writingProvider{}, d,
		&history, d.Tools(), "", 0, host)
	return reply, d, err
}

// With nobody to ask, the loop refuses and says how to enable the call. That
// is the -m contract and it must survive the seam being added.
func TestQuietLoopRefusesWhenThereIsNobodyToAsk(t *testing.T) {
	reply, d, err := runGated(t, quietHost{rec: newRunRecorder()})
	if err != nil {
		t.Fatal(err)
	}
	if d.ran != 0 {
		t.Error("a gated tool ran with no approval")
	}
	if !strings.Contains(reply, "auto_write") {
		t.Errorf("the refusal must name the way to enable it, got %q", reply)
	}
}

// A delegated child has no user of its own but runs inside a parent that does,
// so its question travels up and the answer decides the call.
func TestQuietLoopForwardsApproval(t *testing.T) {
	var asked []string
	host := quietHost{rec: newRunRecorder(),
		approve: func(_ context.Context, tc provider.ToolCall, _ string) (bool, string) {
			asked = append(asked, tc.Name)
			return true, ""
		}}
	reply, d, err := runGated(t, host)
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != "write_file" {
		t.Errorf("gate saw %v, want one write_file", asked)
	}
	if d.ran != 1 {
		t.Errorf("tool ran %d times, want 1", d.ran)
	}
	if !strings.Contains(reply, "written") {
		t.Errorf("the model should have seen the result, got %q", reply)
	}
}

// A denial is a result the model reads, not an aborted turn: the child carries
// on and reports back, which is what the parent's terminal showed.
func TestQuietLoopDenialContinuesTheRun(t *testing.T) {
	host := quietHost{rec: newRunRecorder(),
		approve: func(_ context.Context, tc provider.ToolCall, _ string) (bool, string) {
			return false, "The user declined this call."
		}}
	reply, d, err := runGated(t, host)
	if err != nil {
		t.Fatal(err)
	}
	if d.ran != 0 {
		t.Error("a denied tool ran anyway")
	}
	if !strings.Contains(reply, "declined") {
		t.Errorf("the model must be told it was declined, got %q", reply)
	}
}

// The refusal text is the call's result either way, so a failing prompt must
// not be mistaken for consent.
func TestQuietHostAskApprovalShapes(t *testing.T) {
	var h quietHost
	ok, why := h.askApproval(context.Background(), provider.ToolCall{Name: "edit_file"}, "path:x")
	if ok || !strings.Contains(why, "edit_file") {
		t.Errorf("nil approver = (%v, %q)", ok, why)
	}
	h.approve = func(_ context.Context, tc provider.ToolCall, _ string) (bool, string) {
		return false, fmt.Sprintf("%v", errors.New("prompt broke"))
	}
	if ok, why := h.askApproval(context.Background(), provider.ToolCall{Name: "edit_file"}, "path:x"); ok || why != "prompt broke" {
		t.Errorf("failed prompt = (%v, %q), want a refusal carrying the reason", ok, why)
	}
}

// detailDispatch names the file its gated tool would write, the way the code
// set's headliner does.
type detailDispatch struct{ gatedDispatch }

func (detailDispatch) HeaderSummary(_ string, args map[string]any) (string, bool) {
	p, _ := args["path"].(string)
	return p, true
}

// The prompt has to say what the call is ABOUT. For a delegated call nothing
// else on screen does: the widget above describes the delegation, not the
// operation the child is asking to perform, so a gate naming only the tool
// asks the user to authorize "edit_file" without saying which file.
func TestForwardedApprovalCarriesTheCallDetail(t *testing.T) {
	var seen []string
	host := quietHost{rec: newRunRecorder(),
		approve: func(_ context.Context, _ provider.ToolCall, detail string) (bool, string) {
			seen = append(seen, detail)
			return false, "The user declined this call."
		}}
	d := &detailDispatch{}
	tp := &pathWritingProvider{}
	history := []provider.Message{{Role: "user", Content: "go"}}
	if _, _, err := executeWithTools(context.Background(), tp, d, &history,
		d.Tools(), "", 0, host); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "internal/ui/model.go" {
		t.Errorf("gate saw detail %v, want the path the call would write", seen)
	}
}

type pathWritingProvider struct{ calls int }

func (p *pathWritingProvider) StreamChatWithTools(ctx context.Context, msgs []provider.Message, tools []provider.ToolDef, w io.Writer, reasoning io.WriteCloser) (string, string, []provider.ToolCall, error) {
	reasoning.Close()
	p.calls++
	if p.calls == 1 {
		return "", "", []provider.ToolCall{{ID: "c1", Name: "write_file",
			Arguments: map[string]any{"path": "internal/ui/model.go"}}}, nil
	}
	return "done", "", nil, nil
}

// The header keeps its shape after the detail was split out of it — one
// implementation, two readers.
func TestToolCallHeaderUnchangedBySplit(t *testing.T) {
	d := &detailDispatch{}
	tc := provider.ToolCall{Name: "write_file", Arguments: map[string]any{"path": "a/b.go"}}
	if got := toolCallHeader(d, tc); got != "[write_file a/b.go]" {
		t.Errorf("header = %q", got)
	}
	// A tool with no summary of its own falls back to the argument digest,
	// and an empty summary renders as a bare name rather than the digest.
	plain := &gatedDispatch{}
	if got := toolCallHeader(plain, tc); got != "[write_file path:a/b.go]" {
		t.Errorf("digest header = %q", got)
	}
	if got := toolCallHeader(plain, provider.ToolCall{Name: "x"}); got != "[x]" {
		t.Errorf("bare header = %q", got)
	}
}

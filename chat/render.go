package chat

import (
	"sync"

	"chatchain/internal/ui"
)

// turnRender is one turn's block-spacing arbiter. A turn's output is a
// sequence of logical blocks — reasoning markers, markdown content, tool-call
// widgets — and the spacing rule is uniform: every block is preceded by
// exactly one blank row (above the first block sits the user's input line).
// Renderers declare block boundaries here instead of committing ad-hoc ""
// separators; crucially the separator lands when a block OPENS, so the
// staging view (a spinning tool-call widget) is spaced exactly like the
// settled scrollback it morphs into — nothing shifts at write-back.
type turnRender struct {
	sink ui.StreamSink

	mu       sync.Mutex
	callOpen bool // the tool-call widget is raised (separator already paid)
}

func newTurnRender(sink ui.StreamSink) *turnRender { return &turnRender{sink: sink} }

// openBlock starts a plain block (reasoning marker, markdown content).
func (r *turnRender) openBlock() {
	r.sink.CommitLines("")
}

// openCall raises the tool-call lifecycle widget, or expands its header in
// place when it is already up (the clock keeps running). The separator is
// committed on the first raise only — composing and the later full-header
// expansion are one block.
func (r *turnRender) openCall(label string) {
	r.mu.Lock()
	if !r.callOpen {
		r.callOpen = true
		r.sink.CommitLines("")
	}
	r.mu.Unlock()
	r.sink.CallPreview(label)
}

// settleCall morphs the widget into its final collapsed header: the header
// commit replaces the spinner row in place (the elapsed row's placeholder is
// consumed by the result lines that follow, which belong to the same block —
// no separator between header and result).
func (r *turnRender) settleCall(header string) {
	r.mu.Lock()
	r.callOpen = false
	r.mu.Unlock()
	r.sink.ClosePreview()
	r.sink.CommitLines(header)
}

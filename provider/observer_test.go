package provider

import (
	"strings"
	"testing"
)

// The tool-call stream observer: nil-safe before installation, delivers
// deltas in order, and detaches cleanly.
func TestToolCallObserver(t *testing.T) {
	var got []string
	b := &baseProvider{}
	b.notifyToolDelta("x", "dropped") // no observer installed: no panic

	b.SetToolCallObserver(func(name, delta string) { got = append(got, name+":"+delta) })
	b.notifyToolDelta("write_file", `{"pa`)
	b.notifyToolDelta("write_file", `th":`)

	b.SetToolCallObserver(nil)
	b.notifyToolDelta("write_file", "after-clear")

	if strings.Join(got, "|") != `write_file:{"pa|write_file:th":` {
		t.Fatalf("observer deliveries = %v", got)
	}
}

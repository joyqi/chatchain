package tool

import (
	"context"
	"testing"

	"github.com/joyqi/iota/provider"
)

// A tool may run concurrently only if it needs nothing that concurrency
// would break: no writes, no approval prompt, no interactive surface. This
// test is the guard on that invariant — the failure it prevents (two writers
// racing on a file, two modal prompts sharing one screen) is intermittent and
// invisible in a transcript, so it must be caught here rather than in use.
func TestOnlySafeToolsOptIntoParallel(t *testing.T) {
	cs := &codeSet{root: "/proj", cwd: "/proj"}
	all := map[string]Tool{
		"glob":       &codeGlob{cs},
		"grep":       &codeGrep{cs},
		"list_dir":   &codeListDir{cs},
		"read_file":  &codeReadFile{cs},
		"edit_file":  &codeEditFile{cs},
		"write_file": &codeWriteFile{cs},
		"bash":       &bashTool{root: "/proj", cwd: "/proj"},
	}
	r := &Registry{index: all}

	wantParallel := map[string]bool{
		"glob": true, "grep": true, "list_dir": true, "read_file": true,
	}
	for name, tl := range all {
		// These four answer the same for every call, so the arguments are
		// irrelevant here — nil is the honest thing to pass.
		got := r.SupportsParallel(name, nil)
		if got != wantParallel[name] {
			t.Errorf("%s: SupportsParallel = %v, want %v", name, got, wantParallel[name])
		}
		if !got {
			continue
		}
		// Whatever opts in must also be harmless in the three ways that
		// matter, checked against the tool itself rather than a list.
		if a, ok := tl.(approver); ok && a.RequiresApproval() {
			t.Errorf("%s runs in parallel but gates on approval: two prompts, one screen", name)
		}
		if p, ok := tl.(presenter); ok {
			switch p.Presentation() {
			case PresentSurface:
				t.Errorf("%s runs in parallel but opens a surface", name)
			case PresentExpanded:
				t.Errorf("%s runs in parallel but expands a diff — it writes", name)
			}
		}
	}
}

// A dispatcher without the capability serializes everything. The MCP manager
// is the real case: its servers make no promise about concurrent calls.
func TestParallelDefaultsToNo(t *testing.T) {
	var r *Registry
	if r.SupportsParallel("read_file", nil) {
		t.Fatal("a nil registry claimed parallel support")
	}
	empty := &Registry{index: map[string]Tool{}}
	if empty.SupportsParallel("anything", nil) {
		t.Fatal("an unknown tool claimed parallel support")
	}
	// A tool that simply does not implement the interface.
	plain := &Registry{index: map[string]Tool{"x": &codeEditFile{&codeSet{}}}}
	if plain.SupportsParallel("x", nil) {
		t.Fatal("edit_file claimed parallel support")
	}
}

// perCallTool answers from a key into a fixed table — the delegation shape,
// where every call shares a name and differs only in which configured entry
// it selects. Nothing here interprets a free-form argument; that distinction
// is the whole reason the arguments are allowed to decide (see parallelizer).
type perCallTool struct{ safe map[string]bool }

func (perCallTool) Def() provider.ToolDef { return provider.ToolDef{Name: "delegate"} }
func (perCallTool) Call(context.Context, map[string]any) (string, bool, error) {
	return "", false, nil
}
func (p perCallTool) SupportsParallel(args map[string]any) bool {
	key, _ := args["agent"].(string)
	return p.safe[key]
}

// The capability is a property of the CALL. One answer per tool name would
// have to lie in one direction: serializing a read-only fan-out, or letting a
// write-capable call into a batch.
func TestSupportsParallelIsPerCall(t *testing.T) {
	r := &Registry{index: map[string]Tool{
		"delegate": perCallTool{safe: map[string]bool{"search": true, "implement": false}},
	}}
	if !r.SupportsParallel("delegate", map[string]any{"agent": "search"}) {
		t.Error("the read-only entry must be allowed to run concurrently")
	}
	if r.SupportsParallel("delegate", map[string]any{"agent": "implement"}) {
		t.Error("the write-capable entry must stay serialized")
	}
	// An entry that is not in the table at all, and a call with no argument:
	// unknown resolves to the safe answer, never to the permissive one.
	if r.SupportsParallel("delegate", map[string]any{"agent": "nonesuch"}) {
		t.Error("an unknown entry must default to serialized")
	}
	if r.SupportsParallel("delegate", nil) {
		t.Error("a call with no entry named must default to serialized")
	}
}

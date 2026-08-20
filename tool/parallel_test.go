package tool

import "testing"

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
		got := r.SupportsParallel(name)
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
	if r.SupportsParallel("read_file") {
		t.Fatal("a nil registry claimed parallel support")
	}
	empty := &Registry{index: map[string]Tool{}}
	if empty.SupportsParallel("anything") {
		t.Fatal("an unknown tool claimed parallel support")
	}
	// A tool that simply does not implement the interface.
	plain := &Registry{index: map[string]Tool{"x": &codeEditFile{&codeSet{}}}}
	if plain.SupportsParallel("x") {
		t.Fatal("edit_file claimed parallel support")
	}
}

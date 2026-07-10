// Package tool provides chatchain's built-in tools — internal tools the user
// enables through the config file (alongside, or instead of, MCP servers).
//
// Each built-in tool is registered in the factories table by name. A provider's
// `tools:` config maps a tool name to that tool's raw config; the tool decodes
// its own config (an empty value means defaults). The Registry aggregates the
// enabled tools behind the Dispatcher surface, and Merge combines several
// dispatchers (e.g. built-ins + an MCP manager) into one.
package tool

import (
	"context"
	"fmt"

	"chatchain/provider"

	"gopkg.in/yaml.v3"
)

// Dispatcher is the surface the chat loop uses to advertise tools to the model
// and execute the model's tool calls. Both *Registry and the MCP manager satisfy
// it; Merge combines several into one.
type Dispatcher interface {
	// Tools returns the tool definitions to advertise to the provider.
	Tools() []provider.ToolDef
	// CallTool executes a tool call. It returns the result text, whether the
	// call is an error (surfaced to the model), and a hard error (transport-level).
	CallTool(ctx context.Context, name string, arguments map[string]any) (string, bool, error)
}

// Tool is a single built-in tool.
type Tool interface {
	Def() provider.ToolDef
	Call(ctx context.Context, args map[string]any) (text string, isError bool, err error)
}

// Factory builds a tool from its raw YAML config node. A zero/null node means
// "use defaults" — every factory must succeed on an empty node.
type Factory func(node yaml.Node) (Tool, error)

// factories is the central registry of built-in tools. Adding a tool = add a
// file with its Factory and one line here.
var factories = map[string]Factory{
	"run_command": newRunCommand,
	"read_file":   newReadFile,
}

// Registry holds the enabled built-in tools and satisfies Dispatcher.
type Registry struct {
	order []Tool
	index map[string]Tool
}

// Build constructs the enabled built-in tools from the per-tool raw config: a
// key's presence enables that tool. Unknown tool names and construction errors
// are reported via warnf (nil to suppress) and skipped — a bad entry never
// aborts startup (mirrors MCP's graceful degradation).
func Build(raw map[string]yaml.Node, warnf func(string, ...any)) *Registry {
	r := &Registry{index: make(map[string]Tool)}
	for name, node := range raw {
		f, ok := factories[name]
		if !ok {
			if warnf != nil {
				warnf("unknown built-in tool %q (ignored)", name)
			}
			continue
		}
		t, err := f(node)
		if err != nil {
			if warnf != nil {
				warnf("tool %q: %v (ignored)", name, err)
			}
			continue
		}
		def := t.Def()
		if _, dup := r.index[def.Name]; dup {
			continue
		}
		r.order = append(r.order, t)
		r.index[def.Name] = t
	}
	return r
}

// Enable registers the named built-in tool with its default (empty) config
// unless the config already enabled it. Agent mode uses it to auto-register
// read_file without requiring a `tools:` entry. Failures are reported via
// warnf (nil to suppress) and skipped, like Build.
func (r *Registry) Enable(name string, warnf func(string, ...any)) {
	if _, ok := r.index[name]; ok {
		return
	}
	f, ok := factories[name]
	if !ok {
		if warnf != nil {
			warnf("unknown built-in tool %q (ignored)", name)
		}
		return
	}
	t, err := f(yaml.Node{})
	if err != nil {
		if warnf != nil {
			warnf("tool %q: %v (ignored)", name, err)
		}
		return
	}
	r.order = append(r.order, t)
	r.index[t.Def().Name] = t
}

// Tools returns the definitions of the enabled built-in tools.
func (r *Registry) Tools() []provider.ToolDef {
	if r == nil {
		return nil
	}
	out := make([]provider.ToolDef, 0, len(r.order))
	for _, t := range r.order {
		out = append(out, t.Def())
	}
	return out
}

// CallTool dispatches a call to the matching built-in tool.
func (r *Registry) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if r == nil {
		return "", true, fmt.Errorf("unknown tool: %s", name)
	}
	t, ok := r.index[name]
	if !ok {
		return "", true, fmt.Errorf("unknown tool: %s", name)
	}
	return t.Call(ctx, args)
}

// Merge combines several dispatchers behind one surface. Nil parts (including a
// typed-nil whose Tools() is nil-safe) contribute nothing. On a tool-name
// collision the earlier dispatcher wins, so pass built-ins before MCP to let
// them take precedence. The result is always non-nil.
//
// The merged view is LIVE, not a snapshot: Tools() and CallTool re-query each
// part on every call, so a part whose tool set grows after Merge (e.g. an MCP
// manager still connecting servers in the background) is reflected immediately.
func Merge(parts ...Dispatcher) Dispatcher {
	md := &multiDispatcher{}
	for _, p := range parts {
		if p != nil {
			md.parts = append(md.parts, p)
		}
	}
	return md
}

// multiDispatcher routes each tool call to whichever merged part owns the tool
// name, re-querying parts live so late-arriving tools appear without a rebuild.
type multiDispatcher struct {
	parts []Dispatcher
}

func (m *multiDispatcher) Tools() []provider.ToolDef {
	var tools []provider.ToolDef
	seen := make(map[string]bool)
	for _, p := range m.parts {
		for _, def := range p.Tools() {
			if seen[def.Name] {
				continue // earlier part wins the name
			}
			seen[def.Name] = true
			tools = append(tools, def)
		}
	}
	return tools
}

func (m *multiDispatcher) CallTool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	for _, p := range m.parts {
		for _, def := range p.Tools() {
			if def.Name == name {
				return p.CallTool(ctx, name, args) // first (earliest) owner
			}
		}
	}
	return "", true, fmt.Errorf("unknown tool: %s", name)
}

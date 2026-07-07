package chat

import "strings"

// mcpWirePrefix mirrors the MCP manager's wire-name prefix
// (mcp.ComposeWireName): MCP tools are advertised to models as
// "mcp__<server>__<tool>".
const mcpWirePrefix = "mcp__"

// displayToolName converts a wire tool name to its user-facing form:
// "mcp__<server>__<tool>" becomes "<server>:<tool>". The server segment can
// never contain "__" (mcp.sanitizeNameSegment collapses underscore runs and
// trims the ends), so the first "__" after the "mcp__" prefix is always the
// server/tool separator; everything after it is the tool part, preserved
// verbatim. Both segments shown are the sanitized wire forms (underscores
// instead of e.g. hyphens, plus a hash suffix on lossy tool names), which is
// acceptable for display. Any other name — built-in tools like run_command,
// raw names replayed from pre-namespacing sessions, or a degenerate wire name
// with an empty segment — is returned unchanged.
func displayToolName(name string) string {
	rest, ok := strings.CutPrefix(name, mcpWirePrefix)
	if !ok {
		return name
	}
	server, tool, ok := strings.Cut(rest, "__")
	if !ok || server == "" || tool == "" {
		return name
	}
	return server + ":" + tool
}

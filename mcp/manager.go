package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"chatchain/provider"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerConfig describes how to connect to an MCP server.
type ServerConfig struct {
	Name    string
	Command string
	Args    []string
	URL     string
	Env     map[string]string
	Headers map[string]string
}

// ServerStatus holds runtime info about a connected MCP server.
type ServerStatus struct {
	Name      string   // display name
	Segment   string   // manager-assigned wire-name segment (unique across connected servers; empty when not Connected)
	Endpoint  string   // command or URL
	Connected bool     // whether connection succeeded
	ToolCount int      // number of tools from this server
	Tools     []string // raw tool names as reported by the server (not wire names)
	Err       string   // connection/tool-listing error (empty when Connected)
}

// toolTarget locates a registered tool: which session serves it and the raw
// (un-namespaced) name that server knows it by. The corresponding ToolDef
// carries the wire name (see ComposeWireName).
type toolTarget struct {
	session int    // index into Manager.sessions
	raw     string // server-side tool name
}

// Manager manages connections to MCP servers and dispatches tool calls.
type Manager struct {
	sessions  []*mcp.ClientSession
	tools     []provider.ToolDef
	toolIndex map[string]toolTarget // wire tool name → target
	segments  map[string]bool       // wire-name segments already assigned to servers
	servers   []ServerStatus        // per-server status
}

// wireNameMaxLen is the longest tool name accepted by every supported
// provider: OpenAI and Anthropic constrain tool names to [a-zA-Z0-9_-]{1,64},
// and Gemini enforces the same 64-char cap on function declaration names.
const wireNameMaxLen = 64

// wireNamePrefix marks a tool name as MCP-namespaced: "mcp__<server>__<tool>".
const wireNamePrefix = "mcp__"

// sanitizeNameSegment maps a name (server config key, --mcp flag value, or
// server-side tool name) to a wire-safe segment: every character outside
// [A-Za-z0-9_] becomes "_" (hyphens too — Gemini's functionDeclaration name
// pattern forbids them, even though OpenAI and Anthropic would accept them),
// runs of "_" collapse to a single "_", and leading/trailing "_" are trimmed.
// A name that sanitizes to nothing falls back to "srv". Collapse + trim
// guarantee a segment never contains "__", so in a composed wire name the
// first "__" after the "mcp__" prefix is always the server/tool separator.
func sanitizeNameSegment(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	prevUnderscore := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			prevUnderscore = false
		default:
			if prevUnderscore {
				continue
			}
			prevUnderscore = true
			r = '_'
		}
		b.WriteRune(r)
	}
	s := strings.Trim(b.String(), "_")
	if s == "" {
		return "srv"
	}
	return s
}

// ComposeWireName composes the name an MCP tool is advertised under:
// "mcp__<segment>__<tool>". segment must be an already-sanitized server
// segment (the manager assigns one per server, see addServer); only the tool
// part is sanitized here, so the result satisfies the strictest provider
// charset (Gemini forbids hyphens, which MCP servers commonly use in tool
// names). If composing is lossy — sanitizing changed the tool segment — or
// the name exceeds wireNameMaxLen, the name is trimmed to fit and suffixed
// with "_" plus an 8-char lowercase hex hash of the unsanitized composition,
// keeping distinct raw tool names distinct and within the limit. The function
// is pure, so recomposing from ServerStatus.Segment and a raw tool name
// always matches what was registered.
func ComposeWireName(segment, tool string) string {
	base := wireNamePrefix + segment + "__"
	wire := base + sanitizeNameSegment(tool)
	if wire == base+tool && len(wire) <= wireNameMaxLen {
		return wire
	}
	sum := sha256.Sum256([]byte(base + tool))
	suffix := "_" + hex.EncodeToString(sum[:4])
	if len(wire)+len(suffix) > wireNameMaxLen {
		wire = wire[:wireNameMaxLen-len(suffix)]
	}
	return wire + suffix
}

// WireToolName derives the wire name for a tool of a single, standalone
// server: sanitizeNameSegment(server) fed into ComposeWireName. It is for
// single-server contexts and tests; the Manager registers tools under its
// per-server assigned segments (unique across connected servers), so
// recomposing a managed server's wire names must use ServerStatus.Segment
// with ComposeWireName instead.
func WireToolName(server, tool string) string {
	return ComposeWireName(sanitizeNameSegment(server), tool)
}

// LogFunc is used for verbose logging without importing the chat package.
type LogFunc func(format string, args ...any)

// serverResult is one server's outcome from a concurrent connect attempt.
type serverResult struct {
	status  ServerStatus
	session *mcp.ClientSession
	tools   []provider.ToolDef
	logs    []string // buffered verbose lines, flushed in config order
}

// NewManager connects to all configured MCP servers concurrently and discovers
// their tools. Connection is graceful: a server that fails to connect or list
// tools is marked (ServerStatus.Connected=false, .Err set) and skipped — the
// remaining servers are still usable. logf is called for verbose output (per
// server, emitted in config order); pass nil to suppress.
func NewManager(ctx context.Context, configs []ServerConfig, logf LogFunc) (*Manager, error) {
	m := &Manager{
		toolIndex: make(map[string]toolTarget),
	}
	if len(configs) == 0 {
		return m, nil
	}

	// Connect concurrently. Each goroutine writes its own results[i] slot
	// (distinct indices → no shared-state race); we merge in config order
	// afterwards to keep tool ordering and toolIndex deterministic.
	results := make([]serverResult, len(configs))
	var wg sync.WaitGroup
	for i, raw := range configs {
		wg.Add(1)
		go func(i int, raw ServerConfig) {
			defer wg.Done()
			results[i] = connectServer(ctx, raw)
		}(i, raw)
	}
	wg.Wait()

	for _, r := range results {
		if logf != nil {
			for _, line := range r.logs {
				logf("%s", line)
			}
		}
		m.addServer(r, logf)
	}

	return m, nil
}

// assignSegment reserves a unique wire-name segment for a server: the
// sanitized server name, or — when another connected server already holds it
// (two servers with the same name, or names that sanitize identically, e.g.
// "my-server" and "my_server") — the first free "<segment>_2", "<segment>_3",
// … suffix, deterministic in connect (config) order.
func (m *Manager) assignSegment(name string) string {
	if m.segments == nil {
		m.segments = make(map[string]bool)
	}
	base := sanitizeNameSegment(name)
	seg := base
	for n := 2; m.segments[seg]; n++ {
		seg = fmt.Sprintf("%s_%d", base, n)
	}
	m.segments[seg] = true
	return seg
}

// addServer merges one server's connect outcome into the manager. A failed
// server only contributes its status; a connected one is assigned a unique
// wire-name segment (exposed as ServerStatus.Segment) and contributes its
// session and tools, each registered under its wire name (see
// ComposeWireName) mapped to the session index and the server's raw tool
// name. Unique segments that never contain "__", per-server tool uniqueness
// (MCP spec), and the hash suffix on lossy/truncated tool segments together
// make wire names unique, so the duplicate check below should be unreachable;
// it is a guardrail that skips the duplicate (never overwrites the earlier
// registration) and reports it through logf.
func (m *Manager) addServer(r serverResult, logf LogFunc) {
	if !r.status.Connected {
		m.servers = append(m.servers, r.status)
		return
	}
	idx := len(m.sessions)
	m.sessions = append(m.sessions, r.session)
	r.status.Segment = m.assignSegment(r.status.Name)
	for _, td := range r.tools {
		raw := td.Name
		td.Name = ComposeWireName(r.status.Segment, raw)
		if _, dup := m.toolIndex[td.Name]; dup {
			if logf != nil {
				logf("Warning: MCP server %s: duplicate wire tool name %s, skipping\n", r.status.Name, td.Name)
			}
			continue
		}
		m.tools = append(m.tools, td)
		m.toolIndex[td.Name] = toolTarget{session: idx, raw: raw}
	}
	m.servers = append(m.servers, r.status)
}

// connectServer connects to a single MCP server and lists its tools. It never
// returns an error: failures are captured in the returned status's Err field.
func connectServer(ctx context.Context, raw ServerConfig) serverResult {
	cfg := expandServerConfig(raw)
	endpoint := cfg.URL
	if endpoint == "" {
		endpoint = cfg.Command
		if len(cfg.Args) > 0 {
			endpoint += " " + strings.Join(cfg.Args, " ")
		}
	}
	res := serverResult{status: ServerStatus{Name: cfg.Name, Endpoint: endpoint}}
	res.logs = append(res.logs, fmt.Sprintf("Connecting to MCP server: %s\n", cfg.Name))

	transport, stderr, err := makeTransport(cfg)
	if err != nil {
		res.status.Err = err.Error()
		return res
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "chatchain", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		msg := fmt.Sprintf("connect failed: %v", err)
		if stderr != nil && stderr.Len() > 0 {
			msg += "\n  subprocess stderr:\n" + strings.TrimRight(stderr.String(), "\n")
		}
		res.status.Err = msg
		return res
	}

	res.session = session
	res.status.Connected = true

	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			// Tool listing failed: treat the whole server as unavailable.
			session.Close()
			return serverResult{
				status: ServerStatus{Name: cfg.Name, Endpoint: endpoint, Err: fmt.Sprintf("list tools: %v", err)},
				logs:   res.logs,
			}
		}

		var schema map[string]any
		if tool.InputSchema != nil {
			if s, ok := tool.InputSchema.(map[string]any); ok {
				schema = s
			}
		}
		res.tools = append(res.tools, provider.ToolDef{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: schema,
		})
		res.status.ToolCount++
		res.status.Tools = append(res.status.Tools, tool.Name)
		res.logs = append(res.logs, fmt.Sprintf("  Tool: %s — %s\n", tool.Name, tool.Description))
	}

	return res
}

// Servers returns status info for all configured MCP servers.
func (m *Manager) Servers() []ServerStatus {
	if m == nil {
		return nil
	}
	return m.servers
}

// Tools returns the aggregated list of tools from all connected servers.
// Each ToolDef.Name is the namespaced wire name ("mcp__<segment>__<tool>",
// see ComposeWireName), which is what gets advertised to models and must be
// passed back to CallTool.
func (m *Manager) Tools() []provider.ToolDef {
	if m == nil {
		return nil
	}
	return m.tools
}

// CallTool dispatches a tool call to the appropriate MCP server. name is the
// wire name the tool was advertised under (see ComposeWireName); it is
// translated back to the server's raw tool name for the actual call.
func (m *Manager) CallTool(ctx context.Context, name string, arguments map[string]any) (string, bool, error) {
	target, ok := m.toolIndex[name]
	if !ok {
		return "", true, fmt.Errorf("unknown tool: %s", name)
	}

	result, err := m.sessions[target.session].CallTool(ctx, &mcp.CallToolParams{
		Name:      target.raw,
		Arguments: arguments,
	})
	if err != nil {
		return "", true, err
	}

	// Extract text content from result
	var parts []string
	for _, c := range result.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, v.Text)
		}
	}

	return strings.Join(parts, "\n"), result.IsError, nil
}

// Close closes all MCP server connections.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	for _, s := range m.sessions {
		s.Close()
	}
}

// makeTransport returns the transport and an optional stderr buffer
// (non-nil for command transports) to surface subprocess errors on failure.
func makeTransport(cfg ServerConfig) (mcp.Transport, *bytes.Buffer, error) {
	if cfg.URL != "" {
		if strings.HasPrefix(cfg.URL, "http://") || strings.HasPrefix(cfg.URL, "https://") {
			transport := &mcp.StreamableClientTransport{
				Endpoint: cfg.URL,
			}
			if len(cfg.Headers) > 0 {
				transport.HTTPClient = &http.Client{
					Transport: &headerTransport{
						base:    http.DefaultTransport,
						headers: cfg.Headers,
					},
				}
			}
			return transport, nil, nil
		}
		return nil, nil, fmt.Errorf("unsupported URL scheme: %s", cfg.URL)
	}

	if cfg.Command != "" {
		args := cfg.Args
		cmd := exec.CommandContext(context.Background(), cfg.Command, args...)
		if len(cfg.Env) > 0 {
			cmd.Env = os.Environ()
			for k, v := range cfg.Env {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
		}
		stderr := &bytes.Buffer{}
		cmd.Stderr = stderr
		return &mcp.CommandTransport{Command: cmd}, stderr, nil
	}

	return nil, nil, fmt.Errorf("server config must have either command or url")
}

// headerTransport injects custom headers into every HTTP request.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	return t.base.RoundTrip(req)
}

// ParseMCPFlag parses a --mcp flag value into a ServerConfig.
// URLs (http:// or https://) become URL-based configs.
// Everything else is treated as a command (split on spaces).
func ParseMCPFlag(value string) ServerConfig {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return ServerConfig{
			Name: value,
			URL:  value,
		}
	}
	parts := strings.Fields(value)
	name := parts[0]
	var args []string
	if len(parts) > 1 {
		args = parts[1:]
	}
	return ServerConfig{
		Name:    name,
		Command: parts[0],
		Args:    args,
	}
}

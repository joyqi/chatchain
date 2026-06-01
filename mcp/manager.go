package mcp

import (
	"bytes"
	"context"
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
	Endpoint  string   // command or URL
	Connected bool     // whether connection succeeded
	ToolCount int      // number of tools from this server
	Tools     []string // tool names
	Err       string   // connection/tool-listing error (empty when Connected)
}

// Manager manages connections to MCP servers and dispatches tool calls.
type Manager struct {
	sessions  []*mcp.ClientSession
	tools     []provider.ToolDef
	toolIndex map[string]int // tool name → session index
	servers   []ServerStatus // per-server status
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
		toolIndex: make(map[string]int),
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
		if !r.status.Connected {
			m.servers = append(m.servers, r.status)
			continue
		}
		idx := len(m.sessions)
		m.sessions = append(m.sessions, r.session)
		for _, td := range r.tools {
			m.tools = append(m.tools, td)
			m.toolIndex[td.Name] = idx
		}
		m.servers = append(m.servers, r.status)
	}

	return m, nil
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
func (m *Manager) Tools() []provider.ToolDef {
	if m == nil {
		return nil
	}
	return m.tools
}

// CallTool dispatches a tool call to the appropriate MCP server.
func (m *Manager) CallTool(ctx context.Context, name string, arguments map[string]any) (string, bool, error) {
	idx, ok := m.toolIndex[name]
	if !ok {
		return "", true, fmt.Errorf("unknown tool: %s", name)
	}

	result, err := m.sessions[idx].CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
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

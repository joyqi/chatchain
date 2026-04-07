package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

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
}

// ServerStatus holds runtime info about a connected MCP server.
type ServerStatus struct {
	Name      string   // display name
	Endpoint  string   // command or URL
	Connected bool     // whether connection succeeded
	ToolCount int      // number of tools from this server
	Tools     []string // tool names
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

// NewManager connects to all configured MCP servers and discovers their tools.
// logf is called for verbose output; pass nil to suppress.
func NewManager(ctx context.Context, configs []ServerConfig, logf LogFunc) (*Manager, error) {
	m := &Manager{
		toolIndex: make(map[string]int),
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "chatchain",
		Version: "1.0.0",
	}, nil)

	for _, cfg := range configs {
		transport, err := makeTransport(cfg)
		if err != nil {
			return nil, fmt.Errorf("MCP server %q: %w", cfg.Name, err)
		}

		endpoint := cfg.URL
		if endpoint == "" {
			endpoint = cfg.Command
			if len(cfg.Args) > 0 {
				endpoint += " " + strings.Join(cfg.Args, " ")
			}
		}

		if logf != nil {
			logf("Connecting to MCP server: %s\n", cfg.Name)
		}

		session, err := client.Connect(ctx, transport, nil)
		if err != nil {
			return nil, fmt.Errorf("MCP server %q: connect failed: %w", cfg.Name, err)
		}

		idx := len(m.sessions)
		m.sessions = append(m.sessions, session)

		status := ServerStatus{
			Name:      cfg.Name,
			Endpoint:  endpoint,
			Connected: true,
		}

		for tool, err := range session.Tools(ctx, nil) {
			if err != nil {
				return nil, fmt.Errorf("MCP server %q: list tools: %w", cfg.Name, err)
			}

			// Convert InputSchema (any) to map[string]any
			var schema map[string]any
			if tool.InputSchema != nil {
				if s, ok := tool.InputSchema.(map[string]any); ok {
					schema = s
				}
			}

			td := provider.ToolDef{
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: schema,
			}
			m.tools = append(m.tools, td)
			m.toolIndex[tool.Name] = idx
			status.ToolCount++
			status.Tools = append(status.Tools, tool.Name)

			if logf != nil {
				logf("  Tool: %s — %s\n", tool.Name, tool.Description)
			}
		}

		m.servers = append(m.servers, status)
	}

	return m, nil
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

func makeTransport(cfg ServerConfig) (mcp.Transport, error) {
	if cfg.URL != "" {
		// URL-based: try streamable HTTP first (newer spec), fall back to SSE
		if strings.HasPrefix(cfg.URL, "http://") || strings.HasPrefix(cfg.URL, "https://") {
			return &mcp.StreamableClientTransport{
				Endpoint: cfg.URL,
			}, nil
		}
		return nil, fmt.Errorf("unsupported URL scheme: %s", cfg.URL)
	}

	if cfg.Command != "" {
		args := cfg.Args
		cmd := exec.CommandContext(context.Background(), cfg.Command, args...)
		// Set env vars if configured
		if len(cfg.Env) > 0 {
			cmd.Env = os.Environ()
			for k, v := range cfg.Env {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
		}
		return &mcp.CommandTransport{Command: cmd}, nil
	}

	return nil, fmt.Errorf("server config must have either command or url")
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

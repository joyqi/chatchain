package mcp

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"chatchain/provider"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSanitizeNameSegment(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"github", "github"},
		{"chrome-devtools", "chrome_devtools"},
		{"my_server_2", "my_server_2"},
		// Runs of "_" collapse and the ends are trimmed: a segment can never
		// contain "__", so the first "__" after "mcp__" always separates
		// server from tool.
		{"https://mcp.example.com/sse", "https_mcp_example_com_sse"},
		{"npx -y server", "npx_y_server"},
		{"a__b", "a_b"},
		{"__tool__", "tool"},
		{"-x-", "x"},
		// Names that sanitize to nothing fall back to "srv".
		{"团队", "srv"},
		{"_", "srv"},
		{"", "srv"},
	}
	for _, tt := range tests {
		if got := sanitizeNameSegment(tt.in); got != tt.want {
			t.Errorf("sanitizeNameSegment(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestWireToolName(t *testing.T) {
	tests := []struct {
		name, server, tool, want string
	}{
		{"simple", "github", "get_me", "mcp__github__get_me"},
		{"server sanitized", "chrome-devtools", "take_screenshot", "mcp__chrome_devtools__take_screenshot"},
		{"url server collapses to single underscores", "https://mcp.example.com/sse", "search", "mcp__https_mcp_example_com_sse__search"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WireToolName(tt.server, tt.tool); got != tt.want {
				t.Errorf("WireToolName(%q, %q) = %q, want %q", tt.server, tt.tool, got, tt.want)
			}
		})
	}

	// Exactly at the limit: no truncation. "mcp__srv__" is 10 chars, so a
	// 54-char tool name lands exactly on wireNameMaxLen.
	exact := WireToolName("srv", strings.Repeat("a", 54))
	if want := "mcp__srv__" + strings.Repeat("a", 54); exact != want {
		t.Errorf("64-char name should be untouched, got %q (len %d)", exact, len(exact))
	}

	// One char over the limit: truncated back to exactly wireNameMaxLen and
	// still distinct from the name that landed exactly on it.
	over := WireToolName("srv", strings.Repeat("a", 55))
	if len(over) != wireNameMaxLen {
		t.Errorf("65-char name not capped: got len %d (%q)", len(over), over)
	}
	if over == exact {
		t.Errorf("on-limit and over-limit names collided: %q", over)
	}
	for _, n := range []string{exact, over} {
		if !geminiNamePattern.MatchString(n) {
			t.Errorf("wire name %q violates the Gemini charset", n)
		}
	}
}

// geminiNamePattern is the strictest tool-name charset among the supported
// providers (Gemini functionDeclaration names): no hyphens, letter or
// underscore first.
var geminiNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// TestWireToolNameSanitizedTool covers server-side tool names outside the
// Gemini charset (hyphens are common in the wild): the tool segment is
// sanitized and, because that is lossy, disambiguated with a hash suffix so
// distinct raw names that sanitize identically stay distinct.
func TestWireToolNameSanitizedTool(t *testing.T) {
	hyphen := WireToolName("github", "add-issue-comment")
	under := WireToolName("github", "add_issue_comment")

	if !strings.HasPrefix(hyphen, "mcp__github__add_issue_comment_") {
		t.Errorf("hyphenated tool not sanitized+suffixed: %q", hyphen)
	}
	if under != "mcp__github__add_issue_comment" {
		t.Errorf("clean tool should compose without a suffix, got %q", under)
	}
	if hyphen == under {
		t.Errorf("raw names sanitizing alike collided: %q", hyphen)
	}
	for _, n := range []string{hyphen, under} {
		if len(n) > wireNameMaxLen {
			t.Errorf("wire name over %d chars: %q", wireNameMaxLen, n)
		}
		if !geminiNamePattern.MatchString(n) {
			t.Errorf("wire name %q violates the Gemini charset", n)
		}
	}

	// Lossy AND over the limit at once: still capped, still charset-clean.
	long := WireToolName("srv", strings.Repeat("a-", 40))
	if len(long) != wireNameMaxLen || !geminiNamePattern.MatchString(long) {
		t.Errorf("long lossy name not capped/clean: %q (len %d)", long, len(long))
	}
}

// TestWireToolNameDelimiterAmbiguity: ("a", "b__c") and ("a__b", "c") used to
// compose the same wire name "mcp__a__b__c". Underscore-run collapsing keeps
// "__" out of segments — the server "a__b" becomes segment "a_b", and the
// tool "b__c" sanitizes lossily (hash suffix) — so the compositions stay
// distinct and parse unambiguously.
func TestWireToolNameDelimiterAmbiguity(t *testing.T) {
	splitTool := WireToolName("a", "b__c")
	splitServer := WireToolName("a__b", "c")

	if splitServer != "mcp__a_b__c" {
		t.Errorf(`WireToolName("a__b", "c") = %q, want "mcp__a_b__c"`, splitServer)
	}
	if !strings.HasPrefix(splitTool, "mcp__a__b_c_") {
		t.Errorf(`WireToolName("a", "b__c") = %q, want "mcp__a__b_c_<hash>"`, splitTool)
	}
	if splitTool == splitServer {
		t.Errorf("delimiter-ambiguous compositions collided: %q", splitTool)
	}
}

func TestWireToolNameTruncation(t *testing.T) {
	// Two long tool names that differ only past the truncation cut must yield
	// distinct wire names of exactly wireNameMaxLen chars.
	base := strings.Repeat("a", 80)
	n1 := WireToolName("srv", base+"1")
	n2 := WireToolName("srv", base+"2")

	for _, n := range []string{n1, n2} {
		if len(n) != wireNameMaxLen {
			t.Errorf("truncated name length = %d, want %d: %q", len(n), wireNameMaxLen, n)
		}
		if !strings.HasPrefix(n, "mcp__srv__aaa") {
			t.Errorf("truncated name lost its prefix: %q", n)
		}
		// Tail is "_" + 8 lowercase hex chars.
		tail := n[len(n)-9:]
		if tail[0] != '_' || strings.Trim(tail[1:], "0123456789abcdef") != "" {
			t.Errorf("truncated name tail %q is not _hhhhhhhh", tail)
		}
	}
	if n1 == n2 {
		t.Errorf("names differing past the cut collided: %q", n1)
	}
}

// startEchoServer spins up an in-memory MCP server exposing one tool named
// "echo" that replies "<id>:<raw tool name the server saw>", and returns a
// connected client session for it.
func startEchoServer(t *testing.T, id string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{Name: id, Version: "1.0.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "echo back the server id",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: id + ":" + req.Params.Name}},
		}, nil
	})

	clientTr, serverTr := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverTr, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { ss.Wait() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	cs, err := client.Connect(ctx, clientTr, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// TestManagerRoutesSameToolNameAcrossServers proves the namespacing fixes the
// routing bug: two servers exposing the same raw tool name get distinct wire
// names, each call reaches its own server, and the server receives the raw
// (un-namespaced) name.
func TestManagerRoutesSameToolNameAcrossServers(t *testing.T) {
	ctx := context.Background()
	m := &Manager{toolIndex: make(map[string]toolTarget)}
	ids := []string{"alpha", "beta"}
	m.servers = make([]ServerStatus, len(ids))
	for i, id := range ids {
		m.mergeResult(i, serverResult{
			status: ServerStatus{
				Name:      id,
				Connected: true,
				ToolCount: 1,
				Tools:     []string{"echo"},
			},
			session: startEchoServer(t, id),
			tools:   []provider.ToolDef{{Name: "echo", Description: "echo back the server id"}},
		})
	}

	// Both tools survive registration under distinct wire names.
	defs := m.Tools()
	if len(defs) != 2 || defs[0].Name != "mcp__alpha__echo" || defs[1].Name != "mcp__beta__echo" {
		t.Fatalf("unexpected tool defs: %+v", defs)
	}
	// ServerStatus keeps raw names and carries the assigned segment.
	for _, s := range m.Servers() {
		if len(s.Tools) != 1 || s.Tools[0] != "echo" {
			t.Errorf("server %s should list raw tool names, got %v", s.Name, s.Tools)
		}
		if s.Segment != s.Name {
			t.Errorf("server %s: assigned segment = %q, want %q", s.Name, s.Segment, s.Name)
		}
	}

	// Each wire name reaches its own server, which sees the raw name "echo".
	for _, tt := range []struct{ wire, want string }{
		{"mcp__alpha__echo", "alpha:echo"},
		{"mcp__beta__echo", "beta:echo"},
	} {
		text, isErr, err := m.CallTool(ctx, tt.wire, map[string]any{})
		if err != nil || isErr {
			t.Fatalf("CallTool(%s): isErr=%v err=%v", tt.wire, isErr, err)
		}
		if text != tt.want {
			t.Errorf("CallTool(%s) = %q, want %q", tt.wire, text, tt.want)
		}
	}

	// The raw name is not registered — only wire names are dispatchable.
	if _, _, err := m.CallTool(ctx, "echo", nil); err == nil {
		t.Errorf("CallTool with raw name should fail")
	}
}

// TestManagerDuplicateServerNames covers two servers sharing one name — a
// reachable config: repeated `--mcp "npx …"` flags are all named by argv[0].
// The manager assigns distinct segments in connect order ("npx", "npx_2"), so
// both servers' tools register and each wire name routes to its own server.
func TestManagerDuplicateServerNames(t *testing.T) {
	ctx := context.Background()
	m := &Manager{toolIndex: make(map[string]toolTarget)}
	ids := []string{"first", "second"}
	m.servers = make([]ServerStatus, len(ids))
	for i, id := range ids {
		m.mergeResult(i, serverResult{
			status: ServerStatus{
				Name:      "npx",
				Connected: true,
				ToolCount: 1,
				Tools:     []string{"echo"},
			},
			session: startEchoServer(t, id),
			tools:   []provider.ToolDef{{Name: "echo"}},
		})
	}

	defs := m.Tools()
	if len(defs) != 2 || defs[0].Name != "mcp__npx__echo" || defs[1].Name != "mcp__npx_2__echo" {
		t.Fatalf("unexpected tool defs: %+v", defs)
	}
	servers := m.Servers()
	if servers[0].Segment != "npx" || servers[1].Segment != "npx_2" {
		t.Fatalf("unexpected segments: %q, %q", servers[0].Segment, servers[1].Segment)
	}

	for _, tt := range []struct{ wire, want string }{
		{"mcp__npx__echo", "first:echo"},
		{"mcp__npx_2__echo", "second:echo"},
	} {
		text, isErr, err := m.CallTool(ctx, tt.wire, map[string]any{})
		if err != nil || isErr {
			t.Fatalf("CallTool(%s): isErr=%v err=%v", tt.wire, isErr, err)
		}
		if text != tt.want {
			t.Errorf("CallTool(%s) = %q, want %q", tt.wire, text, tt.want)
		}
	}
}

// TestManagerSanitizeCollidingServerNames covers names that differ only in
// characters the sanitizer rewrites: "my-server" and "my_server" both
// sanitize to "my_server", so the second gets the "_2" segment.
func TestManagerSanitizeCollidingServerNames(t *testing.T) {
	m := &Manager{toolIndex: make(map[string]toolTarget)}
	names := []string{"my-server", "my_server"}
	m.servers = make([]ServerStatus, len(names))
	for i, name := range names {
		m.mergeResult(i, serverResult{
			status: ServerStatus{
				Name:      name,
				Connected: true,
				ToolCount: 1,
				Tools:     []string{"echo"},
			},
			session: startEchoServer(t, name),
			tools:   []provider.ToolDef{{Name: "echo"}},
		})
	}

	defs := m.Tools()
	if len(defs) != 2 || defs[0].Name != "mcp__my_server__echo" || defs[1].Name != "mcp__my_server_2__echo" {
		t.Fatalf("unexpected tool defs: %+v", defs)
	}
}

// TestManagerSkipsDuplicateWireName exercises the mergeResult guardrail. Unique
// segments make wire collisions unreachable through normal registration, so
// force one with a (spec-violating) duplicated raw tool name: the first
// registration wins, the duplicate is skipped — never overwritten — and a
// warning is emitted through logf.
func TestManagerSkipsDuplicateWireName(t *testing.T) {
	var warnings []string
	logf := func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}
	m := &Manager{toolIndex: make(map[string]toolTarget), logf: logf}
	m.servers = make([]ServerStatus, 1)
	m.mergeResult(0, serverResult{
		status: ServerStatus{
			Name:      "srv",
			Connected: true,
			ToolCount: 2,
			Tools:     []string{"echo", "echo"},
		},
		session: startEchoServer(t, "srv"),
		tools: []provider.ToolDef{
			{Name: "echo", Description: "first"},
			{Name: "echo", Description: "second"},
		},
	})

	defs := m.Tools()
	if len(defs) != 1 || defs[0].Name != "mcp__srv__echo" || defs[0].Description != "first" {
		t.Fatalf("duplicate should be skipped and the first kept: %+v", defs)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "mcp__srv__echo") {
		t.Fatalf("expected one warning naming the duplicate, got %v", warnings)
	}
}

// TestManagerConcurrentMergeAndRead exercises the lock: several servers merge
// concurrently (as Connect's goroutines do) while readers hammer Tools /
// Servers / CallTool. Run under -race, it proves the incremental, background
// connection has no data race between a merging writer and a live reader.
func TestManagerConcurrentMergeAndRead(t *testing.T) {
	ctx := context.Background()
	const n = 8
	m := &Manager{toolIndex: make(map[string]toolTarget)}
	m.servers = make([]ServerStatus, n)

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = m.Tools()
					_ = m.Servers()
					_, _, _ = m.CallTool(ctx, "mcp__srv0__echo", map[string]any{})
				}
			}
		}()
	}

	var mergers sync.WaitGroup
	for i := 0; i < n; i++ {
		mergers.Add(1)
		go func(i int) {
			defer mergers.Done()
			id := fmt.Sprintf("srv%d", i)
			m.mergeResult(i, serverResult{
				status:  ServerStatus{Name: id, Connected: true, ToolCount: 1, Tools: []string{"echo"}},
				session: startEchoServer(t, id),
				tools:   []provider.ToolDef{{Name: "echo"}},
			})
		}(i)
	}
	mergers.Wait()
	close(stop)
	readers.Wait()

	// All servers resolved to connected with their tool registered.
	if got := len(m.Tools()); got != n {
		t.Errorf("Tools() = %d, want %d after all merges", got, n)
	}
	for _, s := range m.Servers() {
		if !s.Connected || s.Pending {
			t.Errorf("server %s: Connected=%v Pending=%v, want connected & not pending", s.Name, s.Connected, s.Pending)
		}
	}
}

// TestManagerConnectTimeout verifies the connect timeout is one kind of failure:
// a subprocess that starts but never speaks MCP (here a `cat` that reads and
// discards, emitting nothing) is abandoned when the timeout fires, surfacing as
// a failed server with a "timed out" error and no tools — quickly, not after the
// full 30s default. The SDK closes the session on the failed handshake, which
// terminates the subprocess (no leak; `cat` exits on the stdin EOF).
func TestManagerConnectTimeout(t *testing.T) {
	old := connectTimeout
	connectTimeout = 300 * time.Millisecond
	defer func() { connectTimeout = old }()

	m := NewManager([]ServerConfig{{Name: "hang", Command: "sh", Args: []string{"-c", "cat >/dev/null"}}}, nil)
	defer m.Close()

	start := time.Now()
	m.ConnectWait(context.Background())
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("ConnectWait took %s — timeout did not fire (would be ~%s)", elapsed, connectTimeout)
	}

	servers := m.Servers()
	if len(servers) != 1 {
		t.Fatalf("want 1 server, got %d", len(servers))
	}
	s := servers[0]
	if s.Connected || s.Pending {
		t.Errorf("hung server should be a resolved failure: Connected=%v Pending=%v", s.Connected, s.Pending)
	}
	if !strings.Contains(s.Err, "timed out") {
		t.Errorf("expected a timeout error, got %q", s.Err)
	}
	if got := len(m.Tools()); got != 0 {
		t.Errorf("timed-out server should contribute no tools, got %d", got)
	}
}

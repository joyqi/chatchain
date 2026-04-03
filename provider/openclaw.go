package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"sync"

	"github.com/a3tai/openclaw-go/gateway"
	"github.com/a3tai/openclaw-go/protocol"
	"github.com/fatih/color"
)

type openClawEvent struct {
	chatEvent *protocol.ChatEvent
	thinking  string
}

type OpenClawProvider struct {
	token      string
	wsURL      string
	agentID    string
	verbose    bool
	mu         sync.Mutex
	client     *gateway.Client
	sessionKey string
	eventMu    sync.Mutex
	listeners  map[string]chan openClawEvent
}

func NewOpenClaw(token, wsURL, agentID string, verbose bool) *OpenClawProvider {
	// Normalize URL: http → ws, https → wss, append /ws if missing
	u := wsURL
	if strings.HasPrefix(u, "http://") {
		u = "ws://" + u[len("http://"):]
	} else if strings.HasPrefix(u, "https://") {
		u = "wss://" + u[len("https://"):]
	}
	if !strings.HasSuffix(u, "/ws") {
		u = strings.TrimRight(u, "/") + "/ws"
	}

	return &OpenClawProvider{
		token:     token,
		wsURL:     u,
		agentID:   agentID,
		verbose:   verbose,
		listeners: make(map[string]chan openClawEvent),
	}
}

func (p *OpenClawProvider) ensureConnected(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.client != nil {
		return nil
	}

	client := gateway.NewClient(
		gateway.WithToken(p.token),
		gateway.WithRole(protocol.RoleOperator),
		gateway.WithScopes(
			protocol.ScopeOperatorAdmin,
			protocol.ScopeOperatorRead,
			protocol.ScopeOperatorWrite,
		),
		gateway.WithCaps("thinking-events"),
		gateway.WithOnEvent(p.handleEvent),
	)

	if err := client.Connect(ctx, p.wsURL); err != nil {
		return fmt.Errorf("failed to connect to OpenClaw gateway: %w", err)
	}

	if p.verbose {
		dimLog( "Connected to %s\n", p.wsURL)
	}

	p.client = client
	return nil
}

func (p *OpenClawProvider) handleEvent(e protocol.Event) {
	switch e.EventName {
	case protocol.EventChat:
		var evt protocol.ChatEvent
		if err := json.Unmarshal(e.Payload, &evt); err != nil {
			return
		}
		if p.verbose {
			dimLog( "← event:chat {state:%s, runId:%s}\n", evt.State, evt.RunID)
		}
		p.eventMu.Lock()
		ch, ok := p.listeners[evt.RunID]
		p.eventMu.Unlock()
		if ok {
			ch <- openClawEvent{chatEvent: &evt}
		}

	case protocol.EventAgent:
		// Parse thinking events: {runId, stream: "thinking", data: {text, delta}}
		var raw struct {
			RunID  string `json:"runId"`
			Stream string `json:"stream"`
			Data   struct {
				Delta string `json:"delta"`
			} `json:"data"`
		}
		if err := json.Unmarshal(e.Payload, &raw); err != nil {
			return
		}
		if raw.Stream != "thinking" || raw.Data.Delta == "" {
			return
		}
		if p.verbose {
			dimLog( "← event:agent {stream:thinking, runId:%s}\n", raw.RunID)
		}
		p.eventMu.Lock()
		ch, ok := p.listeners[raw.RunID]
		p.eventMu.Unlock()
		if ok {
			ch <- openClawEvent{thinking: raw.Data.Delta}
		}
	}
}

func (p *OpenClawProvider) ensureSession(ctx context.Context) error {
	if p.sessionKey != "" {
		return nil
	}

	p.sessionKey = fmt.Sprintf("chatchain-%08x", rand.Int31())

	if p.verbose {
		dimLog( "→ sessions.create {key:%s, agentId:%s}\n", p.sessionKey, p.agentID)
	}

	_, err := p.client.SessionsCreate(ctx, protocol.SessionsCreateParams{
		Key:     p.sessionKey,
		AgentID: p.agentID,
	})
	if err != nil {
		p.sessionKey = ""
		return fmt.Errorf("failed to create session: %w", err)
	}
	return nil
}

func (p *OpenClawProvider) ListModels(ctx context.Context) ([]string, error) {
	if err := p.ensureConnected(ctx); err != nil {
		return nil, err
	}

	if p.verbose {
		dimLog("→ agents.list\n")
	}

	result, err := p.client.AgentsList(ctx)
	if err != nil {
		// AgentsList requires operator.read scope which the token may not have.
		// Return a helpful error suggesting to use -M to specify the agent directly.
		return nil, fmt.Errorf("failed to list agents (token may lack operator.read scope): %w\n  Tip: use -M <agent-id> to specify the agent directly (e.g. -M main)", err)
	}

	var agents []string
	for _, a := range result.Agents {
		agents = append(agents, a.ID)
	}
	return agents, nil
}

func (p *OpenClawProvider) Chat(ctx context.Context, messages []Message) (string, error) {
	content, _, err := p.StreamChat(ctx, messages, io.Discard, nopWriteCloser{})
	return content, err
}

func (p *OpenClawProvider) StreamChat(ctx context.Context, messages []Message, w io.Writer, reasoningW io.WriteCloser) (string, string, error) {
	if err := p.ensureConnected(ctx); err != nil {
		return "", "", err
	}
	if err := p.ensureSession(ctx); err != nil {
		return "", "", err
	}

	// Extract last user message
	var lastMsg string
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			lastMsg = messages[i].Content
			break
		}
	}
	if lastMsg == "" {
		return "", "", fmt.Errorf("no user message found")
	}

	idempotencyKey := fmt.Sprintf("cc-%08x", rand.Int31())

	if p.verbose {
		dimLog( "→ chat.send {sessionKey:%s, message:%q}\n", p.sessionKey, truncate(lastMsg, 80))
	}

	resp, err := p.client.ChatSend(ctx, protocol.ChatSendParams{
		SessionKey:     p.sessionKey,
		Message:        lastMsg,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return "", "", fmt.Errorf("chat send error: %w", err)
	}

	runID := resp.RunID

	// Register listener
	ch := make(chan openClawEvent, 64)
	p.eventMu.Lock()
	p.listeners[runID] = ch
	p.eventMu.Unlock()

	defer func() {
		p.eventMu.Lock()
		delete(p.listeners, runID)
		p.eventMu.Unlock()
	}()

	var full, thinkFull string
	reasoningClosed := false
	closeReasoning := func() {
		if !reasoningClosed {
			reasoningW.Close()
			reasoningClosed = true
		}
	}

	for {
		select {
		case <-ctx.Done():
			closeReasoning()
			return full, thinkFull, ctx.Err()

		case evt := <-ch:
			if evt.thinking != "" {
				// Thinking delta
				fmt.Fprint(reasoningW, evt.thinking)
				thinkFull += evt.thinking
				continue
			}

			ce := evt.chatEvent
			if ce == nil {
				continue
			}

			switch ce.State {
			case "delta":
				closeReasoning()
				var delta string
				if err := json.Unmarshal(ce.Message, &delta); err == nil {
					fmt.Fprint(w, delta)
					full += delta
				}

			case "final":
				closeReasoning()
				return full, thinkFull, nil

			case "error":
				closeReasoning()
				msg := ce.ErrorMessage
				if msg == "" {
					msg = "unknown error"
				}
				return full, thinkFull, fmt.Errorf("openclaw error: %s", msg)

			case "aborted":
				closeReasoning()
				return full, thinkFull, fmt.Errorf("openclaw: response aborted")
			}
		}
	}
}

var dimStyle = color.New(color.Faint)

func dimLog(format string, a ...any) {
	dimStyle.Fprintf(os.Stderr, format, a...)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

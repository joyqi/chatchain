package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"chatchain/chat"
	"chatchain/config"
	mcpmgr "chatchain/mcp"
	"chatchain/provider"
	"chatchain/tool"

	"github.com/spf13/cobra"
)

var (
	apiKey            string
	baseURL           string
	model             string
	temperature       float64
	chatMessage       string
	systemPrompt      string
	systemInteractive bool
	verbose           bool
	configPath        string
	list              bool
	mcpFlags          []string
	resumeID          string
	noSave            bool
	contextWindowFlag string
	agentFlag         bool
)

var rootCmd = &cobra.Command{
	Use:   "chatchain [openai|anthropic|gemini|vertexai|openresponses]",
	Short: "A lightweight cross-platform AI chat CLI",
	Args:  cobra.RangeArgs(0, 1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.Load(configPath)

		// List mode: no provider arg → list providers; with provider arg → list models
		if list {
			return runList(cmd, cfg, args)
		}

		if len(args) == 0 {
			return fmt.Errorf("provider argument is required (e.g. openai, anthropic, gemini), or use -l to list available providers")
		}

		providerType, pc := cfg.Get(args[0])

		// Priority: CLI flag > env var > config file
		if !cmd.Flags().Changed("key") {
			envKey := providerEnvKey(providerType)
			if envVal := os.Getenv(envKey); envVal != "" {
				apiKey = envVal
			} else if pc.Key != "" {
				apiKey = pc.Key
			}
		}
		if !cmd.Flags().Changed("url") && baseURL == "" {
			if pc.URL != "" {
				baseURL = pc.URL
			}
		}
		if !cmd.Flags().Changed("model") && model == "" {
			if pc.Model != "" {
				model = pc.Model
			}
		}
		if !cmd.Flags().Changed("system") && systemPrompt == "" {
			if pc.System != "" {
				systemPrompt = pc.System
			}
		}

		if apiKey == "" {
			envKey := providerEnvKey(providerType)
			return fmt.Errorf("API key is required: use -k/--key or set %s", envKey)
		}

		// Non-interactive mode: read from stdin when -m -
		if chatMessage == "-" {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("failed to read from stdin: %w", err)
			}
			chatMessage = strings.TrimSpace(string(data))
			if chatMessage == "" {
				return fmt.Errorf("no message provided via stdin")
			}
		}

		// Non-interactive mode requires a model
		if chatMessage != "" && model == "" {
			return fmt.Errorf("--model/-M is required when using --message/-m")
		}

		var temp *float64
		if cmd.Flags().Changed("temperature") {
			temp = &temperature
		}

		var httpClient *http.Client
		if verbose {
			httpClient = chat.NewVerboseHTTPClient()
		}

		p, err := provider.New(providerType, apiKey, baseURL, model, temp, httpClient)
		if err != nil {
			return err
		}

		// Agent mode is explicitly opt-in: the --agent flag or the provider's
		// `agent: true` config. It anchors the AGENTS.md/skills overlay in the
		// REPL and auto-enables the read_file built-in tool everywhere.
		agentMode := agentFlag || pc.Agent

		// Build MCP server configs from CLI flags + config file
		mcpConfigs := buildMCPConfigs(cfg)
		var logf mcpmgr.LogFunc
		if verbose {
			logf = func(format string, args ...any) {
				chat.DimStyle.Fprintf(os.Stderr, format, args...)
			}
		}

		// The working directory is recorded in session meta in both modes; in
		// agent mode the project root additionally anchors the AGENTS.md/skills
		// overlay and (interactively) the project session bucket and scoped
		// pickers.
		cwd, cwdErr := os.Getwd()
		var agentOpts chat.AgentOptions
		if agentMode {
			if cwdErr != nil {
				return fmt.Errorf("failed to resolve working directory: %w", cwdErr)
			}
			agentOpts = chat.AgentOptions{Enabled: true, Root: chat.ProjectRoot(cwd)}
		}

		// Non-interactive mode: connect MCP synchronously (quiet), then respond.
		if chatMessage != "" {
			var mgr *mcpmgr.Manager
			if len(mcpConfigs) > 0 {
				mgr, _ = mcpmgr.NewManager(context.Background(), mcpConfigs, logf)
				defer mgr.Close()
			}
			dispatch := buildDispatcher(pc, mgr, agentMode)
			return chat.Once(context.Background(), p, chatMessage, systemPrompt, dispatch, agentOpts, os.Stdout)
		}

		// Interactive: kick off MCP connect concurrently in the background so it
		// overlaps the interactive model/session prompts; we join before chat.
		var mcpCh chan *mcpmgr.Manager
		if len(mcpConfigs) > 0 {
			mcpCh = make(chan *mcpmgr.Manager, 1)
			go func() {
				m, _ := mcpmgr.NewManager(context.Background(), mcpConfigs, logf)
				mcpCh <- m
			}()
		}

		if noSave && cmd.Flags().Changed("resume") {
			return fmt.Errorf("--no-save cannot be combined with --resume")
		}


		// Resume an existing session (before model selection: a resumed session
		// can supply the model when -M is omitted).
		var importedHistory []provider.Message
		var sw *chat.SessionWriter
		sessionWindow := 0
		if cmd.Flags().Changed("resume") {
			id := strings.TrimSpace(resumeID)
			if id == "" {
				id, err = chat.PickSession(agentOpts.Root)
				if err != nil {
					return fmt.Errorf("failed to list sessions: %w", err)
				}
			} else {
				// The flag value may be an id fragment; resolve exact matches
				// or unique prefixes (works for old ULID ids too). Agent mode
				// tries the project's bucket first, then the global store.
				id, err = chat.ResolveSessionID(id, agentOpts.Root)
				if err != nil {
					return err
				}
			}
			if id == "" {
				return fmt.Errorf("no session to resume")
			}
			writer, sess, rerr := chat.ResumeSession(id, p)
			if rerr != nil {
				return rerr
			}
			sw = writer
			importedHistory = sess.Messages
			if model == "" && sess.Meta.Provider == p.Type() && sess.Meta.Model != "" {
				p.SetModel(sess.Meta.Model)
			}
			// Replay the session's tuning knobs; explicit flags win (mirroring
			// the model guard above): temperature only when -t wasn't passed,
			// context window only when --context-window wasn't. Effort has no
			// flag, so the session's value always applies.
			chat.ApplySessionTuning(sess, p,
				cmd.Flags().Changed("temperature"),
				strings.TrimSpace(contextWindowFlag) != "",
				func(n int) { sessionWindow = n })
			fmt.Printf("Resumed session %s (%d messages)\n\n", id, len(importedHistory))
		}

		// If no model is set yet, offer interactive selection. Cancelling
		// (ESC) is allowed: we enter the chat without a model and pick one
		// lazily on the first message (see ensureModel in chat.Run).
		if p.Model() == "" {
			models, fetchErr := chat.FetchModels(context.Background(), p)
			if fetchErr != nil {
				return fmt.Errorf("failed to list models: %w", fetchErr)
			}
			if len(models) == 0 {
				return fmt.Errorf("no models available")
			}

			selected, serr := chat.SelectModel(models)
			if serr != nil {
				return fmt.Errorf("model selection failed: %w", serr)
			}
			if selected != "" {
				fmt.Printf("Using model: %s\n\n", chat.BoldStyle.Sprint(selected))
				p.SetModel(selected)
			}
		}

		systemPrompt = strings.TrimSpace(systemPrompt)
		if systemInteractive {
			sp, sperr := chat.ReadSystemPrompt(os.Stdout)
			if sperr != nil {
				return sperr
			}
			systemPrompt = sp
		}

		// Create a fresh session writer unless resuming or ephemeral (--no-save).
		// Agent-mode sessions land in the project's bucket keyed by its root;
		// normal-mode sessions stay flat but still record where they started.
		if sw == nil && !noSave {
			sessionCwd := cwd
			if agentMode {
				sessionCwd = agentOpts.Root
			}
			sw, err = chat.NewSessionWriter(p, temp, baseURL, sessionCwd, agentMode)
			if err != nil {
				return fmt.Errorf("failed to create session: %w", err)
			}
		}

		// Resolve context window: flag > session meta > config > default
		// (0 → chat default).
		contextWindow := 0
		if v := strings.TrimSpace(contextWindowFlag); v != "" {
			n, perr := chat.ParseWindowSize(v)
			if perr != nil {
				return fmt.Errorf("--context-window: %w", perr)
			}
			contextWindow = n
		} else if sessionWindow > 0 {
			contextWindow = sessionWindow
		} else if pc.ContextWindow != "" {
			n, perr := chat.ParseWindowSize(pc.ContextWindow)
			if perr != nil {
				return fmt.Errorf("config context_window: %w", perr)
			}
			contextWindow = n
		}

		// Join the background MCP connect (started above) before entering chat.
		// Already-finished → spinner flashes briefly; otherwise it shows until
		// the slowest server resolves. Failed servers are reported, not fatal.
		var mgr *mcpmgr.Manager
		if mcpCh != nil {
			chat.WithSpinner("Connecting to MCP servers…", func() { mgr = <-mcpCh })
			reportMCPStatus(mgr)
			defer mgr.Close()
		}

		dispatch := buildDispatcher(pc, mgr, agentMode)
		return chat.Run(p, systemPrompt, importedHistory, dispatch, mgr, sw, contextWindow, agentOpts, os.Stdout)
	},
}

func init() {
	rootCmd.Flags().StringVarP(&apiKey, "key", "k", "", "API key (required)")
	rootCmd.Flags().StringVarP(&baseURL, "url", "u", "", "Base URL (optional)")
	rootCmd.Flags().StringVarP(&model, "model", "M", "", "Model name (optional, interactive selection if omitted)")
	rootCmd.Flags().Float64VarP(&temperature, "temperature", "t", 0, "Sampling temperature (0.0-2.0)")
	rootCmd.Flags().StringVarP(&chatMessage, "message", "m", "", "Send a single message and print the response (non-interactive, use '-' to read from stdin)")
	rootCmd.Flags().StringVarP(&systemPrompt, "system", "s", "", "System prompt")
	rootCmd.Flags().BoolVarP(&systemInteractive, "system-input", "S", false, "Enter system prompt interactively")
	rootCmd.Flags().BoolVarP(&list, "list", "l", false, "List configured providers, or models for a given provider")
	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Print request and response bodies for debugging")
	rootCmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to config file (default: ~/.chatchain.yaml)")
	rootCmd.Flags().StringArrayVar(&mcpFlags, "mcp", nil, "MCP server (command string or URL, repeatable)")
	rootCmd.Flags().StringVar(&resumeID, "resume", "", "Resume a saved session: --resume to pick interactively, or --resume=<id>")
	rootCmd.Flags().Lookup("resume").NoOptDefVal = " " // allow bare --resume (interactive picker)
	rootCmd.Flags().BoolVar(&noSave, "no-save", false, "Do not persist this session to disk (ephemeral)")
	rootCmd.Flags().StringVar(&contextWindowFlag, "context-window", "", "Context window size for compaction accounting (e.g. 200k, 1m); default 128k")
	rootCmd.Flags().BoolVar(&agentFlag, "agent", false, "Enable agent mode (AGENTS.md system-prompt overlay)")
}

// hasAPIKey checks if a provider has a usable API key from env or config.
func hasAPIKey(providerType string, pc config.ProviderConfig) bool {
	if pc.Key != "" {
		return true
	}
	envKey := providerEnvKey(providerType)
	return os.Getenv(envKey) != ""
}

func runList(cmd *cobra.Command, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		var available []string
		for name := range cfg.Providers {
			providerType, pc := cfg.Get(name)
			if !hasAPIKey(providerType, pc) {
				continue
			}
			info := name
			if providerType != name {
				info += fmt.Sprintf(" (type: %s", providerType)
				if pc.URL != "" {
					info += fmt.Sprintf(", url: %s", pc.URL)
				}
				if pc.Model != "" {
					info += fmt.Sprintf(", model: %s", pc.Model)
				}
				info += ")"
			} else if pc.Model != "" {
				info += fmt.Sprintf(" (default model: %s)", pc.Model)
			}
			available = append(available, info)
		}
		sort.Strings(available)

		if len(available) == 0 {
			fmt.Println("No providers configured. Set API keys via environment variables or ~/.chatchain.yaml")
			return nil
		}

		fmt.Println("Available providers:")
		for _, info := range available {
			fmt.Printf("  %s\n", info)
		}
		return nil
	}

	// List models for a specific provider
	providerType, pc := cfg.Get(args[0])

	// Priority: CLI flag > env var > config file
	if !cmd.Flags().Changed("key") {
		envKey := providerEnvKey(providerType)
		if envVal := os.Getenv(envKey); envVal != "" {
			apiKey = envVal
		} else if pc.Key != "" {
			apiKey = pc.Key
		}
	}
	if !cmd.Flags().Changed("url") && baseURL == "" {
		if pc.URL != "" {
			baseURL = pc.URL
		}
	}

	if apiKey == "" {
		envKey := providerEnvKey(providerType)
		return fmt.Errorf("API key is required to list models: use -k/--key or set %s", envKey)
	}

	var httpClient *http.Client
	if verbose {
		httpClient = chat.NewVerboseHTTPClient()
	}

	p, err := provider.New(providerType, apiKey, baseURL, "", nil, httpClient)
	if err != nil {
		return err
	}

	models, err := chat.FetchModels(context.Background(), p)
	if err != nil {
		return fmt.Errorf("failed to list models: %w", err)
	}
	if len(models) == 0 {
		fmt.Println("No models available.")
		return nil
	}

	fmt.Printf("Models for %s:\n", args[0])
	for _, m := range models {
		fmt.Printf("  %s\n", m)
	}
	return nil
}

var providerEnvKeys = map[string]string{
	"openai":        "OPENAI_API_KEY",
	"anthropic":     "ANTHROPIC_API_KEY",
	"gemini":        "GOOGLE_API_KEY",
	"vertexai":      "GOOGLE_API_KEY",
	"openresponses": "OPENAI_API_KEY",
}

func providerEnvKey(providerType string) string {
	if key, ok := providerEnvKeys[providerType]; ok {
		return key
	}
	return "API_KEY"
}

// reportMCPStatus prints a persistent one-line MCP summary plus a warning line
// per failed server. Persistent (unlike the ephemeral connect spinner, which is
// invisible when the connect finishes during interactive model selection) so the
// user always sees what loaded. Connected servers stay usable on failure
// (graceful degradation); failures are surfaced, not silently dropped.
func reportMCPStatus(mgr *mcpmgr.Manager) {
	if mgr == nil {
		return
	}
	servers := mgr.Servers()
	if len(servers) == 0 {
		return
	}

	connected, tools := 0, 0
	for _, s := range servers {
		if s.Connected {
			connected++
			tools += s.ToolCount
		}
	}
	if connected > 0 {
		chat.DimStyle.Fprintf(os.Stdout, "MCP: %d/%d servers connected, %d tools\n", connected, len(servers), tools)
	}
	for _, s := range servers {
		if s.Connected {
			continue
		}
		msg := s.Err
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		chat.ErrorStyle.Fprintf(os.Stdout, "⚠ MCP %s: %s\n", s.Name, msg)
	}
}

// buildDispatcher assembles the tool dispatcher for a provider: its enabled
// built-in tools (from the provider's `tools:` config) merged with the MCP
// manager. Agent mode additionally auto-enables the read_file built-in — a
// skill is activated by reading its SKILL.md through it — with a config entry
// still free to declare it explicitly. Built-ins are passed first so they win
// any tool-name collision. The result is always non-nil (it may advertise no
// tools).
func buildDispatcher(pc config.ProviderConfig, mgr *mcpmgr.Manager, agent bool) tool.Dispatcher {
	warnf := func(format string, args ...any) {
		chat.ErrorStyle.Fprintf(os.Stderr, "⚠ "+format+"\n", args...)
	}
	reg := tool.Build(pc.Tools, warnf)
	if agent {
		reg.Enable("read_file", warnf)
	}

	var parts []tool.Dispatcher
	if len(reg.Tools()) > 0 {
		parts = append(parts, reg)
	}
	if mgr != nil {
		parts = append(parts, mgr)
	}
	return tool.Merge(parts...)
}

func buildMCPConfigs(cfg *config.Config) []mcpmgr.ServerConfig {
	var configs []mcpmgr.ServerConfig

	// From config file
	for name, sc := range cfg.MCPServers {
		configs = append(configs, mcpmgr.ServerConfig{
			Name:    name,
			Command: sc.Command,
			Args:    sc.Args,
			URL:     sc.URL,
			Env:     sc.Env,
			Headers: sc.Headers,
		})
	}

	// From CLI flags
	for _, flag := range mcpFlags {
		configs = append(configs, mcpmgr.ParseMCPFlag(flag))
	}

	return configs
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

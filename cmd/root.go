package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"chatchain/chat"
	"chatchain/config"
	"chatchain/internal/agents"
	mcpmgr "chatchain/mcp"
	"chatchain/provider"
	"chatchain/tool"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	apiKey            string
	baseURL           string
	model             string
	temperature       float64
	chatMessage       string
	systemPrompt      string
	systemInteractive bool
	configPath        string
	list              bool
	mcpFlags          []string
	resumeID          string
	noSave            bool
	contextWindowFlag string
	agentFlag         bool
	maxTurns          int
)

var rootCmd = &cobra.Command{
	Use:   "chatchain [openai|anthropic|gemini|vertexai|openresponses|imagen|images]",
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
		if err := checkProviderName(cfg, args[0], providerType); err != nil {
			return err
		}

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
			sys, serr := pc.ResolveSystem()
			if serr != nil {
				return serr
			}
			systemPrompt = sys
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

		// Always install the recording transport: the /debug command browses
		// recent requests, and its Verbose toggle flips the live echo to stderr
		// (this replaces the old -v flag).
		reqLog := chat.NewRequestLog()
		p, err := provider.New(providerType, apiKey, baseURL, model, temp, reqLog.HTTPClient())
		if err != nil {
			return err
		}
		if pc.Image {
			if tun, ok := p.(provider.ImageTunable); ok {
				tun.SetImageOutput(true)
			} else if _, ok := p.(provider.ImageGenTunable); ok {
				fmt.Fprintf(os.Stderr, "Warning: `image: true` is redundant for provider type %s (it always generates images)\n", p.Type())
			}
		}
		if pc.Effort != "" {
			if !provider.ValidEffort(pc.Effort) {
				return fmt.Errorf("config effort %q: want low|medium|high|xhigh|max", pc.Effort)
			}
			if tun, ok := p.(provider.Tunable); ok {
				tun.SetEffort(pc.Effort)
			} else {
				fmt.Fprintf(os.Stderr, "Warning: `effort` does not apply to provider type %s (ignored)\n", p.Type())
			}
		}
		if temp != nil {
			if _, ok := p.(provider.Tunable); !ok {
				fmt.Fprintf(os.Stderr, "Warning: --temperature does not apply to provider type %s (ignored)\n", p.Type())
			}
		}
		if pc.JSONEdits {
			if tun, ok := p.(provider.ImageEditJSONTunable); ok {
				tun.SetJSONEdits(true)
			} else {
				fmt.Fprintf(os.Stderr, "Warning: `json_edits` applies only to the images provider type (ignored for %s)\n", p.Type())
			}
		}
		if g := (provider.ImageGenParams{
			AspectRatio:    pc.AspectRatio,
			ImageSize:      pc.ImageSize,
			NegativePrompt: pc.NegativePrompt,
		}); g != (provider.ImageGenParams{}) {
			if tun, ok := p.(provider.ImageGenTunable); ok {
				tun.SetImageGenParams(g)
			} else {
				fmt.Fprintf(os.Stderr, "Warning: aspect_ratio/image_size/negative_prompt apply only to image providers (ignored for type %s)\n", p.Type())
			}
		}

		// Agent mode is explicitly opt-in: the --agent flag or the provider's
		// `agent: true` config. It anchors the AGENTS.md/skills overlay in the
		// REPL and auto-enables the agent toolset (load_skill) everywhere.
		agentMode := agentFlag || pc.Agent

		// Build MCP server configs from CLI flags + config file (the
		// provider's mcp_servers key selects the config-file subset).
		mcpConfigs, mcpErr := buildMCPConfigs(cfg, pc)
		if mcpErr != nil {
			return mcpErr
		}
		// Explicitly configured tools/MCP servers can never be called by a
		// provider without tool calling — say so instead of listing dead tools.
		if _, ok := p.(provider.ToolProvider); !ok && (len(pc.Tools) > 0 || len(mcpConfigs) > 0) {
			fmt.Fprintf(os.Stderr, "Warning: tools/mcp_servers do not apply to provider type %s (no tool calling)\n", p.Type())
		}
		// MCP connection logging was tied to the removed -v flag; keep it off.
		var logf mcpmgr.LogFunc

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
			agentOpts = chat.AgentOptions{Enabled: true, Root: agents.ProjectRoot(cwd)}
		}

		// The toolsets' host context. The project root anchors the agent set's
		// skill discovery — resolved in every mode, so a `tools: {agent: ...}`
		// entry works outside agent mode too.
		toolEnv := tool.Env{ProjectRoot: agentOpts.Root}
		// Interactive runs get the ask seam (created unbound — chat.Run
		// binds the live UI); -m runs leave it nil, so the ask set
		// contributes no tools and the model never sees them.
		var interact *chat.Interactor
		if chatMessage == "" {
			interact = chat.NewInteractor()
			toolEnv.Interact = interact
		}
		if toolEnv.ProjectRoot == "" && cwdErr == nil {
			toolEnv.ProjectRoot = agents.ProjectRoot(cwd)
		}

		// Non-interactive mode: connect MCP synchronously (the single request needs
		// the full tool set before it is sent), quietly, then respond.
		if chatMessage != "" {
			var mgr *mcpmgr.Manager
			if len(mcpConfigs) > 0 {
				mgr = mcpmgr.NewManager(mcpConfigs, logf)
				mgr.ConnectWait(context.Background())
				defer mgr.Close()
			}
			dispatch := buildDispatcher(pc, mgr, agentMode, toolEnv)
			return chat.Once(context.Background(), p, chatMessage, systemPrompt, dispatch, agentOpts, maxTurns, os.Stdout)
		}

		// Interactive: start connecting MCP servers NOW, in the background, so the
		// connect overlaps the (potentially slow) model/session selection below
		// instead of only starting once we reach chat.Run. Their tools join the
		// live set as they resolve; chat.Run consumes mgr.Events() to report
		// failures once the prompt (and its refreshing writer) exist.
		var mgr *mcpmgr.Manager
		if len(mcpConfigs) > 0 {
			mgr = mcpmgr.NewManager(mcpConfigs, logf)
			defer mgr.Close()
			connectCtx, cancelConnect := context.WithCancel(context.Background())
			defer cancelConnect()
			mgr.Connect(connectCtx)
		}

		if noSave && cmd.Flags().Changed("resume") {
			return fmt.Errorf("--no-save cannot be combined with --resume")
		}
		// Config no_save: same ephemeral start, except an explicit --resume
		// of a persisted session outranks it.
		if pc.NoSave && !cmd.Flags().Changed("resume") {
			noSave = true
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

		// Startup model selection (when -M is omitted and the session supplies
		// none) and the -S system-prompt read both run inside the chat UI's
		// Program now — see Run's pre-loop interactions.
		systemPrompt = strings.TrimSpace(systemPrompt)

		// Create a fresh session writer unless resuming or ephemeral (--no-save).
		// Agent-mode sessions land in the project's bucket keyed by its root;
		// normal-mode sessions stay flat but still record where they started.
		sessionCwd := cwd
		if agentMode {
			sessionCwd = agentOpts.Root
		}
		if sw == nil && !noSave {
			sw, err = chat.NewSessionWriter(p, temp, baseURL, sessionCwd, agentMode)
			if err != nil {
				return fmt.Errorf("failed to create session: %w", err)
			}
		}
		// Ephemeral mode gets a DEFERRED factory instead: /save mints the
		// session then, reading the provider's current tuning (not startup
		// flags) so mid-chat /model changes land in the meta.
		var newSession chat.SessionFactory
		if sw == nil && noSave {
			newSession = func() (*chat.SessionWriter, error) {
				curTemp := temp
				if tun, ok := p.(provider.Tunable); ok {
					curTemp = tun.Temperature()
				}
				return chat.NewSessionWriter(p, curTemp, baseURL, sessionCwd, agentMode)
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
		if contextWindow > 0 {
			if _, ok := p.(provider.UsageReporter); !ok {
				fmt.Fprintf(os.Stderr, "Warning: context window does not apply to provider type %s (no token accounting)\n", p.Type())
			}
		}

		// MCP connects in the background from inside chat.Run (which owns the
		// prompt and reports each server as it resolves); the dispatcher reads the
		// manager's tool set live, so late-arriving tools appear without a rebuild.
		dispatch := buildDispatcher(pc, mgr, agentMode, toolEnv)
		// The interactive UI requires a terminal (docs/design/ui-architecture.md);
		// piped input goes through -m/--message.
		if !term.IsTerminal(int(os.Stdout.Fd())) {
			return fmt.Errorf("interactive mode requires a terminal; use -m/--message for piped input")
		}
		return chat.Run(p, systemPrompt, systemInteractive, importedHistory, dispatch, mgr, sw, newSession, interact, contextWindow, agentOpts, reqLog)
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
	rootCmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to config file (default: ~/.chatchain.yaml)")
	rootCmd.Flags().StringArrayVar(&mcpFlags, "mcp", nil, "MCP server (command string or URL, repeatable)")
	rootCmd.Flags().StringVar(&resumeID, "resume", "", "Resume a saved session: --resume to pick interactively, or --resume=<id>")
	rootCmd.Flags().Lookup("resume").NoOptDefVal = " " // allow bare --resume (interactive picker)
	rootCmd.Flags().BoolVar(&noSave, "no-save", false, "Start ephemeral: nothing persists unless you run /save in the chat")
	rootCmd.Flags().IntVar(&maxTurns, "max-turns", 0, "Limit agentic tool turns in non-interactive mode (-m only; 0 = unlimited)")
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
	if err := checkProviderName(cfg, args[0], providerType); err != nil {
		return err
	}

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

	p, err := provider.New(providerType, apiKey, baseURL, "", nil, nil)
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

// checkProviderName rejects a name that is neither a configured alias nor a
// built-in type BEFORE key resolution — a mistyped alias otherwise dies on a
// misleading "API key is required" error.
func checkProviderName(cfg *config.Config, name, providerType string) error {
	if _, configured := cfg.Providers[name]; configured || provider.KnownType(providerType) {
		return nil
	}
	aliases := make([]string, 0, len(cfg.Providers))
	for a := range cfg.Providers {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	hint := ""
	if len(aliases) > 0 {
		hint = fmt.Sprintf("\n  configured aliases: %s", strings.Join(aliases, ", "))
	}
	return fmt.Errorf("unknown provider %q: not a configured alias or a built-in type%s\n  built-in types: %s",
		name, hint, strings.Join(provider.KnownTypes(), ", "))
}

var providerEnvKeys = map[string]string{
	"openai":        "OPENAI_API_KEY",
	"anthropic":     "ANTHROPIC_API_KEY",
	"gemini":        "GOOGLE_API_KEY",
	"vertexai":      "GOOGLE_API_KEY",
	"openresponses": "OPENAI_API_KEY",
	"imagen":        "GOOGLE_API_KEY", // official Gemini API is its default target
	"images":        "OPENAI_API_KEY", // official OpenAI API is its default target
}

func providerEnvKey(providerType string) string {
	if key, ok := providerEnvKeys[providerType]; ok {
		return key
	}
	return "API_KEY"
}

// buildDispatcher assembles the tool dispatcher for a provider: its enabled
// built-in toolsets (from the provider's `tools:` config) merged with the MCP
// manager. Agent mode additionally auto-enables the agent set — skills are
// activated through its load_skill tool — with a config entry still free to
// declare it explicitly. Built-ins are passed first so they win any tool-name
// collision. The result is always non-nil (it may advertise no tools).
func buildDispatcher(pc config.ProviderConfig, mgr *mcpmgr.Manager, agent bool, env tool.Env) tool.Dispatcher {
	warnf := func(format string, args ...any) {
		chat.ErrorStyle.Fprintf(os.Stderr, "⚠ "+format+"\n", args...)
	}
	reg := tool.Build(env, pc.Tools, warnf)
	if agent {
		reg.EnableSet(env, "agent", warnf)
	}
	// The ask set is on by default whenever interaction is possible; an
	// explicit `ask: false` opts out (Build already skipped it; EnableSet
	// must not resurrect it).
	if env.Interact != nil && !tool.SetDisabled(pc.Tools, "ask") {
		reg.EnableSet(env, "ask", warnf)
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

func buildMCPConfigs(cfg *config.Config, pc config.ProviderConfig) ([]mcpmgr.ServerConfig, error) {
	var configs []mcpmgr.ServerConfig

	// From config file, filtered by the provider's mcp_servers selection.
	// --mcp flag servers always load: an explicit flag outranks config.
	selected, err := cfg.MCPServersFor(pc)
	if err != nil {
		return nil, err
	}
	for name, sc := range selected {
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

	return configs, nil
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

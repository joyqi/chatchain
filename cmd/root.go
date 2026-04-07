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

	"github.com/spf13/cobra"
)

var (
	apiKey       string
	baseURL      string
	model        string
	temperature  float64
	chatMessage  string
	systemPrompt string
	verbose      bool
	configPath   string
	list         bool
	mcpFlags     []string
)

var rootCmd = &cobra.Command{
	Use:   "chatchain [openai|anthropic|gemini|vertexai|openresponses|openclaw]",
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

		// Build MCP server configs from CLI flags + config file
		mcpConfigs := buildMCPConfigs(cfg)
		var mgr *mcpmgr.Manager
		if len(mcpConfigs) > 0 {
			var logf mcpmgr.LogFunc
			if verbose {
				logf = func(format string, args ...any) {
					chat.DimStyle.Fprintf(os.Stderr, format, args...)
				}
			}
			mgr, err = mcpmgr.NewManager(context.Background(), mcpConfigs, logf)
			if err != nil {
				return fmt.Errorf("MCP setup failed: %w", err)
			}
			defer mgr.Close()
		}

		// Non-interactive mode: single message, direct response
		if chatMessage != "" {
			return chat.Once(context.Background(), p, chatMessage, systemPrompt, mgr, os.Stdout)
		}

		// If no model specified, let user select from available models
		if model == "" {
			models, fetchErr := chat.FetchModels(context.Background(), p)
			if fetchErr != nil {
				return fmt.Errorf("failed to list models: %w", fetchErr)
			}
			if len(models) == 0 {
				return fmt.Errorf("no models available")
			}

			selected, err := chat.SelectModel(models)
			if err != nil {
				return fmt.Errorf("model selection cancelled: %w", err)
			}

			fmt.Printf("Using model: %s\n\n", chat.BoldStyle.Sprint(selected))
			// Recreate provider with selected model
			p, err = provider.New(providerType, apiKey, baseURL, selected, temp, httpClient)
			if err != nil {
				return err
			}
		}

		// Interactive system prompt input when -s is used without a value
		systemPrompt = strings.TrimSpace(systemPrompt)
		var importedHistory []provider.Message
		if cmd.Flags().Changed("system") && systemPrompt == "" {
			sp, imported, err := chat.ReadSystemPrompt(os.Stdout)
			if err != nil {
				return err
			}
			systemPrompt = sp
			importedHistory = imported
		}

		return chat.Run(p, systemPrompt, importedHistory, mgr, os.Stdout)
	},
}

func init() {
	rootCmd.Flags().StringVarP(&apiKey, "key", "k", "", "API key (required)")
	rootCmd.Flags().StringVarP(&baseURL, "url", "u", "", "Base URL (optional)")
	rootCmd.Flags().StringVarP(&model, "model", "M", "", "Model name (optional, interactive selection if omitted)")
	rootCmd.Flags().Float64VarP(&temperature, "temperature", "t", 0, "Sampling temperature (0.0-2.0)")
	rootCmd.Flags().StringVarP(&chatMessage, "message", "m", "", "Send a single message and print the response (non-interactive, use '-' to read from stdin)")
	rootCmd.Flags().StringVarP(&systemPrompt, "system", "s", "", "System prompt (omit value for interactive input)")
	rootCmd.Flags().Lookup("system").NoOptDefVal = " "
	rootCmd.Flags().BoolVarP(&list, "list", "l", false, "List configured providers, or models for a given provider")
	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Print request and response bodies for debugging")
	rootCmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to config file (default: ~/.chatchain.yaml)")
	rootCmd.Flags().StringArrayVar(&mcpFlags, "mcp", nil, "MCP server (command string or URL, repeatable)")
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
	"openclaw":      "OPENCLAW_GATEWAY_TOKEN",
}

func providerEnvKey(providerType string) string {
	if key, ok := providerEnvKeys[providerType]; ok {
		return key
	}
	return "API_KEY"
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

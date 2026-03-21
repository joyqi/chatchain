package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"chatchain/chat"
	"chatchain/config"
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
)

var rootCmd = &cobra.Command{
	Use:   "chatchain [openai|anthropic|gemini|vertexai|openresponses]",
	Short: "A lightweight cross-platform AI chat CLI",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.Load(configPath)
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

		// Non-interactive mode: single message, direct response
		if chatMessage != "" {
			return chat.Once(context.Background(), p, chatMessage, systemPrompt, os.Stdout)
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

		return chat.Run(p, systemPrompt, importedHistory, os.Stdout)
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
	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Print request and response bodies for debugging")
	rootCmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to config file (default: ~/.chatchain.yaml)")
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

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

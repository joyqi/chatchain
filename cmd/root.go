package cmd

import (
	"context"
	"fmt"
	"os"

	"chatchain/chat"
	"chatchain/provider"

	"github.com/spf13/cobra"
)

var (
	apiKey      string
	baseURL     string
	model       string
	temperature float64
	chatMessage string
)

var rootCmd = &cobra.Command{
	Use:   "chatchain [openai|anthropic|gemini]",
	Short: "A lightweight cross-platform AI chat CLI",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		providerType := args[0]

		if apiKey == "" {
			envKey := providerEnvKey(providerType)
			apiKey = os.Getenv(envKey)
			if apiKey == "" {
				return fmt.Errorf("API key is required: use -k/--key or set %s", envKey)
			}
		}

		// Non-interactive mode requires a model
		if chatMessage != "" && model == "" {
			return fmt.Errorf("--model/-m is required when using --chat/-c")
		}

		var temp *float64
		if cmd.Flags().Changed("temperature") {
			temp = &temperature
		}

		p, err := provider.New(providerType, apiKey, baseURL, model, temp)
		if err != nil {
			return err
		}

		// Non-interactive mode: single message, direct response
		if chatMessage != "" {
			return chat.Once(context.Background(), p, chatMessage, os.Stdout)
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
			p, err = provider.New(providerType, apiKey, baseURL, selected, temp)
			if err != nil {
				return err
			}
		}

		return chat.Run(p, os.Stdout)
	},
}

func init() {
	rootCmd.Flags().StringVarP(&apiKey, "key", "k", "", "API key (required)")
	rootCmd.Flags().StringVarP(&baseURL, "url", "u", "", "Base URL (optional)")
	rootCmd.Flags().StringVarP(&model, "model", "m", "", "Model name (optional, interactive selection if omitted)")
	rootCmd.Flags().Float64VarP(&temperature, "temperature", "t", 0, "Sampling temperature (0.0-2.0)")
	rootCmd.Flags().StringVarP(&chatMessage, "chat", "c", "", "Send a single message and print the response (non-interactive)")
}

var providerEnvKeys = map[string]string{
	"openai":    "OPENAI_API_KEY",
	"anthropic": "ANTHROPIC_API_KEY",
	"gemini":    "GOOGLE_API_KEY",
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

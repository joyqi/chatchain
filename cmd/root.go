package cmd

import (
	"context"
	"fmt"
	"os"

	"chatchain/chat"
	"chatchain/provider"

	"github.com/charmbracelet/huh/spinner"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

var (
	apiKey  string
	baseURL string
	model   string
)

var boldStyle = lipgloss.NewStyle().Bold(true)

var rootCmd = &cobra.Command{
	Use:   "chatchain [openai|anthropic]",
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

		p, err := provider.New(providerType, apiKey, baseURL, model)
		if err != nil {
			return err
		}

		// If no model specified, let user select from available models
		if model == "" {
			var models []string
			var fetchErr error

			err := spinner.New().
				Title("Fetching available models...").
				Action(func() {
					models, fetchErr = p.ListModels(context.Background())
				}).
				Run()
			if err != nil {
				return fmt.Errorf("spinner error: %w", err)
			}
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

			fmt.Printf("Using model: %s\n\n", boldStyle.Render(selected))
			// Recreate provider with selected model
			p, err = provider.New(providerType, apiKey, baseURL, selected)
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
}

var providerEnvKeys = map[string]string{
	"openai":    "OPENAI_API_KEY",
	"anthropic": "ANTHROPIC_API_KEY",
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

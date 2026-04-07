package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ProviderConfig holds per-provider settings from the config file.
type ProviderConfig struct {
	Type   string `yaml:"type"`
	Key    string `yaml:"key"`
	URL    string `yaml:"url"`
	Model  string `yaml:"model"`
	System string `yaml:"system"`
}

// MCPServerConfig holds settings for an MCP tool server.
type MCPServerConfig struct {
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	URL     string            `yaml:"url"`
	Env     map[string]string `yaml:"env"`
}

// Config is the top-level config file structure.
type Config struct {
	Providers  map[string]ProviderConfig  `yaml:"providers"`
	MCPServers map[string]MCPServerConfig `yaml:"mcp_servers"`
}

// Load reads and merges config files. Priority: explicitPath > local > global.
// Returns a non-nil Config even on errors (empty config).
func Load(explicitPath string) *Config {
	cfg := &Config{Providers: make(map[string]ProviderConfig)}

	if explicitPath != "" {
		cfg.loadFile(explicitPath)
		return cfg
	}

	// Global: ~/.chatchain.yaml / .yml
	if home, err := os.UserHomeDir(); err == nil {
		if f := findConfigFile(home); f != "" {
			cfg.loadFile(f)
		}
	}

	// Local: ./.chatchain.yaml / .yml
	if wd, err := os.Getwd(); err == nil {
		if f := findConfigFile(wd); f != "" {
			cfg.loadFile(f)
		}
	}

	return cfg
}

// Get resolves a provider name to its underlying type and config.
// If the config has a Type field, that is returned; otherwise name is used as the type.
func (c *Config) Get(name string) (providerType string, pc ProviderConfig) {
	pc, ok := c.Providers[name]
	if !ok {
		return name, ProviderConfig{}
	}
	providerType = pc.Type
	if providerType == "" {
		providerType = name
	}
	return providerType, pc
}

// findConfigFile looks for .chatchain.yaml then .chatchain.yml in dir.
func findConfigFile(dir string) string {
	for _, ext := range []string{".yaml", ".yml"} {
		p := filepath.Join(dir, ".chatchain"+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// loadFile reads a single config file and merges its entries into c.
func (c *Config) loadFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var fc Config
	if err := yaml.Unmarshal(data, &fc); err != nil {
		return
	}
	for name, pc := range fc.Providers {
		c.Providers[name] = pc
	}
	if c.MCPServers == nil && len(fc.MCPServers) > 0 {
		c.MCPServers = make(map[string]MCPServerConfig)
	}
	for name, sc := range fc.MCPServers {
		c.MCPServers[name] = sc
	}
}

package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"chatchain/internal/vars"
)

// ProviderConfig holds per-provider settings from the config file.
type ProviderConfig struct {
	Type  string `yaml:"type"`
	Key   string `yaml:"key"`
	URL   string `yaml:"url"`
	Model string `yaml:"model"`
	// System is an inline system prompt; SystemFile reads it from a file
	// instead (inline wins when both are set). Key, URL, and SystemFile
	// expand ${…} variables (internal/vars): ${userHome}, ${appHome},
	// ${cwd}, ${env:VAR}, …
	System     string `yaml:"system"`
	SystemFile string `yaml:"system_file"`
	// Effort is the default reasoning-effort level for providers that
	// support it: low|medium|high|xhigh|max ("" = provider default). A
	// resumed session's own effort still overrides it.
	Effort string `yaml:"effort"`
	// NoSave starts sessions ephemeral (the --no-save behavior): nothing
	// persists unless the user runs /save mid-chat. --resume overrides it.
	NoSave bool `yaml:"no_save"`
	// MCPServers selects which of the top-level mcp_servers this provider
	// loads, by name. nil (key absent) = all of them; an empty list = none;
	// names must exist in the top-level map (a typo fails loudly).
	MCPServers []string `yaml:"mcp_servers"`
	// Agent enables agent mode for this provider (docs/design/agent-mode.md).
	// yaml.v3 decodes the YAML 1.1 truthy spellings (true/yes/on) natively.
	Agent         bool   `yaml:"agent"`
	ContextWindow string `yaml:"context_window"` // e.g. "200k", "1m", "128000"
	// Tools enables built-in toolsets for this provider. The key is the set
	// name (its presence enables every tool in the set); the value is the
	// set's shared raw config, decoded lazily by the set itself (an empty/null
	// value means defaults). Sets and their value shapes: "shell" (bash; a
	// mapping with sandbox/network/auto_run/write), "agent" (load_skill; no
	// settings yet), and "code" (glob/grep/list_dir/read_file/edit_file/
	// write_file; a mapping with auto_write).
	Tools map[string]yaml.Node `yaml:"tools"`
}

// MCPServerConfig holds settings for an MCP tool server.
type MCPServerConfig struct {
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	URL     string            `yaml:"url"`
	Env     map[string]string `yaml:"env"`
	Headers map[string]string `yaml:"headers"`
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
		pc.Key = vars.Expand(pc.Key)
		pc.URL = vars.Expand(pc.URL)
		pc.SystemFile = vars.Expand(pc.SystemFile)
		c.Providers[name] = pc
	}
	if c.MCPServers == nil && len(fc.MCPServers) > 0 {
		c.MCPServers = make(map[string]MCPServerConfig)
	}
	for name, sc := range fc.MCPServers {
		c.MCPServers[name] = sc
	}
}

// ResolveSystem returns the provider's system prompt: the inline System text,
// or the contents of SystemFile when only the file is configured. A
// configured file that cannot be read is a hard error — a silently empty
// system prompt is worse than failing loudly.
func (pc ProviderConfig) ResolveSystem() (string, error) {
	if pc.System != "" || pc.SystemFile == "" {
		return pc.System, nil
	}
	data, err := os.ReadFile(pc.SystemFile)
	if err != nil {
		return "", fmt.Errorf("system_file: %w", err)
	}
	return string(data), nil
}

// MCPServersFor returns the top-level MCP servers the given provider config
// selects: all of them when the provider's mcp_servers key is absent, none
// for an explicit empty list, and exactly the named subset otherwise — an
// unknown name is an error, not a silent skip.
func (c *Config) MCPServersFor(pc ProviderConfig) (map[string]MCPServerConfig, error) {
	if pc.MCPServers == nil {
		return c.MCPServers, nil
	}
	out := make(map[string]MCPServerConfig, len(pc.MCPServers))
	for _, name := range pc.MCPServers {
		sc, ok := c.MCPServers[name]
		if !ok {
			return nil, fmt.Errorf("mcp_servers: %q is not defined under the top-level mcp_servers", name)
		}
		out[name] = sc
	}
	return out, nil
}

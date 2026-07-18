package mcp

import "chatchain/internal/vars"

// expandServerConfig returns a copy of cfg with ${…} variables expanded in
// all string fields (command, args, url, env values, header values) — the
// shared resolver in internal/vars, so MCP definitions and provider config
// speak one variable syntax.
func expandServerConfig(cfg ServerConfig) ServerConfig {
	out := ServerConfig{
		Name:    cfg.Name,
		Command: vars.Expand(cfg.Command),
		URL:     vars.Expand(cfg.URL),
	}
	if len(cfg.Args) > 0 {
		out.Args = make([]string, len(cfg.Args))
		for i, a := range cfg.Args {
			out.Args[i] = vars.Expand(a)
		}
	}
	if len(cfg.Env) > 0 {
		out.Env = make(map[string]string, len(cfg.Env))
		for k, v := range cfg.Env {
			out.Env[k] = vars.Expand(v)
		}
	}
	if len(cfg.Headers) > 0 {
		out.Headers = make(map[string]string, len(cfg.Headers))
		for k, v := range cfg.Headers {
			out.Headers[k] = vars.Expand(v)
		}
	}
	return out
}

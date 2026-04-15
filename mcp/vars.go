package mcp

import (
	"os"
	"regexp"
	"strings"
)

// varPattern matches ${name} or ${env:NAME} variable references.
var varPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// expandVars resolves VS Code-style predefined variables in a string.
// Supported:
//
//	${workspaceFolder}, ${cwd}  — current working directory
//	${userHome}                  — user home directory
//	${pathSeparator}, ${/}       — OS path separator
//	${env:VAR}                   — environment variable VAR
//
// Unknown variables are left untouched.
func expandVars(s string) string {
	if s == "" || !strings.Contains(s, "${") {
		return s
	}
	return varPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-1] // strip ${ and }
		if val, ok := resolveVar(name); ok {
			return val
		}
		return match
	})
}

func resolveVar(name string) (string, bool) {
	if strings.HasPrefix(name, "env:") {
		return os.Getenv(name[4:]), true
	}
	switch name {
	case "workspaceFolder", "cwd":
		if wd, err := os.Getwd(); err == nil {
			return wd, true
		}
	case "userHome":
		if home, err := os.UserHomeDir(); err == nil {
			return home, true
		}
	case "pathSeparator", "/":
		return string(os.PathSeparator), true
	}
	return "", false
}

// expandServerConfig returns a copy of cfg with variables expanded in all
// string fields (command, args, url, env values, header values).
func expandServerConfig(cfg ServerConfig) ServerConfig {
	out := ServerConfig{
		Name:    cfg.Name,
		Command: expandVars(cfg.Command),
		URL:     expandVars(cfg.URL),
	}
	if len(cfg.Args) > 0 {
		out.Args = make([]string, len(cfg.Args))
		for i, a := range cfg.Args {
			out.Args[i] = expandVars(a)
		}
	}
	if len(cfg.Env) > 0 {
		out.Env = make(map[string]string, len(cfg.Env))
		for k, v := range cfg.Env {
			out.Env[k] = expandVars(v)
		}
	}
	if len(cfg.Headers) > 0 {
		out.Headers = make(map[string]string, len(cfg.Headers))
		for k, v := range cfg.Headers {
			out.Headers[k] = expandVars(v)
		}
	}
	return out
}

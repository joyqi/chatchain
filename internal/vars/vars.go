// Package vars resolves VS Code-style ${…} variables in configuration
// strings. Both the provider config (key/url/system_file) and the MCP server
// definitions expand through it, so one syntax works everywhere.
package vars

import (
	"os"
	"regexp"
	"strings"

	"github.com/joyqi/iota/internal/app"
)

// pattern matches ${name} or ${env:NAME} variable references.
var pattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// Expand resolves predefined variables in a string. Supported:
//
//	${workspaceFolder}, ${cwd}  — current working directory
//	${userHome}                 — user home directory
//	${appHome}            — iota's global directory (~/.iota)
//	${pathSeparator}, ${/}      — OS path separator
//	${env:VAR}                  — environment variable VAR
//
// Unknown variables are left untouched.
func Expand(s string) string {
	if s == "" || !strings.Contains(s, "${") {
		return s
	}
	return pattern.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-1] // strip ${ and }
		if val, ok := resolve(name); ok {
			return val
		}
		return match
	})
}

func resolve(name string) (string, bool) {
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
	case "appHome":
		if dir, err := app.Home(); err == nil {
			return dir, true
		}
	case "pathSeparator", "/":
		return string(os.PathSeparator), true
	}
	return "", false
}

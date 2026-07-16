package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"chatchain/internal/shell"
	"chatchain/provider"

	"gopkg.in/yaml.v3"
)

// The "shell" toolset: one bash tool running real shell command lines. The
// execution and sandboxing mechanics live in internal/shell (Seatbelt on
// macOS, bubblewrap on Linux — writes confined to the project root plus
// temp/cache directories, network blocked by default); this layer holds the
// policy: configuration, the approval contract, and the model-facing tool
// surface. Sandboxed calls run without prompting; where no sandbox is
// available (or it is configured off), every call goes through the
// interactive approval gate instead (tool.ApprovalReporter).

// bashTimeout is the hard cap on a single bash execution. A call can also be
// cancelled earlier (ESC) via the context passed to Call.
const bashTimeout = 10 * time.Minute

// shellConfig is the set's shared config.
type shellConfig struct {
	// Sandbox selects the isolation mode: "auto" (default — sandbox when the
	// platform supports it) or "off".
	Sandbox string `yaml:"sandbox"`
	// Network allows outbound network access inside the sandbox (default:
	// blocked). Unsandboxed runs always have network.
	Network bool `yaml:"network"`
	// AutoRun skips the interactive approval for unsandboxed calls (and
	// permits them in non-interactive -m runs).
	AutoRun bool `yaml:"auto_run"`
	// Write lists extra sandbox-writable paths beyond the project root and
	// the temp/cache directories (e.g. a shared build dir). ~ expands.
	Write []string `yaml:"write"`
}

// newShellSet builds the "shell" toolset.
func newShellSet(env Env, node yaml.Node) ([]Tool, error) {
	cfg := shellConfig{Sandbox: "auto"}
	if !node.IsZero() {
		if err := node.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("config must be a mapping (sandbox, network, auto_run, write): %w", err)
		}
	}
	switch cfg.Sandbox {
	case "":
		cfg.Sandbox = "auto"
	case "auto", "off":
	default:
		return nil, fmt.Errorf("sandbox must be \"auto\" or \"off\", got %q", cfg.Sandbox)
	}

	root := env.ProjectRoot
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = cwd
		}
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return []Tool{&bashTool{
		cfg:       cfg,
		root:      root,
		sandboxed: cfg.Sandbox == "auto" && shell.Available(),
	}}, nil
}

// bashTool runs shell command lines, sandboxed when the platform allows.
type bashTool struct {
	cfg       shellConfig
	root      string
	sandboxed bool
}

// RequiresApproval gates unsandboxed execution behind user consent; a
// sandboxed call is pre-contained and runs without prompting (the industry
// default — Claude Code and Codex behave the same way).
func (b *bashTool) RequiresApproval() bool { return !b.sandboxed && !b.cfg.AutoRun }

func (b *bashTool) Def() provider.ToolDef {
	desc := "Run a bash command line on the user's machine and return its combined stdout/stderr. " +
		"The full shell is available: pipes, redirects, globbing, && chaining, heredocs. " +
		"The working directory defaults to the project root (override with \"cwd\"). "
	if b.sandboxed {
		net := "network access is BLOCKED"
		if b.cfg.Network {
			net = "network access is allowed"
		}
		desc += fmt.Sprintf("Commands run inside an OS sandbox: file writes are confined to the project root "+
			"and temp/cache directories (writes elsewhere fail with permission errors), and %s.", net)
	} else {
		desc += "Commands run WITHOUT a sandbox on this system, with the user's full permissions — be conservative."
	}
	return provider.ToolDef{
		Name:        "bash",
		Description: desc,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Bash command line to execute, e.g. 'go test ./... 2>&1 | tail -20'.",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Optional working directory (defaults to the project root).",
				},
			},
			"required": []any{"command"},
		},
	}
}

func (b *bashTool) Call(ctx context.Context, args map[string]any) (string, bool, error) {
	command, _ := args["command"].(string)
	command = strings.TrimSpace(command)
	if command == "" {
		return "missing required argument: command", true, nil
	}

	cwd := b.root
	if arg, ok := args["cwd"].(string); ok && strings.TrimSpace(arg) != "" {
		cwd = filepath.FromSlash(strings.TrimSpace(arg))
		if !filepath.IsAbs(cwd) {
			cwd = filepath.Join(b.root, cwd)
		}
	}

	opts := shell.Options{Command: command, Dir: cwd, Timeout: bashTimeout}
	if b.sandboxed {
		var write []string
		for _, p := range b.cfg.Write {
			if p = expandHome(strings.TrimSpace(p)); p != "" {
				write = append(write, p)
			}
		}
		opts.Sandbox = &shell.Sandbox{Root: b.root, Network: b.cfg.Network, Write: write}
	}

	res := shell.Run(ctx, opts)
	switch {
	case res.Err != nil:
		if strings.TrimSpace(res.Output) == "" {
			return fmt.Sprintf("failed to run: %v", res.Err), true, nil
		}
		return fmt.Sprintf("%s\n[failed to run: %v]", res.Output, res.Err), true, nil
	case res.TimedOut:
		return fmt.Sprintf("%s\n[command timed out after %s]", res.Output, bashTimeout), true, nil
	case res.Cancelled:
		return fmt.Sprintf("%s\n[command cancelled]", res.Output), true, nil
	case res.ExitCode != 0:
		return fmt.Sprintf("%s\n[exit code %d]", res.Output, res.ExitCode), true, nil
	}
	if strings.TrimSpace(res.Output) == "" {
		return "[command produced no output]", false, nil
	}
	return res.Output, false, nil
}

// expandHome resolves a leading ~ to the user's home directory.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

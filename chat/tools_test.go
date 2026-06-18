package chat

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	"github.com/manifoldco/promptui"
)

// TestToolPanelTemplates renders the /tools panel templates the way promptui
// does (its FuncMap) so a typo or a missing color func is caught here rather than
// only surfacing inside the interactive panel at runtime.
func TestToolPanelTemplates(t *testing.T) {
	render := func(tmpl string, row toolRow) string {
		t.Helper()
		parsed, err := template.New("t").Funcs(promptui.FuncMap).Parse(tmpl)
		if err != nil {
			t.Fatalf("parse %q: %v", tmpl, err)
		}
		var b bytes.Buffer
		if err := parsed.Execute(&b, row); err != nil {
			t.Fatalf("exec %q: %v", tmpl, err)
		}
		return b.String()
	}

	builtin := toolRow{Name: "run_command", Source: "built-in", Desc: "Run a command", IsMCP: false}
	mcp := toolRow{Name: "read_file", Source: "mcp: filesystem", Desc: "Read a file", IsMCP: true}

	for _, tmpl := range []string{toolPanelActive, toolPanelInactive} {
		out := render(tmpl, builtin)
		if !strings.Contains(out, "run_command") || !strings.Contains(out, "[built-in]") || !strings.Contains(out, "Run a command") {
			t.Errorf("built-in row missing expected columns: %q", out)
		}

		out = render(tmpl, mcp)
		if !strings.Contains(out, "read_file") || !strings.Contains(out, "[mcp: filesystem]") || !strings.Contains(out, "Read a file") {
			t.Errorf("mcp row missing expected columns: %q", out)
		}
		// MCP rows must not be tagged built-in (the if/else branch picked right).
		if strings.Contains(out, "[built-in]") {
			t.Errorf("mcp row should not show [built-in]: %q", out)
		}
	}
}

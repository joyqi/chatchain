package chat

import "testing"

func TestDisplayToolName(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"mcp tool", "mcp__a__b", "a:b"},
		{"real-world", "mcp__chrome_devtools__take_screenshot", "chrome_devtools:take_screenshot"},
		// URL-named server: the segment is fully collapsed (no "__" runs), so
		// the first-"__" split lands on the real server/tool boundary.
		{"url-named server", "mcp__https_mcp_example_com_sse__search", "https_mcp_example_com_sse:search"},
		{"tool part keeps double underscore", "mcp__srv__take__screenshot", "srv:take__screenshot"},
		{"built-in unchanged", "run_command", "run_command"},
		{"raw name from old session unchanged", "get_file_contents", "get_file_contents"},
		{"degenerate empty server unchanged", "mcp____x", "mcp____x"},
		{"degenerate empty tool unchanged", "mcp__srv__", "mcp__srv__"},
		{"no second separator unchanged", "mcp__s", "mcp__s"},
		{"prefix only unchanged", "mcp__", "mcp__"},
		{"empty unchanged", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := displayToolName(tt.in); got != tt.want {
				t.Errorf("displayToolName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

package vars

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpand(t *testing.T) {
	home, _ := os.UserHomeDir()
	wd, _ := os.Getwd()
	t.Setenv("VARS_TEST_TOKEN", "sekrit")

	for in, want := range map[string]string{
		"":                                "",
		"plain":                           "plain",
		"${userHome}/x":                   filepath.Join(home, "x"),
		"${appHome}/sys.md":         filepath.Join(home, ".chatchain", "sys.md"),
		"${cwd}":                          wd,
		"${workspaceFolder}":              wd,
		"${env:VARS_TEST_TOKEN}":          "sekrit",
		"${unknownVar}":                   "${unknownVar}", // untouched
		"a${/}b":                          "a" + string(os.PathSeparator) + "b",
		"${userHome}${pathSeparator}deep": home + string(os.PathSeparator) + "deep",
	} {
		if got := Expand(in); got != want {
			t.Errorf("Expand(%q) = %q, want %q", in, got, want)
		}
	}

	if !strings.HasSuffix(Expand("${appHome}"), ".chatchain") {
		t.Error("appHome must end in .chatchain")
	}
}

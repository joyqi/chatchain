package ui

import (
	"errors"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

// copyToClipboard pipes text into the platform clipboard tool (the v1
// promptui implementation, ported): pbcopy on macOS; wl-copy/xclip/xsel on
// Linux; clip on Windows.
func copyToClipboard(text string) error {
	var candidates [][]string
	switch runtime.GOOS {
	case "darwin":
		candidates = [][]string{{"pbcopy"}}
	case "windows":
		candidates = [][]string{{"clip"}}
	default:
		candidates = [][]string{
			{"wl-copy"},
			{"xclip", "-selection", "clipboard"},
			{"xsel", "--clipboard", "--input"},
		}
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	}
	return errors.New("no clipboard tool found")
}

var sgrRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// stripSGRText removes SGR escapes so copied text is plain.
func stripSGRText(s string) string { return sgrRe.ReplaceAllString(s, "") }

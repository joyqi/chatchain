package promptui

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// copyFn performs the actual clipboard write; a package var so tests can stub it
// without clobbering the real system clipboard.
var copyFn = copyToClipboard

// copyToClipboard writes text to the system clipboard by piping it to the
// platform's clipboard utility (no external Go dependency). Returns an error if
// no supported utility is available or the pipe fails.
func copyToClipboard(text string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "windows":
		cmd = exec.Command("clip")
	default: // linux/bsd: try the common X11/Wayland tools in turn
		switch {
		case hasCmd("wl-copy"):
			cmd = exec.Command("wl-copy")
		case hasCmd("xclip"):
			cmd = exec.Command("xclip", "-selection", "clipboard")
		case hasCmd("xsel"):
			cmd = exec.Command("xsel", "--clipboard", "--input")
		default:
			return fmt.Errorf("no clipboard utility found (install wl-copy, xclip, or xsel)")
		}
	}
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}

func hasCmd(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// stripANSI removes ANSI CSI escape sequences from s, leaving plain text —
// suitable for the clipboard, where styling is noise. Non-escape bytes
// (including multi-byte UTF-8) are copied verbatim.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if n := ansiLen(s, i); n > 0 {
			i += n
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

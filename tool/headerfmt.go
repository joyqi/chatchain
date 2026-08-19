package tool

import (
	"os"
	"path/filepath"
	"strings"

	"chatchain/internal/textwidth"
)

// Display formatting shared by the toolsets' call headers (the headliner
// capability). Kept out of any one set: the file tools and bash both render
// paths, and one ladder is the point.

// headerPathMax is the display width a header path is truncated to. Long
// paths lose their HEAD, not their tail: the basename is the part that
// identifies the file, and a leading "…/" reads as "somewhere above".
const headerPathMax = 48

// headerPath renders a model-supplied path for the call header. The ladder is
// the one Codex settled on (codex-rs/tui/src/diff_render.rs), and each rung
// exists for a reason:
//
//  1. Already relative → verbatim. The model's own spelling matches what the
//     user would type next, and re-deriving it can only lose that.
//  2. Under cwd → cwd-relative. The common case.
//  3. Elsewhere under the project root → "../" form. Still nearby, and a
//     relative hop reads better than an absolute path (this is what a run
//     started in a subdirectory produces).
//  4. Under $HOME → "~/…". What pi and crush both do, and all we did before
//     was print the whole "/Users/…" string.
//  5. Otherwise → absolute.
func headerPath(raw, cwd, root string) string {
	p := strings.TrimSpace(raw)
	if p == "" {
		return ""
	}
	return elidePathHead(headerPathFull(p, cwd, root), headerPathMax)
}

func headerPathFull(p, cwd, root string) string {
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) {
		return filepath.ToSlash(filepath.Clean(p)) // 1: the model's own spelling
	}
	abs := filepath.Clean(p)
	if cwd != "" {
		if rel, err := filepath.Rel(cwd, abs); err == nil {
			if !strings.HasPrefix(rel, "..") {
				return filepath.ToSlash(rel) // 2: under cwd
			}
			if within(root, abs) {
				return filepath.ToSlash(rel) // 3: elsewhere in the project
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, abs); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + filepath.ToSlash(rel) // 4
		}
	}
	return filepath.ToSlash(abs) // 5
}

// within reports whether abs sits inside dir.
func within(dir, abs string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, abs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// elidePathHead trims a path from the FRONT to fit max display columns,
// keeping whole segments where it can so the result stays a readable path.
func elidePathHead(p string, max int) string {
	if textwidth.StringWidth(p) <= max {
		return p
	}
	segs := strings.Split(p, "/")
	for i := 1; i < len(segs); i++ {
		cand := ".../" + strings.Join(segs[i:], "/")
		if textwidth.StringWidth(cand) <= max {
			return cand
		}
	}
	// A single oversized segment (one very long file name): cut into it.
	last := segs[len(segs)-1]
	return "..." + truncateRunesFront(last, max-3)
}

// truncateRunesFront keeps the LAST n display columns of s.
func truncateRunesFront(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	for i := 0; i < len(r); i++ {
		if textwidth.StringWidth(string(r[i:])) <= n {
			return string(r[i:])
		}
	}
	return s
}

// headerCmdMax is the display width a command summary is truncated to. Far
// wider than a generic argument value: for bash the command IS the call, and
// the informative tail of "go test ./... 2>&1 | tail -20" is exactly what a
// 24-column budget used to cut off.
const headerCmdMax = 64

// headerCommand renders a shell command for a call header: the first line
// only (a multi-line script cannot be read on one row anyway, and flattening
// it just makes a smear), truncated from the end.
func headerCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	line, _, more := strings.Cut(cmd, "\n")
	line = strings.ReplaceAll(strings.TrimSpace(line), "\t", " ")
	if more {
		line += " …"
	}
	return truncateRunesTail(line, headerCmdMax)
}

// truncateRunesTail keeps the FIRST n display columns of s — the opposite end
// from truncateRunesFront, because a command reads left to right.
func truncateRunesTail(s string, n int) string {
	if textwidth.StringWidth(s) <= n {
		return s
	}
	r := []rune(s)
	for i := len(r); i > 0; i-- {
		if textwidth.StringWidth(string(r[:i])) <= n-1 {
			return string(r[:i]) + "…"
		}
	}
	return ""
}

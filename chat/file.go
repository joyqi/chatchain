package chat

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"chatchain/provider"
)

const maxFileSize = 20 * 1024 * 1024 // 20MB

var mimeTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
	".pdf":  "application/pdf",
	".txt":  "text/plain",
	".md":   "text/plain",
	".go":   "text/plain",
	".py":   "text/plain",
	".js":   "text/plain",
	".ts":   "text/plain",
	".jsx":  "text/plain",
	".tsx":  "text/plain",
	".java": "text/plain",
	".c":    "text/plain",
	".cpp":  "text/plain",
	".h":    "text/plain",
	".rs":   "text/plain",
	".rb":   "text/plain",
	".sh":   "text/plain",
	".yaml": "text/plain",
	".yml":  "text/plain",
	".json": "text/plain",
	".xml":  "text/plain",
	".html": "text/plain",
	".css":  "text/plain",
	".sql":  "text/plain",
	".csv":  "text/plain",
	".log":  "text/plain",
	".toml": "text/plain",
	".ini":  "text/plain",
	".cfg":  "text/plain",
	".conf": "text/plain",
}

func DetectMimeType(path string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if mime, ok := mimeTypes[ext]; ok {
		return mime, nil
	}
	return "", fmt.Errorf("unsupported file type: %s", ext)
}

func ReadAttachment(path string) (provider.Attachment, error) {
	// Expand ~
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return provider.Attachment{}, fmt.Errorf("cannot resolve home dir: %w", err)
		}
		path = filepath.Join(home, path[2:])
	}

	info, err := os.Stat(path)
	if err != nil {
		return provider.Attachment{}, fmt.Errorf("cannot access file: %w", err)
	}
	if info.IsDir() {
		return provider.Attachment{}, fmt.Errorf("path is a directory, not a file")
	}
	if info.Size() > maxFileSize {
		return provider.Attachment{}, fmt.Errorf("file too large: %d bytes (max %d)", info.Size(), maxFileSize)
	}

	mimeType, err := DetectMimeType(path)
	if err != nil {
		return provider.Attachment{}, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return provider.Attachment{}, fmt.Errorf("cannot read file: %w", err)
	}

	return provider.Attachment{
		Filename: filepath.Base(path),
		MimeType: mimeType,
		Data:     data,
	}, nil
}

func humanSize(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func attachmentLabel(a provider.Attachment) string {
	return fmt.Sprintf("%s (%s, %s)", a.Filename, a.MimeType, humanSize(len(a.Data)))
}

// pickFile opens an interactive directory browser starting at the working
// directory: "../" and subdirectories navigate, a file selects and returns its
// path. Returns "" if cancelled. Hidden entries are skipped. Reuses
// cancelableSelect, so Esc/Ctrl+C cancel cleanly with no residue.
func pickFile() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		dir, _ = os.UserHomeDir()
	}
	type fsItem struct {
		label string
		path  string
		isDir bool
	}
	for {
		entries, rerr := os.ReadDir(dir)
		if rerr != nil {
			return "", rerr
		}
		var items []fsItem
		if parent := filepath.Dir(dir); parent != dir {
			items = append(items, fsItem{"../", parent, true})
		}
		var dirs, files []fsItem
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") {
				continue // skip hidden entries
			}
			p := filepath.Join(dir, name)
			if e.IsDir() {
				dirs = append(dirs, fsItem{name + "/", p, true})
			} else {
				files = append(files, fsItem{name, p, false})
			}
		}
		items = append(items, dirs...)
		items = append(items, files...)

		labels := make([]string, len(items))
		for i, it := range items {
			labels[i] = it.label
		}
		prompt := cancelableSelect("Attach a file · "+dir, labels, 15)
		idx, _, perr := prompt.Run()
		if perr != nil || idx == 0 {
			return "", nil // cancelled
		}
		chosen := items[idx-1]
		if chosen.isDir {
			dir = chosen.path
			continue
		}
		return chosen.path, nil
	}
}

// cleanAttachments shows the pending attachments as a multi-select and removes
// the chosen ones, returning the kept set. Cancelling leaves them unchanged.
func cleanAttachments(w io.Writer, pending []provider.Attachment) []provider.Attachment {
	if len(pending) == 0 {
		DimStyle.Fprintln(w, "No attachments.")
		return pending
	}
	rows := make([]string, len(pending))
	for i, a := range pending {
		rows[i] = attachmentLabel(a)
	}
	idxs, ok := multiSelect("Attachments — Space toggles · Enter removes · Esc cancels", rows)
	if !ok || len(idxs) == 0 {
		return pending
	}
	remove := make(map[int]bool, len(idxs))
	for _, i := range idxs {
		remove[i] = true
	}
	var kept []provider.Attachment
	for i, a := range pending {
		if !remove[i] {
			kept = append(kept, a)
		}
	}
	DimStyle.Fprintf(w, "Removed %d attachment(s).\n", len(idxs))
	return kept
}

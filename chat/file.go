package chat

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"chatchain/internal/promptui"
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

// manageAttachments opens a two-tab selector over the pending attachments: the
// "Attached" tab multi-selects attached files to remove, the "Add" tab is a
// directory browser to add one. It returns the resulting attachment set; on
// cancel (Esc/Ctrl+C) the set is unchanged. The two panels' logic used to live
// in cleanAttachments and pickFile respectively.
func manageAttachments(w io.Writer, pending []provider.Attachment) []provider.Attachment {
	rows := make([]string, len(pending))
	for i, a := range pending {
		rows[i] = attachmentLabel(a)
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd, _ = os.UserHomeDir()
	}

	attached := promptui.NewListPanel("Attached", rows, true)
	attached.RuneWidth = runeWidth
	browser := promptui.NewBrowserPanel("Add", cwd)
	browser.RuneWidth = runeWidth

	tb := &promptui.Tabbed{
		Panels:    []promptui.Panel{attached, browser},
		RuneWidth: runeWidth,
	}
	focused, rerr := tb.Run()
	if rerr != nil {
		return pending // cancelled — leave the set as is
	}

	switch focused {
	case 0: // Attached: remove the checked attachments
		idxs := attached.Selected()
		if len(idxs) == 0 {
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
	case 1: // Add: attach the browsed file
		path := browser.Chosen()
		if path == "" {
			return pending
		}
		att, aerr := ReadAttachment(path)
		if aerr != nil {
			ErrorStyle.Fprintf(w, "Error: %v\n", aerr)
			return pending
		}
		DimStyle.Fprintf(w, "Attached: %s (%s, %d bytes)\n", att.Filename, att.MimeType, len(att.Data))
		return append(pending, att)
	}
	return pending
}

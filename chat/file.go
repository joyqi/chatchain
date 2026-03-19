package chat

import (
	"fmt"
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

func SaveHistory(messages []provider.Message, path string) error {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot resolve home dir: %w", err)
		}
		path = filepath.Join(home, path[2:])
	}

	var b strings.Builder
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			fmt.Fprintf(&b, "System> %s\n\n", msg.Content)
		case "user":
			fmt.Fprintf(&b, "You> %s\n", msg.Content)
			for _, att := range msg.Attachments {
				fmt.Fprintf(&b, "  [Attached: %s (%s)]\n", att.Filename, att.MimeType)
			}
			b.WriteString("\n")
		case "assistant":
			if msg.Reasoning != "" {
				fmt.Fprintf(&b, "Reasoning> %s\n\n", msg.Reasoning)
			}
			fmt.Fprintf(&b, "Assistant> %s\n\n", msg.Content)
		}
	}

	if err := os.WriteFile(path, []byte(b.String()), 0644); err != nil {
		return fmt.Errorf("cannot write file: %w", err)
	}
	return nil
}

func FormatAttachmentList(attachments []provider.Attachment) string {
	if len(attachments) == 0 {
		return "No files attached."
	}
	var b strings.Builder
	for i, a := range attachments {
		size := len(a.Data)
		var sizeStr string
		switch {
		case size >= 1024*1024:
			sizeStr = fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
		case size >= 1024:
			sizeStr = fmt.Sprintf("%.1f KB", float64(size)/1024)
		default:
			sizeStr = fmt.Sprintf("%d B", size)
		}
		fmt.Fprintf(&b, "  [%d] %s (%s, %s)\n", i+1, a.Filename, a.MimeType, sizeStr)
	}
	return b.String()
}

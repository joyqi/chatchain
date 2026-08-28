package chat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joyqi/iota/provider"
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

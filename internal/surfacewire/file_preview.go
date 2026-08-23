package surfacewire

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cat3399/pi-go/internal/application"
)

const (
	TextPreviewMaxBytes     int64 = 256 * 1024
	ImagePreviewMaxBytes    int64 = 10 * 1024 * 1024
	DOCXPreviewMaxBytes     int64 = 10 * 1024 * 1024
	EmbeddedPreviewMaxBytes int64 = 32 * 1024 * 1024
)

type FilePreview struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	MIMEType  string `json:"mimeType"`
	Language  string `json:"language"`
	Size      int64  `json:"size"`
	Content   string `json:"content,omitempty"`
	SourceURL string `json:"sourceUrl,omitempty"`
}

var fileLanguageByExtension = map[string]string{
	"ts": "typescript", "tsx": "typescript", "js": "javascript", "jsx": "javascript",
	"mjs": "javascript", "cjs": "javascript", "py": "python", "rb": "ruby",
	"go": "go", "rs": "rust", "java": "java", "kt": "kotlin", "swift": "swift",
	"c": "c", "cpp": "cpp", "h": "c", "hpp": "cpp", "cs": "csharp",
	"html": "html", "htm": "html", "css": "css", "scss": "css", "less": "css",
	"json": "json", "jsonl": "json", "yaml": "yaml", "yml": "yaml",
	"toml": "toml", "xml": "xml", "md": "markdown", "mdx": "markdown",
	"sh": "bash", "bash": "bash", "zsh": "bash", "fish": "bash",
	"sql": "sql", "graphql": "graphql", "gql": "graphql",
	"dockerfile": "dockerfile", "tf": "hcl", "hcl": "hcl",
	"env": "bash", "gitignore": "bash", "txt": "text", "pdf": "pdf", "docx": "word",
}

var imageMIMEByExtension = map[string]string{
	"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "gif": "image/gif",
	"webp": "image/webp", "svg": "image/svg+xml", "bmp": "image/bmp", "ico": "image/x-icon", "avif": "image/avif",
}

var audioMIMEByExtension = map[string]string{
	"mp3": "audio/mpeg", "wav": "audio/wav", "ogg": "audio/ogg", "oga": "audio/ogg",
	"opus": "audio/ogg", "m4a": "audio/mp4", "aac": "audio/aac", "flac": "audio/flac",
	"weba": "audio/webm", "webm": "audio/webm",
}

var documentMIMEByExtension = map[string]string{
	"pdf":  "application/pdf",
	"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
}

func FileExtension(path string) string {
	base := strings.ToLower(filepath.Base(path))
	return strings.TrimPrefix(filepath.Ext(base), ".")
}

func FileLanguage(path string) string {
	base := strings.ToLower(filepath.Base(path))
	if base == "dockerfile" || strings.HasPrefix(base, "dockerfile.") {
		return "dockerfile"
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return "bash"
	}
	if base == "makefile" || base == "gnumakefile" {
		return "makefile"
	}
	if language := fileLanguageByExtension[FileExtension(path)]; language != "" {
		return language
	}
	return "text"
}

func ImageMIME(path string) string { return imageMIMEByExtension[FileExtension(path)] }

func AudioMIME(path string) string { return audioMIMEByExtension[FileExtension(path)] }

func DocumentMIME(path string) string { return documentMIMEByExtension[FileExtension(path)] }

func FileMIME(path string) string {
	if value := ImageMIME(path); value != "" {
		return value
	}
	if value := AudioMIME(path); value != "" {
		return value
	}
	if value := DocumentMIME(path); value != "" {
		return value
	}
	return "application/octet-stream"
}

func PreviewFile(ctx context.Context, api application.API, requested string) (FilePreview, error) {
	if api == nil {
		return FilePreview{}, errors.New("application API is required")
	}
	resource, err := api.ResolveFile(ctx, requested, "")
	if err != nil {
		return FilePreview{}, err
	}
	if !resource.IsFile {
		return FilePreview{}, application.ErrNotFile
	}

	preview := FilePreview{
		Path: resource.Path, Name: resource.Name, Language: FileLanguage(resource.Path),
		MIMEType: FileMIME(resource.Path), Size: resource.Size,
	}
	switch {
	case FileExtension(resource.Path) == "docx":
		if resource.Size > DOCXPreviewMaxBytes {
			return FilePreview{}, errors.New("DOCX too large for preview (>10MB)")
		}
		preview.Kind = "docx"
		preview.Content, err = RenderDOCXPreview(resource.Path, resource.Name)
		return preview, err
	case DocumentMIME(resource.Path) == "application/pdf":
		preview.Kind = "pdf"
		preview.SourceURL, err = previewDataURL(resource.Path, preview.MIMEType, EmbeddedPreviewMaxBytes)
		return preview, err
	case ImageMIME(resource.Path) != "":
		preview.Kind = "image"
		preview.SourceURL, err = previewDataURL(resource.Path, preview.MIMEType, ImagePreviewMaxBytes)
		return preview, err
	case AudioMIME(resource.Path) != "":
		preview.Kind = "audio"
		preview.SourceURL, err = previewDataURL(resource.Path, preview.MIMEType, EmbeddedPreviewMaxBytes)
		return preview, err
	default:
		preview.Kind = "text"
		preview.MIMEType = "text/plain"
		data, readErr := readPreviewBytes(resource.Path, TextPreviewMaxBytes)
		if readErr != nil {
			return FilePreview{}, readErr
		}
		preview.Content = string(data)
		return preview, nil
	}
}

func previewDataURL(path, mimeType string, limit int64) (string, error) {
	data, err := readPreviewBytes(path, limit)
	if err != nil {
		return "", err
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func readPreviewBytes(path string, limit int64) ([]byte, error) {
	handle, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	data, err := io.ReadAll(io.LimitReader(handle, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file too large for embedded preview (>%s)", formatPreviewLimit(limit))
	}
	return data, nil
}

func formatPreviewLimit(limit int64) string {
	if limit < 1024*1024 {
		return fmt.Sprintf("%dKB", limit/1024)
	}
	return fmt.Sprintf("%dMB", limit/(1024*1024))
}

package surfacewire

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/application"
)

type filePreviewAPI struct {
	application.API
	resource application.FileResource
	err      error
}

func (api filePreviewAPI) ResolveFile(context.Context, string, string) (application.FileResource, error) {
	return api.resource, api.err
}

func TestPreviewFileTextAndImage(t *testing.T) {
	root := t.TempDir()
	markdownPath := filepath.Join(root, "README.md")
	markdown := []byte("# Preview\n\nShared workbench.")
	if err := os.WriteFile(markdownPath, markdown, 0o600); err != nil {
		t.Fatal(err)
	}

	text, err := PreviewFile(context.Background(), previewAPIForPath(t, markdownPath), markdownPath)
	if err != nil {
		t.Fatal(err)
	}
	if text.Kind != "text" || text.Language != "markdown" || text.MIMEType != "text/plain" || text.Content != string(markdown) {
		t.Fatalf("text preview = %#v", text)
	}

	imagePath := filepath.Join(root, "pixel.png")
	image := []byte{0x89, 0x50, 0x4e, 0x47}
	if err := os.WriteFile(imagePath, image, 0o600); err != nil {
		t.Fatal(err)
	}
	media, err := PreviewFile(context.Background(), previewAPIForPath(t, imagePath), imagePath)
	if err != nil {
		t.Fatal(err)
	}
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(image)
	if media.Kind != "image" || media.MIMEType != "image/png" || media.SourceURL != wantURL || media.Content != "" {
		t.Fatalf("image preview = %#v", media)
	}
}

func TestPreviewFileRejectsDirectory(t *testing.T) {
	_, err := PreviewFile(context.Background(), filePreviewAPI{
		resource: application.FileResource{Path: t.TempDir(), Name: "folder", IsDir: true},
	}, "folder")
	if !errors.Is(err, application.ErrNotFile) {
		t.Fatalf("error = %v, want ErrNotFile", err)
	}
}

func TestPreviewFileEnforcesTextLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(TextPreviewMaxBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := PreviewFile(context.Background(), previewAPIForPath(t, path), path)
	if err == nil || !strings.Contains(err.Error(), ">256KB") {
		t.Fatalf("error = %v, want 256KB preview limit", err)
	}
}

func previewAPIForPath(t *testing.T, path string) filePreviewAPI {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return filePreviewAPI{resource: application.FileResource{
		Path: path, Name: info.Name(), Size: info.Size(), Modified: time.Now(), IsFile: true,
	}}
}

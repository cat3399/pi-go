//go:build pi_go_webui

package web

import (
	"io/fs"
	"testing"
)

func TestExportContainsApplicationAndVisualAssets(t *testing.T) {
	assets, err := EmbeddedAssets()
	if err != nil {
		t.Fatalf("EmbeddedAssets: %v", err)
	}
	for _, name := range []string{"index.html", "manifest.webmanifest", "sw.js", "icons/icon-192.png"} {
		info, err := fs.Stat(assets, name)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.IsDir() || info.Size() == 0 {
			t.Fatalf("asset %s is not a non-empty file", name)
		}
	}
	css, err := fs.Glob(assets, "_next/static/css/*.css")
	if err != nil || len(css) == 0 {
		t.Fatalf("exported CSS = %v, %v", css, err)
	}
}

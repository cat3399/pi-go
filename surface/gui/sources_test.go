package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cat3399/pi-go/internal/installation"
)

func TestGUISourcesInstallTogetherWithCoreDocumentation(t *testing.T) {
	docs, err := installation.InstallKnowledge(context.Background(), filepath.Join(t.TempDir(), "agent"), []installation.SourceBundle{{Prefix: "surface/gui", Files: guiSources}})
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"docs/gui.md", "internal/agent/agent.go", "surface/gui/main.go", "surface/gui/bridge.go", "surface/gui/THIRD_PARTY_NOTICES.md", "surface/gui/frontend/package.json"} {
		data, err := os.ReadFile(filepath.Join(docs.SourcePath, filepath.FromSlash(relative)))
		if err != nil || len(data) == 0 {
			t.Fatalf("missing GUI installation source %s: %v", relative, err)
		}
	}
}

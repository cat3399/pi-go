package application

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
)

func TestBrowseDirectoriesResolvesRealPathAndListsOnlyDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "beta"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "Alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ServiceOptions{
		Production:    app.ProductionConfig{WorkingDir: root, AgentDir: filepath.Join(t.TempDir(), "agent"), Environment: []string{}},
		DisableReaper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	result, err := service.BrowseDirectories(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != realRoot || result.ParentPath == nil || len(result.Directories) != 2 || result.Directories[0].Name != "Alpha" || result.Directories[1].Name != "beta" {
		t.Fatalf("browse result = %#v", result)
	}
}

package installation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func TestKnowledgeIsVersionedAndPreservesCustomization(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	first, err := InstallKnowledge(ctx, agentDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{first.ReadmePath, filepath.Join(first.DocsPath, "providers.md"), filepath.Join(first.SourcePath, "internal", "agent", "agent.go"), filepath.Join(first.SourcePath, "sources.go")} {
		data, err := os.ReadFile(path)
		if err != nil || len(data) == 0 {
			t.Fatalf("installed file %s: %v", path, err)
		}
		if !filepath.IsAbs(path) || !strings.HasPrefix(path, root+string(filepath.Separator)) {
			t.Fatalf("file is outside this installation: %s", path)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0o200 == 0 {
			t.Fatalf("installed file is not writable: %s / %v", path, err)
		}
	}
	customized := map[string]string{
		first.ReadmePath: "locally customized documentation",
		filepath.Join(first.DocsPath, "local.md"):        "additional user documentation",
		filepath.Join(first.SourcePath, "manifest.json"): "user-maintained metadata",
	}
	for path, content := range customized {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(first.DocsPath, "providers.md")
	if err := os.Rename(missing, filepath.Join(root, "providers.md")); err != nil {
		t.Fatal(err)
	}
	// Reusing local files must not wait for or write an installation lock.
	release, err := lock(ctx, filepath.Join(root, "knowledge", ".install.lock"))
	if err != nil {
		t.Fatal(err)
	}
	reuseContext, cancel := context.WithTimeout(ctx, time.Second)
	again, err := InstallKnowledge(reuseContext, agentDir, nil)
	release()
	cancel()
	if err != nil || again != first {
		t.Fatalf("customized installation was not reused: %v", err)
	}
	for path, expected := range customized {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != expected {
			t.Fatalf("local changes were not retained: %s / %v", path, err)
		}
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file moved by the user was restored: %v", err)
	}
	if archives, err := filepath.Glob(filepath.Join(root, "knowledge", ".superseded-*")); err != nil || len(archives) != 0 {
		t.Fatalf("customized installation was archived: %v / %v", archives, err)
	}
	variant, err := InstallKnowledge(ctx, agentDir, []SourceBundle{{Prefix: "surface/example", Files: fstest.MapFS{"main.go": {Data: []byte("package example\n")}}}})
	if err != nil || variant.BuildID == first.BuildID {
		t.Fatalf("source change did not identify a new build: %v", err)
	}
	if data, err := os.ReadFile(first.ReadmePath); err != nil || string(data) != customized[first.ReadmePath] {
		t.Fatal("a new build displaced the user's changes", err)
	}
	if _, err := InstallKnowledge(ctx, agentDir, []SourceBundle{{Files: fstest.MapFS{"README.md": {Data: []byte("collision")}}}}); err == nil {
		t.Fatal("conflicting source bundles were accepted")
	}
}

func TestConcurrentKnowledgeInstallationPublishesOnce(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	var group sync.WaitGroup
	results := make(chan error, 4)
	for range cap(results) {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := InstallKnowledge(context.Background(), agentDir, nil)
			results <- err
		}()
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "knowledge"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected one build directory and an installation lock: %v / %v", entries, err)
	}
	for _, entry := range entries {
		if entry.Name() == ".install.lock" {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "knowledge", entry.Name(), "manifest.json")); err != nil {
			t.Fatalf("incomplete installation: %s / %v", entry.Name(), err)
		}
	}
}

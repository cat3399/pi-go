package installation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(filepath.Base(first.SourcePath), "dev-") && variant.BuildID == first.BuildID {
		t.Fatal("a development build with different sources reused the same build identity")
	}
	if data, err := os.ReadFile(first.ReadmePath); err != nil || string(data) != customized[first.ReadmePath] {
		t.Fatal("a new build displaced the user's changes", err)
	}
	if _, err := InstallKnowledge(ctx, agentDir, []SourceBundle{{Files: fstest.MapFS{"README.md": {Data: []byte("collision")}}}}); err == nil {
		t.Fatal("conflicting source bundles were accepted")
	}
}

func knowledgeFixture(t *testing.T) ([]sourceFile, []SourceBundle) {
	t.Helper()
	base, err := readBundle(SourceBundle{Files: fstest.MapFS{
		"README.md": {Data: []byte("release docs")}, "docs/usage.md": {Data: []byte("usage")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	gui := []SourceBundle{{Prefix: "surface/gui", Files: fstest.MapFS{
		"main.go": {Data: []byte("gui source")}, "frontend/app.ts": {Data: []byte("frontend")},
	}}}
	return base, gui
}

func TestReleaseKnowledgeIsAdditiveInEitherOrder(t *testing.T) {
	base, gui := knowledgeFixture(t)
	for _, order := range []string{"terminal first", "GUI first"} {
		t.Run(order, func(t *testing.T) {
			root := t.TempDir()
			install := func(extra []SourceBundle) string {
				t.Helper()
				docs, err := installKnowledge(t.Context(), root, "1.2.3", base, extra)
				if err != nil || docs.SourcePath != filepath.Join(root, "1.2.3") {
					t.Fatalf("release installation: %#v, %v", docs, err)
				}
				return docs.SourcePath
			}
			var first, second []SourceBundle
			if order == "GUI first" {
				first = gui
			} else {
				second = gui
			}
			dir := install(first)
			guiDir := filepath.Join(dir, "surface", "gui")
			moved := filepath.Join(dir, "docs", "usage.md")
			if err := os.Rename(moved, filepath.Join(dir, "my-notes.md")); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(guiDir, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"README.md", "manifest.json", "surface/gui/main.go"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("my copy"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			install(second)
			for name, want := range map[string]string{"README.md": "my copy", "manifest.json": "my copy", "surface/gui/main.go": "my copy", "surface/gui/frontend/app.ts": "frontend"} {
				if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(data) != want {
					t.Fatalf("%s = %q, %v", name, data, err)
				}
			}
			if _, err := os.Stat(moved); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("removed common file was restored: %v", err)
			}
			// A moved module directory (not just one missing file) stays moved.
			if err := os.Rename(guiDir, filepath.Join(dir, "my-gui")); err != nil {
				t.Fatal(err)
			}
			unlock, err := lock(t.Context(), filepath.Join(root, ".install.lock"))
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			for _, extras := range [][]SourceBundle{nil, gui} {
				if _, err := installKnowledge(ctx, root, "v1.2.3", base, extras); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(guiDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("moved GUI tree was restored: %v", err)
			}
		})
	}
}

func TestReleaseRejectsDifferentCommonInputsWithoutTouchingLocalFiles(t *testing.T) {
	root := t.TempDir()
	base, gui := knowledgeFixture(t)
	docs, err := installKnowledge(t.Context(), root, "1.2.3", base, nil)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := readBundle(SourceBundle{Files: fstest.MapFS{"README.md": {Data: []byte("wrong tag")}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := installKnowledge(t.Context(), root, "1.2.3", changed, gui); err == nil {
		t.Fatal("same release accepted different common sources")
	}
	if data, err := os.ReadFile(docs.ReadmePath); err != nil || string(data) != "release docs" {
		t.Fatalf("original release: %q, %v", data, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 {
		t.Fatalf("unexpected fallback directories: %v, %v", entries, err)
	}
}

func TestDevKnowledgeUsesShortFullContentIdentity(t *testing.T) {
	root := t.TempDir()
	base, gui := knowledgeFixture(t)
	first, err := installKnowledge(t.Context(), root, "0.1.0-dev", base, nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := installKnowledge(t.Context(), root, "0.1.0-dev", base, gui)
	if err != nil {
		t.Fatal(err)
	}
	if first.SourcePath == other.SourcePath || len(first.BuildID) != 64 || filepath.Base(first.SourcePath) != "dev-"+first.BuildID[:12] {
		t.Fatalf("dev roots: %#v / %#v", first, other)
	}
	data, err := os.ReadFile(filepath.Join(other.SourcePath, "manifest.json"))
	var m knowledgeManifest
	if err != nil || json.Unmarshal(data, &m) != nil || m.BuildID != other.BuildID || m.Files["surface/gui/main.go"] == "" {
		t.Fatalf("wrong original manifest: %s (%v)", data, err)
	}
	// An occupied short name must be left alone, even for unusual build labels.
	collision := t.TempDir()
	if err := os.Mkdir(filepath.Join(collision, filepath.Base(first.SourcePath)), 0o700); err != nil {
		t.Fatal(err)
	}
	again, err := installKnowledge(t.Context(), collision, "0.1.0-dev", base, nil)
	if err != nil || filepath.Base(again.SourcePath) != "dev-"+first.BuildID[:16] {
		t.Fatalf("prefix collision = %#v, %v", again, err)
	}
	odd, err := installKnowledge(t.Context(), root, "../../other", base, nil)
	if err != nil || filepath.Dir(odd.SourcePath) != root {
		t.Fatalf("unsafe build label: %#v, %v", odd, err)
	}
}

func TestReleaseSupplementRefusesSymlinkParentAndCanRetry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires unprivileged symlinks")
	}
	root, outside := t.TempDir(), t.TempDir()
	base, gui := knowledgeFixture(t)
	docs, err := installKnowledge(t.Context(), root, "1.2.3", base, nil)
	if err != nil {
		t.Fatal(err)
	}
	surface := filepath.Join(docs.SourcePath, "surface")
	if err := os.Symlink(outside, surface); err != nil {
		t.Fatal(err)
	}
	if _, err := installKnowledge(t.Context(), root, "1.2.3", base, gui); err == nil {
		t.Fatal("wrote through a user symlink")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("outside directory modified: %v, %v", entries, err)
	}
	if err := os.Rename(surface, filepath.Join(docs.SourcePath, "my-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := installKnowledge(t.Context(), root, "1.2.3", base, gui); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(surface, "gui", "main.go")); err != nil || string(data) != "gui source" {
		t.Fatalf("retry = %q, %v", data, err)
	}
}

func TestConcurrentKnowledgeInstallationPublishesOnce(t *testing.T) {
	root := t.TempDir()
	base, gui := knowledgeFixture(t)
	var group sync.WaitGroup
	results := make(chan error, 4)
	for i := range cap(results) {
		group.Add(1)
		go func() {
			defer group.Done()
			var extra []SourceBundle
			if i%2 == 0 {
				extra = gui
			}
			_, err := installKnowledge(t.Context(), root, "1.2.3", base, extra)
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
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected one build directory and an installation lock: %v / %v", entries, err)
	}
	for _, path := range []string{"manifest.json", "surface/gui/main.go", "docs/usage.md"} {
		if _, err := os.Stat(filepath.Join(root, "1.2.3", path)); err != nil {
			t.Fatalf("incomplete installation: %s / %v", path, err)
		}
	}
}

package application

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
)

func TestFileResourcesListAuthorizeAndUpload(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "node_modules"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "zeta.txt"), []byte("zeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.pyc"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := newFileTestService(t, root)

	listed, err := service.ListFiles(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Entries) != 2 || listed.Entries[0].Name != "Alpha" || !listed.Entries[0].IsDir || listed.Entries[1].Name != "zeta.txt" {
		t.Fatalf("file list = %#v", listed)
	}

	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(root, "escape.txt")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		resource, err := service.ResolveFile(context.Background(), link, "")
		if err != nil || !resource.IsFile {
			t.Fatalf("operator-selected symlink = %#v, %v", resource, err)
		}
	}

	inspection, err := service.InspectUploadTargets(context.Background(), root, []string{"zeta.txt", "new.txt", "Alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inspection.Conflicts) != 2 || len(inspection.NonReplaceable) != 1 || inspection.NonReplaceable[0] != "Alpha" {
		t.Fatalf("upload inspection = %#v", inspection)
	}
	if _, err := service.SaveUploads(context.Background(), root, []UploadFile{{Name: "zeta.txt", Data: []byte("replacement")}}, UploadConflictError); !errors.Is(err, ErrUploadConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	result, err := service.SaveUploads(context.Background(), root, []UploadFile{
		{Name: "zeta.txt", Data: []byte("not-written")},
		{Name: "new.txt", Data: []byte("new\n")},
	}, UploadConflictSkip)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Uploaded) != 1 || result.Uploaded[0] != "new.txt" || len(result.Skipped) != 1 || result.Skipped[0] != "zeta.txt" {
		t.Fatalf("upload result = %#v", result)
	}
	content, err := os.ReadFile(filepath.Join(root, "zeta.txt"))
	if err != nil || string(content) != "zeta\n" {
		t.Fatalf("skipped file = %q, %v", content, err)
	}
}

func TestDeleteFileRemovesOnlyOneAuthorizedEntry(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(filePath, []byte("notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyDirectory := filepath.Join(root, "empty")
	if err := os.Mkdir(emptyDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	nonEmptyDirectory := filepath.Join(root, "non-empty")
	if err := os.Mkdir(nonEmptyDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonEmptyDirectory, "child.txt"), []byte("child"), 0o600); err != nil {
		t.Fatal(err)
	}
	protectedDirectory := filepath.Join(root, "protected")
	if err := os.Mkdir(protectedDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	service := newFileTestService(t, root)
	service.allowResourceRoot(protectedDirectory)

	if err := service.DeleteFile(context.Background(), filePath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted file still exists: %v", err)
	}
	if err := service.DeleteFile(context.Background(), emptyDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(emptyDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted empty directory still exists: %v", err)
	}
	if err := service.DeleteFile(context.Background(), nonEmptyDirectory); !errors.Is(err, ErrDirectoryNotEmpty) {
		t.Fatalf("non-empty directory delete error = %v", err)
	}
	if err := service.DeleteFile(context.Background(), protectedDirectory); !errors.Is(err, ErrProtectedResourceRoot) {
		t.Fatalf("protected directory delete error = %v", err)
	}

	if runtime.GOOS != "windows" {
		outside := filepath.Join(t.TempDir(), "outside.txt")
		if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "outside-link.txt")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if err := service.DeleteFile(context.Background(), link); err != nil {
			t.Fatal(err)
		}
		if content, err := os.ReadFile(outside); err != nil || string(content) != "outside" {
			t.Fatalf("symbolic-link target changed: %q, %v", content, err)
		}
	}
}

func TestFileReadsDoNotDependOnConversationOrProjectCatalog(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	service := newFileTestService(t, root)
	path := filepath.Join(outside, "notes.txt")
	if err := os.WriteFile(path, []byte("outside workspace"), 0600); err != nil {
		t.Fatal(err)
	}
	// File reading does not need discovery or a valid session. A corrupt catalog
	// must not turn an ordinary filesystem read into a session-history operation.
	if err := os.MkdirAll(service.paths.AgentDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(service.paths.AgentDir, "projects.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "not-a-session", "01234567-89ab-4cde-8abc-0123456789ab"} {
		resource, err := service.ResolveFile(context.Background(), path, id)
		if err != nil || !resource.IsFile || resource.Size != int64(len("outside workspace")) {
			t.Fatalf("ResolveFile(%q) = %#v, %v", id, resource, err)
		}
	}
	for _, path := range []string{"", "relative.txt"} {
		if _, err := service.ResolveFile(context.Background(), path, ""); !errors.Is(err, ErrResourceAccessDenied) {
			t.Fatalf("invalid path %q: %v", path, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.ResolveFile(ctx, path, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
}

func TestOutsideFileReadDoesNotGrantDirectoryOrMutationAccess(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	service := newFileTestService(t, root)
	path := filepath.Join(outside, "notes.txt")
	if err := os.WriteFile(path, []byte("kept"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResolveFile(context.Background(), path, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ListFiles(context.Background(), outside); !errors.Is(err, ErrResourceAccessDenied) {
		t.Fatalf("outside listing: %v", err)
	}
	if _, err := service.InspectUploadTargets(context.Background(), outside, []string{"notes.txt"}); !errors.Is(err, ErrResourceAccessDenied) {
		t.Fatalf("outside upload inspection: %v", err)
	}
	if _, err := service.SaveUploads(context.Background(), outside, []UploadFile{{Name: "notes.txt", Data: []byte("replacement")}}, UploadConflictOverwrite); !errors.Is(err, ErrResourceAccessDenied) {
		t.Fatalf("outside upload: %v", err)
	}
	if err := service.DeleteFile(context.Background(), path); !errors.Is(err, ErrResourceAccessDenied) {
		t.Fatalf("outside deletion: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "kept" {
		t.Fatalf("outside file = %q, %v", content, err)
	}
}

func newFileTestService(t *testing.T, root string) *Service {
	t.Helper()
	service, err := NewService(ServiceOptions{
		Production: app.ProductionConfig{
			WorkingDir: root, AgentDir: filepath.Join(root, ".pi-agent-test"), Environment: []string{},
		},
		DisableReaper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	return service
}

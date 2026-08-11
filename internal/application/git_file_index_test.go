package application

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGitStatusDiffAndFileIndexContracts(t *testing.T) {
	parent := t.TempDir()
	repository := filepath.Join(parent, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repository, "init", "-b", "main")
	if err := os.Mkdir(filepath.Join(repository, "components"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"tracked.txt":              "one\n",
		"components/ChatInput.tsx": "export const ChatInput = 1;\n",
		".gitignore":               "ignored.log\n",
	} {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runTestGit(t, repository, "add", ".")
	runTestGit(t, repository, "-c", "user.name=Pi Go Test", "-c", "user.email=pi-go@example.invalid", "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("one\nchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "untracked.txt"), []byte("a\nb"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "ignored.log"), []byte("ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := newFileTestService(t, repository)

	status, err := service.GetGitStatus(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if !status.IsGitRepository || status.RepositoryRoot == nil || len(status.Files) != 2 || status.Additions != 3 || status.Deletions != 0 {
		t.Fatalf("git status = %#v", status)
	}
	statusByName := make(map[string]GitFileStatus)
	for _, file := range status.Files {
		statusByName[filepath.Base(file.FilePath)] = file
	}
	if statusByName["tracked.txt"].Status != GitStatusModified || statusByName["untracked.txt"].Status != GitStatusUntracked {
		t.Fatalf("git files = %#v", status.Files)
	}
	diff, err := service.GetGitFileDiff(context.Background(), repository, filepath.Join(repository, "untracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !diff.Supported || diff.Status != GitStatusUntracked || diff.Patch == "" {
		t.Fatalf("untracked diff = %#v", diff)
	}

	index, err := service.QueryFileIndex(context.Background(), repository, "")
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string]bool, len(index.Files))
	for _, file := range index.Files {
		files[file] = true
	}
	if !files["tracked.txt"] || !files["untracked.txt"] || !files["components/ChatInput.tsx"] || files["ignored.log"] {
		t.Fatalf("file index = %#v", index)
	}
	query, err := service.QueryFileIndex(context.Background(), repository, "chinp")
	if err != nil {
		t.Fatal(err)
	}
	if !query.HasQuery || len(query.Matches) == 0 || query.Matches[0].Path != "components/ChatInput.tsx" {
		t.Fatalf("file query = %#v", query)
	}
}

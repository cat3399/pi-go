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
)

func migrationFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestImportCopiesActualInstructionEntriesOnce(t *testing.T) {
	for _, names := range [][]string{
		{"AGENTS.md", "CLAUDE.md"},
		{"AGENTS.MD", "CLAUDE.MD"},
		{"AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"},
	} {
		t.Run(strings.Join(names, "+"), func(t *testing.T) {
			root := t.TempDir()
			source, target := filepath.Join(root, "legacy"), filepath.Join(root, "agent")
			if err := os.Mkdir(source, 0o700); err != nil {
				t.Fatal(err)
			}
			actual := make(map[string]string)
			for _, name := range names {
				file, err := os.OpenFile(filepath.Join(source, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
				if errors.Is(err, os.ErrExist) {
					// This volume resolves both spellings to one directory entry.
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				contents := "instructions from " + name
				_, writeErr := file.WriteString(contents)
				if err := errors.Join(writeErr, file.Close()); err != nil {
					t.Fatal(err)
				}
				actual[name] = contents
			}
			if err := initializeDirectory(context.Background(), source, target, root, agentEntries); err != nil {
				t.Fatal(err)
			}
			imported, err := os.ReadDir(target)
			if err != nil {
				t.Fatal(err)
			}
			if len(imported) != len(actual)+1 {
				t.Fatalf("imported %d entries, want %d files plus the migration record", len(imported), len(actual))
			}
			for _, entry := range imported {
				if entry.Name() == migrationRecordName {
					continue
				}
				expected, exists := actual[entry.Name()]
				data, err := os.ReadFile(filepath.Join(target, entry.Name()))
				if !exists || err != nil || string(data) != expected {
					t.Fatalf("instruction name or content changed: %s / %v", entry.Name(), err)
				}
			}
			var record migrationRecord
			data, err := os.ReadFile(filepath.Join(target, migrationRecordName))
			if err != nil || json.Unmarshal(data, &record) != nil || len(record.Files) != len(actual) || len(record.Skipped) != 0 {
				t.Fatalf("incorrect instruction import record: %s / %v", data, err)
			}
		})
	}
}

func TestImportPreservesOriginalDataAndRebasesOnlyOwnedReferences(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "legacy"), filepath.Join(root, "agent")
	settings, _ := json.Marshal(map[string]any{"skills": []string{filepath.Join(source, "skills", "review")}, "prompts": []string{filepath.Join(source, "prompts")}, "future": map[string]any{"literal": source}, "shellCommandPrefix": "echo " + source})
	migrationFile(t, filepath.Join(source, "settings.json"), string(settings))
	auth := "{\"openai\":{\"type\":\"api_key\",\"key\":\"fixture\",\"future\":42}}\n"
	migrationFile(t, filepath.Join(source, "auth.json"), auth)
	migrationFile(t, filepath.Join(source, "skills", "review", "SKILL.md"), "skill instructions")
	migrationFile(t, filepath.Join(source, "skills", "review", "Cargo.lock"), "a skill's resource, not a process lock")
	migrationFile(t, filepath.Join(source, "prompts", "review.md"), "review $1")
	migrationFile(t, filepath.Join(source, "extensions", "extension.ts"), "not executed")
	parent := filepath.Join(source, "sessions", "project", "parent.jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": "child", "parentSession": parent, "cwd": root})
	tail := "\n{\"type\":\"custom\",\"data\":\"" + filepath.ToSlash(source) + "\"}\n{damaged trailing entry\n"
	migrationFile(t, filepath.Join(source, "sessions", "project", "child.jsonl"), string(header)+tail)
	migrationFile(t, parent, "{\"type\":\"session\",\"version\":3}\n")
	migrationFile(t, parent+".pi-go.lock", "")
	if err := initializeDirectory(context.Background(), source, target, root, agentEntries); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(filepath.Join(source, "settings.json"))
	if string(original) != string(settings) {
		t.Fatal("source settings changed")
	}
	credential, _ := os.ReadFile(filepath.Join(target, "auth.json"))
	if string(credential) != auth {
		t.Fatal("credential fields were lost")
	}
	info, _ := os.Stat(filepath.Join(target, "auth.json"))
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %o", info.Mode())
	}
	var imported map[string]json.RawMessage
	data, _ := os.ReadFile(filepath.Join(target, "settings.json"))
	if err := json.Unmarshal(data, &imported); err != nil {
		t.Fatal(err)
	}
	var skills []string
	_ = json.Unmarshal(imported["skills"], &skills)
	if len(skills) != 1 || skills[0] != filepath.Join(target, "skills", "review") {
		t.Fatalf("skills = %v", skills)
	}
	var command string
	_ = json.Unmarshal(imported["shellCommandPrefix"], &command)
	if command != "echo "+source {
		t.Fatal("arbitrary command content was rewritten")
	}
	child, _ := os.ReadFile(filepath.Join(target, "sessions", "project", "child.jsonl"))
	line, body, _ := strings.Cut(string(child), "\n")
	var importedHeader map[string]any
	_ = json.Unmarshal([]byte(line), &importedHeader)
	if importedHeader["parentSession"] != filepath.Join(target, "sessions", "project", "parent.jsonl") || "\n"+body != tail {
		t.Fatalf("session reference/body changed incorrectly: %s", child)
	}
	if _, err := os.Stat(filepath.Join(target, "skills", "review", "Cargo.lock")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(target, "extensions"), filepath.Join(target, "sessions", "project", "parent.jsonl.pi-go.lock")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("runtime artifact imported: %s", path)
		}
	}
	migrationFile(t, filepath.Join(source, "auth.json"), "changed after import")
	if err := initializeDirectory(context.Background(), source, target, root, agentEntries); err != nil {
		t.Fatal(err)
	}
	credential, _ = os.ReadFile(filepath.Join(target, "auth.json"))
	if string(credential) != auth {
		t.Fatal("second startup reimported credentials")
	}
}

func TestConcurrentImportPublishesOneCompleteDirectory(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "old"), filepath.Join(root, "new")
	migrationFile(t, filepath.Join(source, "settings.json"), "{\"future\":true}\n")
	var group sync.WaitGroup
	errorsOut := make(chan error, 8)
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			errorsOut <- initializeDirectory(context.Background(), source, target, root, agentEntries)
		}()
	}
	group.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	var record migrationRecord
	data, err := os.ReadFile(filepath.Join(target, migrationRecordName))
	if err != nil || json.Unmarshal(data, &record) != nil || len(record.Files) != 1 {
		t.Fatalf("incomplete publication: %s / %v", data, err)
	}
	stages, _ := filepath.Glob(filepath.Join(root, ".new-import-*"))
	if len(stages) != 0 {
		t.Fatalf("concurrent imports created extra staging trees: %v", stages)
	}
}

func TestImportPreservesAmbiguousJSONWithoutDroppingDuplicateFields(t *testing.T) {
	root := t.TempDir()
	source, target := filepath.Join(root, "legacy"), filepath.Join(root, "agent")
	owned, _ := json.Marshal(filepath.Join(source, "skills"))
	settings := `{"skills":[` + string(owned) + `],"future":1,"future":2}`
	header := `{"parentSession":` + string(owned) + `,"future":1,"future":2}` + "\n"
	migrationFile(t, filepath.Join(source, "settings.json"), settings)
	migrationFile(t, filepath.Join(source, "sessions", "old.jsonl"), header)
	if err := initializeDirectory(context.Background(), source, target, root, agentEntries); err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string]string{"settings.json": settings, "sessions/old.jsonl": header} {
		data, err := os.ReadFile(filepath.Join(target, path))
		if err != nil || string(data) != expected {
			t.Fatalf("imported %s = %q, %v", path, data, err)
		}
	}
}

func TestInterruptedImportCanRetryWithoutReplacingExistingData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test requires unprivileged symlinks")
	}
	root := t.TempDir()
	source, target := filepath.Join(root, "old"), filepath.Join(root, "new")
	migrationFile(t, filepath.Join(source, "auth.json"), "{}")
	if err := os.Symlink("missing", filepath.Join(source, "skills")); err != nil {
		t.Fatal(err)
	}
	if err := initializeDirectory(context.Background(), source, target, root, agentEntries); err == nil {
		t.Fatal("invalid link was accepted")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial target was published")
	}
	if err := os.Rename(filepath.Join(source, "skills"), filepath.Join(root, "failed-link")); err != nil {
		t.Fatal(err)
	}
	migrationFile(t, filepath.Join(source, "skills", "review", "SKILL.md"), "complete")
	if err := initializeDirectory(context.Background(), source, target, root, agentEntries); err != nil {
		t.Fatal(err)
	}
	migrationFile(t, filepath.Join(target, "auth.json"), "user's own value")
	if err := initializeDirectory(context.Background(), source, target, root, agentEntries); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(target, "auth.json"))
	if string(data) != "user's own value" {
		t.Fatal("existing target overwritten")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := initializeDirectory(cancelled, source, filepath.Join(root, "cancelled"), root, agentEntries); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
}

func TestImportRebasesInternalLinksAndPreservesExternalSharedResources(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test requires unprivileged symlinks")
	}
	root := t.TempDir()
	source, target := filepath.Join(root, "old"), filepath.Join(root, "new")
	migrationFile(t, filepath.Join(source, "skills", "original", "SKILL.md"), "original")
	shared := filepath.Join(root, "shared")
	migrationFile(t, filepath.Join(shared, "SKILL.md"), "shared")
	if err := os.Symlink("original", filepath.Join(source, "skills", "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(source, "skills", "shared")); err != nil {
		t.Fatal(err)
	}
	if err := initializeDirectory(context.Background(), source, target, root, agentEntries); err != nil {
		t.Fatal(err)
	}
	alias, err := os.Readlink(filepath.Join(target, "skills", "alias"))
	if err != nil || alias != filepath.Join(target, "skills", "original") {
		t.Fatalf("alias = %q / %v", alias, err)
	}
	alias, err = os.Readlink(filepath.Join(target, "skills", "shared"))
	resolvedShared, _ := filepath.EvalSymlinks(shared)
	if err != nil || alias != resolvedShared {
		t.Fatalf("shared = %q / %v", alias, err)
	}
}

func TestExplicitDirectoryDoesNotImportOrCreateUserData(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "agent")
	if err := InitializeAgent(context.Background(), target, root, nil, false); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("explicit directory had startup side effects: %v / %v", entries, err)
	}
}

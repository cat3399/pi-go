package resource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newService(t *testing.T) (*Service, string, string) {
	t.Helper()
	root := t.TempDir()
	agent := filepath.Join(root, "agent")
	cwd := filepath.Join(root, "project")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{CWD: cwd, AgentDir: agent, Tools: []Tool{{Name: "read", Snippet: "Read files"}}})
	if err != nil {
		t.Fatal(err)
	}
	return s, agent, cwd
}
func write(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUntrustedProjectConfigIsGatedWhileContextIsIncluded(t *testing.T) {
	s, agent, cwd := newService(t)
	write(t, filepath.Join(agent, "AGENTS.md"), "global rule")
	write(t, filepath.Join(cwd, "AGENTS.md"), string([]byte{0xff}))
	write(t, filepath.Join(cwd, ".pi-go", "SYSTEM.md"), "project secret")
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("untrusted Reload() = %v", err)
	}
	snapshot, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Trusted || len(snapshot.Instructions) != 2 || snapshot.Instructions[0].Content != "global rule" || snapshot.Instructions[1].Content != "�" || strings.Contains(snapshot.SystemPrompt, "project secret") {
		t.Fatalf("untrusted snapshot = %#v", snapshot)
	}
	if err := s.Trust().Set(context.Background(), cwd, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("trusted invalid project Reload() = %v", err)
	}
	snapshot, _ = s.Snapshot()
	if snapshot.BaseSystemPrompt != "project secret" {
		t.Fatalf("trusted project system prompt = %#v", snapshot)
	}
}
func TestTrustedResourcesPrecedenceCollisionAndAssembly(t *testing.T) {
	s, agent, cwd := newService(t)
	write(t, filepath.Join(agent, "prompts", "review.md"), "---\ndescription: global\n---\nglobal $1")
	write(t, filepath.Join(agent, "skills", "review", "SKILL.md"), "---\nname: review\ndescription: global skill\n---\nbody")
	write(t, filepath.Join(cwd, ".pi-go", "prompts", "review.md"), "---\ndescription: project\nargument-hint: <file>\n---\nproject ${1:-all}")
	write(t, filepath.Join(cwd, ".pi-go", "skills", "review", "SKILL.md"), "---\nname: review\ndescription: project skill\n---\nbody")
	if err := s.Trust().Set(context.Background(), cwd, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Snapshot()
	if len(snap.Templates) != 1 || snap.Templates[0].Description != "project" || ExpandTemplate("/review", snap.Templates) != "project all" {
		t.Fatalf("templates = %#v", snap.Templates)
	}
	if len(snap.Skills) != 1 || snap.Skills[0].Scope != ScopeProject || !strings.Contains(snap.SystemPrompt, "project skill") {
		t.Fatalf("skills/prompt = %#v", snap)
	}
	if len(snap.Diagnostics) != 2 || snap.Diagnostics[0].WinnerPath == snap.Diagnostics[0].LoserPath {
		t.Fatalf("collisions = %#v", snap.Diagnostics)
	}
}
func TestBrokenSymlinkIsSkippedAndDefaultResourceSizeIsUnbounded(t *testing.T) {
	s, agent, _ := newService(t)
	write(t, filepath.Join(agent, "SYSTEM.md"), "healthy")
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(agent, "SYSTEM.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(agent, "missing"), filepath.Join(agent, "SYSTEM.md")); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("symlink reload = %v", err)
	}
	after, _ := s.Snapshot()
	if after.BaseSystemPrompt != "" || strings.Contains(after.SystemPrompt, "healthy") {
		t.Fatalf("broken prompt symlink was not skipped: %#v", after)
	}
	if err := os.Remove(filepath.Join(agent, "SYSTEM.md")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(agent, "SYSTEM.md"), strings.Repeat("x", int(DefaultMaxFileBytes)+1))
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("large reload = %v", err)
	}
	after, _ = s.Snapshot()
	if len(after.BaseSystemPrompt) != int(DefaultMaxFileBytes)+1 {
		t.Fatalf("large prompt length = %d", len(after.BaseSystemPrompt))
	}
}
func TestTrustStoreAtomicPreservesDecisionsAndJSONUsesLastDuplicate(t *testing.T) {
	s, agent, cwd := newService(t)
	other := filepath.Join(filepath.Dir(cwd), "other")
	write(t, filepath.Join(agent, "trust.json"), "{\n  \""+other+"\": false\n}\n")
	if err := s.Trust().Set(context.Background(), cwd, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(agent, "trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\""+other+"\": false") || strings.Contains(string(data), ".trust.json-") {
		t.Fatalf("trust bytes = %q", data)
	}
	trusted, known, err := s.Trust().Get(context.Background(), cwd)
	if err != nil || !known || !trusted {
		t.Fatalf("Get = %t,%t,%v", trusted, known, err)
	}
	write(t, filepath.Join(agent, "trust.json"), `{"x":true,"x":false}`)
	if trusted, known, err := s.Trust().Get(context.Background(), "x"); err != nil || known || trusted {
		t.Fatalf("duplicate trust JSON = %t,%t,%v", trusted, known, err)
	}
}
func TestCancelAndConcurrentSnapshots(t *testing.T) {
	s, agent, _ := newService(t)
	write(t, filepath.Join(agent, "AGENTS.md"), "safe")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Reload(ctx); !errors.Is(err, ErrTrustStore) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for n := 0; n < 80; n++ {
				if err := s.Reload(context.Background()); errors.Is(err, ErrStaleReload) {
					continue
				} else if err != nil {
					t.Errorf("reload: %v", err)
					return
				}
				snap, err := s.Snapshot()
				if err != nil || !strings.Contains(snap.SystemPrompt, "safe") {
					t.Errorf("snapshot: %#v %v", snap, err)
					return
				}
			}
		}()
	}
	group.Wait()
}
func FuzzResourceParsersDoNotPanic(f *testing.F) {
	f.Add("/name one two")
	f.Add("---\nname: valid\ndescription: x\n---\nbody")
	f.Add("1e9999")
	f.Fuzz(func(t *testing.T, input string) {
		_, _, _ = frontmatter(input)
		_ = ExpandTemplate(input, []Template{{Name: "name", Content: "$1 ${2:-x} $@"}})
		_ = validateRawJSON([]byte(input))
	})
}

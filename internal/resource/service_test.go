package resource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

func TestUntrustedProjectIsNotProbedOrIncluded(t *testing.T) {
	s, agent, cwd := newService(t)
	write(t, filepath.Join(agent, "AGENTS.md"), "global rule")
	write(t, filepath.Join(cwd, "AGENTS.md"), string([]byte{0xff}))
	write(t, filepath.Join(cwd, ".pi", "SYSTEM.md"), "project secret")
	if err := s.Reload(context.Background()); err != nil {
		t.Fatalf("untrusted Reload() = %v", err)
	}
	snapshot, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Trusted || len(snapshot.Instructions) != 1 || snapshot.Instructions[0].Content != "global rule" || strings.Contains(snapshot.SystemPrompt, "project secret") {
		t.Fatalf("untrusted snapshot = %#v", snapshot)
	}
	if err := s.Trust().Set(context.Background(), cwd, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); !errors.Is(err, ErrMalformed) {
		t.Fatalf("trusted invalid project Reload() = %v", err)
	}
}
func TestTrustedResourcesPrecedenceCollisionAndAssembly(t *testing.T) {
	s, agent, cwd := newService(t)
	write(t, filepath.Join(agent, "prompts", "review.md"), "---\ndescription: global\n---\nglobal $1")
	write(t, filepath.Join(agent, "skills", "review", "SKILL.md"), "---\nname: review\ndescription: global skill\n---\nbody")
	write(t, filepath.Join(cwd, ".pi", "prompts", "review.md"), "---\ndescription: project\nargument-hint: <file>\n---\nproject ${1:-all}")
	write(t, filepath.Join(cwd, ".pi", "skills", "review", "SKILL.md"), "---\nname: review\ndescription: project skill\n---\nbody")
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
func TestUnsafeAndOversizedResourcesFailClosedAndKeepHealthySnapshot(t *testing.T) {
	s, agent, _ := newService(t)
	write(t, filepath.Join(agent, "SYSTEM.md"), "healthy")
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Snapshot()
	if err := os.Remove(filepath.Join(agent, "SYSTEM.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(agent, "missing"), filepath.Join(agent, "SYSTEM.md")); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(context.Background()); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink reload = %v", err)
	}
	after, _ := s.Snapshot()
	if after.SystemPrompt != before.SystemPrompt {
		t.Fatalf("failed reload replaced healthy snapshot")
	}
	if err := os.Remove(filepath.Join(agent, "SYSTEM.md")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(agent, "SYSTEM.md"), strings.Repeat("x", int(DefaultMaxFileBytes)+1))
	if err := s.Reload(context.Background()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("large reload = %v", err)
	}
}
func TestTrustStoreStrictPrivateAtomicAndPreservesOtherDecisions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("v0.1 has no Windows private-file implementation")
	}
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
	if _, _, err := s.Trust().Get(context.Background(), cwd); !errors.Is(err, ErrTrustStore) {
		t.Fatalf("duplicate trust = %v", err)
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
				if err := s.Reload(context.Background()); err != nil {
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

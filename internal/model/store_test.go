package model

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func cached(providerID, id string) CachedCatalog {
	return CachedCatalog{Models: []Model{{Provider: providerID, ID: id, API: OpenAIResponsesAPI}}, CheckedAt: 1, ETag: "opaque"}
}

func TestStoreProviderScopedMergeAndUnknownPreservation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows v0.1 fails closed for private durable configuration")
	}
	path := filepath.Join(t.TempDir(), "models-store.json")
	writeFile(t, path, `{"future":{"opaque":true}}`)
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(context.Background(), "one", cached("one", "m1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(context.Background(), "two", cached("two", "m2")); err != nil {
		t.Fatal(err)
	}
	one, ok, err := s.Read(context.Background(), "one")
	if err != nil || !ok || one.Models[0].ID != "m1" {
		t.Fatalf("one=%#v %t %v", one, ok, err)
	}
	if err := s.Delete(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	_, ok, err = s.Read(context.Background(), "one")
	if err != nil || ok {
		t.Fatalf("deleted=%t %v", ok, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(b), `"future"`) {
		t.Fatalf("unknown cache entry lost: %s", b)
	}
}
func TestStoreConcurrentWriters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows v0.1 fails closed for private durable configuration")
	}
	s, err := NewStore(filepath.Join(t.TempDir(), "models-store.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Write(context.Background(), "provider", cached("provider", string(rune('a'+i)))); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if _, ok, err := s.Read(context.Background(), "provider"); err != nil || !ok {
		t.Fatalf("read %t %v", ok, err)
	}
}

func TestStorePreservesOpaqueEntryAndModelFields(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows v0.1 fails closed for private durable configuration")
	}
	path := filepath.Join(t.TempDir(), "models-store.json")
	writeFile(t, path, `{"one":{"models":[{"provider":"one","id":"m1","api":"openai-responses","futureModel":{"keep":true}}],"checkedAt":1,"futureEntry":{"keep":true}},"future":{"opaque":true}}`)
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(context.Background(), "one", cached("one", "m1")); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"future"`, `"futureEntry"`, `"futureModel"`} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("write lost %s: %s", want, content)
		}
	}
}

func TestStoreRejectsNullAndRuntimeProjectsValidCatalog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows v0.1 fails closed for private durable configuration")
	}
	path := filepath.Join(t.TempDir(), "models-store.json")
	writeFile(t, path, `{"broken":null,"cached":{"models":[{"provider":"cached","id":"m1","api":"openai-responses"}],"checkedAt":1}}`)
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Read(context.Background(), "broken"); err == nil {
		t.Fatal("null catalog was admitted")
	}
	r, err := NewRuntime(Options{AgentDir: filepath.Dir(path), ModelsStorePath: path})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := r.Resolve(Selection{Provider: "cached", Model: "m1"})
	if err != nil || r.ValidateRoute(resolved.Model) != nil {
		t.Fatalf("valid cache was not projected: %#v, %v", resolved, err)
	}
	if !errors.Is(r.ValidateRoute(Model{Provider: "broken"}), ErrUnsupported) {
		t.Fatal("selected invalid cached provider must fail route")
	}
}

func TestStoreSerializesAcrossProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows v0.1 fails closed for private durable configuration")
	}
	path := filepath.Join(t.TempDir(), "models-store.json")
	command := exec.Command(os.Args[0], "-test.run=^TestStoreHelperProcess$")
	command.Env = append(os.Environ(), "PI_GO_MODEL_STORE_HELPER=1", "PI_GO_MODEL_STORE_PATH="+path)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(context.Background(), "parent", cached("parent", "one")); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("helper failed: %v", err)
	}
	for _, providerID := range []string{"parent", "child"} {
		if _, ok, err := s.Read(context.Background(), providerID); err != nil || !ok {
			t.Fatalf("%s missing after re-exec: %t %v", providerID, ok, err)
		}
	}
}

func TestStoreHelperProcess(t *testing.T) {
	if os.Getenv("PI_GO_MODEL_STORE_HELPER") != "1" {
		return
	}
	s, err := NewStore(os.Getenv("PI_GO_MODEL_STORE_PATH"))
	if err != nil {
		os.Exit(2)
	}
	if err := s.Write(context.Background(), "child", cached("child", "two")); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestStoreCancellationAndPublicationOutcome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows v0.1 fails closed for private durable configuration")
	}
	s, err := NewStore(filepath.Join(t.TempDir(), "models-store.json"))
	if err != nil {
		t.Fatal(err)
	}
	release, err := acquireLocal(context.Background(), s.local)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.Read(ctx, "one"); !errors.Is(err, ErrCancelled) {
		t.Fatalf("cancelled read = %v", err)
	}
	release()
	if err := s.Write(context.Background(), "one", cached("one", "old")); err != nil {
		t.Fatal(err)
	}
	s.faults.beforeRename = func() error { return errors.New("injected") }
	if err := s.Write(context.Background(), "one", cached("one", "new")); err == nil {
		t.Fatal("pre-rename fault succeeded")
	}
	got, ok, err := s.Read(context.Background(), "one")
	if err != nil || !ok || got.Models[0].ID != "old" {
		t.Fatalf("pre-rename publish changed state: %#v %t %v", got, ok, err)
	}
	s.faults.beforeRename = nil
	s.faults.afterRename = func() error { return errors.New("injected") }
	if err := s.Write(context.Background(), "one", cached("one", "new")); !errors.Is(err, ErrCommitUnknown) {
		t.Fatalf("post-rename error = %v", err)
	}
	got, ok, err = s.Read(context.Background(), "one")
	if err != nil || !ok || got.Models[0].ID != "new" {
		t.Fatalf("post-rename state = %#v %t %v", got, ok, err)
	}
}

func TestWindowsStorePersistenceFailsClosed(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only contract")
	}
	s, err := NewStore(filepath.Join(t.TempDir(), "models-store.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Read(context.Background(), "one"); !errors.Is(err, ErrPersistence) {
		t.Fatalf("read = %v", err)
	}
	if err := s.Write(context.Background(), "one", cached("one", "model")); !errors.Is(err, ErrPersistence) {
		t.Fatalf("write = %v", err)
	}
	if err := s.Delete(context.Background(), "one"); !errors.Is(err, ErrPersistence) {
		t.Fatalf("delete = %v", err)
	}
}
func contains(s, part string) bool {
	return len(part) == 0 || (len(s) >= len(part) && (func() bool {
		for i := 0; i+len(part) <= len(s); i++ {
			if s[i:i+len(part)] == part {
				return true
			}
		}
		return false
	})())
}

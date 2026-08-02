package model

import (
	"context"
	"encoding/json"
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
	return CachedCatalog{Models: []CachedModel{{Provider: providerID, ID: id, API: OpenAIResponsesAPI}}, CheckedAt: 1, ETag: "opaque"}
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

func TestStoreCanonicalProviderKeyAndCompleteModelRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows v0.1 fails closed for private durable configuration")
	}
	path := filepath.Join(t.TempDir(), "models-store.json")
	writeFile(t, path, `{"OpenAI":{"models":[{"provider":"OPENAI","id":"MODEL","name":"old","api":"openai-responses","baseUrl":"https://old.invalid/v1","reasoning":false,"headers":{"X-Old":"old"},"futureModel":{"keep":true}}],"checkedAt":1,"futureEntry":{"keep":true}}}`)
	headers := map[string]string{"X-Secret": "header-secret"}
	entry := CachedCatalog{Models: []CachedModel{{Provider: "OpenAI", ID: "model", Name: "New model", API: OpenAIResponsesAPI, BaseURL: "https://new.invalid/v1", Reasoning: true, Headers: headers}}, CheckedAt: 2, ETag: "new-etag", LastModified: "now"}
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(context.Background(), "OPENAI", entry); err != nil {
		t.Fatal(err)
	}
	headers["X-Secret"] = "mutated-after-write"
	first, ok, err := s.Read(context.Background(), "openai")
	if err != nil || !ok || len(first.Models) != 1 {
		t.Fatalf("read = %#v, %t, %v", first, ok, err)
	}
	if first.CheckedAt != 2 || first.ETag != "new-etag" || first.LastModified != "now" {
		t.Fatalf("catalog metadata round trip = %#v", first)
	}
	model := first.Models[0]
	if model.Provider != "openai" || model.ID != "model" || model.Name != "New model" || model.API != OpenAIResponsesAPI || model.BaseURL != "https://new.invalid/v1" || !model.Reasoning || model.Headers["X-Secret"] != "header-secret" {
		t.Fatalf("model round trip = %#v", model)
	}
	first.Models[0].Headers["X-Secret"] = "mutated-read"
	second, _, err := s.Read(context.Background(), "OpEnAi")
	if err != nil || second.Models[0].Headers["X-Secret"] != "header-secret" {
		t.Fatalf("headers were not deep copied: %#v, %v", second, err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(content, &root); err != nil {
		t.Fatal(err)
	}
	if len(root) != 1 || root["openai"] == nil || !strings.Contains(string(content), `"futureModel"`) || !strings.Contains(string(content), `"futureEntry"`) {
		t.Fatalf("canonical write or opaque preservation failed: %s", content)
	}
	bad := entry
	bad.Models = append([]CachedModel(nil), entry.Models...)
	bad.Models[0] = cloneCachedModel(bad.Models[0])
	bad.Models[0].Headers["X-Bad"] = "do-not-leak\nsecret"
	if err := s.Write(context.Background(), "openai", bad); err == nil || strings.Contains(err.Error(), "do-not-leak") {
		t.Fatalf("invalid header diagnostic = %v", err)
	}
	if err := s.Delete(context.Background(), "OPENAI"); err != nil {
		t.Fatalf("canonical delete = %v", err)
	}
	if _, ok, err := s.Read(context.Background(), "openai"); err != nil || ok {
		t.Fatalf("canonical delete left entry: %t, %v", ok, err)
	}
}

func TestStoreRejectsCaseFoldDuplicateProviderKeys(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows v0.1 fails closed for private durable configuration")
	}
	path := filepath.Join(t.TempDir(), "models-store.json")
	content := `{"OpenAI":{"models":[{"provider":"OpenAI","id":"one","api":"openai-responses"}],"checkedAt":1},"openai":{"models":[{"provider":"openai","id":"two","api":"openai-responses"}],"checkedAt":2}}`
	writeFile(t, path, content)
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Read(context.Background(), "OPENAI"); err == nil || !strings.Contains(err.Error(), "case-fold duplicate") {
		t.Fatalf("duplicate read = %v", err)
	}
	if err := s.Write(context.Background(), "openai", cached("openai", "new")); err == nil || !strings.Contains(err.Error(), "case-fold duplicate") {
		t.Fatalf("duplicate write = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != content {
		t.Fatalf("duplicate store was changed: %q, %v", after, err)
	}
}

func TestStoreRequiresPreexistingDurableParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows v0.1 fails closed earlier")
	}
	root := t.TempDir()
	path := filepath.Join(root, "missing", "nested", "models-store.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Write(context.Background(), "one", cached("one", "model"))
	if !errors.Is(err, ErrPersistence) {
		t.Fatalf("missing parent write = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "missing")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("missing ancestor was created: %v", statErr)
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
	s.faults.afterRename = nil
	synced := false
	s.faults.syncDirectory = func(path string) error { synced = true; return syncModelDirectory(path) }
	if err := s.Write(context.Background(), "one", cached("one", "durable")); err != nil || !synced {
		t.Fatalf("successful store publication did not sync parent: %v, synced=%t", err, synced)
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

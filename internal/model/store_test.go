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

	"github.com/cat3399/pi-go/internal/provider"
)

func cached(providerID, id string) CachedCatalog {
	return CachedCatalog{Models: []CachedModel{{Provider: providerID, ID: id, API: OpenAIResponsesAPI}}, CheckedAt: int64Pointer(1), ETag: "opaque"}
}

func int64Pointer(value int64) *int64 { return &value }

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

func TestModelsStoreFullModelFixturePreservesFieldsAndCompatPresence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows v0.1 fails closed for private durable configuration")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "models-store.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	high := "high"
	checkedAt := int64(0)
	lastModified := int64(0)
	compat := json.RawMessage(`{
		"supportsStore":false,
		"supportsDeveloperRole":false,
		"supportsUsageInStreaming":true,
		"maxTokensField":"max_tokens",
		"thinkingFormat":"openrouter",
		"sendSessionAffinityHeaders":true,
		"sessionAffinityFormat":"openrouter",
		"supportsToolSearch":false,
		"deferredToolsMode":"kimi",
		"chatTemplateKwargs":{"enable_thinking":false},
		"openRouterRouting":{"only":["one"]},
		"vercelGatewayRouting":{"order":["two"]}
	}`)
	entry := CachedCatalog{
		Models: []CachedModel{{
			Provider: "fixture", ID: "full", Name: "Full fixture", API: provider.OpenAICompletionsAPI,
			BaseURL: "https://fixture.test/v1", Headers: map[string]string{"X-Model": "full"}, Reasoning: true,
			ThinkingLevelMap: map[provider.ThinkingLevel]*string{provider.ThinkingOff: nil, provider.ThinkingHigh: &high},
			Input:            []provider.InputKind{provider.InputText, provider.InputImage},
			Cost:             provider.CostRates{Input: 1, Output: 2, CacheRead: 0.25, CacheWrite: 3, Tiers: []provider.CostTier{{InputTokensAbove: 100_000, Input: 4, Output: 5, CacheRead: 0.5, CacheWrite: 6}}},
			ContextWindow:    200_000, MaxTokens: 8_000, Compat: compat,
		}},
		ETag: `"opaque"`, LastModified: &lastModified, CheckedAt: &checkedAt,
	}
	if err := store.Write(context.Background(), "fixture", entry); err != nil {
		t.Fatal(err)
	}
	restored, ok, err := store.Read(context.Background(), "fixture")
	if err != nil || !ok || len(restored.Models) != 1 {
		t.Fatalf("Read = (%#v, %t, %v)", restored, ok, err)
	}
	model := restored.Models[0]
	if model.Provider != "fixture" || model.ID != "full" || model.Name != "Full fixture" || model.API != provider.OpenAICompletionsAPI || model.BaseURL != "https://fixture.test/v1" || !model.Reasoning || model.ContextWindow != 200_000 || model.MaxTokens != 8_000 {
		t.Fatalf("model identity/capability fields = %#v", model)
	}
	if len(model.Input) != 2 || model.Input[1] != provider.InputImage || model.Headers["X-Model"] != "full" || len(model.Cost.Tiers) != 1 || model.Cost.Tiers[0].Output != 5 {
		t.Fatalf("model collection/cost fields = %#v", model)
	}
	if value, present := model.ThinkingLevelMap[provider.ThinkingOff]; !present || value != nil {
		t.Fatalf("explicit disabled thinking level = %#v", model.ThinkingLevelMap)
	}
	if value := model.ThinkingLevelMap[provider.ThinkingHigh]; value == nil || *value != "high" {
		t.Fatalf("mapped thinking level = %#v", model.ThinkingLevelMap)
	}
	if restored.LastModified == nil || *restored.LastModified != 0 || restored.CheckedAt == nil || *restored.CheckedAt != 0 {
		t.Fatalf("optional timestamp presence = %#v", restored)
	}
	var compatObject map[string]json.RawMessage
	if err := json.Unmarshal(model.Compat, &compatObject); err != nil {
		t.Fatal(err)
	}
	if raw, present := compatObject["supportsDeveloperRole"]; !present || string(raw) != "false" {
		t.Fatalf("explicit false compat presence = %#v", compatObject)
	}

	writeFile(t, filepath.Join(directory, "models.json"), `{"providers":{"fixture":{"api":"openai-completions","baseUrl":"https://fixture.test/v1","apiKey":"key"}}}`)
	runtimeModel, err := NewRuntime(Options{AgentDir: directory, ModelsStorePath: path})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := runtimeModel.Resolve(Selection{Provider: "fixture", Model: "full"})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := resolved.Model.Ref()
	if err != nil {
		t.Fatal(err)
	}
	typed := ref.Compat().OpenAICompletions
	if typed == nil || typed.SupportsDeveloperRole == nil || *typed.SupportsDeveloperRole || typed.SupportsUsageInStreaming == nil || !*typed.SupportsUsageInStreaming || typed.ChatTemplateKwargs["enable_thinking"] != false {
		t.Fatalf("typed compat = %#v", typed)
	}
}

func TestModelsStoreDuplicateModelsUseLastDynamicOverlay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows v0.1 fails closed for private durable configuration")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "models-store.json")
	writeFile(t, path, `{"fixture":{"models":[{"provider":"fixture","id":"same","name":"first","api":"openai-responses","reasoning":false},{"provider":"fixture","id":"same","name":"second","api":"openai-responses","reasoning":true}]}}`)
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	restored, ok, err := store.Read(context.Background(), "fixture")
	if err != nil || !ok || len(restored.Models) != 2 {
		t.Fatalf("Read = (%#v, %t, %v)", restored, ok, err)
	}
	writeFile(t, filepath.Join(directory, "models.json"), `{"providers":{"fixture":{"api":"openai-responses","baseUrl":"https://fixture.example/v1","apiKey":"key"}}}`)
	models, err := NewRuntime(Options{AgentDir: directory, ModelsStorePath: path})
	if err != nil {
		t.Fatal(err)
	}
	selected, ok := models.GetModel("fixture", "same")
	if !ok || selected.Name != "second" || !selected.Reasoning {
		t.Fatalf("last cached overlay = %#v, %t", selected, ok)
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

func TestStoreExactProviderKeyAndCompleteModelRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows v0.1 fails closed for private durable configuration")
	}
	path := filepath.Join(t.TempDir(), "models-store.json")
	writeFile(t, path, `{"OpenAI":{"models":[{"provider":"OpenAI","id":"model","name":"old","api":"openai-responses","baseUrl":"https://old.invalid/v1","reasoning":false,"headers":{"X-Old":"old"},"futureModel":{"keep":true}}],"checkedAt":1,"futureEntry":{"keep":true}}}`)
	headers := map[string]string{"X-Secret": "header-secret"}
	entry := CachedCatalog{Models: []CachedModel{{Provider: "OpenAI", ID: "model", Name: "New model", API: OpenAIResponsesAPI, BaseURL: "https://new.invalid/v1", Reasoning: true, Headers: headers}}, CheckedAt: int64Pointer(2), ETag: "new-etag", LastModified: int64Pointer(3)}
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(context.Background(), "OpenAI", entry); err != nil {
		t.Fatal(err)
	}
	headers["X-Secret"] = "mutated-after-write"
	first, ok, err := s.Read(context.Background(), "OpenAI")
	if err != nil || !ok || len(first.Models) != 1 {
		t.Fatalf("read = %#v, %t, %v", first, ok, err)
	}
	if first.CheckedAt == nil || *first.CheckedAt != 2 || first.ETag != "new-etag" || first.LastModified == nil || *first.LastModified != 3 {
		t.Fatalf("catalog metadata round trip = %#v", first)
	}
	model := first.Models[0]
	if model.Provider != "OpenAI" || model.ID != "model" || model.Name != "New model" || model.API != OpenAIResponsesAPI || model.BaseURL != "https://new.invalid/v1" || !model.Reasoning || model.Headers["X-Secret"] != "header-secret" {
		t.Fatalf("model round trip = %#v", model)
	}
	first.Models[0].Headers["X-Secret"] = "mutated-read"
	second, _, err := s.Read(context.Background(), "OpenAI")
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
	if len(root) != 1 || root["OpenAI"] == nil || !strings.Contains(string(content), `"futureModel"`) || !strings.Contains(string(content), `"futureEntry"`) {
		t.Fatalf("exact-key write or opaque preservation failed: %s", content)
	}
	bad := entry
	bad.Models = append([]CachedModel(nil), entry.Models...)
	bad.Models[0] = cloneCachedModel(bad.Models[0])
	bad.Models[0].Headers["X-Bad"] = "do-not-leak\nsecret"
	if err := s.Write(context.Background(), "OpenAI", bad); err == nil || strings.Contains(err.Error(), "do-not-leak") {
		t.Fatalf("invalid header diagnostic = %v", err)
	}
	if err := s.Delete(context.Background(), "OpenAI"); err != nil {
		t.Fatalf("exact-key delete = %v", err)
	}
	if _, ok, err := s.Read(context.Background(), "OpenAI"); err != nil || ok {
		t.Fatalf("exact-key delete left entry: %t, %v", ok, err)
	}
}

func TestStoreKeepsCaseDistinctProviderKeys(t *testing.T) {
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
	upper, upperOK, err := s.Read(context.Background(), "OpenAI")
	if err != nil || !upperOK || upper.CheckedAt == nil || *upper.CheckedAt != 1 {
		t.Fatalf("upper read = %#v, %t, %v", upper, upperOK, err)
	}
	lower, lowerOK, err := s.Read(context.Background(), "openai")
	if err != nil || !lowerOK || lower.CheckedAt == nil || *lower.CheckedAt != 2 {
		t.Fatalf("lower read = %#v, %t, %v", lower, lowerOK, err)
	}
	if _, ok, err := s.Read(context.Background(), "OPENAI"); err != nil || ok {
		t.Fatalf("folded lookup = %t, %v", ok, err)
	}
	if err := s.Write(context.Background(), "openai", cached("openai", "new")); err != nil {
		t.Fatalf("lower write = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(after), `"OpenAI"`) || !strings.Contains(string(after), `"openai"`) {
		t.Fatalf("case-distinct store = %q, %v", after, err)
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
	directory := t.TempDir()
	path := filepath.Join(directory, "models-store.json")
	writeFile(t, path, `{"broken":null,"cached":{"models":[{"provider":"cached","id":"m1","api":"openai-responses"}],"checkedAt":1},"orphan":{"models":[{"provider":"orphan","id":"m2","api":"openai-responses"}],"checkedAt":1}}`)
	writeFile(t, filepath.Join(directory, "models.json"), `{"providers":{"cached":{"api":"openai-responses","baseUrl":"https://cached.test/v1","apiKey":"key"}}}`)
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
	if _, ok := r.GetProvider("orphan"); ok {
		t.Fatal("unregistered cache entry created a provider")
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

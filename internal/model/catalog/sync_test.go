package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Frozen public v1 response excerpts from https://models.dev/api.json,
// captured 2026-11-05. These are independent of the generated built-in snapshot.
func loadSyncFixture(t *testing.T) (Policy, []byte) {
	t.Helper()
	policyJSON, err := os.ReadFile("../catalogdata/policy.json")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := DecodePolicy(policyJSON)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../testdata/catalog-sync.json")
	if err != nil {
		t.Fatal(err)
	}
	return policy, raw
}

func TestSyncModelsDevRoutingControlsAndMetadata(t *testing.T) {
	policy, raw := loadSyncFixture(t)
	before, _ := json.Marshal(policy)
	requests := 0
	client := fixtureClient(t, raw, &requests)
	doc, err := syncCatalog(context.Background(), client, policy, "")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || doc.Sources.Models.Name != "models.dev" || doc.Sources.Models.URL != ModelsDevURL ||
		!strings.HasPrefix(doc.Sources.Models.Integrity, "sha256-") || doc.Sources.Policy.Name != "pi-go" {
		t.Fatalf("unexpected source provenance/requests: %#v, %d", doc.Sources, requests)
	}
	if !reflect.DeepEqual(doc.Defaults, policy.Defaults) || len(doc.Providers) != len(policy.Providers) {
		t.Fatal("remote data changed local default ordering or provider scope")
	}
	for _, p := range doc.Providers {
		if len(p.Models) != 1 || p.Models[p.API] == nil {
			t.Fatalf("unexpected protocol for %s: %#v", p.ID, p.Models)
		}
	}
	openai := modelFields(t, doc, "openai", "gpt-5.4")
	if openai["api"] != "openai-responses" || openai["baseUrl"] != "https://api.openai.com/v1" ||
		openai["contextWindow"] != float64(272000) || openai["maxTokens"] != float64(128000) {
		t.Fatalf("OpenAI local protocol/context policy lost: %#v", openai)
	}
	cost := openai["cost"].(map[string]any)
	tier := cost["tiers"].([]any)[0].(map[string]any)
	if cost["input"] != 2.5 || cost["cacheRead"] != 0.25 || tier["inputTokensAbove"] != float64(272000) || tier["output"] != 22.5 {
		t.Fatalf("model prices or long-context tiers lost: %#v", cost)
	}
	if openai["compat"].(map[string]any)["supportsToolSearch"] != true || openai["thinkingLevelMap"].(map[string]any)["off"] != "none" {
		t.Fatal("OpenAI runtime controls lost")
	}
	// New entries derive facts/effort controls from the public catalog. Models
	// without local corrections must not inherit an unrelated model's limits.
	newModel := modelFields(t, doc, "openai", "gpt-6.1-terra")
	if newModel["contextWindow"] != float64(1050000) || newModel["thinkingLevelMap"].(map[string]any)["max"] != "max" {
		t.Fatalf("new model metadata not imported: %#v", newModel)
	}
	azure := modelFields(t, doc, "azure-openai-responses", "gpt-5.4")
	if azure["provider"] != "azure-openai-responses" || azure["api"] != "azure-openai-responses" || azure["baseUrl"] != "" ||
		azure["thinkingLevelMap"].(map[string]any)["off"] != nil {
		t.Fatalf("Azure registration/controls changed: %#v", azure)
	}
	for _, id := range []string{"deepseek-v4-pro", "claude-opus-4-8"} {
		if modelJSON(doc, "azure-openai-responses", id) != nil {
			t.Fatalf("Azure Foundry model %s enabled without an adapter", id)
		}
	}
	for p, id := range map[string]string{"openai": "gpt-5.6", "xai": "grok-4.20-0309-reasoning", "together": "Qwen/Qwen3-Coder-Next-FP8"} {
		if modelJSON(doc, p, id) != nil {
			t.Fatalf("unsupported/deprecated model %s/%s entered catalog", p, id)
		}
	}
	fable := modelFields(t, doc, "anthropic", "claude-fable-5")
	if fable["compat"].(map[string]any)["forceAdaptiveThinking"] != true ||
		len(fable["compat"].(map[string]any)["allowedFallbackModels"].([]any)) != 2 || fable["promptCache"] == nil {
		t.Fatal("Anthropic compatibility/custom metadata lost")
	}
	deepseek := modelFields(t, doc, "deepseek", "deepseek-v4-pro")
	if deepseek["cost"].(map[string]any)["input"] != 0.66 || deepseek["contextWindow"] != float64(1048576) ||
		deepseek["compat"].(map[string]any)["thinkingFormat"] != "deepseek" {
		t.Fatal("DeepSeek facts did not refresh independently of request controls")
	}
	together := modelFields(t, doc, "together", "MiniMaxAI/MiniMax-M3")
	if together["provider"] != "together" || together["baseUrl"] != "https://api.together.ai/v1" ||
		together["compat"].(map[string]any)["thinkingFormat"] != "together" ||
		together["compat"].(map[string]any)["supportsReasoningEffort"] != false {
		t.Fatal("Together alias mapping or toggle protocol lost")
	}
	// The raw input, including source-only fields, survives without being
	// confused with our request protocol or configuration.
	var source map[string]struct{ Models map[string]json.RawMessage }
	if err := json.Unmarshal(raw, &source); err != nil {
		t.Fatal(err)
	}
	var projected map[string]json.RawMessage
	_ = json.Unmarshal(modelJSON(doc, "openai", "gpt-5.4"), &projected)
	if !equalJSON(projected["modelsDev"], source["openai"].Models["gpt-5.4"]) {
		t.Fatal("original models.dev metadata was not retained")
	}
	for _, rule := range policy.Providers {
		if rule.ID == "openai-codex" {
			for _, raw := range rule.Models {
				var id struct{ ID string }
				_ = json.Unmarshal(raw, &id)
				if !equalJSON(raw, modelJSON(doc, rule.ID, id.ID)) {
					t.Fatalf("local subscription model changed: %s", id.ID)
				}
			}
		}
	}
	if modelJSON(doc, "openai-codex", "gpt-6.1-terra") != nil {
		t.Fatal("public API availability invented Codex subscription availability")
	}
	after, _ := json.Marshal(policy)
	if !bytes.Equal(before, after) {
		t.Fatal("sync mutated the locally shipped policy")
	}
}

func TestSyncFiltersSpecializedModelsAndPreservesUnknownFields(t *testing.T) {
	policy, raw := loadSyncFixture(t)
	var source map[string]any
	_ = json.Unmarshal(raw, &source)
	models := source["openai"].(map[string]any)["models"].(map[string]any)
	for id, patch := range map[string]map[string]any{
		"decision": {"type": "decision"}, "image-generator": {"modalities": map[string]any{"output": []string{"image"}}},
		"no-tools": {"tool_call": false}, "new-chat": {"future_capability": map[string]any{"enabled": true}},
	} {
		model := map[string]any{"id": id, "tool_call": true, "limit": map[string]any{"context": 8000, "output": 1000}}
		for key, value := range patch {
			model[key] = value
		}
		models[id] = model
	}
	raw, _ = json.Marshal(source)
	doc, err := projectModelsDev(context.Background(), policy, raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"decision", "image-generator", "no-tools"} {
		if modelJSON(doc, "openai", id) != nil {
			t.Fatalf("non-chat/tool-capable model %s imported", id)
		}
	}
	chat := modelFields(t, doc, "openai", "new-chat")
	if chat["modelsDev"].(map[string]any)["future_capability"].(map[string]any)["enabled"] != true {
		t.Fatal("unknown source metadata was discarded")
	}
}

func TestNewAnthropicBudgetAndEffortDoesNotEnableAdaptiveThinking(t *testing.T) {
	policy, raw := loadSyncFixture(t)
	var source map[string]any
	_ = json.Unmarshal(raw, &source)
	models := source["anthropic"].(map[string]any)["models"].(map[string]any)
	models["budget-and-effort"] = map[string]any{
		"id": "budget-and-effort", "tool_call": true, "reasoning": true,
		"reasoning_options": []any{
			map[string]any{"type": "budget_tokens", "min": 1024},
			map[string]any{"type": "effort", "values": []string{"low", "medium", "high"}},
		},
	}
	raw, _ = json.Marshal(source)
	doc, err := projectModelsDev(context.Background(), policy, raw)
	if err != nil {
		t.Fatal(err)
	}
	model := modelFields(t, doc, "anthropic", "budget-and-effort")
	if model["compat"].(map[string]any)["forceAdaptiveThinking"] == true || model["thinkingLevelMap"] != nil {
		t.Fatal("effort support incorrectly replaced token-budget thinking")
	}
}

func TestSyncRejectsInvalidOrIncompleteSource(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing provider", func(s map[string]any) { delete(s, "groq") }},
		{"wrong provider identity", func(s map[string]any) { s["openai"].(map[string]any)["id"] = "other" }},
		{"null models", func(s map[string]any) { s["openai"].(map[string]any)["models"] = nil }},
		{"wrong model identity", func(s map[string]any) { sourceTestModel(s)["id"] = "other" }},
		{"missing model identity", func(s map[string]any) { delete(sourceTestModel(s), "id") }},
		{"negative cost", func(s map[string]any) { sourceTestModel(s)["cost"] = map[string]any{"input": -1} }},
		{"invalid cost type", func(s map[string]any) { sourceTestModel(s)["cost"] = map[string]any{"input": "free"} }},
		{"negative context", func(s map[string]any) { sourceTestModel(s)["limit"] = map[string]any{"context": -1} }},
		{"invalid tier", func(s map[string]any) {
			sourceTestModel(s)["cost"] = map[string]any{"tiers": []any{map[string]any{"tier": map[string]any{"type": "context", "size": -1}}}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy, raw := loadSyncFixture(t)
			var source map[string]any
			_ = json.Unmarshal(raw, &source)
			test.change(source)
			raw, _ = json.Marshal(source)
			doc, err := projectModelsDev(context.Background(), policy, raw)
			if err == nil || len(doc.Providers) != 0 {
				t.Fatalf("invalid source produced a partial catalog: %#v, %v", doc, err)
			}
		})
	}
	for _, invalid := range []string{"null", "{}", "[]", "{", `{"openai":[]}`} {
		policy, _ := loadSyncFixture(t)
		if _, err := projectModelsDev(context.Background(), policy, []byte(invalid)); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
}

func TestSyncExplicitEmptyProviderAndReproducibleReplay(t *testing.T) {
	policy, raw := loadSyncFixture(t)
	var source map[string]any
	_ = json.Unmarshal(raw, &source)
	source["openai"].(map[string]any)["models"] = map[string]any{}
	raw, _ = json.Marshal(source)
	requests := 0
	downloaded, err := syncCatalog(context.Background(), fixtureClient(t, raw, &requests), policy, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range downloaded.Providers {
		if p.ID == "openai" && len(p.Models[p.API]) != 0 {
			t.Fatal("explicitly empty provider retained removed models")
		}
	}
	input := filepath.Join(t.TempDir(), "models.dev.json")
	if err := os.WriteFile(input, raw, 0600); err != nil {
		t.Fatal(err)
	}
	// Nil client proves offline replay cannot use a network fallback.
	replayed, err := syncCatalog(context.Background(), nil, policy, input)
	if err != nil || !reflect.DeepEqual(downloaded, replayed) {
		t.Fatalf("replay differs from network snapshot: %v", err)
	}
	target := filepath.Join(t.TempDir(), Filename)
	for i, want := range []bool{true, false} {
		changed, err := Write(context.Background(), target, replayed)
		if err != nil || changed != want {
			t.Fatalf("publication %d = %t, %v", i, changed, err)
		}
	}
}

func TestSyncHTTPFailureAndCancellation(t *testing.T) {
	policy, _ := loadSyncFixture(t)
	for _, status := range []int{http.StatusNotModified, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		client := &http.Client{Transport: syncRoundTripper(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("not a catalog")), Header: make(http.Header)}, nil
		})}
		if _, err := syncCatalog(context.Background(), client, policy, ""); err == nil || !strings.Contains(err.Error(), fmt.Sprint(status)) {
			t.Fatalf("HTTP %d did not fail explicitly: %v", status, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := syncCatalog(ctx, nil, policy, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestLegacyInstalledSnapshotMigratesWithoutNetwork(t *testing.T) {
	policy, raw := loadSyncFixture(t)
	doc, err := projectModelsDev(context.Background(), policy, raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(doc)
	var legacy map[string]any
	_ = json.Unmarshal(encoded, &legacy)
	legacy["schemaVersion"] = 1
	legacy["sources"] = map[string]any{
		"models":   map[string]any{"package": "legacy-models", "version": "1.0", "integrity": "sha512-old-models"},
		"defaults": map[string]any{"package": "legacy-defaults", "version": "1.0", "integrity": "sha512-old-defaults"},
	}
	encoded, _ = json.Marshal(legacy)
	migrated, err := Decode(encoded)
	if err != nil || migrated.SchemaVersion != SchemaVersion || migrated.Sources.Models.Name != "legacy-models" ||
		!reflect.DeepEqual(migrated.Defaults, doc.Defaults) {
		t.Fatalf("installed snapshot migration changed content: %v", err)
	}
	diff := Compare(doc, migrated)
	if len(diff.Added)+len(diff.Removed)+len(diff.Changed)+len(diff.Providers) != 0 {
		t.Fatalf("installed snapshot migration changed models: %#v", diff)
	}
}

func sourceTestModel(source map[string]any) map[string]any {
	return source["openai"].(map[string]any)["models"].(map[string]any)["gpt-5.4"].(map[string]any)
}

func modelJSON(doc Document, provider, id string) json.RawMessage {
	for _, p := range doc.Providers {
		if p.ID == provider {
			for _, models := range p.Models {
				if raw := models[id]; raw != nil {
					return raw
				}
			}
		}
	}
	return nil
}

func modelFields(t *testing.T, doc Document, provider, id string) map[string]any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(modelJSON(doc, provider, id), &fields); err != nil {
		t.Fatalf("missing/invalid %s/%s: %v", provider, id, err)
	}
	return fields
}

type syncRoundTripper func(*http.Request) (*http.Response, error)

func (f syncRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func fixtureClient(t *testing.T, raw []byte, requests *int) *http.Client {
	t.Helper()
	return &http.Client{Transport: syncRoundTripper(func(request *http.Request) (*http.Response, error) {
		*requests++
		if request.URL.String() != ModelsDevURL || request.Method != http.MethodGet || request.Header.Get("Authorization") != "" {
			return nil, fmt.Errorf("unexpected request: %s %s", request.Method, request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(raw)), Header: make(http.Header)}, nil
	})}
}

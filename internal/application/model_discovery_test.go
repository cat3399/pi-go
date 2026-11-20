package application

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
	modelcatalog "github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/model/catalog"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestDiscoverModelsResolvesDraftAuthAndNormalizesCommonCatalogShapes(t *testing.T) {
	var requestedPath, authorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestedPath = request.URL.RequestURI()
		authorization = request.Header.Get("Authorization")
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": []any{
			map[string]any{"id": "model-z", "name": "Zulu"},
			map[string]any{"model": "models/model-a", "displayName": "Alpha"},
			"model-z",
		}})
	}))
	t.Cleanup(upstream.Close)

	cwd := t.TempDir()
	service, err := NewService(ServiceOptions{
		Production: app.ProductionConfig{
			WorkingDir: cwd, AgentDir: filepath.Join(t.TempDir(), "agent"),
			Environment: []string{"DISCOVERY_KEY=fixture-secret"},
		},
		ModelHTTP: upstream.Client(), DisableReaper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })

	result, err := service.DiscoverModels(context.Background(), "fixture", ModelProviderDraft{
		BaseURL: upstream.URL + "/v1", API: "openai-completions", APIKey: "$DISCOVERY_KEY",
	})
	if err != nil {
		t.Fatal(err)
	}
	if requestedPath != "/v1/models" || authorization != "Bearer fixture-secret" {
		t.Fatalf("request = %q, auth present = %v", requestedPath, authorization != "")
	}
	if len(result.Models) != 2 || result.Models[0].ID != "model-a" || result.Models[0].Name != "Alpha" || result.Models[1].ID != "model-z" {
		t.Fatalf("models = %#v", result.Models)
	}
}

func TestPresetLookupUsesExactBuiltinIDsAndDisambiguatesByChannel(t *testing.T) {
	builtins := []modelcatalog.Model{
		{Provider: "openai", API: "openai-responses", ID: "shared", Name: "OpenAI", Reasoning: true},
		{Provider: "azure-openai-responses", API: "azure-openai-responses", ID: "shared", Name: "Azure", Reasoning: true},
		{Provider: "first", BaseURL: "https://first.test/v1", API: "openai-completions", ID: "vendor/model", Name: "First", Reasoning: true},
		{Provider: "second", BaseURL: "https://second.test/v1", API: "openai-completions", ID: "vendor/model", Name: "Second"},
	}
	index := newModelPresetIndex(builtins)
	for _, tc := range []struct{ id, channel, api, url, wantName string }{
		{"shared", "gateway", "openai-completions", "", "OpenAI"},
		{"shared", "gateway", "azure-openai-responses", "", "Azure"},
		{"openai/shared", "gateway", "openai-completions", "", "OpenAI"},
		{"vendor/model", "first", "openai-completions", "", "First"},
		{"vendor/model", "gateway", "openai-completions", "https://second.test/v2", "Second"},
		{"vendor/model", "gateway", "openai-completions", "https://first.test.other.test", ""},
		{"SHARED", "openai", "openai-responses", "", ""},
		{"shared-latest", "openai", "openai-responses", "", ""},
	} {
		got := index.lookup(tc.id, tc.channel, catalogHostname(tc.url), tc.api)
		if got.ID != tc.id || got.Name != tc.wantName || (got.Reasoning != nil) != (tc.wantName != "") {
			t.Errorf("%+v: got %+v", tc, got)
		} else if tc.wantName == "Second" && *got.Reasoning {
			t.Error("non-reasoning match acquired reasoning")
		}
	}
}

func TestTypedAndDiscoveredModelsCarryInstalledCapabilitiesIntoCustomChannelSession(t *testing.T) {
	ctx := context.Background()
	cwd, agentDir := t.TempDir(), t.TempDir()
	// Add an installed-only model; a lookup in the embedded or effective
	// (user-overridden) catalog would yield the wrong thinking levels.
	doc, err := catalog.Read(filepath.Join("..", "model", "catalogdata", "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range doc.Providers {
		if p.ID == "openai" {
			p.Models["openai-responses"]["catalog-index"] = json.RawMessage(`{"provider":"openai","api":"openai-responses","baseUrl":"https://builtin.test/v1","id":"installed-only","name":"Builtin","input":["text","image"],"contextWindow":90000,"maxTokens":7000,"reasoning":true,"thinkingLevelMap":{"minimal":null,"max":"peak"},"cost":{"input":7},"headers":{"X-Builtin":"not-portable"},"compat":{"supportsWebsockets":true}}`)
		}
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, catalog.Filename), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(`{"providers":{"openai":{"enabled":false,"modelOverrides":{"installed-only":{"reasoning":false}}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"openai/installed-only","name":"Gateway label","baseUrl":"https://untrusted.test"}]}`))
	}))
	defer upstream.Close()
	service, err := NewService(ServiceOptions{Production: app.ProductionConfig{WorkingDir: cwd, AgentDir: agentDir, Environment: []string{}}, ModelHTTP: upstream.Client(), DisableReaper: true})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(ctx)
	draft := ModelProviderDraft{BaseURL: upstream.URL + "/v1", API: "openai-completions", APIKey: "gateway-key"}
	typed, err := service.LookupModelPresets(ctx, ModelPresetRequest{ProviderID: "gateway", Models: []ModelPresetQuery{
		{ID: "openai/installed-only", BaseURL: draft.BaseURL, API: draft.API}, {ID: "unlisted"},
	}})
	if err != nil || len(typed) != 2 || typed[0].Name != "Builtin" || !reflect.DeepEqual(typed[1], DiscoveredModel{ID: "unlisted"}) || requests.Load() != 0 {
		t.Fatalf("offline lookup: %+v, %v, upstream requests: %d", typed, err, requests.Load())
	}
	result, err := service.DiscoverModels(ctx, "gateway", draft)
	if err != nil || !reflect.DeepEqual(result.Models, []DiscoveredModel{{ID: "openai/installed-only", Name: "Gateway label"}}) {
		t.Fatalf("discovery should return only the channel's identity: %+v, %v", result, err)
	}
	selected, err := service.LookupModelPresets(ctx, ModelPresetRequest{ProviderID: "gateway", Models: []ModelPresetQuery{
		{ID: result.Models[0].ID, BaseURL: draft.BaseURL, API: draft.API},
	}})
	if err != nil || len(selected) != 1 || !reflect.DeepEqual(selected[0], typed[0]) {
		t.Fatalf("explicit lookup for a selected ID: %+v, %v", selected, err)
	}
	selected[0].Name = result.Models[0].Name // Keep the name supplied by the channel.
	for _, preset := range []DiscoveredModel{typed[0], selected[0]} {
		config, err := json.Marshal(map[string]any{"api": draft.API, "baseUrl": draft.BaseURL, "apiKey": draft.APIKey, "models": []DiscoveredModel{preset}})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.WriteModelProvider(ctx, "gateway", config); err != nil {
			t.Fatal(err)
		}
		wantLevels := []provider.ThinkingLevel{provider.ThinkingOff, provider.ThinkingLow, provider.ThinkingMedium, provider.ThinkingHigh, provider.ThinkingMax}
		view, err := service.ListModels(ctx, cwd)
		if err != nil || len(view.Models) != 1 || !slices.Equal(view.Models[0].ThinkingLevels, wantLevels) {
			t.Fatalf("available models = %+v, %v", view.Models, err)
		}
		state, err := service.NewSession(ctx, NewSessionOptions{CWD: cwd, Provider: "gateway", ModelID: "openai/installed-only"})
		if err != nil {
			t.Fatal(err)
		}
		m := state.Model
		if m.Provider() != "gateway" || m.ID() != "openai/installed-only" || m.Name() != preset.Name || m.API() != draft.API || m.BaseURL() != draft.BaseURL || len(m.Headers()) != 0 || m.Cost().Input != 0 || m.Compat().OpenAIResponses != nil {
			t.Fatal("import changed channel identity, routing, price or headers")
		}
		if m.ContextWindow() != 90000 || m.MaxTokens() != 7000 || !slices.Contains(m.Input(), provider.InputImage) || !slices.Equal(m.SupportedThinkingLevels(), wantLevels) {
			t.Fatalf("session lost imported capabilities: %v", m.SupportedThinkingLevels())
		}
		if effort, ok := m.ThinkingEffort(provider.ThinkingMax); !ok || effort != "peak" {
			t.Fatalf("mapped max effort = %q, %v", effort, ok)
		}
	}
}

func TestBuildModelsListURLMatchesProviderDialects(t *testing.T) {
	anthropic, err := buildModelsListURL("https://api.example.test", "anthropic-messages")
	if err != nil || anthropic.String() != "https://api.example.test/v1/models?limit=1000" {
		t.Fatalf("anthropic endpoint = %v, %v", anthropic, err)
	}
	google, err := buildModelsListURL("https://api.example.test/v1beta/", "google-generative-ai")
	if err != nil || google.String() != "https://api.example.test/v1beta/models?pageSize=1000" {
		t.Fatalf("google endpoint = %v, %v", google, err)
	}
}

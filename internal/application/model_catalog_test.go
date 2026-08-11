package application

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
)

func catalogFixture(t *testing.T) []ModelCatalogEntry {
	t.Helper()
	data := []byte(`{
  "openai": {"name":"OpenAI","models":{"gpt-5":{"id":"gpt-5","name":"GPT-5","reasoning":true,"modalities":{"input":["text","image","pdf"]},"limit":{"context":400000,"output":128000},"cost":{"input":1.25,"output":10,"cache_read":0.125}}}},
  "openrouter": {"name":"OpenRouter","api":"https://openrouter.ai/api/v1","models":{"openai/gpt-5":{"id":"openai/gpt-5","name":"GPT-5 via OpenRouter","limit":{"context":400000,"output":128000},"cost":{"input":1.3,"output":10.5,"cache_write":2}},"missing-price":{"id":"missing-price","name":"No Price","limit":{"context":32000,"output":8000}}}}
}`)
	var payload any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	return flattenModelsDevCatalog(payload)
}

func TestModelCatalogMatchesOriginalProviderAndBaseURLSelection(t *testing.T) {
	entries := catalogFixture(t)
	provider := recommendModelCatalogPreset(entries, "openai/gpt-5", "openrouter", "https://proxy.example.com/v1")
	if provider.MetadataMethod != "provider" || provider.MatchedProviderID != "openrouter" || provider.Price.Status != "reliable" || provider.Price.Method != "provider" || provider.Price.Cost == nil || *provider.Price.Cost.Input != 1.3 {
		t.Fatalf("provider recommendation = %#v", provider)
	}
	baseURL := recommendModelCatalogPreset(entries, "openai/gpt-5", "custom", "https://api.openai.com/v1")
	if baseURL.MetadataMethod != "base-url" || baseURL.MatchedProviderID != "openai" || baseURL.Price.Method != "base-url" {
		t.Fatalf("base URL recommendation = %#v", baseURL)
	}
	lookalike := recommendModelCatalogPreset(entries, "openai/gpt-5", "custom", "https://openai.com.example.test/v1")
	if lookalike.MetadataMethod != "consensus" {
		t.Fatalf("lookalike recommendation = %#v", lookalike)
	}
	search := searchModelCatalog(entries, "gpt-5", "openai", 50)
	if len(search) < 2 || search[0].ProviderID != "openai" {
		t.Fatalf("ranked search = %#v", search)
	}
}

func TestModelCatalogConsensusRefusesConflictingPrices(t *testing.T) {
	payload := map[string]any{}
	for _, item := range []struct {
		provider string
		input    float64
		output   float64
	}{
		{"a", 1, 2}, {"b", 1, 2}, {"c", 3, 4}, {"d", 3, 4},
	} {
		payload[item.provider] = map[string]any{"models": map[string]any{
			"model": map[string]any{"id": "model", "cost": map[string]any{"input": item.input, "output": item.output}},
		}}
	}
	recommendation := recommendModelCatalogPreset(flattenModelsDevCatalog(payload), "model", "custom", "")
	if recommendation.Price.Status != "unreliable" || recommendation.Price.Reason != "conflict" || recommendation.Price.Support != 2 || recommendation.Price.Total != 4 || recommendation.Preset.Cost != nil {
		t.Fatalf("conflict recommendation = %#v", recommendation)
	}
}

func TestQueryModelCatalogCachesSuccessfulRemoteCatalog(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"fixture": map[string]any{"models": map[string]any{"model": map[string]any{"id": "model"}}},
		})
	}))
	t.Cleanup(server.Close)
	cwd := t.TempDir()
	service, err := NewService(ServiceOptions{
		Production: app.ProductionConfig{WorkingDir: cwd, AgentDir: filepath.Join(t.TempDir(), "agent"), Environment: []string{}},
		ModelHTTP:  server.Client(), ModelCatalogURL: server.URL, DisableReaper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	for range 2 {
		result, err := service.QueryModelCatalog(context.Background(), "model", "fixture", "", 50)
		if err != nil || len(result.Models) != 1 || result.Source != server.URL {
			t.Fatalf("query result = %#v, %v", result, err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("catalog requests = %d", requests.Load())
	}
}

package application

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
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

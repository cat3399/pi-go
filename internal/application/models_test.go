package application

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/auth"
)

func TestListModelsUsesProductionAuthAndReturnsOnlyAvailableRoutes(t *testing.T) {
	cwd := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agent")
	store, err := auth.NewStore(auth.Options{Path: filepath.Join(agentDir, "auth.json")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKey(context.Background(), "deepseek", "test-deepseek-key", nil); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ServiceOptions{
		Production:    app.ProductionConfig{WorkingDir: cwd, AgentDir: agentDir, Environment: []string{}},
		DisableReaper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })

	snapshot, err := service.ListModels(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Models) == 0 {
		t.Fatal("configured DeepSeek provider returned no models")
	}
	for _, candidate := range snapshot.Models {
		if candidate.Provider != "deepseek" {
			t.Fatalf("unconfigured provider leaked into available models: %#v", candidate)
		}
	}
}

func TestUIThemeSettingsPersistWithoutLosingUnknownFields(t *testing.T) {
	cwd := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(`{"theme":"dark","future":{"keep":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ServiceOptions{
		Production:    app.ProductionConfig{WorkingDir: cwd, AgentDir: agentDir, Environment: []string{}},
		DisableReaper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })

	settings, err := service.GetUISettings(context.Background(), cwd)
	if err != nil || settings.Theme != "dark" {
		t.Fatalf("initial settings = %#v, %v", settings, err)
	}
	settings, err = service.SetTheme(context.Background(), cwd, "light/dark")
	if err != nil || settings.Theme != "light/dark" {
		t.Fatalf("updated settings = %#v, %v", settings, err)
	}
	data, err := os.ReadFile(filepath.Join(agentDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	var future map[string]bool
	if err := json.Unmarshal(root["future"], &future); err != nil {
		t.Fatal(err)
	}
	if string(root["theme"]) != `"light/dark"` || !future["keep"] {
		t.Fatalf("settings.json = %s", data)
	}
}

func TestModelsConfigRoundTripPreservesUnknownFields(t *testing.T) {
	cwd := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agent")
	service, err := NewService(ServiceOptions{
		Production:    app.ProductionConfig{WorkingDir: cwd, AgentDir: agentDir, Environment: []string{}},
		DisableReaper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })

	document := ModelsConfigDocument{
		"providers": json.RawMessage(`{"private-gateway":{"api":"openai-completions","models":[]}}`),
		"future":    json.RawMessage(`{"preserved":true}`),
	}
	if err := service.WriteModelsConfig(context.Background(), document); err != nil {
		t.Fatal(err)
	}
	loaded, err := service.ReadModelsConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var future map[string]bool
	if err := json.Unmarshal(loaded["future"], &future); err != nil || !future["preserved"] {
		t.Fatalf("future field = %s", loaded["future"])
	}
	if err := service.WriteModelsConfig(context.Background(), ModelsConfigDocument{
		"providers": json.RawMessage(`[]`),
	}); err == nil {
		t.Fatal("non-object providers value was accepted")
	}
}

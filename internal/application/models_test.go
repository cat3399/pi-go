package application

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/auth"
	modelcatalog "github.com/cat3399/pi-go/internal/model"
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
		if candidate.ProviderName != "DeepSeek" {
			t.Fatalf("available model provider name = %q, want DeepSeek", candidate.ProviderName)
		}
	}
}

func TestBuiltinProviderSelectionPreservesCustomConfigCredentialsAndCurrentModel(t *testing.T) {
	ctx := context.Background()
	cwd, agentDir := t.TempDir(), t.TempDir()
	service, err := NewService(ServiceOptions{Production: app.ProductionConfig{WorkingDir: cwd, AgentDir: agentDir, Environment: []string{}}, DisableReaper: true})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(ctx)
	if err := service.SetProviderAPIKey(ctx, "deepseek", "saved-key"); err != nil {
		t.Fatal(err)
	}
	custom := json.RawMessage(`{"api":"openai-completions","baseUrl":"http://127.0.0.1:1/v1","apiKey":"local-key","models":[{"id":"custom-model","future":{"keep":true}}],"future":"preserve"}`)
	document := ModelsConfigDocument{"providers": json.RawMessage(`{"deepseek":{"baseUrl":"https://api.deepseek.com","future":true}}`), "future": json.RawMessage(`{"retain":1}`)}
	if err := service.WriteModelsConfig(ctx, document); err != nil {
		t.Fatal(err)
	}
	if err := service.WriteModelProvider(ctx, "custom", custom); err != nil {
		t.Fatal(err)
	}
	models, err := service.ListProviderModels(ctx, cwd, "deepseek")
	if err != nil || len(models) == 0 {
		t.Fatalf("provider models = %v, %v", models, err)
	}
	state, err := service.NewSession(ctx, NewSessionOptions{CWD: cwd, Provider: "deepseek", ModelID: models[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetBuiltinProviderEnabled(ctx, "deepseek", BuiltinProviderUpdate{Enabled: true, Only: true}); err != nil {
		t.Fatal(err)
	}
	providers, err := service.ListModelProviders(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range providers {
		if info.Builtin && info.Enabled != (info.ID == "deepseek") {
			t.Fatalf("exclusive selection = %#v", info)
		}
		if info.ID == "custom" && !info.Enabled {
			t.Fatal("exclusive selection disabled custom provider")
		}
	}
	if err := service.SetBuiltinProviderEnabled(ctx, "deepseek", BuiltinProviderUpdate{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	available, err := service.ListModels(ctx, cwd)
	if err != nil || len(available.Models) != 1 || available.Models[0].Provider != "custom" {
		t.Fatalf("disabled builtin fallback = %#v, %v", available, err)
	}
	current, active, err := service.LiveState(state.SessionID)
	if err != nil || !active || !current.Model.Equal(state.Model) {
		t.Fatalf("configuration switched live model: %#v, %v", current, err)
	}
	result, err := service.Dispatch(ctx, state.SessionID, GetAvailableModelsCommand{})
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range result.(GetAvailableModelsResult).Models {
		if modelcatalog.IsBuiltinProvider(candidate.Provider()) {
			t.Fatalf("active selector retained disabled provider: %s", candidate.Provider())
		}
	}
	if err := service.SetBuiltinProviderEnabled(ctx, "deepseek", BuiltinProviderUpdate{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	result, err = service.Dispatch(ctx, state.SessionID, GetAvailableModelsCommand{})
	if err != nil || len(result.(GetAvailableModelsResult).Models) <= 1 {
		t.Fatalf("reenabled live selector = %#v, %v", result, err)
	}
	stored, err := service.ReadModelsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var storedProviders map[string]json.RawMessage
	if err := json.Unmarshal(stored["providers"], &storedProviders); err != nil {
		t.Fatal(err)
	}
	var before, after any
	_ = json.Unmarshal(custom, &before)
	_ = json.Unmarshal(storedProviders["custom"], &after)
	var future map[string]int
	_ = json.Unmarshal(stored["future"], &future)
	if !reflect.DeepEqual(before, after) || future["retain"] != 1 {
		t.Fatalf("unrelated configuration changed: %s", stored["providers"])
	}
	store, _ := auth.NewStore(auth.Options{Path: filepath.Join(agentDir, "auth.json")})
	credential, exists, err := store.Read(ctx, "deepseek")
	if err != nil || !exists || credential.Type != "api_key" {
		t.Fatalf("credential changed: %#v, %v", credential, err)
	}
}

func TestListModelProvidersIsolatesUnrelatedAuthCheckFailure(t *testing.T) {
	cwd := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agent")
	store, err := auth.NewStore(auth.Options{Path: filepath.Join(agentDir, "auth.json")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKey(context.Background(), "anthropic", "test-anthropic-key", nil); err != nil {
		t.Fatal(err)
	}
	// OAuth is a valid durable credential shape, but it is not supported by
	// DeepSeek. Its per-provider CheckAuth must not hide the healthy Anthropic
	// provider from an authentication-management listing.
	if err := store.SetOAuth(context.Background(), "deepseek", auth.OAuthCredential{
		Access: "access", Refresh: "refresh", Expires: 1,
	}); err != nil {
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

	providers, err := service.ListModelProviders(context.Background(), cwd)
	if err != nil {
		t.Fatalf("ListModelProviders() = %v", err)
	}
	byID := make(map[string]ProviderAuthInfo, len(providers))
	for _, info := range providers {
		byID[info.ID] = info
	}
	if got, ok := byID["anthropic"]; !ok || !got.Configured || got.Source != "stored credential" {
		t.Fatalf("healthy Anthropic provider = %#v, present=%v", got, ok)
	}
	if got, ok := byID["deepseek"]; !ok || got.Configured || got.Source != "" || got.CredentialType != "oauth" {
		t.Fatalf("failed DeepSeek provider must degrade to unconfigured without an error: %#v, present=%v", got, ok)
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

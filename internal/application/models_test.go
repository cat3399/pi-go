package application

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/auth"
	modelcatalog "github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/model/catalog"
	"github.com/cat3399/pi-go/internal/provider"
)

type modelSyncHTTPFunc func(*http.Request) (*http.Response, error)

func (f modelSyncHTTPFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestSyncBuiltinModelsRefreshesActiveSelectionWithoutSwitchingModels(t *testing.T) {
	ctx := context.Background()
	cwd, agentDir, logs := t.TempDir(), t.TempDir(), t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, logs)
	}
	raw, err := os.ReadFile("../model/testdata/catalog-sync.json")
	if err != nil {
		t.Fatal(err)
	}
	const newID = "fixture-model-sync"
	raw = bytes.ReplaceAll(raw, []byte("gpt-6.1-terra"), []byte(newID))
	responseCode := http.StatusOK
	client := modelSyncHTTPFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != catalog.ModelsDevURL || r.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected public catalog request: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: responseCode, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	})
	service, err := NewService(ServiceOptions{Production: app.ProductionConfig{WorkingDir: cwd, AgentDir: agentDir, Environment: []string{"OPENAI_API_KEY=fixture-key"}}, ModelHTTP: client, DisableReaper: true})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(ctx)
	state, err := service.NewSession(ctx, NewSessionOptions{CWD: cwd, Provider: "openai", ModelID: "gpt-5.4"})
	if err != nil {
		t.Fatal(err)
	}
	revision := service.CurrentRevision()
	result, err := service.SyncBuiltinModels(ctx)
	if err != nil || !result.Published {
		t.Fatalf("sync = %+v, %v", result, err)
	}
	events, err := service.SubscribeEvents(revision)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	if !slices.ContainsFunc(events.Replay, func(e Event) bool {
		c, ok := e.Value.(ConfigurationResourceEvent)
		return ok && c.Models && c.CWD == ""
	}) {
		t.Fatal("no global model invalidation for connected clients")
	}
	active, ok, err := service.LiveState(state.SessionID)
	if err != nil || !ok || !active.Model.Equal(state.Model) {
		t.Fatalf("sync switched active model: %+v, %v", active, err)
	}
	selection, err := service.Dispatch(ctx, state.SessionID, GetAvailableModelsCommand{})
	if err != nil || !slices.ContainsFunc(selection.(GetAvailableModelsResult).Models, func(m provider.Model) bool { return m.ID() == newID }) {
		t.Fatalf("active selection did not pick up the new catalog: %v", err)
	}
	details, err := service.ListProviderModels(ctx, cwd, "openai")
	if err != nil || !slices.ContainsFunc(details, func(m ProviderModelInfo) bool { return m.ID == newID }) {
		t.Fatalf("provider details did not pick up the new catalog: %v", err)
	}
	result, err = service.SyncBuiltinModels(ctx)
	if err != nil || result.Published || result.FullOutputPath != "" || !strings.Contains(result.Summary, "Already up to date") {
		t.Fatalf("identical sync = %+v, %v", result, err)
	}
	responseCode = http.StatusServiceUnavailable
	revision = service.CurrentRevision()
	if _, err := service.SyncBuiltinModels(ctx); err == nil || !strings.Contains(err.Error(), "503") || service.CurrentRevision() != revision {
		t.Fatalf("failed sync reported an update: %v", err)
	}
}

func TestPerModelThinkingDefaultsInConfigurationAndSessions(t *testing.T) {
	ctx := context.Background()
	cwd, dir := t.TempDir(), t.TempDir()
	settings := `{"defaultProvider":"gateway","defaultModel":"a","defaultThinkingLevel":"medium"}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewService(ServiceOptions{Production: app.ProductionConfig{WorkingDir: cwd, AgentDir: dir, Environment: []string{}}, DisableReaper: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	const config = `{"api":"openai-completions","baseUrl":"http://localhost:1/v1","apiKey":"fixture","models":[{"id":"a","reasoning":true,"thinkingLevelMap":{"low":null,"xhigh":"strong"},"defaultThinkingLevel":"xhigh"},{"id":"b","reasoning":true},{"id":"plain"}],"modelOverrides":{"b":{"defaultThinkingLevel":"low"}}}`
	if err := s.WriteModelProvider(ctx, "gateway", json.RawMessage(config)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"auto", "max", "low"} { // off-map, opt-in but unavailable, explicitly disabled.
		raw := strings.Replace(config, `"defaultThinkingLevel":"xhigh"`, `"defaultThinkingLevel":"`+bad+`"`, 1)
		if err := s.WriteModelProvider(ctx, "gateway", json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted unsupported default %q", bad)
		}
	}
	view, err := s.ListModels(ctx, cwd)
	if err != nil || len(view.Models) != 3 {
		t.Fatalf("models = %+v, %v", view.Models, err)
	}
	want := map[string]provider.ThinkingLevel{"a": provider.ThinkingXHigh, "b": provider.ThinkingLow, "plain": provider.ThinkingOff}
	for _, m := range view.Models {
		if want[m.ID] != m.DefaultThinkingLevel || !slices.Contains(m.ThinkingLevels, m.DefaultThinkingLevel) {
			t.Fatalf("bad projected default: %+v", m)
		}
	}
	state, err := s.NewSession(ctx, NewSessionOptions{CWD: cwd})
	if err != nil || state.Model.ID() != "a" || state.ThinkingLevel != want["a"] {
		t.Fatalf("initial level = %s on %s, %v", state.ThinkingLevel, state.Model.ID(), err)
	}
	for _, id := range []string{"b", "plain", "a"} {
		if _, err := s.Dispatch(ctx, state.SessionID, SetModelCommand{Provider: "gateway", ModelID: id}); err != nil {
			t.Fatal(err)
		}
		got, _, err := s.LiveState(state.SessionID)
		if err != nil || got.ThinkingLevel != want[id] {
			t.Fatalf("switched to %s: %s, %v", id, got.ThinkingLevel, err)
		}
	}
	override := provider.ThinkingHigh
	state, err = s.NewSession(ctx, NewSessionOptions{CWD: cwd, Provider: "gateway", ModelID: "a", ThinkingLevel: &override})
	if err != nil || state.ThinkingLevel != override || state.Model.DefaultThinkingLevel() != want["a"] {
		t.Fatalf("session override changed model default: %s, %v", state.ThinkingLevel, err)
	}
	stored, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	var defaults struct{ DefaultThinkingLevel string }
	if err != nil || json.Unmarshal(stored, &defaults) != nil || defaults.DefaultThinkingLevel != "medium" {
		t.Fatalf("model-specific preferences leaked into global settings: %s, %v", stored, err)
	}
}

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

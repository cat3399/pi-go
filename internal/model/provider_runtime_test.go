package model

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"

	"github.com/cat3399/pi-go/internal/provider"
)

type providerRuntimeAuthStub struct {
	checks   []string
	resolved []string
}

type providerRuntimeCredentialStub struct {
	providerRuntimeAuthStub
	credential *ProviderCredential
}

func (s *providerRuntimeCredentialStub) ReadCredential(context.Context, ProviderConfig) (*ProviderCredential, error) {
	return cloneProviderCredential(s.credential), nil
}

func (s *providerRuntimeAuthStub) Check(_ context.Context, config ProviderConfig) (*AuthCheck, error) {
	s.checks = append(s.checks, config.ID)
	return &AuthCheck{Source: "test", Type: "api_key"}, nil
}

func (s *providerRuntimeAuthStub) Resolve(_ context.Context, config ProviderConfig, selected provider.Model, overrides AuthOverrides) (*AuthResult, error) {
	s.resolved = append(s.resolved, config.ID+"/"+selected.ID())
	return &AuthResult{
		APIKey: "owned-key", BaseURL: "https://resolved.example/v1", Source: "test", Type: "api_key",
		Headers: map[string]string{"Authorization": "Bearer owned", "X-Auth": "owned"},
		Env:     map[string]string{"OWNED": "yes", "SHARED": "auth"},
	}, nil
}

func TestModelsRuntimeOwnsProviderAuthAvailabilityAndDispatch(t *testing.T) {
	agentDir := t.TempDir()
	adapter, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	auth := &providerRuntimeAuthStub{}
	runtime, err := NewRuntime(Options{
		AgentDir: agentDir, WorkingDir: t.TempDir(), AuthResolver: auth,
		Adapters: map[string]provider.Streamer{OpenAIResponsesAPI: adapter},
	})
	if err != nil {
		t.Fatal(err)
	}

	registered, ok := runtime.GetProvider(OpenAIProviderID)
	if !ok || registered.ID() != OpenAIProviderID || registered.Name() != "OpenAI" {
		t.Fatalf("registered provider = %#v, %t", registered, ok)
	}
	if _, ok := runtime.GetProvider("OpenAI"); ok {
		t.Fatal("provider lookup unexpectedly ignored identity casing")
	}
	selected, ok := runtime.GetModel(OpenAIProviderID, DefaultOpenAIModel)
	if !ok {
		t.Fatalf("missing builtin %s/%s", OpenAIProviderID, DefaultOpenAIModel)
	}
	available, err := runtime.GetAvailable(context.Background(), OpenAIProviderID)
	if err != nil {
		t.Fatal(err)
	}
	if len(available) == 0 || available[0].Provider != OpenAIProviderID || len(auth.checks) == 0 {
		t.Fatalf("available/checks = %#v / %#v", available, auth.checks)
	}
	ref, err := selected.Ref()
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequestWithOptions(ref, "system", nil, provider.RequestOptions{Stream: provider.StreamOptions{
		Headers: map[string]string{"X-Auth": "request", "X-Request": "yes"},
		Env:     map[string]string{"SHARED": "request", "REQUEST": "yes"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	stream := runtime.Stream(context.Background(), request)
	defer stream.Close()
	if len(auth.resolved) != 0 || len(adapter.Requests()) != 0 {
		t.Fatal("stream resolved auth or dispatched before the first pull")
	}
	if _, nextErr := stream.Next(); nextErr != nil && nextErr != io.EOF {
		t.Fatalf("adapter stream initialization = %v", nextErr)
	}
	requests := adapter.Requests()
	if len(requests) != 1 {
		t.Fatalf("adapter requests = %d", len(requests))
	}
	prepared := requests[0]
	options := prepared.StreamOptions()
	if options.APIKey != "owned-key" || options.Headers["Authorization"] != "Bearer owned" || options.Headers["X-Auth"] != "request" || options.Headers["X-Request"] != "yes" {
		t.Fatalf("prepared auth/headers = %#v", options)
	}
	if options.Env["OWNED"] != "yes" || options.Env["SHARED"] != "request" || options.Env["REQUEST"] != "yes" {
		t.Fatalf("prepared env = %#v", options.Env)
	}
	if prepared.Model().BaseURL() != "https://resolved.example/v1" || prepared.Model().Provider() != ref.Provider() || prepared.Model().ID() != ref.ID() {
		t.Fatalf("prepared model = %s %s/%s", prepared.Model().BaseURL(), prepared.Model().Provider(), prepared.Model().ID())
	}
	if len(auth.resolved) != 1 || auth.resolved[0] != OpenAIProviderID+"/"+DefaultOpenAIModel {
		t.Fatalf("auth resolutions = %#v", auth.resolved)
	}
}

func TestModelsRuntimeMissingAdapterFailsWithoutFallback(t *testing.T) {
	agentDir := t.TempDir()
	models := `{"providers":{"custom":{"baseUrl":"https://example.invalid/v1","apiKey":"key","models":[{"id":"future","api":"future-api"}]}}}`
	writeFile(t, filepath.Join(agentDir, "models.json"), models)
	runtime, err := NewRuntime(Options{AgentDir: agentDir, WorkingDir: t.TempDir(), AuthResolver: &providerRuntimeAuthStub{}, Adapters: map[string]provider.Streamer{
		OpenAIResponsesAPI: mustScriptedProvider(t),
	}})
	if err != nil {
		t.Fatal(err)
	}
	selected, ok := runtime.GetModel("custom", "future")
	if !ok {
		t.Fatal("custom model was not loaded")
	}
	ref, err := selected.Ref()
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequest(ref, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	stream := runtime.Stream(context.Background(), request)
	defer stream.Close()
	if _, err := stream.Next(); err == nil || err == io.EOF {
		t.Fatalf("missing adapter stream error = %v", err)
	}
}

func TestModelsRuntimeRefreshesProviderBeforeItHasModels(t *testing.T) {
	agentDir := t.TempDir()
	writeFile(t, filepath.Join(agentDir, "models.json"), `{"providers":{"dynamic":{"api":"openai-responses","baseUrl":"https://dynamic.example/v1","apiKey":"key"}}}`)
	auth := &providerRuntimeAuthStub{}
	runtime, err := NewRuntime(Options{
		AgentDir: agentDir, WorkingDir: t.TempDir(), AuthResolver: auth,
		Refreshers: map[string]RefreshModelsFunc{
			"dynamic": func(_ context.Context, input RefreshModelsContext) (CachedCatalog, error) {
				if input.Credential == nil || input.Credential.Type != "api_key" || input.Credential.Key != "owned-key" || input.Auth == nil || input.Provider.ID != "dynamic" {
					t.Fatalf("refresh input = %#v", input)
				}
				return cached("dynamic", "discovered"), nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := runtime.GetModel("dynamic", "discovered"); ok {
		t.Fatal("dynamic model existed before refresh")
	}
	result := runtime.Refresh(context.Background(), ModelsRefreshOptions{})
	if result.Aborted || len(result.Errors) != 0 {
		t.Fatalf("refresh result = %#v", result)
	}
	if _, ok := runtime.GetModel("dynamic", "discovered"); !ok {
		t.Fatal("dynamic model was not published")
	}
	if len(auth.resolved) != 1 || auth.resolved[0] != "dynamic/" {
		t.Fatalf("provider-scoped auth resolutions = %#v", auth.resolved)
	}
}

func TestModelsRuntimeFilterReceivesStoredCredentialMetadata(t *testing.T) {
	auth := &providerRuntimeCredentialStub{credential: &ProviderCredential{
		Type: "oauth", Extra: map[string]json.RawMessage{"availableModelIds": json.RawMessage(`["gpt-5.5"]`)},
	}}
	filtered := false
	runtime, err := NewRuntime(Options{
		AgentDir: t.TempDir(), WorkingDir: t.TempDir(), AuthResolver: auth,
		Filters: map[string]ProviderModelFilter{
			OpenAIProviderID: func(models []Model, credential *ProviderCredential) []Model {
				filtered = true
				if credential == nil || credential.Type != "oauth" || string(credential.Extra["availableModelIds"]) != `["gpt-5.5"]` {
					t.Fatalf("filter credential = %#v", credential)
				}
				credential.Extra["availableModelIds"][0] = '{'
				for _, candidate := range models {
					if candidate.ID == DefaultOpenAIModel {
						return []Model{candidate}
					}
				}
				return nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	available, err := runtime.GetAvailable(context.Background(), OpenAIProviderID)
	if err != nil || !filtered || len(available) != 1 || available[0].ID != DefaultOpenAIModel {
		t.Fatalf("filtered availability = %#v, %t, %v", available, filtered, err)
	}
	if string(auth.credential.Extra["availableModelIds"]) != `["gpt-5.5"]` {
		t.Fatal("filter mutated the stored credential snapshot")
	}
	selected, ok := runtime.GetModel(OpenAIProviderID, DefaultOpenAIModel)
	if !ok || !runtime.Availability().Available(selected) {
		t.Fatalf("filtered model was not synchronously available: %#v, %t", selected, ok)
	}
	for _, candidate := range runtime.GetModels(OpenAIProviderID) {
		if candidate.ID != DefaultOpenAIModel && runtime.Availability().Available(candidate) {
			t.Fatalf("credential filter did not constrain synchronous availability: %#v", candidate)
		}
	}
}

func mustScriptedProvider(t *testing.T) *provider.ScriptedProvider {
	t.Helper()
	result, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

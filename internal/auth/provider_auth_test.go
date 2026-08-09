package auth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestStandardOpenAIAndCodexCredentialsAreIndependent(t *testing.T) {
	requirePersistentAuth(t)
	store, err := NewStore(Options{Path: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntime(store)
	if err := store.SetAPIKey(context.Background(), OpenAIProviderID, "openai-key", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKey(context.Background(), OpenAICodexProviderID, "codex-key", nil); err != nil {
		t.Fatal(err)
	}
	standard, err := ResolveOpenAIAuth(context.Background(), runtime, nil, nil, map[string]string{"OPENAI_API_KEY": "ambient"}, OpenAIResolveOptions{})
	if err != nil || standard.APIKey != "openai-key" || standard.Source != "stored credential" || standard.AccountID != "" {
		t.Fatalf("standard auth = %#v, %v", standard, err)
	}
	codex, err := ResolveOpenAICodexAuth(context.Background(), runtime, nil, nil, nil, OpenAIResolveOptions{})
	if err != nil || codex.APIKey != "codex-key" || codex.Source != "stored credential" {
		t.Fatalf("Codex auth = %#v, %v", codex, err)
	}

	if err := runtime.Delete(context.Background(), OpenAIProviderID); err != nil {
		t.Fatal(err)
	}
	standard, err = ResolveOpenAIAuth(context.Background(), runtime, nil, nil, map[string]string{"OPENAI_API_KEY": "ambient"}, OpenAIResolveOptions{})
	if err != nil || standard.APIKey != "ambient" || standard.Source != "OPENAI_API_KEY" {
		t.Fatalf("standard ambient auth = %#v, %v", standard, err)
	}
	codex, err = ResolveOpenAICodexAuth(context.Background(), runtime, nil, nil, nil, OpenAIResolveOptions{})
	if err != nil || codex.APIKey != "codex-key" {
		t.Fatalf("standard deletion affected Codex = %#v, %v", codex, err)
	}
}

func TestAnthropicResolutionPrecedenceAndHeaderOwnedAuth(t *testing.T) {
	requirePersistentAuth(t)
	store, err := NewStore(Options{Path: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntime(store)
	ambient := map[string]string{
		AnthropicAuthTokenEnvironment: "auth-token", AnthropicOAuthTokenEnvironment: "sk-ant-oat-environment",
		AnthropicAPIKeyEnvironment: "api-key",
	}
	resolved, err := ResolveAnthropicAuth(context.Background(), runtime, nil, nil, ambient, AnthropicResolveOptions{})
	if err != nil || resolved.APIKey != "" || resolved.Source != AnthropicAuthTokenEnvironment || resolved.Headers["Authorization"] != "Bearer auth-token" || !resolved.HasAuthorization() {
		t.Fatalf("auth token result = %#v, %v", resolved, err)
	}
	delete(ambient, AnthropicAuthTokenEnvironment)
	resolved, err = ResolveAnthropicAuth(context.Background(), runtime, nil, nil, ambient, AnthropicResolveOptions{})
	if err != nil || resolved.APIKey != "sk-ant-oat-environment" || resolved.Source != AnthropicOAuthTokenEnvironment || !resolved.HasAuthorization() {
		t.Fatalf("OAuth token result = %#v, %v", resolved, err)
	}
	delete(ambient, AnthropicOAuthTokenEnvironment)
	resolved, err = ResolveAnthropicAuth(context.Background(), runtime, nil, nil, ambient, AnthropicResolveOptions{})
	if err != nil || resolved.APIKey != "api-key" || resolved.Source != AnthropicAPIKeyEnvironment {
		t.Fatalf("API key result = %#v, %v", resolved, err)
	}

	if err := store.SetAPIKey(context.Background(), AnthropicProviderID, "stored-${KEY}", map[string]string{"KEY": "anthropic"}); err != nil {
		t.Fatal(err)
	}
	configured := "configured"
	resolved, err = ResolveAnthropicAuth(context.Background(), runtime, nil, &configured, ambient, AnthropicResolveOptions{})
	if err != nil || resolved.APIKey != "stored-anthropic" || resolved.Source != "stored credential" {
		t.Fatalf("stored ownership = %#v, %v", resolved, err)
	}
}

func TestResolvedAuthRecognizesOnlyNonEmptySupportedAuthorizationHeaders(t *testing.T) {
	for _, name := range []string{"Authorization", "x-api-key", "cf-aig-authorization"} {
		result := OpenAIAuthResult{Headers: map[string]string{name: "Bearer gateway-token"}}
		if !result.HasAuthorization() {
			t.Fatalf("%s was not recognized as authorization", name)
		}
		result.Headers[name] = ""
		if result.HasAuthorization() {
			t.Fatalf("empty %s was recognized as authorization", name)
		}
	}
}

func TestOpenAICodexOAuthFailuresUseIndependentProviderIdentity(t *testing.T) {
	_, err := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{AuthBaseURL: "://invalid"})
	var authError *Error
	if !errors.As(err, &authError) || authError.Provider != OpenAICodexProviderID {
		t.Fatalf("OAuth error = %#v", err)
	}
}

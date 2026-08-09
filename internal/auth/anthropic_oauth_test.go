package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAnthropicBrowserLoginUsesUpstreamPKCEAndManualRedirectProtocol(t *testing.T) {
	listener := &ephemeralListener{}
	var requestBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/token" || request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Accept") != "application/json" {
			t.Errorf("token request = %s %s headers=%v", request.Method, request.URL.Path, request.Header)
		}
		if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode token body: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"access_token":"access","refresh_token":"refresh","expires_in":3600}`)
	}))
	defer server.Close()
	now := time.Unix(2_000_000_000, 0)
	flow, err := NewAnthropicOAuth(AnthropicOAuthConfig{
		AuthorizeURL: server.URL + "/authorize", TokenURL: server.URL + "/token",
		CallbackListener: listener, Random: bytes.NewReader(bytes.Repeat([]byte{4}, 32)), Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	authorization, err := flow.BeginBrowserLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authorization.URL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	state := query.Get("state")
	if parsed.Path != "/authorize" || query.Get("client_id") != AnthropicOAuthClientID || query.Get("code") != "true" ||
		query.Get("response_type") != "code" || query.Get("redirect_uri") != "http://localhost:53692/callback" ||
		query.Get("scope") != anthropicOAuthScopes || query.Get("code_challenge_method") != "S256" ||
		state == "" || query.Get("code_challenge") == "" || state != authorization.verifier {
		t.Fatalf("authorization URL = %q", authorization.URL)
	}
	manual := make(chan string, 1)
	manual <- query.Get("redirect_uri") + "?code=manual-code&state=" + url.QueryEscape(state)
	credential, err := authorization.Wait(context.Background(), manual)
	if err != nil {
		t.Fatal(err)
	}
	if credential.Access != "access" || credential.Refresh != "refresh" || credential.Expires != now.Add(55*time.Minute).UnixMilli() {
		t.Fatalf("credential = %#v", credential)
	}
	if requestBody["grant_type"] != "authorization_code" || requestBody["client_id"] != AnthropicOAuthClientID ||
		requestBody["code"] != "manual-code" || requestBody["state"] != state || requestBody["code_verifier"] != state ||
		requestBody["redirect_uri"] != "http://localhost:53692/callback" {
		t.Fatalf("exchange body = %#v", requestBody)
	}
}

func TestAnthropicRefreshOmitsScopeAndStoredResolutionRefreshesOnce(t *testing.T) {
	requirePersistentAuth(t)
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode refresh: %v", err)
		}
		if body["grant_type"] != "refresh_token" || body["client_id"] != AnthropicOAuthClientID || body["refresh_token"] != "old-refresh" {
			t.Errorf("refresh body = %#v", body)
		}
		if _, exists := body["scope"]; exists {
			t.Errorf("refresh unexpectedly sent scope: %#v", body)
		}
		refreshes.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"access_token":"sk-ant-oat-new","refresh_token":"new-refresh","expires_in":3600,"scope":"ignored"}`)
	}))
	defer server.Close()
	now := time.Unix(2_000_000_000, 0)
	flow, err := NewAnthropicOAuth(AnthropicOAuthConfig{TokenURL: server.URL, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(Options{Path: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatal(err)
	}
	old := OAuthCredential{
		Access: "sk-ant-oat-old", Refresh: "old-refresh", Expires: now.Add(-time.Hour).UnixMilli(),
		Extra: map[string]json.RawMessage{"future": json.RawMessage(`{"keep":true}`)},
	}
	if err := store.SetOAuth(context.Background(), AnthropicProviderID, old); err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntime(store)
	var group sync.WaitGroup
	errorsOut := make(chan error, 10)
	for range 10 {
		group.Add(1)
		go func() {
			defer group.Done()
			resolved, err := ResolveAnthropicAuth(context.Background(), runtime, nil, nil, nil, AnthropicResolveOptions{OAuth: flow, Clock: func() time.Time { return now }})
			if err != nil || resolved.APIKey != "sk-ant-oat-new" || resolved.Source != "OAuth" {
				errorsOut <- errors.New("unexpected Anthropic OAuth resolution")
			}
		}()
	}
	group.Wait()
	close(errorsOut)
	for err := range errorsOut {
		t.Error(err)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh count = %d", refreshes.Load())
	}
	stored, exists, err := store.Read(context.Background(), AnthropicProviderID)
	if err != nil || !exists || stored.OAuth.Access != "sk-ant-oat-new" || stored.OAuth.Refresh != "new-refresh" {
		t.Fatalf("stored credential = %#v exists=%t err=%v", stored, exists, err)
	}
	var future map[string]bool
	if err := json.Unmarshal(stored.OAuth.Extra["future"], &future); err != nil || !future["keep"] {
		t.Fatalf("future metadata = %#v, %v", future, err)
	}
}

func TestResolveAnthropicOAuthRejectsRefreshedCredentialBelowExplicitMinimum(t *testing.T) {
	requirePersistentAuth(t)
	now := time.Unix(2_100_000_000, 0)
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		refreshes.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"access_token":"sk-ant-oat-short","refresh_token":"short-refresh","expires_in":600}`)
	}))
	defer server.Close()

	store, err := NewStore(Options{Path: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetOAuth(context.Background(), AnthropicProviderID, OAuthCredential{
		Access: "sk-ant-oat-old", Refresh: "old-refresh", Expires: now.Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	flow, err := NewAnthropicOAuth(AnthropicOAuthConfig{TokenURL: server.URL, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveAnthropicAuth(context.Background(), NewRuntime(store), nil, nil, nil, AnthropicResolveOptions{
		OAuth: flow, Clock: func() time.Time { return now }, MinimumValidity: 2 * time.Hour,
	})
	if !IsKind(err, KindOAuth) || refreshes.Load() != 1 {
		t.Fatalf("short refreshed credential = %v, refreshes=%d", err, refreshes.Load())
	}
	stored, exists, readErr := store.Read(context.Background(), AnthropicProviderID)
	if readErr != nil || !exists || stored.OAuth.Refresh != "short-refresh" {
		t.Fatalf("rotated credential was not persisted: %#v exists=%t err=%v", stored, exists, readErr)
	}
}

func TestResolveAnthropicOAuthDistinguishesOmittedAndExplicitZeroMinimum(t *testing.T) {
	requirePersistentAuth(t)
	now := time.Unix(2_100_000_000, 0)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"access_token":"sk-ant-oat-short","refresh_token":"short-refresh","expires_in":60}`)
	}))
	defer server.Close()
	store, err := NewStore(Options{Path: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatal(err)
	}
	old := OAuthCredential{Access: "sk-ant-oat-old", Refresh: "old-refresh", Expires: now.Add(time.Minute).UnixMilli()}
	flow, err := NewAnthropicOAuth(AnthropicOAuthConfig{TokenURL: server.URL, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetOAuth(context.Background(), AnthropicProviderID, old); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveAnthropicAuth(context.Background(), NewRuntime(store), nil, nil, nil, AnthropicResolveOptions{
		OAuth: flow, Clock: func() time.Time { return now },
	})
	if err != nil || resolved.APIKey != "sk-ant-oat-short" {
		t.Fatalf("omitted minimum = %#v, %v", resolved, err)
	}
	if err := store.SetOAuth(context.Background(), AnthropicProviderID, old); err != nil {
		t.Fatal(err)
	}
	_, err = ResolveAnthropicAuth(context.Background(), NewRuntime(store), nil, nil, nil, AnthropicResolveOptions{
		OAuth: flow, Clock: func() time.Time { return now }, MinimumValiditySet: true,
	})
	if !IsKind(err, KindOAuth) {
		t.Fatalf("explicit zero minimum error = %v", err)
	}
}

func TestAnthropicOAuthFailuresAreTypedBoundedAndSecretSafe(t *testing.T) {
	secret := "refresh-secret-must-not-leak"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(writer, `{"error":`+strconvQuote(secret+strings.Repeat("x", 1024))+`}`)
	}))
	defer server.Close()
	flow, err := NewAnthropicOAuth(AnthropicOAuthConfig{TokenURL: server.URL, MaxResponseBytes: 256})
	if err != nil {
		t.Fatal(err)
	}
	_, err = flow.Refresh(context.Background(), OAuthCredential{Refresh: secret})
	var typed *Error
	if !errors.As(err, &typed) || typed.Provider != AnthropicProviderID || !IsKind(err, KindMalformed) || strings.Contains(err.Error(), secret) {
		t.Fatalf("refresh error = %#v", err)
	}
	_, err = NewAnthropicOAuth(AnthropicOAuthConfig{TokenURL: "://invalid"})
	if !errors.As(err, &typed) || typed.Provider != AnthropicProviderID {
		t.Fatalf("constructor error = %#v", err)
	}
}

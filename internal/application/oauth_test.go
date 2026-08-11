package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/auth"
)

func TestProviderOAuthLoginPersistsCredentialWithoutSurfaceOwnedAuthState(t *testing.T) {
	claim, err := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "account-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	accessToken := "e30." + base64.RawURLEncoding.EncodeToString(claim) + ".sig"
	tokenServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/oauth/token" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"access_token": accessToken, "refresh_token": "refresh-test", "expires_in": 3600,
		})
	}))
	t.Cleanup(tokenServer.Close)

	cwd := t.TempDir()
	agentDir := filepath.Join(t.TempDir(), "agent")
	service, err := NewService(ServiceOptions{
		Production: app.ProductionConfig{
			WorkingDir: cwd, AgentDir: agentDir, Environment: []string{}, OpenAIOAuthBaseURL: tokenServer.URL,
		},
		DisableReaper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	login, err := service.StartProviderOAuth(ctx, auth.OpenAICodexProviderID)
	if err != nil {
		t.Fatal(err)
	}
	defer login.Close()
	if !strings.HasPrefix(login.URL, tokenServer.URL+"/oauth/authorize?") {
		t.Fatalf("authorization URL = %q", login.URL)
	}
	if err := login.Submit("manual-code"); err != nil {
		t.Fatal(err)
	}
	if err := login.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	store, err := auth.NewStore(auth.Options{Path: filepath.Join(agentDir, "auth.json")})
	if err != nil {
		t.Fatal(err)
	}
	credential, exists, err := store.Read(context.Background(), auth.OpenAICodexProviderID)
	if err != nil || !exists || credential.Type != "oauth" || credential.OAuth.AccountID != "account-test" {
		t.Fatalf("stored credential = %#v, %v, %v", credential, exists, err)
	}
}

func TestProviderOAuthRejectsUnsupportedProvider(t *testing.T) {
	cwd := t.TempDir()
	service, err := NewService(ServiceOptions{
		Production:    app.ProductionConfig{WorkingDir: cwd, AgentDir: filepath.Join(t.TempDir(), "agent"), Environment: []string{}},
		DisableReaper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if _, err := service.StartProviderOAuth(context.Background(), "deepseek"); !errors.Is(err, ErrProviderOAuthUnsupported) {
		t.Fatalf("unsupported provider error = %v", err)
	}
}

package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
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

type ephemeralListener struct {
	listener net.Listener
	address  string
}

func (l *ephemeralListener) Listen(_ string, address string) (net.Listener, error) {
	l.address = address
	var err error
	l.listener, err = net.Listen("tcp", "127.0.0.1:0")
	return l.listener, err
}
func oauthJWT(account string) string {
	claim, _ := json.Marshal(map[string]any{openAICodexClaim: map[string]string{"chatgpt_account_id": account}})
	return "e30." + base64.RawURLEncoding.EncodeToString(claim) + ".sig"
}
func oauthJSON(access, refresh string) string {
	return `{"access_token":` + strconvQuote(access) + `,"refresh_token":` + strconvQuote(refresh) + `,"expires_in":3600}`
}
func strconvQuote(value string) string { data, _ := json.Marshal(value); return string(data) }

func TestBrowserAuthorizationUsesPKCEStateCallbackAndExplicitOpener(t *testing.T) {
	listener := &ephemeralListener{}
	var opened string
	flow, err := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{AuthBaseURL: "https://fixture.test", CallbackListener: listener, Random: bytes.NewReader(bytes.Repeat([]byte{7}, 64)), BrowserOpener: func(_ context.Context, url string) error { opened = url; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	authorization, err := flow.BeginBrowserLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer authorization.callback.Close()
	if opened != "" || listener.address != "127.0.0.1:1455" {
		t.Fatalf("unexpected implicit opener/bind: opened %q bind %q", opened, listener.address)
	}
	parsed, err := url.Parse(authorization.URL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if parsed.Path != "/oauth/authorize" || query.Get("client_id") != OpenAICodexClientID || query.Get("redirect_uri") != "http://localhost:1455/auth/callback" || query.Get("code_challenge_method") != "S256" || len(query.Get("state")) != 32 || query.Get("state") != strings.Repeat("07", 16) || query.Get("code_challenge") == "" {
		t.Fatalf("authorization URL = %q", authorization.URL)
	}
	if err := authorization.Open(context.Background()); err != nil || opened != authorization.URL {
		t.Fatalf("Open() = %v, %q", err, opened)
	}
}

func TestBrowserCallbackStateErrorsCancellationAndLateCallback(t *testing.T) {
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		exchanges.Add(1)
		if request.URL.Path != "/oauth/token" {
			t.Errorf("token path = %s", request.URL.Path)
		}
		_, _ = io.WriteString(writer, oauthJSON(oauthJWT("account"), "rotated"))
	}))
	defer server.Close()
	listener := &ephemeralListener{}
	flow, err := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{AuthBaseURL: server.URL, CallbackListener: listener, Random: bytes.NewReader(bytes.Repeat([]byte{3}, 64))})
	if err != nil {
		t.Fatal(err)
	}
	authz, err := flow.BeginBrowserLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + listener.listener.Addr().String() + "/auth/callback"
	bad, err := http.Get(base + "?state=wrong&code=bad-code")
	if err != nil {
		t.Fatal(err)
	}
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad state status = %d", bad.StatusCode)
	}
	bad.Body.Close()
	good, err := http.Get(base + "?state=" + authz.callback.state + "&code=authorization-code")
	if err != nil {
		t.Fatal(err)
	}
	if good.StatusCode != http.StatusOK {
		t.Fatalf("good status = %d", good.StatusCode)
	}
	good.Body.Close()
	credential, err := authz.Wait(context.Background(), nil)
	if err != nil || credential.Access != oauthJWT("account") || credential.Refresh != "rotated" || credential.AccountID != "account" {
		t.Fatalf("Wait() = %#v, %v", credential, err)
	}
	if exchanges.Load() != 1 {
		t.Fatalf("exchanges = %d", exchanges.Load())
	}
	late, err := http.Get(base + "?state=" + authz.callback.state + "&code=late-code")
	if err == nil {
		late.Body.Close()
		t.Fatalf("late callback unexpectedly reached closed listener")
	}

	listener = &ephemeralListener{}
	flow, _ = NewOpenAICodexOAuth(OpenAICodexOAuthConfig{AuthBaseURL: server.URL, CallbackListener: listener, Random: bytes.NewReader(bytes.Repeat([]byte{4}, 64))})
	authz, err = flow.BeginBrowserLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = authz.Wait(cancelled, nil)
	if !IsKind(err, KindCancelled) {
		t.Fatalf("cancel wait = %v", err)
	}
}

func TestDeviceFlowPollsPendingSlowDownAndExchanges(t *testing.T) {
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			if request.Header.Get("Content-Type") != "application/json" {
				t.Errorf("content-type = %q", request.Header.Get("Content-Type"))
			}
			_, _ = io.WriteString(writer, `{"device_auth_id":"device-id","user_code":"ABCD-1234","interval":"1"}`)
		case "/api/accounts/deviceauth/token":
			switch polls.Add(1) {
			case 1:
				writer.WriteHeader(http.StatusForbidden)
			case 2:
				writer.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(writer, `{"error":"slow_down"}`)
			default:
				_, _ = io.WriteString(writer, `{"authorization_code":"device-code","code_verifier":"device-verifier"}`)
			}
		case "/oauth/token":
			_, _ = io.WriteString(writer, oauthJSON(oauthJWT("device-account"), "device-refresh"))
		default:
			t.Errorf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	var sleeps []time.Duration
	flow, err := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{AuthBaseURL: server.URL, Sleep: func(context.Context, time.Duration) error {
		sleeps = append(sleeps, time.Duration(len(sleeps)+1)*time.Second)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	device, err := flow.StartDeviceLogin(context.Background())
	if err != nil || device.VerificationURI != server.URL+"/codex/device" || device.Interval != time.Second {
		t.Fatalf("StartDeviceLogin = %#v, %v", device, err)
	}
	credential, err := flow.CompleteDeviceLogin(context.Background(), device)
	if err != nil || credential.AccountID != "device-account" || polls.Load() != 3 || len(sleeps) != 2 {
		t.Fatalf("CompleteDeviceLogin = %#v, %v polls=%d sleeps=%v", credential, err, polls.Load(), sleeps)
	}
}

func TestResolveStoredOAuthRefreshesOnceDurablyAndDoesNotOverwriteOnFailure(t *testing.T) {
	requirePersistentAuth(t)
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		refreshes.Add(1)
		_, _ = io.WriteString(writer, oauthJSON(oauthJWT("fresh-account"), "fresh-refresh"))
	}))
	defer server.Close()
	store, err := NewStore(Options{Path: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatal(err)
	}
	old := OAuthCredential{Access: oauthJWT("old-account"), Refresh: "old-refresh", Expires: time.Now().Add(-time.Hour).UnixMilli(), AccountID: "old-account", Extra: map[string]json.RawMessage{"future-field": json.RawMessage(`{"keep":true}`)}}
	if err := store.SetOAuth(context.Background(), "openai", old); err != nil {
		t.Fatal(err)
	}
	flow, _ := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{AuthBaseURL: server.URL})
	runtime := NewRuntime(store)
	var group sync.WaitGroup
	errorsOut := make(chan error, 12)
	for range 12 {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := ResolveOpenAIAuth(context.Background(), runtime, nil, nil, map[string]string{"OPENAI_API_KEY": "must-not-fallback"}, OpenAIResolveOptions{OAuth: flow})
			if err != nil || result.APIKey != oauthJWT("fresh-account") {
				errorsOut <- errors.New("unexpected resolved OAuth")
			}
		}()
	}
	group.Wait()
	close(errorsOut)
	for err := range errorsOut {
		t.Error(err)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes = %d", refreshes.Load())
	}
	stored, exists, err := store.Read(context.Background(), "openai")
	var preserved map[string]bool
	_ = json.Unmarshal(stored.OAuth.Extra["future-field"], &preserved)
	if err != nil || !exists || stored.OAuth.Refresh != "fresh-refresh" || !preserved["keep"] {
		t.Fatalf("stored = %#v exists %t err %v", stored, exists, err)
	}

	if err := store.SetOAuth(context.Background(), "openai", old); err != nil {
		t.Fatal(err)
	}
	store.beforeRename = func() error { return errors.New("durability fault") }
	_, err = ResolveOpenAIAuth(context.Background(), runtime, nil, nil, nil, OpenAIResolveOptions{OAuth: flow})
	if !IsKind(err, KindIO) {
		t.Fatalf("refresh write error = %v", err)
	}
	store.beforeRename = nil
	stored, exists, err = store.Read(context.Background(), "openai")
	if err != nil || !exists || stored.OAuth.Refresh != "old-refresh" {
		t.Fatalf("failed refresh overwrote old credential: %#v, %t, %v", stored, exists, err)
	}
}

func TestTokenFailuresAreBoundedUTF8AndSecretSafe(t *testing.T) {
	secret := "refresh-never-leak"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(writer, secret+strings.Repeat("x", 1024))
	}))
	defer server.Close()
	flow, _ := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{AuthBaseURL: server.URL, MaxResponseBytes: 256})
	_, err := flow.Refresh(context.Background(), OAuthCredential{Refresh: secret})
	if !IsKind(err, KindOAuth) || strings.Contains(err.Error(), secret) {
		t.Fatalf("status failure = %v", err)
	}
}

func TestDeviceFlowCancellationAndTimeoutAreTyped(t *testing.T) {
	flow, err := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{HTTPClient: http.DefaultClient})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = flow.CompleteDeviceLogin(cancelled, DeviceCode{DeviceAuthID: "device", UserCode: "code", ExpiresIn: time.Second})
	if !IsKind(err, KindCancelled) {
		t.Fatalf("cancel device login = %v", err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	timeoutFlow, err := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{Clock: func() time.Time {
		if calls.Add(1) == 1 {
			return start
		}
		return start.Add(2 * time.Minute)
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = timeoutFlow.CompleteDeviceLogin(context.Background(), DeviceCode{DeviceAuthID: "device", UserCode: "code", ExpiresIn: time.Minute})
	if !IsKind(err, KindTimeout) {
		t.Fatalf("timeout device login = %v", err)
	}
}

func FuzzOAuthInputFailuresNeverLeakSecret(f *testing.F) {
	f.Add("not-a-token")
	f.Add("e30.eyJ4IjoieCJ9.sig")
	f.Fuzz(func(t *testing.T, token string) {
		_, err := accountIDFromJWT(token + "-never-leak-oauth")
		if err != nil && strings.Contains(err.Error(), "never-leak-oauth") {
			t.Fatalf("secret leaked: %v", err)
		}
	})
}

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
	defer authorization.Close()
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
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
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
	callbackRequest := func(rawURL string) (*http.Response, error) {
		request, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		request.Host = "localhost:1455"
		return http.DefaultClient.Do(request)
	}
	bad, err := callbackRequest(base + "?state=wrong&code=bad-code")
	if err != nil {
		t.Fatal(err)
	}
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad state status = %d", bad.StatusCode)
	}
	bad.Body.Close()
	good, err := callbackRequest(base + "?state=" + authz.callback.state + "&code=authorization-code")
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
	late, err := callbackRequest(base + "?state=" + authz.callback.state + "&code=late-code")
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

func TestBrowserWaitStartsBeforeCallbackClientReadsCompletePage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, oauthJSON(oauthJWT("concurrent-account"), "refresh"))
	}))
	defer server.Close()
	listener := &ephemeralListener{}
	flow, err := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{AuthBaseURL: server.URL, CallbackListener: listener})
	if err != nil {
		t.Fatal(err)
	}
	authz, err := flow.BeginBrowserLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() {
		credential, waitErr := authz.Wait(context.Background(), nil)
		if waitErr == nil && credential.AccountID != "concurrent-account" {
			waitErr = errors.New("wrong account")
		}
		waitDone <- waitErr
	}()
	request, _ := http.NewRequest(http.MethodGet, "http://"+listener.listener.Addr().String()+"/auth/callback?state="+authz.callback.state+"&code=code", nil)
	request.Host = "localhost:1455"
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("callback request returned EOF/error: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "OpenAI authentication completed") {
		t.Fatalf("callback page = status %d body %q error %v", response.StatusCode, body, readErr)
	}
	select {
	case waitErr := <-waitDone:
		if waitErr != nil {
			t.Fatal(waitErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not complete")
	}
}

func TestDeviceFlowPollsPendingSlowDownAndExchanges(t *testing.T) {
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
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
	flow, err := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{AuthBaseURL: server.URL, Sleep: func(_ context.Context, delay time.Duration) error {
		sleeps = append(sleeps, delay)
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
	if err != nil || credential.AccountID != "device-account" || polls.Load() != 3 || len(sleeps) != 2 || sleeps[0] != time.Second || sleeps[1] != 6*time.Second {
		t.Fatalf("CompleteDeviceLogin = %#v, %v polls=%d sleeps=%v", credential, err, polls.Load(), sleeps)
	}
}

func TestResolveStoredOAuthRefreshesOnceDurablyAndDoesNotOverwriteOnFailure(t *testing.T) {
	requirePersistentAuth(t)
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
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
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(writer, `{"error":`+strconvQuote(secret+strings.Repeat("x", 1024))+`}`)
	}))
	defer server.Close()
	flow, _ := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{AuthBaseURL: server.URL, MaxResponseBytes: 256})
	_, err := flow.Refresh(context.Background(), OAuthCredential{Refresh: secret})
	if !IsKind(err, KindMalformed) || strings.Contains(err.Error(), secret) {
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
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { writer.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	timeoutFlow, err := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{AuthBaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = timeoutFlow.CompleteDeviceLogin(context.Background(), DeviceCode{DeviceAuthID: "device", UserCode: "code", Interval: time.Hour, ExpiresIn: 20 * time.Millisecond})
	if !IsKind(err, KindTimeout) {
		t.Fatalf("timeout device login = %v", err)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("timeout exceeded deadline: %s", time.Since(started))
	}
}

func TestOAuthHTTPRejects307BeforeAnySecretReachesCollector(t *testing.T) {
	var collectorCalls atomic.Int32
	var collectorBody atomic.Value
	collector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		collectorCalls.Add(1)
		body, _ := io.ReadAll(request.Body)
		collectorBody.Store(string(body))
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{}`)
	}))
	defer collector.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Location", collector.URL+request.URL.Path)
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	var callerPolicyCalls atomic.Int32
	callerClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { callerPolicyCalls.Add(1); return nil }}
	flow, err := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{AuthBaseURL: redirector.URL, HTTPClient: callerClient})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Refresh(context.Background(), OAuthCredential{Refresh: "refresh-redirect-secret"}); !IsKind(err, KindOAuth) {
		t.Fatalf("refresh redirect = %v", err)
	}
	if _, err := flow.StartDeviceLogin(context.Background()); !IsKind(err, KindOAuth) {
		t.Fatalf("device start redirect = %v", err)
	}
	if _, err := flow.CompleteDeviceLogin(context.Background(), DeviceCode{DeviceAuthID: "device-redirect-secret", UserCode: "user-redirect-secret", ExpiresIn: time.Second}); !IsKind(err, KindOAuth) {
		t.Fatalf("device poll redirect = %v", err)
	}
	if collectorCalls.Load() != 0 {
		t.Fatalf("collector received %d requests, last body %q", collectorCalls.Load(), collectorBody.Load())
	}
	if callerPolicyCalls.Load() != 0 {
		t.Fatalf("caller redirect policy was used/mutated: %d", callerPolicyCalls.Load())
	}
	request, _ := http.NewRequest(http.MethodGet, redirector.URL, nil)
	_ = callerClient.CheckRedirect(request, nil)
	if callerPolicyCalls.Load() != 1 {
		t.Fatal("caller client policy was mutated")
	}
}

func TestDeviceHungPollIsBoundedByExpiryContext(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(entered)
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	defer func() { close(release); server.Close() }()
	flow, _ := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{AuthBaseURL: server.URL})
	started := time.Now()
	_, err := flow.CompleteDeviceLogin(context.Background(), DeviceCode{DeviceAuthID: "device", UserCode: "code", ExpiresIn: 30 * time.Millisecond})
	if !IsKind(err, KindTimeout) {
		t.Fatalf("hung poll = %v", err)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("hung poll exceeded expiry: %s", time.Since(started))
	}
	select {
	case <-entered:
	default:
		t.Fatal("poll was not issued")
	}
}

func TestDeviceFinalExchangeDeadlineAndCallerCancelClassification(t *testing.T) {
	for _, test := range []struct {
		name         string
		expires      time.Duration
		cancelCaller bool
		want         Kind
	}{
		{name: "deadline", expires: 30 * time.Millisecond, want: KindTimeout},
		{name: "caller cancel", expires: time.Second, cancelCaller: true, want: KindCancelled},
	} {
		t.Run(test.name, func(t *testing.T) {
			exchangeEntered := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api/accounts/deviceauth/token":
					writer.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(writer, `{"authorization_code":"code","code_verifier":"verifier"}`)
				case "/oauth/token":
					close(exchangeEntered)
					<-release
				default:
					t.Errorf("path = %s", request.URL.Path)
				}
			}))
			defer func() { close(release); server.Close() }()
			flow, _ := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{AuthBaseURL: server.URL})
			ctx := context.Background()
			var cancel context.CancelFunc
			if test.cancelCaller {
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
			}
			done := make(chan error, 1)
			go func() {
				_, err := flow.CompleteDeviceLogin(ctx, DeviceCode{DeviceAuthID: "device", UserCode: "user", ExpiresIn: test.expires})
				done <- err
			}()
			select {
			case <-exchangeEntered:
			case <-time.After(time.Second):
				t.Fatal("final exchange was not issued")
			}
			if test.cancelCaller {
				cancel()
			}
			select {
			case err := <-done:
				if !IsKind(err, test.want) {
					t.Fatalf("exchange error = %v, want %s", err, test.want)
				}
			case <-time.After(time.Second):
				t.Fatal("hung final exchange exceeded deadline/cancel")
			}
		})
	}
}

func TestOAuthJSONAndMediaAdmission(t *testing.T) {
	tests := []struct {
		name, path, contentType, body string
		status                        int
		invoke                        func(*OpenAICodexOAuth) error
		want                          Kind
	}{
		{name: "token duplicate", path: "/oauth/token", contentType: "application/json", body: `{"access_token":"a","access_token":"b","refresh_token":"r","expires_in":1}`, invoke: func(flow *OpenAICodexOAuth) error {
			_, err := flow.Refresh(context.Background(), OAuthCredential{Refresh: "secret"})
			return err
		}, want: KindMalformed},
		{name: "token media", path: "/oauth/token", contentType: "text/plain", body: oauthJSON(oauthJWT("account"), "refresh"), invoke: func(flow *OpenAICodexOAuth) error {
			_, err := flow.Refresh(context.Background(), OAuthCredential{Refresh: "secret"})
			return err
		}, want: KindMalformed},
		{name: "token UTF-8", path: "/oauth/token", contentType: "application/json", body: string([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}), invoke: func(flow *OpenAICodexOAuth) error {
			_, err := flow.Refresh(context.Background(), OAuthCredential{Refresh: "secret"})
			return err
		}, want: KindMalformed},
		{name: "token status", path: "/oauth/token", contentType: "application/json; charset=utf-8", body: `{"error":{"code":"denied"}}`, status: http.StatusTeapot, invoke: func(flow *OpenAICodexOAuth) error {
			_, err := flow.Refresh(context.Background(), OAuthCredential{Refresh: "secret"})
			return err
		}, want: KindOAuth},
		{name: "start nested duplicate", path: "/api/accounts/deviceauth/usercode", contentType: "application/json", body: `{"device_auth_id":"d","user_code":"u","interval":1,"future":{"x":1,"x":2}}`, invoke: func(flow *OpenAICodexOAuth) error { _, err := flow.StartDeviceLogin(context.Background()); return err }, want: KindMalformed},
		{name: "poll media", path: "/api/accounts/deviceauth/token", contentType: "application/problem+json", body: `{"authorization_code":"c","code_verifier":"v"}`, invoke: func(flow *OpenAICodexOAuth) error {
			_, err := flow.CompleteDeviceLogin(context.Background(), DeviceCode{DeviceAuthID: "d", UserCode: "u", ExpiresIn: time.Second})
			return err
		}, want: KindMalformed},
		{name: "pending duplicate error", path: "/api/accounts/deviceauth/token", contentType: "application/json", body: `{"error":"deviceauth_authorization_pending","error":"slow_down"}`, status: http.StatusBadRequest, invoke: func(flow *OpenAICodexOAuth) error {
			_, err := flow.CompleteDeviceLogin(context.Background(), DeviceCode{DeviceAuthID: "d", UserCode: "u", ExpiresIn: time.Second})
			return err
		}, want: KindMalformed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != test.path {
					t.Errorf("path = %s", request.URL.Path)
				}
				writer.Header().Set("Content-Type", test.contentType)
				status := test.status
				if status == 0 {
					status = http.StatusOK
				}
				writer.WriteHeader(status)
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()
			flow, _ := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{AuthBaseURL: server.URL})
			if err := test.invoke(flow); !IsKind(err, test.want) {
				t.Fatalf("error = %v, want %s", err, test.want)
			}
		})
	}
}

func TestBrowserTransactionCloseContextOpenerAndRequestLimits(t *testing.T) {
	newAuthorization := func(t *testing.T, begin context.Context, opener BrowserOpener) (*Authorization, string) {
		t.Helper()
		listener := &ephemeralListener{}
		flow, err := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{AuthBaseURL: "https://fixture.test", CallbackListener: listener, BrowserOpener: opener})
		if err != nil {
			t.Fatal(err)
		}
		authz, err := flow.BeginBrowserLogin(begin)
		if err != nil {
			t.Fatal(err)
		}
		return authz, "http://" + listener.listener.Addr().String() + "/auth/callback"
	}
	request := func(method, rawURL, host string) (*http.Response, error) {
		req, err := http.NewRequest(method, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Host = host
		return http.DefaultClient.Do(req)
	}
	authz, base := newAuthorization(t, context.Background(), nil)
	for _, input := range []struct {
		method, target, host string
		status               int
	}{
		{http.MethodPost, base, "localhost:1455", http.StatusNotFound},
		{http.MethodGet, base + "?state=" + authz.callback.state + "&code=x", "evil.example", http.StatusBadRequest},
		{http.MethodGet, base + "?state=" + authz.callback.state + "&code=" + strings.Repeat("x", callbackMaxCodeBytes+1), "localhost:1455", http.StatusBadRequest},
		{http.MethodGet, base + "?state=" + authz.callback.state + "&code=x&padding=" + strings.Repeat("y", callbackMaxQueryBytes), "localhost:1455", http.StatusRequestURITooLong},
		{http.MethodGet, base + "?state=" + authz.callback.state + "&error=" + strings.Repeat("z", callbackMaxErrorBytes+1), "localhost:1455", http.StatusBadRequest},
		{http.MethodGet, base + "?state=" + authz.callback.state + "&state=" + authz.callback.state + "&code=x", "localhost:1455", http.StatusBadRequest},
	} {
		response, err := request(input.method, input.target, input.host)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != input.status {
			t.Fatalf("%s status = %d", input.target, response.StatusCode)
		}
	}
	authz.Close()
	authz.Close()
	authz.Cancel()
	if _, err := request(http.MethodGet, base, "localhost:1455"); err == nil {
		t.Fatal("closed transaction still accepted a request")
	}

	begin, cancel := context.WithCancel(context.Background())
	authz, base = newAuthorization(t, begin, nil)
	cancel()
	select {
	case <-authz.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("begin cancellation did not close transaction")
	}
	if _, err := request(http.MethodGet, base, "localhost:1455"); err == nil {
		t.Fatal("context-cancelled transaction still listening")
	}

	authz, base = newAuthorization(t, context.Background(), func(context.Context, string) error { return errors.New("opener failed") })
	if err := authz.Open(context.Background()); !IsKind(err, KindOAuth) {
		t.Fatalf("opener failure = %v", err)
	}
	if _, err := request(http.MethodGet, base, "localhost:1455"); err == nil {
		t.Fatal("opener failure leaked listener")
	}

	authz, _ = newAuthorization(t, context.Background(), nil)
	done := make(chan error, 1)
	go func() { _, err := authz.Wait(context.Background(), nil); done <- err }()
	authz.Close()
	select {
	case err := <-done:
		if !IsKind(err, KindCancelled) {
			t.Fatalf("Wait/Close race = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait/Close race hung")
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

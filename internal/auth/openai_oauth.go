package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	OpenAICodexClientID        = "app_EMoamEEZ73f0CkXaXp7hrann"
	defaultOpenAICodexAuthBase = "https://auth.openai.com"
	defaultCallbackHost        = "127.0.0.1"
	defaultCallbackPort        = 1455
	defaultOAuthResponseBytes  = 64 << 10
	defaultDeviceTimeout       = 15 * time.Minute
	defaultDeviceInterval      = 5 * time.Second
	minimumDeviceInterval      = time.Second
	openAICodexScope           = "openid profile email offline_access"
	openAICodexClaim           = "https://api.openai.com/auth"
)

type Sleep func(context.Context, time.Duration) error
type BrowserOpener func(context.Context, string) error

// OpenAICodexOAuthConfig controls only the OAuth boundary. The zero value uses
// production endpoints, a real HTTP client, cryptographic randomness and the
// upstream localhost callback endpoint. Browser opening is deliberately left
// to a caller; this service only emits a URL.
type OpenAICodexOAuthConfig struct {
	HTTPClient       *http.Client
	Clock            func() time.Time
	AuthBaseURL      string
	CallbackHost     string
	CallbackPort     int
	CallbackListener ListenerFactory
	BrowserOpener    BrowserOpener
	Sleep            Sleep
	Random           io.Reader
	MaxResponseBytes int64
}

// OpenAICodexOAuth implements browser/device login and token refresh. It owns
// no auth.json state; callers persist a successful login through Store.
type OpenAICodexOAuth struct {
	client   *http.Client
	now      func() time.Time
	base     string
	host     string
	port     int
	listener ListenerFactory
	opener   BrowserOpener
	sleep    Sleep
	random   io.Reader
	maxBody  int64
}

func NewOpenAICodexOAuth(config OpenAICodexOAuthConfig) (*OpenAICodexOAuth, error) {
	base := config.AuthBaseURL
	if base == "" {
		base = defaultOpenAICodexAuthBase
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, failure(KindInvalid, "initialize OpenAI Codex OAuth", "openai", nil)
	}
	client := cloneOAuthHTTPClient(config.HTTPClient)
	now := config.Clock
	if now == nil {
		now = time.Now
	}
	host := config.CallbackHost
	if host == "" {
		host = defaultCallbackHost
	}
	port := config.CallbackPort
	if port == 0 {
		port = defaultCallbackPort
	}
	if port < 1 || port > 65535 || strings.TrimSpace(host) == "" {
		return nil, failure(KindInvalid, "initialize OpenAI Codex OAuth", "openai", nil)
	}
	sleep := config.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	random := config.Random
	if random == nil {
		random = rand.Reader
	}
	max := config.MaxResponseBytes
	if max == 0 {
		max = defaultOAuthResponseBytes
	}
	if max < 256 {
		return nil, failure(KindInvalid, "initialize OpenAI Codex OAuth", "openai", nil)
	}
	return &OpenAICodexOAuth{client: client, now: now, base: strings.TrimRight(base, "/"), host: host, port: port, listener: config.CallbackListener, opener: config.BrowserOpener, sleep: sleep, random: random, maxBody: max}, nil
}

var errOAuthRedirect = errors.New("OAuth redirects are disabled")

func cloneOAuthHTTPClient(input *http.Client) *http.Client {
	if input == nil {
		input = http.DefaultClient
	}
	clone := *input
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return errOAuthRedirect }
	return &clone
}

func (o *OpenAICodexOAuth) endpoint(path string) string { return o.base + path }
func (o *OpenAICodexOAuth) callbackURL() string {
	return "http://localhost:" + strconv.Itoa(o.port) + "/auth/callback"
}

// Authorization is a bound browser-login transaction. URL can be handed to
// any injected browser opener by the CLI/TUI, while Wait owns callback cleanup.
type Authorization struct {
	URL         string
	callback    *callbackWaiter
	flow        *OpenAICodexOAuth
	verifier    string
	redirectURI string
	ctx         context.Context
	cancel      context.CancelCauseFunc
	stopContext func() bool
	closeOnce   sync.Once
	stateMu     sync.Mutex
	closed      bool
	waitMu      sync.Mutex
	waitStarted bool
}

// Close and Cancel are idempotent. Either one unblocks Wait and shuts down the
// callback listener even when the caller never starts Wait.
func (a *Authorization) Close() {
	if a == nil {
		return
	}
	a.closeOnce.Do(func() {
		a.stateMu.Lock()
		a.closed = true
		a.stateMu.Unlock()
		if a.stopContext != nil {
			a.stopContext()
		}
		if a.callback != nil {
			a.callback.Close()
		}
		if a.cancel != nil {
			a.cancel(context.Canceled)
		}
	})
}
func (a *Authorization) Cancel() { a.Close() }

// Open invokes the caller-provided browser adapter explicitly. BeginBrowserLogin
// never calls it on its own, keeping this package suitable for headless CLI
// and future TUI callers.
func (a *Authorization) Open(ctx context.Context) error {
	if err := contextFailure(ctx, "open OAuth browser"); err != nil {
		if a != nil {
			a.Close()
		}
		return err
	}
	if a != nil {
		a.stateMu.Lock()
		closed := a.closed
		a.stateMu.Unlock()
		if closed {
			return failure(KindCancelled, "open OAuth browser", "openai", context.Canceled)
		}
	}
	if a == nil || a.flow == nil || a.flow.opener == nil {
		if a != nil {
			a.Close()
		}
		return failure(KindUnsupported, "open OAuth browser", "openai", nil)
	}
	if err := a.flow.opener(ctx, a.URL); err != nil {
		a.Close()
		return requestFailure(ctx, "open OAuth browser", err)
	}
	return nil
}

func (o *OpenAICodexOAuth) BeginBrowserLogin(ctx context.Context) (*Authorization, error) {
	if err := contextFailure(ctx, "start OpenAI Codex browser login"); err != nil {
		return nil, err
	}
	verifier, challenge, err := o.pkce()
	if err != nil {
		return nil, err
	}
	state, err := o.state()
	if err != nil {
		return nil, err
	}
	waiter, err := startCallbackWaiter(o.host, o.port, state, o.listener)
	if err != nil {
		return nil, err
	}
	redirect := o.callbackURL()
	query := url.Values{
		"response_type": {"code"}, "client_id": {OpenAICodexClientID}, "redirect_uri": {redirect},
		"scope": {openAICodexScope}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"state": {state}, "id_token_add_organizations": {"true"}, "codex_cli_simplified_flow": {"true"}, "originator": {"pi"},
	}
	txContext, cancel := context.WithCancelCause(context.Background())
	authorization := &Authorization{URL: o.endpoint("/oauth/authorize") + "?" + query.Encode(), callback: waiter, flow: o, verifier: verifier, redirectURI: redirect, ctx: txContext, cancel: cancel}
	authorization.stopContext = context.AfterFunc(ctx, authorization.Close)
	if cause := context.Cause(ctx); cause != nil {
		authorization.Close()
		return nil, failure(KindCancelled, "start OpenAI Codex browser login", "openai", cause)
	}
	return authorization, nil
}

// Wait accepts either a validated local callback or a manually supplied code.
// The caller owns browser opening; cancellation and all terminal paths close
// the listener, so a late browser redirect cannot mutate a later login.
func (a *Authorization) Wait(ctx context.Context, manual <-chan string) (OAuthCredential, error) {
	if a == nil || a.callback == nil {
		return OAuthCredential{}, failure(KindInvalid, "wait for OAuth callback", "openai", nil)
	}
	a.waitMu.Lock()
	if a.waitStarted {
		a.waitMu.Unlock()
		return OAuthCredential{}, failure(KindInvalid, "wait for OAuth callback", "openai", nil)
	}
	a.waitStarted = true
	a.waitMu.Unlock()
	if ctx == nil {
		a.Close()
		return OAuthCredential{}, failure(KindInvalid, "wait for OAuth callback", "openai", nil)
	}
	waitCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(context.Canceled)
	defer a.Close()
	go func() {
		select {
		case <-a.ctx.Done():
			cancel(context.Cause(a.ctx))
		case <-waitCtx.Done():
		}
	}()
	for {
		select {
		case result := <-a.callback.result:
			if result.err != nil {
				return OAuthCredential{}, result.err
			}
			return a.flow.ExchangeCode(waitCtx, result.code, a.verifier, a.redirectURI)
		case input, ok := <-manual:
			if !ok {
				manual = nil
				continue
			}
			code, state := parseAuthorizationInput(input)
			if len(input) > callbackMaxQueryBytes || (state != "" && state != a.callback.state) || code == "" || len(code) > callbackMaxCodeBytes {
				return OAuthCredential{}, failure(KindOAuth, "validate manual OAuth callback", "openai", nil)
			}
			return a.flow.ExchangeCode(waitCtx, code, a.verifier, a.redirectURI)
		case <-waitCtx.Done():
			return OAuthCredential{}, failure(KindCancelled, "wait for OAuth callback", "openai", context.Cause(waitCtx))
		}
	}
}

func (o *OpenAICodexOAuth) pkce() (string, string, error) {
	bytes := make([]byte, 32)
	if _, err := io.ReadFull(o.random, bytes); err != nil {
		return "", "", failure(KindIO, "generate OAuth PKCE", "openai", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(bytes)
	digest := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func (o *OpenAICodexOAuth) state() (string, error) {
	bytes := make([]byte, 16)
	if _, err := io.ReadFull(o.random, bytes); err != nil {
		return "", failure(KindIO, "generate OAuth state", "openai", err)
	}
	return hex.EncodeToString(bytes), nil
}

func (o *OpenAICodexOAuth) ExchangeCode(ctx context.Context, code, verifier, redirectURI string) (OAuthCredential, error) {
	if !validOAuthText(code) || !validOAuthText(verifier) || !validOAuthText(redirectURI) {
		return OAuthCredential{}, failure(KindInvalid, "exchange OpenAI Codex authorization code", "openai", nil)
	}
	return o.token(ctx, "exchange", url.Values{"grant_type": {"authorization_code"}, "client_id": {OpenAICodexClientID}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirectURI}})
}

func (o *OpenAICodexOAuth) Refresh(ctx context.Context, credential OAuthCredential) (OAuthCredential, error) {
	if !validOAuthText(credential.Refresh) {
		return OAuthCredential{}, failure(KindInvalid, "refresh OpenAI Codex token", "openai", nil)
	}
	return o.token(ctx, "refresh", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {credential.Refresh}, "client_id": {OpenAICodexClientID}})
}

func (o *OpenAICodexOAuth) token(ctx context.Context, operation string, form url.Values) (OAuthCredential, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint("/oauth/token"), strings.NewReader(form.Encode()))
	if err != nil {
		return OAuthCredential{}, failure(KindInvalid, operation+" OpenAI Codex token", "openai", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := o.client.Do(request)
	if err != nil {
		if context.Cause(ctx) != nil {
			return OAuthCredential{}, failure(KindCancelled, operation+" OpenAI Codex token", "openai", context.Cause(ctx))
		}
		return OAuthCredential{}, failure(KindOAuth, operation+" OpenAI Codex token", "openai", err)
	}
	defer response.Body.Close()
	body, root, admissionErr := readJSONDocument(response, o.maxBody, response.StatusCode < 200 || response.StatusCode > 299)
	if response.StatusCode < 200 || response.StatusCode > 299 {
		if admissionErr != nil {
			return OAuthCredential{}, failure(KindMalformed, operation+" OpenAI Codex token", "openai", admissionErr)
		}
		return OAuthCredential{}, failure(KindOAuth, operation+" OpenAI Codex token", "openai", nil)
	}
	if admissionErr != nil {
		return OAuthCredential{}, failure(KindMalformed, operation+" OpenAI Codex token", "openai", admissionErr)
	}
	var raw struct {
		Access  string          `json:"access_token"`
		Refresh string          `json:"refresh_token"`
		Expires json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || !validOAuthText(raw.Access) || !validOAuthText(raw.Refresh) {
		return OAuthCredential{}, failure(KindMalformed, operation+" OpenAI Codex token", "openai", err)
	}
	var seconds float64
	if err := json.Unmarshal(raw.Expires, &seconds); err != nil || !isPositiveFinite(seconds) {
		return OAuthCredential{}, failure(KindMalformed, operation+" OpenAI Codex token", "openai", err)
	}
	if seconds > float64(math.MaxInt64/int64(time.Second)) {
		return OAuthCredential{}, failure(KindMalformed, operation+" OpenAI Codex token", "openai", nil)
	}
	accountID, err := accountIDFromJWT(raw.Access)
	if err != nil {
		return OAuthCredential{}, err
	}
	for _, key := range []string{"access_token", "refresh_token", "expires_in"} {
		delete(root, key)
	}
	return OAuthCredential{Access: raw.Access, Refresh: raw.Refresh, Expires: o.now().Add(time.Duration(seconds * float64(time.Second))).UnixMilli(), AccountID: accountID, Extra: root}, nil
}

func (o *OpenAICodexOAuth) StartDeviceLogin(ctx context.Context) (DeviceCode, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint("/api/accounts/deviceauth/usercode"), strings.NewReader(`{"client_id":"`+OpenAICodexClientID+`"}`))
	if err != nil {
		return DeviceCode{}, failure(KindInvalid, "start OpenAI Codex device login", "openai", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := o.client.Do(request)
	if err != nil {
		return DeviceCode{}, requestFailure(ctx, "start OpenAI Codex device login", err)
	}
	defer response.Body.Close()
	body, _, admissionErr := readJSONDocument(response, o.maxBody, response.StatusCode < 200 || response.StatusCode > 299)
	if response.StatusCode < 200 || response.StatusCode > 299 {
		if admissionErr != nil {
			return DeviceCode{}, failure(KindMalformed, "start OpenAI Codex device login", "openai", admissionErr)
		}
		return DeviceCode{}, failure(KindOAuth, "start OpenAI Codex device login", "openai", nil)
	}
	if admissionErr != nil {
		return DeviceCode{}, failure(KindMalformed, "start OpenAI Codex device login", "openai", admissionErr)
	}
	var value struct {
		ID       string          `json:"device_auth_id"`
		UserCode string          `json:"user_code"`
		Interval json.RawMessage `json:"interval"`
	}
	if err := json.Unmarshal(body, &value); err != nil || !validOAuthText(value.ID) || !validOAuthText(value.UserCode) {
		return DeviceCode{}, failure(KindMalformed, "start OpenAI Codex device login", "openai", err)
	}
	interval, err := durationSeconds(value.Interval)
	if err != nil {
		return DeviceCode{}, failure(KindMalformed, "start OpenAI Codex device login", "openai", err)
	}
	return DeviceCode{DeviceAuthID: value.ID, UserCode: value.UserCode, VerificationURI: o.endpoint("/codex/device"), Interval: interval, ExpiresIn: defaultDeviceTimeout}, nil
}

type DeviceCode struct {
	DeviceAuthID, UserCode, VerificationURI string
	Interval, ExpiresIn                     time.Duration
}

func (o *OpenAICodexOAuth) CompleteDeviceLogin(ctx context.Context, device DeviceCode) (OAuthCredential, error) {
	if err := contextFailure(ctx, "complete OpenAI Codex device login"); err != nil {
		return OAuthCredential{}, err
	}
	if !validOAuthText(device.DeviceAuthID) || !validOAuthText(device.UserCode) {
		return OAuthCredential{}, failure(KindInvalid, "complete OpenAI Codex device login", "openai", nil)
	}
	expiresIn := device.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = defaultDeviceTimeout
	}
	pollContext, cancel := context.WithTimeout(ctx, expiresIn)
	defer cancel()
	interval := device.Interval
	if interval <= 0 {
		interval = defaultDeviceInterval
	}
	if interval < minimumDeviceInterval {
		interval = minimumDeviceInterval
	}
	for first := true; ; first = false {
		if cause := context.Cause(pollContext); cause != nil {
			return OAuthCredential{}, deviceContextFailure(cause)
		}
		if !first {
			delay := interval
			if deadline, ok := pollContext.Deadline(); ok && time.Until(deadline) < delay {
				delay = time.Until(deadline)
			}
			if delay <= 0 {
				return OAuthCredential{}, failure(KindTimeout, "complete OpenAI Codex device login", "openai", context.DeadlineExceeded)
			}
			if err := o.sleep(pollContext, delay); err != nil {
				if cause := context.Cause(pollContext); cause != nil {
					return OAuthCredential{}, deviceContextFailure(cause)
				}
				return OAuthCredential{}, failure(KindOAuth, "wait to poll OpenAI Codex device login", "openai", err)
			}
		}
		result, err := o.pollDevice(pollContext, device)
		if err != nil {
			return OAuthCredential{}, err
		}
		if result.status == devicePollComplete {
			return o.ExchangeCode(pollContext, result.code, result.verifier, o.endpoint("/deviceauth/callback"))
		}
		if result.status == devicePollSlowDown {
			interval += 5 * time.Second
		}
	}
}

// pollDevice returns an explicit pending/slow_down/complete state. 403/404 and
// deviceauth_authorization_pending are pending as in the fixed upstream flow.
type devicePollStatus uint8

const (
	devicePollPending devicePollStatus = iota
	devicePollSlowDown
	devicePollComplete
)

type devicePollResult struct {
	status         devicePollStatus
	code, verifier string
}

func (o *OpenAICodexOAuth) pollDevice(ctx context.Context, device DeviceCode) (devicePollResult, error) {
	body := `{"device_auth_id":` + strconv.Quote(device.DeviceAuthID) + `,"user_code":` + strconv.Quote(device.UserCode) + `}`
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint("/api/accounts/deviceauth/token"), strings.NewReader(body))
	if err != nil {
		return devicePollResult{}, failure(KindInvalid, "poll OpenAI Codex device login", "openai", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := o.client.Do(request)
	if err != nil {
		return devicePollResult{}, requestFailure(ctx, "poll OpenAI Codex device login", err)
	}
	defer response.Body.Close()
	payload, root, admissionErr := readJSONDocument(response, o.maxBody, response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusNotFound)
	if response.StatusCode >= 200 && response.StatusCode <= 299 {
		if admissionErr != nil {
			return devicePollResult{}, failure(KindMalformed, "poll OpenAI Codex device login", "openai", admissionErr)
		}
		var value struct {
			Code     string `json:"authorization_code"`
			Verifier string `json:"code_verifier"`
		}
		if err := json.Unmarshal(payload, &value); err != nil || !validOAuthText(value.Code) || !validOAuthText(value.Verifier) {
			return devicePollResult{}, failure(KindMalformed, "poll OpenAI Codex device login", "openai", err)
		}
		return devicePollResult{status: devicePollComplete, code: value.Code, verifier: value.Verifier}, nil
	}
	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusNotFound {
		if admissionErr != nil {
			return devicePollResult{}, failure(KindMalformed, "poll OpenAI Codex device login", "openai", admissionErr)
		}
		return devicePollResult{status: devicePollPending}, nil
	}
	if admissionErr != nil {
		return devicePollResult{}, failure(KindMalformed, "poll OpenAI Codex device login", "openai", admissionErr)
	}
	var code string
	if raw, ok := root["error"]; ok {
		if json.Unmarshal(raw, &code) != nil {
			var nested struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(raw, &nested)
			code = nested.Code
		}
	}
	if code == "deviceauth_authorization_pending" {
		return devicePollResult{status: devicePollPending}, nil
	}
	if code == "slow_down" {
		return devicePollResult{status: devicePollSlowDown}, nil
	}
	return devicePollResult{}, failure(KindOAuth, "poll OpenAI Codex device login", "openai", nil)
}

func readJSONDocument(response *http.Response, maximum int64, allowEmpty bool) ([]byte, map[string]json.RawMessage, error) {
	body, err := boundedRead(response.Body, maximum)
	if err != nil {
		return nil, nil, err
	}
	if len(body) == 0 && allowEmpty {
		return body, nil, nil
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return nil, nil, errors.New("response content type is not application/json")
	}
	if !utf8.Valid(body) {
		return nil, nil, errors.New("response is not valid UTF-8")
	}
	root, err := decodeObject(body)
	if err != nil {
		return nil, nil, err
	}
	return body, root, nil
}

func boundedRead(reader io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, errors.New("response exceeds limit")
	}
	return data, nil
}
func isPositiveFinite(v float64) bool { return v > 0 && !math.IsInf(v, 0) && !math.IsNaN(v) }
func durationSeconds(raw json.RawMessage) (time.Duration, error) {
	var n float64
	if json.Unmarshal(raw, &n) != nil {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, err
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return 0, err
		}
		n = parsed
	}
	if n < 0 || math.IsNaN(n) || math.IsInf(n, 0) || n > float64(math.MaxInt64/int64(time.Second)) {
		return 0, errors.New("invalid interval")
	}
	return time.Duration(n * float64(time.Second)), nil
}
func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}
func contextFailure(ctx context.Context, operation string) error {
	if ctx == nil {
		return failure(KindInvalid, operation, "openai", nil)
	}
	if cause := context.Cause(ctx); cause != nil {
		return failure(KindCancelled, operation, "openai", cause)
	}
	return nil
}
func requestFailure(ctx context.Context, operation string, err error) error {
	if ctx != nil && context.Cause(ctx) != nil {
		if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			return failure(KindTimeout, operation, "openai", context.Cause(ctx))
		}
		return failure(KindCancelled, operation, "openai", context.Cause(ctx))
	}
	return failure(KindOAuth, operation, "openai", err)
}

func deviceContextFailure(cause error) error {
	if errors.Is(cause, context.DeadlineExceeded) {
		return failure(KindTimeout, "complete OpenAI Codex device login", "openai", cause)
	}
	return failure(KindCancelled, "complete OpenAI Codex device login", "openai", cause)
}

func parseAuthorizationInput(input string) (code, state string) {
	value := strings.TrimSpace(input)
	if value == "" {
		return "", ""
	}
	if parsed, err := url.Parse(value); err == nil && parsed.IsAbs() {
		query := parsed.Query()
		return singleQueryValue(query, "code"), singleQueryValue(query, "state")
	}
	if before, after, found := strings.Cut(value, "#"); found {
		return before, after
	}
	if strings.Contains(value, "code=") {
		parsed, err := url.ParseQuery(value)
		if err == nil {
			return singleQueryValue(parsed, "code"), singleQueryValue(parsed, "state")
		}
	}
	return value, ""
}
func singleQueryValue(values url.Values, name string) string {
	if len(values[name]) != 1 {
		return ""
	}
	return values[name][0]
}
func accountIDFromJWT(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", failure(KindMalformed, "read OpenAI Codex token", "openai", nil)
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !utf8.Valid(body) {
		return "", failure(KindMalformed, "read OpenAI Codex token", "openai", err)
	}
	root, err := decodeObject(body)
	if err != nil {
		return "", failure(KindMalformed, "read OpenAI Codex token", "openai", err)
	}
	raw, ok := root[openAICodexClaim]
	if !ok {
		return "", failure(KindMalformed, "read OpenAI Codex token", "openai", nil)
	}
	var claim struct {
		AccountID string `json:"chatgpt_account_id"`
	}
	if json.Unmarshal(raw, &claim) != nil || !validOAuthText(claim.AccountID) {
		return "", failure(KindMalformed, "read OpenAI Codex token", "openai", nil)
	}
	return claim.AccountID, nil
}

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	AnthropicOAuthClientID         = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	defaultAnthropicAuthorizeURL   = "https://claude.ai/oauth/authorize"
	defaultAnthropicTokenURL       = "https://platform.claude.com/v1/oauth/token"
	defaultAnthropicCallbackPort   = 53692
	defaultAnthropicRequestTimeout = 30 * time.Second
	defaultAnthropicOAuthSkew      = 5 * time.Minute
	anthropicCallbackPath          = "/callback"
	anthropicOAuthScopes           = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
)

// AnthropicOAuthConfig controls the Claude Pro/Max OAuth boundary. The zero
// value uses the same public client, endpoints, localhost callback and timeout
// as pi. Browser opening remains caller-owned so headless transports can show
// the URL and use the manual-code path instead.
type AnthropicOAuthConfig struct {
	HTTPClient       *http.Client
	Clock            func() time.Time
	AuthorizeURL     string
	TokenURL         string
	CallbackHost     string
	CallbackPort     int
	CallbackListener ListenerFactory
	BrowserOpener    BrowserOpener
	Random           io.Reader
	RequestTimeout   time.Duration
	MaxResponseBytes int64
}

type AnthropicOAuth struct {
	client       *http.Client
	now          func() time.Time
	authorizeURL string
	tokenURL     string
	host         string
	port         int
	listener     ListenerFactory
	opener       BrowserOpener
	random       io.Reader
	timeout      time.Duration
	maxBody      int64
}

func NewAnthropicOAuth(config AnthropicOAuthConfig) (*AnthropicOAuth, error) {
	authorizeURL := config.AuthorizeURL
	if authorizeURL == "" {
		authorizeURL = defaultAnthropicAuthorizeURL
	}
	tokenURL := config.TokenURL
	if tokenURL == "" {
		tokenURL = defaultAnthropicTokenURL
	}
	for _, endpoint := range []string{authorizeURL, tokenURL} {
		parsed, err := url.Parse(endpoint)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, failure(KindInvalid, "initialize Anthropic OAuth", AnthropicProviderID, nil)
		}
	}
	host := config.CallbackHost
	if host == "" {
		host = defaultCallbackHost
	}
	port := config.CallbackPort
	if port == 0 {
		port = defaultAnthropicCallbackPort
	}
	if port < 1 || port > 65535 || strings.TrimSpace(host) == "" {
		return nil, failure(KindInvalid, "initialize Anthropic OAuth", AnthropicProviderID, nil)
	}
	now := config.Clock
	if now == nil {
		now = time.Now
	}
	random := config.Random
	if random == nil {
		random = rand.Reader
	}
	timeout := config.RequestTimeout
	if timeout == 0 {
		timeout = defaultAnthropicRequestTimeout
	}
	maximum := config.MaxResponseBytes
	if maximum == 0 {
		maximum = defaultOAuthResponseBytes
	}
	if timeout < 0 || maximum < 256 {
		return nil, failure(KindInvalid, "initialize Anthropic OAuth", AnthropicProviderID, nil)
	}
	return &AnthropicOAuth{
		client: cloneOAuthHTTPClient(config.HTTPClient), now: now,
		authorizeURL: strings.TrimRight(authorizeURL, "/"), tokenURL: strings.TrimRight(tokenURL, "/"),
		host: host, port: port, listener: config.CallbackListener, opener: config.BrowserOpener,
		random: random, timeout: timeout, maxBody: maximum,
	}, nil
}

func (o *AnthropicOAuth) callbackURL() string {
	return "http://localhost:" + strconv.Itoa(o.port) + anthropicCallbackPath
}

// AnthropicAuthorization is one bound PKCE login transaction. State is the
// PKCE verifier in the upstream protocol, and Wait accepts either the local
// callback or a manually pasted code/redirect URL.
type AnthropicAuthorization struct {
	URL         string
	callback    *callbackWaiter
	flow        *AnthropicOAuth
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

func (a *AnthropicAuthorization) Close() {
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

func (a *AnthropicAuthorization) Cancel() { a.Close() }

func (a *AnthropicAuthorization) Open(ctx context.Context) error {
	if err := anthropicContextFailure(ctx, "open OAuth browser"); err != nil {
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
			return failure(KindCancelled, "open OAuth browser", AnthropicProviderID, context.Canceled)
		}
	}
	if a == nil || a.flow == nil || a.flow.opener == nil {
		if a != nil {
			a.Close()
		}
		return failure(KindUnsupported, "open OAuth browser", AnthropicProviderID, nil)
	}
	if err := a.flow.opener(ctx, a.URL); err != nil {
		a.Close()
		return anthropicRequestFailure(ctx, "open OAuth browser", err)
	}
	return nil
}

func (o *AnthropicOAuth) BeginBrowserLogin(ctx context.Context) (*AnthropicAuthorization, error) {
	if err := anthropicContextFailure(ctx, "start Anthropic browser login"); err != nil {
		return nil, err
	}
	verifier, challenge, err := o.pkce()
	if err != nil {
		return nil, err
	}
	waiter, err := startProviderCallbackWaiter(
		o.host, o.port, verifier, o.listener, AnthropicProviderID, anthropicCallbackPath,
		"Anthropic authentication completed. You can close this window.",
	)
	if err != nil {
		return nil, err
	}
	redirectURI := o.callbackURL()
	query := url.Values{
		"code":                  {"true"},
		"client_id":             {AnthropicOAuthClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {anthropicOAuthScopes},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {verifier},
	}
	txContext, cancel := context.WithCancelCause(context.Background())
	authorization := &AnthropicAuthorization{
		URL: o.authorizeURL + "?" + query.Encode(), callback: waiter, flow: o,
		verifier: verifier, redirectURI: redirectURI, ctx: txContext, cancel: cancel,
	}
	authorization.stopContext = context.AfterFunc(ctx, authorization.Close)
	if cause := context.Cause(ctx); cause != nil {
		authorization.Close()
		return nil, failure(KindCancelled, "start Anthropic browser login", AnthropicProviderID, cause)
	}
	return authorization, nil
}

func (a *AnthropicAuthorization) Wait(ctx context.Context, manual <-chan string) (OAuthCredential, error) {
	if a == nil || a.callback == nil {
		return OAuthCredential{}, failure(KindInvalid, "wait for OAuth callback", AnthropicProviderID, nil)
	}
	a.waitMu.Lock()
	if a.waitStarted {
		a.waitMu.Unlock()
		return OAuthCredential{}, failure(KindInvalid, "wait for OAuth callback", AnthropicProviderID, nil)
	}
	a.waitStarted = true
	a.waitMu.Unlock()
	if ctx == nil {
		a.Close()
		return OAuthCredential{}, failure(KindInvalid, "wait for OAuth callback", AnthropicProviderID, nil)
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
			return a.flow.ExchangeCode(waitCtx, result.code, a.verifier, a.verifier, a.redirectURI)
		case input, ok := <-manual:
			if !ok {
				manual = nil
				continue
			}
			code, state := parseAuthorizationInput(input)
			if len(input) > callbackMaxQueryBytes || code == "" || len(code) > callbackMaxCodeBytes || (state != "" && state != a.verifier) {
				return OAuthCredential{}, failure(KindOAuth, "validate manual OAuth callback", AnthropicProviderID, nil)
			}
			if state == "" {
				state = a.verifier
			}
			return a.flow.ExchangeCode(waitCtx, code, state, a.verifier, a.redirectURI)
		case <-waitCtx.Done():
			return OAuthCredential{}, failure(KindCancelled, "wait for OAuth callback", AnthropicProviderID, context.Cause(waitCtx))
		}
	}
}

func (o *AnthropicOAuth) pkce() (string, string, error) {
	value := make([]byte, 32)
	if _, err := io.ReadFull(o.random, value); err != nil {
		return "", "", failure(KindIO, "generate OAuth PKCE", AnthropicProviderID, err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(value)
	digest := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func (o *AnthropicOAuth) ExchangeCode(ctx context.Context, code, state, verifier, redirectURI string) (OAuthCredential, error) {
	if !validOAuthText(code) || !validOAuthText(state) || !validOAuthText(verifier) || !validOAuthText(redirectURI) {
		return OAuthCredential{}, failure(KindInvalid, "exchange Anthropic authorization code", AnthropicProviderID, nil)
	}
	return o.token(ctx, "exchange", map[string]string{
		"grant_type": "authorization_code", "client_id": AnthropicOAuthClientID,
		"code": code, "state": state, "redirect_uri": redirectURI, "code_verifier": verifier,
	})
}

func (o *AnthropicOAuth) Refresh(ctx context.Context, credential OAuthCredential) (OAuthCredential, error) {
	if !validOAuthText(credential.Refresh) {
		return OAuthCredential{}, failure(KindInvalid, "refresh Anthropic token", AnthropicProviderID, nil)
	}
	return o.token(ctx, "refresh", map[string]string{
		"grant_type": "refresh_token", "client_id": AnthropicOAuthClientID, "refresh_token": credential.Refresh,
	})
}

func (o *AnthropicOAuth) token(ctx context.Context, operation string, body map[string]string) (OAuthCredential, error) {
	if err := anthropicContextFailure(ctx, operation+" Anthropic token"); err != nil {
		return OAuthCredential{}, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return OAuthCredential{}, failure(KindInvalid, operation+" Anthropic token", AnthropicProviderID, err)
	}
	requestContext := ctx
	cancel := func() {}
	if o.timeout > 0 {
		requestContext, cancel = context.WithTimeout(ctx, o.timeout)
	}
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, o.tokenURL, strings.NewReader(string(encoded)))
	if err != nil {
		return OAuthCredential{}, failure(KindInvalid, operation+" Anthropic token", AnthropicProviderID, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := o.client.Do(request)
	if err != nil {
		return OAuthCredential{}, anthropicRequestFailure(requestContext, operation+" Anthropic token", err)
	}
	defer response.Body.Close()
	data, _, admissionErr := readJSONDocument(response, o.maxBody, response.StatusCode < 200 || response.StatusCode > 299)
	if response.StatusCode < 200 || response.StatusCode > 299 {
		if admissionErr != nil {
			return OAuthCredential{}, failure(KindMalformed, operation+" Anthropic token", AnthropicProviderID, admissionErr)
		}
		return OAuthCredential{}, failure(KindOAuth, operation+" Anthropic token", AnthropicProviderID, nil)
	}
	if admissionErr != nil {
		return OAuthCredential{}, failure(KindMalformed, operation+" Anthropic token", AnthropicProviderID, admissionErr)
	}
	var wire struct {
		Access  string          `json:"access_token"`
		Refresh string          `json:"refresh_token"`
		Expires json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &wire); err != nil || !validOAuthText(wire.Access) || !validOAuthText(wire.Refresh) {
		return OAuthCredential{}, failure(KindMalformed, operation+" Anthropic token", AnthropicProviderID, err)
	}
	var seconds float64
	if err := json.Unmarshal(wire.Expires, &seconds); err != nil || !isPositiveFinite(seconds) || seconds > float64(math.MaxInt64/int64(time.Second)) {
		return OAuthCredential{}, failure(KindMalformed, operation+" Anthropic token", AnthropicProviderID, err)
	}
	expiresIn := time.Duration(seconds * float64(time.Second))
	return OAuthCredential{
		Access: wire.Access, Refresh: wire.Refresh,
		Expires: o.now().Add(expiresIn - defaultAnthropicOAuthSkew).UnixMilli(),
	}, nil
}

func anthropicContextFailure(ctx context.Context, operation string) error {
	if ctx == nil {
		return failure(KindInvalid, operation, AnthropicProviderID, nil)
	}
	if cause := context.Cause(ctx); cause != nil {
		return failure(KindCancelled, operation, AnthropicProviderID, cause)
	}
	return nil
}

func anthropicRequestFailure(ctx context.Context, operation string, err error) error {
	if ctx != nil && context.Cause(ctx) != nil {
		if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			return failure(KindTimeout, operation, AnthropicProviderID, context.Cause(ctx))
		}
		return failure(KindCancelled, operation, AnthropicProviderID, context.Cause(ctx))
	}
	return failure(KindOAuth, operation, AnthropicProviderID, err)
}

package application

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cat3399/pi-go/internal/auth"
)

var (
	ErrProviderOAuthUnsupported = errors.New("provider does not support OAuth authentication")
	ErrOAuthLoginClosed         = errors.New("OAuth login is no longer pending")
)

// ProviderOAuthLogin is a transport-neutral, single-use OAuth transaction.
// A GUI, TUI, or Web surface presents URL, optionally submits a manually
// copied callback URL through Submit, and waits for durable completion.
type ProviderOAuthLogin struct {
	ProviderID   string
	URL          string
	Instructions string

	manual chan string
	done   chan struct{}
	cancel context.CancelCauseFunc

	resultMu sync.RWMutex
	result   error
	finish   sync.Once
}

func (login *ProviderOAuthLogin) Submit(value string) error {
	if login == nil {
		return ErrOAuthLoginClosed
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("OAuth authorization code is required")
	}
	select {
	case <-login.done:
		return ErrOAuthLoginClosed
	default:
	}
	select {
	case login.manual <- value:
		return nil
	case <-login.done:
		return ErrOAuthLoginClosed
	default:
		return errors.New("OAuth authorization input is already pending")
	}
}

func (login *ProviderOAuthLogin) Wait(ctx context.Context) error {
	if login == nil {
		return ErrOAuthLoginClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-login.done:
		login.resultMu.RLock()
		defer login.resultMu.RUnlock()
		return login.result
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (login *ProviderOAuthLogin) Close() {
	if login != nil && login.cancel != nil {
		login.cancel(context.Canceled)
	}
}

func (login *ProviderOAuthLogin) complete(err error) {
	login.finish.Do(func() {
		login.resultMu.Lock()
		login.result = err
		login.resultMu.Unlock()
		close(login.done)
	})
}

type providerAuthorization struct {
	url   string
	wait  func(context.Context, <-chan string) (auth.OAuthCredential, error)
	close func()
}

func (s *Service) StartProviderOAuth(ctx context.Context, providerID string) (*ProviderOAuthLogin, error) {
	if s == nil {
		return nil, errors.New("application service is unavailable")
	}
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return nil, fmt.Errorf("%w: provider is required", ErrProviderOAuthUnsupported)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	transactionContext, cancel := context.WithCancelCause(ctx)
	authorization, err := s.beginProviderAuthorization(transactionContext, providerID)
	if err != nil {
		cancel(err)
		return nil, err
	}
	login := &ProviderOAuthLogin{
		ProviderID: providerID, URL: authorization.url,
		Instructions: "Complete sign-in in the browser. If the callback does not complete automatically, paste the redirect URL.",
		manual:       make(chan string, 1), done: make(chan struct{}), cancel: cancel,
	}
	stopServiceCancellation := context.AfterFunc(s.ctx, func() { cancel(context.Canceled) })
	go func() {
		defer stopServiceCancellation()
		defer authorization.close()
		credential, waitErr := authorization.wait(transactionContext, login.manual)
		if waitErr == nil {
			store, storeErr := auth.NewStore(auth.Options{Path: filepath.Join(s.paths.AgentDir, "auth.json")})
			if storeErr != nil {
				waitErr = storeErr
			} else {
				s.modelMu.Lock()
				waitErr = store.SetOAuth(transactionContext, providerID, credential)
				s.modelMu.Unlock()
			}
		}
		login.complete(waitErr)
		cancel(context.Canceled)
	}()
	return login, nil
}

func (s *Service) beginProviderAuthorization(ctx context.Context, providerID string) (providerAuthorization, error) {
	callbackHost := environmentValue(s.production.Environment, "PI_OAUTH_CALLBACK_HOST")
	switch providerID {
	case auth.OpenAICodexProviderID:
		flow, err := auth.NewOpenAICodexOAuth(auth.OpenAICodexOAuthConfig{
			HTTPClient: s.production.OpenAIOAuthHTTPClient, AuthBaseURL: s.production.OpenAIOAuthBaseURL,
			Clock: s.production.OpenAIOAuthClock, CallbackHost: callbackHost,
		})
		if err != nil {
			return providerAuthorization{}, err
		}
		authorization, err := flow.BeginBrowserLogin(ctx)
		if err != nil {
			return providerAuthorization{}, err
		}
		return providerAuthorization{
			url: authorization.URL, wait: authorization.Wait, close: authorization.Close,
		}, nil
	case auth.AnthropicProviderID:
		flow, err := auth.NewAnthropicOAuth(auth.AnthropicOAuthConfig{
			HTTPClient: s.production.AnthropicOAuthHTTPClient, TokenURL: s.production.AnthropicOAuthTokenURL,
			Clock: s.production.AnthropicOAuthClock, CallbackHost: callbackHost,
		})
		if err != nil {
			return providerAuthorization{}, err
		}
		authorization, err := flow.BeginBrowserLogin(ctx)
		if err != nil {
			return providerAuthorization{}, err
		}
		return providerAuthorization{
			url: authorization.URL, wait: authorization.Wait, close: authorization.Close,
		}, nil
	default:
		return providerAuthorization{}, fmt.Errorf("%w: %s", ErrProviderOAuthUnsupported, providerID)
	}
}

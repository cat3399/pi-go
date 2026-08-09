package auth

import (
	"context"
	"strings"
	"time"
)

const (
	AnthropicAuthTokenEnvironment  = "ANTHROPIC_AUTH_TOKEN"
	AnthropicOAuthTokenEnvironment = "ANTHROPIC_OAUTH_TOKEN"
	AnthropicAPIKeyEnvironment     = "ANTHROPIC_API_KEY"
)

type AnthropicResolveOptions struct {
	OAuth              *AnthropicOAuth
	Clock              func() time.Time
	MinimumValidity    time.Duration
	MinimumValiditySet bool
}

// ResolveAnthropicAuth mirrors the provider's auth composition order. An
// explicit or stored API key is passed through as apiKey; ANTHROPIC_AUTH_TOKEN
// owns the Authorization header because it is not an Anthropic SDK API key.
// ANTHROPIC_OAUTH_TOKEN remains an APIKey result so the adapter can recognize
// sk-ant-oat tokens and install Claude Code OAuth identity headers.
func ResolveAnthropicAuth(ctx context.Context, runtime *Runtime, explicit *string, configured *string, ambient map[string]string, options AnthropicResolveOptions) (OpenAIAuthResult, error) {
	if explicit != nil {
		if !validAPIKey(*explicit) {
			return OpenAIAuthResult{}, failure(KindInvalid, "resolve CLI API key", AnthropicProviderID, nil)
		}
		return OpenAIAuthResult{APIKey: *explicit, Source: "CLI API key"}, nil
	}
	credential, exists, err := runtime.Read(ctx, AnthropicProviderID)
	if err != nil {
		return OpenAIAuthResult{}, err
	}
	if exists {
		switch credential.Type {
		case "api_key":
			key, resolveErr := ResolveValue(ctx, credential.Key, "stored Anthropic API key", credential.Env, ambient)
			if resolveErr != nil {
				return OpenAIAuthResult{}, resolveErr
			}
			return OpenAIAuthResult{APIKey: key, Source: "stored credential", Env: cloneEnv(credential.Env)}, nil
		case "oauth":
			return resolveStoredAnthropicOAuth(ctx, runtime, credential.OAuth, options)
		default:
			return OpenAIAuthResult{}, failure(KindUnsupported, "resolve stored credential", AnthropicProviderID, ErrCredentialType)
		}
	}
	if configured != nil {
		key, resolveErr := ResolveValueUncached(ctx, *configured, "configured Anthropic API key", nil, ambient)
		if resolveErr != nil {
			return OpenAIAuthResult{}, resolveErr
		}
		return OpenAIAuthResult{APIKey: key, Source: "configured API key"}, nil
	}
	if token := ambient[AnthropicAuthTokenEnvironment]; token != "" {
		if !validAPIKey(token) {
			return OpenAIAuthResult{}, failure(KindInvalid, "resolve environment auth token", AnthropicProviderID, nil)
		}
		return OpenAIAuthResult{
			Source:  AnthropicAuthTokenEnvironment,
			Headers: map[string]string{"Authorization": "Bearer " + token},
		}, nil
	}
	for _, name := range []string{AnthropicOAuthTokenEnvironment, AnthropicAPIKeyEnvironment} {
		if key := ambient[name]; key != "" {
			if !validAPIKey(key) {
				return OpenAIAuthResult{}, failure(KindInvalid, "resolve environment API key", AnthropicProviderID, nil)
			}
			return OpenAIAuthResult{APIKey: key, Source: name}, nil
		}
	}
	return OpenAIAuthResult{}, failure(KindNotConfigured, "resolve Anthropic credential", AnthropicProviderID, nil)
}

func resolveStoredAnthropicOAuth(ctx context.Context, runtime *Runtime, initial OAuthCredential, options AnthropicResolveOptions) (OpenAIAuthResult, error) {
	if options.OAuth == nil {
		return OpenAIAuthResult{}, failure(KindUnsupported, "resolve stored OAuth credential", AnthropicProviderID, ErrCredentialType)
	}
	now := options.Clock
	if now == nil {
		now = time.Now
	}
	requireMinimumAfterRefresh := options.MinimumValiditySet || options.MinimumValidity != 0
	skew := options.MinimumValidity
	if skew == 0 {
		skew = DefaultOAuthRefreshSkew
	}
	if skew < DefaultOAuthRefreshSkew {
		skew = DefaultOAuthRefreshSkew
	}
	expiresSoon := func(value OAuthCredential) bool { return !now().Add(skew).Before(value.Expiry()) }
	credential := initial
	if expiresSoon(credential) {
		if key, ok := runtime.runtimeKey(AnthropicProviderID); ok {
			return OpenAIAuthResult{APIKey: key, Source: "runtime API key"}, nil
		}
		post, exists, err := runtime.store.ModifyOAuth(ctx, AnthropicProviderID, func(current OAuthCredential) (OAuthCredential, bool, error) {
			if !expiresSoon(current) {
				return current, false, nil
			}
			next, refreshErr := options.OAuth.Refresh(ctx, current)
			if refreshErr != nil {
				return OAuthCredential{}, false, refreshErr
			}
			return next, true, nil
		})
		if err != nil {
			return OpenAIAuthResult{}, err
		}
		if !exists {
			return OpenAIAuthResult{}, failure(KindNotConfigured, "resolve stored OAuth credential", AnthropicProviderID, nil)
		}
		credential = post
		if requireMinimumAfterRefresh && expiresSoon(credential) {
			return OpenAIAuthResult{}, failure(KindOAuth, "validate refreshed OAuth credential", AnthropicProviderID, nil)
		}
	}
	if !validOAuthText(credential.Access) {
		return OpenAIAuthResult{}, failure(KindMalformed, "resolve stored OAuth credential", AnthropicProviderID, nil)
	}
	return OpenAIAuthResult{APIKey: credential.Access, Source: "OAuth", Env: oauthCredentialEnv(credential)}, nil
}

// HasAuthorization reports whether a resolved auth result can authorize a
// request without exposing the credential value to status/UI callers.
func (r OpenAIAuthResult) HasAuthorization() bool {
	if validAPIKey(r.APIKey) {
		return true
	}
	for name, value := range r.Headers {
		if strings.EqualFold(name, "authorization") ||
			strings.EqualFold(name, "x-api-key") ||
			strings.EqualFold(name, "cf-aig-authorization") {
			return validAPIKey(value)
		}
	}
	return false
}

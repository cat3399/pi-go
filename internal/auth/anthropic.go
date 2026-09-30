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
	OAuth           *AnthropicOAuth
	Clock           func() time.Time
	MinimumValidity time.Duration
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
	return resolveStoredOAuth(ctx, runtime, AnthropicProviderID, initial, options.Clock, options.MinimumValidity, options.OAuth.Refresh)
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

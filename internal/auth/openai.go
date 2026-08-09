package auth

import (
	"context"
	"encoding/json"
	"time"
)

const DefaultOAuthRefreshSkew = 5 * time.Minute

const (
	OpenAIProviderID      = "openai"
	OpenAICodexProviderID = "openai-codex"
	AnthropicProviderID   = "anthropic"
)

// OpenAIAuthResult is the provider-ready projection of an API key or a
// persisted OpenAI Codex OAuth credential. AccountID is metadata for callers
// that need to surface the selected ChatGPT account; the standard Responses
// adapter only requires the bearer token, matching the fixed upstream flow.
type OpenAIAuthResult struct {
	APIKey    string
	Source    string
	AccountID string
	Headers   map[string]string
	Env       map[string]string
}

type OpenAIResolveOptions struct {
	OAuth           *OpenAICodexOAuth
	Clock           func() time.Time
	MinimumValidity time.Duration
	// MinimumValiditySet distinguishes an explicit zero override from omission,
	// matching minOAuthValidityMs?: number in the upstream resolver.
	MinimumValiditySet bool
}

// ResolveOpenAIKey codifies product precedence. A selected stored credential
// owns the provider: malformed, OAuth, unresolved, or invalid stored data is
// an error and must not fall through to lower-priority configured/environment
// sources.
func ResolveOpenAIKey(ctx context.Context, runtime *Runtime, explicit *string, configured *string, ambient map[string]string) (string, error) {
	resolved, err := resolveOpenAIAPIKey(ctx, runtime, explicit, configured, ambient)
	return resolved.APIKey, err
}

func resolveOpenAIAPIKey(ctx context.Context, runtime *Runtime, explicit *string, configured *string, ambient map[string]string) (OpenAIAuthResult, error) {
	if explicit != nil {
		if !validAPIKey(*explicit) {
			return OpenAIAuthResult{}, failure(KindInvalid, "resolve CLI API key", OpenAIProviderID, nil)
		}
		return OpenAIAuthResult{APIKey: *explicit, Source: "CLI API key"}, nil
	}
	credential, exists, err := runtime.Read(ctx, OpenAIProviderID)
	if err != nil {
		return OpenAIAuthResult{}, err
	}
	if exists {
		if credential.Type != "api_key" {
			return OpenAIAuthResult{}, failure(KindUnsupported, "resolve stored credential", OpenAIProviderID, ErrCredentialType)
		}
		key, resolveErr := ResolveValue(ctx, credential.Key, "stored OpenAI API key", credential.Env, ambient)
		if resolveErr != nil {
			return OpenAIAuthResult{}, resolveErr
		}
		return OpenAIAuthResult{APIKey: key, Source: "stored credential", Env: cloneEnv(credential.Env)}, nil
	}
	if configured != nil {
		key, resolveErr := ResolveValueUncached(ctx, *configured, "configured OpenAI API key", nil, ambient)
		if resolveErr != nil {
			return OpenAIAuthResult{}, resolveErr
		}
		return OpenAIAuthResult{APIKey: key, Source: "configured API key"}, nil
	}
	if value := ambient["OPENAI_API_KEY"]; value != "" {
		if !validAPIKey(value) {
			return OpenAIAuthResult{}, failure(KindInvalid, "resolve environment API key", OpenAIProviderID, nil)
		}
		return OpenAIAuthResult{APIKey: value, Source: "OPENAI_API_KEY"}, nil
	}
	return OpenAIAuthResult{}, failure(KindNotConfigured, "resolve OpenAI API key", OpenAIProviderID, nil)
}

// ResolveOpenAIAuth resolves only the standard OpenAI API-key provider. Codex
// OAuth intentionally has a separate provider identity and resolver below.
// The options argument remains for source compatibility with existing callers
// while production composition migrates to the independent Codex route.
func ResolveOpenAIAuth(ctx context.Context, runtime *Runtime, explicit *string, configured *string, ambient map[string]string, _ OpenAIResolveOptions) (OpenAIAuthResult, error) {
	return resolveOpenAIAPIKey(ctx, runtime, explicit, configured, ambient)
}

// ResolveOpenAICodexAuth resolves the independent ChatGPT Codex provider. A
// selected stored credential owns this provider: malformed or failed OAuth
// refreshes never fall through to a configured/runtime key. API-key records
// remain supported for explicit testing and compatible private gateways.
func ResolveOpenAICodexAuth(ctx context.Context, runtime *Runtime, explicit *string, configured *string, ambient map[string]string, options OpenAIResolveOptions) (OpenAIAuthResult, error) {
	if explicit != nil {
		if !validAPIKey(*explicit) {
			return OpenAIAuthResult{}, failure(KindInvalid, "resolve CLI API key", OpenAICodexProviderID, nil)
		}
		return OpenAIAuthResult{APIKey: *explicit, Source: "CLI API key"}, nil
	}
	credential, exists, err := runtime.Read(ctx, OpenAICodexProviderID)
	if err != nil {
		return OpenAIAuthResult{}, err
	}
	if exists {
		switch credential.Type {
		case "api_key":
			key, resolveErr := ResolveValue(ctx, credential.Key, "stored OpenAI Codex API key", credential.Env, ambient)
			if resolveErr != nil {
				return OpenAIAuthResult{}, resolveErr
			}
			return OpenAIAuthResult{APIKey: key, Source: "stored credential", Env: cloneEnv(credential.Env)}, nil
		case "oauth":
			return resolveStoredOpenAIOAuth(ctx, runtime, credential.OAuth, options)
		default:
			return OpenAIAuthResult{}, failure(KindUnsupported, "resolve stored credential", OpenAICodexProviderID, ErrCredentialType)
		}
	}
	if configured != nil {
		key, resolveErr := ResolveValueUncached(ctx, *configured, "configured OpenAI Codex API key", nil, ambient)
		if resolveErr != nil {
			return OpenAIAuthResult{}, resolveErr
		}
		return OpenAIAuthResult{APIKey: key, Source: "configured API key"}, nil
	}
	return OpenAIAuthResult{}, failure(KindNotConfigured, "resolve OpenAI Codex credential", OpenAICodexProviderID, nil)
}

func resolveStoredOpenAIOAuth(ctx context.Context, runtime *Runtime, initial OAuthCredential, options OpenAIResolveOptions) (OpenAIAuthResult, error) {
	if options.OAuth == nil {
		return OpenAIAuthResult{}, failure(KindUnsupported, "resolve stored OAuth credential", OpenAICodexProviderID, ErrCredentialType)
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
	valid := func(value OAuthCredential) bool { return now().Add(skew).Before(value.Expiry()) }
	credential := initial
	if !valid(credential) {
		// A concurrent CLI/runtime override wins before an irreversible token
		// refresh. This makes runtime ownership explicit even during a refresh.
		if key, ok := runtime.runtimeKey(OpenAICodexProviderID); ok {
			return OpenAIAuthResult{APIKey: key, Source: "runtime API key"}, nil
		}
		post, exists, err := runtime.store.ModifyOAuth(ctx, OpenAICodexProviderID, func(current OAuthCredential) (OAuthCredential, bool, error) {
			if valid(current) {
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
			return OpenAIAuthResult{}, failure(KindNotConfigured, "resolve stored OAuth credential", OpenAICodexProviderID, nil)
		}
		credential = post
		if requireMinimumAfterRefresh && !valid(credential) {
			return OpenAIAuthResult{}, failure(KindOAuth, "validate refreshed OAuth credential", OpenAICodexProviderID, nil)
		}
	}
	if !validOAuthText(credential.Access) {
		return OpenAIAuthResult{}, failure(KindMalformed, "resolve stored OAuth credential", OpenAICodexProviderID, nil)
	}
	return OpenAIAuthResult{APIKey: credential.Access, Source: "OAuth", AccountID: credential.AccountID, Env: oauthCredentialEnv(credential)}, nil
}

// ResolveProviderAPIKey provides the same composed API-key path for a
// models.json-only provider. Built-in environment and OAuth behavior remain
// owned by their provider-specific resolvers.
func ResolveProviderAPIKey(ctx context.Context, runtime *Runtime, providerID string, explicit *string, configured *string, ambient map[string]string) (OpenAIAuthResult, error) {
	if !validProviderID(providerID) {
		return OpenAIAuthResult{}, failure(KindInvalid, "resolve provider API key", providerID, nil)
	}
	if explicit != nil {
		if !validAPIKey(*explicit) {
			return OpenAIAuthResult{}, failure(KindInvalid, "resolve CLI API key", providerID, nil)
		}
		return OpenAIAuthResult{APIKey: *explicit, Source: "CLI API key"}, nil
	}
	credential, exists, err := runtime.Read(ctx, providerID)
	if err != nil {
		return OpenAIAuthResult{}, err
	}
	if exists {
		if credential.Type != "api_key" {
			return OpenAIAuthResult{}, failure(KindUnsupported, "resolve stored credential", providerID, ErrCredentialType)
		}
		key, resolveErr := ResolveValue(ctx, credential.Key, "stored provider API key", credential.Env, ambient)
		if resolveErr != nil {
			return OpenAIAuthResult{}, resolveErr
		}
		return OpenAIAuthResult{APIKey: key, Source: "stored credential", Env: cloneEnv(credential.Env)}, nil
	}
	if configured != nil {
		key, resolveErr := ResolveValueUncached(ctx, *configured, "configured provider API key", nil, ambient)
		if resolveErr != nil {
			return OpenAIAuthResult{}, resolveErr
		}
		return OpenAIAuthResult{APIKey: key, Source: "configured API key"}, nil
	}
	return OpenAIAuthResult{}, failure(KindNotConfigured, "resolve provider API key", providerID, nil)
}

func oauthCredentialEnv(credential OAuthCredential) map[string]string {
	raw := credential.Extra["env"]
	if len(raw) == 0 {
		return nil
	}
	var environment map[string]string
	if json.Unmarshal(raw, &environment) != nil {
		return nil
	}
	for name, value := range environment {
		if !validEnvironmentName(name) || !utf8Valid(value) {
			return nil
		}
	}
	return cloneEnv(environment)
}

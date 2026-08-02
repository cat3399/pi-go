package auth

import (
	"context"
	"time"
)

const DefaultOAuthRefreshSkew = 5 * time.Minute

// OpenAIAuthResult is the provider-ready projection of an API key or a
// persisted OpenAI Codex OAuth credential. AccountID is metadata for callers
// that need to surface the selected ChatGPT account; the standard Responses
// adapter only requires the bearer token, matching the fixed upstream flow.
type OpenAIAuthResult struct {
	APIKey    string
	Source    string
	AccountID string
}

type OpenAIResolveOptions struct {
	OAuth           *OpenAICodexOAuth
	Clock           func() time.Time
	MinimumValidity time.Duration
}

// ResolveOpenAIKey codifies product precedence. A selected stored credential
// owns the provider: malformed, OAuth, unresolved, or invalid stored data is
// an error and must not fall through to lower-priority configured/environment
// sources.
func ResolveOpenAIKey(ctx context.Context, runtime *Runtime, explicit *string, configured *string, ambient map[string]string) (string, error) {
	if explicit != nil {
		if !validAPIKey(*explicit) {
			return "", failure(KindInvalid, "resolve CLI API key", "openai", nil)
		}
		return *explicit, nil
	}
	credential, exists, err := runtime.Read(ctx, "openai")
	if err != nil {
		return "", err
	}
	if exists {
		if credential.Type != "api_key" {
			return "", failure(KindUnsupported, "resolve stored credential", "openai", ErrCredentialType)
		}
		return ResolveValue(ctx, credential.Key, "stored OpenAI API key", credential.Env, ambient)
	}
	if configured != nil {
		return ResolveValue(ctx, *configured, "configured OpenAI API key", nil, ambient)
	}
	if value := ambient["OPENAI_API_KEY"]; value != "" {
		if !validAPIKey(value) {
			return "", failure(KindInvalid, "resolve environment API key", "openai", nil)
		}
		return value, nil
	}
	return "", failure(KindNotConfigured, "resolve OpenAI API key", "openai", nil)
}

// ResolveOpenAIAuth resolves the existing API-key precedence and adds the
// OpenAI Codex OAuth branch. A stored OAuth record owns the provider: parse,
// refresh, or persistence failures do not fall through to config/environment.
// Refresh occurs inside Store.ModifyOAuth, which double-checks expiry under the
// same cross-process lock used for every auth.json mutation.
func ResolveOpenAIAuth(ctx context.Context, runtime *Runtime, explicit *string, configured *string, ambient map[string]string, options OpenAIResolveOptions) (OpenAIAuthResult, error) {
	if explicit != nil {
		if !validAPIKey(*explicit) {
			return OpenAIAuthResult{}, failure(KindInvalid, "resolve CLI API key", "openai", nil)
		}
		return OpenAIAuthResult{APIKey: *explicit, Source: "CLI API key"}, nil
	}
	credential, exists, err := runtime.Read(ctx, "openai")
	if err != nil {
		return OpenAIAuthResult{}, err
	}
	if exists {
		switch credential.Type {
		case "api_key":
			key, err := ResolveValue(ctx, credential.Key, "stored OpenAI API key", credential.Env, ambient)
			if err != nil {
				return OpenAIAuthResult{}, err
			}
			return OpenAIAuthResult{APIKey: key, Source: "stored credential"}, nil
		case "oauth":
			return resolveStoredOpenAIOAuth(ctx, runtime, credential.OAuth, options)
		default:
			return OpenAIAuthResult{}, failure(KindUnsupported, "resolve stored credential", "openai", ErrCredentialType)
		}
	}
	if configured != nil {
		key, err := ResolveValue(ctx, *configured, "configured OpenAI API key", nil, ambient)
		if err != nil {
			return OpenAIAuthResult{}, err
		}
		return OpenAIAuthResult{APIKey: key, Source: "configured API key"}, nil
	}
	if value := ambient["OPENAI_API_KEY"]; value != "" {
		if !validAPIKey(value) {
			return OpenAIAuthResult{}, failure(KindInvalid, "resolve environment API key", "openai", nil)
		}
		return OpenAIAuthResult{APIKey: value, Source: "OPENAI_API_KEY"}, nil
	}
	return OpenAIAuthResult{}, failure(KindNotConfigured, "resolve OpenAI API key", "openai", nil)
}

func resolveStoredOpenAIOAuth(ctx context.Context, runtime *Runtime, initial OAuthCredential, options OpenAIResolveOptions) (OpenAIAuthResult, error) {
	if options.OAuth == nil {
		return OpenAIAuthResult{}, failure(KindUnsupported, "resolve stored OAuth credential", "openai", ErrCredentialType)
	}
	now := options.Clock
	if now == nil {
		now = time.Now
	}
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
		if key, ok := runtime.runtimeKey("openai"); ok {
			return OpenAIAuthResult{APIKey: key, Source: "runtime API key"}, nil
		}
		post, exists, err := runtime.store.ModifyOAuth(ctx, "openai", func(current OAuthCredential) (OAuthCredential, bool, error) {
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
			return OpenAIAuthResult{}, failure(KindNotConfigured, "resolve stored OAuth credential", "openai", nil)
		}
		credential = post
	}
	if !validOAuthText(credential.Access) {
		return OpenAIAuthResult{}, failure(KindMalformed, "resolve stored OAuth credential", "openai", nil)
	}
	return OpenAIAuthResult{APIKey: credential.Access, Source: "OAuth", AccountID: credential.AccountID}, nil
}

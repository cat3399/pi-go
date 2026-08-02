package auth

import "context"

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

package auth

import (
	"context"
	"time"
)

const DefaultOAuthRefreshWindow = 3 * 24 * time.Hour

// Long-lived tokens refresh three days early. Short-lived tokens get half
// their lifetime before another refresh, including across process restarts.
func oauthRefreshAt(issued, expires time.Time) int64 {
	window := min(DefaultOAuthRefreshWindow, max(0, expires.Sub(issued))/2)
	return expires.Add(-window).UnixMilli()
}

func shouldRefreshOAuth(credential OAuthCredential, now time.Time, minimumValidity time.Duration) bool {
	if !now.Add(max(0, minimumValidity)).Before(credential.Expiry()) {
		return true
	}
	refreshAt := credential.RefreshAt
	if refreshAt <= 0 || refreshAt >= credential.Expires {
		refreshAt = credential.Expiry().Add(-DefaultOAuthRefreshWindow).UnixMilli()
	}
	return now.UnixMilli() >= refreshAt
}

func resolveStoredOAuth(
	ctx context.Context,
	runtime *Runtime,
	providerID string,
	initial OAuthCredential,
	clock func() time.Time,
	minimumValidity time.Duration,
	refresh func(context.Context, OAuthCredential) (OAuthCredential, error),
) (OpenAIAuthResult, error) {
	if clock == nil {
		clock = time.Now
	}
	credential := initial
	if shouldRefreshOAuth(credential, clock(), minimumValidity) {
		if key, ok := runtime.runtimeKey(providerID); ok {
			return OpenAIAuthResult{APIKey: key, Source: "runtime API key"}, nil
		}
		post, exists, err := runtime.store.ModifyOAuth(ctx, providerID, func(current OAuthCredential) (OAuthCredential, bool, error) {
			if !shouldRefreshOAuth(current, clock(), minimumValidity) {
				return current, false, nil
			}
			next, err := refresh(ctx, current)
			return next, err == nil, err
		})
		if err != nil {
			return OpenAIAuthResult{}, err
		}
		if !exists {
			return OpenAIAuthResult{}, failure(KindNotConfigured, "resolve stored OAuth credential", providerID, nil)
		}
		credential = post
	}
	// The refresh schedule is not a minimum lifetime requirement for the token
	// returned by the provider. Only expiry and a caller's explicit minimum apply.
	if !clock().Add(max(0, minimumValidity)).Before(credential.Expiry()) {
		return OpenAIAuthResult{}, failure(KindOAuth, "validate OAuth credential lifetime", providerID, nil)
	}
	if !validOAuthText(credential.Access) {
		return OpenAIAuthResult{}, failure(KindMalformed, "resolve stored OAuth credential", providerID, nil)
	}
	return OpenAIAuthResult{
		APIKey: credential.Access, Source: "OAuth", AccountID: credential.AccountID, Env: oauthCredentialEnv(credential),
	}, nil
}

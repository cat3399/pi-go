package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestOAuthRefreshScheduleSurvivesRestart(t *testing.T) {
	requirePersistentAuth(t)
	for _, providerID := range []string{OpenAICodexProviderID, AnthropicProviderID} {
		for _, lifetime := range []time.Duration{10 * 24 * time.Hour, 8 * time.Hour, time.Hour} {
			t.Run(providerID+"/"+lifetime.String(), func(t *testing.T) {
				now := time.Unix(2_100_000_000, 0)
				clock := func() time.Time { return now }
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"access_token": oauthJWT("account"), "refresh_token": "rotated", "expires_in": lifetime.Seconds(),
					})
				}))
				defer server.Close()
				var refresh func(context.Context, OAuthCredential) (OAuthCredential, error)
				var resolve func(*Runtime) (OpenAIAuthResult, error)
				if providerID == OpenAICodexProviderID {
					flow, err := NewOpenAICodexOAuth(OpenAICodexOAuthConfig{AuthBaseURL: server.URL, Clock: clock})
					if err != nil {
						t.Fatal(err)
					}
					refresh = flow.Refresh
					resolve = func(r *Runtime) (OpenAIAuthResult, error) {
						return ResolveOpenAICodexAuth(context.Background(), r, nil, nil, nil, OpenAIResolveOptions{OAuth: flow, Clock: clock})
					}
				} else {
					flow, err := NewAnthropicOAuth(AnthropicOAuthConfig{TokenURL: server.URL, Clock: clock})
					if err != nil {
						t.Fatal(err)
					}
					refresh = flow.Refresh
					resolve = func(r *Runtime) (OpenAIAuthResult, error) {
						return ResolveAnthropicAuth(context.Background(), r, nil, nil, nil, AnthropicResolveOptions{OAuth: flow, Clock: clock})
					}
				}
				credential, err := refresh(context.Background(), OAuthCredential{Refresh: "initial"})
				if err != nil {
					t.Fatal(err)
				}
				expires := now.Add(lifetime)
				refreshTime := expires.Add(-min(3*24*time.Hour, lifetime/2))
				if credential.Expires != expires.UnixMilli() || credential.RefreshAt != refreshTime.UnixMilli() {
					t.Fatalf("expiry/refresh schedule = %d/%d", credential.Expires, credential.RefreshAt)
				}
				path := filepath.Join(t.TempDir(), "auth.json")
				store, err := NewStore(Options{Path: path})
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SetOAuth(context.Background(), providerID, credential); err != nil {
					t.Fatal(err)
				}
				for _, step := range []struct {
					time time.Time
					want int32
				}{
					{now, 1},
					{refreshTime.Add(-time.Millisecond), 1},
					{refreshTime, 2},
					{refreshTime.Add(time.Second), 2},
					{refreshTime.Add(lifetime + time.Minute), 3},
				} {
					now = step.time
					// Each use opens a new store/runtime so an in-memory throttle
					// cannot hide repeated refreshes after restarting the process.
					store, err := NewStore(Options{Path: path})
					if err != nil {
						t.Fatal(err)
					}
					result, err := resolve(NewRuntime(store))
					if err != nil || result.APIKey != credential.Access || requests.Load() != step.want {
						t.Fatalf("at %s: error=%v, token present=%t, requests=%d want=%d", now, err, result.APIKey != "", requests.Load(), step.want)
					}
				}
			})
		}
	}
}

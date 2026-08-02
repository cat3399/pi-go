package auth

import (
	"context"
	"sync"
)

// Runtime overlays ephemeral API keys without ever persisting them.
type Runtime struct {
	store     *Store
	mu        sync.RWMutex
	overrides map[string]string
}

func NewRuntime(store *Store) *Runtime {
	return &Runtime{store: store, overrides: make(map[string]string)}
}
func (r *Runtime) SetAPIKey(provider, key string) error {
	if !validProviderID(provider) || !validAPIKey(key) {
		return failure(KindInvalid, "set runtime credential", provider, nil)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overrides[provider] = key
	return nil
}
func (r *Runtime) RemoveAPIKey(provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.overrides, provider)
}

func (r *Runtime) runtimeKey(provider string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	value, ok := r.overrides[provider]
	return value, ok
}
func (r *Runtime) Read(ctx context.Context, provider string) (Credential, bool, error) {
	r.mu.RLock()
	key, ok := r.overrides[provider]
	r.mu.RUnlock()
	if ok {
		return Credential{Type: "api_key", Key: key}, true, nil
	}
	return r.store.Read(ctx, provider)
}
func (r *Runtime) Delete(ctx context.Context, provider string) error {
	r.RemoveAPIKey(provider)
	return r.store.Delete(ctx, provider)
}

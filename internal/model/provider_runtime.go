package model

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/cat3399/pi-go/internal/provider"
)

// AuthCheck is the side-effect-free provider authentication status exposed by
// Models. A nil *AuthCheck means that the provider is known but unconfigured.
type AuthCheck struct {
	Source string
	Type   string
}

// AuthOverrides are explicit request-scoped values. They have precedence over
// stored, configured, and ambient credentials, matching pi-ai Models.getAuth.
type AuthOverrides struct {
	APIKey string
	Env    map[string]string
}

// AuthResult is the provider-neutral request projection produced by auth.
// BaseURL supports auth flows whose gateway endpoint is credential-specific.
type AuthResult struct {
	APIKey  string
	Headers map[string]string
	Env     map[string]string
	BaseURL string
	Source  string
	Type    string
}

// ProviderCredential is the provider-facing stored/effective credential shape
// used by credential-specific model filters and dynamic refreshers. Extra keeps
// OAuth/provider metadata such as GitHub Copilot's availableModelIds without
// exposing it to Agent or API adapters.
type ProviderCredential struct {
	Type      string
	Key       string
	Env       map[string]string
	Access    string
	Refresh   string
	Expires   int64
	AccountID string
	Extra     map[string]json.RawMessage
}

// ProviderAuthResolver is the auth half of a composed Provider. Implementors
// receive the immutable provider config instead of reaching back into Runtime,
// which keeps construction acyclic while Runtime retains operational ownership.
type ProviderAuthResolver interface {
	Check(context.Context, ProviderConfig) (*AuthCheck, error)
	Resolve(context.Context, ProviderConfig, provider.Model, AuthOverrides) (*AuthResult, error)
}

// ProviderModelAuthChecker is optional. It preserves compatibility providers
// whose configured model headers carry auth even when no provider API key is
// present; ordinary providers only need ProviderAuthResolver.Check.
type ProviderModelAuthChecker interface {
	CheckModel(context.Context, ProviderConfig, Model) (*AuthCheck, error)
}

// ProviderCredentialReader is optional. Models uses it to pass the same raw
// stored credential that pi-ai supplies to filterModels. Resolved request auth
// remains separate so filters cannot accidentally change dispatch credentials.
type ProviderCredentialReader interface {
	ReadCredential(context.Context, ProviderConfig) (*ProviderCredential, error)
}

// RuntimeCredentialMutator is implemented by auth resolvers backed by mutable
// runtime credentials. Runtime exposes these operations so product assembly no
// longer mutates a second credential owner beside Models.
type RuntimeCredentialMutator interface {
	SetRuntimeAPIKey(providerID, apiKey string) error
	RemoveRuntimeAPIKey(providerID string)
	Logout(context.Context, string) error
}

type RefreshModelsContext struct {
	Provider     ProviderConfig
	Credential   *ProviderCredential
	Auth         *AuthResult
	Stored       *CachedCatalog
	AllowNetwork bool
	Force        bool
}

// RefreshModelsFunc is the dynamic-provider seam corresponding to
// Provider.refreshModels. Static providers omit it. The returned catalog is
// validated and persisted by Runtime before it becomes visible.
type RefreshModelsFunc func(context.Context, RefreshModelsContext) (CachedCatalog, error)

// ProviderModelFilter applies credential-specific availability policy after
// authentication. It must not mutate its input.
type ProviderModelFilter func([]Model, *ProviderCredential) []Model

type ModelsRefreshOptions struct {
	// nil means true, matching pi-ai's refresh default.
	AllowNetwork *bool
	Force        bool
}

type ModelsRefreshResult struct {
	Aborted bool
	Errors  map[string]error
}

// Provider is the concrete runtime unit: metadata, auth, current models,
// refresh/filter policy, and streaming are owned together. Go uses one typed
// Request stream method for both pi-ai stream and streamSimple call shapes.
type Provider interface {
	ID() string
	Name() string
	BaseURL() string
	Headers() map[string]string
	GetModels() []Model
	CheckAuth(context.Context) (*AuthCheck, error)
	GetAuth(context.Context, provider.Model, AuthOverrides) (*AuthResult, error)
	RefreshModels(context.Context, ModelsRefreshOptions) error
	FilterModels([]Model, *ProviderCredential) []Model
	SupportsModel(provider.Model) bool
	Stream(context.Context, provider.Request) provider.EventStream
}

// Models is the collection contract mirrored from pi-ai. Runtime implements it
// and is also the narrow provider.Streamer injected into AgentSession.
type Models interface {
	GetProviders() []Provider
	GetProvider(string) (Provider, bool)
	GetModels(...string) []Model
	GetModel(string, string) (Model, bool)
	Refresh(context.Context, ModelsRefreshOptions) ModelsRefreshResult
	CheckAuth(context.Context, string) (*AuthCheck, error)
	GetAvailable(context.Context, ...string) ([]Model, error)
	GetProviderAuth(context.Context, string, AuthOverrides) (*AuthResult, error)
	GetAuth(context.Context, provider.Model, AuthOverrides) (*AuthResult, error)
	provider.Streamer
}

type runtimeProvider struct {
	runtime *Runtime
	id      string
}

type providerRefreshCall struct {
	done chan struct{}
	err  error
}

func (r *Runtime) GetProviders() []Provider {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	ids := append([]string(nil), r.snapshot.Providers...)
	registered := make(map[string]*runtimeProvider, len(r.registered))
	for id, entry := range r.registered {
		registered[id] = entry
	}
	r.mu.RUnlock()
	result := make([]Provider, 0, len(ids))
	for _, id := range ids {
		if entry := registered[id]; entry != nil {
			result = append(result, entry)
		}
	}
	return result
}

func (r *Runtime) GetProvider(id string) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	entry, ok := r.registered[id]
	r.mu.RUnlock()
	return entry, ok
}

// GetModels is a best-effort synchronous read. With no provider id it returns
// the complete snapshot; with one it uses exact provider identity.
func (r *Runtime) GetModels(providerID ...string) []Model {
	if r == nil || len(providerID) > 1 {
		return nil
	}
	snapshot := r.Snapshot()
	if len(providerID) == 0 {
		return snapshot.Models
	}
	result := make([]Model, 0)
	for _, candidate := range snapshot.Models {
		if candidate.Provider == providerID[0] {
			result = append(result, cloneModel(candidate))
		}
	}
	return result
}

func (r *Runtime) GetModel(providerID, modelID string) (Model, bool) {
	for _, candidate := range r.GetModels(providerID) {
		if candidate.ID == modelID {
			return cloneModel(candidate), true
		}
	}
	return Model{}, false
}

func (r *Runtime) CheckAuth(ctx context.Context, providerID string) (*AuthCheck, error) {
	entry, ok := r.GetProvider(providerID)
	if !ok {
		return nil, nil
	}
	return entry.CheckAuth(ctx)
}

func (r *Runtime) GetAuth(ctx context.Context, selected provider.Model, overrides AuthOverrides) (*AuthResult, error) {
	entry, ok := r.GetProvider(selected.Provider())
	if !ok {
		return nil, nil
	}
	return entry.GetAuth(ctx, selected, overrides)
}

// GetProviderAuth mirrors pi-ai's provider-id getAuth overload. A zero model is
// intentional here: provider-scoped auth is sufficient for dynamic catalog
// refreshes that begin with an empty model list.
func (r *Runtime) GetProviderAuth(ctx context.Context, providerID string, overrides AuthOverrides) (*AuthResult, error) {
	entry, ok := r.GetProvider(providerID)
	if !ok {
		return nil, nil
	}
	return entry.GetAuth(ctx, provider.Model{}, overrides)
}

func (r *Runtime) GetAvailable(ctx context.Context, providerID ...string) ([]Model, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidConfig)
	}
	if len(providerID) > 1 {
		return nil, fmt.Errorf("%w: at most one provider may be selected", ErrInvalidConfig)
	}
	providers := r.GetProviders()
	result := make([]Model, 0)
	for _, entry := range providers {
		if len(providerID) == 1 && entry.ID() != providerID[0] {
			continue
		}
		check, err := entry.CheckAuth(ctx)
		if err != nil {
			return nil, err
		}
		models := entry.GetModels()
		var credential *ProviderCredential
		if reader, ok := r.auth.(ProviderCredentialReader); ok {
			config, exists := r.Provider(entry.ID())
			if !exists {
				continue
			}
			credential, err = reader.ReadCredential(ctx, config)
			if err != nil {
				return nil, err
			}
		}
		if check == nil {
			// Provider-level auth makes every model eligible; only fall back to
			// per-model header checks when the provider itself is unconfigured.
			if checker, ok := r.auth.(ProviderModelAuthChecker); ok {
				config, exists := r.Provider(entry.ID())
				if !exists {
					continue
				}
				available := make([]Model, 0, len(models))
				for _, candidate := range models {
					modelCheck, checkErr := checker.CheckModel(ctx, config, candidate)
					if checkErr != nil {
						return nil, checkErr
					}
					if modelCheck != nil {
						available = append(available, cloneModel(candidate))
					}
				}
				models = available
			} else {
				continue
			}
		}
		result = append(result, entry.FilterModels(models, credential)...)
	}
	return result, nil
}

// Availability adapts the context-aware Models API to the existing synchronous
// selection helpers. It samples live auth and route state on every call.
func (r *Runtime) Availability() Availability {
	return Availability{
		HasConfiguredAuth: func(providerID string) bool {
			check, err := r.CheckAuth(context.Background(), providerID)
			return err == nil && check != nil
		},
		HasConfiguredModelAuth: func(candidate Model) bool {
			ctx := context.Background()
			check, err := r.CheckAuth(ctx, candidate.Provider)
			if err != nil {
				return false
			}
			if check == nil {
				checker, ok := r.auth.(ProviderModelAuthChecker)
				if !ok {
					return false
				}
				config, exists := r.Provider(candidate.Provider)
				if !exists {
					return false
				}
				check, err = checker.CheckModel(ctx, config, candidate)
				if err != nil || check == nil {
					return false
				}
			}
			r.mu.RLock()
			filter := r.filters[candidate.Provider]
			r.mu.RUnlock()
			if filter == nil {
				return true
			}
			entry, exists := r.GetProvider(candidate.Provider)
			if !exists {
				return false
			}
			var credential *ProviderCredential
			if reader, ok := r.auth.(ProviderCredentialReader); ok {
				config, configured := r.Provider(candidate.Provider)
				if !configured {
					return false
				}
				credential, err = reader.ReadCredential(ctx, config)
				if err != nil {
					return false
				}
			}
			for _, allowed := range entry.FilterModels(entry.GetModels(), credential) {
				if allowed.Provider == candidate.Provider && allowed.ID == candidate.ID {
					return true
				}
			}
			return false
		},
		SupportsRoute: func(candidate Model) bool { return r.ValidateRoute(candidate) == nil },
	}
}

func (r *Runtime) SetRuntimeAPIKey(providerID, apiKey string) error {
	mutator, ok := r.auth.(RuntimeCredentialMutator)
	if !ok {
		return fmt.Errorf("%w: runtime credentials are not mutable", ErrUnsupported)
	}
	return mutator.SetRuntimeAPIKey(providerID, apiKey)
}

func (r *Runtime) RemoveRuntimeAPIKey(providerID string) error {
	mutator, ok := r.auth.(RuntimeCredentialMutator)
	if !ok {
		return fmt.Errorf("%w: runtime credentials are not mutable", ErrUnsupported)
	}
	mutator.RemoveRuntimeAPIKey(providerID)
	return nil
}

func (r *Runtime) Logout(ctx context.Context, providerID string) error {
	mutator, ok := r.auth.(RuntimeCredentialMutator)
	if !ok {
		return fmt.Errorf("%w: provider logout is not supported", ErrUnsupported)
	}
	return mutator.Logout(ctx, providerID)
}

func (r *Runtime) SupportsModel(selected provider.Model) bool {
	if r == nil {
		return false
	}
	entry, ok := r.GetProvider(selected.Provider())
	return ok && entry.SupportsModel(selected)
}

func (r *Runtime) Stream(ctx context.Context, request provider.Request) provider.EventStream {
	if r == nil {
		return provider.FailureStream(fmt.Errorf("%w: nil models runtime", ErrUnsupported))
	}
	entry, ok := r.GetProvider(request.Model().Provider())
	if !ok {
		return provider.FailureStream(fmt.Errorf("%w: unknown provider %q", ErrUnsupported, request.Model().Provider()))
	}
	return entry.Stream(ctx, request)
}

func (r *Runtime) Refresh(ctx context.Context, options ModelsRefreshOptions) ModelsRefreshResult {
	result := ModelsRefreshResult{Errors: make(map[string]error)}
	if ctx == nil {
		result.Errors[""] = fmt.Errorf("%w: nil context", ErrInvalidConfig)
		return result
	}
	ids := make([]string, 0, len(r.refreshers))
	r.mu.RLock()
	for id := range r.refreshers {
		ids = append(ids, id)
	}
	r.mu.RUnlock()
	sort.Strings(ids)
	var mu sync.Mutex
	var wait sync.WaitGroup
	for _, id := range ids {
		if context.Cause(ctx) != nil {
			break
		}
		id := id
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := r.refreshProvider(ctx, id, options); err != nil && context.Cause(ctx) == nil {
				mu.Lock()
				result.Errors[id] = err
				mu.Unlock()
			}
		}()
	}
	wait.Wait()
	result.Aborted = context.Cause(ctx) != nil
	return result
}

func (r *Runtime) refreshProvider(ctx context.Context, providerID string, options ModelsRefreshOptions) error {
	r.refreshMu.Lock()
	if active := r.refreshing[providerID]; active != nil {
		r.refreshMu.Unlock()
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-active.done:
			return active.err
		}
	}
	call := &providerRefreshCall{done: make(chan struct{})}
	r.refreshing[providerID] = call
	r.refreshMu.Unlock()

	call.err = r.runProviderRefresh(ctx, providerID, options)
	r.refreshMu.Lock()
	delete(r.refreshing, providerID)
	close(call.done)
	r.refreshMu.Unlock()
	return call.err
}

func (r *Runtime) runProviderRefresh(ctx context.Context, providerID string, options ModelsRefreshOptions) error {
	_, ok := r.GetProvider(providerID)
	if !ok {
		return fmt.Errorf("%w: unknown provider %q", ErrNotFound, providerID)
	}
	r.mu.RLock()
	refresh := r.refreshers[providerID]
	r.mu.RUnlock()
	if refresh == nil {
		return nil
	}
	store, err := NewStore(r.options.ModelsStorePath)
	if err != nil {
		return err
	}
	stored, storedOK, err := store.Read(ctx, providerID)
	if err != nil {
		return err
	}
	allowNetwork := options.AllowNetwork == nil || *options.AllowNetwork
	if !allowNetwork {
		if storedOK {
			return r.Reload(ctx)
		}
		return nil
	}
	authResult, err := r.GetProviderAuth(ctx, providerID, AuthOverrides{})
	if err != nil {
		return err
	}
	if authResult == nil {
		return nil
	}
	credential := &ProviderCredential{Type: authResult.Type, Key: authResult.APIKey, Env: cloneHeaders(authResult.Env)}
	if reader, ok := r.auth.(ProviderCredentialReader); ok {
		config, exists := r.Provider(providerID)
		if exists {
			stored, readErr := reader.ReadCredential(ctx, config)
			if readErr != nil {
				return readErr
			}
			if stored != nil && stored.Type == "oauth" {
				credential = cloneProviderCredential(stored)
			}
		}
	}
	config, _ := r.Provider(providerID)
	input := RefreshModelsContext{Provider: config, Credential: credential, Auth: authResult, AllowNetwork: true, Force: options.Force}
	if storedOK {
		copy := stored
		input.Stored = &copy
	}
	refreshed, err := refresh(ctx, input)
	if err != nil {
		return err
	}
	if context.Cause(ctx) != nil {
		return context.Cause(ctx)
	}
	if refreshed.CheckedAt == nil {
		now := time.Now().UnixMilli()
		refreshed.CheckedAt = &now
	}
	if err := store.Write(ctx, providerID, refreshed); err != nil {
		return err
	}
	return r.Reload(ctx)
}

func (p *runtimeProvider) ID() string { return p.id }

func (p *runtimeProvider) config() (ProviderConfig, bool) {
	if p == nil || p.runtime == nil {
		return ProviderConfig{}, false
	}
	return p.runtime.Provider(p.id)
}

func (p *runtimeProvider) Name() string {
	config, ok := p.config()
	if !ok || config.Name == "" {
		return p.id
	}
	return config.Name
}

func (p *runtimeProvider) BaseURL() string {
	config, _ := p.config()
	return config.BaseURL
}

func (p *runtimeProvider) Headers() map[string]string {
	config, _ := p.config()
	return cloneHeaders(config.Headers)
}

func (p *runtimeProvider) GetModels() []Model { return p.runtime.GetModels(p.id) }

func (p *runtimeProvider) CheckAuth(ctx context.Context) (*AuthCheck, error) {
	config, ok := p.config()
	if !ok || p.runtime.auth == nil {
		return nil, nil
	}
	return p.runtime.auth.Check(ctx, config)
}

func (p *runtimeProvider) GetAuth(ctx context.Context, selected provider.Model, overrides AuthOverrides) (*AuthResult, error) {
	config, ok := p.config()
	if !ok {
		return nil, nil
	}
	if selected.Provider() != "" && selected.Provider() != p.id {
		return nil, fmt.Errorf("%w: model belongs to provider %q", ErrUnsupported, selected.Provider())
	}
	if p.runtime.auth == nil {
		if overrides.APIKey == "" {
			return nil, nil
		}
		return &AuthResult{APIKey: overrides.APIKey, Env: cloneHeaders(overrides.Env), Source: "request override", Type: "api_key"}, nil
	}
	return p.runtime.auth.Resolve(ctx, config, selected, AuthOverrides{APIKey: overrides.APIKey, Env: cloneHeaders(overrides.Env)})
}

func (p *runtimeProvider) RefreshModels(ctx context.Context, options ModelsRefreshOptions) error {
	if p == nil || p.runtime == nil {
		return fmt.Errorf("%w: nil provider", ErrUnsupported)
	}
	return p.runtime.refreshProvider(ctx, p.id, options)
}

func (p *runtimeProvider) FilterModels(models []Model, credential *ProviderCredential) []Model {
	p.runtime.mu.RLock()
	filter := p.runtime.filters[p.id]
	p.runtime.mu.RUnlock()
	if filter == nil {
		result := make([]Model, len(models))
		for index := range models {
			result[index] = cloneModel(models[index])
		}
		return result
	}
	filtered := filter(cloneModels(models), cloneProviderCredential(credential))
	return cloneModels(filtered)
}

func (p *runtimeProvider) SupportsModel(selected provider.Model) bool {
	if p == nil || p.runtime == nil || selected.Provider() != p.id {
		return false
	}
	p.runtime.mu.RLock()
	adapter := p.runtime.adapters[selected.API()]
	storeErr := p.runtime.storeErrors[p.id]
	config := p.runtime.providers[p.id]
	p.runtime.mu.RUnlock()
	if storeErr != nil || len(config.UnsupportedFields) != 0 || adapter == nil {
		return false
	}
	if validator, ok := adapter.(provider.RouteValidator); ok {
		return validator.SupportsModel(selected)
	}
	return true
}

func (p *runtimeProvider) Stream(ctx context.Context, request provider.Request) provider.EventStream {
	if p == nil || p.runtime == nil {
		return provider.FailureStream(fmt.Errorf("%w: nil provider", ErrUnsupported))
	}
	return provider.LazyStream(func() provider.EventStream {
		return p.prepareStream(ctx, request)
	})
}

func (p *runtimeProvider) prepareStream(ctx context.Context, request provider.Request) provider.EventStream {
	selected := request.Model()
	if selected.Provider() != p.id {
		return provider.FailureStream(fmt.Errorf("%w: provider %q cannot stream model owned by %q", ErrUnsupported, p.id, selected.Provider()))
	}
	p.runtime.mu.RLock()
	adapter := p.runtime.adapters[selected.API()]
	storeErr := p.runtime.storeErrors[p.id]
	config := cloneProvider(p.runtime.providers[p.id])
	p.runtime.mu.RUnlock()
	if storeErr != nil {
		return provider.FailureStream(fmt.Errorf("%w: selected provider has an invalid cached catalog", ErrUnsupported))
	}
	if len(config.UnsupportedFields) != 0 {
		return provider.FailureStream(fmt.Errorf("%w: selected provider contains unsupported configuration fields", ErrUnsupported))
	}
	if adapter == nil {
		return provider.FailureStream(fmt.Errorf("%w: provider %s has no API implementation for %q", ErrUnsupported, p.id, selected.API()))
	}
	if validator, ok := adapter.(provider.RouteValidator); ok && !validator.SupportsModel(selected) {
		return provider.FailureStream(fmt.Errorf("%w: adapter does not support model %s/%s/%s", ErrUnsupported, selected.Provider(), selected.API(), selected.ID()))
	}
	stream := request.StreamOptions()
	resolution, err := p.GetAuth(ctx, selected, AuthOverrides{APIKey: stream.APIKey, Env: stream.Env})
	if err != nil {
		return provider.FailureStream(err)
	}
	if resolution == nil {
		return provider.FailureStream(fmt.Errorf("%w: provider is not configured: %s", ErrUnavailable, p.id))
	}
	if stream.APIKey == "" {
		stream.APIKey = resolution.APIKey
	}
	stream.Headers = mergeHeaders(resolution.Headers, stream.Headers)
	stream.Env = mergeStringMaps(resolution.Env, stream.Env)
	if resolution.BaseURL != "" {
		selected, err = selected.WithBaseURL(resolution.BaseURL)
		if err != nil {
			return provider.FailureStream(err)
		}
	}
	prepared, err := request.WithModelAndStream(selected, stream)
	if err != nil {
		return provider.FailureStream(err)
	}
	return adapter.Stream(ctx, prepared)
}

func rebuildRuntimeProviders(snapshot Snapshot, runtime *Runtime) map[string]*runtimeProvider {
	result := make(map[string]*runtimeProvider, len(snapshot.Providers))
	for _, id := range snapshot.Providers {
		result[id] = &runtimeProvider{runtime: runtime, id: id}
	}
	return result
}

func cloneModels(models []Model) []Model {
	result := make([]Model, len(models))
	for index := range models {
		result[index] = cloneModel(models[index])
	}
	return result
}

func cloneProviderCredential(value *ProviderCredential) *ProviderCredential {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Env = cloneHeaders(value.Env)
	if value.Extra != nil {
		copy.Extra = make(map[string]json.RawMessage, len(value.Extra))
		for key, raw := range value.Extra {
			copy.Extra[key] = append(json.RawMessage(nil), raw...)
		}
	}
	return &copy
}

func mergeStringMaps(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	result := make(map[string]string, len(base)+len(override))
	for key, value := range base {
		result[key] = value
	}
	for key, value := range override {
		result[key] = value
	}
	return result
}

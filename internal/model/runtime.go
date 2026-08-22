// Package model owns the process-local provider registry, model catalog,
// credential resolution, availability checks, dynamic catalog refresh, and
// API-dialect stream dispatch used by product assembly. Callers that only need
// catalog projection may omit the optional auth, adapter, and refresh services.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/provider"
)

const (
	OpenAIProviderID          = "openai"
	AzureOpenAIProviderID     = "azure-openai-responses"
	OpenAICodexProviderID     = "openai-codex"
	AnthropicProviderID       = "anthropic"
	OpenAIResponsesAPI        = "openai-responses"
	AzureOpenAIResponsesAPI   = "azure-openai-responses"
	OpenAICompletionsAPI      = "openai-completions"
	OpenAICodexResponsesAPI   = "openai-codex-responses"
	AnthropicMessagesAPI      = "anthropic-messages"
	DefaultOpenAIModel        = "gpt-5.5"
	DefaultAzureOpenAIModel   = "gpt-5.4"
	DefaultOpenAICodexModel   = "gpt-5.5"
	DefaultAnthropicModel     = "claude-opus-4-8"
	defaultOpenAIBaseURL      = "https://api.openai.com/v1"
	defaultOpenAICodexBaseURL = "https://chatgpt.com/backend-api"
	defaultAnthropicBaseURL   = "https://api.anthropic.com"
	maxFileBytes              = 4 << 20
)

var (
	ErrInvalidConfig = errors.New("invalid model or settings configuration")
	ErrNotFound      = errors.New("model not found")
	ErrUnavailable   = errors.New("model is unavailable")
	ErrUnsupported   = errors.New("unsupported model configuration")
	ErrCancelled     = errors.New("model runtime operation cancelled")
	ErrPersistence   = errors.New("persistent model settings are unavailable")
	ErrCommitUnknown = errors.New("configuration publication outcome is unknown")
)

// Diagnostic is intentionally value-only.  It never contains a configuration
// value, which makes it safe for errors emitted while parsing apiKey/header data.
type Diagnostic struct{ Source, Path, Message string }

func (d Diagnostic) Error() string { return fmt.Sprintf("%s %s: %s", d.Source, d.Path, d.Message) }

type Model struct {
	Provider         string                             `json:"provider"`
	ID               string                             `json:"id"`
	Name             string                             `json:"name"`
	API              string                             `json:"api"`
	BaseURL          string                             `json:"baseUrl"`
	Headers          map[string]string                  `json:"headers,omitempty"`
	Reasoning        bool                               `json:"reasoning"`
	ThinkingLevelMap map[provider.ThinkingLevel]*string `json:"thinkingLevelMap,omitempty"`
	Input            []provider.InputKind               `json:"input,omitempty"`
	Cost             provider.CostRates                 `json:"cost"`
	ContextWindow    uint64                             `json:"contextWindow"`
	MaxTokens        uint64                             `json:"maxTokens"`
	Compat           provider.ModelCompat               `json:"-"`
	// UnsupportedFields and UnknownFields are parser diagnostics attached to a
	// runtime projection. They are not model metadata and are intentionally not
	// part of CachedCatalog's durable model contract.
	UnsupportedFields []string `json:"-"`
	UnknownFields     []string `json:"-"`
	compatRaw         json.RawMessage
}

func (m Model) Ref() (provider.Model, error) {
	return provider.NewModel(provider.ModelSpec{
		Provider: m.Provider, API: m.API, ID: m.ID, Name: m.Name, BaseURL: m.BaseURL,
		Headers: m.Headers, Reasoning: m.Reasoning, ThinkingLevelMap: m.ThinkingLevelMap, Input: m.Input,
		Cost: m.Cost, ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens, Compat: m.Compat,
	})
}

type ProviderConfig struct {
	ID, Name, API, BaseURL string
	Headers                map[string]string
	Compat                 provider.ModelCompat
	AuthHeader             *bool
	// ConfiguredAPIKey is returned only to the in-process assembly that passes it
	// to auth. It is never put in diagnostics or persisted by this package.
	ConfiguredAPIKey *string
	// APIKeyEnvironment and Keyless are builtin Provider auth metadata. They
	// are not models.json fields; provider factories supply them alongside the
	// builtin catalog and request adapter.
	APIKeyEnvironment []string
	Keyless           bool
	Models            []Model
	UnknownFields     []string
	UnsupportedFields []string
	overrides         map[string]modelOverride
	compatRaw         json.RawMessage
	compatPresent     bool
	oauthRadius       bool
}

// modelOverride keeps presence separate from a zero value so config overlays do
// not accidentally erase builtin metadata.
type modelOverride struct {
	Name              *string
	Reasoning         *bool
	ThinkingLevelMap  map[provider.ThinkingLevel]*string
	Input             *[]provider.InputKind
	Cost              *modelCostOverride
	ContextWindow     *uint64
	MaxTokens         *uint64
	Headers           map[string]string
	Compat            provider.ModelCompat
	CompatPresent     bool
	UnsupportedFields []string
	UnknownFields     []string
	compatRaw         json.RawMessage
}

type modelCostOverride struct {
	Input, Output, CacheRead, CacheWrite *float64
	Tiers                                *[]provider.CostTier
}

type Settings struct {
	DefaultProvider           string
	DefaultModel              string
	DefaultThinkingLevel      provider.ThinkingLevel
	Theme                     string
	Transport                 provider.Transport
	SteeringMode              string
	FollowUpMode              string
	ShellPath                 string
	ShellCommandPrefix        string
	Images                    ImageSettings
	Skills                    []string
	Prompts                   []string
	ThinkingBudgets           ThinkingBudgetSettings
	EnabledModels             []string
	Compaction                CompactionSettings
	BranchSummary             BranchSummarySettings
	Retry                     RetrySettings
	HTTPIdleTimeoutMS         *uint64
	WebsocketConnectTimeoutMS *uint64
	transportPresent          bool
	imagesPresence            settingsObjectPresence
	skillsPresence            settingsObjectPresence
	promptsPresence           settingsObjectPresence
	shellPathPresent          bool
	shellCommandPrefixPresent bool
}

// ImageSettings mirrors settings.images. Pointer optionality preserves the
// upstream default and project-over-global field merge behavior.
type ImageSettings struct {
	AutoResize  *bool `json:"autoResize,omitempty"`
	BlockImages *bool `json:"blockImages,omitempty"`
}

func (s ImageSettings) AutoResizeOrDefault() bool {
	return s.AutoResize == nil || *s.AutoResize
}

func (s ImageSettings) BlockImagesOrDefault() bool {
	return s.BlockImages != nil && *s.BlockImages
}

// ThinkingBudgetSettings mirrors settings.thinkingBudgets. Pointer fields
// preserve explicit zero budgets and the original field-level global/project
// merge behavior.
type ThinkingBudgetSettings struct {
	Minimal  *uint64 `json:"minimal,omitempty"`
	Low      *uint64 `json:"low,omitempty"`
	Medium   *uint64 `json:"medium,omitempty"`
	High     *uint64 `json:"high,omitempty"`
	presence settingsObjectPresence
}

func (s ThinkingBudgetSettings) ProviderBudgets() map[provider.ThinkingLevel]uint64 {
	values := make(map[provider.ThinkingLevel]uint64, 4)
	for level, value := range map[provider.ThinkingLevel]*uint64{
		provider.ThinkingMinimal: s.Minimal,
		provider.ThinkingLow:     s.Low,
		provider.ThinkingMedium:  s.Medium,
		provider.ThinkingHigh:    s.High,
	} {
		if value != nil {
			values[level] = *value
		}
	}
	if len(values) == 0 {
		return nil
	}
	return values
}

func (s Settings) TransportOrDefault() provider.Transport {
	if s.Transport == "" {
		return provider.TransportAuto
	}
	return s.Transport
}

const (
	QueueModeAll        = "all"
	QueueModeOneAtATime = "one-at-a-time"
)

func (s Settings) SteeringModeOrDefault() string {
	if s.SteeringMode == "" {
		return QueueModeOneAtATime
	}
	return s.SteeringMode
}

func (s Settings) FollowUpModeOrDefault() string {
	if s.FollowUpMode == "" {
		return QueueModeOneAtATime
	}
	return s.FollowUpMode
}

// BranchSummarySettings mirrors pi's settings.branchSummary. SkipPrompt is
// retained for the later interactive layer even though core navigateTree is
// explicitly directed by its caller.
type BranchSummarySettings struct {
	ReserveTokens *uint64 `json:"reserveTokens,omitempty"`
	SkipPrompt    *bool   `json:"skipPrompt,omitempty"`
}

func (s BranchSummarySettings) ReserveTokensOrDefault() uint64 {
	if s.ReserveTokens == nil {
		return 16_384
	}
	return *s.ReserveTokens
}

// CompactionSettings preserves optionality so project settings can override
// individual global fields and an explicit enabled:false is not lost.
type CompactionSettings struct {
	Enabled          *bool   `json:"enabled,omitempty"`
	ReserveTokens    *uint64 `json:"reserveTokens,omitempty"`
	KeepRecentTokens *uint64 `json:"keepRecentTokens,omitempty"`
}

func (s CompactionSettings) EnabledOrDefault() bool {
	return s.Enabled == nil || *s.Enabled
}

func (s CompactionSettings) ReserveTokensOrDefault() uint64 {
	if s.ReserveTokens == nil {
		return 16_384
	}
	return *s.ReserveTokens
}

func (s CompactionSettings) KeepRecentTokensOrDefault() uint64 {
	if s.KeepRecentTokens == nil {
		return 20_000
	}
	return *s.KeepRecentTokens
}

// RetrySettings mirrors settings.retry. Optional fields are retained so
// global/project overlays preserve explicit false and zero values.
type RetrySettings struct {
	Enabled          *bool                 `json:"enabled,omitempty"`
	MaxRetries       *uint64               `json:"maxRetries,omitempty"`
	BaseDelayMS      *uint64               `json:"baseDelayMs,omitempty"`
	Provider         ProviderRetrySettings `json:"provider,omitempty"`
	presence         settingsObjectPresence
	providerPresence settingsObjectPresence
}

type settingsObjectPresence uint8

const (
	settingsObjectAbsent settingsObjectPresence = iota
	settingsObjectPresent
	settingsObjectNull
)

type ProviderRetrySettings struct {
	TimeoutMS       *uint64 `json:"timeoutMs,omitempty"`
	MaxRetries      *uint64 `json:"maxRetries,omitempty"`
	MaxRetryDelayMS *uint64 `json:"maxRetryDelayMs,omitempty"`
}

func (s RetrySettings) EnabledOrDefault() bool {
	return s.Enabled == nil || *s.Enabled
}

func (s RetrySettings) MaxRetriesOrDefault() uint64 {
	if s.MaxRetries == nil {
		return 3
	}
	return *s.MaxRetries
}

func (s RetrySettings) BaseDelayMSOrDefault() uint64 {
	if s.BaseDelayMS == nil {
		return 2_000
	}
	return *s.BaseDelayMS
}

func (s Settings) HTTPIdleTimeoutMSOrDefault() uint64 {
	if s.HTTPIdleTimeoutMS == nil {
		return 300_000
	}
	return *s.HTTPIdleTimeoutMS
}

func (s ProviderRetrySettings) MaxRetryDelayMSOrDefault() uint64 {
	if s.MaxRetryDelayMS == nil {
		return 60_000
	}
	return *s.MaxRetryDelayMS
}

type Snapshot struct {
	Models     []Model
	Providers  []string
	Settings   Settings
	Generation uint64
}

type Options struct {
	// AgentDir may be absent for read-only startup. SetGlobalSettings requires
	// it to have been created by the application before mutation so one leaf
	// rename never masquerades as durable creation of missing ancestors.
	AgentDir   string
	WorkingDir string
	// ModelsStorePath is the optional, provider-scoped catalog cache. Registered
	// dynamic providers may refresh it through Runtime.Refresh.
	ModelsStorePath string
	// ProjectTrusted is deliberately opt-in. A project .pi/settings.json is not
	// read merely because it exists; a formal trust decision is deferred.
	ProjectTrusted bool
	// Adapters are API-dialect stream implementations. Runtime composes them
	// with provider metadata and auth instead of exposing a separate app-owned
	// router. A missing API remains a valid catalog entry but is unavailable for
	// selection and fails explicitly if streamed.
	Adapters map[string]provider.Streamer
	// AuthResolver owns provider credential checks and request-time resolution.
	// It is optional for credential-blind catalog consumers.
	AuthResolver ProviderAuthResolver
	// Refreshers and Filters are provider-scoped extension points used by
	// dynamic providers. Static providers need neither.
	Refreshers map[string]RefreshModelsFunc
	Filters    map[string]ProviderModelFilter
}

type Runtime struct {
	options     Options
	mu          sync.RWMutex
	local       chan struct{}
	snapshot    Snapshot
	providers   map[string]ProviderConfig
	registered  map[string]*runtimeProvider
	adapters    map[string]provider.Streamer
	auth        ProviderAuthResolver
	refreshers  map[string]RefreshModelsFunc
	filters     map[string]ProviderModelFilter
	storeErrors map[string]error
	configError error
	storeError  error
	// Settings layers retain their last successful projections. A malformed
	// settings file is a diagnostic source, not a reason to discard a healthy
	// Agent/runtime: this mirrors SettingsManager's per-layer recovery.
	globalSettings       Settings
	projectSettings      Settings
	globalSettingsError  error
	projectSettingsError error
	composeErrs          map[string]error
	faults               atomicWriteFaults
	refreshMu            sync.Mutex
	refreshing           map[string]*providerRefreshCall
}

func NewRuntime(options Options) (*Runtime, error) {
	if strings.TrimSpace(options.AgentDir) == "" {
		return nil, fmt.Errorf("%w: agent directory is required", ErrInvalidConfig)
	}
	if options.WorkingDir == "" {
		options.WorkingDir = "."
	}
	if options.ModelsStorePath == "" {
		options.ModelsStorePath = filepath.Join(options.AgentDir, "models-store.json")
	}
	adapters := make(map[string]provider.Streamer, len(options.Adapters))
	for api, adapter := range options.Adapters {
		if strings.TrimSpace(api) == "" || adapter == nil {
			return nil, fmt.Errorf("%w: invalid provider adapter registration", ErrInvalidConfig)
		}
		adapters[api] = adapter
	}
	refreshers := make(map[string]RefreshModelsFunc, len(options.Refreshers))
	for providerID, refresh := range options.Refreshers {
		if !validID(providerID) || refresh == nil {
			return nil, fmt.Errorf("%w: invalid provider model refresher", ErrInvalidConfig)
		}
		refreshers[providerID] = refresh
	}
	filters := make(map[string]ProviderModelFilter, len(options.Filters))
	for providerID, filter := range options.Filters {
		if !validID(providerID) || filter == nil {
			return nil, fmt.Errorf("%w: invalid provider model filter", ErrInvalidConfig)
		}
		filters[providerID] = filter
	}
	r := &Runtime{
		options: options, local: newLocalGate(), storeErrors: make(map[string]error),
		adapters: adapters, auth: options.AuthResolver, refreshers: refreshers,
		filters: filters, refreshing: make(map[string]*providerRefreshCall),
	}
	if err := r.Reload(context.Background()); err != nil {
		return nil, err
	}
	return r, nil
}

// LoadEffectiveSettings reads only the settings layers used by production.
// Resource-management surfaces use this narrower boundary so an unrelated
// models.json/provider error cannot prevent skills and prompts from loading.
func LoadEffectiveSettings(agentDir, workingDir string, projectTrusted bool) (Settings, error) {
	if strings.TrimSpace(agentDir) == "" {
		return Settings{}, fmt.Errorf("%w: agent directory is required", ErrInvalidConfig)
	}
	if workingDir == "" {
		workingDir = "."
	}
	global, err := loadSettings(filepath.Join(agentDir, "settings.json"), "global settings.json")
	if err != nil {
		return Settings{}, err
	}
	if !projectTrusted {
		return cloneSettings(global), nil
	}
	project, err := loadSettings(filepath.Join(workingDir, ".pi", "settings.json"), "project settings.json")
	if err != nil {
		return Settings{}, err
	}
	return cloneSettings(mergeSettings(global, project)), nil
}

func (r *Runtime) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneSnapshot(r.snapshot)
}

// Error returns non-fatal model/settings-source diagnostics from the latest
// reload. Settings retain their last healthy layer; invalid model sources use
// the built-in fallback catalog. Neither prevents Runtime construction.
func (r *Runtime) Error() error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return errors.Join(r.settingsErrorLocked(), r.modelSourceErrorLocked())
}

// SettingsError reports non-fatal settings-layer diagnostics separately from
// model/catalog sources. Selected-route validation remains a separate strict
// boundary.
func (r *Runtime) SettingsError() error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.settingsErrorLocked()
}

// ModelSourceError reports the non-settings model/catalog diagnostics used by
// the selected-route assembly boundary.
func (r *Runtime) ModelSourceError() error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.modelSourceErrorLocked()
}

func (r *Runtime) settingsErrorLocked() error {
	return errors.Join(r.globalSettingsError, r.projectSettingsError)
}

func (r *Runtime) modelSourceErrorLocked() error {
	values := []error{r.configError, r.storeError}
	ids := make([]string, 0, len(r.composeErrs))
	for id := range r.composeErrs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		values = append(values, r.composeErrs[id])
	}
	return errors.Join(values...)
}
func (r *Runtime) Provider(id string) (ProviderConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[id]
	return cloneProvider(p), ok
}

// ValidateRoute is the selected-route consumption boundary. Like pi's TypeBox
// models.json schema, unknown extra object members are preserved by the source
// file but ignored by composition. Only recognized, unsupported features and
// an API without a registered Go adapter make an otherwise valid route fail.
func (r *Runtime) ValidateRoute(selected Model) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	providerID := selected.Provider
	if err := r.storeErrors[providerID]; err != nil {
		return fmt.Errorf("%w: selected provider has an invalid cached catalog", ErrUnsupported)
	}
	if configured, ok := r.providers[providerID]; ok {
		if len(configured.UnsupportedFields) != 0 {
			return fmt.Errorf("%w: selected provider contains unsupported configuration fields", ErrUnsupported)
		}
	}
	if len(selected.UnsupportedFields) != 0 {
		return fmt.Errorf("%w: selected model contains unsupported configuration fields", ErrUnsupported)
	}
	if len(r.adapters) != 0 {
		adapter := r.adapters[selected.API]
		if adapter == nil {
			return fmt.Errorf("%w: selected model uses an unimplemented API", ErrUnsupported)
		}
		ref, err := selected.Ref()
		if err != nil {
			return fmt.Errorf("%w: selected model is invalid", ErrUnsupported)
		}
		if validator, ok := adapter.(provider.RouteValidator); ok && !validator.SupportsModel(ref) {
			return fmt.Errorf("%w: selected model is rejected by its API adapter", ErrUnsupported)
		}
		return nil
	}
	switch selected.API {
	case OpenAIResponsesAPI, AzureOpenAIResponsesAPI, OpenAICompletionsAPI, OpenAICodexResponsesAPI, AnthropicMessagesAPI:
	default:
		return fmt.Errorf("%w: selected model uses an unimplemented API", ErrUnsupported)
	}
	return nil
}

// Reload publishes source projections transactionally. A malformed settings
// layer retains its previous successful layer (or an empty initial layer) and
// is recorded as a non-fatal diagnostic. Invalid models.json or
// models-store.json likewise publish their healthy fallback. A missing
// optional file is a healthy empty source.
func (r *Runtime) Reload(ctx context.Context) error {
	releaseLocal, err := acquireLocal(ctx, r.local)
	if err != nil {
		return err
	}
	defer releaseLocal()
	r.mu.RLock()
	globalSettings := cloneSettings(r.globalSettings)
	projectSettings := cloneSettings(r.projectSettings)
	r.mu.RUnlock()
	loadedGlobal, globalSettingsError := loadSettings(filepath.Join(r.options.AgentDir, "settings.json"), "global settings.json")
	if globalSettingsError == nil {
		globalSettings = loadedGlobal
	}
	var projectSettingsError error
	if r.options.ProjectTrusted {
		loadedProject, err := loadSettings(filepath.Join(r.options.WorkingDir, ".pi", "settings.json"), "project settings.json")
		if err != nil {
			projectSettingsError = err
		} else {
			projectSettings = loadedProject
		}
	} else {
		projectSettings = Settings{}
	}
	settings := mergeSettings(globalSettings, projectSettings)
	providers, configError := loadModels(filepath.Join(r.options.AgentDir, "models.json"))
	compositionErrors := map[string]error{}
	if configError != nil {
		providers = map[string]ProviderConfig{}
	} else {
		providers, compositionErrors = validateConfiguredProviders(providers)
	}
	providers = composeProviderConfigs(providers)
	cached, storeErrors, storeError := loadStoreCatalogs(r.options.ModelsStorePath)
	if storeError != nil {
		cached = map[string]CachedCatalog{}
		storeErrors = map[string]error{}
	}
	snapshot := buildSnapshot(providers, cached, settings)
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot.Generation = r.snapshot.Generation + 1
	r.providers = providers
	r.storeErrors = storeErrors
	r.configError = configError
	r.storeError = storeError
	r.globalSettings = cloneSettings(globalSettings)
	r.projectSettings = cloneSettings(projectSettings)
	r.globalSettingsError = globalSettingsError
	r.projectSettingsError = projectSettingsError
	r.composeErrs = compositionErrors
	r.snapshot = snapshot
	r.registered = rebuildRuntimeProviders(snapshot, r)
	return nil
}

func composeProviderConfigs(configured map[string]ProviderConfig) map[string]ProviderConfig {
	defaults := builtinProviderConfigs()
	result := make(map[string]ProviderConfig, len(configured)+len(defaults))
	for id, value := range configured {
		result[id] = cloneProvider(value)
	}
	for _, fallback := range defaults {
		current, exists := result[fallback.ID]
		if !exists {
			result[fallback.ID] = fallback
			continue
		}
		if current.Name == "" {
			current.Name = fallback.Name
		}
		if current.API == "" {
			current.API = fallback.API
		}
		if current.BaseURL == "" {
			current.BaseURL = fallback.BaseURL
		}
		current.APIKeyEnvironment = append([]string(nil), fallback.APIKeyEnvironment...)
		current.Keyless = fallback.Keyless
		result[fallback.ID] = current
	}
	return result
}

func validateConfiguredProviders(configured map[string]ProviderConfig) (map[string]ProviderConfig, map[string]error) {
	baselines := make(map[string][]Model)
	for _, candidate := range builtinModels() {
		baselines[candidate.Provider] = append(baselines[candidate.Provider], candidate)
	}
	valid := make(map[string]ProviderConfig, len(configured))
	invalid := make(map[string]error)
	ids := make([]string, 0, len(configured))
	for id := range configured {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		entry := configured[id]
		if err := validateConfiguredProvider(entry, baselines[id]); err != nil {
			invalid[id] = err
			continue
		}
		valid[id] = entry
	}
	return valid, invalid
}

func validateConfiguredProvider(config ProviderConfig, baseline []Model) error {
	path := "providers." + config.ID
	if config.oauthRadius && config.BaseURL == "" {
		return Diagnostic{"models.json", path + ".baseUrl", "is required when oauth is set"}
	}
	if config.BaseURL == "" && config.Headers == nil && !config.compatPresent && len(config.overrides) == 0 &&
		len(config.Models) == 0 && config.ConfiguredAPIKey == nil && !config.oauthRadius && config.AuthHeader == nil {
		return Diagnostic{"models.json", path, "must configure baseUrl, headers, compat, modelOverrides, models, apiKey, oauth, or authHeader"}
	}
	byID := make(map[string]Model, len(baseline))
	for _, candidate := range baseline {
		byID[candidate.ID] = candidate
	}
	var first Model
	if len(baseline) != 0 {
		first = baseline[0]
	}
	for index, definition := range config.Models {
		defaults, exists := byID[definition.ID]
		if !exists {
			defaults = first
		}
		api := firstNonEmpty(definition.API, config.API, defaults.API)
		if api == "" {
			return Diagnostic{"models.json", fmt.Sprintf("%s.models.%d.api", path, index), "is required at model or provider level"}
		}
		baseURL := firstNonEmpty(definition.BaseURL, config.BaseURL, defaults.BaseURL)
		if baseURL == "" {
			return Diagnostic{"models.json", fmt.Sprintf("%s.models.%d.baseUrl", path, index), "is required when defining custom models"}
		}
		resolved := definition
		resolved.API = api
		resolved.BaseURL = baseURL
		byID[definition.ID] = resolved
		if first.Provider == "" {
			first = resolved
		}
	}
	return nil
}

// SetGlobalSettings safely merges only known settings into the current raw root.
// Unknown fields are decoded as RawMessage and survive a write unchanged.
func (r *Runtime) SetGlobalSettings(ctx context.Context, change func(*Settings) error) error {
	if change == nil {
		return fmt.Errorf("%w: nil settings change", ErrInvalidConfig)
	}
	if runtime.GOOS == "windows" {
		return fmt.Errorf("%w: global settings.json", ErrPersistence)
	}
	path := filepath.Join(r.options.AgentDir, "settings.json")
	releaseLocal, err := acquireLocal(ctx, r.local)
	if err != nil {
		return err
	}
	defer releaseLocal()
	if err := requireExistingDirectory(filepath.Dir(path), "global settings directory"); err != nil {
		return err
	}
	release, err := acquireFileLock(ctx, path)
	if err != nil {
		return err
	}
	defer release()
	root, exists, err := readRawObject(path, false, "global settings.json")
	if err != nil {
		return err
	}
	if !exists {
		root = map[string]json.RawMessage{}
	}
	migratedQueueMode := migrateLegacyQueueMode(root)
	current, err := settingsFromRaw(root, "global settings.json")
	if err != nil {
		return err
	}
	previousTransport := current.Transport
	if err := change(&current); err != nil {
		return err
	}
	if current.Transport == "" && previousTransport != "" {
		current.transportPresent = false
	} else if current.Transport != "" {
		current.transportPresent = true
	}
	if err := validateSettings(current, "global settings.json"); err != nil {
		return err
	}
	projectSettings := Settings{}
	var projectSettingsError error
	if r.options.ProjectTrusted {
		r.mu.RLock()
		projectSettings = cloneSettings(r.projectSettings)
		r.mu.RUnlock()
		project, e := loadSettings(filepath.Join(r.options.WorkingDir, ".pi", "settings.json"), "project settings.json")
		if e != nil {
			projectSettingsError = e
		} else {
			projectSettings = project
		}
	}
	settings := mergeSettings(current, projectSettings)
	cached, storeErrors, storeError := loadStoreCatalogs(r.options.ModelsStorePath)
	if storeError != nil {
		cached = map[string]CachedCatalog{}
		storeErrors = map[string]error{}
	}
	putString(root, "defaultProvider", current.DefaultProvider)
	putString(root, "defaultModel", current.DefaultModel)
	putString(root, "defaultThinkingLevel", string(current.DefaultThinkingLevel))
	putString(root, "theme", current.Theme)
	putString(root, "transport", string(current.Transport))
	putString(root, "steeringMode", current.SteeringMode)
	putString(root, "followUpMode", current.FollowUpMode)
	putString(root, "shellPath", current.ShellPath)
	putString(root, "shellCommandPrefix", current.ShellCommandPrefix)
	if migratedQueueMode {
		delete(root, "queueMode")
	}
	if current.EnabledModels == nil {
		delete(root, "enabledModels")
	} else {
		b, _ := json.Marshal(current.EnabledModels)
		root["enabledModels"] = b
	}
	putOptionalSettingsObject(root, "compaction", map[string]json.RawMessage{
		"enabled": optionalBoolJSON(current.Compaction.Enabled), "reserveTokens": optionalUint64JSON(current.Compaction.ReserveTokens),
		"keepRecentTokens": optionalUint64JSON(current.Compaction.KeepRecentTokens),
	})
	putOptionalSettingsObject(root, "branchSummary", map[string]json.RawMessage{
		"reserveTokens": optionalUint64JSON(current.BranchSummary.ReserveTokens), "skipPrompt": optionalBoolJSON(current.BranchSummary.SkipPrompt),
	})
	putImageSettings(root, current.Images, current.imagesPresence)
	putSettingsStringArray(root, "skills", current.Skills, current.skillsPresence)
	putSettingsStringArray(root, "prompts", current.Prompts, current.promptsPresence)
	putThinkingBudgets(root, current.ThinkingBudgets)
	putRetrySettings(root, current.Retry)
	if current.HTTPIdleTimeoutMS == nil {
		delete(root, "httpIdleTimeoutMs")
	} else {
		root["httpIdleTimeoutMs"] = optionalUint64JSON(current.HTTPIdleTimeoutMS)
	}
	if current.WebsocketConnectTimeoutMS == nil {
		delete(root, "websocketConnectTimeoutMs")
	} else {
		root["websocketConnectTimeoutMs"] = optionalUint64JSON(current.WebsocketConnectTimeoutMS)
	}
	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encode global settings", ErrInvalidConfig)
	}
	writeErr := atomicWrite(ctx, path, append(encoded, '\n'), "global settings.json", r.faults)
	if writeErr != nil && !errors.Is(writeErr, ErrCommitUnknown) {
		return writeErr
	}
	// ErrCommitUnknown is only produced after the target rename. The new file is
	// therefore the sole visible candidate even though directory durability was
	// not acknowledged. Publish the same forward snapshot before returning the
	// uncertainty so callers never observe file-new/snapshot-old divergence.
	r.mu.Lock()
	generation := r.snapshot.Generation + 1
	r.snapshot = buildSnapshot(r.providers, cached, settings)
	r.snapshot.Generation = generation
	r.storeErrors = storeErrors
	r.storeError = storeError
	r.globalSettings = cloneSettings(current)
	r.projectSettings = cloneSettings(projectSettings)
	r.globalSettingsError = nil
	r.projectSettingsError = projectSettingsError
	r.mu.Unlock()
	return writeErr
}

func putString(root map[string]json.RawMessage, key, value string) {
	if value == "" {
		delete(root, key)
		return
	}
	b, _ := json.Marshal(value)
	root[key] = b
}

func putOptionalSettingsObject(root map[string]json.RawMessage, key string, fields map[string]json.RawMessage) {
	object := map[string]json.RawMessage{}
	if existing := root[key]; existing != nil {
		_ = json.Unmarshal(existing, &object)
	}
	if object == nil {
		object = map[string]json.RawMessage{}
	}
	for name, value := range fields {
		if value == nil {
			delete(object, name)
		} else {
			object[name] = value
		}
	}
	if len(object) == 0 {
		delete(root, key)
		return
	}
	encoded, _ := json.Marshal(object)
	root[key] = encoded
}

func optionalBoolJSON(value *bool) json.RawMessage {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(*value)
	return encoded
}

func optionalUint64JSON(value *uint64) json.RawMessage {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(*value)
	return encoded
}

func putRetrySettings(root map[string]json.RawMessage, value RetrySettings) {
	presence := value.presence
	if retrySettingsHaveKnownValues(value) {
		presence = settingsObjectPresent
	}
	switch presence {
	case settingsObjectAbsent:
		delete(root, "retry")
		return
	case settingsObjectNull:
		root["retry"] = json.RawMessage("null")
		return
	}

	object := map[string]json.RawMessage{}
	_ = json.Unmarshal(root["retry"], &object)
	if object == nil {
		object = map[string]json.RawMessage{}
	}
	for key, raw := range map[string]json.RawMessage{
		"enabled": optionalBoolJSON(value.Enabled), "maxRetries": optionalUint64JSON(value.MaxRetries),
		"baseDelayMs": optionalUint64JSON(value.BaseDelayMS),
	} {
		if raw == nil {
			delete(object, key)
		} else {
			object[key] = raw
		}
	}
	providerPresence := value.providerPresence
	if providerRetrySettingsHaveKnownValues(value.Provider) {
		providerPresence = settingsObjectPresent
	}
	switch providerPresence {
	case settingsObjectAbsent:
		delete(object, "provider")
	case settingsObjectNull:
		object["provider"] = json.RawMessage("null")
	case settingsObjectPresent:
		object["provider"] = optionalProviderRetryJSON(value.Provider, object["provider"])
	}
	encoded, _ := json.Marshal(object)
	root["retry"] = encoded
}

func putImageSettings(root map[string]json.RawMessage, value ImageSettings, presence settingsObjectPresence) {
	if value.AutoResize != nil || value.BlockImages != nil {
		presence = settingsObjectPresent
	}
	switch presence {
	case settingsObjectAbsent:
		delete(root, "images")
	case settingsObjectNull:
		root["images"] = json.RawMessage("null")
	case settingsObjectPresent:
		putOptionalSettingsObject(root, "images", map[string]json.RawMessage{
			"autoResize": optionalBoolJSON(value.AutoResize), "blockImages": optionalBoolJSON(value.BlockImages),
		})
	}
}

func putSettingsStringArray(root map[string]json.RawMessage, key string, values []string, presence settingsObjectPresence) {
	if values != nil {
		presence = settingsObjectPresent
	}
	switch presence {
	case settingsObjectAbsent:
		delete(root, key)
	case settingsObjectNull:
		root[key] = json.RawMessage("null")
	case settingsObjectPresent:
		encoded, _ := json.Marshal(values)
		root[key] = encoded
	}
}

func putThinkingBudgets(root map[string]json.RawMessage, value ThinkingBudgetSettings) {
	presence := value.presence
	if thinkingBudgetSettingsHaveKnownValues(value) {
		presence = settingsObjectPresent
	}
	switch presence {
	case settingsObjectAbsent:
		delete(root, "thinkingBudgets")
		return
	case settingsObjectNull:
		root["thinkingBudgets"] = json.RawMessage("null")
		return
	}
	object := map[string]json.RawMessage{}
	_ = json.Unmarshal(root["thinkingBudgets"], &object)
	if object == nil {
		object = map[string]json.RawMessage{}
	}
	for key, raw := range map[string]json.RawMessage{
		"minimal": optionalUint64JSON(value.Minimal), "low": optionalUint64JSON(value.Low),
		"medium": optionalUint64JSON(value.Medium), "high": optionalUint64JSON(value.High),
	} {
		if raw == nil {
			delete(object, key)
		} else {
			object[key] = raw
		}
	}
	encoded, _ := json.Marshal(object)
	root["thinkingBudgets"] = encoded
}

func thinkingBudgetSettingsHaveKnownValues(value ThinkingBudgetSettings) bool {
	return value.Minimal != nil || value.Low != nil || value.Medium != nil || value.High != nil
}

func retrySettingsHaveKnownValues(value RetrySettings) bool {
	return value.Enabled != nil || value.MaxRetries != nil || value.BaseDelayMS != nil ||
		value.providerPresence != settingsObjectAbsent || providerRetrySettingsHaveKnownValues(value.Provider)
}

func providerRetrySettingsHaveKnownValues(value ProviderRetrySettings) bool {
	return value.TimeoutMS != nil || value.MaxRetries != nil || value.MaxRetryDelayMS != nil
}

func optionalProviderRetryJSON(value ProviderRetrySettings, existing json.RawMessage) json.RawMessage {
	object := map[string]json.RawMessage{}
	_ = json.Unmarshal(existing, &object)
	if object == nil {
		object = map[string]json.RawMessage{}
	}
	for key, raw := range map[string]json.RawMessage{
		"timeoutMs": optionalUint64JSON(value.TimeoutMS), "maxRetries": optionalUint64JSON(value.MaxRetries),
		"maxRetryDelayMs": optionalUint64JSON(value.MaxRetryDelayMS),
	} {
		if raw == nil {
			delete(object, key)
		} else {
			object[key] = raw
		}
	}
	encoded, _ := json.Marshal(object)
	return encoded
}

type Selection struct{ Provider, Model string }
type Resolution struct {
	Model       Model
	Diagnostics []Diagnostic
}

// Resolve is a catalog-only compatibility entry point. Explicit selections use
// the same CLI resolver as pi; implicit selections use the same new-session
// scope/settings/provider-default order. Production selection should use
// availability-aware predicates or GetAvailable before accepting a model.
func (r *Runtime) Resolve(selection Selection) (Resolution, error) {
	s := r.Snapshot()
	providerID, modelID := strings.TrimSpace(selection.Provider), strings.TrimSpace(selection.Model)
	if modelID != "" {
		resolved := ResolveCLIModel(CLIModelOptions{Provider: providerID, Model: modelID, AllModels: s.Models})
		if resolved.Error != "" || resolved.Model == nil {
			return Resolution{}, fmt.Errorf("%w: %s", ErrNotFound, resolved.Error)
		}
		selected := r.applyConfiguredOverride(*resolved.Model)
		var diagnostics []Diagnostic
		if resolved.Warning != "" {
			diagnostics = append(diagnostics, Diagnostic{Source: "selection", Path: modelID, Message: resolved.Warning})
		}
		return Resolution{Model: selected, Diagnostics: diagnostics}, nil
	}
	if providerID != "" {
		return Resolution{}, fmt.Errorf("%w: provider requires a model", ErrInvalidConfig)
	}
	// The current production assembly still calls this compatibility method
	// before the session-aware factory exists. Preserve its explicit custom
	// settings path without leaking that behavior into ResolveInitialModel,
	// whose saved-default semantics remain identical to pi.
	if len(s.Settings.EnabledModels) == 0 && s.Settings.DefaultProvider != "" && s.Settings.DefaultModel != "" &&
		exactProviderModel(s.Models, s.Settings.DefaultProvider, s.Settings.DefaultModel) == nil {
		resolved := ResolveCLIModel(CLIModelOptions{Provider: s.Settings.DefaultProvider, Model: s.Settings.DefaultModel, AllModels: s.Models})
		if resolved.Model != nil && resolved.Error == "" {
			return Resolution{Model: r.applyConfiguredOverride(*resolved.Model)}, nil
		}
	}
	thinking := s.Settings.DefaultThinkingLevel
	var thinkingPointer *provider.ThinkingLevel
	if thinking != "" {
		thinkingPointer = &thinking
	}
	initial := ResolveInitialModel(InitialModelOptions{
		ScopePatterns: s.Settings.EnabledModels, DefaultProvider: s.Settings.DefaultProvider,
		DefaultModelID: s.Settings.DefaultModel, DefaultThinkingLevel: thinkingPointer, AllModels: s.Models,
		Availability: Availability{HasConfiguredAuth: func(string) bool { return true }, SupportsRoute: func(Model) bool { return true }},
	})
	diagnostics := make([]Diagnostic, 0, len(initial.Scope.Diagnostics))
	for _, diagnostic := range initial.Scope.Diagnostics {
		diagnostics = append(diagnostics, Diagnostic{Source: "settings", Path: "enabledModels", Message: diagnostic.Message})
	}
	if initial.Model == nil {
		return Resolution{Diagnostics: diagnostics}, fmt.Errorf("%w: no available model", ErrUnavailable)
	}
	return Resolution{Model: *initial.Model, Diagnostics: diagnostics}, nil
}

func (r *Runtime) applyConfiguredOverride(selected Model) Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	configured, ok := r.providers[selected.Provider]
	if !ok {
		return selected
	}
	override, ok := configured.overrides[selected.ID]
	if !ok {
		return selected
	}
	if len(override.compatRaw) != 0 {
		override.Compat, _ = decodeCompat(override.compatRaw, configured.ID+"/"+selected.ID, selected.API)
	}
	result := applyModelOverride(selected, override)
	result.Headers = mergeHeaders(override.Headers, result.Headers)
	return result
}

func buildSnapshot(providers map[string]ProviderConfig, cached map[string]CachedCatalog, settings Settings) Snapshot {
	byKey := make(map[string]Model)
	providerSet := make(map[string]struct{})
	for _, builtin := range builtinModels() {
		byKey[modelKey(builtin.Provider, builtin.ID)] = builtin
		providerSet[builtin.Provider] = struct{}{}
	}
	for id := range providers {
		providerSet[id] = struct{}{}
	}
	for id, entry := range cached {
		if _, registered := providers[id]; !registered {
			// models-store.json is provider-scoped cache, not a registration
			// source. Unknown entries remain on disk but cannot invent providers.
			continue
		}
		providerSet[id] = struct{}{}
		for index, cached := range entry.Models {
			m := cachedRuntimeModel(entry, index)
			byKey[modelKey(cached.Provider, cached.ID)] = m
		}
	}
	ids := make([]string, 0, len(providerSet))
	for id := range providerSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p, configured := providers[id]
		if !configured {
			continue
		}
		var firstProviderModel Model
		for _, candidate := range byKey {
			if candidate.Provider == id && (firstProviderModel.Provider == "" || candidate.ID < firstProviderModel.ID) {
				firstProviderModel = candidate
			}
		}
		// Provider baseUrl/compat change model metadata. Provider headers remain
		// in auth composition and are resolved at request time; only static and
		// per-model headers belong to Model.headers in pi.
		for key, base := range byKey {
			if base.Provider != id {
				continue
			}
			if p.BaseURL != "" {
				base.BaseURL = p.BaseURL
			}
			providerCompat := p.Compat
			if len(p.compatRaw) != 0 {
				providerCompat, _ = decodeCompat(p.compatRaw, p.ID, base.API)
			}
			base.Compat = mergeCompat(base.Compat, providerCompat)
			byKey[key] = base
		}
		for _, m := range p.Models {
			key := modelKey(p.ID, m.ID)
			base := byKey[key]
			defaults := base
			if defaults.Provider == "" {
				defaults = firstProviderModel
			}
			if m.API == "" {
				m.API = firstNonEmpty(p.API, defaults.API, defaultProviderAPI(p.ID))
			}
			if m.BaseURL == "" {
				m.BaseURL = firstNonEmpty(p.BaseURL, defaults.BaseURL, defaultProviderBaseURL(p.ID))
			}
			if m.Name == "" {
				m.Name = m.ID
			}
			if m.Input == nil {
				m.Input = []provider.InputKind{provider.InputText}
			}
			if m.ContextWindow == 0 {
				m.ContextWindow = 128_000
			}
			if m.MaxTokens == 0 {
				m.MaxTokens = 16_384
			}
			if len(m.compatRaw) != 0 {
				m.Compat, _ = decodeCompat(m.compatRaw, p.ID+"/"+m.ID, m.API)
			}
			providerCompat := p.Compat
			if len(p.compatRaw) != 0 {
				// Provider compat may be declared while each model supplies its
				// own API. Project the validated object after that API is known.
				providerCompat, _ = decodeCompat(p.compatRaw, p.ID, m.API)
			}
			m.Compat = mergeCompat(providerCompat, m.Compat)
			m.Provider = p.ID
			byKey[key] = m
			if firstProviderModel.Provider == "" {
				firstProviderModel = m
			}
		}
		for modelID, override := range p.overrides {
			key := modelKey(p.ID, modelID)
			m, ok := byKey[key]
			if !ok {
				continue
			}
			if len(override.compatRaw) != 0 {
				override.Compat, _ = decodeCompat(override.compatRaw, p.ID+"/"+modelID, m.API)
			}
			m = applyModelOverride(m, override)
			// This mirrors rawModelHeaders(): an explicit custom model's header
			// wins over modelOverrides for the same name; otherwise the override
			// supplies the request-only header.
			m.Headers = mergeHeaders(override.Headers, m.Headers)
			byKey[key] = m
		}
	}
	models := make([]Model, 0, len(byKey))
	for _, model := range byKey {
		models = append(models, cloneModel(model))
	}
	sort.Slice(models, func(left, right int) bool {
		if models[left].Provider != models[right].Provider {
			return models[left].Provider < models[right].Provider
		}
		return models[left].ID < models[right].ID
	})
	return Snapshot{Models: models, Providers: ids, Settings: cloneSettings(settings)}
}

// builtinModels is generated from the checked-in, scoped upstream pi-ai
// catalog oracle. The generator and per-file hashes make refreshes
// reproducible and prevent hand-maintained model subsets from drifting.
func builtinModels() []Model { return generatedBuiltinModels() }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func defaultProviderAPI(providerID string) string {
	switch providerID {
	case OpenAIProviderID:
		return OpenAIResponsesAPI
	case AzureOpenAIProviderID:
		return AzureOpenAIResponsesAPI
	case OpenAICodexProviderID:
		return OpenAICodexResponsesAPI
	case AnthropicProviderID:
		return AnthropicMessagesAPI
	default:
		return ""
	}
}

func defaultProviderBaseURL(providerID string) string {
	switch providerID {
	case OpenAIProviderID:
		return defaultOpenAIBaseURL
	case OpenAICodexProviderID:
		return defaultOpenAICodexBaseURL
	case AnthropicProviderID:
		return defaultAnthropicBaseURL
	case AzureOpenAIProviderID:
		return ""
	default:
		return ""
	}
}

func applyModelOverride(model Model, override modelOverride) Model {
	if override.Name != nil {
		model.Name = *override.Name
	}
	if override.Reasoning != nil {
		model.Reasoning = *override.Reasoning
	}
	if override.ThinkingLevelMap != nil {
		if model.ThinkingLevelMap == nil {
			model.ThinkingLevelMap = make(map[provider.ThinkingLevel]*string)
		}
		for level, value := range override.ThinkingLevelMap {
			if value == nil {
				model.ThinkingLevelMap[level] = nil
			} else {
				copy := *value
				model.ThinkingLevelMap[level] = &copy
			}
		}
	}
	if override.Input != nil {
		model.Input = cloneInputKinds(*override.Input)
	}
	if override.Cost != nil {
		if override.Cost.Input != nil {
			model.Cost.Input = *override.Cost.Input
		}
		if override.Cost.Output != nil {
			model.Cost.Output = *override.Cost.Output
		}
		if override.Cost.CacheRead != nil {
			model.Cost.CacheRead = *override.Cost.CacheRead
		}
		if override.Cost.CacheWrite != nil {
			model.Cost.CacheWrite = *override.Cost.CacheWrite
		}
		if override.Cost.Tiers != nil {
			model.Cost.Tiers = append([]provider.CostTier(nil), (*override.Cost.Tiers)...)
		}
	}
	if override.ContextWindow != nil {
		model.ContextWindow = *override.ContextWindow
	}
	if override.MaxTokens != nil {
		model.MaxTokens = *override.MaxTokens
	}
	if override.CompatPresent {
		model.Compat = mergeCompat(model.Compat, override.Compat)
	}
	model.UnsupportedFields = appendUnique(model.UnsupportedFields, override.UnsupportedFields...)
	model.UnknownFields = appendUnique(model.UnknownFields, override.UnknownFields...)
	return model
}

func loadModels(path string) (map[string]ProviderConfig, error) {
	root, exists, err := readRawObject(path, true, "models.json")
	if err != nil || !exists {
		return map[string]ProviderConfig{}, err
	}
	raw, ok := root["providers"]
	if !ok {
		return nil, Diagnostic{"models.json", "providers", "required object is missing"}
	}
	var providers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &providers); err != nil || providers == nil {
		return nil, Diagnostic{"models.json", "providers", "must be an object"}
	}
	result := make(map[string]ProviderConfig, len(providers))
	ids := make([]string, 0, len(providers))
	for id := range providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		data := providers[id]
		if !validID(id) {
			return nil, Diagnostic{"models.json", "providers", "provider identifier is invalid"}
		}
		p, err := parseProvider(id, data)
		if err != nil {
			return nil, err
		}
		result[id] = p
	}
	return result, nil
}
func parseProvider(id string, raw json.RawMessage) (ProviderConfig, error) {
	var o map[string]json.RawMessage
	if err := json.Unmarshal(raw, &o); err != nil || o == nil {
		return ProviderConfig{}, Diagnostic{"models.json", "providers." + id, "must be an object"}
	}
	p := ProviderConfig{ID: id}
	var err error
	if p.Name, err = optionalString(o, "name", id); err != nil {
		return p, err
	}
	if p.BaseURL, err = optionalString(o, "baseUrl", id); err != nil {
		return p, err
	}
	if p.API, err = optionalString(o, "api", id); err != nil {
		return p, err
	}
	if p.Headers, err = optionalHeaders(o, "headers", id); err != nil {
		return p, err
	}
	if raw, ok := o["compat"]; ok {
		p.compatPresent = true
		compatAPI := firstNonEmpty(p.API, defaultProviderAPI(id))
		decoded, decodeErr := decodeCompat(raw, id, compatAPI)
		if decodeErr != nil {
			return p, decodeErr
		}
		if compatAPI == "" {
			p.compatRaw = bytes.Clone(raw)
		} else {
			p.Compat = decoded
		}
	}
	if key, ok, err := optionalSecret(o, "apiKey", id); err != nil {
		return p, err
	} else if ok {
		p.ConfiguredAPIKey = &key
	}
	if value, present, parseErr := optionalBool(o, "authHeader", id); parseErr != nil {
		return p, parseErr
	} else if present {
		p.AuthHeader = &value
	}
	if rawOAuth, present := o["oauth"]; present {
		var oauth string
		if err := json.Unmarshal(rawOAuth, &oauth); err != nil || oauth != "radius" {
			return p, Diagnostic{"models.json", "providers." + id + ".oauth", "must be \"radius\""}
		}
		p.UnsupportedFields = append(p.UnsupportedFields, "oauth")
		p.oauthRadius = true
	}
	if data, ok := o["models"]; ok {
		var models []json.RawMessage
		if err := json.Unmarshal(data, &models); err != nil || models == nil {
			return p, Diagnostic{"models.json", "providers." + id + ".models", "must be an array"}
		}
		for i, entry := range models {
			m, e := parseModel(id, p.API, i, entry)
			if e != nil {
				return p, e
			}
			p.Models = append(p.Models, m)
		}
	}
	if data, ok := o["modelOverrides"]; ok {
		var overrides map[string]json.RawMessage
		if err := json.Unmarshal(data, &overrides); err != nil || overrides == nil {
			return p, Diagnostic{"models.json", "providers." + id + ".modelOverrides", "must be an object"}
		}
		p.overrides = make(map[string]modelOverride, len(overrides))
		modelIDs := make([]string, 0, len(overrides))
		for modelID := range overrides {
			modelIDs = append(modelIDs, modelID)
		}
		sort.Strings(modelIDs)
		for _, modelID := range modelIDs {
			value := overrides[modelID]
			if !validValue(modelID) {
				return p, Diagnostic{"models.json", "providers." + id + ".modelOverrides", "contains invalid model id"}
			}
			overrideAPI := firstNonEmpty(p.API, defaultProviderAPI(id))
			for _, model := range p.Models {
				if model.ID == modelID && model.API != "" {
					overrideAPI = model.API
					break
				}
			}
			override, err := parseOverride(id, modelID, overrideAPI, value)
			if err != nil {
				return p, err
			}
			p.overrides[modelID] = override
		}
	}
	for key := range o {
		switch key {
		case "name", "baseUrl", "apiKey", "api", "headers", "models", "modelOverrides", "compat", "oauth", "authHeader":
		default:
			p.UnknownFields = append(p.UnknownFields, key)
		}
	}
	sort.Strings(p.UnknownFields)
	sort.Strings(p.UnsupportedFields)
	return p, nil
}
func parseOverride(providerID, modelID, api string, raw json.RawMessage) (modelOverride, error) {
	var o map[string]json.RawMessage
	if err := json.Unmarshal(raw, &o); err != nil || o == nil {
		return modelOverride{}, Diagnostic{"models.json", "providers." + providerID + ".modelOverrides." + modelID, "must be an object"}
	}
	result := modelOverride{}
	if value, present, err := requiredString(o, "name", providerID); err != nil {
		return result, err
	} else if present {
		result.Name = &value
	}
	if value, present, err := optionalBool(o, "reasoning", providerID); err != nil {
		return result, err
	} else if present {
		result.Reasoning = &value
	}
	if rawValue, present := o["thinkingLevelMap"]; present {
		value, err := decodeThinkingLevelMap(rawValue, providerID)
		if err != nil {
			return result, err
		}
		result.ThinkingLevelMap = value
	}
	if rawValue, present := o["input"]; present {
		value, err := decodeInputKinds(rawValue, providerID)
		if err != nil {
			return result, err
		}
		result.Input = &value
	}
	if rawValue, present := o["cost"]; present {
		value, err := decodeCostOverride(rawValue, providerID)
		if err != nil {
			return result, err
		}
		result.Cost = &value
	}
	if rawValue, present := o["contextWindow"]; present {
		value, err := requiredPositiveUint64(rawValue, providerID, "contextWindow")
		if err != nil {
			return result, err
		}
		result.ContextWindow = &value
	}
	if rawValue, present := o["maxTokens"]; present {
		value, err := requiredPositiveUint64(rawValue, providerID, "maxTokens")
		if err != nil {
			return result, err
		}
		result.MaxTokens = &value
	}
	if value, err := optionalHeaders(o, "headers", providerID); err != nil {
		return result, err
	} else if _, present := o["headers"]; present {
		result.Headers = value
	}
	if rawValue, present := o["compat"]; present {
		value, err := decodeCompat(rawValue, providerID, api)
		if err != nil {
			return result, err
		}
		result.Compat, result.CompatPresent = value, true
		result.compatRaw = bytes.Clone(rawValue)
	}
	for key := range o {
		switch key {
		case "name", "reasoning", "thinkingLevelMap", "input", "cost", "contextWindow", "maxTokens", "headers", "compat":
		default:
			result.UnknownFields = append(result.UnknownFields, key)
		}
	}
	sort.Strings(result.UnsupportedFields)
	sort.Strings(result.UnknownFields)
	return result, nil
}
func parseModel(providerID, providerAPI string, index int, raw json.RawMessage) (Model, error) {
	var o map[string]json.RawMessage
	if err := json.Unmarshal(raw, &o); err != nil || o == nil {
		return Model{}, Diagnostic{"models.json", fmt.Sprintf("providers.%s.models.%d", providerID, index), "must be an object"}
	}
	id, ok, err := requiredString(o, "id", providerID)
	if err != nil {
		return Model{}, err
	}
	if !ok {
		return Model{}, Diagnostic{"models.json", fmt.Sprintf("providers.%s.models.%d.id", providerID, index), "is required"}
	}
	m := Model{ID: id}
	m.Name, err = optionalString(o, "name", providerID)
	if err != nil {
		return m, err
	}
	m.API, err = optionalString(o, "api", providerID)
	if err != nil {
		return m, err
	}
	m.BaseURL, err = optionalString(o, "baseUrl", providerID)
	if err != nil {
		return m, err
	}
	if value, present, parseErr := optionalBool(o, "reasoning", providerID); parseErr != nil {
		return m, parseErr
	} else if present {
		m.Reasoning = value
	}
	if m.ThinkingLevelMap, err = decodeThinkingLevelMap(o["thinkingLevelMap"], providerID); err != nil {
		return m, err
	}
	if m.Input, err = decodeInputKinds(o["input"], providerID); err != nil {
		return m, err
	}
	if m.Headers, err = optionalHeaders(o, "headers", providerID); err != nil {
		return m, err
	}
	if raw, exists := o["cost"]; exists {
		if m.Cost, err = decodeCost(raw, providerID); err != nil {
			return m, err
		}
	}
	if m.ContextWindow, err = optionalUint64(o, "contextWindow", providerID); err != nil {
		return m, err
	}
	if m.MaxTokens, err = optionalUint64(o, "maxTokens", providerID); err != nil {
		return m, err
	}
	if raw, exists := o["compat"]; exists {
		compatAPI := m.API
		if compatAPI == "" {
			compatAPI = providerAPI
		}
		if m.Compat, err = decodeCompat(raw, providerID, compatAPI); err != nil {
			return m, err
		}
		m.compatRaw = bytes.Clone(raw)
	}
	for key := range o {
		switch key {
		case "id", "name", "api", "baseUrl", "reasoning", "thinkingLevelMap", "input", "cost", "contextWindow", "maxTokens", "headers", "compat":
		default:
			m.UnknownFields = append(m.UnknownFields, key)
		}
	}
	sort.Strings(m.UnsupportedFields)
	sort.Strings(m.UnknownFields)
	return m, nil
}

func loadSettings(path, label string) (Settings, error) {
	root, exists, err := readRawObject(path, false, label)
	if err != nil || !exists {
		return Settings{}, err
	}
	migrateLegacyQueueMode(root)
	return settingsFromRaw(root, label)
}

func migrateLegacyQueueMode(root map[string]json.RawMessage) bool {
	if _, exists := root["steeringMode"]; exists {
		return false
	}
	if legacy, exists := root["queueMode"]; exists {
		root["steeringMode"] = legacy
		delete(root, "queueMode")
		return true
	}
	return false
}

// migrateLegacyWebsockets mirrors SettingsManager's legacy boolean mapping.
// An explicit transport always wins; an invalid legacy value remains an
// unknown field instead of changing request routing.
func migrateLegacyWebsockets(root map[string]json.RawMessage) bool {
	if _, exists := root["transport"]; exists {
		return false
	}
	raw, exists := root["websockets"]
	if !exists {
		return false
	}
	var enabled bool
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &enabled) != nil {
		return false
	}
	transport := provider.TransportSSE
	if enabled {
		transport = provider.TransportWebsocket
	}
	encoded, _ := json.Marshal(transport)
	root["transport"] = encoded
	delete(root, "websockets")
	return true
}

// migrateLegacyProviderRetry mirrors SettingsManager's retry.maxDelayMs
// migration. The legacy member was never part of the provider request
// contract; when valid it becomes retry.provider.maxRetryDelayMs, unless the
// nested value is already present.
func migrateLegacyProviderRetry(root map[string]json.RawMessage) bool {
	retryRaw, exists := root["retry"]
	if !exists {
		return false
	}
	var retry map[string]json.RawMessage
	if json.Unmarshal(retryRaw, &retry) != nil || retry == nil {
		return false
	}
	legacy, exists := retry["maxDelayMs"]
	if !exists {
		return false
	}
	delete(retry, "maxDelayMs")
	var value uint64
	if !bytes.Equal(bytes.TrimSpace(legacy), []byte("null")) && json.Unmarshal(legacy, &value) == nil {
		var providerSettings map[string]json.RawMessage
		_ = json.Unmarshal(retry["provider"], &providerSettings)
		if providerSettings == nil {
			providerSettings = map[string]json.RawMessage{}
		}
		current, configured := providerSettings["maxRetryDelayMs"]
		if !configured || bytes.Equal(bytes.TrimSpace(current), []byte("null")) {
			providerSettings["maxRetryDelayMs"] = optionalUint64JSON(&value)
			encoded, _ := json.Marshal(providerSettings)
			retry["provider"] = encoded
		}
	}
	encoded, _ := json.Marshal(retry)
	root["retry"] = encoded
	return true
}

func settingsFromRaw(root map[string]json.RawMessage, label string) (Settings, error) {
	migrateLegacyQueueMode(root)
	migrateLegacyWebsockets(root)
	migrateLegacyProviderRetry(root)
	s := Settings{}
	var err error
	if s.DefaultProvider, err = optionalString(root, "defaultProvider", ""); err != nil {
		return s, fmt.Errorf("%w: %s", ErrInvalidConfig, label)
	}
	if s.DefaultModel, err = optionalString(root, "defaultModel", ""); err != nil {
		return s, fmt.Errorf("%w: %s", ErrInvalidConfig, label)
	}
	if s.Theme, err = optionalString(root, "theme", ""); err != nil {
		return s, Diagnostic{label, "theme", "must be a valid theme name"}
	}
	if raw, ok := root["defaultThinkingLevel"]; ok {
		var value string
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) < 2 || trimmed[0] != '"' {
			return s, Diagnostic{label, "defaultThinkingLevel", "must be a valid thinking level"}
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return s, Diagnostic{label, "defaultThinkingLevel", "must be a valid thinking level"}
		}
		s.DefaultThinkingLevel = provider.ThinkingLevel(value)
	}
	if raw, ok := root["transport"]; ok {
		s.transportPresent = true
		var value string
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
			return s, Diagnostic{label, "transport", "must be one of auto, sse, websocket, or websocket-cached"}
		}
		s.Transport = provider.Transport(value)
	}
	if s.SteeringMode, err = optionalString(root, "steeringMode", ""); err != nil {
		return s, Diagnostic{label, "steeringMode", "must be all or one-at-a-time"}
	}
	if s.FollowUpMode, err = optionalString(root, "followUpMode", ""); err != nil {
		return s, Diagnostic{label, "followUpMode", "must be all or one-at-a-time"}
	}
	if s.ShellPath, s.shellPathPresent, err = optionalSettingsString(root, "shellPath"); err != nil {
		return s, Diagnostic{label, "shellPath", "must be a valid string"}
	}
	if s.ShellCommandPrefix, s.shellCommandPrefixPresent, err = optionalSettingsString(root, "shellCommandPrefix"); err != nil {
		return s, Diagnostic{label, "shellCommandPrefix", "must be a valid string"}
	}
	if raw, ok := root["enabledModels"]; ok {
		if err := json.Unmarshal(raw, &s.EnabledModels); err != nil {
			return s, Diagnostic{label, "enabledModels", "must be an array of strings"}
		}
		for _, v := range s.EnabledModels {
			if !validValue(v) {
				return s, Diagnostic{label, "enabledModels", "contains invalid selector"}
			}
		}
	}
	if raw, ok := root["compaction"]; ok {
		if err := json.Unmarshal(raw, &s.Compaction); err != nil {
			return s, Diagnostic{label, "compaction", "must be an object with enabled, reserveTokens, and keepRecentTokens"}
		}
	}
	if raw, ok := root["branchSummary"]; ok {
		if err := json.Unmarshal(raw, &s.BranchSummary); err != nil {
			return s, Diagnostic{label, "branchSummary", "must be an object with reserveTokens and skipPrompt"}
		}
	}
	if raw, ok := root["images"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			s.imagesPresence = settingsObjectNull
		} else if err := json.Unmarshal(raw, &s.Images); err != nil {
			return s, Diagnostic{label, "images", "must be an object with autoResize and blockImages"}
		} else {
			s.imagesPresence = settingsObjectPresent
		}
	}
	if s.Skills, s.skillsPresence, err = decodeSettingsStringArray(root, "skills", label); err != nil {
		return s, err
	}
	if s.Prompts, s.promptsPresence, err = decodeSettingsStringArray(root, "prompts", label); err != nil {
		return s, err
	}
	if raw, ok := root["thinkingBudgets"]; ok {
		if s.ThinkingBudgets, err = decodeThinkingBudgetSettings(raw, label); err != nil {
			return s, err
		}
	}
	if raw, ok := root["retry"]; ok {
		if s.Retry, err = decodeRetrySettings(raw, label); err != nil {
			return s, err
		}
	}
	if s.HTTPIdleTimeoutMS, err = decodeOptionalSettingsUint64(root, "httpIdleTimeoutMs", label); err != nil {
		return s, err
	}
	if s.WebsocketConnectTimeoutMS, err = decodeOptionalSettingsUint64(root, "websocketConnectTimeoutMs", label); err != nil {
		return s, err
	}
	if err := validateSettings(s, label); err != nil {
		return s, err
	}
	return s, nil
}

func decodeSettingsStringArray(root map[string]json.RawMessage, key, label string) ([]string, settingsObjectPresence, error) {
	raw, ok := root[key]
	if !ok {
		return nil, settingsObjectAbsent, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, settingsObjectNull, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, settingsObjectAbsent, Diagnostic{label, key, "must be an array of strings"}
	}
	for _, value := range values {
		if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			return nil, settingsObjectAbsent, Diagnostic{label, key, "contains an invalid path"}
		}
	}
	return values, settingsObjectPresent, nil
}

func decodeThinkingBudgetSettings(raw json.RawMessage, label string) (ThinkingBudgetSettings, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ThinkingBudgetSettings{presence: settingsObjectNull}, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return ThinkingBudgetSettings{}, Diagnostic{label, "thinkingBudgets", "must be an object with minimal, low, medium, and high"}
	}
	result := ThinkingBudgetSettings{presence: settingsObjectPresent}
	fields := []struct {
		key    string
		target **uint64
	}{
		{"minimal", &result.Minimal}, {"low", &result.Low}, {"medium", &result.Medium}, {"high", &result.High},
	}
	for _, field := range fields {
		value, err := decodeOptionalSettingsUint64(object, field.key, label)
		if err != nil {
			return ThinkingBudgetSettings{}, Diagnostic{label, "thinkingBudgets." + field.key, "must be a non-negative integer"}
		}
		*field.target = value
	}
	return result, nil
}

func validateSettings(s Settings, label string) error {
	if s.DefaultProvider != "" && !validID(s.DefaultProvider) {
		return Diagnostic{label, "defaultProvider", "must be a non-empty identifier"}
	}
	if s.DefaultModel != "" && !validValue(s.DefaultModel) {
		return Diagnostic{label, "defaultModel", "must be a non-empty selector"}
	}
	if s.DefaultThinkingLevel != "" && !s.DefaultThinkingLevel.Valid() {
		return Diagnostic{label, "defaultThinkingLevel", "must be one of off, minimal, low, medium, high, xhigh, max"}
	}
	if s.Theme != "" && (!validValue(s.Theme) || strings.Count(s.Theme, "/") > 1) {
		return Diagnostic{label, "theme", "must be a theme name or light/dark pair"}
	}
	if (s.transportPresent || s.Transport != "") && s.Transport != provider.TransportAuto && s.Transport != provider.TransportSSE &&
		s.Transport != provider.TransportWebsocket && s.Transport != provider.TransportWebsocketCached {
		return Diagnostic{label, "transport", "must be one of auto, sse, websocket, or websocket-cached"}
	}
	if s.SteeringMode != "" && s.SteeringMode != QueueModeAll && s.SteeringMode != QueueModeOneAtATime {
		return Diagnostic{label, "steeringMode", "must be all or one-at-a-time"}
	}
	if s.FollowUpMode != "" && s.FollowUpMode != QueueModeAll && s.FollowUpMode != QueueModeOneAtATime {
		return Diagnostic{label, "followUpMode", "must be all or one-at-a-time"}
	}
	if s.Retry.MaxRetries != nil && *s.Retry.MaxRetries > uint64(math.MaxUint32-1) {
		return Diagnostic{label, "retry.maxRetries", "is too large"}
	}
	if s.Retry.BaseDelayMS != nil && *s.Retry.BaseDelayMS > uint64(math.MaxInt64/int64(time.Millisecond)) {
		return Diagnostic{label, "retry.baseDelayMs", "is too large"}
	}
	if s.Retry.Provider.MaxRetries != nil && *s.Retry.Provider.MaxRetries > uint64(math.MaxUint32) {
		return Diagnostic{label, "retry.provider.maxRetries", "is too large"}
	}
	for path, value := range map[string]*uint64{
		"httpIdleTimeoutMs":              s.HTTPIdleTimeoutMS,
		"websocketConnectTimeoutMs":      s.WebsocketConnectTimeoutMS,
		"retry.provider.timeoutMs":       s.Retry.Provider.TimeoutMS,
		"retry.provider.maxRetryDelayMs": s.Retry.Provider.MaxRetryDelayMS,
	} {
		if value != nil && *value > uint64(math.MaxInt64/int64(time.Millisecond)) {
			return Diagnostic{label, path, "is too large"}
		}
	}
	return nil
}

func decodeRetrySettings(raw json.RawMessage, label string) (RetrySettings, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return RetrySettings{presence: settingsObjectNull}, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return RetrySettings{}, Diagnostic{label, "retry", "must be an object with enabled, maxRetries, baseDelayMs, and provider"}
	}
	result := RetrySettings{presence: settingsObjectPresent}
	var err error
	if result.Enabled, err = decodeOptionalSettingsBool(object, "enabled", label, "retry.enabled"); err != nil {
		return RetrySettings{}, err
	}
	if result.MaxRetries, err = decodeOptionalSettingsUint64(object, "maxRetries", label); err != nil {
		return RetrySettings{}, Diagnostic{label, "retry.maxRetries", "must be a non-negative integer"}
	}
	if result.BaseDelayMS, err = decodeOptionalSettingsUint64(object, "baseDelayMs", label); err != nil {
		return RetrySettings{}, Diagnostic{label, "retry.baseDelayMs", "must be a non-negative integer"}
	}
	if providerRaw, ok := object["provider"]; ok {
		if bytes.Equal(bytes.TrimSpace(providerRaw), []byte("null")) {
			result.providerPresence = settingsObjectNull
			return result, nil
		}
		var providerObject map[string]json.RawMessage
		if err := json.Unmarshal(providerRaw, &providerObject); err != nil || providerObject == nil {
			return RetrySettings{}, Diagnostic{label, "retry.provider", "must be an object"}
		}
		result.providerPresence = settingsObjectPresent
		if result.Provider.TimeoutMS, err = decodeOptionalSettingsUint64(providerObject, "timeoutMs", label); err != nil {
			return RetrySettings{}, Diagnostic{label, "retry.provider.timeoutMs", "must be a non-negative integer"}
		}
		if result.Provider.MaxRetries, err = decodeOptionalSettingsUint64(providerObject, "maxRetries", label); err != nil {
			return RetrySettings{}, Diagnostic{label, "retry.provider.maxRetries", "must be a non-negative integer"}
		}
		if result.Provider.MaxRetryDelayMS, err = decodeOptionalSettingsUint64(providerObject, "maxRetryDelayMs", label); err != nil {
			return RetrySettings{}, Diagnostic{label, "retry.provider.maxRetryDelayMs", "must be a non-negative integer"}
		}
	}
	return result, nil
}

func decodeOptionalSettingsBool(object map[string]json.RawMessage, key, label, path string) (*bool, error) {
	raw, ok := object[key]
	if !ok {
		return nil, nil
	}
	var value bool
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return nil, Diagnostic{label, path, "must be a boolean"}
	}
	return &value, nil
}

func decodeOptionalSettingsUint64(object map[string]json.RawMessage, key, label string) (*uint64, error) {
	raw, ok := object[key]
	if !ok {
		return nil, nil
	}
	var value uint64
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return nil, Diagnostic{label, key, "must be a non-negative integer"}
	}
	return &value, nil
}
func mergeSettings(base, override Settings) Settings {
	out := cloneSettings(base)
	if override.DefaultProvider != "" {
		out.DefaultProvider = override.DefaultProvider
	}
	if override.DefaultModel != "" {
		out.DefaultModel = override.DefaultModel
	}
	if override.DefaultThinkingLevel != "" {
		out.DefaultThinkingLevel = override.DefaultThinkingLevel
	}
	if override.Theme != "" {
		out.Theme = override.Theme
	}
	if override.transportPresent || override.Transport != "" {
		out.Transport = override.Transport
		out.transportPresent = true
	}
	if override.SteeringMode != "" {
		out.SteeringMode = override.SteeringMode
	}
	if override.FollowUpMode != "" {
		out.FollowUpMode = override.FollowUpMode
	}
	if override.shellPathPresent || override.ShellPath != "" {
		out.ShellPath = override.ShellPath
		out.shellPathPresent = true
	}
	if override.shellCommandPrefixPresent || override.ShellCommandPrefix != "" {
		out.ShellCommandPrefix = override.ShellCommandPrefix
		out.shellCommandPrefixPresent = true
	}
	imagesPresence := override.imagesPresence
	if imagesPresence == settingsObjectAbsent && (override.Images.AutoResize != nil || override.Images.BlockImages != nil) {
		imagesPresence = settingsObjectPresent
	}
	switch imagesPresence {
	case settingsObjectNull:
		out.Images = ImageSettings{}
		out.imagesPresence = settingsObjectNull
	case settingsObjectPresent:
		if out.imagesPresence == settingsObjectNull {
			out.Images = ImageSettings{}
		}
		out.imagesPresence = settingsObjectPresent
		if override.Images.AutoResize != nil {
			out.Images.AutoResize = cloneBoolPointer(override.Images.AutoResize)
		}
		if override.Images.BlockImages != nil {
			out.Images.BlockImages = cloneBoolPointer(override.Images.BlockImages)
		}
	}
	skillsPresence := override.skillsPresence
	if skillsPresence == settingsObjectAbsent && override.Skills != nil {
		skillsPresence = settingsObjectPresent
	}
	switch skillsPresence {
	case settingsObjectNull:
		out.Skills = nil
		out.skillsPresence = settingsObjectNull
	case settingsObjectPresent:
		out.Skills = append([]string{}, override.Skills...)
		out.skillsPresence = settingsObjectPresent
	}
	promptsPresence := override.promptsPresence
	if promptsPresence == settingsObjectAbsent && override.Prompts != nil {
		promptsPresence = settingsObjectPresent
	}
	switch promptsPresence {
	case settingsObjectNull:
		out.Prompts = nil
		out.promptsPresence = settingsObjectNull
	case settingsObjectPresent:
		out.Prompts = append([]string{}, override.Prompts...)
		out.promptsPresence = settingsObjectPresent
	}
	thinkingBudgetsPresence := override.ThinkingBudgets.presence
	if thinkingBudgetsPresence == settingsObjectAbsent && thinkingBudgetSettingsHaveKnownValues(override.ThinkingBudgets) {
		thinkingBudgetsPresence = settingsObjectPresent
	}
	switch thinkingBudgetsPresence {
	case settingsObjectNull:
		out.ThinkingBudgets = ThinkingBudgetSettings{presence: settingsObjectNull}
	case settingsObjectPresent:
		if out.ThinkingBudgets.presence == settingsObjectNull {
			out.ThinkingBudgets = ThinkingBudgetSettings{}
		}
		out.ThinkingBudgets.presence = settingsObjectPresent
		if override.ThinkingBudgets.Minimal != nil {
			out.ThinkingBudgets.Minimal = cloneUint64Pointer(override.ThinkingBudgets.Minimal)
		}
		if override.ThinkingBudgets.Low != nil {
			out.ThinkingBudgets.Low = cloneUint64Pointer(override.ThinkingBudgets.Low)
		}
		if override.ThinkingBudgets.Medium != nil {
			out.ThinkingBudgets.Medium = cloneUint64Pointer(override.ThinkingBudgets.Medium)
		}
		if override.ThinkingBudgets.High != nil {
			out.ThinkingBudgets.High = cloneUint64Pointer(override.ThinkingBudgets.High)
		}
	}
	if override.EnabledModels != nil {
		out.EnabledModels = append([]string(nil), override.EnabledModels...)
	}
	if override.Compaction.Enabled != nil {
		out.Compaction.Enabled = cloneBoolPointer(override.Compaction.Enabled)
	}
	if override.Compaction.ReserveTokens != nil {
		out.Compaction.ReserveTokens = cloneUint64Pointer(override.Compaction.ReserveTokens)
	}
	if override.Compaction.KeepRecentTokens != nil {
		out.Compaction.KeepRecentTokens = cloneUint64Pointer(override.Compaction.KeepRecentTokens)
	}
	if override.BranchSummary.ReserveTokens != nil {
		out.BranchSummary.ReserveTokens = cloneUint64Pointer(override.BranchSummary.ReserveTokens)
	}
	if override.BranchSummary.SkipPrompt != nil {
		out.BranchSummary.SkipPrompt = cloneBoolPointer(override.BranchSummary.SkipPrompt)
	}
	retryPresence := override.Retry.presence
	if retryPresence == settingsObjectAbsent && retrySettingsHaveKnownValues(override.Retry) {
		retryPresence = settingsObjectPresent
	}
	switch retryPresence {
	case settingsObjectNull:
		out.Retry = RetrySettings{presence: settingsObjectNull}
	case settingsObjectPresent:
		if out.Retry.presence == settingsObjectNull {
			out.Retry = RetrySettings{}
		}
		out.Retry.presence = settingsObjectPresent
		if override.Retry.Enabled != nil {
			out.Retry.Enabled = cloneBoolPointer(override.Retry.Enabled)
		}
		if override.Retry.MaxRetries != nil {
			out.Retry.MaxRetries = cloneUint64Pointer(override.Retry.MaxRetries)
		}
		if override.Retry.BaseDelayMS != nil {
			out.Retry.BaseDelayMS = cloneUint64Pointer(override.Retry.BaseDelayMS)
		}
		providerPresence := override.Retry.providerPresence
		if providerPresence == settingsObjectAbsent && providerRetrySettingsHaveKnownValues(override.Retry.Provider) {
			providerPresence = settingsObjectPresent
		}
		switch providerPresence {
		case settingsObjectNull:
			out.Retry.Provider = ProviderRetrySettings{}
			out.Retry.providerPresence = settingsObjectNull
		case settingsObjectPresent:
			out.Retry.Provider = cloneProviderRetrySettings(override.Retry.Provider)
			out.Retry.providerPresence = settingsObjectPresent
		}
	}
	if override.HTTPIdleTimeoutMS != nil {
		out.HTTPIdleTimeoutMS = cloneUint64Pointer(override.HTTPIdleTimeoutMS)
	}
	if override.WebsocketConnectTimeoutMS != nil {
		out.WebsocketConnectTimeoutMS = cloneUint64Pointer(override.WebsocketConnectTimeoutMS)
	}
	return out
}

func optionalString(o map[string]json.RawMessage, key, owner string) (string, error) {
	v, _, err := requiredString(o, key, owner)
	return v, err
}

func optionalSettingsString(object map[string]json.RawMessage, key string) (string, bool, error) {
	raw, ok := object[key]
	if !ok {
		return "", false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", true, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return "", true, ErrInvalidConfig
	}
	return value, true, nil
}
func requiredString(o map[string]json.RawMessage, key, owner string) (string, bool, error) {
	raw, ok := o[key]
	if !ok {
		return "", false, nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil || v == "" || !utf8.ValidString(v) {
		return "", true, Diagnostic{"models.json", key, "must be a non-empty valid string"}
	}
	return v, true, nil
}
func optionalSecret(o map[string]json.RawMessage, key, owner string) (string, bool, error) {
	v, ok, err := requiredString(o, key, owner)
	if err != nil {
		return "", ok, Diagnostic{"models.json", key, "must be a valid configured credential"}
	}
	return v, ok, nil
}
func optionalBool(o map[string]json.RawMessage, key, owner string) (bool, bool, error) {
	raw, ok := o[key]
	if !ok {
		return false, false, nil
	}
	var v *bool
	if err := json.Unmarshal(raw, &v); err != nil || v == nil {
		return false, true, Diagnostic{"models.json", key, "must be a boolean"}
	}
	return *v, true, nil
}
func optionalHeaders(o map[string]json.RawMessage, key, owner string) (map[string]string, error) {
	raw, ok := o[key]
	if !ok {
		return nil, nil
	}
	var h map[string]string
	if err := json.Unmarshal(raw, &h); err != nil || h == nil {
		return nil, Diagnostic{"models.json", key, "must be an object of strings"}
	}
	return h, nil
}

func optionalUint64(o map[string]json.RawMessage, key, owner string) (uint64, error) {
	raw, ok := o[key]
	if !ok {
		return 0, nil
	}
	return requiredPositiveUint64(raw, owner, key)
}

func requiredPositiveUint64(raw json.RawMessage, owner, key string) (uint64, error) {
	var value uint64
	if err := json.Unmarshal(raw, &value); err != nil || value == 0 {
		return 0, Diagnostic{"models.json", key, "must be a positive integer"}
	}
	return value, nil
}

func decodeThinkingLevelMap(raw json.RawMessage, owner string) (map[provider.ThinkingLevel]*string, error) {
	if raw == nil {
		return nil, nil
	}
	var wire map[string]*string
	if err := json.Unmarshal(raw, &wire); err != nil || wire == nil {
		return nil, Diagnostic{"models.json", owner, "thinkingLevelMap must be an object"}
	}
	result := make(map[provider.ThinkingLevel]*string, len(wire))
	for key, value := range wire {
		level := provider.ThinkingLevel(key)
		if !level.Valid() {
			// TypeBox objects are open unless additionalProperties:false is
			// requested. Unknown future thinking-level members therefore do not
			// participate in the current runtime projection.
			continue
		}
		if value != nil && !utf8.ValidString(*value) {
			return nil, Diagnostic{"models.json", owner, "thinkingLevelMap contains an invalid value"}
		}
		if value != nil {
			copy := *value
			result[level] = &copy
		} else {
			result[level] = nil
		}
	}
	return result, nil
}

func decodeInputKinds(raw json.RawMessage, owner string) ([]provider.InputKind, error) {
	if raw == nil {
		return []provider.InputKind{provider.InputText}, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, Diagnostic{"models.json", owner, "input must be an array"}
	}
	result := make([]provider.InputKind, len(values))
	for index, value := range values {
		kind := provider.InputKind(value)
		if kind != provider.InputText && kind != provider.InputImage {
			return nil, Diagnostic{"models.json", owner, "input contains an invalid value"}
		}
		result[index] = kind
	}
	return result, nil
}

func decodeCost(raw json.RawMessage, owner string) (provider.CostRates, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return provider.CostRates{}, Diagnostic{"models.json", owner, "cost must contain all rates"}
	}
	for _, name := range []string{"input", "output", "cacheRead", "cacheWrite"} {
		if _, ok := fields[name]; !ok {
			return provider.CostRates{}, Diagnostic{"models.json", owner, "cost must contain all rates"}
		}
	}
	input, err := decodeRate(fields["input"], owner)
	if err != nil {
		return provider.CostRates{}, Diagnostic{"models.json", owner, "cost must contain numeric rates"}
	}
	output, err := decodeRate(fields["output"], owner)
	if err != nil {
		return provider.CostRates{}, Diagnostic{"models.json", owner, "cost must contain numeric rates"}
	}
	cacheRead, err := decodeRate(fields["cacheRead"], owner)
	if err != nil {
		return provider.CostRates{}, Diagnostic{"models.json", owner, "cost must contain numeric rates"}
	}
	cacheWrite, err := decodeRate(fields["cacheWrite"], owner)
	if err != nil {
		return provider.CostRates{}, Diagnostic{"models.json", owner, "cost must contain numeric rates"}
	}
	var tiers []provider.CostTier
	if rawTiers, present := fields["tiers"]; present {
		tiers, err = decodeCostTiers(rawTiers, owner)
		if err != nil {
			return provider.CostRates{}, err
		}
	}
	return provider.CostRates{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite, Tiers: tiers}, nil
}

func decodeCostOverride(raw json.RawMessage, owner string) (modelCostOverride, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return modelCostOverride{}, Diagnostic{"models.json", owner, "cost override must be an object"}
	}
	result := modelCostOverride{}
	for key, value := range fields {
		switch key {
		case "input":
			parsed, err := decodeRate(value, owner)
			if err != nil {
				return result, err
			}
			result.Input = &parsed
		case "output":
			parsed, err := decodeRate(value, owner)
			if err != nil {
				return result, err
			}
			result.Output = &parsed
		case "cacheRead":
			parsed, err := decodeRate(value, owner)
			if err != nil {
				return result, err
			}
			result.CacheRead = &parsed
		case "cacheWrite":
			parsed, err := decodeRate(value, owner)
			if err != nil {
				return result, err
			}
			result.CacheWrite = &parsed
		case "tiers":
			tiers, err := decodeCostTiers(value, owner)
			if err != nil {
				return result, err
			}
			result.Tiers = &tiers
		default:
			// Preserve the raw models.json file and ignore unknown overlay keys,
			// matching the open TypeBox object used by pi.
		}
	}
	return result, nil
}

func decodeCostTiers(raw json.RawMessage, owner string) ([]provider.CostTier, error) {
	var wire []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil || wire == nil {
		return nil, Diagnostic{"models.json", owner, "cost tiers must be an array"}
	}
	result := make([]provider.CostTier, len(wire))
	for index, fields := range wire {
		for _, name := range []string{"inputTokensAbove", "input", "output", "cacheRead", "cacheWrite"} {
			if _, present := fields[name]; !present {
				return nil, Diagnostic{"models.json", owner, "cost tiers must contain all rates"}
			}
		}
		threshold, err := decodeRate(fields["inputTokensAbove"], owner)
		if err != nil {
			return nil, Diagnostic{"models.json", owner, "cost tier threshold must be a number"}
		}
		input, err := decodeRate(fields["input"], owner)
		if err != nil {
			return nil, Diagnostic{"models.json", owner, "cost tiers must contain numeric rates"}
		}
		output, err := decodeRate(fields["output"], owner)
		if err != nil {
			return nil, Diagnostic{"models.json", owner, "cost tiers must contain numeric rates"}
		}
		cacheRead, err := decodeRate(fields["cacheRead"], owner)
		if err != nil {
			return nil, Diagnostic{"models.json", owner, "cost tiers must contain numeric rates"}
		}
		cacheWrite, err := decodeRate(fields["cacheWrite"], owner)
		if err != nil {
			return nil, Diagnostic{"models.json", owner, "cost tiers must contain numeric rates"}
		}
		result[index] = provider.CostTier{InputTokensAbove: threshold, Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite}
	}
	return result, nil
}

func decodeRate(raw json.RawMessage, owner string) (float64, error) {
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil || !validRate(value) {
		return 0, Diagnostic{"models.json", owner, "cost override rates must be numbers"}
	}
	return value, nil
}

func validRate(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func decodeCompat(raw json.RawMessage, owner, api string) (provider.ModelCompat, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return provider.ModelCompat{}, Diagnostic{"models.json", owner, "compat must be an object"}
	}
	// ProviderCompatSchema is a union of three open TypeBox objects. Only fields
	// common to every branch are unconditionally constrained; currently that is
	// supportsLongCacheRetention. Other forward/branch-specific values remain in
	// Additional and are projected into typed fields only when valid.
	if err := validateCompatBoolFields(object, owner, "supportsLongCacheRetention"); err != nil {
		return provider.ModelCompat{}, err
	}
	if api == "anthropic-messages" {
		return provider.ModelCompat{
			AnthropicMessages: &provider.AnthropicMessagesCompat{
				SupportsEagerToolInputStreaming: compatBool(object, "supportsEagerToolInputStreaming"),
				SupportsLongCacheRetention:      compatBool(object, "supportsLongCacheRetention"),
				SendSessionAffinityHeaders:      compatBool(object, "sendSessionAffinityHeaders"),
				SupportsCacheControlOnTools:     compatBool(object, "supportsCacheControlOnTools"),
				SupportsTemperature:             compatBool(object, "supportsTemperature"),
				ForceAdaptiveThinking:           compatBool(object, "forceAdaptiveThinking"),
				AllowEmptySignature:             compatBool(object, "allowEmptySignature"),
				SupportsStrictTools:             compatBool(object, "supportsStrictTools"),
				SupportsToolReferences:          compatBool(object, "supportsToolReferences"),
			},
			Additional: compatAdditional(api, raw),
		}, nil
	}
	if api == "bedrock-converse-stream" {
		return provider.ModelCompat{
			Bedrock:    &provider.BedrockCompat{SupportsStrictMode: compatBool(object, "supportsStrictMode")},
			Additional: compatAdditional(api, raw),
		}, nil
	}
	if api != OpenAICompletionsAPI && api != OpenAIResponsesAPI && api != AzureOpenAIResponsesAPI && api != OpenAICodexResponsesAPI {
		return provider.ModelCompat{Additional: map[string]json.RawMessage{api: bytes.Clone(raw)}}, nil
	}
	supportsDeveloperRole := compatBool(object, "supportsDeveloperRole")
	sessionAffinityFormat := compatString(object, "sessionAffinityFormat", "openai", "openai-nosession", "openrouter")
	supportsLongCacheRetention := compatBool(object, "supportsLongCacheRetention")
	supportsStrictMode := compatBool(object, "supportsStrictMode")
	supportsOpenAIGrammarTools := compatBool(object, "supportsOpenAIGrammarTools")
	return provider.ModelCompat{
		OpenAIResponses: &provider.OpenAIResponsesCompat{
			SupportsDeveloperRole:           supportsDeveloperRole,
			SessionAffinityFormat:           sessionAffinityFormat,
			SupportsLongCacheRetention:      supportsLongCacheRetention,
			SupportsStrictMode:              supportsStrictMode,
			SupportsOpenAIGrammarTools:      supportsOpenAIGrammarTools,
			SupportsToolSearch:              compatBool(object, "supportsToolSearch"),
			SupportsExplicitPromptCacheMode: compatBool(object, "supportsExplicitPromptCacheMode"),
		},
		OpenAICompletions: &provider.OpenAICompletionsCompat{
			SupportsStore:                               compatBool(object, "supportsStore"),
			SupportsDeveloperRole:                       supportsDeveloperRole,
			SupportsReasoningEffort:                     compatBool(object, "supportsReasoningEffort"),
			SupportsUsageInStreaming:                    compatBool(object, "supportsUsageInStreaming"),
			SupportsFinishReason:                        compatBool(object, "supportsFinishReason"),
			MaxTokensField:                              compatString(object, "maxTokensField", "max_completion_tokens", "max_tokens"),
			RequiresToolResultName:                      compatBool(object, "requiresToolResultName"),
			RequiresAssistantAfterToolResult:            compatBool(object, "requiresAssistantAfterToolResult"),
			RequiresThinkingAsText:                      compatBool(object, "requiresThinkingAsText"),
			RequiresReasoningContentOnAssistantMessages: compatBool(object, "requiresReasoningContentOnAssistantMessages"),
			ThinkingFormat:                              compatString(object, "thinkingFormat", "openai", "openrouter", "together", "deepseek", "zai", "qwen", "chat-template", "qwen-chat-template", "string-thinking", "ant-ling"),
			SupportsOpenAIGrammarTools:                  supportsOpenAIGrammarTools,
			SupportsStrictMode:                          supportsStrictMode,
			SendSessionAffinityHeaders:                  compatBool(object, "sendSessionAffinityHeaders"),
			SessionAffinityFormat:                       sessionAffinityFormat,
			SupportsLongCacheRetention:                  supportsLongCacheRetention,
			CacheControlFormat:                          compatString(object, "cacheControlFormat", "anthropic"),
			DeferredToolsMode:                           compatString(object, "deferredToolsMode", "kimi"),
			ZaiToolStream:                               compatBool(object, "zaiToolStream"),
			ChatTemplateKwargs:                          compatObject(object, "chatTemplateKwargs"),
			OpenRouterRouting:                           compatObject(object, "openRouterRouting"),
			VercelGatewayRouting:                        compatObject(object, "vercelGatewayRouting"),
		},
		Additional: compatAdditional(api, raw),
	}, nil
}

func compatBool(object map[string]json.RawMessage, field string) *bool {
	raw, ok := object[field]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return &value
}

func compatString(object map[string]json.RawMessage, field string, allowed ...string) *string {
	raw, ok := object[field]
	if !ok {
		return nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	for _, candidate := range allowed {
		if value == candidate {
			return &value
		}
	}
	return nil
}

func compatObject(object map[string]json.RawMessage, field string) map[string]any {
	raw, ok := object[field]
	if !ok {
		return nil
	}
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return nil
	}
	return value
}

func compatAdditional(api string, raw json.RawMessage) map[string]json.RawMessage {
	if api == "" || len(raw) == 0 {
		return nil
	}
	return map[string]json.RawMessage{api: bytes.Clone(raw)}
}

func validateCompatBoolFields(object map[string]json.RawMessage, owner string, fields ...string) error {
	for _, field := range fields {
		raw, ok := object[field]
		if !ok {
			continue
		}
		var value bool
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
			return Diagnostic{"models.json", owner + ".compat." + field, "must be a boolean"}
		}
	}
	return nil
}

func readRawObject(path string, jsonc bool, label string) (map[string]json.RawMessage, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: read %s", ErrInvalidConfig, label)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, Diagnostic{label, "root", "must be a regular file"}
	}
	// Do not reject settings.json or models.json based on Unix permission bits.
	// Upstream pi accepts user-managed files created with the process umask.
	if info.Size() > maxFileBytes {
		return nil, false, Diagnostic{label, "root", "exceeds size limit"}
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil || len(data) > maxFileBytes || !utf8.Valid(data) {
		return nil, false, Diagnostic{label, "root", "is unreadable or invalid UTF-8"}
	}
	if jsonc {
		data = normalizeJSONC(data)
	}
	root, err := decodeObject(data)
	if err != nil {
		return nil, false, Diagnostic{label, "root", "is not strict JSON"}
	}
	return root, true, nil
}
func decodeObject(data []byte) (map[string]json.RawMessage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil || root == nil {
		return nil, errors.New("not object")
	}
	return root, nil
}
func normalizeJSONC(data []byte) []byte {
	out := append([]byte(nil), data...)
	in, esc := false, false
	for i := 0; i < len(out); i++ {
		c := out[i]
		if in {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				in = false
			}
			continue
		}
		if c == '"' {
			in = true
			continue
		}
		if c == '/' && i+1 < len(out) && out[i+1] == '/' {
			out[i], out[i+1] = ' ', ' '
			i += 2
			for i < len(out) && out[i] != '\n' && out[i] != '\r' {
				out[i] = ' '
				i++
			}
		}
	}
	in, esc = false, false
	for i := 0; i < len(out); i++ {
		if in {
			if esc {
				esc = false
			} else if out[i] == '\\' {
				esc = true
			} else if out[i] == '"' {
				in = false
			}
			continue
		}
		if out[i] == '"' {
			in = true
			continue
		}
		if out[i] == ',' {
			j := i + 1
			for j < len(out) && (out[j] == ' ' || out[j] == '\t' || out[j] == '\r' || out[j] == '\n') {
				j++
			}
			if j < len(out) && (out[j] == '}' || out[j] == ']') {
				out[i] = ' '
			}
		}
	}
	return out
}

type atomicWriteFaults struct {
	beforeRename  func() error
	afterRename   func() error
	syncDirectory func(string) error
}

func atomicWrite(ctx context.Context, path string, data []byte, label string, faults atomicWriteFaults) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("%w: %s", ErrPersistence, label)
	}
	if err := contextCause(ctx); err != nil {
		return err
	}
	if err := requireExistingDirectory(filepath.Dir(path), label+" directory"); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".model-config-*")
	if err != nil {
		return fmt.Errorf("%w: create %s temporary file", ErrInvalidConfig, label)
	}
	name := f.Name()
	published := false
	defer func() {
		if !published {
			// Preserve an unsuccessful temporary write for recovery/inspection.
			// We deliberately never delete configuration artifacts in place.
			_ = os.Rename(name, filepath.Join(os.TempDir(), "pi-go-"+filepath.Base(name)))
		}
	}()
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("%w: write %s", ErrInvalidConfig, label)
	}
	if err := contextCause(ctx); err != nil {
		return err
	}
	if faults.beforeRename != nil {
		if err := faults.beforeRename(); err != nil {
			return fmt.Errorf("%w: publish %s", ErrInvalidConfig, label)
		}
	}
	if err = os.Rename(name, path); err != nil {
		return fmt.Errorf("%w: publish %s", ErrInvalidConfig, label)
	}
	published = true
	if faults.afterRename != nil {
		if err := faults.afterRename(); err != nil {
			return fmt.Errorf("%w: %s", ErrCommitUnknown, label)
		}
	}
	syncDirectory := faults.syncDirectory
	if syncDirectory == nil {
		syncDirectory = syncModelDirectory
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: %s", ErrCommitUnknown, label)
	}
	return nil
}

// Mutation requires an already-created parent directory. Files are created
// mode 0600 and admitted as private. This mirrors session's
// durable-parent precondition: atomically publishing one leaf cannot make a
// newly-created ancestor durable without syncing every ancestor's parent.
func requireExistingDirectory(path, label string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w: %s must already exist", ErrPersistence, label)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrPersistence, label)
	}
	return nil
}

func syncModelDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		return errors.Join(err, directory.Close())
	}
	return directory.Close()
}

// acquireFileLock deliberately does not reclaim a stale lock. Guessing that a
// suspended process is dead risks overwriting a concurrent user's settings.
func acquireFileLock(ctx context.Context, path string) (func(), error) {
	if err := contextCause(ctx); err != nil {
		return nil, err
	}
	lock := path + ".lock"
	for {
		if err := os.Mkdir(lock, 0o700); err == nil {
			return func() { _ = os.Remove(lock) }, nil
		} else if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: acquire settings lock", ErrInvalidConfig)
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, fmt.Errorf("%w: acquire configuration lock: %w", ErrCancelled, context.Cause(ctx))
		case <-timer.C:
		}
	}
}
func newLocalGate() chan struct{} { gate := make(chan struct{}, 1); gate <- struct{}{}; return gate }
func acquireLocal(ctx context.Context, gate chan struct{}) (func(), error) {
	if err := contextCause(ctx); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: wait for local serialization: %w", ErrCancelled, context.Cause(ctx))
	case <-gate:
		return func() { gate <- struct{}{} }, nil
	}
}
func contextCause(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrInvalidConfig)
	}
	if err := context.Cause(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrCancelled, err)
	}
	return nil
}
func validID(v string) bool { return validValue(v) && !strings.ContainsAny(v, "/\\") }

func modelKey(providerID, modelID string) string { return providerID + "/" + modelID }
func validValue(v string) bool {
	return utf8.ValidString(v) && strings.TrimSpace(v) != "" && !strings.ContainsFunc(v, unicode.IsControl)
}
func mergeHeaders(a, b map[string]string) map[string]string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := cloneHeaders(a)
	if out == nil {
		out = make(map[string]string, len(b))
	}
	for k, v := range b {
		for old := range out {
			if strings.EqualFold(old, k) {
				delete(out, old)
			}
		}
		out[k] = v
	}
	return out
}
func cloneHeaders(v map[string]string) map[string]string {
	if v == nil {
		return nil
	}
	out := make(map[string]string, len(v))
	for k, x := range v {
		out[k] = x
	}
	return out
}
func cloneThinkingMap(v map[provider.ThinkingLevel]*string) map[provider.ThinkingLevel]*string {
	if v == nil {
		return nil
	}
	out := make(map[provider.ThinkingLevel]*string, len(v))
	for key, value := range v {
		if value == nil {
			out[key] = nil
		} else {
			copy := *value
			out[key] = &copy
		}
	}
	return out
}
func cloneCompat(v provider.ModelCompat) provider.ModelCompat {
	// Provider owns the full API-specific compatibility union. Keeping the
	// cloning rule there prevents a model-runtime snapshot from silently
	// dropping compat for an adapter not implemented in this binary yet.
	return provider.CloneModelCompat(v)
}

func mergeCompat(base, override provider.ModelCompat) provider.ModelCompat {
	result := cloneCompat(base)
	copyBool := func(target **bool, value *bool) {
		if value != nil {
			copy := *value
			*target = &copy
		}
	}
	copyString := func(target **string, value *string) {
		if value != nil {
			copy := *value
			*target = &copy
		}
	}
	if value := override.OpenAIResponses; value != nil {
		if result.OpenAIResponses == nil {
			result.OpenAIResponses = &provider.OpenAIResponsesCompat{}
		}
		target := result.OpenAIResponses
		copyBool(&target.SupportsDeveloperRole, value.SupportsDeveloperRole)
		copyBool(&target.SupportsLongCacheRetention, value.SupportsLongCacheRetention)
		copyBool(&target.SupportsStrictMode, value.SupportsStrictMode)
		copyBool(&target.SupportsOpenAIGrammarTools, value.SupportsOpenAIGrammarTools)
		copyBool(&target.SupportsToolSearch, value.SupportsToolSearch)
		copyBool(&target.SupportsExplicitPromptCacheMode, value.SupportsExplicitPromptCacheMode)
		copyString(&target.SessionAffinityFormat, value.SessionAffinityFormat)
	}
	if value := override.OpenAICompletions; value != nil {
		if result.OpenAICompletions == nil {
			result.OpenAICompletions = &provider.OpenAICompletionsCompat{}
		}
		target := result.OpenAICompletions
		copyBool(&target.SupportsStore, value.SupportsStore)
		copyBool(&target.SupportsDeveloperRole, value.SupportsDeveloperRole)
		copyBool(&target.SupportsReasoningEffort, value.SupportsReasoningEffort)
		copyBool(&target.SupportsUsageInStreaming, value.SupportsUsageInStreaming)
		copyBool(&target.SupportsFinishReason, value.SupportsFinishReason)
		copyString(&target.MaxTokensField, value.MaxTokensField)
		copyBool(&target.RequiresToolResultName, value.RequiresToolResultName)
		copyBool(&target.RequiresAssistantAfterToolResult, value.RequiresAssistantAfterToolResult)
		copyBool(&target.RequiresThinkingAsText, value.RequiresThinkingAsText)
		copyBool(&target.RequiresReasoningContentOnAssistantMessages, value.RequiresReasoningContentOnAssistantMessages)
		copyString(&target.ThinkingFormat, value.ThinkingFormat)
		copyBool(&target.SupportsOpenAIGrammarTools, value.SupportsOpenAIGrammarTools)
		copyBool(&target.SupportsStrictMode, value.SupportsStrictMode)
		copyBool(&target.SendSessionAffinityHeaders, value.SendSessionAffinityHeaders)
		copyString(&target.SessionAffinityFormat, value.SessionAffinityFormat)
		copyBool(&target.SupportsLongCacheRetention, value.SupportsLongCacheRetention)
		copyString(&target.CacheControlFormat, value.CacheControlFormat)
		copyString(&target.DeferredToolsMode, value.DeferredToolsMode)
		copyBool(&target.ZaiToolStream, value.ZaiToolStream)
		if value.ChatTemplateKwargs != nil {
			target.ChatTemplateKwargs = mergeCompatObject(target.ChatTemplateKwargs, value.ChatTemplateKwargs)
		}
		if value.OpenRouterRouting != nil {
			target.OpenRouterRouting = mergeCompatObject(target.OpenRouterRouting, value.OpenRouterRouting)
		}
		if value.VercelGatewayRouting != nil {
			target.VercelGatewayRouting = mergeCompatObject(target.VercelGatewayRouting, value.VercelGatewayRouting)
		}
	}
	if value := override.AnthropicMessages; value != nil {
		if result.AnthropicMessages == nil {
			result.AnthropicMessages = &provider.AnthropicMessagesCompat{}
		}
		target := result.AnthropicMessages
		copyBool(&target.SupportsEagerToolInputStreaming, value.SupportsEagerToolInputStreaming)
		copyBool(&target.SupportsLongCacheRetention, value.SupportsLongCacheRetention)
		copyBool(&target.SendSessionAffinityHeaders, value.SendSessionAffinityHeaders)
		copyBool(&target.SupportsCacheControlOnTools, value.SupportsCacheControlOnTools)
		copyBool(&target.SupportsTemperature, value.SupportsTemperature)
		copyBool(&target.ForceAdaptiveThinking, value.ForceAdaptiveThinking)
		copyBool(&target.AllowEmptySignature, value.AllowEmptySignature)
		copyBool(&target.SupportsStrictTools, value.SupportsStrictTools)
		copyBool(&target.SupportsToolReferences, value.SupportsToolReferences)
	}
	if value := override.Bedrock; value != nil {
		if result.Bedrock == nil {
			result.Bedrock = &provider.BedrockCompat{}
		}
		copyBool(&result.Bedrock.SupportsStrictMode, value.SupportsStrictMode)
	}
	if override.Additional != nil {
		if result.Additional == nil {
			result.Additional = map[string]json.RawMessage{}
		}
		for key, value := range override.Additional {
			_, baseHasRawProjection := base.Additional[key]
			result.Additional[key] = mergeCompatRaw(result.Additional[key], value)
			if baseHasRawProjection {
				reprojectCompat(&result, key, result.Additional[key])
			}
		}
	}
	return result
}

func reprojectCompat(target *provider.ModelCompat, api string, raw json.RawMessage) {
	projected, err := decodeCompat(raw, "compat", api)
	if err != nil {
		return
	}
	switch api {
	case OpenAICompletionsAPI:
		target.OpenAICompletions = projected.OpenAICompletions
	case OpenAIResponsesAPI, AzureOpenAIResponsesAPI, OpenAICodexResponsesAPI:
		target.OpenAIResponses = projected.OpenAIResponses
	case AnthropicMessagesAPI:
		target.AnthropicMessages = projected.AnthropicMessages
	case "bedrock-converse-stream":
		target.Bedrock = projected.Bedrock
	}
}

func mergeCompatObject(base, override map[string]any) map[string]any {
	result := provider.CloneJSONMap(base)
	if result == nil {
		result = make(map[string]any, len(override))
	}
	for key, value := range provider.CloneJSONMap(override) {
		result[key] = value
	}
	return result
}

func mergeCompatRaw(base, override json.RawMessage) json.RawMessage {
	if len(base) == 0 {
		return bytes.Clone(override)
	}
	var baseObject, overrideObject map[string]json.RawMessage
	if json.Unmarshal(base, &baseObject) != nil || json.Unmarshal(override, &overrideObject) != nil || baseObject == nil || overrideObject == nil {
		return bytes.Clone(override)
	}
	for key, value := range overrideObject {
		switch key {
		case "openRouterRouting", "vercelGatewayRouting", "chatTemplateKwargs":
			var baseNested, overrideNested map[string]json.RawMessage
			baseIsObject := json.Unmarshal(baseObject[key], &baseNested) == nil && baseNested != nil
			overrideIsObject := json.Unmarshal(value, &overrideNested) == nil && overrideNested != nil
			if baseIsObject || overrideIsObject {
				mergedNested := make(map[string]json.RawMessage, len(baseNested)+len(overrideNested))
				for nestedKey, nestedValue := range baseNested {
					mergedNested[nestedKey] = bytes.Clone(nestedValue)
				}
				for nestedKey, nestedValue := range overrideNested {
					mergedNested[nestedKey] = bytes.Clone(nestedValue)
				}
				if encoded, err := json.Marshal(mergedNested); err == nil {
					baseObject[key] = encoded
					continue
				}
			}
		}
		baseObject[key] = bytes.Clone(value)
	}
	merged, err := json.Marshal(baseObject)
	if err != nil {
		return bytes.Clone(override)
	}
	return merged
}
func cloneModel(m Model) Model {
	m.Headers = cloneHeaders(m.Headers)
	m.Input = cloneInputKinds(m.Input)
	m.ThinkingLevelMap = cloneThinkingMap(m.ThinkingLevelMap)
	m.Compat = cloneCompat(m.Compat)
	m.Cost.Tiers = append([]provider.CostTier(nil), m.Cost.Tiers...)
	m.UnsupportedFields = append([]string(nil), m.UnsupportedFields...)
	m.UnknownFields = append([]string(nil), m.UnknownFields...)
	m.compatRaw = bytes.Clone(m.compatRaw)
	return m
}
func cloneProvider(p ProviderConfig) ProviderConfig {
	p.Headers = cloneHeaders(p.Headers)
	p.Compat = cloneCompat(p.Compat)
	p.AuthHeader = cloneBoolPointer(p.AuthHeader)
	p.Models = append([]Model(nil), p.Models...)
	p.APIKeyEnvironment = append([]string(nil), p.APIKeyEnvironment...)
	p.UnknownFields = append([]string(nil), p.UnknownFields...)
	p.UnsupportedFields = append([]string(nil), p.UnsupportedFields...)
	p.compatRaw = bytes.Clone(p.compatRaw)
	if p.overrides != nil {
		sourceOverrides := p.overrides
		p.overrides = make(map[string]modelOverride, len(sourceOverrides))
		for k, v := range sourceOverrides {
			if v.Name != nil {
				x := *v.Name
				v.Name = &x
			}
			v.Reasoning = cloneBoolPointer(v.Reasoning)
			v.ThinkingLevelMap = cloneThinkingMap(v.ThinkingLevelMap)
			if v.Input != nil {
				input := cloneInputKinds(*v.Input)
				v.Input = &input
			}
			if v.Cost != nil {
				cost := *v.Cost
				cost.Input = cloneFloat64Pointer(cost.Input)
				cost.Output = cloneFloat64Pointer(cost.Output)
				cost.CacheRead = cloneFloat64Pointer(cost.CacheRead)
				cost.CacheWrite = cloneFloat64Pointer(cost.CacheWrite)
				if cost.Tiers != nil {
					tiers := append([]provider.CostTier(nil), (*cost.Tiers)...)
					cost.Tiers = &tiers
				}
				v.Cost = &cost
			}
			v.ContextWindow = cloneUint64Pointer(v.ContextWindow)
			v.MaxTokens = cloneUint64Pointer(v.MaxTokens)
			v.Headers = cloneHeaders(v.Headers)
			v.Compat = cloneCompat(v.Compat)
			v.UnsupportedFields = append([]string(nil), v.UnsupportedFields...)
			v.UnknownFields = append([]string(nil), v.UnknownFields...)
			v.compatRaw = bytes.Clone(v.compatRaw)
			p.overrides[k] = v
		}
	}
	for i := range p.Models {
		p.Models[i] = cloneModel(p.Models[i])
	}
	if p.ConfiguredAPIKey != nil {
		v := *p.ConfiguredAPIKey
		p.ConfiguredAPIKey = &v
	}
	return p
}

func cloneFloat64Pointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func appendUnique(base []string, values ...string) []string {
	seen := make(map[string]struct{}, len(base)+len(values))
	for _, v := range base {
		seen[v] = struct{}{}
	}
	for _, v := range values {
		if _, ok := seen[v]; !ok {
			base = append(base, v)
			seen[v] = struct{}{}
		}
	}
	sort.Strings(base)
	return base
}
func cloneSettings(s Settings) Settings {
	s.Skills = cloneStringSlice(s.Skills)
	s.Prompts = cloneStringSlice(s.Prompts)
	s.EnabledModels = append([]string(nil), s.EnabledModels...)
	s.Compaction.Enabled = cloneBoolPointer(s.Compaction.Enabled)
	s.Compaction.ReserveTokens = cloneUint64Pointer(s.Compaction.ReserveTokens)
	s.Compaction.KeepRecentTokens = cloneUint64Pointer(s.Compaction.KeepRecentTokens)
	s.BranchSummary.ReserveTokens = cloneUint64Pointer(s.BranchSummary.ReserveTokens)
	s.BranchSummary.SkipPrompt = cloneBoolPointer(s.BranchSummary.SkipPrompt)
	s.Images.AutoResize = cloneBoolPointer(s.Images.AutoResize)
	s.Images.BlockImages = cloneBoolPointer(s.Images.BlockImages)
	s.ThinkingBudgets.Minimal = cloneUint64Pointer(s.ThinkingBudgets.Minimal)
	s.ThinkingBudgets.Low = cloneUint64Pointer(s.ThinkingBudgets.Low)
	s.ThinkingBudgets.Medium = cloneUint64Pointer(s.ThinkingBudgets.Medium)
	s.ThinkingBudgets.High = cloneUint64Pointer(s.ThinkingBudgets.High)
	s.Retry.Enabled = cloneBoolPointer(s.Retry.Enabled)
	s.Retry.MaxRetries = cloneUint64Pointer(s.Retry.MaxRetries)
	s.Retry.BaseDelayMS = cloneUint64Pointer(s.Retry.BaseDelayMS)
	s.Retry.Provider.TimeoutMS = cloneUint64Pointer(s.Retry.Provider.TimeoutMS)
	s.Retry.Provider.MaxRetries = cloneUint64Pointer(s.Retry.Provider.MaxRetries)
	s.Retry.Provider.MaxRetryDelayMS = cloneUint64Pointer(s.Retry.Provider.MaxRetryDelayMS)
	s.HTTPIdleTimeoutMS = cloneUint64Pointer(s.HTTPIdleTimeoutMS)
	s.WebsocketConnectTimeoutMS = cloneUint64Pointer(s.WebsocketConnectTimeoutMS)
	return s
}

func cloneStringSlice(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}

func cloneInputKinds(values []provider.InputKind) []provider.InputKind {
	if values == nil {
		return nil
	}
	return append([]provider.InputKind{}, values...)
}

func cloneProviderRetrySettings(value ProviderRetrySettings) ProviderRetrySettings {
	value.TimeoutMS = cloneUint64Pointer(value.TimeoutMS)
	value.MaxRetries = cloneUint64Pointer(value.MaxRetries)
	value.MaxRetryDelayMS = cloneUint64Pointer(value.MaxRetryDelayMS)
	return value
}

func cloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneUint64Pointer(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func cloneSnapshot(s Snapshot) Snapshot {
	s.Models = append([]Model(nil), s.Models...)
	for i := range s.Models {
		s.Models[i] = cloneModel(s.Models[i])
	}
	s.Providers = append([]string(nil), s.Providers...)
	s.Settings = cloneSettings(s.Settings)
	return s
}

// Package model owns the credential-blind, process-local model catalog and
// settings projection used by product assembly.  It deliberately does not
// contact a catalog service or resolve credentials: those are separate trust
// boundaries owned by future catalog work and internal/auth respectively.
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
	OpenAICodexProviderID     = "openai-codex"
	AnthropicProviderID       = "anthropic"
	OpenAIResponsesAPI        = "openai-responses"
	OpenAICompletionsAPI      = "openai-completions"
	OpenAICodexResponsesAPI   = "openai-codex-responses"
	AnthropicMessagesAPI      = "anthropic-messages"
	DefaultOpenAIModel        = "gpt-5.5"
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
	ErrUnsafeMode    = errors.New("configuration file permissions are unsafe")
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
	ConfiguredAPIKey  *string
	Models            []Model
	UnknownFields     []string
	UnsupportedFields []string
	overrides         map[string]modelOverride
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
}

type modelCostOverride struct {
	Input, Output, CacheRead, CacheWrite *float64
	Tiers                                *[]provider.CostTier
}

type Settings struct {
	DefaultProvider           string
	DefaultModel              string
	DefaultThinkingLevel      provider.ThinkingLevel
	Transport                 provider.Transport
	SteeringMode              string
	FollowUpMode              string
	EnabledModels             []string
	Compaction                CompactionSettings
	BranchSummary             BranchSummarySettings
	Retry                     RetrySettings
	HTTPIdleTimeoutMS         *uint64
	WebsocketConnectTimeoutMS *uint64
	transportPresent          bool
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
	// ModelsStorePath is the optional, local catalog cache. It is read into the
	// snapshot, but never refreshed by this package.
	ModelsStorePath string
	// ProjectTrusted is deliberately opt-in. A project .pi/settings.json is not
	// read merely because it exists; a formal trust decision is deferred.
	ProjectTrusted bool
}

type Runtime struct {
	options     Options
	mu          sync.RWMutex
	local       chan struct{}
	snapshot    Snapshot
	providers   map[string]ProviderConfig
	storeErrors map[string]error
	faults      atomicWriteFaults
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
	r := &Runtime{options: options, local: newLocalGate(), storeErrors: make(map[string]error)}
	if err := r.Reload(context.Background()); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Runtime) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneSnapshot(r.snapshot)
}
func (r *Runtime) Provider(id string) (ProviderConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[canonicalKey(id)]
	return cloneProvider(p), ok
}

// ValidateRoute is the selected-route consumption boundary. Like pi's TypeBox
// models.json schema, unknown extra object members are preserved by the source
// file but ignored by composition. Only recognized, unsupported features and
// an API without a registered Go adapter make an otherwise valid route fail.
func (r *Runtime) ValidateRoute(selected Model) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	providerID := canonicalKey(selected.Provider)
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
	switch selected.API {
	case OpenAIResponsesAPI, OpenAICompletionsAPI, OpenAICodexResponsesAPI, AnthropicMessagesAPI:
	default:
		return fmt.Errorf("%w: selected model uses an unimplemented API", ErrUnsupported)
	}
	return nil
}

// Reload is transactional: malformed replacement files leave the last healthy
// snapshot published. A missing optional file is a healthy empty source.
func (r *Runtime) Reload(ctx context.Context) error {
	releaseLocal, err := acquireLocal(ctx, r.local)
	if err != nil {
		return err
	}
	defer releaseLocal()
	settings, err := loadSettings(filepath.Join(r.options.AgentDir, "settings.json"), "global settings.json")
	if err != nil {
		return err
	}
	if r.options.ProjectTrusted {
		project, err := loadSettings(filepath.Join(r.options.WorkingDir, ".pi", "settings.json"), "project settings.json")
		if err != nil {
			return err
		}
		settings = mergeSettings(settings, project)
	}
	providers, err := loadModels(filepath.Join(r.options.AgentDir, "models.json"))
	if err != nil {
		return err
	}
	providers = composeProviderConfigs(providers)
	cached, storeErrors, err := loadStoreCatalogs(r.options.ModelsStorePath)
	if err != nil {
		return err
	}
	snapshot := buildSnapshot(providers, cached, settings)
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot.Generation = r.snapshot.Generation + 1
	r.providers = providers
	r.storeErrors = storeErrors
	r.snapshot = snapshot
	return nil
}

func composeProviderConfigs(configured map[string]ProviderConfig) map[string]ProviderConfig {
	result := make(map[string]ProviderConfig, len(configured)+3)
	for id, value := range configured {
		result[id] = cloneProvider(value)
	}
	defaults := []ProviderConfig{
		{ID: OpenAIProviderID, Name: "OpenAI", API: OpenAIResponsesAPI, BaseURL: defaultOpenAIBaseURL},
		{ID: OpenAICodexProviderID, Name: "OpenAI Codex", API: OpenAICodexResponsesAPI, BaseURL: defaultOpenAICodexBaseURL},
		{ID: AnthropicProviderID, Name: "Anthropic", API: AnthropicMessagesAPI, BaseURL: defaultAnthropicBaseURL},
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
		result[fallback.ID] = current
	}
	return result
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
	settings := current
	if r.options.ProjectTrusted {
		project, e := loadSettings(filepath.Join(r.options.WorkingDir, ".pi", "settings.json"), "project settings.json")
		if e != nil {
			return e
		}
		settings = mergeSettings(settings, project)
	}
	cached, storeErrors, err := loadStoreCatalogs(r.options.ModelsStorePath)
	if err != nil {
		return err
	}
	putString(root, "defaultProvider", current.DefaultProvider)
	putString(root, "defaultModel", current.DefaultModel)
	putString(root, "defaultThinkingLevel", string(current.DefaultThinkingLevel))
	putString(root, "transport", string(current.Transport))
	putString(root, "steeringMode", current.SteeringMode)
	putString(root, "followUpMode", current.FollowUpMode)
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

// Resolve is a credential-blind compatibility entry point. Explicit selections
// use the same CLI resolver as pi; implicit selections use the same new-session
// scope/settings/provider-default order. Production code that knows credentials
// and registered routes must call ResolveInitialModel with real predicates.
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
	configured, ok := r.providers[canonicalKey(selected.Provider)]
	if !ok {
		return selected
	}
	override, ok := configured.overrides[canonicalKey(selected.ID)]
	if !ok {
		return selected
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
		providerSet[canonicalKey(builtin.Provider)] = struct{}{}
	}
	for id := range providers {
		providerSet[canonicalKey(id)] = struct{}{}
	}
	for id, entry := range cached {
		providerSet[canonicalKey(id)] = struct{}{}
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
		// Provider baseUrl/compat change model metadata. Provider headers remain
		// in auth composition and are resolved at request time; only static and
		// per-model headers belong to Model.headers in pi.
		for key, base := range byKey {
			if canonicalKey(base.Provider) != id {
				continue
			}
			if p.BaseURL != "" {
				base.BaseURL = p.BaseURL
			}
			base.Compat = mergeCompat(base.Compat, p.Compat)
			byKey[key] = base
		}
		for _, m := range p.Models {
			key := modelKey(p.ID, m.ID)
			base := byKey[key]
			if m.API == "" {
				m.API = firstNonEmpty(p.API, base.API, defaultProviderAPI(p.ID))
			}
			if m.BaseURL == "" {
				m.BaseURL = firstNonEmpty(p.BaseURL, base.BaseURL, defaultProviderBaseURL(p.ID))
			}
			if m.Name == "" {
				m.Name = m.ID
			}
			if len(m.Input) == 0 {
				m.Input = []provider.InputKind{provider.InputText}
			}
			if m.ContextWindow == 0 {
				m.ContextWindow = 128_000
			}
			if m.MaxTokens == 0 {
				m.MaxTokens = 16_384
			}
			if m.MaxTokens > m.ContextWindow {
				m.MaxTokens = m.ContextWindow
			}
			m.Compat = mergeCompat(p.Compat, m.Compat)
			m.Provider = p.ID
			byKey[key] = m
		}
		for modelID, override := range p.overrides {
			key := modelKey(p.ID, modelID)
			m, ok := byKey[key]
			if !ok {
				continue
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
	switch canonicalKey(providerID) {
	case OpenAIProviderID:
		return OpenAIResponsesAPI
	case OpenAICodexProviderID:
		return OpenAICodexResponsesAPI
	case AnthropicProviderID:
		return AnthropicMessagesAPI
	default:
		return ""
	}
}

func defaultProviderBaseURL(providerID string) string {
	switch canonicalKey(providerID) {
	case OpenAIProviderID:
		return defaultOpenAIBaseURL
	case OpenAICodexProviderID:
		return defaultOpenAICodexBaseURL
	case AnthropicProviderID:
		return defaultAnthropicBaseURL
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
		model.Input = append([]provider.InputKind(nil), (*override.Input)...)
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
		canonical := canonicalKey(id)
		if _, duplicate := result[canonical]; duplicate {
			return nil, Diagnostic{"models.json", "providers", "contains case-fold duplicate provider id"}
		}
		p, err := parseProvider(canonical, data)
		if err != nil {
			return nil, err
		}
		result[canonical] = p
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
		compatAPI := firstNonEmpty(p.API, defaultProviderAPI(id))
		if p.Compat, err = decodeCompat(raw, id, compatAPI); err != nil {
			return p, err
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
	if _, present := o["oauth"]; present {
		p.UnsupportedFields = append(p.UnsupportedFields, "oauth")
	}
	if data, ok := o["models"]; ok {
		var models []json.RawMessage
		if err := json.Unmarshal(data, &models); err != nil || models == nil {
			return p, Diagnostic{"models.json", "providers." + id + ".models", "must be an array"}
		}
		seen := map[string]bool{}
		for i, entry := range models {
			m, e := parseModel(id, p.API, i, entry)
			if e != nil {
				return p, e
			}
			if seen[canonicalKey(m.ID)] {
				return p, Diagnostic{"models.json", "providers." + id + ".models", "contains duplicate model id"}
			}
			seen[canonicalKey(m.ID)] = true
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
			canonical := canonicalKey(modelID)
			if _, duplicate := p.overrides[canonical]; duplicate {
				return p, Diagnostic{"models.json", "providers." + id + ".modelOverrides", "contains case-fold duplicate model id"}
			}
			overrideAPI := firstNonEmpty(p.API, defaultProviderAPI(id))
			for _, model := range p.Models {
				if canonicalKey(model.ID) == canonical && model.API != "" {
					overrideAPI = model.API
					break
				}
			}
			override, err := parseOverride(id, modelID, overrideAPI, value)
			if err != nil {
				return p, err
			}
			p.overrides[canonical] = override
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
func requiredString(o map[string]json.RawMessage, key, owner string) (string, bool, error) {
	raw, ok := o[key]
	if !ok {
		return "", false, nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil || !validValue(v) {
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
	for k, v := range h {
		if !validValue(k) || !utf8.ValidString(v) || strings.ContainsFunc(v, unicode.IsControl) {
			return nil, Diagnostic{"models.json", key, "contains an invalid header"}
		}
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
		if value != nil && !validValue(*value) {
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
	if err := json.Unmarshal(raw, &values); err != nil || len(values) == 0 {
		return nil, Diagnostic{"models.json", owner, "input must be a non-empty array"}
	}
	result := make([]provider.InputKind, len(values))
	seen := map[provider.InputKind]bool{}
	for index, value := range values {
		kind := provider.InputKind(value)
		if (kind != provider.InputText && kind != provider.InputImage) || seen[kind] {
			return nil, Diagnostic{"models.json", owner, "input contains an invalid value"}
		}
		seen[kind] = true
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
	var value struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cacheRead"`
		CacheWrite float64 `json:"cacheWrite"`
		Tiers      []struct {
			InputTokensAbove uint64  `json:"inputTokensAbove"`
			Input            float64 `json:"input"`
			Output           float64 `json:"output"`
			CacheRead        float64 `json:"cacheRead"`
			CacheWrite       float64 `json:"cacheWrite"`
		} `json:"tiers"`
	}
	if err := json.Unmarshal(raw, &value); err != nil || value.Input < 0 || value.Output < 0 || value.CacheRead < 0 || value.CacheWrite < 0 {
		return provider.CostRates{}, Diagnostic{"models.json", owner, "cost must contain non-negative rates"}
	}
	tiers := make([]provider.CostTier, len(value.Tiers))
	var previous uint64
	for index, tier := range value.Tiers {
		if (index != 0 && tier.InputTokensAbove <= previous) || tier.Input < 0 || tier.Output < 0 || tier.CacheRead < 0 || tier.CacheWrite < 0 {
			return provider.CostRates{}, Diagnostic{"models.json", owner, "cost tiers must be strictly increasing non-negative rates"}
		}
		previous = tier.InputTokensAbove
		tiers[index] = provider.CostTier{InputTokensAbove: tier.InputTokensAbove, Input: tier.Input, Output: tier.Output, CacheRead: tier.CacheRead, CacheWrite: tier.CacheWrite}
	}
	return provider.CostRates{Input: value.Input, Output: value.Output, CacheRead: value.CacheRead, CacheWrite: value.CacheWrite, Tiers: tiers}, nil
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
			parsed, err := decodeNonNegativeRate(value, owner)
			if err != nil {
				return result, err
			}
			result.Input = &parsed
		case "output":
			parsed, err := decodeNonNegativeRate(value, owner)
			if err != nil {
				return result, err
			}
			result.Output = &parsed
		case "cacheRead":
			parsed, err := decodeNonNegativeRate(value, owner)
			if err != nil {
				return result, err
			}
			result.CacheRead = &parsed
		case "cacheWrite":
			parsed, err := decodeNonNegativeRate(value, owner)
			if err != nil {
				return result, err
			}
			result.CacheWrite = &parsed
		case "tiers":
			var wire []struct {
				InputTokensAbove uint64  `json:"inputTokensAbove"`
				Input            float64 `json:"input"`
				Output           float64 `json:"output"`
				CacheRead        float64 `json:"cacheRead"`
				CacheWrite       float64 `json:"cacheWrite"`
			}
			if err := json.Unmarshal(value, &wire); err != nil || wire == nil {
				return result, Diagnostic{"models.json", owner, "cost tiers must be an array"}
			}
			tiers := make([]provider.CostTier, len(wire))
			var previous uint64
			for index, tier := range wire {
				if (index != 0 && tier.InputTokensAbove <= previous) || !validRate(tier.Input) || !validRate(tier.Output) || !validRate(tier.CacheRead) || !validRate(tier.CacheWrite) {
					return result, Diagnostic{"models.json", owner, "cost tiers must be strictly increasing non-negative rates"}
				}
				previous = tier.InputTokensAbove
				tiers[index] = provider.CostTier{InputTokensAbove: tier.InputTokensAbove, Input: tier.Input, Output: tier.Output, CacheRead: tier.CacheRead, CacheWrite: tier.CacheWrite}
			}
			result.Tiers = &tiers
		default:
			// Preserve the raw models.json file and ignore unknown overlay keys,
			// matching the open TypeBox object used by pi.
		}
	}
	return result, nil
}

func decodeNonNegativeRate(raw json.RawMessage, owner string) (float64, error) {
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil || !validRate(value) {
		return 0, Diagnostic{"models.json", owner, "cost override rates must be non-negative numbers"}
	}
	return value, nil
}

func validRate(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func decodeCompat(raw json.RawMessage, owner, api string) (provider.ModelCompat, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return provider.ModelCompat{}, Diagnostic{"models.json", owner, "compat must be an object"}
	}
	// Provider-level compat is valid even when the provider leaves `api`
	// unspecified and supplies it per model. Validate every currently known
	// compat member before selecting the dialect projection so null or an invalid
	// literal cannot evade the models.json schema through an omitted API. Unknown
	// members remain open, matching TypeBox's default object behavior.
	if err := validateKnownCompatObject(object, owner); err != nil {
		return provider.ModelCompat{}, err
	}
	if api == "anthropic-messages" {
		if err := validateCompatBoolFields(object, owner,
			"supportsEagerToolInputStreaming", "supportsLongCacheRetention", "sendSessionAffinityHeaders",
			"supportsCacheControlOnTools", "supportsTemperature", "forceAdaptiveThinking", "allowEmptySignature",
			"supportsStrictTools", "supportsToolReferences"); err != nil {
			return provider.ModelCompat{}, err
		}
		var wire struct {
			SupportsEagerToolInputStreaming *bool `json:"supportsEagerToolInputStreaming"`
			SupportsLongCacheRetention      *bool `json:"supportsLongCacheRetention"`
			SendSessionAffinityHeaders      *bool `json:"sendSessionAffinityHeaders"`
			SupportsCacheControlOnTools     *bool `json:"supportsCacheControlOnTools"`
			SupportsTemperature             *bool `json:"supportsTemperature"`
			ForceAdaptiveThinking           *bool `json:"forceAdaptiveThinking"`
			AllowEmptySignature             *bool `json:"allowEmptySignature"`
			SupportsStrictTools             *bool `json:"supportsStrictTools"`
			SupportsToolReferences          *bool `json:"supportsToolReferences"`
		}
		if json.Unmarshal(raw, &wire) != nil {
			return provider.ModelCompat{}, Diagnostic{"models.json", owner, "compat must be an object"}
		}
		return provider.ModelCompat{AnthropicMessages: &provider.AnthropicMessagesCompat{SupportsEagerToolInputStreaming: wire.SupportsEagerToolInputStreaming, SupportsLongCacheRetention: wire.SupportsLongCacheRetention, SendSessionAffinityHeaders: wire.SendSessionAffinityHeaders, SupportsCacheControlOnTools: wire.SupportsCacheControlOnTools, SupportsTemperature: wire.SupportsTemperature, ForceAdaptiveThinking: wire.ForceAdaptiveThinking, AllowEmptySignature: wire.AllowEmptySignature, SupportsStrictTools: wire.SupportsStrictTools, SupportsToolReferences: wire.SupportsToolReferences}}, nil
	}
	if api == "bedrock-converse-stream" {
		if err := validateCompatBoolFields(object, owner, "supportsStrictMode"); err != nil {
			return provider.ModelCompat{}, err
		}
		var wire struct {
			SupportsStrictMode *bool `json:"supportsStrictMode"`
		}
		if json.Unmarshal(raw, &wire) != nil {
			return provider.ModelCompat{}, Diagnostic{"models.json", owner, "compat must be an object"}
		}
		return provider.ModelCompat{Bedrock: &provider.BedrockCompat{SupportsStrictMode: wire.SupportsStrictMode}}, nil
	}
	if api != "openai-completions" && api != "openai-responses" && api != "azure-openai-responses" && api != "openai-codex-responses" {
		return provider.ModelCompat{Additional: map[string]json.RawMessage{api: bytes.Clone(raw)}}, nil
	}
	if err := validateCompatBoolFields(object, owner,
		"supportsStore", "supportsDeveloperRole", "supportsReasoningEffort", "supportsUsageInStreaming", "supportsFinishReason",
		"requiresToolResultName", "requiresAssistantAfterToolResult", "requiresThinkingAsText",
		"requiresReasoningContentOnAssistantMessages", "sendSessionAffinityHeaders", "supportsLongCacheRetention",
		"supportsStrictMode", "supportsOpenAIGrammarTools", "supportsToolSearch", "supportsExplicitPromptCacheMode", "zaiToolStream"); err != nil {
		return provider.ModelCompat{}, err
	}
	if err := validateCompatStringLiteral(object, owner, "sessionAffinityFormat", "openai", "openai-nosession", "openrouter"); err != nil {
		return provider.ModelCompat{}, err
	}
	if err := validateCompatStringLiteral(object, owner, "maxTokensField", "max_completion_tokens", "max_tokens"); err != nil {
		return provider.ModelCompat{}, err
	}
	if err := validateCompatStringLiteral(object, owner, "thinkingFormat", "openai", "openrouter", "together", "deepseek", "zai", "qwen", "chat-template", "qwen-chat-template", "string-thinking", "ant-ling"); err != nil {
		return provider.ModelCompat{}, err
	}
	if err := validateCompatStringLiteral(object, owner, "cacheControlFormat", "anthropic"); err != nil {
		return provider.ModelCompat{}, err
	}
	if err := validateCompatStringLiteral(object, owner, "deferredToolsMode", "kimi"); err != nil {
		return provider.ModelCompat{}, err
	}
	if raw, ok := object["chatTemplateKwargs"]; ok {
		if err := validateChatTemplateKwargs(raw, owner+".compat.chatTemplateKwargs"); err != nil {
			return provider.ModelCompat{}, err
		}
	}
	if raw, ok := object["openRouterRouting"]; ok {
		if err := validateOpenRouterRouting(raw, owner+".compat.openRouterRouting"); err != nil {
			return provider.ModelCompat{}, err
		}
	}
	if raw, ok := object["vercelGatewayRouting"]; ok {
		if err := validateVercelGatewayRouting(raw, owner+".compat.vercelGatewayRouting"); err != nil {
			return provider.ModelCompat{}, err
		}
	}
	var wire struct {
		SupportsStore                               *bool          `json:"supportsStore"`
		SupportsDeveloperRole                       *bool          `json:"supportsDeveloperRole"`
		SupportsReasoningEffort                     *bool          `json:"supportsReasoningEffort"`
		SupportsUsageInStreaming                    *bool          `json:"supportsUsageInStreaming"`
		SupportsFinishReason                        *bool          `json:"supportsFinishReason"`
		MaxTokensField                              *string        `json:"maxTokensField"`
		RequiresToolResultName                      *bool          `json:"requiresToolResultName"`
		RequiresAssistantAfterToolResult            *bool          `json:"requiresAssistantAfterToolResult"`
		RequiresThinkingAsText                      *bool          `json:"requiresThinkingAsText"`
		RequiresReasoningContentOnAssistantMessages *bool          `json:"requiresReasoningContentOnAssistantMessages"`
		ThinkingFormat                              *string        `json:"thinkingFormat"`
		SendSessionAffinityHeaders                  *bool          `json:"sendSessionAffinityHeaders"`
		SessionAffinityFormat                       *string        `json:"sessionAffinityFormat"`
		SupportsLongCacheRetention                  *bool          `json:"supportsLongCacheRetention"`
		SupportsStrictMode                          *bool          `json:"supportsStrictMode"`
		SupportsOpenAIGrammarTools                  *bool          `json:"supportsOpenAIGrammarTools"`
		SupportsToolSearch                          *bool          `json:"supportsToolSearch"`
		SupportsExplicitPromptCacheMode             *bool          `json:"supportsExplicitPromptCacheMode"`
		CacheControlFormat                          *string        `json:"cacheControlFormat"`
		DeferredToolsMode                           *string        `json:"deferredToolsMode"`
		ZaiToolStream                               *bool          `json:"zaiToolStream"`
		ChatTemplateKwargs                          map[string]any `json:"chatTemplateKwargs"`
		OpenRouterRouting                           map[string]any `json:"openRouterRouting"`
		VercelGatewayRouting                        map[string]any `json:"vercelGatewayRouting"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return provider.ModelCompat{}, Diagnostic{"models.json", owner, "compat must be an object"}
	}
	if wire.SessionAffinityFormat != nil && *wire.SessionAffinityFormat != "openai" && *wire.SessionAffinityFormat != "openai-nosession" && *wire.SessionAffinityFormat != "openrouter" {
		return provider.ModelCompat{}, Diagnostic{"models.json", owner, "invalid sessionAffinityFormat"}
	}
	if wire.MaxTokensField != nil && *wire.MaxTokensField != "max_completion_tokens" && *wire.MaxTokensField != "max_tokens" {
		return provider.ModelCompat{}, Diagnostic{"models.json", owner, "invalid maxTokensField"}
	}
	if wire.ThinkingFormat != nil {
		switch *wire.ThinkingFormat {
		case "openai", "openrouter", "deepseek", "together", "zai", "qwen", "chat-template", "qwen-chat-template", "string-thinking", "ant-ling":
		default:
			return provider.ModelCompat{}, Diagnostic{"models.json", owner, "invalid thinkingFormat"}
		}
	}
	return provider.ModelCompat{
		OpenAIResponses:   &provider.OpenAIResponsesCompat{SupportsDeveloperRole: wire.SupportsDeveloperRole, SessionAffinityFormat: wire.SessionAffinityFormat, SupportsLongCacheRetention: wire.SupportsLongCacheRetention, SupportsStrictMode: wire.SupportsStrictMode, SupportsOpenAIGrammarTools: wire.SupportsOpenAIGrammarTools, SupportsToolSearch: wire.SupportsToolSearch, SupportsExplicitPromptCacheMode: wire.SupportsExplicitPromptCacheMode},
		OpenAICompletions: &provider.OpenAICompletionsCompat{SupportsStore: wire.SupportsStore, SupportsDeveloperRole: wire.SupportsDeveloperRole, SupportsReasoningEffort: wire.SupportsReasoningEffort, SupportsUsageInStreaming: wire.SupportsUsageInStreaming, SupportsFinishReason: wire.SupportsFinishReason, MaxTokensField: wire.MaxTokensField, RequiresToolResultName: wire.RequiresToolResultName, RequiresAssistantAfterToolResult: wire.RequiresAssistantAfterToolResult, RequiresThinkingAsText: wire.RequiresThinkingAsText, RequiresReasoningContentOnAssistantMessages: wire.RequiresReasoningContentOnAssistantMessages, ThinkingFormat: wire.ThinkingFormat, SupportsOpenAIGrammarTools: wire.SupportsOpenAIGrammarTools, SupportsStrictMode: wire.SupportsStrictMode, SendSessionAffinityHeaders: wire.SendSessionAffinityHeaders, SessionAffinityFormat: wire.SessionAffinityFormat, SupportsLongCacheRetention: wire.SupportsLongCacheRetention, CacheControlFormat: wire.CacheControlFormat, DeferredToolsMode: wire.DeferredToolsMode, ZaiToolStream: wire.ZaiToolStream, ChatTemplateKwargs: wire.ChatTemplateKwargs, OpenRouterRouting: wire.OpenRouterRouting, VercelGatewayRouting: wire.VercelGatewayRouting},
	}, nil
}

func validateKnownCompatObject(object map[string]json.RawMessage, owner string) error {
	if err := validateCompatBoolFields(object, owner,
		"supportsStore", "supportsDeveloperRole", "supportsReasoningEffort", "supportsUsageInStreaming", "supportsFinishReason",
		"requiresToolResultName", "requiresAssistantAfterToolResult", "requiresThinkingAsText",
		"requiresReasoningContentOnAssistantMessages", "sendSessionAffinityHeaders", "supportsLongCacheRetention",
		"supportsStrictMode", "supportsOpenAIGrammarTools", "supportsToolSearch", "supportsExplicitPromptCacheMode", "zaiToolStream",
		"supportsEagerToolInputStreaming", "supportsCacheControlOnTools", "supportsTemperature", "forceAdaptiveThinking",
		"allowEmptySignature", "supportsStrictTools", "supportsToolReferences"); err != nil {
		return err
	}
	for _, literal := range []struct {
		field   string
		allowed []string
	}{
		{field: "sessionAffinityFormat", allowed: []string{"openai", "openai-nosession", "openrouter"}},
		{field: "maxTokensField", allowed: []string{"max_completion_tokens", "max_tokens"}},
		{field: "thinkingFormat", allowed: []string{"openai", "openrouter", "together", "deepseek", "zai", "qwen", "chat-template", "qwen-chat-template", "string-thinking", "ant-ling"}},
		{field: "cacheControlFormat", allowed: []string{"anthropic"}},
		{field: "deferredToolsMode", allowed: []string{"kimi"}},
	} {
		if err := validateCompatStringLiteral(object, owner, literal.field, literal.allowed...); err != nil {
			return err
		}
	}
	if raw, ok := object["chatTemplateKwargs"]; ok {
		if err := validateChatTemplateKwargs(raw, owner+".compat.chatTemplateKwargs"); err != nil {
			return err
		}
	}
	if raw, ok := object["openRouterRouting"]; ok {
		if err := validateOpenRouterRouting(raw, owner+".compat.openRouterRouting"); err != nil {
			return err
		}
	}
	if raw, ok := object["vercelGatewayRouting"]; ok {
		if err := validateVercelGatewayRouting(raw, owner+".compat.vercelGatewayRouting"); err != nil {
			return err
		}
	}
	return nil
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

func validateCompatStringLiteral(object map[string]json.RawMessage, owner, field string, allowed ...string) error {
	raw, ok := object[field]
	if !ok {
		return nil
	}
	var value string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return Diagnostic{"models.json", owner + ".compat." + field, "must be a supported string literal"}
	}
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return Diagnostic{"models.json", owner + ".compat." + field, "must be a supported string literal"}
}

func validateChatTemplateKwargs(raw json.RawMessage, path string) error {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return Diagnostic{"models.json", path, "must be an object of scalar values or variable descriptors"}
	}
	for key, value := range object {
		trimmed := bytes.TrimSpace(value)
		if bytes.Equal(trimmed, []byte("null")) || len(trimmed) != 0 && (trimmed[0] == '"' || trimmed[0] == 't' || trimmed[0] == 'f' || trimmed[0] == '-' || trimmed[0] >= '0' && trimmed[0] <= '9') {
			var scalar any
			if json.Unmarshal(value, &scalar) == nil {
				switch scalar.(type) {
				case nil, string, bool, float64:
					continue
				}
			}
		}
		var descriptor map[string]json.RawMessage
		if json.Unmarshal(value, &descriptor) != nil || descriptor == nil {
			return Diagnostic{"models.json", path + "." + key, "must be a scalar or variable descriptor"}
		}
		variable, ok := descriptor["$var"]
		if !ok {
			return Diagnostic{"models.json", path + "." + key + ".$var", "is required"}
		}
		var name string
		if json.Unmarshal(variable, &name) != nil || name != "thinking.enabled" && name != "thinking.effort" {
			return Diagnostic{"models.json", path + "." + key + ".$var", "must be thinking.enabled or thinking.effort"}
		}
		if rawOmit, ok := descriptor["omitWhenOff"]; ok {
			var omit bool
			if bytes.Equal(bytes.TrimSpace(rawOmit), []byte("null")) || json.Unmarshal(rawOmit, &omit) != nil {
				return Diagnostic{"models.json", path + "." + key + ".omitWhenOff", "must be a boolean"}
			}
		}
	}
	return nil
}

func validateVercelGatewayRouting(raw json.RawMessage, path string) error {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return Diagnostic{"models.json", path, "must be an object"}
	}
	for _, field := range []string{"only", "order"} {
		if value, ok := object[field]; ok {
			if err := validateStringArray(value); err != nil {
				return Diagnostic{"models.json", path + "." + field, "must be an array of strings"}
			}
		}
	}
	return nil
}

func validateOpenRouterRouting(raw json.RawMessage, path string) error {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return Diagnostic{"models.json", path, "must be an object"}
	}
	for _, field := range []string{"allow_fallbacks", "require_parameters", "zdr", "enforce_distillable_text"} {
		if value, ok := object[field]; ok {
			var boolean bool
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &boolean) != nil {
				return Diagnostic{"models.json", path + "." + field, "must be a boolean"}
			}
		}
	}
	if value, ok := object["data_collection"]; ok {
		var literal string
		if json.Unmarshal(value, &literal) != nil || literal != "deny" && literal != "allow" {
			return Diagnostic{"models.json", path + ".data_collection", "must be deny or allow"}
		}
	}
	for _, field := range []string{"order", "only", "ignore", "quantizations"} {
		if value, ok := object[field]; ok && validateStringArray(value) != nil {
			return Diagnostic{"models.json", path + "." + field, "must be an array of strings"}
		}
	}
	if value, ok := object["sort"]; ok {
		var literal string
		if json.Unmarshal(value, &literal) != nil {
			var sortObject map[string]json.RawMessage
			if json.Unmarshal(value, &sortObject) != nil || sortObject == nil {
				return Diagnostic{"models.json", path + ".sort", "must be a string or object"}
			}
			if by, ok := sortObject["by"]; ok && json.Unmarshal(by, &literal) != nil {
				return Diagnostic{"models.json", path + ".sort.by", "must be a string"}
			}
			if partition, ok := sortObject["partition"]; ok && !bytes.Equal(bytes.TrimSpace(partition), []byte("null")) && json.Unmarshal(partition, &literal) != nil {
				return Diagnostic{"models.json", path + ".sort.partition", "must be a string or null"}
			}
		}
	}
	if value, ok := object["max_price"]; ok {
		var prices map[string]json.RawMessage
		if json.Unmarshal(value, &prices) != nil || prices == nil {
			return Diagnostic{"models.json", path + ".max_price", "must be an object"}
		}
		for _, field := range []string{"prompt", "completion", "image", "audio", "request"} {
			if price, ok := prices[field]; ok && !jsonNumberOrString(price) {
				return Diagnostic{"models.json", path + ".max_price." + field, "must be a number or string"}
			}
		}
	}
	for _, field := range []string{"preferred_min_throughput", "preferred_max_latency"} {
		if value, ok := object[field]; ok && !validOpenRouterPercentile(value) {
			return Diagnostic{"models.json", path + "." + field, "must be a number or percentile object"}
		}
	}
	return nil
}

func validateStringArray(raw json.RawMessage) error {
	var values []string
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return errors.New("not a string array")
	}
	return nil
}

func jsonNumberOrString(raw json.RawMessage) bool {
	var number float64
	if json.Unmarshal(raw, &number) == nil {
		return true
	}
	var text string
	return json.Unmarshal(raw, &text) == nil
}

func validOpenRouterPercentile(raw json.RawMessage) bool {
	var number float64
	if json.Unmarshal(raw, &number) == nil {
		return true
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return false
	}
	for _, field := range []string{"p50", "p75", "p90", "p99"} {
		if value, ok := object[field]; ok {
			if json.Unmarshal(value, &number) != nil {
				return false
			}
		}
	}
	return true
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
	if runtime.GOOS == "windows" {
		return nil, false, fmt.Errorf("%w: cannot admit private %s on Windows", ErrUnsafeMode, label)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, false, fmt.Errorf("%w: %s", ErrUnsafeMode, label)
	}
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
		var duplicate duplicateFieldError
		if errors.As(err, &duplicate) {
			return nil, false, Diagnostic{label, duplicate.Path, "contains a duplicate object field"}
		}
		return nil, false, Diagnostic{label, "root", "is not strict JSON"}
	}
	return root, true, nil
}
func decodeObject(data []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := validateJSONValue(d, "root", 0); err != nil {
		return nil, err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("trailing JSON value")
		}
		return nil, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil || root == nil {
		return nil, errors.New("not object")
	}
	return root, nil
}

type duplicateFieldError struct{ Path string }

func (e duplicateFieldError) Error() string { return "duplicate JSON object field" }

func validateJSONValue(decoder *json.Decoder, path string, depth int) error {
	if depth > 64 {
		return errors.New("JSON nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			child := path + "." + key
			if _, duplicate := seen[key]; duplicate {
				return duplicateFieldError{Path: child}
			}
			seen[key] = struct{}{}
			if err := validateJSONValue(decoder, child, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("unterminated object")
		}
		return nil
	case '[':
		index := 0
		for decoder.More() {
			if err := validateJSONValue(decoder, fmt.Sprintf("%s.%d", path, index), depth+1); err != nil {
				return err
			}
			index++
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("unterminated array")
		}
		return nil
	default:
		return errors.New("unexpected JSON delimiter")
	}
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

// canonicalKey returns one stable representative for the same equivalence
// class used by strings.EqualFold. Provider and model lookup maps must never
// be keyed by user casing directly.
func canonicalKey(value string) string {
	var result strings.Builder
	result.Grow(len(value))
	for _, character := range value {
		representative := character
		hasLower := unicode.IsLower(character)
		for folded := unicode.SimpleFold(character); folded != character; folded = unicode.SimpleFold(folded) {
			isLower := unicode.IsLower(folded)
			if isLower && (!hasLower || folded < representative) || !isLower && !hasLower && folded < representative {
				representative = folded
				hasLower = isLower
			}
		}
		result.WriteRune(representative)
	}
	return result.String()
}

func modelKey(providerID, modelID string) string {
	return canonicalKey(providerID) + "/" + canonicalKey(modelID)
}
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
			target.ChatTemplateKwargs = provider.CloneJSONMap(value.ChatTemplateKwargs)
		}
		if value.OpenRouterRouting != nil {
			target.OpenRouterRouting = provider.CloneJSONMap(value.OpenRouterRouting)
		}
		if value.VercelGatewayRouting != nil {
			target.VercelGatewayRouting = provider.CloneJSONMap(value.VercelGatewayRouting)
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
			result.Additional[key] = bytes.Clone(value)
		}
	}
	return result
}
func cloneModel(m Model) Model {
	m.Headers = cloneHeaders(m.Headers)
	m.Input = append([]provider.InputKind(nil), m.Input...)
	m.ThinkingLevelMap = cloneThinkingMap(m.ThinkingLevelMap)
	m.Compat = cloneCompat(m.Compat)
	m.Cost.Tiers = append([]provider.CostTier(nil), m.Cost.Tiers...)
	m.UnsupportedFields = append([]string(nil), m.UnsupportedFields...)
	m.UnknownFields = append([]string(nil), m.UnknownFields...)
	return m
}
func cloneProvider(p ProviderConfig) ProviderConfig {
	p.Headers = cloneHeaders(p.Headers)
	p.Compat = cloneCompat(p.Compat)
	p.AuthHeader = cloneBoolPointer(p.AuthHeader)
	p.Models = append([]Model(nil), p.Models...)
	p.UnknownFields = append([]string(nil), p.UnknownFields...)
	p.UnsupportedFields = append([]string(nil), p.UnsupportedFields...)
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
				input := append([]provider.InputKind(nil), (*v.Input)...)
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
	s.EnabledModels = append([]string(nil), s.EnabledModels...)
	s.Compaction.Enabled = cloneBoolPointer(s.Compaction.Enabled)
	s.Compaction.ReserveTokens = cloneUint64Pointer(s.Compaction.ReserveTokens)
	s.Compaction.KeepRecentTokens = cloneUint64Pointer(s.Compaction.KeepRecentTokens)
	s.BranchSummary.ReserveTokens = cloneUint64Pointer(s.BranchSummary.ReserveTokens)
	s.BranchSummary.SkipPrompt = cloneBoolPointer(s.BranchSummary.SkipPrompt)
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

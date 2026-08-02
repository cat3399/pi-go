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
	OpenAIProviderID   = "openai"
	OpenAIResponsesAPI = "openai-responses"
	DefaultOpenAIModel = "gpt-5.5"
	maxFileBytes       = 4 << 20
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
	Provider  string            `json:"provider"`
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	API       string            `json:"api"`
	BaseURL   string            `json:"baseUrl"`
	Headers   map[string]string `json:"headers,omitempty"`
	Reasoning bool              `json:"reasoning"`
	// UnsupportedFields and UnknownFields are parser diagnostics attached to a
	// runtime projection. They are not model metadata and are intentionally not
	// part of CachedCatalog's durable model contract.
	UnsupportedFields []string `json:"-"`
	UnknownFields     []string `json:"-"`
}

func (m Model) Ref() (provider.ModelRef, error) { return provider.NewModelRef(m.Provider, m.API, m.ID) }

type ProviderConfig struct {
	ID, Name, API, BaseURL string
	Headers                map[string]string
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
	UnsupportedFields []string
	UnknownFields     []string
}

type Settings struct {
	DefaultProvider string
	DefaultModel    string
	EnabledModels   []string
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

// ValidateRoute is the selected-route consumption boundary. Future or known
// but unimplemented fields are preserved in their source file and ignored for
// unrelated providers, but they may not silently influence a production call.
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
		if len(configured.UnknownFields) != 0 {
			return fmt.Errorf("%w: selected provider contains unknown configuration fields", ErrUnsupported)
		}
	}
	if len(selected.UnsupportedFields) != 0 {
		return fmt.Errorf("%w: selected model contains unsupported configuration fields", ErrUnsupported)
	}
	if len(selected.UnknownFields) != 0 {
		return fmt.Errorf("%w: selected model contains unknown configuration fields", ErrUnsupported)
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
	current, err := settingsFromRaw(root, "global settings.json")
	if err != nil {
		return err
	}
	if err := change(&current); err != nil {
		return err
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
	if current.EnabledModels == nil {
		delete(root, "enabledModels")
	} else {
		b, _ := json.Marshal(current.EnabledModels)
		root["enabledModels"] = b
	}
	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encode global settings", ErrInvalidConfig)
	}
	if err := atomicWrite(ctx, path, append(encoded, '\n'), "global settings.json", r.faults); err != nil {
		return err
	}
	// Rebuild without recursively acquiring the operation gate.
	r.mu.Lock()
	defer r.mu.Unlock()
	generation := r.snapshot.Generation + 1
	r.snapshot = buildSnapshot(r.providers, cached, settings)
	r.snapshot.Generation = generation
	r.storeErrors = storeErrors
	return nil
}

func putString(root map[string]json.RawMessage, key, value string) {
	if value == "" {
		delete(root, key)
		return
	}
	b, _ := json.Marshal(value)
	root[key] = b
}

type Selection struct{ Provider, Model string }
type Resolution struct {
	Model       Model
	Diagnostics []Diagnostic
}

// Resolve applies exact provider/model selection first, otherwise settings'
// default then the ordered enabledModels scope. It is intentionally exact: fuzzy
// selection and cycling need catalog completeness and remain deferred.
func (r *Runtime) Resolve(selection Selection) (Resolution, error) {
	s := r.Snapshot()
	providerID, modelID := strings.TrimSpace(selection.Provider), strings.TrimSpace(selection.Model)
	if modelID != "" && providerID == "" {
		if p, id, ok := splitKnownProvider(modelID, s.Models); ok {
			providerID, modelID = p, id
		}
	}
	if modelID != "" {
		matches := filterModels(s.Models, providerID, modelID)
		if len(matches) == 1 {
			return Resolution{Model: matches[0]}, nil
		}
		if len(matches) > 1 {
			return Resolution{}, fmt.Errorf("%w: bare model %q is ambiguous", ErrNotFound, modelID)
		}
		// An explicit provider is an intentional custom-model request. Derive its
		// transport metadata from the provider baseline, never from a guessed API.
		if providerID != "" {
			for _, base := range s.Models {
				if strings.EqualFold(base.Provider, providerID) {
					base.ID, base.Name = modelID, modelID
					base = r.applyModelOverride(providerID, modelID, base)
					return Resolution{Model: base}, nil
				}
			}
		}
		return Resolution{}, fmt.Errorf("%w: %s", ErrNotFound, reference(providerID, modelID))
	}
	if providerID != "" {
		return Resolution{}, fmt.Errorf("%w: provider requires a model", ErrInvalidConfig)
	}
	if s.Settings.DefaultModel != "" {
		p := s.Settings.DefaultProvider
		if p == "" {
			_, p, _ = splitKnownProvider(s.Settings.DefaultModel, s.Models)
		}
		matches := filterModels(s.Models, p, s.Settings.DefaultModel)
		if len(matches) == 1 {
			return Resolution{Model: matches[0]}, nil
		}
		if p != "" {
			for _, base := range s.Models {
				if strings.EqualFold(base.Provider, p) {
					base.ID, base.Name = s.Settings.DefaultModel, s.Settings.DefaultModel
					base = r.applyModelOverride(p, s.Settings.DefaultModel, base)
					return Resolution{Model: base}, nil
				}
			}
		}
		return Resolution{}, fmt.Errorf("%w: settings default %s", ErrNotFound, reference(p, s.Settings.DefaultModel))
	}
	if len(s.Settings.EnabledModels) > 0 {
		scoped, diagnostics := scope(s.Models, s.Settings.EnabledModels)
		if len(scoped) > 0 {
			return Resolution{Model: scoped[0], Diagnostics: diagnostics}, nil
		}
		return Resolution{Diagnostics: diagnostics}, fmt.Errorf("%w: no enabled model", ErrUnavailable)
	}
	for _, m := range s.Models {
		if m.Provider == OpenAIProviderID && m.ID == DefaultOpenAIModel {
			return Resolution{Model: m}, nil
		}
	}
	return Resolution{}, fmt.Errorf("%w: no baseline model", ErrNotFound)
}

func reference(providerID, modelID string) string {
	if providerID == "" {
		return modelID
	}
	return providerID + "/" + modelID
}
func filterModels(models []Model, providerID, modelID string) []Model {
	var out []Model
	for _, m := range models {
		if (providerID == "" || strings.EqualFold(m.Provider, providerID)) && strings.EqualFold(m.ID, modelID) {
			out = append(out, m)
		}
	}
	return out
}
func splitKnownProvider(value string, models []Model) (providerID, modelID string, ok bool) {
	p, id, found := strings.Cut(value, "/")
	if !found || p == "" || id == "" {
		return "", value, false
	}
	for _, m := range models {
		if strings.EqualFold(m.Provider, p) {
			return m.Provider, id, true
		}
	}
	return "", value, false
}
func scope(models []Model, patterns []string) ([]Model, []Diagnostic) {
	var out []Model
	var ds []Diagnostic
	for _, p := range patterns {
		matches := filterPattern(models, p)
		if len(matches) == 0 {
			ds = append(ds, Diagnostic{Source: "settings", Path: "enabledModels", Message: "no model matches configured scope"})
			continue
		}
		for _, m := range matches {
			duplicate := false
			for _, x := range out {
				if x.Provider == m.Provider && x.ID == m.ID {
					duplicate = true
				}
			}
			if !duplicate {
				out = append(out, m)
			}
		}
	}
	return out, ds
}
func filterPattern(models []Model, pattern string) []Model {
	pattern = strings.TrimSpace(pattern)
	if p, id, ok := splitKnownProvider(pattern, models); ok {
		return filterModels(models, p, id)
	}
	return filterModels(models, "", pattern)
}

func buildSnapshot(providers map[string]ProviderConfig, cached map[string]CachedCatalog, settings Settings) Snapshot {
	// The hand-written baseline is deliberately tiny because the fixed upstream
	// catalog cannot be regenerated from its absent manifest/source data.
	byKey := map[string]Model{modelKey(OpenAIProviderID, DefaultOpenAIModel): {Provider: OpenAIProviderID, ID: DefaultOpenAIModel, Name: DefaultOpenAIModel, API: OpenAIResponsesAPI}}
	ids := make([]string, 0, len(providers)+len(cached))
	for id := range providers {
		ids = append(ids, id)
	}
	for id, entry := range cached {
		if _, exists := providers[id]; !exists {
			ids = append(ids, id)
		}
		for index, cached := range entry.Models {
			m := cachedRuntimeModel(entry, index)
			byKey[modelKey(cached.Provider, cached.ID)] = m
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := providers[id]
		for _, m := range p.Models {
			if m.API == "" {
				m.API = p.API
			}
			if m.BaseURL == "" {
				m.BaseURL = p.BaseURL
			}
			if m.Name == "" {
				m.Name = m.ID
			}
			m.Provider = p.ID
			byKey[modelKey(m.Provider, m.ID)] = m
		}
		if id == OpenAIProviderID {
			key := modelKey(id, DefaultOpenAIModel)
			m := byKey[key]
			if p.API != "" {
				m.API = p.API
			}
			if p.BaseURL != "" {
				m.BaseURL = p.BaseURL
			}
			byKey[key] = m
		}
		for modelID, override := range p.overrides {
			key := modelKey(p.ID, modelID)
			m, ok := byKey[key]
			if !ok {
				continue
			}
			byKey[key] = applyModelOverride(m, override)
		}
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	models := make([]Model, 0, len(keys))
	for _, key := range keys {
		models = append(models, cloneModel(byKey[key]))
	}
	return Snapshot{Models: models, Providers: ids, Settings: cloneSettings(settings)}
}

func (r *Runtime) applyModelOverride(providerID, modelID string, model Model) Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	provider, ok := r.providers[canonicalKey(providerID)]
	if !ok {
		return model
	}
	override, ok := provider.overrides[canonicalKey(modelID)]
	if !ok {
		return model
	}
	return applyModelOverride(model, override)
}

func applyModelOverride(model Model, override modelOverride) Model {
	if override.Name != nil {
		model.Name = *override.Name
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
	if key, ok, err := optionalSecret(o, "apiKey", id); err != nil {
		return p, err
	} else if ok {
		p.ConfiguredAPIKey = &key
	}
	for _, key := range []string{"headers", "compat", "oauth", "authHeader"} {
		if _, present := o[key]; present {
			p.UnsupportedFields = append(p.UnsupportedFields, key)
		}
	}
	if data, ok := o["models"]; ok {
		var models []json.RawMessage
		if err := json.Unmarshal(data, &models); err != nil {
			return p, Diagnostic{"models.json", "providers." + id + ".models", "must be an array"}
		}
		seen := map[string]bool{}
		for i, entry := range models {
			m, e := parseModel(id, i, entry)
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
			override, err := parseOverride(id, modelID, value)
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
func parseOverride(providerID, modelID string, raw json.RawMessage) (modelOverride, error) {
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
	for key := range o {
		switch key {
		case "name":
		case "reasoning", "thinkingLevelMap", "input", "cost", "contextWindow", "maxTokens", "headers", "compat":
			result.UnsupportedFields = append(result.UnsupportedFields, key)
		default:
			result.UnknownFields = append(result.UnknownFields, key)
		}
	}
	sort.Strings(result.UnsupportedFields)
	sort.Strings(result.UnknownFields)
	return result, nil
}
func parseModel(providerID string, index int, raw json.RawMessage) (Model, error) {
	var o map[string]json.RawMessage
	if err := json.Unmarshal(raw, &o); err != nil || o == nil {
		return Model{}, Diagnostic{"models.json", fmt.Sprintf("providers.%s.models.%d", providerID, index), "must be an object"}
	}
	id, ok, err := requiredString(o, "id", providerID)
	if err != nil || !ok {
		return Model{}, err
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
	for key := range o {
		switch key {
		case "id", "name", "api", "baseUrl":
		case "reasoning", "thinkingLevelMap", "input", "cost", "contextWindow", "maxTokens", "headers", "compat":
			m.UnsupportedFields = append(m.UnsupportedFields, key)
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
	return settingsFromRaw(root, label)
}
func settingsFromRaw(root map[string]json.RawMessage, label string) (Settings, error) {
	s := Settings{}
	var err error
	if s.DefaultProvider, err = optionalString(root, "defaultProvider", ""); err != nil {
		return s, fmt.Errorf("%w: %s", ErrInvalidConfig, label)
	}
	if s.DefaultModel, err = optionalString(root, "defaultModel", ""); err != nil {
		return s, fmt.Errorf("%w: %s", ErrInvalidConfig, label)
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
	return nil
}
func mergeSettings(base, override Settings) Settings {
	out := cloneSettings(base)
	if override.DefaultProvider != "" {
		out.DefaultProvider = override.DefaultProvider
	}
	if override.DefaultModel != "" {
		out.DefaultModel = override.DefaultModel
	}
	if override.EnabledModels != nil {
		out.EnabledModels = append([]string(nil), override.EnabledModels...)
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
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, true, Diagnostic{"models.json", key, "must be a boolean"}
	}
	return v, true, nil
}
func optionalHeaders(o map[string]json.RawMessage, key, owner string) (map[string]string, error) {
	raw, ok := o[key]
	if !ok {
		return nil, nil
	}
	var h map[string]string
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, Diagnostic{"models.json", key, "must be an object of strings"}
	}
	for k, v := range h {
		if !validValue(k) || !utf8.ValidString(v) || strings.ContainsFunc(v, unicode.IsControl) {
			return nil, Diagnostic{"models.json", key, "contains an invalid header"}
		}
	}
	return h, nil
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
func cloneModel(m Model) Model {
	m.Headers = cloneHeaders(m.Headers)
	m.UnsupportedFields = append([]string(nil), m.UnsupportedFields...)
	m.UnknownFields = append([]string(nil), m.UnknownFields...)
	return m
}
func cloneProvider(p ProviderConfig) ProviderConfig {
	p.Headers = cloneHeaders(p.Headers)
	p.Models = append([]Model(nil), p.Models...)
	p.UnknownFields = append([]string(nil), p.UnknownFields...)
	p.UnsupportedFields = append([]string(nil), p.UnsupportedFields...)
	if p.overrides != nil {
		p.overrides = make(map[string]modelOverride, len(p.overrides))
		for k, v := range p.overrides {
			if v.Name != nil {
				x := *v.Name
				v.Name = &x
			}
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
	return s
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

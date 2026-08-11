package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/provider"
)

// CachedModel is the complete public model contract of models-store.json.
// Runtime parse diagnostics deliberately live on Model instead of this type.
type CachedModel struct {
	Provider         string                             `json:"provider"`
	ID               string                             `json:"id"`
	Name             string                             `json:"name,omitempty"`
	API              string                             `json:"api"`
	BaseURL          string                             `json:"baseUrl,omitempty"`
	Headers          map[string]string                  `json:"headers,omitempty"`
	Reasoning        bool                               `json:"reasoning"`
	ThinkingLevelMap map[provider.ThinkingLevel]*string `json:"thinkingLevelMap,omitempty"`
	Input            []provider.InputKind               `json:"input,omitempty"`
	Cost             provider.CostRates                 `json:"cost"`
	ContextWindow    uint64                             `json:"contextWindow"`
	MaxTokens        uint64                             `json:"maxTokens"`
	// Compat is the original JSON object, kept separately because the runtime
	// exposes it as a typed provider.ModelCompat selected by API dialect.
	// Keeping its wire spelling makes cache read/write lossless for adapters
	// that this binary does not implement yet.
	Compat json.RawMessage `json:"compat,omitempty"`
}

// CachedCatalog is the durable, provider-scoped result of a dynamic provider
// refresh. The cache augments an already registered provider; it never creates
// a provider or acts as an authoritative builtin by itself.
type CachedCatalog struct {
	Models        []CachedModel `json:"models"`
	ETag          string        `json:"etag,omitempty"`
	LastModified  *int64        `json:"lastModified,omitempty"`
	CheckedAt     *int64        `json:"checkedAt,omitempty"`
	runtimeModels []Model
}

// Store is intentionally separate from Runtime: cache mutation must not alter
// a currently selected catalog snapshot until a caller explicitly reloads it.
// Write/Delete require the path's parent to have been created durably by the
// application; the store never creates unsynced ancestor directories.
type Store struct {
	path   string
	mu     sync.Mutex
	local  chan struct{}
	faults atomicWriteFaults
}

func NewStore(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: models store path is required", ErrInvalidConfig)
	}
	return &Store{path: path, local: newLocalGate()}, nil
}

func (s *Store) Read(ctx context.Context, providerID string) (CachedCatalog, bool, error) {
	if !validID(providerID) {
		return CachedCatalog{}, false, fmt.Errorf("%w: invalid provider identifier", ErrInvalidConfig)
	}
	if runtime.GOOS == "windows" {
		return CachedCatalog{}, false, fmt.Errorf("%w: models-store.json", ErrPersistence)
	}
	releaseLocal, err := acquireLocal(ctx, s.local)
	if err != nil {
		return CachedCatalog{}, false, err
	}
	defer releaseLocal()
	if _, err := os.Stat(filepath.Dir(s.path)); errors.Is(err, os.ErrNotExist) {
		return CachedCatalog{}, false, nil
	} else if err != nil {
		return CachedCatalog{}, false, fmt.Errorf("%w: inspect models store directory", ErrInvalidConfig)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	release, err := acquireFileLock(ctx, s.path)
	if err != nil {
		return CachedCatalog{}, false, err
	}
	defer release()
	root, exists, err := readRawObject(s.path, false, "models-store.json")
	if err != nil || !exists {
		return CachedCatalog{}, false, err
	}
	index, err := indexStoreRoot(root)
	if err != nil {
		return CachedCatalog{}, false, err
	}
	storedKey, ok := index[providerID]
	if !ok {
		return CachedCatalog{}, false, nil
	}
	raw := root[storedKey]
	entry, err := decodeCatalog(raw, providerID)
	return entry, err == nil, err
}
func (s *Store) Write(ctx context.Context, providerID string, entry CachedCatalog) error {
	if !validID(providerID) {
		return fmt.Errorf("%w: invalid provider identifier", ErrInvalidConfig)
	}
	entry, err := canonicalizeCatalog(entry, providerID)
	if err != nil {
		return err
	}
	if err := validateCatalog(entry, providerID); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return fmt.Errorf("%w: models-store.json", ErrPersistence)
	}
	releaseLocal, err := acquireLocal(ctx, s.local)
	if err != nil {
		return err
	}
	defer releaseLocal()
	if err := requireExistingDirectory(filepath.Dir(s.path), "models store directory"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	release, err := acquireFileLock(ctx, s.path)
	if err != nil {
		return err
	}
	defer release()
	root, exists, err := readRawObject(s.path, false, "models-store.json")
	if err != nil {
		return err
	}
	if !exists {
		root = map[string]json.RawMessage{}
	}
	index, err := indexStoreRoot(root)
	if err != nil {
		return err
	}
	storedKey, exists := index[providerID]
	var previous json.RawMessage
	if exists {
		previous = root[storedKey]
		if _, err := decodeCatalog(previous, providerID); err != nil {
			return err
		}
		delete(root, storedKey)
	}
	encoded, err := encodeCatalog(entry, previous)
	if err != nil {
		return fmt.Errorf("%w: encode models store", ErrInvalidConfig)
	}
	root[providerID] = encoded
	all, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encode models store", ErrInvalidConfig)
	}
	return atomicWrite(ctx, s.path, append(all, '\n'), "models-store.json", s.faults)
}
func (s *Store) Delete(ctx context.Context, providerID string) error {
	if !validID(providerID) {
		return fmt.Errorf("%w: invalid provider identifier", ErrInvalidConfig)
	}
	if runtime.GOOS == "windows" {
		return fmt.Errorf("%w: models-store.json", ErrPersistence)
	}
	releaseLocal, err := acquireLocal(ctx, s.local)
	if err != nil {
		return err
	}
	defer releaseLocal()
	if err := requireExistingDirectory(filepath.Dir(s.path), "models store directory"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	release, err := acquireFileLock(ctx, s.path)
	if err != nil {
		return err
	}
	defer release()
	root, exists, err := readRawObject(s.path, false, "models-store.json")
	if err != nil || !exists {
		return err
	}
	index, err := indexStoreRoot(root)
	if err != nil {
		return err
	}
	storedKey, ok := index[providerID]
	if !ok {
		return nil
	}
	delete(root, storedKey)
	all, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encode models store", ErrInvalidConfig)
	}
	return atomicWrite(ctx, s.path, append(all, '\n'), "models-store.json", s.faults)
}

// loadStoreCatalogs projects only valid cache entries. A malformed entry is
// retained on disk and becomes a selected-route error; it cannot poison an
// unrelated provider's route.
func loadStoreCatalogs(path string) (map[string]CachedCatalog, map[string]error, error) {
	root, exists, err := readRawObject(path, false, "models-store.json")
	if err != nil || !exists {
		return map[string]CachedCatalog{}, map[string]error{}, err
	}
	index, err := indexStoreRoot(root)
	if err != nil {
		return nil, nil, err
	}
	entries := make(map[string]CachedCatalog, len(root))
	errs := make(map[string]error)
	providerIDs := make([]string, 0, len(index))
	for providerID := range index {
		providerIDs = append(providerIDs, providerID)
	}
	sort.Strings(providerIDs)
	for _, providerID := range providerIDs {
		storedKey := index[providerID]
		if !validID(storedKey) {
			continue // opaque future root key: preserve it, but do not route through it.
		}
		raw := root[storedKey]
		entry, err := decodeCatalog(raw, providerID)
		if err != nil {
			errs[providerID] = err
			continue
		}
		entries[providerID] = entry
	}
	return entries, errs, nil
}
func decodeCatalog(raw json.RawMessage, providerID string) (CachedCatalog, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return CachedCatalog{}, Diagnostic{"models-store.json", providerID, "must be a catalog object"}
	}
	var entry CachedCatalog
	if rawModels, ok := object["models"]; ok {
		var models []json.RawMessage
		if err := json.Unmarshal(rawModels, &models); err != nil || models == nil {
			return entry, Diagnostic{"models-store.json", providerID, "models must be an array"}
		}
		for index, rawModel := range models {
			model, runtimeModel, err := parseCatalogModel(providerID, index, rawModel)
			if err != nil {
				return entry, err
			}
			entry.Models = append(entry.Models, model)
			entry.runtimeModels = append(entry.runtimeModels, runtimeModel)
		}
	} else {
		return entry, Diagnostic{"models-store.json", providerID, "models is required"}
	}
	if rawETag, ok := object["etag"]; ok && json.Unmarshal(rawETag, &entry.ETag) != nil {
		return entry, Diagnostic{"models-store.json", providerID, "etag must be a string"}
	}
	if rawLastModified, ok := object["lastModified"]; ok && json.Unmarshal(rawLastModified, &entry.LastModified) != nil {
		return entry, Diagnostic{"models-store.json", providerID, "lastModified must be an integer"}
	}
	if rawCheckedAt, ok := object["checkedAt"]; ok && json.Unmarshal(rawCheckedAt, &entry.CheckedAt) != nil {
		return entry, Diagnostic{"models-store.json", providerID, "checkedAt must be an integer"}
	}
	if err := validateCatalog(entry, providerID); err != nil {
		return entry, err
	}
	return entry, nil
}

func parseCatalogModel(providerID string, index int, raw json.RawMessage) (CachedModel, Model, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return CachedModel{}, Model{}, Diagnostic{"models-store.json", providerID, "contains an invalid model"}
	}
	rawProvider, ok := object["provider"]
	var declaredProvider string
	if !ok || json.Unmarshal(rawProvider, &declaredProvider) != nil || declaredProvider != providerID {
		return CachedModel{}, Model{}, Diagnostic{"models-store.json", providerID, "contains an invalid model"}
	}
	var reasoning bool
	if rawReasoning, exists := object["reasoning"]; exists && json.Unmarshal(rawReasoning, &reasoning) != nil {
		return CachedModel{}, Model{}, Diagnostic{"models-store.json", providerID, "contains an invalid model"}
	}
	headers, err := optionalHeaders(object, "headers", providerID)
	if err != nil {
		return CachedModel{}, Model{}, Diagnostic{"models-store.json", providerID, "contains an invalid model"}
	}
	delete(object, "provider")
	withoutProvider, err := json.Marshal(object)
	if err != nil {
		return CachedModel{}, Model{}, Diagnostic{"models-store.json", providerID, "contains an invalid model"}
	}
	model, err := parseModel(providerID, "", index, withoutProvider)
	if err != nil {
		return CachedModel{}, Model{}, Diagnostic{"models-store.json", providerID, "contains an invalid model"}
	}
	model.Provider = providerID
	model.Reasoning = reasoning
	model.Headers = cloneHeaders(headers)
	cached := cachedFromRuntimeModel(model, object["compat"])
	return cached, model, nil
}

// encodeCatalog preserves unknown fields in the old entry while replacing the
// known cache values. A remote refresh is allowed to replace its model list;
// all unrelated provider and entry metadata survives the update.
func encodeCatalog(entry CachedCatalog, previous json.RawMessage) (json.RawMessage, error) {
	object := map[string]json.RawMessage{}
	if len(previous) != 0 {
		if err := json.Unmarshal(previous, &object); err != nil || object == nil {
			object = map[string]json.RawMessage{}
		}
	}
	oldModels := map[string]json.RawMessage{}
	if rawModels, ok := object["models"]; ok {
		var values []json.RawMessage
		if json.Unmarshal(rawModels, &values) == nil {
			for _, value := range values {
				var modelObject map[string]json.RawMessage
				var id string
				if json.Unmarshal(value, &modelObject) == nil && json.Unmarshal(modelObject["id"], &id) == nil {
					oldModels[id] = value
				}
			}
		}
	}
	models := make([]json.RawMessage, 0, len(entry.Models))
	for _, model := range entry.Models {
		modelObject := map[string]json.RawMessage{}
		if previousModel, ok := oldModels[model.ID]; ok {
			_ = json.Unmarshal(previousModel, &modelObject)
		}
		if err := writeCachedModelFields(modelObject, model); err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(modelObject)
		if err != nil {
			return nil, err
		}
		models = append(models, encoded)
	}
	encodedModels, err := json.Marshal(models)
	if err != nil {
		return nil, err
	}
	object["models"] = encodedModels
	if entry.CheckedAt == nil {
		delete(object, "checkedAt")
	} else if value, err := json.Marshal(*entry.CheckedAt); err != nil {
		return nil, err
	} else {
		object["checkedAt"] = value
	}
	if entry.ETag == "" {
		delete(object, "etag")
	} else if value, err := json.Marshal(entry.ETag); err != nil {
		return nil, err
	} else {
		object["etag"] = value
	}
	if entry.LastModified == nil {
		delete(object, "lastModified")
	} else if value, err := json.Marshal(*entry.LastModified); err != nil {
		return nil, err
	} else {
		object["lastModified"] = value
	}
	return json.Marshal(object)
}
func validateCatalog(entry CachedCatalog, providerID string) error {
	if entry.CheckedAt != nil && *entry.CheckedAt < 0 {
		return Diagnostic{"models-store.json", providerID, "checkedAt cannot be negative"}
	}
	if entry.LastModified != nil && *entry.LastModified < 0 {
		return Diagnostic{"models-store.json", providerID, "lastModified cannot be negative"}
	}
	for _, m := range entry.Models {
		if !validValue(m.ID) || !validID(m.Provider) || m.Provider != providerID || !validValue(m.API) || m.Name != "" && !validValue(m.Name) || m.BaseURL != "" && !validValue(m.BaseURL) {
			return Diagnostic{"models-store.json", providerID, "contains an invalid model"}
		}
		for name, value := range m.Headers {
			if !validValue(name) || !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
				return Diagnostic{"models-store.json", providerID, "contains an invalid model"}
			}
		}
	}
	return nil
}

func canonicalizeCatalog(entry CachedCatalog, providerID string) (CachedCatalog, error) {
	entry.Models = append([]CachedModel(nil), entry.Models...)
	entry.runtimeModels = nil
	for index := range entry.Models {
		model := cloneCachedModel(entry.Models[index])
		if !validID(model.Provider) || model.Provider != providerID {
			return CachedCatalog{}, Diagnostic{"models-store.json", providerID, "contains a model for another provider"}
		}
		model.Provider = providerID
		entry.Models[index] = model
	}
	return entry, nil
}

func cloneCachedModel(model CachedModel) CachedModel {
	model.Headers = cloneHeaders(model.Headers)
	model.Input = cloneInputKinds(model.Input)
	model.ThinkingLevelMap = cloneThinkingMap(model.ThinkingLevelMap)
	model.Cost.Tiers = append([]provider.CostTier(nil), model.Cost.Tiers...)
	model.Compat = append(json.RawMessage(nil), model.Compat...)
	return model
}

func cachedRuntimeModel(entry CachedCatalog, index int) Model {
	if len(entry.runtimeModels) == len(entry.Models) {
		return cloneModel(entry.runtimeModels[index])
	}
	model := entry.Models[index]
	return Model{Provider: model.Provider, ID: model.ID, Name: model.Name, API: model.API, BaseURL: model.BaseURL, Headers: cloneHeaders(model.Headers), Reasoning: model.Reasoning, ThinkingLevelMap: cloneThinkingMap(model.ThinkingLevelMap), Input: cloneInputKinds(model.Input), Cost: provider.CostRates{Input: model.Cost.Input, Output: model.Cost.Output, CacheRead: model.Cost.CacheRead, CacheWrite: model.Cost.CacheWrite, Tiers: append([]provider.CostTier(nil), model.Cost.Tiers...)}, ContextWindow: model.ContextWindow, MaxTokens: model.MaxTokens}
}

func cachedFromRuntimeModel(model Model, compat json.RawMessage) CachedModel {
	return CachedModel{Provider: model.Provider, ID: model.ID, Name: model.Name, API: model.API, BaseURL: model.BaseURL, Headers: cloneHeaders(model.Headers), Reasoning: model.Reasoning, ThinkingLevelMap: cloneThinkingMap(model.ThinkingLevelMap), Input: cloneInputKinds(model.Input), Cost: provider.CostRates{Input: model.Cost.Input, Output: model.Cost.Output, CacheRead: model.Cost.CacheRead, CacheWrite: model.Cost.CacheWrite, Tiers: append([]provider.CostTier(nil), model.Cost.Tiers...)}, ContextWindow: model.ContextWindow, MaxTokens: model.MaxTokens, Compat: append(json.RawMessage(nil), compat...)}
}

// writeCachedModelFields replaces every member of the public Model contract;
// only future/unknown keys from the previous wire object are preserved.
func writeCachedModelFields(object map[string]json.RawMessage, model CachedModel) error {
	values := map[string]any{"provider": model.Provider, "id": model.ID, "api": model.API, "reasoning": model.Reasoning, "cost": model.Cost}
	if model.Name != "" {
		values["name"] = model.Name
	}
	if model.BaseURL != "" {
		values["baseUrl"] = model.BaseURL
	}
	if model.Headers != nil {
		values["headers"] = cloneHeaders(model.Headers)
	}
	if model.ThinkingLevelMap != nil {
		values["thinkingLevelMap"] = cloneThinkingMap(model.ThinkingLevelMap)
	}
	if model.Input != nil {
		values["input"] = cloneInputKinds(model.Input)
	}
	if model.ContextWindow != 0 {
		values["contextWindow"] = model.ContextWindow
	}
	if model.MaxTokens != 0 {
		values["maxTokens"] = model.MaxTokens
	}
	for _, key := range []string{"name", "baseUrl", "headers", "thinkingLevelMap", "input", "contextWindow", "maxTokens", "compat"} {
		delete(object, key)
	}
	for key, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		object[key] = encoded
	}
	if len(model.Compat) != 0 {
		if !json.Valid(model.Compat) {
			return fmt.Errorf("invalid model compat")
		}
		object["compat"] = append(json.RawMessage(nil), model.Compat...)
	}
	return nil
}

func indexStoreRoot(root map[string]json.RawMessage) (map[string]string, error) {
	keys := make([]string, 0, len(root))
	for key := range root {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	index := make(map[string]string, len(keys))
	for _, key := range keys {
		index[key] = key
	}
	return index, nil
}

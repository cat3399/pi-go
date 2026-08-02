package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// CachedCatalog is the durable, provider-scoped result of a future remote
// catalog refresh. v0.1 reads and writes it safely but never refreshes a
// catalog over the network or treats the cache as an authoritative builtin.
type CachedCatalog struct {
	Models       []Model `json:"models"`
	ETag         string  `json:"etag,omitempty"`
	LastModified string  `json:"lastModified,omitempty"`
	CheckedAt    int64   `json:"checkedAt"`
}

// Store is intentionally separate from Runtime: cache mutation must not alter
// a currently selected catalog snapshot until a caller explicitly reloads it.
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
	raw, ok := root[providerID]
	if !ok {
		return CachedCatalog{}, false, nil
	}
	entry, err := decodeCatalog(raw, providerID)
	return entry, err == nil, err
}
func (s *Store) Write(ctx context.Context, providerID string, entry CachedCatalog) error {
	if !validID(providerID) {
		return fmt.Errorf("%w: invalid provider identifier", ErrInvalidConfig)
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
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("%w: create models store directory", ErrInvalidConfig)
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
	encoded, err := encodeCatalog(entry, root[providerID])
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
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("%w: create models store directory", ErrInvalidConfig)
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
	delete(root, providerID)
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
	entries := make(map[string]CachedCatalog, len(root))
	errs := make(map[string]error)
	for providerID, raw := range root {
		if !validID(providerID) {
			continue // opaque future root key: preserve it, but do not route through it.
		}
		entry, err := decodeCatalog(raw, providerID)
		if err != nil {
			errs[strings.ToLower(providerID)] = err
			continue
		}
		entries[strings.ToLower(providerID)] = entry
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
			model, err := parseCatalogModel(providerID, index, rawModel)
			if err != nil {
				return entry, err
			}
			entry.Models = append(entry.Models, model)
		}
	} else {
		return entry, Diagnostic{"models-store.json", providerID, "models is required"}
	}
	if rawETag, ok := object["etag"]; ok && json.Unmarshal(rawETag, &entry.ETag) != nil {
		return entry, Diagnostic{"models-store.json", providerID, "etag must be a string"}
	}
	if rawLastModified, ok := object["lastModified"]; ok && json.Unmarshal(rawLastModified, &entry.LastModified) != nil {
		return entry, Diagnostic{"models-store.json", providerID, "lastModified must be a string"}
	}
	if rawCheckedAt, ok := object["checkedAt"]; !ok || json.Unmarshal(rawCheckedAt, &entry.CheckedAt) != nil {
		return entry, Diagnostic{"models-store.json", providerID, "checkedAt must be an integer"}
	}
	if err := validateCatalog(entry, providerID); err != nil {
		return entry, err
	}
	return entry, nil
}

func parseCatalogModel(providerID string, index int, raw json.RawMessage) (Model, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return Model{}, Diagnostic{"models-store.json", providerID, "contains an invalid model"}
	}
	rawProvider, ok := object["provider"]
	var declaredProvider string
	if !ok || json.Unmarshal(rawProvider, &declaredProvider) != nil || declaredProvider != providerID {
		return Model{}, Diagnostic{"models-store.json", providerID, "contains an invalid model"}
	}
	delete(object, "provider")
	withoutProvider, err := json.Marshal(object)
	if err != nil {
		return Model{}, Diagnostic{"models-store.json", providerID, "contains an invalid model"}
	}
	model, err := parseModel(providerID, index, withoutProvider)
	if err != nil {
		return Model{}, Diagnostic{"models-store.json", providerID, "contains an invalid model"}
	}
	model.Provider = providerID
	return model, nil
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
					oldModels[strings.ToLower(id)] = value
				}
			}
		}
	}
	models := make([]json.RawMessage, 0, len(entry.Models))
	for _, model := range entry.Models {
		modelObject := map[string]json.RawMessage{}
		if previousModel, ok := oldModels[strings.ToLower(model.ID)]; ok {
			_ = json.Unmarshal(previousModel, &modelObject)
		}
		for key, value := range map[string]string{"provider": model.Provider, "id": model.ID, "name": model.Name, "api": model.API, "baseUrl": model.BaseURL} {
			if value == "" {
				delete(modelObject, key)
				continue
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			modelObject[key] = encoded
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
	checkedAt, err := json.Marshal(entry.CheckedAt)
	if err != nil {
		return nil, err
	}
	object["models"] = encodedModels
	object["checkedAt"] = checkedAt
	if entry.ETag == "" {
		delete(object, "etag")
	} else if value, err := json.Marshal(entry.ETag); err != nil {
		return nil, err
	} else {
		object["etag"] = value
	}
	if entry.LastModified == "" {
		delete(object, "lastModified")
	} else if value, err := json.Marshal(entry.LastModified); err != nil {
		return nil, err
	} else {
		object["lastModified"] = value
	}
	return json.Marshal(object)
}
func validateCatalog(entry CachedCatalog, providerID string) error {
	if entry.CheckedAt < 0 {
		return Diagnostic{"models-store.json", providerID, "checkedAt cannot be negative"}
	}
	seen := map[string]bool{}
	for _, m := range entry.Models {
		if !validValue(m.ID) || !validID(m.Provider) || m.Provider != providerID || !validValue(m.API) {
			return Diagnostic{"models-store.json", providerID, "contains an invalid model"}
		}
		key := strings.ToLower(m.ID)
		if seen[key] {
			return Diagnostic{"models-store.json", providerID, "contains duplicate model id"}
		}
		seen[key] = true
	}
	return nil
}

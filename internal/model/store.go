package model

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	path string
	mu   sync.Mutex
}

func NewStore(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: models store path is required", ErrInvalidConfig)
	}
	return &Store{path: path}, nil
}

func (s *Store) Read(ctx context.Context, providerID string) (CachedCatalog, bool, error) {
	if err := contextCause(ctx); err != nil {
		return CachedCatalog{}, false, err
	}
	if !validID(providerID) {
		return CachedCatalog{}, false, fmt.Errorf("%w: invalid provider identifier", ErrInvalidConfig)
	}
	if _, err := os.Stat(s.path); os.IsNotExist(err) {
		return CachedCatalog{}, false, nil
	} else if err != nil {
		return CachedCatalog{}, false, fmt.Errorf("%w: inspect models store", ErrInvalidConfig)
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
	if err := contextCause(ctx); err != nil {
		return err
	}
	if !validID(providerID) {
		return fmt.Errorf("%w: invalid provider identifier", ErrInvalidConfig)
	}
	if err := validateCatalog(entry, providerID); err != nil {
		return err
	}
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
	encoded, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("%w: encode models store", ErrInvalidConfig)
	}
	root[providerID] = encoded
	all, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encode models store", ErrInvalidConfig)
	}
	return atomicWrite(s.path, append(all, '\n'))
}
func (s *Store) Delete(ctx context.Context, providerID string) error {
	if err := contextCause(ctx); err != nil {
		return err
	}
	if !validID(providerID) {
		return fmt.Errorf("%w: invalid provider identifier", ErrInvalidConfig)
	}
	if _, err := os.Stat(s.path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("%w: inspect models store", ErrInvalidConfig)
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
	return atomicWrite(s.path, append(all, '\n'))
}
func decodeCatalog(raw json.RawMessage, providerID string) (CachedCatalog, error) {
	var entry CachedCatalog
	if err := json.Unmarshal(raw, &entry); err != nil {
		return entry, Diagnostic{"models-store.json", providerID, "must be a catalog object"}
	}
	if err := validateCatalog(entry, providerID); err != nil {
		return entry, err
	}
	return entry, nil
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

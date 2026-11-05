// Package catalog reads and synchronizes built-in model data. It has no
// settings, credentials, provider implementation, or runtime state ownership.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const SchemaVersion = 2
const Filename = "builtin-models.json"
const maxDocumentBytes = 32 << 20

type Source struct {
	Name      string `json:"name"`
	URL       string `json:"url,omitempty"`
	Revision  string `json:"revision,omitempty"`
	Integrity string `json:"integrity"`
}

type Sources struct {
	Models Source `json:"models"`
	Policy Source `json:"policy"`
}

// Preference is ordered: the first available provider default wins.
type Preference struct {
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

type Provider struct {
	ID      string `json:"id"`
	API     string `json:"api"`
	BaseURL string `json:"baseUrl"`
	// Entries use the runtime model shape. The modelsDev member retains the
	// original source object, including fields this binary cannot yet project.
	// Model identity comes from each object's id, not its index key.
	Models map[string]map[string]json.RawMessage `json:"models"`
}

// Document is a complete built-in snapshot, never an overlay on user config.
// A single file allows models, defaults and provenance to be published together.
type Document struct {
	SchemaVersion int          `json:"schemaVersion"`
	Sources       Sources      `json:"sources"`
	Defaults      []Preference `json:"defaults"`
	Providers     []Provider   `json:"providers"`
}

func Decode(raw []byte) (Document, error) {
	var doc Document
	if len(raw) > maxDocumentBytes {
		return doc, fmt.Errorf("built-in model catalog is too large")
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return doc, fmt.Errorf("decode built-in model catalog: %w", err)
	}
	if doc.SchemaVersion == 1 {
		// Installed snapshots from older binaries remain readable. This is
		// a local format migration, not an updater for the former data source.
		var legacy struct {
			Sources struct {
				Models   legacySource `json:"models"`
				Defaults legacySource `json:"defaults"`
			} `json:"sources"`
		}
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return doc, err
		}
		doc.Sources = Sources{Models: legacy.Sources.Models.source(), Policy: legacy.Sources.Defaults.source()}
		doc.SchemaVersion = SchemaVersion
	}
	return doc, doc.Validate()
}

type legacySource struct {
	Package   string `json:"package"`
	Version   string `json:"version"`
	Integrity string `json:"integrity"`
}

func (s legacySource) source() Source {
	if s.Version == "" {
		return Source{}
	}
	return Source{Name: s.Package, Revision: s.Version, Integrity: s.Integrity}
}

func Read(path string) (Document, error) {
	file, err := os.Open(path)
	if err != nil {
		return Document{}, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxDocumentBytes+1))
	if err != nil {
		return Document{}, err
	}
	return Decode(raw)
}

func (d Document) Validate() error {
	if d.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported built-in catalog schema %d", d.SchemaVersion)
	}
	for _, source := range []Source{d.Sources.Models, d.Sources.Policy} {
		if source.Name == "" || source.Integrity == "" {
			return fmt.Errorf("built-in catalog source metadata is incomplete")
		}
	}
	if len(d.Providers) == 0 || len(d.Defaults) == 0 {
		return fmt.Errorf("built-in catalog providers and defaults must be nonempty")
	}
	seen := map[string]bool{}
	for _, preference := range d.Defaults {
		if strings.TrimSpace(preference.Provider) == "" || strings.TrimSpace(preference.ModelID) == "" || seen[preference.Provider] {
			return fmt.Errorf("invalid or duplicate default provider %q", preference.Provider)
		}
		seen[preference.Provider] = true
	}
	seen = map[string]bool{}
	for _, p := range d.Providers {
		if p.ID == "" || strings.ContainsAny(p.ID, "/\\") || seen[p.ID] || p.API == "" || p.Models == nil {
			return fmt.Errorf("invalid or duplicate built-in provider %q", p.ID)
		}
		seen[p.ID] = true
		ids := map[string]bool{}
		for api, models := range p.Models {
			if api == "" || models == nil {
				return fmt.Errorf("invalid API group for %s", p.ID)
			}
			for key, raw := range models {
				var identity struct {
					ID       string `json:"id"`
					Provider string `json:"provider"`
					API      string `json:"api"`
				}
				if err := json.Unmarshal(raw, &identity); err != nil || identity.ID == "" || identity.Provider != p.ID || identity.API != api {
					return fmt.Errorf("invalid catalog model %s/%s", p.ID, key)
				}
				if ids[identity.ID] {
					return fmt.Errorf("duplicate catalog model %s/%s", p.ID, identity.ID)
				}
				ids[identity.ID] = true
			}
		}
	}
	return nil
}

// Write publishes one complete snapshot. Identical input is a no-op. Failed
// staging files are moved to the OS temporary directory instead of deleted.
func Write(ctx context.Context, path string, doc Document) (changed bool, err error) {
	if err := doc.Validate(); err != nil {
		return false, err
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	raw = append(raw, '\n')
	if len(raw) > maxDocumentBytes {
		return false, fmt.Errorf("built-in model catalog is too large")
	}
	previous, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return false, readErr
	}
	if bytes.Equal(previous, raw) {
		return false, nil
	}
	if err := context.Cause(ctx); err != nil {
		return false, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	file, err := os.CreateTemp(dir, ".builtin-models-*")
	if err != nil {
		return false, err
	}
	temporary := file.Name()
	defer func() {
		_ = file.Close()
		if !changed {
			destination := filepath.Join(os.TempDir(), filepath.Base(temporary))
			if moveErr := os.Rename(temporary, destination); moveErr != nil {
				err = errors.Join(err, fmt.Errorf("staging catalog remains at %s: %w", temporary, moveErr))
			}
		}
	}()
	if _, err = file.Write(raw); err != nil {
		return false, err
	}
	if err = file.Sync(); err != nil {
		return false, err
	}
	if err = file.Close(); err != nil {
		return false, err
	}
	if err = context.Cause(ctx); err != nil {
		return false, err
	}
	if err = os.Rename(temporary, path); err != nil {
		return false, err
	}
	changed = true
	directory, err := os.Open(dir)
	if err == nil {
		err = directory.Sync()
		err = errors.Join(err, directory.Close())
	}
	if err != nil {
		return true, fmt.Errorf("catalog was published, but directory durability is unconfirmed: %w", err)
	}
	return true, nil
}

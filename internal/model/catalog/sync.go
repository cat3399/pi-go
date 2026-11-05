package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"time"
)

const ModelsDevURL = "https://models.dev/api.json"
const maxSourceBytes = 64 << 20

// Sync reads the public models.dev catalog, or replays a saved response. The
// locally shipped policy owns provider scope, routing, defaults and exceptions.
func Sync(ctx context.Context, policy Policy, sourceFile string) (Document, error) {
	return syncCatalog(ctx, &http.Client{Timeout: 30 * time.Second}, policy, sourceFile)
}

func syncCatalog(ctx context.Context, client *http.Client, policy Policy, sourceFile string) (Document, error) {
	if err := policy.Validate(); err != nil {
		return Document{}, err
	}
	if err := context.Cause(ctx); err != nil {
		return Document{}, err
	}
	raw, err := readSource(ctx, client, sourceFile)
	if err != nil {
		return Document{}, err
	}
	return projectModelsDev(ctx, policy, raw)
}

func readSource(ctx context.Context, client *http.Client, sourceFile string) ([]byte, error) {
	var reader io.ReadCloser
	if sourceFile != "" {
		file, err := os.Open(sourceFile)
		if err != nil {
			return nil, fmt.Errorf("read models.dev snapshot: %w", err)
		}
		reader = file
	} else {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelsDevURL, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Accept", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("fetch models.dev: %w", err)
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return nil, fmt.Errorf("models.dev returned HTTP %d", response.StatusCode)
		}
		reader = response.Body
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, maxSourceBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read models.dev: %w", err)
	}
	if len(raw) > maxSourceBytes {
		return nil, fmt.Errorf("models.dev catalog exceeds %d bytes", maxSourceBytes)
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	return raw, nil
}

func projectModelsDev(ctx context.Context, policy Policy, raw []byte) (Document, error) {
	var sources map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sources); err != nil || len(sources) == 0 {
		return Document{}, fmt.Errorf("models.dev must return a nonempty provider object")
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return Document{}, err
	}
	result := Document{SchemaVersion: SchemaVersion, Defaults: append([]Preference(nil), policy.Defaults...), Sources: Sources{
		Models: Source{Name: "models.dev", URL: ModelsDevURL, Integrity: contentIntegrity(raw)},
		Policy: Source{Name: "pi-go", Integrity: contentIntegrity(policyJSON)},
	}}
	for _, rule := range policy.Providers {
		if err := context.Cause(ctx); err != nil {
			return Document{}, err
		}
		entry := Provider{ID: rule.ID, API: rule.API, BaseURL: rule.BaseURL,
			Models: map[string]map[string]json.RawMessage{rule.API: {}}}
		if rule.Source == "" {
			for _, raw := range rule.Models {
				var identity struct{ ID string }
				if err := json.Unmarshal(raw, &identity); err != nil {
					return Document{}, err
				}
				entry.Models[rule.API][identity.ID] = bytes.Clone(raw)
			}
		} else {
			var source struct {
				ID     string                     `json:"id"`
				Models map[string]json.RawMessage `json:"models"`
			}
			providerJSON, exists := sources[rule.Source]
			if !exists {
				return Document{}, fmt.Errorf("models.dev is missing provider %q", rule.Source)
			}
			if err := json.Unmarshal(providerJSON, &source); err != nil || source.Models == nil || (source.ID != "" && source.ID != rule.Source) {
				return Document{}, fmt.Errorf("models.dev provider %q has an invalid identity or models object", rule.Source)
			}
			ids := make([]string, 0, len(source.Models))
			for id := range source.Models {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			for _, id := range ids {
				model, err := projectSourceModel(rule, id, source.Models[id])
				if err != nil {
					return Document{}, fmt.Errorf("models.dev %s/%s: %w", rule.Source, id, err)
				}
				if model != nil {
					entry.Models[rule.API][id] = model
				}
			}
		}
		result.Providers = append(result.Providers, entry)
	}
	return result, result.Validate()
}

func contentIntegrity(raw []byte) string {
	digest := sha256.Sum256(raw)
	return "sha256-" + hex.EncodeToString(digest[:])
}

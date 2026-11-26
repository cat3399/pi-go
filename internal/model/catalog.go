package model

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/cat3399/pi-go/internal/model/catalog"
	"github.com/cat3399/pi-go/internal/provider"
)

//go:embed catalogdata/catalog.json
var embeddedCatalogJSON []byte

//go:embed catalogdata/policy.json
var embeddedCatalogPolicyJSON []byte

// builtinCatalog is an immutable input to Runtime's existing composition.
// Neither installed data nor this projection owns user settings or state.
type builtinCatalog struct {
	models    []Model
	providers []ProviderConfig
	defaults  []ProviderDefault
}

var embeddedBuiltinCatalog = sync.OnceValue(func() *builtinCatalog {
	document, err := catalog.Decode(embeddedCatalogJSON)
	if err != nil {
		panic(err)
	}
	result, err := projectBuiltinCatalog(document)
	if err != nil {
		panic(err)
	}
	return result
})

func projectBuiltinCatalog(document catalog.Document) (*builtinCatalog, error) {
	if err := document.Validate(); err != nil {
		return nil, err
	}
	registrations := builtinProviderRegistrations()
	// An installed snapshot may predate a newly supported provider. Preserve
	// its existing models and add only missing provider entries from this build.
	if len(document.Providers) < len(registrations) {
		embedded, err := catalog.Decode(embeddedCatalogJSON)
		if err != nil {
			return nil, err
		}
		present := make(map[string]bool, len(document.Providers))
		registered := make(map[string]bool, len(registrations))
		for _, entry := range document.Providers {
			present[entry.ID] = true
		}
		for _, entry := range registrations {
			registered[entry.ID] = true
		}
		document.Providers = append([]catalog.Provider(nil), document.Providers...)
		for _, entry := range embedded.Providers {
			if registered[entry.ID] && !present[entry.ID] {
				document.Providers = append(document.Providers, entry)
			}
		}
	}
	if len(document.Providers) != len(registrations) {
		return nil, fmt.Errorf("built-in catalog must contain every registered provider")
	}
	registered := make(map[string]ProviderConfig, len(registrations))
	for _, p := range registrations {
		registered[p.ID] = p
	}
	result := &builtinCatalog{defaults: append([]ProviderDefault(nil), document.Defaults...)}
	for _, entry := range document.Providers {
		p, exists := registered[entry.ID]
		if !exists {
			return nil, fmt.Errorf("built-in catalog provider %q is not registered by this binary", entry.ID)
		}
		p.API, p.BaseURL = entry.API, entry.BaseURL
		result.providers = append(result.providers, p)
		for _, models := range entry.Models {
			for _, raw := range models {
				model, err := projectBuiltinModel(entry.ID, raw)
				if err != nil {
					return nil, err
				}
				result.models = append(result.models, model)
			}
		}
	}
	sort.Slice(result.models, func(i, j int) bool {
		if result.models[i].Provider != result.models[j].Provider {
			return result.models[i].Provider < result.models[j].Provider
		}
		return result.models[i].ID < result.models[j].ID
	})
	return result, nil
}

// Built-in metadata uses the runtime Model shape. In particular, an empty baseUrl
// is valid for Azure; models.json's nonempty user-property schema is different.
type catalogModel struct {
	Provider             string                             `json:"provider"`
	ID                   string                             `json:"id"`
	Name                 string                             `json:"name"`
	API                  string                             `json:"api"`
	BaseURL              string                             `json:"baseUrl"`
	Headers              map[string]string                  `json:"headers"`
	Reasoning            bool                               `json:"reasoning"`
	ThinkingLevelMap     map[provider.ThinkingLevel]*string `json:"thinkingLevelMap"`
	DefaultThinkingLevel provider.ThinkingLevel             `json:"defaultThinkingLevel"`
	Input                []provider.InputKind               `json:"input"`
	Cost                 provider.CostRates                 `json:"cost"`
	ContextWindow        uint64                             `json:"contextWindow"`
	MaxTokens            uint64                             `json:"maxTokens"`
	Compat               json.RawMessage                    `json:"compat"`
}

func projectBuiltinModel(providerID string, raw json.RawMessage) (Model, error) {
	var wire catalogModel
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Model{}, fmt.Errorf("built-in catalog %s: %w", providerID, err)
	}
	id := wire.ID
	model := Model{Provider: wire.Provider, ID: wire.ID, Name: wire.Name, API: wire.API, BaseURL: wire.BaseURL,
		Headers: wire.Headers, Reasoning: wire.Reasoning, ThinkingLevelMap: wire.ThinkingLevelMap, DefaultThinkingLevel: wire.DefaultThinkingLevel, Input: wire.Input,
		Cost: wire.Cost, ContextWindow: wire.ContextWindow, MaxTokens: wire.MaxTokens, compatRaw: bytes.Clone(wire.Compat)}
	if len(wire.Compat) != 0 {
		var err error
		model.Compat, err = decodeCompat(wire.Compat, providerID+"/"+id, wire.API)
		if err != nil {
			return Model{}, err
		}
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return Model{}, err
	}
	for key := range object {
		switch key {
		case "provider", "id", "name", "api", "baseUrl", "headers", "reasoning", "thinkingLevelMap", "defaultThinkingLevel", "input", "cost", "contextWindow", "maxTokens", "compat", "modelsDev":
		default:
			model.UnknownFields = append(model.UnknownFields, key)
		}
	}
	sort.Strings(model.UnknownFields)
	if _, err := model.Ref(); err != nil {
		return Model{}, fmt.Errorf("built-in catalog %s/%s: %w", providerID, id, err)
	}
	return model, nil
}

func loadBuiltinCatalog(path string, previous *builtinCatalog) (*builtinCatalog, error) {
	document, err := catalog.Read(path)
	if errors.Is(err, os.ErrNotExist) {
		return embeddedBuiltinCatalog(), nil
	}
	if err == nil {
		var loaded *builtinCatalog
		loaded, err = projectBuiltinCatalog(document)
		if err == nil {
			return loaded, nil
		}
	}
	if previous == nil {
		previous = embeddedBuiltinCatalog()
	}
	return previous, fmt.Errorf("load %s (retaining the last healthy built-in catalog): %w", path, err)
}

func (c *builtinCatalog) provider(id string) ProviderConfig {
	for _, p := range c.providers {
		if p.ID == id {
			return p
		}
	}
	return ProviderConfig{}
}

// LoadBuiltinModels reads only the baseline for catalog suggestions, with the
// same embedded fallback as a fresh Runtime. Discovery must not depend on the
// working directory, provider credentials, user overrides or selection filters.
func LoadBuiltinModels(agentDir string) []Model {
	baseline, _ := loadBuiltinCatalog(filepath.Join(agentDir, catalog.Filename), nil)
	return cloneModels(baseline.models)
}

func builtinModels() []Model {
	models := append([]Model(nil), embeddedBuiltinCatalog().models...)
	for i := range models {
		models[i] = cloneModel(models[i])
	}
	return models
}

func builtinProviderConfigs() []ProviderConfig {
	configs := append([]ProviderConfig(nil), embeddedBuiltinCatalog().providers...)
	for i := range configs {
		configs[i] = cloneProvider(configs[i])
	}
	return configs
}

// SyncBuiltinCatalog updates only the target built-in data file. It does not
// modify adjacent settings, user models, authentication, or provider caches.
// CLI, development sync and the Application service share publication and reporting.
func SyncBuiltinCatalog(ctx context.Context, target string, options catalog.SyncOptions) (catalog.SyncResult, error) {
	baseline, err := catalog.Decode(embeddedCatalogJSON)
	if err != nil {
		return catalog.SyncResult{}, err
	}
	previous, err := catalog.Read(target)
	if errors.Is(err, os.ErrNotExist) {
		previous = baseline
	} else if err != nil {
		return catalog.SyncResult{}, err
	}
	if _, err := projectBuiltinCatalog(previous); err != nil {
		return catalog.SyncResult{}, err
	}
	policy, err := catalog.DecodePolicy(embeddedCatalogPolicyJSON)
	if err != nil {
		return catalog.SyncResult{}, err
	}
	updated, err := catalog.Sync(ctx, policy, options)
	if err != nil {
		return catalog.SyncResult{}, err
	}
	if _, err := projectBuiltinCatalog(updated); err != nil {
		return catalog.SyncResult{}, err
	}
	result, err := catalog.Compare(previous, updated).Report()
	if err != nil {
		return catalog.SyncResult{}, fmt.Errorf("catalog not updated: %w", err)
	}
	result.Published, err = catalog.Write(ctx, target, updated)
	if err != nil && result.FullOutputPath != "" {
		err = fmt.Errorf("%w (full change report: %s)", err, result.FullOutputPath)
	}
	if err == nil && !result.Published {
		result.Summary += "Already up to date.\n"
	}
	return result, err
}

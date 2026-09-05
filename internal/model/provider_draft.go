package model

import "encoding/json"

// ParseProviderDraft applies the same models.json parser, builtin baseline,
// provider inheritance, compatibility decoding, and model defaults used by
// Runtime without touching disk. Configuration UIs use it to probe an unsaved
// provider/model draft through the real production adapter graph.
func ParseProviderDraft(providerID string, raw json.RawMessage) (ProviderConfig, []Model, error) {
	return parseProviderDraft(providerID, raw, embeddedBuiltinCatalog())
}

// ParseProviderDraft uses the same installed built-in snapshot as this Runtime.
func (r *Runtime) ParseProviderDraft(providerID string, raw json.RawMessage) (ProviderConfig, []Model, error) {
	r.mu.RLock()
	builtins := r.builtins
	r.mu.RUnlock()
	return parseProviderDraft(providerID, raw, builtins)
}

func parseProviderDraft(providerID string, raw json.RawMessage, builtins *builtinCatalog) (ProviderConfig, []Model, error) {
	configured, err := parseProvider(providerID, raw, builtins)
	if err != nil {
		return ProviderConfig{}, nil, err
	}
	baseline := make([]Model, 0)
	for _, candidate := range builtins.models {
		if candidate.Provider == providerID {
			baseline = append(baseline, candidate)
		}
	}
	if err := validateConfiguredProvider(configured, baseline); err != nil {
		return ProviderConfig{}, nil, err
	}
	snapshot := buildSnapshot(map[string]ProviderConfig{providerID: configured}, nil, Settings{}, builtins)
	models := make([]Model, 0)
	for _, candidate := range snapshot.Models {
		if candidate.Provider == providerID {
			models = append(models, candidate)
		}
	}
	return cloneProvider(configured), models, nil
}

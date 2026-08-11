package model

import "encoding/json"

// ParseProviderDraft applies the same models.json parser, builtin baseline,
// provider inheritance, compatibility decoding, and model defaults used by
// Runtime without touching disk. Configuration UIs use it to probe an unsaved
// provider/model draft through the real production adapter graph.
func ParseProviderDraft(providerID string, raw json.RawMessage) (ProviderConfig, []Model, error) {
	configured, err := parseProvider(providerID, raw)
	if err != nil {
		return ProviderConfig{}, nil, err
	}
	baseline := make([]Model, 0)
	for _, candidate := range builtinModels() {
		if candidate.Provider == providerID {
			baseline = append(baseline, candidate)
		}
	}
	if err := validateConfiguredProvider(configured, baseline); err != nil {
		return ProviderConfig{}, nil, err
	}
	snapshot := buildSnapshot(map[string]ProviderConfig{providerID: configured}, nil, Settings{})
	models := make([]Model, 0)
	for _, candidate := range snapshot.Models {
		if candidate.Provider == providerID {
			models = append(models, candidate)
		}
	}
	return cloneProvider(configured), models, nil
}

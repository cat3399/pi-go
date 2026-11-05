package catalog

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Policy is shipped with the Go adapters, independently of generated model
// facts. An online catalog cannot register providers, choose protocols, change
// authentication endpoints, or invent subscription-only model availability.
type Policy struct {
	Version   int              `json:"version"`
	Defaults  []Preference     `json:"defaults"`
	Providers []ProviderPolicy `json:"providers"`
}

type ProviderPolicy struct {
	ID        string                     `json:"id"`
	API       string                     `json:"api"`
	BaseURL   string                     `json:"baseUrl"`
	Source    string                     `json:"source,omitempty"`
	Exclude   []string                   `json:"exclude,omitempty"`
	Overrides map[string]json.RawMessage `json:"overrides,omitempty"`
	// Models is an explicit local catalog, used only when Source is empty.
	Models []json.RawMessage `json:"models,omitempty"`
}

func DecodePolicy(raw []byte) (Policy, error) {
	var policy Policy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return policy, fmt.Errorf("decode built-in model policy: %w", err)
	}
	return policy, policy.Validate()
}

func (p Policy) Validate() error {
	if p.Version != 1 {
		return fmt.Errorf("unsupported built-in model policy %d", p.Version)
	}
	// Reuse document validation for provider identities and default ordering.
	doc := Document{SchemaVersion: SchemaVersion, Defaults: p.Defaults,
		Sources: Sources{Models: Source{Name: "policy", Integrity: "local"}, Policy: Source{Name: "policy", Integrity: "local"}}}
	for _, rule := range p.Providers {
		entry := Provider{ID: rule.ID, API: rule.API, BaseURL: rule.BaseURL,
			Models: map[string]map[string]json.RawMessage{rule.API: {}}}
		if (rule.Source == "") == (len(rule.Models) == 0) {
			return fmt.Errorf("provider %s must have either a models.dev source or local models", rule.ID)
		}
		for i, raw := range rule.Models {
			entry.Models[rule.API][strconv.Itoa(i)] = raw
		}
		for id, raw := range rule.Overrides {
			var fields map[string]json.RawMessage
			if id == "" || json.Unmarshal(raw, &fields) != nil || fields == nil {
				return fmt.Errorf("invalid model policy override %s/%s", rule.ID, id)
			}
			for key := range fields {
				// Routing and identity belong to the provider registration.
				if key == "id" || key == "provider" || key == "api" || key == "baseUrl" || key == "modelsDev" {
					return fmt.Errorf("model policy override %s/%s cannot change %s", rule.ID, id, key)
				}
			}
		}
		doc.Providers = append(doc.Providers, entry)
	}
	return doc.Validate()
}

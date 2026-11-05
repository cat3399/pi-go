package catalog

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/cat3399/pi-go/internal/provider"
)

// The public v1 response is independent of models.dev's repository layout,
// TOML schema and SDK packages. SDK names below are data, never dependencies.
type sourceModel struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Type             string `json:"type"`
	Status           string `json:"status"`
	CanonicalModelID string `json:"canonical_model_id"`
	ToolCall         bool   `json:"tool_call"`
	Reasoning        bool   `json:"reasoning"`
	Temperature      *bool  `json:"temperature"`
	ReasoningOptions []struct {
		Type   string    `json:"type"`
		Values []*string `json:"values"`
	} `json:"reasoning_options"`
	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	Limit struct {
		Context uint64 `json:"context"`
		Output  uint64 `json:"output"`
	} `json:"limit"`
	Cost struct {
		sourceRates
		Tiers []struct {
			sourceRates
			Tier struct {
				Type string  `json:"type"`
				Size float64 `json:"size"`
			} `json:"tier"`
		} `json:"tiers"`
		ContextOver200K *sourceRates `json:"context_over_200k"`
	} `json:"cost"`
	Provider struct {
		NPM string `json:"npm"`
	} `json:"provider"`
}

type sourceRates struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

func (r sourceRates) rates() (provider.CostRates, error) {
	if r.Input < 0 || r.Output < 0 || r.CacheRead < 0 || r.CacheWrite < 0 {
		return provider.CostRates{}, fmt.Errorf("negative token price")
	}
	return provider.CostRates{Input: r.Input, Output: r.Output, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite}, nil
}

func projectSourceModel(rule ProviderPolicy, id string, raw json.RawMessage) (json.RawMessage, error) {
	var source sourceModel
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, err
	}
	if id == "" || source.ID != id {
		return nil, fmt.Errorf("model identity does not match its catalog key")
	}
	if !source.ToolCall || (source.Type != "" && source.Type != "chat") || slices.Contains(rule.Exclude, id) {
		return nil, nil
	}
	if len(source.Modalities.Output) > 0 && !slices.Contains(source.Modalities.Output, "text") {
		return nil, nil
	}
	if rule.ID == "together" && source.Status == "deprecated" {
		return nil, nil
	}
	if rule.ID == "azure-openai-responses" && !azureResponsesModel(source) {
		return nil, nil
	}
	cost, err := source.Cost.sourceRates.rates()
	if err != nil {
		return nil, err
	}
	for _, tier := range source.Cost.Tiers {
		if tier.Tier.Type != "context" {
			continue // The original object retains unsupported pricing dimensions.
		}
		rates, err := tier.sourceRates.rates()
		if err != nil || tier.Tier.Size <= 0 {
			return nil, fmt.Errorf("invalid context price tier")
		}
		cost.Tiers = append(cost.Tiers, provider.CostTier{InputTokensAbove: tier.Tier.Size,
			Input: rates.Input, Output: rates.Output, CacheRead: rates.CacheRead, CacheWrite: rates.CacheWrite})
	}
	if len(source.Cost.Tiers) == 0 && source.Cost.ContextOver200K != nil {
		rates, err := source.Cost.ContextOver200K.rates()
		if err != nil {
			return nil, err
		}
		cost.Tiers = append(cost.Tiers, provider.CostTier{InputTokensAbove: 200000,
			Input: rates.Input, Output: rates.Output, CacheRead: rates.CacheRead, CacheWrite: rates.CacheWrite})
	}
	name := source.Name
	if name == "" {
		name = id
	}
	input := []string{"text"}
	if slices.Contains(source.Modalities.Input, "image") {
		input = append(input, "image")
	}
	contextWindow, maxTokens := source.Limit.Context, source.Limit.Output
	// Match the model generator's defaults for older/incomplete metadata.
	if contextWindow == 0 {
		contextWindow = 4096
	}
	if maxTokens == 0 {
		maxTokens = 4096
	}
	compat, thinking := modelControls(rule, source)
	entry := map[string]any{
		"id": id, "name": name, "provider": rule.ID, "api": rule.API, "baseUrl": rule.BaseURL,
		"type": "chat", "reasoning": source.Reasoning, "input": input, "cost": cost,
		"contextWindow": contextWindow, "maxTokens": maxTokens, "modelsDev": raw,
	}
	if len(compat) > 0 {
		entry["compat"] = compat
	}
	if len(thinking) > 0 {
		entry["thinkingLevelMap"] = thinking
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	if patch := rule.Overrides[id]; len(patch) > 0 {
		var overrides map[string]json.RawMessage
		if err := json.Unmarshal(patch, &overrides); err != nil {
			return nil, err
		}
		// Explicit local controls are complete model-specific contracts. In
		// particular, absent thinking levels retain their runtime defaults;
		// null levels must stay null instead of acting as JSON merge deletions.
		for key, value := range overrides {
			fields[key] = value
		}
	}
	return json.Marshal(fields)
}

func azureResponsesModel(model sourceModel) bool {
	if model.Provider.NPM != "" && model.Provider.NPM != "@ai-sdk/azure" && model.Provider.NPM != "@ai-sdk/openai" {
		return false
	}
	// Foundry's Anthropic/Chat Completions models require different adapters.
	return strings.HasPrefix(model.CanonicalModelID, "openai/") || strings.HasPrefix(model.ID, "gpt-") ||
		strings.HasPrefix(model.ID, "codex-") || strings.HasPrefix(model.ID, "o1") ||
		strings.HasPrefix(model.ID, "o3") || strings.HasPrefix(model.ID, "o4")
}

func modelControls(rule ProviderPolicy, model sourceModel) (map[string]any, map[string]*string) {
	compat := map[string]any{}
	thinking := effortMap(model)
	switch rule.ID {
	case "openai", "azure-openai-responses":
		if rule.ID == "openai" {
			compat["supportsStrictMode"] = true
			if model.Cost.CacheWrite > 0 {
				compat["supportsExplicitPromptCacheMode"] = true
			}
		}
		if strings.HasPrefix(model.ID, "gpt-") {
			major, _, _ := strings.Cut(strings.TrimPrefix(model.ID, "gpt-"), ".")
			major, _, _ = strings.Cut(major, "-")
			if version, err := strconv.Atoi(major); err == nil && version >= 5 {
				compat["supportsOpenAIGrammarTools"] = true
			}
		}
	case "anthropic":
		compat["supportsStrictTools"] = true
		for _, option := range model.ReasoningOptions {
			if option.Type == "budget_tokens" {
				// Effort can coexist with a token budget (for example Opus
				// 4.5). That does not make the API's adaptive mode available.
				thinking = nil
				break
			}
		}
		if len(thinking) > 0 {
			compat["forceAdaptiveThinking"] = true
		}
		if model.Temperature != nil && !*model.Temperature {
			compat["supportsTemperature"] = false
		}
	case "deepseek":
		compat = map[string]any{"supportsStore": false, "supportsDeveloperRole": false, "supportsStrictMode": true,
			"maxTokensField": "max_tokens", "thinkingFormat": "deepseek", "requiresReasoningContentOnAssistantMessages": true}
		for _, option := range model.ReasoningOptions {
			if option.Type == "toggle" {
				delete(thinking, "off") // Off uses the protocol's toggle, not an effort value.
			}
		}
	case "xai":
		compat["supportsLongCacheRetention"] = false
	case "groq":
		compat["supportsStrictMode"] = true
	case "cerebras":
		compat["supportsStore"], compat["supportsDeveloperRole"] = false, false
	case "together":
		compat = map[string]any{"supportsStore": false, "supportsDeveloperRole": false, "supportsReasoningEffort": false,
			"maxTokensField": "max_tokens", "supportsStrictMode": false, "supportsLongCacheRetention": false}
		if model.Reasoning {
			compat["thinkingFormat"] = "together"
			thinking = map[string]*string{"minimal": nil, "low": nil, "medium": nil}
			if strings.HasPrefix(model.ID, "openai/gpt-oss-") {
				compat["thinkingFormat"], compat["supportsReasoningEffort"] = "openai", true
				thinking = effortMap(model)
			} else if model.ID == "deepseek-ai/DeepSeek-R1" || model.ID == "MiniMaxAI/MiniMax-M2.7" {
				thinking["off"] = nil
			}
		}
	}
	return compat, thinking
}

// This is the provider-neutral conversion used by pi's model generator. Values
// without a selectable equivalent (default and null) are deliberately omitted.
func effortMap(model sourceModel) map[string]*string {
	if !model.Reasoning {
		return nil
	}
	supported := map[string]bool{}
	for _, option := range model.ReasoningOptions {
		if option.Type == "effort" {
			for _, value := range option.Values {
				if value != nil {
					supported[*value] = true
				}
			}
		}
	}
	result := map[string]*string{}
	hasKnownValue := false
	for _, level := range []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"} {
		value := level
		if level == "off" {
			value = "none"
		}
		result[level] = nil
		if supported[value] {
			result[level] = &value
			hasKnownValue = true
		}
	}
	if !hasKnownValue {
		return nil
	}
	return result
}

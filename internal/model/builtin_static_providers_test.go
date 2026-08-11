package model

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/cat3399/pi-go/internal/provider"
)

func TestBuiltinStaticProviderMetadataMatchesPiAI083(t *testing.T) {
	want := map[string]ProviderConfig{
		"deepseek": {
			ID: "deepseek", Name: "DeepSeek", API: OpenAICompletionsAPI,
			BaseURL: "https://api.deepseek.com", APIKeyEnvironment: []string{"DEEPSEEK_API_KEY"},
		},
		"xai": {
			ID: "xai", Name: "xAI", API: OpenAICompletionsAPI,
			BaseURL: "https://api.x.ai/v1", APIKeyEnvironment: []string{"XAI_API_KEY"},
		},
		"groq": {
			ID: "groq", Name: "Groq", API: OpenAICompletionsAPI,
			BaseURL: "https://api.groq.com/openai/v1", APIKeyEnvironment: []string{"GROQ_API_KEY"},
		},
		"cerebras": {
			ID: "cerebras", Name: "Cerebras", API: OpenAICompletionsAPI,
			BaseURL: "https://api.cerebras.ai/v1", APIKeyEnvironment: []string{"CEREBRAS_API_KEY"},
		},
		"together": {
			ID: "together", Name: "Together", API: OpenAICompletionsAPI,
			BaseURL: "https://api.together.ai/v1", APIKeyEnvironment: []string{"TOGETHER_API_KEY"},
		},
	}

	got := make(map[string]ProviderConfig)
	for _, config := range builtinProviderConfigs() {
		if _, selected := want[config.ID]; selected {
			got[config.ID] = config
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("static Provider metadata differs from pi-ai 0.83.0\n got: %#v\nwant: %#v", got, want)
	}
}

func TestBuiltinStaticProviderCatalogOracleMatchesPiAI083(t *testing.T) {
	want := map[string]catalogOracle{
		"cerebras.json": {
			Provider: "cerebras", APIs: []string{OpenAICompletionsAPI},
			SHA256: "ff7c257444fa2635864348cbfb8ec51b4a623616bc5c3d2b47758a2d949ff9bb", Count: 3,
		},
		"deepseek.json": {
			Provider: "deepseek", APIs: []string{OpenAICompletionsAPI},
			SHA256: "d8443a8cb5f33377b3bc18d9d923c4e18c9d2fe53d931d1b108ecf4e02f8a01b", Count: 2,
		},
		"groq.json": {
			Provider: "groq", APIs: []string{OpenAICompletionsAPI},
			SHA256: "ff2649b13ac97ffe9383fcb5bb34f7b04b5bbb040a7e5d2030e3434898ca99d3", Count: 7,
		},
		"together.json": {
			Provider: "together", APIs: []string{OpenAICompletionsAPI},
			SHA256: "2c7cacebb0af3d6d62bf919d5181e8c17b8675bceb0084a4059d5fecd216ccab", Count: 17,
		},
		"xai.json": {
			Provider: "xai", APIs: []string{OpenAICompletionsAPI, OpenAIResponsesAPI},
			SHA256: "0e1176eb8529d6591fe352fd6ed790c580ad0a81301f02ffce8319e0181eb494", Count: 3,
		},
	}

	for file, expected := range want {
		actual, ok := generatedCatalogOracle[file]
		if !ok || !reflect.DeepEqual(actual, expected) {
			t.Fatalf("oracle %q = %#v, want %#v", file, actual, expected)
		}
		raw, err := generatedCatalogData.ReadFile("catalogdata/" + file)
		if err != nil {
			t.Fatalf("read embedded oracle %q: %v", file, err)
		}
		digest := sha256.Sum256(raw)
		if got := hex.EncodeToString(digest[:]); got != expected.SHA256 {
			t.Fatalf("oracle %q SHA-256 = %q, want %q", file, got, expected.SHA256)
		}
	}
}

func TestBuiltinStaticProviderModelsPreservePiAI083Fields(t *testing.T) {
	wantCounts := map[string]int{"deepseek": 2, "xai": 3, "groq": 7, "cerebras": 3, "together": 17}
	counts := make(map[string]int, len(wantCounts))
	models := make(map[string]Model)
	for _, candidate := range generatedBuiltinModels() {
		if _, selected := wantCounts[candidate.Provider]; !selected {
			continue
		}
		counts[candidate.Provider]++
		models[candidate.Provider+"/"+candidate.ID] = candidate
		if _, err := candidate.Ref(); err != nil {
			t.Fatalf("generated model %s/%s is incomplete: %v", candidate.Provider, candidate.ID, err)
		}
	}
	if !reflect.DeepEqual(counts, wantCounts) {
		t.Fatalf("static Provider model counts = %#v, want %#v", counts, wantCounts)
	}

	deepseek := models["deepseek/deepseek-v4-pro"]
	if deepseek.Name != "DeepSeek V4 Pro" || deepseek.API != OpenAICompletionsAPI || deepseek.BaseURL != "https://api.deepseek.com" ||
		!deepseek.Reasoning || !reflect.DeepEqual(deepseek.Input, []provider.InputKind{provider.InputText}) ||
		!reflect.DeepEqual(deepseek.Cost, provider.CostRates{Input: 0.435, Output: 0.87, CacheRead: 0.003625}) ||
		deepseek.ContextWindow != 1_000_000 || deepseek.MaxTokens != 384_000 {
		t.Fatalf("deepseek-v4-pro fields = %#v", deepseek)
	}
	deepseekCompat := deepseek.Compat.OpenAICompletions
	if deepseekCompat == nil || !boolValue(deepseekCompat.RequiresReasoningContentOnAssistantMessages) ||
		stringValue(deepseekCompat.ThinkingFormat) != "deepseek" || stringValue(deepseek.ThinkingLevelMap[provider.ThinkingHigh]) != "high" {
		t.Fatalf("deepseek-v4-pro compat/thinking = %#v / %#v", deepseek.Compat, deepseek.ThinkingLevelMap)
	}

	xai := models["xai/grok-4.5"]
	if xai.Name != "Grok 4.5" || xai.API != OpenAIResponsesAPI || xai.BaseURL != "https://api.x.ai/v1" ||
		!xai.Reasoning || !reflect.DeepEqual(xai.Input, []provider.InputKind{provider.InputText, provider.InputImage}) ||
		!reflect.DeepEqual(xai.Cost, provider.CostRates{Input: 2, Output: 6, CacheRead: 0.3}) ||
		xai.ContextWindow != 500_000 || xai.MaxTokens != 500_000 || xai.Compat.OpenAIResponses == nil ||
		xai.Compat.OpenAIResponses.SupportsLongCacheRetention == nil || *xai.Compat.OpenAIResponses.SupportsLongCacheRetention {
		t.Fatalf("grok-4.5 fields = %#v", xai)
	}

	groq := models["groq/qwen/qwen3-32b"]
	if groq.Name != "Qwen3-32B" || groq.API != OpenAICompletionsAPI || !groq.Reasoning ||
		!reflect.DeepEqual(groq.Cost, provider.CostRates{Input: 0.29, Output: 0.59}) || groq.ContextWindow != 131_072 || groq.MaxTokens != 40_960 ||
		stringValue(groq.ThinkingLevelMap[provider.ThinkingOff]) != "none" || stringValue(groq.ThinkingLevelMap[provider.ThinkingHigh]) != "default" {
		t.Fatalf("qwen/qwen3-32b fields = %#v", groq)
	}

	cerebras := models["cerebras/gemma-4-31b"]
	if cerebras.Name != "Gemma 4 31B IT" || cerebras.API != OpenAICompletionsAPI ||
		!reflect.DeepEqual(cerebras.Input, []provider.InputKind{provider.InputText, provider.InputImage}) ||
		cerebras.ContextWindow != 131_072 || cerebras.MaxTokens != 40_960 || cerebras.Compat.OpenAICompletions == nil ||
		cerebras.Compat.OpenAICompletions.SupportsDeveloperRole == nil || *cerebras.Compat.OpenAICompletions.SupportsDeveloperRole {
		t.Fatalf("gemma-4-31b fields = %#v", cerebras)
	}

	together := models["together/MiniMaxAI/MiniMax-M3"]
	if together.Name != "MiniMax-M3" || together.API != OpenAICompletionsAPI || together.BaseURL != "https://api.together.ai/v1" ||
		!together.Reasoning || !reflect.DeepEqual(together.Input, []provider.InputKind{provider.InputText, provider.InputImage}) ||
		!reflect.DeepEqual(together.Cost, provider.CostRates{Input: 0.3, Output: 1.2, CacheRead: 0.06}) ||
		together.ContextWindow != 524_288 || together.MaxTokens != 250_000 {
		t.Fatalf("MiniMaxAI/MiniMax-M3 fields = %#v", together)
	}
	togetherCompat := together.Compat.OpenAICompletions
	if togetherCompat == nil || stringValue(togetherCompat.MaxTokensField) != "max_tokens" ||
		stringValue(togetherCompat.ThinkingFormat) != "together" || boolValue(togetherCompat.SupportsReasoningEffort) {
		t.Fatalf("MiniMaxAI/MiniMax-M3 compat = %#v", together.Compat)
	}
}

func boolValue(value *bool) bool { return value != nil && *value }

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

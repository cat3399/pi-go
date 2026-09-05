package model

import (
	"encoding/json"
	"github.com/cat3399/pi-go/internal/model/catalog"
	"os"
	"reflect"
	"testing"

	"github.com/cat3399/pi-go/internal/provider"
)

func TestBuiltinProviderMetadataComesFromCatalog(t *testing.T) {
	doc, err := catalog.Decode(embeddedCatalogJSON)
	if err != nil {
		t.Fatal(err)
	}
	providers := map[string]ProviderConfig{}
	for _, p := range builtinProviderConfigs() {
		providers[p.ID] = p
	}
	for _, entry := range doc.Providers {
		p, ok := providers[entry.ID]
		if !ok || p.API != entry.API || p.BaseURL != entry.BaseURL {
			t.Fatalf("provider %s did not use catalog metadata: %#v", entry.ID, p)
		}
	}
}

// Frozen upstream examples exercise data projection independently of live
// catalog churn. Syncing built-ins never rewrites this behavioral fixture.
func fixtureBuiltinModels(t *testing.T) []Model {
	t.Helper()
	raw, err := os.ReadFile("testdata/catalog-models.json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	var result []Model
	for _, entry := range entries {
		var identity struct{ Provider, ID string }
		if err := json.Unmarshal(entry, &identity); err != nil {
			t.Fatal(err)
		}
		m, err := projectBuiltinModel(identity.Provider, identity.ID, entry)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, m)
	}
	return result
}

func testDefaultModelID(t *testing.T, providerID string) string {
	t.Helper()
	id, ok := DefaultModelID(providerID)
	if !ok {
		t.Fatalf("missing default for %s", providerID)
	}
	return id
}

func TestUpstreamModelFixturesPreserveFields(t *testing.T) {
	models := make(map[string]Model)
	for _, candidate := range fixtureBuiltinModels(t) {
		models[candidate.Provider+"/"+candidate.ID] = candidate
	}

	azure := models[AzureOpenAIProviderID+"/"+"gpt-5.4"]
	azureOff, hasAzureOff := azure.ThinkingLevelMap[provider.ThinkingOff]
	azureXHigh, hasAzureXHigh := azure.ThinkingLevelMap[provider.ThinkingXHigh]
	if azure.Name != "GPT-5.4" || azure.API != AzureOpenAIResponsesAPI || azure.BaseURL != "" || !azure.Reasoning ||
		!reflect.DeepEqual(azure.Input, []provider.InputKind{provider.InputText, provider.InputImage}) ||
		!reflect.DeepEqual(azure.Cost, provider.CostRates{Input: 2.5, Output: 15, CacheRead: 0.25}) ||
		azure.ContextWindow != 1_050_000 || azure.MaxTokens != 128_000 || azure.Compat.OpenAIResponses == nil ||
		!boolValue(azure.Compat.OpenAIResponses.SupportsOpenAIGrammarTools) || !hasAzureOff || azureOff != nil ||
		!hasAzureXHigh || stringValue(azureXHigh) != "xhigh" {
		t.Fatalf("Azure GPT-5.4 fields = %#v", azure)
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
		stringValue(deepseekCompat.MaxTokensField) != "max_tokens" || stringValue(deepseekCompat.ThinkingFormat) != "deepseek" ||
		stringValue(deepseek.ThinkingLevelMap[provider.ThinkingHigh]) != "high" {
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

	groq := models["groq/qwen/qwen3.6-27b"]
	if groq.Name != "Qwen3.6 27B" || groq.API != OpenAICompletionsAPI || !groq.Reasoning ||
		!reflect.DeepEqual(groq.Input, []provider.InputKind{provider.InputText, provider.InputImage}) ||
		!reflect.DeepEqual(groq.Cost, provider.CostRates{Input: 0.6, Output: 3, CacheRead: 0.3}) || groq.ContextWindow != 131_072 || groq.MaxTokens != 16_384 ||
		stringValue(groq.ThinkingLevelMap[provider.ThinkingOff]) != "none" || stringValue(groq.ThinkingLevelMap[provider.ThinkingHigh]) != "default" {
		t.Fatalf("qwen/qwen3.6-27b fields = %#v", groq)
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

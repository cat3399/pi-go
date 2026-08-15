package provider

import (
	"encoding/json"
	"testing"
)

func TestRawReasoningEffortOptionsReachEachImplementedDialect(t *testing.T) {
	mappedLow, mappedHigh, mappedOff := "mapped-low", "mapped-high", "mapped-off"
	adaptive := true
	models := map[string]Model{
		"responses": mustParityOptionModel(t, ModelSpec{
			Provider: "openai", API: OpenAIResponsesAPI, ID: "responses", Reasoning: true,
			ThinkingLevelMap: map[ThinkingLevel]*string{ThinkingOff: &mappedOff, ThinkingLow: &mappedLow},
		}),
		"completions": mustParityOptionModel(t, ModelSpec{
			Provider: "compatible", API: OpenAICompletionsAPI, ID: "completions", Reasoning: true,
			ThinkingLevelMap: map[ThinkingLevel]*string{ThinkingHigh: &mappedHigh},
			Compat: ModelCompat{OpenAICompletions: &OpenAICompletionsCompat{
				ThinkingFormat: stringPointerForParity("deepseek"),
			}},
		}),
		"codex": mustParityOptionModel(t, ModelSpec{
			Provider: OpenAICodexProviderID, API: OpenAICodexResponsesAPI, ID: "codex", Reasoning: true,
			ThinkingLevelMap: map[ThinkingLevel]*string{ThinkingOff: &mappedOff},
		}),
		"anthropic": mustParityOptionModel(t, ModelSpec{
			Provider: AnthropicProviderID, API: AnthropicMessagesAPI, ID: "anthropic", Reasoning: true,
			ThinkingLevelMap: map[ThinkingLevel]*string{ThinkingHigh: &mappedHigh},
			Compat:           ModelCompat{AnthropicMessages: &AnthropicMessagesCompat{ForceAdaptiveThinking: &adaptive}},
		}),
	}

	summary := "concise"
	responses := mustParityOptionRequest(t, models["responses"], StreamOptions{ReasoningEffort: "low", ReasoningSummary: &summary})
	responsesBody := decodeParityOptionPayload(t, mustEncodeParityOptions(t, func() ([]byte, error) {
		return encodeOpenAIResponsesRequest(responses, "system")
	}))
	reasoning := responsesBody["reasoning"].(map[string]any)
	if reasoning["effort"] != mappedLow || reasoning["summary"] != summary {
		t.Fatalf("Responses reasoning = %#v", reasoning)
	}

	summaryOnly := mustParityOptionRequest(t, models["responses"], StreamOptions{ReasoningSummary: &summary})
	summaryOnlyBody := decodeParityOptionPayload(t, mustEncodeParityOptions(t, func() ([]byte, error) {
		return encodeOpenAIResponsesRequest(summaryOnly, "system")
	}))
	reasoning = summaryOnlyBody["reasoning"].(map[string]any)
	if reasoning["effort"] != "medium" || reasoning["summary"] != summary {
		t.Fatalf("Responses summary-only reasoning = %#v", reasoning)
	}

	completions := mustParityOptionRequest(t, models["completions"], StreamOptions{ReasoningEffort: "high"})
	completionsBody := decodeParityOptionPayload(t, mustEncodeParityOptions(t, func() ([]byte, error) {
		return encodeOpenAICompletionsRequest(completions)
	}))
	thinking := completionsBody["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || completionsBody["reasoning_effort"] != mappedHigh {
		t.Fatalf("Completions thinking = %#v, effort = %#v", thinking, completionsBody["reasoning_effort"])
	}

	codex := mustParityOptionRequest(t, models["codex"], StreamOptions{ReasoningEffort: "none", ReasoningSummary: &summary})
	codexBody := decodeParityOptionPayload(t, mustEncodeParityOptions(t, func() ([]byte, error) {
		return encodeOpenAICodexRequest(codex)
	}))
	reasoning = codexBody["reasoning"].(map[string]any)
	if reasoning["effort"] != mappedOff || reasoning["summary"] != summary {
		t.Fatalf("Codex reasoning = %#v", reasoning)
	}

	enabled := true
	anthropic := mustParityOptionRequest(t, models["anthropic"], StreamOptions{ThinkingEnabled: &enabled, AnthropicEffort: "xhigh"})
	anthropicBody := decodeParityOptionPayload(t, mustEncodeParityOptions(t, func() ([]byte, error) {
		return encodeAnthropicRequest(anthropic, false)
	}))
	outputConfig := anthropicBody["output_config"].(map[string]any)
	if outputConfig["effort"] != "xhigh" {
		t.Fatalf("Anthropic output_config = %#v", outputConfig)
	}
}

func TestRawReasoningEffortOptionsCloneMergeAndValidate(t *testing.T) {
	base := StreamOptions{ReasoningEffort: "low", AnthropicEffort: "medium", AzureAPIVersion: "v1", AzureResourceName: "old-resource", AzureBaseURL: "https://old.openai.azure.com", AzureDeploymentName: "old-deployment"}
	overlay := StreamOptions{ReasoningEffort: "high", AnthropicEffort: "xhigh", AzureAPIVersion: "2026-preview", AzureResourceName: "new-resource", AzureBaseURL: "https://new.openai.azure.com", AzureDeploymentName: "new-deployment"}
	merged := MergeStreamOptions(base, overlay)
	if merged.ReasoningEffort != "high" || merged.AnthropicEffort != "xhigh" || merged.AzureAPIVersion != "2026-preview" || merged.AzureResourceName != "new-resource" || merged.AzureBaseURL != "https://new.openai.azure.com" || merged.AzureDeploymentName != "new-deployment" {
		t.Fatalf("merged options = %#v", merged)
	}
	cloned := CloneStreamOptions(merged)
	if cloned.ReasoningEffort != merged.ReasoningEffort || cloned.AnthropicEffort != merged.AnthropicEffort || cloned.AzureAPIVersion != merged.AzureAPIVersion || cloned.AzureResourceName != merged.AzureResourceName || cloned.AzureBaseURL != merged.AzureBaseURL || cloned.AzureDeploymentName != merged.AzureDeploymentName {
		t.Fatalf("cloned options = %#v", cloned)
	}
	model := mustParityOptionModel(t, ModelSpec{Provider: "openai", API: OpenAIResponsesAPI, ID: "validation", Reasoning: true})
	for _, stream := range []StreamOptions{{ReasoningEffort: "extreme"}, {AnthropicEffort: "minimal"}, {AzureAPIVersion: "v1\ninvalid"}} {
		if _, err := NewRequestWithOptions(model, "", nil, RequestOptions{Stream: stream}); err == nil {
			t.Fatalf("invalid options accepted: %#v", stream)
		}
	}
}

func mustParityOptionModel(t *testing.T, spec ModelSpec) Model {
	t.Helper()
	if spec.Name == "" {
		spec.Name = spec.ID
	}
	if len(spec.Input) == 0 {
		spec.Input = []InputKind{InputText}
	}
	if spec.ContextWindow == 0 {
		spec.ContextWindow = 128_000
	}
	if spec.MaxTokens == 0 {
		spec.MaxTokens = 16_384
	}
	model, err := NewModel(spec)
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func mustParityOptionRequest(t *testing.T, model Model, stream StreamOptions) Request {
	t.Helper()
	request, err := NewRequestWithOptions(model, "", nil, RequestOptions{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func mustEncodeParityOptions(t *testing.T, encode func() ([]byte, error)) []byte {
	t.Helper()
	payload, err := encode()
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func decodeParityOptionPayload(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func stringPointerForParity(value string) *string { return &value }

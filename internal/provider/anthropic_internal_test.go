package provider

import "testing"

func TestDefaultAnthropicSupportsToolReferencesUsesStrictVersionSegments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider string
		modelID  string
		want     bool
	}{
		{name: "supported minor", provider: AnthropicProviderID, modelID: "claude-sonnet-4-5", want: true},
		{name: "supported major with suffix", provider: AnthropicProviderID, modelID: "claude-opus-5-latest", want: true},
		{name: "old minor", provider: AnthropicProviderID, modelID: "claude-opus-4-1"},
		{name: "date is not minor", provider: AnthropicProviderID, modelID: "claude-sonnet-4-20250514"},
		{name: "malformed major suffix", provider: AnthropicProviderID, modelID: "claude-opus-5xyz"},
		{name: "malformed minor suffix", provider: AnthropicProviderID, modelID: "claude-opus-4-5xyz"},
		{name: "haiku", provider: AnthropicProviderID, modelID: "claude-haiku-5"},
		{name: "foreign provider", provider: "gateway", modelID: "claude-opus-5"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model, err := NewModel(ModelSpec{
				Provider: test.provider, API: AnthropicMessagesAPI, ID: test.modelID, Name: test.modelID,
				Input: []InputKind{InputText}, Cost: CostRates{}, ContextWindow: 200_000, MaxTokens: 8_192,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := defaultAnthropicSupportsToolReferences(model); got != test.want {
				t.Fatalf("defaultAnthropicSupportsToolReferences() = %t, want %t", got, test.want)
			}
		})
	}
}

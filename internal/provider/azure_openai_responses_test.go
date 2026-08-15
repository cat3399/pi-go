package provider_test

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestAzureOpenAIResponsesUsesAzureWireAndBasePricing(t *testing.T) {
	var capturedURL string
	var capturedHeader http.Header
	var capturedPayload map[string]any
	body := responsesSSE(
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg_azure", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "ok"}}}},
		map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "resp_azure", "status": "completed", "service_tier": "priority",
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
		}},
	)
	client := responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
		capturedURL, capturedHeader = request.URL.String(), request.Header.Clone()
		if err := json.NewDecoder(request.Body).Decode(&capturedPayload); err != nil {
			return nil, err
		}
		return responsesHTTPResponse(http.StatusOK, "text/event-stream", body), nil
	})
	implementation, err := provider.NewAzureOpenAIResponsesProvider(provider.AzureOpenAIResponsesConfig{Client: client})
	if err != nil {
		t.Fatal(err)
	}
	model, err := newModel(provider.ModelSpec{
		Provider: provider.AzureOpenAIProviderID, API: provider.AzureOpenAIResponsesAPI, ID: "gpt-5", Name: "GPT-5",
		BaseURL: "https://resource.openai.azure.com", Input: []provider.InputKind{provider.InputText},
		Cost: provider.CostRates{Input: 1, Output: 2}, ContextWindow: 100_000, MaxTokens: 4_096,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequestWithOptions(model, "", []llm.ConversationMessage{mustUser(t, "go")}, provider.RequestOptions{Stream: provider.StreamOptions{
		APIKey: "azure-key", AzureAPIVersion: "2026-preview", AzureDeploymentName: "deployment", SessionID: "session",
		CacheRetention: provider.CacheRetentionLong, ServiceTier: "priority",
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	if capturedURL != "https://resource.openai.azure.com/openai/v1/responses?api-version=2026-preview" || capturedHeader.Get("api-key") != "azure-key" || capturedHeader.Get("authorization") != "" {
		t.Fatalf("Azure wire = %q, api-key=%q, authorization=%q", capturedURL, capturedHeader.Get("api-key"), capturedHeader.Get("authorization"))
	}
	if capturedPayload["model"] != "deployment" || capturedPayload["prompt_cache_key"] != "session" || capturedPayload["service_tier"] != nil || capturedPayload["prompt_cache_retention"] != nil {
		t.Fatalf("Azure payload = %#v", capturedPayload)
	}
	cost := terminal.Usage().Cost()
	if math.Abs(cost.Total-20e-6) > 1e-12 {
		t.Fatalf("Azure cost applied OpenAI service-tier multiplier: %#v", cost)
	}
}

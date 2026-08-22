package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestOpenAICompletionsStreamsTextAndEncodesRichRequest(t *testing.T) {
	image, err := llm.NewImageDataBlock("image/png", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	text, err := llm.NewTextBlock("describe this")
	if err != nil {
		t.Fatal(err)
	}
	user, err := llm.NewUserContentMessage([]llm.UserContentBlock{text, image}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	model, err := newModel(provider.ModelSpec{Provider: "compatible", API: provider.OpenAICompletionsAPI, ID: "test-model", BaseURL: "", Reasoning: true, Input: []provider.InputKind{provider.InputText, provider.InputImage}, ThinkingLevelMap: map[provider.ThinkingLevel]*string{provider.ThinkingHigh: ptr("high")}, MaxTokens: 77, Headers: map[string]string{"X-Model": "model"}})
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequestWithOptions(model, "system", []llm.ConversationMessage{user}, provider.RequestOptions{ThinkingLevel: provider.ThinkingHigh, Stream: provider.StreamOptions{APIKey: "request-key", Headers: map[string]string{"X-Model": "request", "X-Request": "yes"}, MaxTokens: uint64Pointer(9)}})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer request-key" {
			t.Errorf("authorization=%q", got)
		}
		if got := r.Header.Get("X-Model"); got != "request" {
			t.Errorf("model header=%q", got)
		}
		if got := r.Header.Get("X-Request"); got != "yes" {
			t.Errorf("request header=%q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, completionsSSE(
			map[string]any{"id": "chat-1", "model": "resolved-model", "choices": []any{map[string]any{"delta": map[string]any{"content": "hello"}, "finish_reason": nil}}},
			map[string]any{"id": "chat-1", "choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}},
			map[string]any{"id": "chat-1", "choices": []any{}, "usage": map[string]any{"prompt_tokens": 9, "completion_tokens": 3, "prompt_tokens_details": map[string]any{"cached_tokens": 2, "cache_write_tokens": 1}, "completion_tokens_details": map[string]any{"reasoning_tokens": 4}}},
		)+"data: [DONE]\n\n")
	}))
	defer server.Close()
	p, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{BaseURL: server.URL + "/v1", APIKey: "adapter-key", Headers: map[string]string{"X-Adapter": "adapter"}, Clock: func() time.Time { return responsesTestTime }})
	if err != nil {
		t.Fatal(err)
	}
	events, terminal := collectStream(t, p.Stream(context.Background(), request))
	if got, want := eventKinds(events), []string{"start", "text_start", "text_delta", "text_end", "done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%v, want %v", got, want)
	}
	message, ok := terminal.(llm.AssistantTextMessage)
	if !ok || terminalText(t, terminal) != "hello" {
		t.Fatalf("terminal=%T", terminal)
	}
	u := message.Usage()
	if u.Input() != 6 || u.Output() != 3 || u.CacheRead() != 2 || u.CacheWrite() != 1 || u.TotalTokens() != 12 {
		t.Fatalf("usage=%#v", u)
	}
	if reasoning, ok := u.Reasoning(); !ok || reasoning != 4 {
		t.Fatalf("reasoning=(%d, %t), want (4, true)", reasoning, ok)
	}
	if provenance := message.AssistantProvenance(); !provenance.Matches("compatible", provider.OpenAICompletionsAPI, "test-model") {
		t.Fatalf("assistant provenance=%#v", provenance)
	}
	if response, ok := message.ResponseMetadata(); !ok || response != (llm.AssistantResponseMetadata{ResponseID: "chat-1", ResponseModel: "resolved-model", RawStopReason: "stop"}) {
		t.Fatalf("assistant response metadata=(%#v, %t)", response, ok)
	}
	if got := payload["max_completion_tokens"]; got != float64(9) {
		t.Fatalf("max_completion_tokens=%#v", got)
	}
	if got := payload["reasoning_effort"]; got != "high" {
		t.Fatalf("reasoning_effort=%#v", got)
	}
	messages := payload["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages=%#v", messages)
	}
	parts := messages[1].(map[string]any)["content"].([]any)
	if len(parts) != 2 || parts[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("rich user parts=%#v", parts)
	}
}

func TestOpenAICompletionsUsesAmbientLongCacheRetention(t *testing.T) {
	t.Setenv("PI_CACHE_RETENTION", "long")
	model, err := newTestModel("compatible", provider.OpenAICompletionsAPI, "cache-model")
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequest(model, "", []llm.ConversationMessage{mustUser(t, "hi")})
	if err != nil {
		t.Fatal(err)
	}
	var captured map[string]any
	implementation, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "key",
		Client: responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
			if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
				return nil, err
			}
			return responsesHTTPResponse(http.StatusOK, "text/event-stream", completionsSSE(
				map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}},
			)+"data: [DONE]\n\n"), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	if terminal.FinishReason() != llm.FinishStop || captured["prompt_cache_retention"] != "24h" {
		t.Fatalf("terminal/payload = %#v / %#v", terminal, captured)
	}
}

func TestOpenAICompletionsAutoDetectsCloudflareGatewayCompatAndHonorsExplicitOverrides(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%t", explicit), func(t *testing.T) {
			compat := provider.ModelCompat{}
			if explicit {
				valueTrue := true
				maxField := "max_completion_tokens"
				compat.OpenAICompletions = &provider.OpenAICompletionsCompat{
					SupportsStore: &valueTrue, SupportsDeveloperRole: &valueTrue, SupportsReasoningEffort: &valueTrue,
					SupportsStrictMode: &valueTrue, MaxTokensField: &maxField,
				}
			}
			model, err := newModel(provider.ModelSpec{
				Provider: "gateway", API: provider.OpenAICompletionsAPI, ID: "reasoning-model",
				BaseURL: "https://gateway.ai.cloudflare.com/v1/account/gateway/openai", Reasoning: true,
				ThinkingLevelMap: map[provider.ThinkingLevel]*string{provider.ThinkingHigh: ptr("high")},
				ContextWindow:    100_000, MaxTokens: 8_000, Compat: compat,
			})
			if err != nil {
				t.Fatal(err)
			}
			definition, err := provider.NewToolDefinitionWithConstrainedSampling(
				"read", "Read", []byte(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
				&provider.ConstrainedSampling{Kind: provider.ConstrainedSamplingJSONSchema, Strict: provider.JSONSchemaStrictPrefer},
			)
			if err != nil {
				t.Fatal(err)
			}
			maxTokens := uint64(77)
			request, err := provider.NewRequestWithOptions(model, "system", []llm.ConversationMessage{mustUser(t, "hi")}, provider.RequestOptions{
				ThinkingLevel: provider.ThinkingHigh, Tools: []provider.ToolDefinition{definition}, Stream: provider.StreamOptions{MaxTokens: &maxTokens},
			})
			if err != nil {
				t.Fatal(err)
			}
			var captured map[string]any
			implementation, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{
				BaseURL: "https://fixture.test/v1", APIKey: "key",
				Client: responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
					if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
						return nil, err
					}
					return responsesHTTPResponse(http.StatusOK, "text/event-stream", completionsSSE(
						map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}},
					)+"data: [DONE]\n\n"), nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
			if terminal.FinishReason() != llm.FinishStop {
				t.Fatalf("terminal = %#v", terminal)
			}
			wireTool := captured["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
			role := captured["messages"].([]any)[0].(map[string]any)["role"]
			if explicit {
				if captured["store"] != false || captured["max_completion_tokens"] != float64(77) || captured["reasoning_effort"] != "high" || wireTool["strict"] != true || role != "developer" {
					t.Fatalf("explicit compat payload = %#v", captured)
				}
			} else {
				if _, present := captured["store"]; present || captured["max_tokens"] != float64(77) || captured["reasoning_effort"] != nil || wireTool["strict"] != nil || role != "system" {
					t.Fatalf("detected Cloudflare compat payload = %#v", captured)
				}
			}
		})
	}
}

func TestOpenAICompletionsAutoDetectsDeepSeekAndOpenRouterThinkingFormats(t *testing.T) {
	for _, testCase := range []struct {
		name, providerID, baseURL, modelID string
		assertPayload                      func(*testing.T, map[string]any)
	}{
		{
			name: "deepseek", providerID: "custom", baseURL: "https://api.deepseek.com/v1", modelID: "deepseek-reasoner",
			assertPayload: func(t *testing.T, payload map[string]any) {
				thinking, _ := payload["thinking"].(map[string]any)
				if thinking["type"] != "enabled" {
					t.Fatalf("DeepSeek thinking = %#v", payload["thinking"])
				}
			},
		},
		{
			name: "openrouter", providerID: "custom", baseURL: "https://openrouter.ai/api/v1", modelID: "anthropic/claude-test",
			assertPayload: func(t *testing.T, payload map[string]any) {
				messages := payload["messages"].([]any)
				if messages[0].(map[string]any)["role"] != "developer" {
					t.Fatalf("OpenRouter system role = %#v", messages[0])
				}
				if reasoning, _ := payload["reasoning"].(map[string]any); reasoning["effort"] != "high" {
					t.Fatalf("OpenRouter reasoning = %#v", payload["reasoning"])
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			model, err := newModel(provider.ModelSpec{
				Provider: testCase.providerID, API: provider.OpenAICompletionsAPI, ID: testCase.modelID, BaseURL: testCase.baseURL,
				Reasoning: true, ThinkingLevelMap: map[provider.ThinkingLevel]*string{provider.ThinkingHigh: ptr("high")}, ContextWindow: 100_000, MaxTokens: 8_000,
			})
			if err != nil {
				t.Fatal(err)
			}
			request, err := provider.NewRequestWithOptions(model, "system", []llm.ConversationMessage{mustUser(t, "hi")}, provider.RequestOptions{ThinkingLevel: provider.ThinkingHigh})
			if err != nil {
				t.Fatal(err)
			}
			var captured map[string]any
			implementation, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{
				BaseURL: "https://fixture.test/v1", APIKey: "key",
				Client: responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
					if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
						return nil, err
					}
					return responsesHTTPResponse(http.StatusOK, "text/event-stream", completionsSSE(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}})+"data: [DONE]\n\n"), nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = collectStream(t, implementation.Stream(context.Background(), request))
			testCase.assertPayload(t, captured)
		})
	}
}

func TestOpenAICompletionsMapsDeepSeekThinkingFormat(t *testing.T) {
	supportsStore := false
	thinkingFormat := "deepseek"
	highEffort := "mapped-high"
	model, err := newModel(provider.ModelSpec{
		Provider: "deepseek", API: provider.OpenAICompletionsAPI, ID: "deepseek-v4-flash",
		Reasoning: true,
		ThinkingLevelMap: map[provider.ThinkingLevel]*string{
			provider.ThinkingHigh: &highEffort,
		},
		Compat: provider.ModelCompat{OpenAICompletions: &provider.OpenAICompletionsCompat{
			SupportsStore:  &supportsStore,
			ThinkingFormat: &thinkingFormat,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	payloads := make(chan map[string]any, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		payloads <- payload
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, completionsSSE(
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "ok"}, "finish_reason": nil}}},
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}},
		)+"data: [DONE]\n\n")
	}))
	defer server.Close()
	implementation, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{
		BaseURL: server.URL + "/v1", APIKey: "key",
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, level := range []provider.ThinkingLevel{provider.ThinkingOff, provider.ThinkingHigh} {
		request, err := provider.NewRequestWithOptions(model, "", []llm.ConversationMessage{mustUser(t, "hello")}, provider.RequestOptions{ThinkingLevel: level})
		if err != nil {
			t.Fatal(err)
		}
		_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
		if terminal.FinishReason() != llm.FinishStop {
			t.Fatalf("level %s terminal = %#v", level, terminal)
		}
		payload := <-payloads
		thinking, ok := payload["thinking"].(map[string]any)
		if !ok {
			t.Fatalf("level %s thinking = %#v", level, payload["thinking"])
		}
		if level == provider.ThinkingOff {
			if thinking["type"] != "disabled" {
				t.Fatalf("off thinking = %#v", thinking)
			}
			if _, sent := payload["reasoning_effort"]; sent {
				t.Fatalf("off reasoning_effort = %#v", payload["reasoning_effort"])
			}
		} else {
			if thinking["type"] != "enabled" || payload["reasoning_effort"] != highEffort {
				t.Fatalf("high thinking/effort = %#v / %#v", thinking, payload["reasoning_effort"])
			}
		}
	}
}

func TestOpenAICompletionsPreservesDeepSeekThinkingOptionSemantics(t *testing.T) {
	supportsStore := false
	supportsEffort := false
	thinkingFormat := "deepseek"
	model, err := newModel(provider.ModelSpec{
		Provider: "deepseek", API: provider.OpenAICompletionsAPI, ID: "deepseek-v4-flash",
		Reasoning: true,
		ThinkingLevelMap: map[provider.ThinkingLevel]*string{
			provider.ThinkingOff: nil,
		},
		Compat: provider.ModelCompat{OpenAICompletions: &provider.OpenAICompletionsCompat{
			SupportsStore: &supportsStore, SupportsReasoningEffort: &supportsEffort, ThinkingFormat: &thinkingFormat,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	payloads := make(chan map[string]any, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		payloads <- payload
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, completionsSSE(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "ok"}, "finish_reason": "stop"}}})+"data: [DONE]\n\n")
	}))
	defer server.Close()
	implementation, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{BaseURL: server.URL, APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	for _, level := range []provider.ThinkingLevel{provider.ThinkingOff, provider.ThinkingHigh} {
		request, err := provider.NewRequestWithOptions(model, "", []llm.ConversationMessage{mustUser(t, "hello")}, provider.RequestOptions{ThinkingLevel: level})
		if err != nil {
			t.Fatal(err)
		}
		_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
		if terminal.FinishReason() != llm.FinishStop {
			t.Fatalf("level %s terminal=%#v", level, terminal)
		}
		payload := <-payloads
		if level == provider.ThinkingOff {
			if _, present := payload["thinking"]; present {
				t.Fatalf("off:null emitted thinking=%#v", payload["thinking"])
			}
		} else {
			thinking, ok := payload["thinking"].(map[string]any)
			if !ok || thinking["type"] != "enabled" {
				t.Fatalf("high thinking=%#v", payload["thinking"])
			}
		}
		if _, present := payload["reasoning_effort"]; present {
			t.Fatalf("unsupported reasoning_effort=%#v", payload["reasoning_effort"])
		}
	}
}

func TestOpenAICompletionsOmitsUnsupportedStreamOptionsAndInfersFinishReason(t *testing.T) {
	supportsUsage, supportsFinish := false, false
	model, err := newModel(provider.ModelSpec{
		Provider: "compatible", API: provider.OpenAICompletionsAPI, ID: "no-finish",
		Compat: provider.ModelCompat{OpenAICompletions: &provider.OpenAICompletionsCompat{
			SupportsUsageInStreaming: &supportsUsage,
			SupportsFinishReason:     &supportsFinish,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequest(model, "", []llm.ConversationMessage{mustUser(t, "hello")})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, completionsSSE(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "done"}, "finish_reason": nil}}})+"data: [DONE]\n\n")
	}))
	defer server.Close()
	implementation, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{BaseURL: server.URL + "/v1", APIKey: "key", Clock: func() time.Time { return responsesTestTime }})
	if err != nil {
		t.Fatal(err)
	}
	_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	if terminal.FinishReason() != llm.FinishStop || terminalText(t, terminal) != "done" {
		t.Fatalf("terminal=%#v", terminal)
	}
	if _, sent := payload["stream_options"]; sent {
		t.Fatalf("unsupported stream_options were sent: %#v", payload["stream_options"])
	}
}

func TestOpenAICompletionsInterleavedToolCalls(t *testing.T) {
	model, err := newTestModel("another-provider", provider.OpenAICompletionsAPI, "chat")
	if err != nil {
		t.Fatal(err)
	}
	definition, err := provider.NewToolDefinition("alpha", "Alpha tool", false, []byte(`{"type":"object","properties":{},"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequestWithOptions(model, "", []llm.ConversationMessage{mustUser(t, "run")}, provider.RequestOptions{Tools: []provider.ToolDefinition{definition}, AllowParallelToolCalls: true})
	if err != nil {
		t.Fatal(err)
	}
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call-b\",\"type\":\"function\",\"function\":{\"name\":\"alpha\",\"arguments\":\"{\\\"b\\\":\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-a\",\"type\":\"function\",\"function\":{\"name\":\"alpha\",\"arguments\":\"{\\\"a\\\":\"}},{\"index\":1,\"function\":{\"arguments\":\"2}\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4}}\n\n" +
		"data: [DONE]\n\n"
	p, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{BaseURL: "https://fixture.test/v1", APIKey: "key", Client: responsesDoerFunc(func(*http.Request) (*http.Response, error) {
		return responsesHTTPResponse(http.StatusOK, "text/event-stream", body), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	events, terminal := collectStream(t, p.Stream(context.Background(), request))
	if got, want := eventKinds(events), []string{"start", "toolcall_start", "toolcall_delta", "toolcall_start", "toolcall_delta", "toolcall_delta", "toolcall_delta", "toolcall_end", "toolcall_end", "done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%v, want %v", got, want)
	}
	toolUse, ok := terminal.(llm.AssistantToolUseMessage)
	if !ok {
		t.Fatalf("terminal=%T", terminal)
	}
	blocks := toolUse.Blocks()
	if len(blocks) != 2 {
		t.Fatalf("blocks=%#v", blocks)
	}
	first := blocks[0].(llm.ToolCallBlock)
	second := blocks[1].(llm.ToolCallBlock)
	// Stream event order is durable assistant-block order, even when a provider
	// assigns the later call a lower Chat Completions index.
	if first.ID() != "call-b" || string(first.ArgumentsJSON()) != `{"b":2}` || second.ID() != "call-a" || string(second.ArgumentsJSON()) != `{"a":1}` {
		t.Fatalf("calls=%#v", blocks)
	}
	if usage := toolUse.Usage(); usage.Input() != 9 || usage.Output() != 4 || usage.TotalTokens() != 13 {
		t.Fatalf("usage=%#v", usage)
	}
}

func TestOpenAICompletionsAcceptsLateToolIdentity(t *testing.T) {
	model, err := newTestModel("compatible", provider.OpenAICompletionsAPI, "chat")
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequest(model, "", []llm.ConversationMessage{mustUser(t, "run")})
	if err != nil {
		t.Fatal(err)
	}
	body := completionsSSE(
		map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "function": map[string]any{"arguments": "{"}},
		}}, "finish_reason": nil}}},
		map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": "call-a", "type": "function", "function": map[string]any{"name": "alpha", "arguments": "}"}},
		}}, "finish_reason": "tool_calls"}}},
	) + "data: [DONE]\n\n"
	implementation, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "key",
		Client: responsesDoerFunc(func(*http.Request) (*http.Response, error) {
			return responsesHTTPResponse(http.StatusOK, "text/event-stream", body), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	events, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	if got, want := eventKinds(events), []string{"start", "toolcall_start", "toolcall_delta", "toolcall_end", "done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	toolUse, ok := terminal.(llm.AssistantToolUseMessage)
	if !ok || len(toolUse.Blocks()) != 1 {
		t.Fatalf("terminal = %#v", terminal)
	}
	call := toolUse.Blocks()[0].(llm.ToolCallBlock)
	if call.ID() != "call-a" || call.Name() != "alpha" || string(call.ArgumentsJSON()) != `{}` {
		t.Fatalf("late-identity call = %#v", call)
	}
}

func TestOpenAICompletionsRoutesIndexlessParallelDeltasByID(t *testing.T) {
	model, err := newTestModel("compatible", provider.OpenAICompletionsAPI, "chat")
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequest(model, "", []llm.ConversationMessage{mustUser(t, "run")})
	if err != nil {
		t.Fatal(err)
	}
	body := completionsSSE(
		map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": "call-a", "type": "function", "function": map[string]any{"name": "alpha", "arguments": "{\"a\":"}},
			map[string]any{"index": 1, "id": "call-b", "type": "function", "function": map[string]any{"name": "alpha", "arguments": "{\"b\":"}},
		}}, "finish_reason": nil}}},
		map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
			map[string]any{"id": "call-b", "function": map[string]any{"arguments": "2}"}},
		}}, "finish_reason": nil}}},
		map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
			map[string]any{"id": "call-a", "function": map[string]any{"arguments": "1}"}},
		}}, "finish_reason": "tool_calls"}}},
	) + "data: [DONE]\n\n"
	implementation, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "key",
		Client: responsesDoerFunc(func(*http.Request) (*http.Response, error) {
			return responsesHTTPResponse(http.StatusOK, "text/event-stream", body), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	toolUse, ok := terminal.(llm.AssistantToolUseMessage)
	if !ok || len(toolUse.Blocks()) != 2 {
		t.Fatalf("terminal = %#v", terminal)
	}
	first := toolUse.Blocks()[0].(llm.ToolCallBlock)
	second := toolUse.Blocks()[1].(llm.ToolCallBlock)
	if first.ID() != "call-a" || string(first.ArgumentsJSON()) != `{"a":1}` || second.ID() != "call-b" || string(second.ArgumentsJSON()) != `{"b":2}` {
		t.Fatalf("parallel calls = %#v", toolUse.Blocks())
	}
}

func TestOpenAICompletionsChoiceUsageSupportsPromptCacheHitFallback(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		usage      map[string]any
		wantInput  uint64
		wantCached uint64
	}{
		{
			name:      "legacy cache hit tokens",
			usage:     map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "prompt_cache_hit_tokens": 3},
			wantInput: 7, wantCached: 3,
		},
		{
			name: "standard cached tokens take precedence",
			usage: map[string]any{
				"prompt_tokens": 10, "completion_tokens": 2, "prompt_cache_hit_tokens": 3,
				"prompt_tokens_details": map[string]any{"cached_tokens": 1},
			},
			wantInput: 9, wantCached: 1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			model, err := newTestModel("compatible", provider.OpenAICompletionsAPI, "chat")
			if err != nil {
				t.Fatal(err)
			}
			request, err := provider.NewRequest(model, "", []llm.ConversationMessage{mustUser(t, "hi")})
			if err != nil {
				t.Fatal(err)
			}
			body := completionsSSE(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"content": "ok"}, "finish_reason": "stop", "usage": testCase.usage,
			}}}) + "data: [DONE]\n\n"
			implementation, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{
				BaseURL: "https://fixture.test/v1", APIKey: "key",
				Client: responsesDoerFunc(func(*http.Request) (*http.Response, error) {
					return responsesHTTPResponse(http.StatusOK, "text/event-stream", body), nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
			usage := terminal.Usage()
			if usage.Input() != testCase.wantInput || usage.CacheRead() != testCase.wantCached || usage.Output() != 2 || usage.TotalTokens() != 12 {
				t.Fatalf("usage = input %d cache %d output %d total %d", usage.Input(), usage.CacheRead(), usage.Output(), usage.TotalTokens())
			}
		})
	}
}

func TestOpenAICompletionsHTTPErrorAndCancellation(t *testing.T) {
	model, err := newTestModel("compatible", provider.OpenAICompletionsAPI, "chat")
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequest(model, "", []llm.ConversationMessage{mustUser(t, "hi")})
	if err != nil {
		t.Fatal(err)
	}
	p, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{BaseURL: "https://fixture.test/v1", APIKey: "key", Client: responsesDoerFunc(func(*http.Request) (*http.Response, error) {
		return responsesHTTPResponse(http.StatusBadRequest, "application/json", `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window"}}`), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	events, terminal := collectStream(t, p.Stream(context.Background(), request))
	if got := eventKinds(events); !reflect.DeepEqual(got, []string{"error"}) {
		t.Fatalf("events=%v", got)
	}
	failure, ok := terminal.(llm.AssistantFailureMessage)
	if !ok {
		t.Fatalf("terminal=%T", terminal)
	}
	if !strings.Contains(failure.ErrorMessage(), "context window") {
		t.Fatalf("failure=%q", failure.ErrorMessage())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stream := p.Stream(ctx, request)
	event, nextErr := stream.Next()
	if nextErr != nil {
		t.Fatalf("cancel Next=%v", nextErr)
	}
	errorEvent, ok := event.(llm.ErrorEvent)
	if !ok || errorEvent.Reason() != llm.FinishAborted {
		t.Fatalf("event=%T/%v", event, errorEvent.Reason())
	}
	if _, err := stream.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after terminal=%v", err)
	}
}

func TestOpenAICompletionsPreservesIncomingReasoningButRejectsSSEError(t *testing.T) {
	model, err := newTestModel("compatible", provider.OpenAICompletionsAPI, "chat")
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequest(model, "", []llm.ConversationMessage{mustUser(t, "hi")})
	if err != nil {
		t.Fatal(err)
	}
	body := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"because\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	p, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{BaseURL: "https://fixture.test/v1", APIKey: "key", Client: responsesDoerFunc(func(*http.Request) (*http.Response, error) {
		return responsesHTTPResponse(http.StatusOK, "text/event-stream", body), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	events, terminal := collectStream(t, p.Stream(context.Background(), request))
	if got, want := eventKinds(events), []string{"start", "thinking_start", "thinking_delta", "text_start", "text_delta", "thinking_end", "text_end", "done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%v, want %v", got, want)
	}
	rich, ok := terminal.(llm.AssistantRichMessage)
	if !ok {
		t.Fatalf("terminal=%T", terminal)
	}
	blocks := rich.Blocks()
	if len(blocks) != 2 || blocks[0].(llm.ThinkingBlock).Thinking() != "because" || blocks[1].(llm.TextBlock).Text() != "answer" {
		t.Fatalf("blocks=%#v", blocks)
	}

	p, err = provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{BaseURL: "https://fixture.test/v1", APIKey: "key", Client: responsesDoerFunc(func(*http.Request) (*http.Response, error) {
		return responsesHTTPResponse(http.StatusOK, "text/event-stream", `data: {"error":{"message":"upstream failed","code":"bad"}}`+"\n\n"), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	events, terminal = collectStream(t, p.Stream(context.Background(), request))
	if got := eventKinds(events); !reflect.DeepEqual(got, []string{"start", "error"}) {
		t.Fatalf("events=%v", got)
	}
	failed, ok := terminal.(llm.AssistantFailureMessage)
	if !ok || failed.ErrorMessage() != "upstream failed" {
		t.Fatalf("terminal=%T/%q", terminal, failed.ErrorMessage())
	}

	thinking, err := llm.NewThinkingBlock("because")
	if err != nil {
		t.Fatal(err)
	}
	prior, err := newAssistantRichMessage([]llm.AssistantBlock{thinking}, llm.FinishStop, llm.Usage{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := provider.NewRequest(model, "", []llm.ConversationMessage{mustUser(t, "hi"), prior, mustUser(t, "continue")})
	if err != nil {
		t.Fatal(err)
	}
	events, terminal = collectStream(t, p.Stream(context.Background(), replay))
	if got := eventKinds(events); !reflect.DeepEqual(got, []string{"start", "error"}) {
		t.Fatalf("replay events=%v", got)
	}
	failed, ok = terminal.(llm.AssistantFailureMessage)
	if !ok || failed.ErrorMessage() != "upstream failed" {
		t.Fatalf("replay terminal=%T/%q", terminal, failed.ErrorMessage())
	}
}

func TestOpenAICompletionsClassifiesMidStreamReadFailureAsTransport(t *testing.T) {
	model, err := newTestModel("compatible", provider.OpenAICompletionsAPI, "chat")
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequest(model, "", []llm.ConversationMessage{mustUser(t, "hi")})
	if err != nil {
		t.Fatal(err)
	}
	body := &responsesFailingBody{
		prefix: bytes.NewReader([]byte(completionsSSE(
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "partial"}, "finish_reason": nil}}},
		))),
		err: io.ErrUnexpectedEOF,
	}
	implementation, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "key",
		Client: responsesDoerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       body,
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	events, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	if got, want := eventKinds(events), []string{"start", "text_start", "text_delta", "error"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%v, want %v", got, want)
	}
	failed, ok := terminal.(llm.AssistantFailureMessage)
	if !ok {
		t.Fatalf("terminal=%T", terminal)
	}
	var classified *provider.ProviderFailure
	if !errors.As(failed.Failure().Cause(), &classified) {
		t.Fatalf("failure cause=%T/%v", failed.Failure().Cause(), failed.Failure().Cause())
	}
	if classified.Kind() != provider.FailureTransport || !provider.IsTransientFailure(classified) {
		t.Fatalf("failure kind/transient=%s/%t", classified.Kind(), provider.IsTransientFailure(classified))
	}
	if body.closes.Load() != 1 {
		t.Fatalf("body closes=%d, want 1", body.closes.Load())
	}
}

func TestOpenAICompletionsClassifiesPrematureEOFAsTransport(t *testing.T) {
	model, err := newTestModel("compatible", provider.OpenAICompletionsAPI, "chat")
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequest(model, "", []llm.ConversationMessage{mustUser(t, "hi")})
	if err != nil {
		t.Fatal(err)
	}
	body := &responsesFailingBody{
		prefix: bytes.NewReader([]byte(completionsSSE(
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "partial"}, "finish_reason": nil}}},
		))),
		err: io.EOF,
	}
	implementation, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "key",
		Client: responsesDoerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       body,
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	failed, ok := terminal.(llm.AssistantFailureMessage)
	if !ok {
		t.Fatalf("terminal=%T", terminal)
	}
	var classified *provider.ProviderFailure
	if !errors.As(failed.Failure().Cause(), &classified) {
		t.Fatalf("failure cause=%T/%v", failed.Failure().Cause(), failed.Failure().Cause())
	}
	if classified.Kind() != provider.FailureTransport || !provider.IsTransientFailure(classified) {
		t.Fatalf("failure kind/transient=%s/%t", classified.Kind(), provider.IsTransientFailure(classified))
	}
	if body.closes.Load() != 1 {
		t.Fatalf("body closes=%d, want 1", body.closes.Load())
	}
}

func TestOpenAICompletionsReplaysReasoningToolDetailsAndToolImages(t *testing.T) {
	requiresBridge := true
	model, err := newModel(provider.ModelSpec{Provider: "compatible", API: provider.OpenAICompletionsAPI, ID: "chat", Reasoning: true, Input: []provider.InputKind{provider.InputText, provider.InputImage}, Compat: provider.ModelCompat{OpenAICompletions: &provider.OpenAICompletionsCompat{RequiresAssistantAfterToolResult: &requiresBridge}}})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := provider.NewToolDefinition("alpha", "Alpha", false, []byte(`{"type":"object","properties":{},"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	thinking, err := llm.NewThinkingBlockWithSignature("reason", "reasoning_content", false)
	if err != nil {
		t.Fatal(err)
	}
	call, err := llm.NewToolCallBlockWithThoughtSignature("call-1", "alpha", []byte(`{}`), `{"type":"reasoning.encrypted","id":"call-1","data":"encrypted"}`)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := llm.NewAssistantToolUseMessageWithMetadata([]llm.AssistantBlock{thinking, call}, llm.Usage{}, time.Time{}, llm.AssistantProvenance{Provider: "compatible", API: provider.OpenAICompletionsAPI, Model: "chat"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	image, err := llm.NewImageDataBlock("image/png", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	output, err := llm.NewTextBlock("tool output")
	if err != nil {
		t.Fatal(err)
	}
	result, err := llm.NewToolResultContentMessage("call-1", "alpha", []llm.ToolResultContentBlock{output, image}, false, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequestWithOptions(model, "", []llm.ConversationMessage{mustUser(t, "go"), prior, result}, provider.RequestOptions{Tools: []provider.ToolDefinition{definition}, ToolChoice: &provider.ToolChoice{Name: "alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	p, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{BaseURL: "https://fixture.test/v1", APIKey: "key", Client: responsesDoerFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			return nil, err
		}
		return responsesHTTPResponse(http.StatusOK, "text/event-stream", `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n"), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = collectStream(t, p.Stream(context.Background(), request))
	if payload["tool_choice"].(map[string]any)["function"].(map[string]any)["name"] != "alpha" {
		t.Fatalf("tool_choice=%#v", payload["tool_choice"])
	}
	messages := payload["messages"].([]any)
	assistant := messages[1].(map[string]any)
	if assistant["reasoning_content"] != "reason" {
		t.Fatalf("assistant=%#v", assistant)
	}
	details := assistant["reasoning_details"].([]any)
	if details[0].(map[string]any)["data"] != "encrypted" {
		t.Fatalf("details=%#v", details)
	}
	if len(messages) != 5 || messages[2].(map[string]any)["role"] != "tool" || messages[3].(map[string]any)["role"] != "assistant" || messages[4].(map[string]any)["role"] != "user" {
		t.Fatalf("messages=%#v", messages)
	}
	parts := messages[4].(map[string]any)["content"].([]any)
	if len(parts) != 2 || parts[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("tool image parts=%#v", parts)
	}
}

func TestOpenAICompletionsNormalizesCrossModelHistoryAndUnsupportedImages(t *testing.T) {
	requiresBridge := true
	model, err := newModel(provider.ModelSpec{
		Provider: provider.OpenAIProviderID, API: provider.OpenAICompletionsAPI, ID: "target",
		Input:  []provider.InputKind{provider.InputText},
		Compat: provider.ModelCompat{OpenAICompletions: &provider.OpenAICompletionsCompat{RequiresAssistantAfterToolResult: &requiresBridge}},
	})
	if err != nil {
		t.Fatal(err)
	}
	visible, err := llm.NewThinkingBlockWithSignature("visible plan", "foreign-thinking", false)
	if err != nil {
		t.Fatal(err)
	}
	redacted, err := llm.NewThinkingBlockWithSignature("must stay hidden", "foreign-redacted", true)
	if err != nil {
		t.Fatal(err)
	}
	sourceID := "call.+/=" + strings.Repeat("x", 50) + "|item/+=" + strings.Repeat("y", 50)
	call, err := llm.NewToolCallBlockWithThoughtSignature(sourceID, "alpha", []byte(`{}`), "foreign-tool-signature")
	if err != nil {
		t.Fatal(err)
	}
	prior, err := llm.NewAssistantToolUseMessageWithMetadata(
		[]llm.AssistantBlock{visible, redacted, call}, llm.Usage{}, time.Time{},
		llm.AssistantProvenance{Provider: provider.OpenAIProviderID, API: provider.OpenAIResponsesAPI, Model: "source"}, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	image, err := llm.NewImageDataBlock("image/png", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	result, err := llm.NewToolResultContentMessage(sourceID, "alpha", []llm.ToolResultContentBlock{mustTextBlock(t, "tool text"), image}, false, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	user, err := llm.NewUserContentMessage([]llm.UserContentBlock{mustTextBlock(t, "next"), image}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequest(model, "", []llm.ConversationMessage{prior, result, user})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	implementation, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{BaseURL: "https://fixture.test/v1", APIKey: "key", Client: responsesDoerFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			return nil, err
		}
		return responsesHTTPResponse(http.StatusOK, "text/event-stream", `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n"), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = collectStream(t, implementation.Stream(context.Background(), request))
	messages := payload["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages=%#v", messages)
	}
	assistant := messages[0].(map[string]any)
	if assistant["content"] != "visible plan" {
		t.Fatalf("cross-model assistant=%#v", assistant)
	}
	encodedAssistant, _ := json.Marshal(assistant)
	if strings.Contains(string(encodedAssistant), "must stay hidden") || assistant["reasoning_details"] != nil {
		t.Fatalf("foreign opaque reasoning leaked: %s", encodedAssistant)
	}
	normalizedID := assistant["tool_calls"].([]any)[0].(map[string]any)["id"].(string)
	if want := "call____xxxxxxxxxxxxxxxxxxxxxxx_15z2bpwo"; normalizedID != want {
		t.Fatalf("normalized tool id=%q, want upstream shortHash id %q", normalizedID, want)
	}
	tool := messages[1].(map[string]any)
	if tool["tool_call_id"] != normalizedID || !strings.Contains(tool["content"].(string), "tool image omitted") {
		t.Fatalf("tool result=%#v", tool)
	}
	if messages[2].(map[string]any)["role"] != "assistant" {
		t.Fatalf("missing tool/user bridge: %#v", messages)
	}
	parts := messages[3].(map[string]any)["content"].([]any)
	if len(parts) != 2 || parts[1].(map[string]any)["type"] != "text" || !strings.Contains(parts[1].(map[string]any)["text"].(string), "image omitted") {
		t.Fatalf("unsupported user image projection=%#v", parts)
	}
}

func TestOpenAICompletionsClampsInconsistentCacheUsage(t *testing.T) {
	model, err := newTestModel("compatible", provider.OpenAICompletionsAPI, "chat")
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequest(model, "", []llm.ConversationMessage{mustUser(t, "hi")})
	if err != nil {
		t.Fatal(err)
	}
	body := completionsSSE(
		map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "ok"}, "finish_reason": "stop"}}},
		map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "prompt_tokens_details": map[string]any{"cached_tokens": 2}}},
	) + "data: [DONE]\n\n"
	implementation, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{BaseURL: "https://fixture.test/v1", APIKey: "key", Client: responsesDoerFunc(func(*http.Request) (*http.Response, error) {
		return responsesHTTPResponse(http.StatusOK, "text/event-stream", body), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	usage := terminal.Usage()
	if usage.Input() != 0 || usage.Output() != 1 || usage.CacheRead() != 2 || usage.TotalTokens() != 3 {
		t.Fatalf("usage=%#v", usage)
	}
}

func completionsSSE(events ...map[string]any) string {
	var b strings.Builder
	for _, event := range events {
		encoded, _ := json.Marshal(event)
		b.WriteString("data: ")
		b.Write(encoded)
		b.WriteString("\n\n")
	}
	return b.String()
}
func ptr(value string) *string { return &value }

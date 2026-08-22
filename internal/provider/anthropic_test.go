package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestAnthropicMessagesRequestAndRichStreamMatchPiContract(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/messages" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("x-api-key"); got != "anthropic-secret" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := request.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q", got)
		}
		if got := request.Header.Get("anthropic-beta"); got != "interleaved-thinking-2025-05-14" {
			t.Errorf("anthropic-beta = %q", got)
		}
		if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writeResponsesSSE(t, writer,
			map[string]any{"type": "message_start", "message": map[string]any{
				"id": "msg-anthropic", "usage": map[string]any{
					"input_tokens": 10, "output_tokens": 0, "cache_read_input_tokens": 2, "cache_creation_input_tokens": 3,
					"cache_creation": map[string]any{"ephemeral_1h_input_tokens": 1},
				},
			}},
			map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}},
			map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "plan"}},
			map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "signature_delta", "signature": "sig"}},
			map[string]any{"type": "content_block_stop", "index": 0},
			map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "text", "text": ""}},
			map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": "answer"}},
			map[string]any{"type": "content_block_stop", "index": 1},
			map[string]any{"type": "content_block_start", "index": 2, "content_block": map[string]any{"type": "tool_use", "id": "toolu-1", "name": "read", "input": map[string]any{}}},
			map[string]any{"type": "content_block_delta", "index": 2, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"path":"README.md"}`}},
			map[string]any{"type": "content_block_stop", "index": 2},
			map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{
				"output_tokens": 5, "output_tokens_details": map[string]any{"thinking_tokens": 2},
			}},
			map[string]any{"type": "message_stop"},
		)
	}))
	defer server.Close()

	strictTools := true
	model, err := newModel(provider.ModelSpec{
		Provider: provider.AnthropicProviderID, API: provider.AnthropicMessagesAPI, ID: "claude-sonnet-4-6", Name: "Claude",
		BaseURL: server.URL, Reasoning: true, Input: []provider.InputKind{provider.InputText, provider.InputImage},
		Cost: provider.CostRates{Input: 3, Output: 15, CacheRead: .3, CacheWrite: 3.75}, ContextWindow: 200_000, MaxTokens: 20_000,
		Compat: provider.ModelCompat{AnthropicMessages: &provider.AnthropicMessagesCompat{SupportsStrictTools: &strictTools}},
	})
	if err != nil {
		t.Fatal(err)
	}
	constrained := &provider.ConstrainedSampling{Kind: provider.ConstrainedSamplingJSONSchema, Strict: provider.JSONSchemaStrictPrefer}
	tool, err := provider.NewToolDefinitionWithConstrainedSampling("read", "Read a file", []byte(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`), constrained)
	if err != nil {
		t.Fatal(err)
	}
	maxTokens := uint64(2000)
	choice := provider.ToolChoice{Name: "read"}
	request, err := provider.NewRequestWithOptions(model, "system", []llm.ConversationMessage{mustUser(t, "inspect")}, provider.RequestOptions{
		Tools: []provider.ToolDefinition{tool}, ToolChoice: &choice, ThinkingLevel: provider.ThinkingHigh,
		Stream: provider.StreamOptions{MaxTokens: &maxTokens, Metadata: map[string]any{"user_id": "user-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := provider.NewAnthropicProvider(provider.AnthropicConfig{
		BaseURL: server.URL, APIKey: "anthropic-secret", Clock: func() time.Time { return responsesTestTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	events, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	if got, want := eventKinds(events), []string{
		"start", "thinking_start", "thinking_delta", "thinking_end", "text_start", "text_delta", "text_end",
		"toolcall_start", "toolcall_delta", "toolcall_end", "done",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	message, ok := terminal.(llm.AssistantToolUseMessage)
	if !ok || message.FinishReason() != llm.FinishToolUse {
		t.Fatalf("terminal = %T/%v", terminal, terminal.FinishReason())
	}
	blocks := message.Blocks()
	if len(blocks) != 3 || blocks[0].(llm.ThinkingBlock).Thinking() != "plan" || blocks[1].(llm.TextBlock).Text() != "answer" {
		t.Fatalf("blocks = %#v", blocks)
	}
	call := blocks[2].(llm.ToolCallBlock)
	if call.ID() != "toolu-1" || call.Name() != "read" || string(call.ArgumentsJSON()) != `{"path":"README.md"}` {
		t.Fatalf("tool call = %s/%s/%s", call.ID(), call.Name(), call.ArgumentsJSON())
	}
	usage := message.Usage()
	if usage.Input() != 10 || usage.Output() != 5 || usage.CacheRead() != 2 || usage.CacheWrite() != 3 {
		t.Fatalf("usage = input=%d output=%d read=%d write=%d", usage.Input(), usage.Output(), usage.CacheRead(), usage.CacheWrite())
	}
	if reasoning, ok := usage.Reasoning(); !ok || reasoning != 2 {
		t.Fatalf("reasoning = %d/%t", reasoning, ok)
	}
	if oneHour, ok := usage.CacheWrite1h(); !ok || oneHour != 1 {
		t.Fatalf("cache write 1h = %d/%t", oneHour, ok)
	}

	if captured["model"] != "claude-sonnet-4-6" || captured["max_tokens"] != float64(18_384) {
		t.Fatalf("request model/max = %#v/%#v", captured["model"], captured["max_tokens"])
	}
	thinking, _ := captured["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(16_384) || thinking["display"] != "summarized" {
		t.Fatalf("thinking = %#v", thinking)
	}
	if _, exists := captured["temperature"]; exists {
		t.Fatalf("temperature must be omitted while thinking: %#v", captured["temperature"])
	}
	system, _ := captured["system"].([]any)
	if len(system) != 1 || system[0].(map[string]any)["text"] != "system" {
		t.Fatalf("system = %#v", system)
	}
	tools, _ := captured["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", tools)
	}
	wireTool := tools[0].(map[string]any)
	if wireTool["strict"] != true || wireTool["eager_input_streaming"] != true {
		t.Fatalf("tool = %#v", wireTool)
	}
	if choice, _ := captured["tool_choice"].(map[string]any); choice["type"] != "tool" || choice["name"] != "read" {
		t.Fatalf("tool_choice = %#v", captured["tool_choice"])
	}
	if metadata, _ := captured["metadata"].(map[string]any); metadata["user_id"] != "user-1" {
		t.Fatalf("metadata = %#v", captured["metadata"])
	}
}

func TestAnthropicPortableThinkingOffUsesDisabledUnlessCatalogExplicitlyOmitsIt(t *testing.T) {
	offEffort := "none"
	for _, testCase := range []struct {
		name         string
		thinkingMap  map[provider.ThinkingLevel]*string
		wantDisabled bool
	}{
		{name: "missing off mapping", thinkingMap: map[provider.ThinkingLevel]*string{}, wantDisabled: true},
		{name: "mapped off", thinkingMap: map[provider.ThinkingLevel]*string{provider.ThinkingOff: &offEffort}, wantDisabled: true},
		{name: "explicit null off", thinkingMap: map[provider.ThinkingLevel]*string{provider.ThinkingOff: nil}, wantDisabled: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var captured map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
					t.Errorf("decode request: %v", err)
				}
				writer.Header().Set("Content-Type", "text/event-stream")
				writeResponsesSSE(t, writer,
					map[string]any{"type": "message_start", "message": map[string]any{"id": "msg-off", "usage": map[string]any{"input_tokens": 1}}},
					map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 0}},
					map[string]any{"type": "message_stop"},
				)
			}))
			defer server.Close()
			model, err := newModel(provider.ModelSpec{
				Provider: provider.AnthropicProviderID, API: provider.AnthropicMessagesAPI, ID: "claude-off", BaseURL: server.URL,
				Reasoning: true, ThinkingLevelMap: testCase.thinkingMap, ContextWindow: 100_000, MaxTokens: 8_000,
			})
			if err != nil {
				t.Fatal(err)
			}
			request, err := provider.NewRequestWithOptions(model, "", []llm.ConversationMessage{mustUser(t, "hi")}, provider.RequestOptions{ThinkingLevel: provider.ThinkingOff})
			if err != nil {
				t.Fatal(err)
			}
			implementation, err := provider.NewAnthropicProvider(provider.AnthropicConfig{BaseURL: server.URL, APIKey: "test-key"})
			if err != nil {
				t.Fatal(err)
			}
			_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
			if terminal.FinishReason() != llm.FinishStop {
				t.Fatalf("terminal = %#v", terminal)
			}
			thinking, present := captured["thinking"].(map[string]any)
			if testCase.wantDisabled {
				if !present || thinking["type"] != "disabled" {
					t.Fatalf("thinking = %#v, want disabled", captured["thinking"])
				}
			} else if _, exists := captured["thinking"]; exists {
				t.Fatalf("explicit null off mapping emitted thinking = %#v", captured["thinking"])
			}
		})
	}
}

func TestAnthropicUsesAmbientLongCacheRetention(t *testing.T) {
	t.Setenv("PI_CACHE_RETENTION", "long")
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writeResponsesSSE(t, writer,
			map[string]any{"type": "message_start", "message": map[string]any{"id": "msg-cache", "usage": map[string]any{"input_tokens": 1}}},
			map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 0}},
			map[string]any{"type": "message_stop"},
		)
	}))
	defer server.Close()
	model, err := newModel(provider.ModelSpec{
		Provider: provider.AnthropicProviderID, API: provider.AnthropicMessagesAPI, ID: "claude-cache", BaseURL: server.URL,
		ContextWindow: 100_000, MaxTokens: 8_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequest(model, "", []llm.ConversationMessage{mustUser(t, "hi")})
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := provider.NewAnthropicProvider(provider.AnthropicConfig{BaseURL: server.URL, APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	if terminal.FinishReason() != llm.FinishStop {
		t.Fatalf("terminal = %#v", terminal)
	}
	messages := captured["messages"].([]any)
	content := messages[0].(map[string]any)["content"].([]any)
	cache := content[len(content)-1].(map[string]any)["cache_control"].(map[string]any)
	if cache["type"] != "ephemeral" || cache["ttl"] != "1h" {
		t.Fatalf("cache control = %#v", cache)
	}
}

func TestAnthropicOAuthIdentityAndToolNameRoundTrip(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer sk-ant-oat-test" || request.Header.Get("x-api-key") != "" {
			t.Errorf("auth = Authorization %q x-api-key %q", got, request.Header.Get("x-api-key"))
		}
		for name, want := range map[string]string{"user-agent": "claude-cli/2.1.75", "x-app": "cli"} {
			if got := request.Header.Get(name); got != want {
				t.Errorf("%s = %q", name, got)
			}
		}
		if beta := request.Header.Get("anthropic-beta"); !strings.Contains(beta, "claude-code-20250219") || !strings.Contains(beta, "oauth-2025-04-20") || !strings.Contains(beta, "interleaved-thinking-2025-05-14") {
			t.Errorf("anthropic-beta = %q", beta)
		}
		if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writeResponsesSSE(t, writer,
			map[string]any{"type": "message_start", "message": map[string]any{"id": "msg-oauth", "usage": map[string]any{"input_tokens": 1}}},
			map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "toolu-oauth", "name": "Read", "input": map[string]any{}}},
			map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"path":"x"}`}},
			map[string]any{"type": "content_block_stop", "index": 0},
			map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 1}},
			map[string]any{"type": "message_stop"},
		)
	}))
	defer server.Close()

	model, err := newModel(provider.ModelSpec{
		Provider: provider.AnthropicProviderID, API: provider.AnthropicMessagesAPI, ID: "claude-sonnet-4-6", Name: "Claude",
		BaseURL: server.URL, Input: []provider.InputKind{provider.InputText}, ContextWindow: 100_000, MaxTokens: 8_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	tool, err := provider.NewToolDefinition("read", "Read a file", false, []byte(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`))
	if err != nil {
		t.Fatal(err)
	}
	choice := provider.ToolChoice{Name: "read"}
	request, err := provider.NewRequestWithOptions(model, "custom system", []llm.ConversationMessage{mustUser(t, "read")}, provider.RequestOptions{
		Tools: []provider.ToolDefinition{tool}, ToolChoice: &choice,
	})
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := provider.NewAnthropicProvider(provider.AnthropicConfig{BaseURL: server.URL, APIKey: "sk-ant-oat-test"})
	if err != nil {
		t.Fatal(err)
	}
	_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	message := terminal.(llm.AssistantToolUseMessage)
	call := message.Blocks()[0].(llm.ToolCallBlock)
	if call.Name() != "read" {
		t.Fatalf("returned tool name = %q", call.Name())
	}
	system, _ := captured["system"].([]any)
	if len(system) != 2 || system[0].(map[string]any)["text"] != "You are Claude Code, Anthropic's official CLI for Claude." || system[1].(map[string]any)["text"] != "custom system" {
		t.Fatalf("OAuth system = %#v", system)
	}
	tools, _ := captured["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "Read" {
		t.Fatalf("OAuth tools = %#v", tools)
	}
	choiceWire, _ := captured["tool_choice"].(map[string]any)
	if choiceWire["name"] != "Read" {
		t.Fatalf("OAuth tool choice = %#v", choiceWire)
	}
}

func TestAnthropicRepairsMalformedToolJSONAndIgnoresUnknownWireContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeResponsesSSE(t, writer,
			map[string]any{"type": "message_start", "message": map[string]any{"id": "msg-repair", "usage": map[string]any{"input_tokens": 1}}},
			map[string]any{"type": "future_metadata", "payload": map[string]any{"may_not": "become content"}},
			map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "future_block", "text": "must not surface"}},
			map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "must not surface"}},
			map[string]any{"type": "content_block_stop", "index": 0},
			map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "id": "toolu-repair", "name": "edit", "input": map[string]any{}}},
			map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "future_delta", "partial_json": "must not alter arguments"}},
		)
		malformed := `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"A\H\",\"text\":\"col1` + "\t" + `col2\"}"}}`
		if _, err := fmt.Fprintf(writer, "data: %s\n\n", malformed); err != nil {
			t.Errorf("write malformed SSE: %v", err)
			return
		}
		writeResponsesSSE(t, writer,
			map[string]any{"type": "content_block_stop", "index": 1},
			map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 1}},
			map[string]any{"type": "message_stop"},
		)
	}))
	defer server.Close()

	model, err := newModel(provider.ModelSpec{
		Provider: provider.AnthropicProviderID, API: provider.AnthropicMessagesAPI, ID: "claude-repair", Name: "Claude Repair",
		BaseURL: server.URL, Input: []provider.InputKind{provider.InputText}, ContextWindow: 100_000, MaxTokens: 8_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	tool, err := provider.NewToolDefinition("edit", "Edit a file", false, []byte(`{"type":"object","properties":{"path":{"type":"string"},"text":{"type":"string"}},"required":["path","text"]}`))
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequestWithTools(model, "", []llm.ConversationMessage{mustUser(t, "edit")}, []provider.ToolDefinition{tool})
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := provider.NewAnthropicProvider(provider.AnthropicConfig{BaseURL: server.URL, APIKey: "anthropic-test-token"})
	if err != nil {
		t.Fatal(err)
	}
	events, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	if got, want := eventKinds(events), []string{"start", "toolcall_start", "toolcall_delta", "toolcall_end", "done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	message, ok := terminal.(llm.AssistantToolUseMessage)
	if !ok || len(message.Blocks()) != 1 {
		t.Fatalf("terminal = %T %#v", terminal, terminal)
	}
	call := message.Blocks()[0].(llm.ToolCallBlock)
	var arguments map[string]string
	if err := json.Unmarshal(call.ArgumentsJSON(), &arguments); err != nil {
		t.Fatalf("tool arguments = %q: %v", call.ArgumentsJSON(), err)
	}
	if arguments["path"] != `A\H` || arguments["text"] != "col1\tcol2" {
		t.Fatalf("repaired arguments = %#v", arguments)
	}
}

func TestAnthropicOptionalUsageAndReplayMetadataCannotVetoCompletedToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeResponsesSSE(t, writer,
			// Response IDs and token accounting are optional observations. This
			// gateway omits the former and sends malformed/inconsistent variants
			// of the latter while still returning a complete executable call.
			map[string]any{"type": "message_start", "message": map[string]any{"usage": map[string]any{
				"input_tokens": 1, "cache_creation_input_tokens": 1,
				"cache_creation": map[string]any{"ephemeral_1h_input_tokens": 2},
			}}},
			// Thinking/redaction signatures are replay metadata, not a prerequisite
			// for executing the following tool call.
			map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": "plan", "signature": map[string]any{"unexpected": true}}},
			map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "signature_delta", "signature": []any{"unexpected"}}},
			map[string]any{"type": "content_block_stop", "index": 0},
			map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "redacted_thinking", "data": map[string]any{"unexpected": true}}},
			map[string]any{"type": "content_block_stop", "index": 1},
			// A future block may not have lifecycle events this adapter knows. It
			// must not keep the known-content tracker open at message_stop.
			map[string]any{"type": "content_block_start", "index": 2, "content_block": map[string]any{"type": "future_metadata"}},
			map[string]any{"type": "content_block_start", "index": 3, "content_block": map[string]any{"type": "tool_use", "id": "toolu-optional", "name": "read", "input": map[string]any{}}},
			map[string]any{"type": "content_block_delta", "index": 3, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"path":"README.md"}`}},
			map[string]any{"type": "content_block_stop", "index": 3},
			map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": "also-not-a-number"}},
			map[string]any{"type": "message_stop"},
		)
	}))
	defer server.Close()

	model, err := newModel(provider.ModelSpec{
		Provider: provider.AnthropicProviderID, API: provider.AnthropicMessagesAPI, ID: "claude-optional", Name: "Claude Optional",
		BaseURL: server.URL, Input: []provider.InputKind{provider.InputText},
		// NewModel intentionally accepts catalog cost values from dynamic sources;
		// unusable billing must not change this completed Agent turn into an error.
		Cost: provider.CostRates{CacheWrite: -1}, ContextWindow: 100_000, MaxTokens: 8_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	tool, err := provider.NewToolDefinition("read", "Read a file", false, []byte(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`))
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequestWithTools(model, "", []llm.ConversationMessage{mustUser(t, "read")}, []provider.ToolDefinition{tool})
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := provider.NewAnthropicProvider(provider.AnthropicConfig{BaseURL: server.URL, APIKey: "anthropic-test-token"})
	if err != nil {
		t.Fatal(err)
	}
	_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	message, ok := terminal.(llm.AssistantToolUseMessage)
	if !ok || message.FinishReason() != llm.FinishToolUse || len(message.Blocks()) != 2 {
		t.Fatalf("terminal = %T %#v", terminal, terminal)
	}
	thinking, ok := message.Blocks()[0].(llm.ThinkingBlock)
	if !ok || thinking.Thinking() != "plan" {
		t.Fatalf("thinking = %#v", message.Blocks()[0])
	}
	if _, hasSignature := thinking.ThinkingSignature(); hasSignature {
		t.Fatalf("invalid thinking signature was retained")
	}
	call, ok := message.Blocks()[1].(llm.ToolCallBlock)
	if !ok || call.ID() != "toolu-optional" || string(call.ArgumentsJSON()) != `{"path":"README.md"}` {
		t.Fatalf("tool call = %#v", message.Blocks()[1])
	}
	metadata, ok := message.ResponseMetadata()
	if !ok || metadata.ResponseID != "" || metadata.RawStopReason != "tool_use" {
		t.Fatalf("response metadata = %#v, present=%t", metadata, ok)
	}
	usage := message.Usage()
	if usage.CacheWrite() != 1 {
		t.Fatalf("cache write = %d", usage.CacheWrite())
	}
	if _, hasLongCache := usage.CacheWrite1h(); hasLongCache || usage.Cost().Total != 0 {
		t.Fatalf("usage accounting = %#v", usage)
	}
}

func TestAnthropicMalformedOuterUsageCannotVetoCompletedToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeResponsesSSE(t, writer,
			map[string]any{"type": "message_start", "message": map[string]any{"id": "msg-bad-usage", "usage": "not-an-object"}},
			map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "toolu-bad-usage", "name": "read", "input": map[string]any{}}},
			map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"path":"README.md"}`}},
			map[string]any{"type": "content_block_stop", "index": 0},
			map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": []any{"also-not-an-object"}},
			map[string]any{"type": "message_stop"},
		)
	}))
	defer server.Close()

	model, err := newModel(provider.ModelSpec{
		Provider: provider.AnthropicProviderID, API: provider.AnthropicMessagesAPI, ID: "claude-bad-usage", Name: "Claude Bad Usage",
		BaseURL: server.URL, Input: []provider.InputKind{provider.InputText}, ContextWindow: 100_000, MaxTokens: 8_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	tool, err := provider.NewToolDefinition("read", "Read a file", false, []byte(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`))
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequestWithTools(model, "", []llm.ConversationMessage{mustUser(t, "read")}, []provider.ToolDefinition{tool})
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := provider.NewAnthropicProvider(provider.AnthropicConfig{BaseURL: server.URL, APIKey: "anthropic-test-token"})
	if err != nil {
		t.Fatal(err)
	}
	_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	message, ok := terminal.(llm.AssistantToolUseMessage)
	if !ok || message.FinishReason() != llm.FinishToolUse || len(message.Blocks()) != 1 {
		t.Fatalf("terminal = %T %#v", terminal, terminal)
	}
	call, ok := message.Blocks()[0].(llm.ToolCallBlock)
	if !ok || call.ID() != "toolu-bad-usage" || string(call.ArgumentsJSON()) != `{"path":"README.md"}` {
		t.Fatalf("tool call = %#v", message.Blocks()[0])
	}
}

func TestAnthropicFailureStopReasonsPreserveResponseMetadata(t *testing.T) {
	tests := []struct {
		name, stopReason, message, vendorCode string
		stopDetails                           any
	}{
		{name: "refusal", stopReason: "refusal", message: "request declined by policy", vendorCode: "refusal", stopDetails: map[string]any{"type": "refusal", "explanation": "request declined by policy"}},
		{name: "sensitive", stopReason: "sensitive", message: "Provider stopped with: sensitive", vendorCode: "sensitive"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				delta := map[string]any{"stop_reason": test.stopReason}
				if test.stopDetails != nil {
					delta["stop_details"] = test.stopDetails
				}
				writeResponsesSSE(t, writer,
					map[string]any{"type": "message_start", "message": map[string]any{"id": "msg-" + test.name, "usage": map[string]any{"input_tokens": 2}}},
					map[string]any{"type": "message_delta", "delta": delta, "usage": map[string]any{"output_tokens": 1}},
					map[string]any{"type": "message_stop"},
				)
			}))
			defer server.Close()

			model, err := newModel(provider.ModelSpec{
				Provider: provider.AnthropicProviderID, API: provider.AnthropicMessagesAPI, ID: "claude-stop", Name: "Claude Stop",
				BaseURL: server.URL, Input: []provider.InputKind{provider.InputText}, ContextWindow: 100_000, MaxTokens: 8_000,
			})
			if err != nil {
				t.Fatal(err)
			}
			request, err := provider.NewRequest(model, "", []llm.ConversationMessage{mustUser(t, "answer")})
			if err != nil {
				t.Fatal(err)
			}
			implementation, err := provider.NewAnthropicProvider(provider.AnthropicConfig{BaseURL: server.URL, APIKey: "anthropic-test-token"})
			if err != nil {
				t.Fatal(err)
			}
			_, terminal := collectStream(t, implementation.Stream(context.Background(), request))
			failure, ok := terminal.(llm.AssistantFailureMessage)
			if !ok || failure.ErrorMessage() != test.message {
				t.Fatalf("terminal = %T/%q", terminal, failure.ErrorMessage())
			}
			metadata, ok := failure.ResponseMetadata()
			if !ok || metadata.ResponseID != "msg-"+test.name || metadata.RawStopReason != test.stopReason {
				t.Fatalf("response metadata = %#v, present=%t", metadata, ok)
			}
			var providerFailure *provider.ProviderFailure
			if !errors.As(failure.Failure().Cause(), &providerFailure) {
				t.Fatalf("failure cause = %T", failure.Failure().Cause())
			}
			if code, ok := providerFailure.VendorCode(); !ok || code != test.vendorCode {
				t.Fatalf("vendor code = %q, present=%t", code, ok)
			}
		})
	}
}

package provider_test

import (
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
	model, err := provider.NewModel(provider.ModelSpec{Provider: "compatible", API: provider.OpenAICompletionsAPI, ID: "test-model", BaseURL: "", Reasoning: true, Input: []provider.InputKind{provider.InputText, provider.InputImage}, ThinkingLevelMap: map[provider.ThinkingLevel]*string{provider.ThinkingHigh: ptr("high")}, MaxTokens: 77, Headers: map[string]string{"X-Model": "model"}})
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequestWithOptions(model, "system", []llm.ConversationMessage{user}, provider.RequestOptions{ThinkingLevel: provider.ThinkingHigh, Stream: provider.StreamOptions{APIKey: "request-key", Headers: map[string]string{"X-Model": "request", "X-Request": "yes"}, MaxTokens: 9}})
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
			map[string]any{"id": "chat-1", "choices": []any{}, "usage": map[string]any{"prompt_tokens": 9, "completion_tokens": 3, "prompt_tokens_details": map[string]any{"cached_tokens": 2, "cache_write_tokens": 1}, "completion_tokens_details": map[string]any{"reasoning_tokens": 1}}},
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
	if u := message.Usage(); u.Input() != 6 || u.Output() != 3 || u.CacheRead() != 2 || u.CacheWrite() != 1 || u.TotalTokens() != 12 {
		t.Fatalf("usage=%#v", u)
	}
	if provenance, ok := message.AssistantProvenance(); !ok || !provenance.Matches("compatible", provider.OpenAICompletionsAPI, "test-model") {
		t.Fatalf("assistant provenance=(%#v, %t)", provenance, ok)
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

func TestOpenAICompletionsOmitsUnsupportedStreamOptionsAndInfersFinishReason(t *testing.T) {
	supportsUsage, supportsFinish := false, false
	model, err := provider.NewModel(provider.ModelSpec{
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
	model, err := provider.NewModelRef("another-provider", provider.OpenAICompletionsAPI, "chat")
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

func TestOpenAICompletionsHTTPErrorAndCancellation(t *testing.T) {
	model, err := provider.NewModelRef("compatible", provider.OpenAICompletionsAPI, "chat")
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
	model, err := provider.NewModelRef("compatible", provider.OpenAICompletionsAPI, "chat")
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
	if got, want := eventKinds(events), []string{"start", "thinking_start", "thinking_delta", "thinking_end", "text_start", "text_delta", "text_end", "done"}; !reflect.DeepEqual(got, want) {
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
	prior, err := llm.NewAssistantRichMessage([]llm.AssistantBlock{thinking}, llm.FinishStop, llm.Usage{}, time.Time{})
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

func TestOpenAICompletionsReplaysReasoningToolDetailsAndToolImages(t *testing.T) {
	requiresBridge := true
	model, err := provider.NewModel(provider.ModelSpec{Provider: "compatible", API: provider.OpenAICompletionsAPI, ID: "chat", Reasoning: true, Input: []provider.InputKind{provider.InputText, provider.InputImage}, Compat: provider.ModelCompat{OpenAICompletions: &provider.OpenAICompletionsCompat{RequiresAssistantAfterToolResult: &requiresBridge}}})
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
	prior, err := llm.NewAssistantToolUseMessageWithMetadata([]llm.AssistantBlock{thinking, call}, llm.Usage{}, time.Time{}, &llm.AssistantProvenance{Provider: "compatible", API: provider.OpenAICompletionsAPI, Model: "chat"}, nil)
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
	model, err := provider.NewModel(provider.ModelSpec{
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
		&llm.AssistantProvenance{Provider: provider.OpenAIProviderID, API: provider.OpenAIResponsesAPI, Model: "source"}, nil,
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
	if normalizedID == sourceID || len(normalizedID) > 40 || strings.ContainsAny(normalizedID, "|+/=.") {
		t.Fatalf("normalized tool id=%q", normalizedID)
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
	model, err := provider.NewModelRef("compatible", provider.OpenAICompletionsAPI, "chat")
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

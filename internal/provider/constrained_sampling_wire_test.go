package provider_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestOpenAIResponsesGrammarToolWireReplayAndStream(t *testing.T) {
	model, tool, messages := grammarToolFixture(t, provider.OpenAIResponsesAPI, provider.OpenAIProviderID, "call-old|ctc-old")
	request, err := provider.NewRequestWithOptions(model, "", messages, provider.RequestOptions{Tools: []provider.ToolDefinition{tool}})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	body := responsesSSE(
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "custom_tool_call", "id": "ctc-new", "call_id": "call-new", "name": "sample_tool", "input": ""}},
		map[string]any{"type": "response.custom_tool_call_input.delta", "output_index": 0, "item_id": "ctc-new", "delta": `a"`},
		map[string]any{"type": "response.custom_tool_call_input.done", "output_index": 0, "item_id": "ctc-new", "input": "a\"\nb"},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "custom_tool_call", "id": "ctc-new", "call_id": "call-new", "name": "sample_tool", "input": "a\"\nb"}},
		map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}},
	)
	implementation, err := provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "test-token",
		Client: responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
			err := json.NewDecoder(request.Body).Decode(&payload)
			return responsesHTTPResponse(http.StatusOK, "text/event-stream", body), err
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	events, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	if got, want := eventKinds(events), []string{"start", "toolcall_start", "toolcall_delta", "toolcall_delta", "toolcall_end", "done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	assertGrammarTerminal(t, terminal, "call-new|ctc-new", "a\"\nb")

	tools := payload["tools"].([]any)
	wireTool := tools[0].(map[string]any)
	format := wireTool["format"].(map[string]any)
	if wireTool["type"] != "custom" || wireTool["name"] != "sample_tool" || format["type"] != "grammar" || format["syntax"] != "lark" || format["definition"] != "start: /[a-z]+/" {
		t.Fatalf("custom tool = %#v", wireTool)
	}
	input := payload["input"].([]any)
	call := wireItemOfType(t, input, "custom_tool_call")
	if call["id"] != "ctc-old" || call["call_id"] != "call-old" || call["input"] != "abc" {
		t.Fatalf("custom replay call = %#v", call)
	}
	output := wireItemOfType(t, input, "custom_tool_call_output")
	if output["call_id"] != "call-old" || output["output"] != "done" {
		t.Fatalf("custom replay output = %#v", output)
	}
}

func TestOpenAICompletionsGrammarToolWireReplayAndStream(t *testing.T) {
	model, tool, messages := grammarToolFixture(t, provider.OpenAICompletionsAPI, "compatible", "call-old")
	request, err := provider.NewRequestWithOptions(model, "", messages, provider.RequestOptions{Tools: []provider.ToolDefinition{tool}})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	body := completionsSSE(
		map[string]any{"id": "chat-custom", "choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call-new", "type": "custom", "custom": map[string]any{"name": "sample_tool", "input": `a"`}}}}, "finish_reason": nil}}},
		map[string]any{"id": "chat-custom", "choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "custom": map[string]any{"input": "\nb"}}}}, "finish_reason": nil}}},
		map[string]any{"id": "chat-custom", "choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}}},
	) + "data: [DONE]\n\n"
	implementation, err := provider.NewOpenAICompletionsProvider(provider.OpenAICompletionsConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "test-token",
		Client: responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
			err := json.NewDecoder(request.Body).Decode(&payload)
			return responsesHTTPResponse(http.StatusOK, "text/event-stream", body), err
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	events, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	if got, want := eventKinds(events), []string{"start", "toolcall_start", "toolcall_delta", "toolcall_delta", "toolcall_delta", "toolcall_end", "done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	assertGrammarTerminal(t, terminal, "call-new", "a\"\nb")

	tools := payload["tools"].([]any)
	wireTool := tools[0].(map[string]any)
	custom := wireTool["custom"].(map[string]any)
	format := custom["format"].(map[string]any)
	grammar := format["grammar"].(map[string]any)
	if wireTool["type"] != "custom" || custom["name"] != "sample_tool" || format["type"] != "grammar" || grammar["syntax"] != "lark" || grammar["definition"] != "start: /[a-z]+/" {
		t.Fatalf("custom tool = %#v", wireTool)
	}
	wireMessages := payload["messages"].([]any)
	assistant := wireMessages[0].(map[string]any)
	replayed := assistant["tool_calls"].([]any)[0].(map[string]any)
	replayedCustom := replayed["custom"].(map[string]any)
	if replayed["type"] != "custom" || replayed["id"] != "call-old" || replayedCustom["name"] != "sample_tool" || replayedCustom["input"] != "abc" {
		t.Fatalf("custom replay = %#v", replayed)
	}
}

func grammarToolFixture(t *testing.T, api, providerID, callID string) (provider.Model, provider.ToolDefinition, []llm.ConversationMessage) {
	t.Helper()
	grammarSupport := true
	compat := provider.ModelCompat{}
	if api == provider.OpenAIResponsesAPI {
		compat.OpenAIResponses = &provider.OpenAIResponsesCompat{SupportsOpenAIGrammarTools: &grammarSupport}
	} else {
		compat.OpenAICompletions = &provider.OpenAICompletionsCompat{SupportsOpenAIGrammarTools: &grammarSupport}
	}
	model, err := newModel(provider.ModelSpec{Provider: providerID, API: api, ID: "grammar-model", Compat: compat})
	if err != nil {
		t.Fatal(err)
	}
	tool, err := provider.NewToolDefinitionWithConstrainedSampling(
		"sample_tool", "Sample tool",
		[]byte(`{"type":"object","properties":{"payload":{"type":"string"}},"required":["payload"],"additionalProperties":false}`),
		&provider.ConstrainedSampling{Kind: provider.ConstrainedSamplingGrammar, Variants: provider.GrammarVariants{OpenAILark: "start: /[a-z]+/"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	call, err := llm.NewToolCallBlock(callID, "sample_tool", []byte(`{"payload":"abc"}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{call}, llm.Usage{}, responsesTestTime, llm.AssistantProvenance{Provider: providerID, API: api, Model: model.ID()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := llm.NewToolResultMessage(callID, "sample_tool", []llm.TextBlock{mustTextBlock(t, "done")}, false, responsesTestTime)
	if err != nil {
		t.Fatal(err)
	}
	return model, tool, []llm.ConversationMessage{assistant, result, mustUser(t, "continue")}
}

func assertGrammarTerminal(t *testing.T, terminal llm.AssistantTerminal, id, payload string) {
	t.Helper()
	message, ok := terminal.(llm.AssistantToolUseMessage)
	if !ok || len(message.Blocks()) != 1 {
		t.Fatalf("terminal = %T %#v", terminal, terminal)
	}
	call := message.Blocks()[0].(llm.ToolCallBlock)
	if call.ID() != id || call.Name() != "sample_tool" {
		t.Fatalf("tool identity = %q/%q", call.ID(), call.Name())
	}
	var arguments map[string]string
	if err := json.Unmarshal(call.ArgumentsJSON(), &arguments); err != nil || arguments["payload"] != payload {
		t.Fatalf("tool arguments = %q / %#v / %v", call.ArgumentsJSON(), arguments, err)
	}
}

func wireItemOfType(t *testing.T, items []any, kind string) map[string]any {
	t.Helper()
	for _, value := range items {
		if item, ok := value.(map[string]any); ok && item["type"] == kind {
			return item
		}
	}
	t.Fatalf("wire item %q not found in %#v", kind, items)
	return nil
}

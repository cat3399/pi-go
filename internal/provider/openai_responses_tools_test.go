package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestOpenAIResponsesToolDefinitionsAndFunctionCallReplay(t *testing.T) {
	model, err := provider.NewModelRef("openai", provider.OpenAIResponsesAPI, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	definition, err := provider.NewToolDefinition("bash", "Run one shell command.", true, []byte(`{"type":"object","additionalProperties":false,"properties":{"command":{"type":"string"}},"required":["command"]}`))
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequestWithTools(model, "", []llm.ConversationMessage{mustUser(t, "run it")}, []provider.ToolDefinition{definition})
	if err != nil {
		t.Fatal(err)
	}

	firstBody := responsesSSE(
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "bash", "arguments": ""}},
		map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "fc_1", "delta": `{"command":"ec`},
		map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "fc_1", "delta": `ho hi"}`},
		map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": "fc_1", "arguments": `{"command":"echo hi"}`},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "bash", "arguments": `{"command":"echo hi"}`}},
		map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "usage": map[string]any{"input_tokens": 2, "output_tokens": 3, "total_tokens": 5}}},
	)
	var firstPayload map[string]any
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "secret",
		Client: responsesDoerFunc(func(incoming *http.Request) (*http.Response, error) {
			if err := json.NewDecoder(incoming.Body).Decode(&firstPayload); err != nil {
				return nil, err
			}
			return responsesHTTPResponse(http.StatusOK, "text/event-stream", firstBody), nil
		}),
	})
	events, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	if got, want := eventKinds(events), []string{"start", "toolcall_start", "toolcall_delta", "toolcall_delta", "toolcall_end", "done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	toolUse, ok := terminal.(llm.AssistantToolUseMessage)
	if !ok || toolUse.FinishReason() != llm.FinishToolUse {
		t.Fatalf("terminal = %T/%v", terminal, terminal.FinishReason())
	}
	if usage := toolUse.Usage(); usage.Input() != 2 || usage.Output() != 3 || usage.TotalTokens() != 5 {
		t.Fatalf("tool use usage = %#v", usage)
	}
	blocks := toolUse.Blocks()
	if len(blocks) != 1 {
		t.Fatalf("blocks = %#v", blocks)
	}
	call, ok := blocks[0].(llm.ToolCallBlock)
	if !ok || call.ID() != "call_1|fc_1" || call.Name() != "bash" || string(call.ArgumentsJSON()) != `{"command":"echo hi"}` {
		t.Fatalf("call = %#v", blocks[0])
	}
	tools, ok := firstPayload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v", firstPayload["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok || tool["type"] != "function" || tool["name"] != "bash" || tool["strict"] != true {
		t.Fatalf("tool payload = %#v", tools[0])
	}

	text, err := llm.NewTextBlock("I will run it.")
	if err != nil {
		t.Fatal(err)
	}
	mixed, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{text, call}, llm.Usage{}, responsesTestTime)
	if err != nil {
		t.Fatal(err)
	}
	result, err := llm.NewToolResultMessage(call.ID(), "bash", []llm.TextBlock{mustTextBlock(t, "")}, true, responsesTestTime)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := provider.NewRequestWithTools(model, "", []llm.ConversationMessage{mustUser(t, "run it"), mixed, result}, []provider.ToolDefinition{definition})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := encodeReplayForTest(replay)
	if err != nil {
		t.Fatal(err)
	}
	input := payload["input"].([]any)
	if len(input) != 4 {
		t.Fatalf("replay input = %#v", input)
	}
	function, ok := input[2].(map[string]any)
	if !ok || function["type"] != "function_call" || function["call_id"] != "call_1" || function["id"] != "fc_1" || function["arguments"] != `{"command":"echo hi"}` {
		t.Fatalf("function replay = %#v", input[2])
	}
	output, ok := input[3].(map[string]any)
	if !ok || output["type"] != "function_call_output" || output["call_id"] != "call_1" || output["output"] != "(no tool output)" {
		t.Fatalf("tool output replay = %#v", input[3])
	}
}

func TestOpenAIResponsesStreamsMixedTextAndMultipleFunctionCallsInSourceOrder(t *testing.T) {
	frames := []any{
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg", "role": "assistant", "content": []any{}}},
		map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": "msg", "delta": "I will run both."},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "I will run both."}}}},
	}
	frames = append(frames, functionCallSSEEvents(1, "fc_a", "call_a", `{"command":"printf a"}`)...)
	frames = append(frames, functionCallSSEEvents(2, "fc_b", "call_b", `{"command":"printf b"}`)...)
	frames = append(frames,
		map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}},
	)
	body := responsesSSE(frames...)
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{BaseURL: "https://fixture.test/v1", APIKey: "secret", Client: staticResponsesDoer(responsesHTTPResponse(http.StatusOK, "text/event-stream", body))})
	events, terminal := collectStream(t, implementation.Stream(context.Background(), mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "run")})))
	if got, want := eventKinds(events), []string{"start", "text_start", "text_delta", "text_end", "toolcall_start", "toolcall_delta", "toolcall_end", "toolcall_start", "toolcall_delta", "toolcall_end", "done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	message, ok := terminal.(llm.AssistantToolUseMessage)
	if !ok {
		t.Fatalf("terminal = %T", terminal)
	}
	blocks := message.Blocks()
	if len(blocks) != 3 || blocks[0].(llm.TextBlock).Text() != "I will run both." || blocks[1].(llm.ToolCallBlock).ID() != "call_a|fc_a" || blocks[2].(llm.ToolCallBlock).ID() != "call_b|fc_b" {
		t.Fatalf("source-order blocks = %#v", blocks)
	}
}

func functionCallSSEEvents(index int, itemID, callID, arguments string) []any {
	return []any{
		map[string]any{"type": "response.output_item.added", "output_index": index, "item": map[string]any{"type": "function_call", "id": itemID, "call_id": callID, "name": "bash", "arguments": ""}},
		map[string]any{"type": "response.function_call_arguments.delta", "output_index": index, "item_id": itemID, "delta": arguments},
		map[string]any{"type": "response.function_call_arguments.done", "output_index": index, "item_id": itemID, "arguments": arguments},
		map[string]any{"type": "response.output_item.done", "output_index": index, "item": map[string]any{"type": "function_call", "id": itemID, "call_id": callID, "name": "bash", "arguments": arguments}},
	}
}

func TestToolDefinitionsAreValidatedAndImmutable(t *testing.T) {
	schema := []byte(`{"type":"object"}`)
	definition, err := provider.NewToolDefinition("one", "first tool", true, schema)
	if err != nil {
		t.Fatal(err)
	}
	schema[2] = 'X'
	if string(definition.ParametersJSON()) != `{"type":"object"}` {
		t.Fatalf("definition schema was mutated: %q", definition.ParametersJSON())
	}
	model, err := provider.NewModelRef("openai", provider.OpenAIResponsesAPI, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.NewRequestWithTools(model, "", nil, []provider.ToolDefinition{definition, definition}); !errors.Is(err, provider.ErrInvalidRequest) {
		t.Fatalf("duplicate definition request error = %v", err)
	}
	for _, bad := range [][]byte{nil, []byte(`[]`), []byte(`{`)} {
		if _, err := provider.NewToolDefinition("bad", "bad tool", true, bad); !errors.Is(err, provider.ErrInvalidToolDefinition) {
			t.Fatalf("schema %q error = %v", bad, err)
		}
	}
}

func TestToolDefinitionEnforcesOpenAIFunctionNameAdmission(t *testing.T) {
	t.Parallel()

	if _, err := provider.NewToolDefinition(strings.Repeat("a", 64), "valid", true, []byte(`{"type":"object"}`)); err != nil {
		t.Fatalf("64-character name rejected: %v", err)
	}
	invalidNames := []string{
		strings.Repeat("a", 65),
		"has.dot",
		"has space",
		"slash/name",
		"工具",
		string([]byte{0xff}),
	}
	for _, name := range invalidNames {
		if _, err := provider.NewToolDefinition(name, "invalid", true, []byte(`{"type":"object"}`)); !errors.Is(err, provider.ErrInvalidToolDefinition) {
			t.Fatalf("NewToolDefinition(%q) error = %v, want ErrInvalidToolDefinition", name, err)
		}
	}
}

func TestOpenAIResponsesReplayNormalizesNonFCIDsAndAppliesOriginlessFCShapePolicy(t *testing.T) {
	t.Parallel()

	model, err := provider.NewModelRef("openai", provider.OpenAIResponsesAPI, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	sharedLongPrefix := strings.Repeat("foreign/item+", 12)
	first := mustReplayCall(t, "call/one|"+sharedLongPrefix+"left", "one")
	second := mustReplayCall(t, "call:two|"+sharedLongPrefix+"right", "two")
	fcShaped := mustReplayCall(t, "call-three|fc_native-1", "three")
	assistant, err := llm.NewAssistantToolUseMessage(
		[]llm.AssistantBlock{first, second, fcShaped},
		llm.Usage{},
		responsesTestTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	messages := []llm.ConversationMessage{
		mustUser(t, "run"),
		assistant,
		mustToolResult(t, first.ID(), first.Name(), "one"),
		mustToolResult(t, second.ID(), second.Name(), "two"),
		mustToolResult(t, fcShaped.ID(), fcShaped.Name(), "three"),
	}
	request, err := provider.NewRequest(model, "", messages)
	if err != nil {
		t.Fatal(err)
	}

	firstPayload, err := encodeReplayForTest(request)
	if err != nil {
		t.Fatal(err)
	}
	secondPayload, err := encodeReplayForTest(request)
	if err != nil {
		t.Fatal(err)
	}
	firstFunctions := replayItemsOfType(t, firstPayload, "function_call")
	secondFunctions := replayItemsOfType(t, secondPayload, "function_call")
	if !reflect.DeepEqual(firstFunctions, secondFunctions) {
		t.Fatalf("normalization is not stable:\nfirst  = %#v\nsecond = %#v", firstFunctions, secondFunctions)
	}
	if len(firstFunctions) != 3 {
		t.Fatalf("function calls = %#v", firstFunctions)
	}

	firstID := firstFunctions[0]["id"].(string)
	secondID := firstFunctions[1]["id"].(string)
	for _, itemID := range []string{firstID, secondID} {
		if !isBoundedResponsesFunctionItemID(itemID) {
			t.Fatalf("normalized item id = %q, want bounded fc_ ASCII shape", itemID)
		}
	}
	if firstID == secondID {
		t.Fatalf("distinct raw IDs sharing a long prefix collided: %q", firstID)
	}
	if got := firstFunctions[2]["id"]; got != "fc_native-1" {
		t.Fatalf("originless fc-shaped item id = %v, want fc_native-1", got)
	}
	if got, want := []any{firstFunctions[0]["call_id"], firstFunctions[1]["call_id"], firstFunctions[2]["call_id"]}, []any{"call_one", "call_two", "call-three"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("call ids = %#v, want %#v", got, want)
	}

	outputs := replayItemsOfType(t, firstPayload, "function_call_output")
	if len(outputs) != 3 {
		t.Fatalf("function call outputs = %#v", outputs)
	}
	if got, want := []any{outputs[0]["call_id"], outputs[1]["call_id"], outputs[2]["call_id"]}, []any{"call_one", "call_two", "call-three"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("result call ids = %#v, want %#v", got, want)
	}
}

func TestOpenAIResponsesReplayAssignsDistinctIDsToMultipleTextBlocks(t *testing.T) {
	t.Parallel()

	model, err := provider.NewModelRef("openai", provider.OpenAIResponsesAPI, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	firstText := mustTextBlock(t, "before")
	secondText := mustTextBlock(t, "after")
	call := mustReplayCall(t, "call-1|fc_1", "bash")
	assistant, err := llm.NewAssistantToolUseMessage(
		[]llm.AssistantBlock{firstText, call, secondText},
		llm.Usage{},
		responsesTestTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequest(model, "", []llm.ConversationMessage{
		mustUser(t, "run"),
		assistant,
		mustToolResult(t, call.ID(), call.Name(), "done"),
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := encodeReplayForTest(request)
	if err != nil {
		t.Fatal(err)
	}
	messages := replayItemsOfType(t, payload, "message")
	if len(messages) != 2 || messages[0]["id"] != "msg_pi_1" || messages[1]["id"] != "msg_pi_1_1" {
		t.Fatalf("assistant message IDs = %#v, want msg_pi_1 and msg_pi_1_1", messages)
	}
}

func TestOpenAIResponsesToolStreamRejectsPartialMalformedAndOutOfOrderEvents(t *testing.T) {
	tests := []string{
		responsesSSE(
			map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc", "call_id": "call", "name": "bash", "arguments": ""}},
			map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "fc", "delta": `{"command":"echo`},
			map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc", "call_id": "call", "name": "bash"}},
		),
		responsesSSE(map[string]any{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"type": "function_call", "id": "fc", "call_id": "call", "name": "bash"}}),
		responsesSSE(map[string]any{"type": "response.unknown_progress", "value": 1}),
	}
	for _, body := range tests {
		implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{BaseURL: "https://fixture.test/v1", APIKey: "secret", Client: staticResponsesDoer(responsesHTTPResponse(http.StatusOK, "text/event-stream", body))})
		_, terminal := collectStream(t, implementation.Stream(context.Background(), mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")})))
		failure := terminalFailure(t, terminal)
		if !errors.Is(failure.Failure().Cause(), provider.ErrOpenAIResponsesStream) {
			t.Fatalf("cause = %v, want stream protocol error", failure.Failure().Cause())
		}
	}
}

func FuzzOpenAIResponsesToolFramesNeverPanic(f *testing.F) {
	f.Add([]byte(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`))
	f.Add([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc","call_id":"call","name":"bash"}}`))
	f.Fuzz(func(t *testing.T, frame []byte) {
		body := "data: " + string(frame) + "\n\n"
		implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
			BaseURL: "https://fixture.test/v1", APIKey: "secret",
			Client: staticResponsesDoer(responsesHTTPResponse(http.StatusOK, "text/event-stream", body)),
		})
		_, _, _ = collectStreamResult(implementation.Stream(context.Background(), mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "fuzz")})))
	})
}

// encodeReplayForTest goes through the real adapter's request transport so the
// test observes exactly the production JSON rather than a private helper.
func encodeReplayForTest(request provider.Request) (map[string]any, error) {
	var payload map[string]any
	body := responsesSSE(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}})
	implementation, err := provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "secret",
		Client: responsesDoerFunc(func(incoming *http.Request) (*http.Response, error) {
			err := json.NewDecoder(incoming.Body).Decode(&payload)
			return responsesHTTPResponse(http.StatusOK, "text/event-stream", body), err
		}),
	})
	if err != nil {
		return nil, err
	}
	_, _, err = collectStreamResult(implementation.Stream(context.Background(), request))
	return payload, err
}

func mustReplayCall(t *testing.T, id, name string) llm.ToolCallBlock {
	t.Helper()
	call, err := llm.NewToolCallBlock(id, name, []byte(`{}`))
	if err != nil {
		t.Fatalf("NewToolCallBlock() error = %v", err)
	}
	return call
}

func replayItemsOfType(t *testing.T, payload map[string]any, itemType string) []map[string]any {
	t.Helper()
	input, ok := payload["input"].([]any)
	if !ok {
		t.Fatalf("input = %#v", payload["input"])
	}
	var items []map[string]any
	for _, candidate := range input {
		item, ok := candidate.(map[string]any)
		if ok && item["type"] == itemType {
			items = append(items, item)
		}
	}
	return items
}

func isBoundedResponsesFunctionItemID(value string) bool {
	if len(value) > 64 || !strings.HasPrefix(value, "fc_") {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

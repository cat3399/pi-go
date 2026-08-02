package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
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

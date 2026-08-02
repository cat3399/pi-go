package provider_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

const onePixelPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

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
	mixed, err := llm.NewAssistantToolUseMessageWithReplay([]llm.AssistantBlock{text, call}, llm.Usage{}, responsesTestTime, &llm.AssistantProvenance{Provider: "openai", API: provider.OpenAIResponsesAPI, Model: "gpt-test"}, nil)
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

func TestOpenAIResponsesReplaysReasoningAndImageInputs(t *testing.T) {
	model, err := provider.NewModelRef("openai", provider.OpenAIResponsesAPI, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	text := mustTextBlock(t, "inspect this")
	image, err := llm.NewImageDataBlock("image/png", mustOnePixelPNG(t))
	if err != nil {
		t.Fatal(err)
	}
	user, err := llm.NewUserContentMessage([]llm.UserContentBlock{text, image}, responsesTestTime)
	if err != nil {
		t.Fatal(err)
	}
	reasoning, err := llm.NewThinkingBlock("brief plan", &llm.OpenAIResponsesReasoning{ItemID: "rs_1", EncryptedContent: "opaque-secret"})
	if err != nil {
		t.Fatal(err)
	}
	call, err := llm.NewToolCallBlock("call_1|fc_1", "bash", []byte(`{"command":"true"}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantToolUseMessageWithReplay([]llm.AssistantBlock{reasoning, call}, llm.Usage{}, responsesTestTime, &llm.AssistantProvenance{Provider: "openai", API: provider.OpenAIResponsesAPI, Model: "gpt-test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := llm.NewToolResultContentMessage(call.ID(), "bash", []llm.ToolResultContentBlock{mustTextBlock(t, "ok"), image}, false, responsesTestTime)
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequest(model, "", []llm.ConversationMessage{user, assistant, result})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := encodeReplayForTest(request)
	if err != nil {
		t.Fatal(err)
	}
	input := payload["input"].([]any)
	userWire := input[0].(map[string]any)["content"].([]any)
	if userWire[1].(map[string]any)["type"] != "input_image" || userWire[1].(map[string]any)["image_url"] != "data:image/png;base64,"+onePixelPNGBase64 {
		t.Fatalf("user image = %#v", userWire[1])
	}
	if reasoningWire := input[1].(map[string]any); reasoningWire["type"] != "reasoning" || reasoningWire["id"] != "rs_1" || reasoningWire["encrypted_content"] != "opaque-secret" {
		t.Fatalf("reasoning = %#v", reasoningWire)
	}
	output := input[3].(map[string]any)["output"].([]any)
	if len(output) != 2 || output[1].(map[string]any)["type"] != "input_image" {
		t.Fatalf("tool output = %#v", output)
	}
}

func TestOpenAIResponsesPreservesMessagePhaseAndIDForSameModelReplay(t *testing.T) {
	body := responsesSSE(
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg_commentary", "role": "assistant", "phase": "commentary", "content": []any{}}},
		map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": "msg_commentary", "delta": "working"},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg_commentary", "role": "assistant", "phase": "commentary", "content": []any{map[string]any{"type": "output_text", "text": "working"}}}},
		map[string]any{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"type": "message", "id": "msg_final", "role": "assistant", "phase": "final_answer", "content": []any{}}},
		map[string]any{"type": "response.output_text.delta", "output_index": 1, "item_id": "msg_final", "delta": "done"},
		map[string]any{"type": "response.output_item.done", "output_index": 1, "item": map[string]any{"type": "message", "id": "msg_final", "role": "assistant", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "done"}}}},
		map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1", "status": "completed", "output": []any{
			map[string]any{"type": "message", "id": "msg_commentary", "role": "assistant", "phase": "commentary"},
			map[string]any{"type": "message", "id": "msg_final", "role": "assistant", "phase": "final_answer"},
		}}},
	)
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "secret",
		Client: staticResponsesDoer(responsesHTTPResponse(http.StatusOK, "text/event-stream", body)),
	})
	events, terminal := collectStream(t, implementation.Stream(context.Background(), mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "go")})))
	if got, want := eventKinds(events), []string{"start", "text_start", "text_delta", "text_end", "text_start", "text_delta", "text_end", "done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	message, ok := terminal.(llm.AssistantTextMessage)
	if !ok {
		t.Fatalf("terminal = %T", terminal)
	}
	blocks := message.Content()
	if len(blocks) != 2 {
		t.Fatalf("blocks = %#v", blocks)
	}
	for index, want := range []llm.TextReplay{
		{MessageID: "msg_commentary", Phase: "commentary"},
		{MessageID: "msg_final", Phase: "final_answer"},
	} {
		got, exists := blocks[index].TextReplay()
		if !exists || got != want {
			t.Fatalf("block %d replay = (%#v, %t), want %#v", index, got, exists, want)
		}
	}

	directory := t.TempDir()
	entryIDs := []string{"entry-user", "entry-assistant"}
	transcript, err := session.Create(filepath.Join(directory, "phase-replay.jsonl"), session.CreateOptions{
		ID:         "phase-replay",
		WorkingDir: directory,
		NewEntryID: func() (string, error) {
			id := entryIDs[0]
			entryIDs = entryIDs[1:]
			return id, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Append(context.Background(), mustUser(t, "go"), session.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Append(context.Background(), message, session.AppendOptions{Assistant: session.AssistantProvenance{
		API:      provider.OpenAIResponsesAPI,
		Provider: provider.OpenAIProviderID,
		Model:    "test-model",
		Cost:     session.ZeroUsageCost(),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := session.Open(transcript.Path(), session.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })

	replayRequest := mustResponsesRequest(t, "", restarted.Context().Messages())
	payload, err := encodeReplayForTest(replayRequest)
	if err != nil {
		t.Fatal(err)
	}
	input := payload["input"].([]any)
	for index, want := range []llm.TextReplay{
		{MessageID: "msg_commentary", Phase: "commentary"},
		{MessageID: "msg_final", Phase: "final_answer"},
	} {
		wire := input[index+1].(map[string]any)
		if wire["id"] != want.MessageID || wire["phase"] != want.Phase {
			t.Fatalf("wire block %d = %#v", index, wire)
		}
	}
}

func TestOpenAIResponsesBackfillsAzureReasoningEncryptionFromTerminalOutput(t *testing.T) {
	body := responsesSSE(
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "rs_azure"}},
		map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": 0, "item_id": "rs_azure", "delta": "plan"},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "rs_azure"}},
		map[string]any{"type": "response.output_item.done", "output_index": 1, "item": map[string]any{"type": "message", "id": "msg_azure", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "answer"}}}},
		map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{
			map[string]any{"type": "reasoning", "id": "rs_azure", "encrypted_content": "terminal-cipher"},
			map[string]any{"type": "message", "id": "msg_azure", "role": "assistant"},
		}}},
	)
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: "https://azure.fixture.test/openai/v1", APIKey: "secret",
		Client: staticResponsesDoer(responsesHTTPResponse(http.StatusOK, "text/event-stream", body)),
	})
	events, terminal := collectStream(t, implementation.Stream(context.Background(), mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "go")})))
	if got, want := eventKinds(events), []string{"start", "thinking_start", "thinking_delta", "thinking_end", "text_start", "text_delta", "text_end", "done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	message, ok := terminal.(llm.AssistantRichMessage)
	if !ok {
		t.Fatalf("terminal = %T", terminal)
	}
	replay, exists := message.Blocks()[0].(llm.ThinkingBlock).OpenAIResponsesReplay()
	if !exists || replay.ItemID != "rs_azure" || replay.EncryptedContent != "terminal-cipher" {
		t.Fatalf("reasoning replay = (%#v, %t)", replay, exists)
	}

	directory := t.TempDir()
	transcript, err := session.Create(filepath.Join(directory, "azure-reasoning.jsonl"), session.CreateOptions{
		ID:         "azure-reasoning",
		WorkingDir: directory,
		NewEntryID: func() (string, error) { return "entry-assistant", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Append(context.Background(), message, session.AppendOptions{Assistant: session.AssistantProvenance{
		API:      provider.OpenAIResponsesAPI,
		Provider: provider.OpenAIProviderID,
		Model:    "test-model",
		Cost:     session.ZeroUsageCost(),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := session.Open(transcript.Path(), session.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restartedMessage := restarted.Context().Messages()[0].(llm.AssistantRichMessage)
	restartedReplay, exists := restartedMessage.Blocks()[0].(llm.ThinkingBlock).OpenAIResponsesReplay()
	if !exists || restartedReplay.ItemID != "rs_azure" || restartedReplay.EncryptedContent != "terminal-cipher" {
		t.Fatalf("restarted reasoning replay = (%#v, %t)", restartedReplay, exists)
	}
}

func TestOpenAIResponsesRejectsUnsafeTerminalReasoningBackfill(t *testing.T) {
	tests := []struct {
		name   string
		frames []any
	}{
		{
			name: "foreign item id",
			frames: []any{
				map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "rs_local"}},
				map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "reasoning", "id": "rs_foreign", "encrypted_content": "foreign-cipher"}}}},
			},
		},
		{
			name: "mismatched indexes",
			frames: []any{
				map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "rs_a"}},
				map[string]any{"type": "response.output_item.done", "output_index": 1, "item": map[string]any{"type": "reasoning", "id": "rs_b"}},
				map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{
					map[string]any{"type": "reasoning", "id": "rs_b", "encrypted_content": "cipher-b"},
					map[string]any{"type": "reasoning", "id": "rs_a", "encrypted_content": "cipher-a"},
				}}},
			},
		},
		{
			name: "duplicate item id",
			frames: []any{
				map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "rs_duplicate"}},
				map[string]any{"type": "response.output_item.done", "output_index": 1, "item": map[string]any{"type": "reasoning", "id": "rs_duplicate"}},
				map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{
					map[string]any{"type": "reasoning", "id": "rs_duplicate", "encrypted_content": "cipher-a"},
					map[string]any{"type": "reasoning", "id": "rs_duplicate", "encrypted_content": "cipher-b"},
				}}},
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
				BaseURL: "https://azure.fixture.test/openai/v1", APIKey: "secret",
				Client: staticResponsesDoer(responsesHTTPResponse(http.StatusOK, "text/event-stream", responsesSSE(testCase.frames...))),
			})
			_, terminal := collectStream(t, implementation.Stream(context.Background(), mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "go")})))
			failure := terminalFailure(t, terminal)
			assertProviderFailure(t, failure, provider.FailureInvalidResponse, provider.ErrOpenAIResponsesStream)
			if len(failure.Blocks()) != 0 {
				t.Fatalf("unsafe reasoning was persisted in failure blocks: %#v", failure.Blocks())
			}
		})
	}
}

func TestOpenAIResponsesOpaqueReplayRequiresExactAssistantProvenance(t *testing.T) {
	reasoning, err := llm.NewThinkingBlock("readable plan", &llm.OpenAIResponsesReasoning{ItemID: "rs_original", EncryptedContent: "opaque-secret"})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := llm.NewTextBlockWithReplay("answer", &llm.TextReplay{MessageID: "msg_original", Phase: "final_answer"})
	if err != nil {
		t.Fatal(err)
	}
	call, err := llm.NewToolCallBlock("call_original|fc_original", "bash", []byte(`{"command":"true"}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := llm.NewToolResultMessage(call.ID(), "bash", []llm.TextBlock{mustTextBlock(t, "ok")}, false, responsesTestTime)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name             string
		source           *llm.AssistantProvenance
		wantOpaque       bool
		wantToolID       bool
		wantOriginalTool bool
	}{
		{
			name: "same provider API and model",
			source: &llm.AssistantProvenance{
				Provider: provider.OpenAIProviderID,
				API:      provider.OpenAIResponsesAPI,
				Model:    "test-model",
			},
			wantOpaque:       true,
			wantToolID:       true,
			wantOriginalTool: true,
		},
		{
			name: "same dialect different model",
			source: &llm.AssistantProvenance{
				Provider: provider.OpenAIProviderID,
				API:      provider.OpenAIResponsesAPI,
				Model:    "other-model",
			},
		},
		{
			name: "foreign provider",
			source: &llm.AssistantProvenance{
				Provider: "azure-openai",
				API:      provider.OpenAIResponsesAPI,
				Model:    "test-model",
			},
			wantToolID: true,
		},
		{
			name: "foreign API",
			source: &llm.AssistantProvenance{
				Provider: provider.OpenAIProviderID,
				API:      "openai-chat-completions",
				Model:    "test-model",
			},
			wantToolID: true,
		},
		{name: "missing source provenance", wantToolID: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			assistant, err := llm.NewAssistantToolUseMessageWithReplay(
				[]llm.AssistantBlock{reasoning, answer, call},
				llm.Usage{},
				responsesTestTime,
				testCase.source,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			messages := []llm.ConversationMessage{assistant, result}
			if testCase.source != nil {
				directory := t.TempDir()
				entryIDs := []string{"entry-assistant", "entry-result"}
				transcript, err := session.Create(filepath.Join(directory, "provenance-replay.jsonl"), session.CreateOptions{
					ID:         "provenance-replay",
					WorkingDir: directory,
					NewEntryID: func() (string, error) {
						id := entryIDs[0]
						entryIDs = entryIDs[1:]
						return id, nil
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := transcript.Append(context.Background(), assistant, session.AppendOptions{Assistant: session.AssistantProvenance{
					API:      testCase.source.API,
					Provider: testCase.source.Provider,
					Model:    testCase.source.Model,
					Cost:     session.ZeroUsageCost(),
				}}); err != nil {
					t.Fatal(err)
				}
				if _, err := transcript.Append(context.Background(), result, session.AppendOptions{}); err != nil {
					t.Fatal(err)
				}
				if err := transcript.Close(); err != nil {
					t.Fatal(err)
				}
				restarted, err := session.Open(transcript.Path(), session.OpenOptions{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = restarted.Close() })
				messages = restarted.Context().Messages()
			}
			request := mustResponsesRequest(t, "", messages)
			payload, err := encodeReplayForTest(request)
			if err != nil {
				t.Fatal(err)
			}
			input := payload["input"].([]any)
			if len(input) != 4 {
				t.Fatalf("input = %#v", input)
			}
			first := input[0].(map[string]any)
			text := input[1].(map[string]any)
			function := input[2].(map[string]any)
			output := input[3].(map[string]any)
			if testCase.wantOpaque {
				if first["type"] != "reasoning" || first["id"] != "rs_original" || first["encrypted_content"] != "opaque-secret" {
					t.Fatalf("reasoning replay = %#v", first)
				}
				if text["id"] != "msg_original" || text["phase"] != "final_answer" {
					t.Fatalf("text replay = %#v", text)
				}
			} else {
				if first["type"] != "message" || first["id"] != "msg_pi_0" || first["phase"] != nil {
					t.Fatalf("reasoning fallback = %#v", first)
				}
				if text["id"] != "msg_pi_0_1" || text["phase"] != nil {
					t.Fatalf("text fallback = %#v", text)
				}
			}
			toolID, hasToolID := function["id"].(string)
			if hasToolID != testCase.wantToolID {
				t.Fatalf("function id presence = %t (%#v)", hasToolID, function)
			}
			if testCase.wantOriginalTool && toolID != "fc_original" {
				t.Fatalf("function id = %q, want original", toolID)
			}
			if testCase.wantToolID && !testCase.wantOriginalTool && (!strings.HasPrefix(toolID, "fc_") || toolID == "fc_original") {
				t.Fatalf("function id = %q, want stable normalized replacement", toolID)
			}
			if function["call_id"] != "call_original" || output["call_id"] != "call_original" {
				t.Fatalf("tool pairing = function %#v, output %#v", function, output)
			}
		})
	}
}

func TestOpenAIResponsesDropsForeignRedactedReasoning(t *testing.T) {
	reasoning, err := llm.NewThinkingBlock("must not downgrade", &llm.OpenAIResponsesReasoning{
		ItemID:           "rs_redacted",
		EncryptedContent: "opaque-secret",
		Redacted:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	answer := mustTextBlock(t, "answer")
	message, err := llm.NewAssistantRichMessageWithReplay(
		[]llm.AssistantBlock{reasoning, answer},
		llm.FinishStop,
		llm.Usage{},
		responsesTestTime,
		&llm.AssistantProvenance{Provider: "foreign", API: provider.OpenAIResponsesAPI, Model: "test-model"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := encodeReplayForTest(mustResponsesRequest(t, "", []llm.ConversationMessage{message}))
	if err != nil {
		t.Fatal(err)
	}
	input := payload["input"].([]any)
	if len(input) != 1 || input[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "answer" {
		t.Fatalf("redacted foreign replay = %#v", input)
	}
}

func mustOnePixelPNG(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(onePixelPNGBase64)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestOpenAIResponsesStreamsReasoningThenText(t *testing.T) {
	body := responsesSSE(
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "rs_1"}},
		map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": 0, "item_id": "rs_1", "delta": "plan"},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "rs_1", "encrypted_content": "cipher"}},
		map[string]any{"type": "response.output_item.done", "output_index": 1, "item": map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "answer"}}}},
		map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "reasoning"}, map[string]any{"type": "message"}}}},
	)
	p := mustResponsesProvider(t, provider.OpenAIResponsesConfig{BaseURL: "https://fixture.test/v1", APIKey: "secret", Client: staticResponsesDoer(responsesHTTPResponse(http.StatusOK, "text/event-stream", body))})
	events, terminal := collectStream(t, p.Stream(context.Background(), mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "go")})))
	if got, want := eventKinds(events), []string{"start", "thinking_start", "thinking_delta", "thinking_end", "text_start", "text_delta", "text_end", "done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%v", got)
	}
	rich, ok := terminal.(llm.AssistantRichMessage)
	if !ok {
		t.Fatalf("terminal=%T", terminal)
	}
	blocks := rich.Blocks()
	replay, ok := blocks[0].(llm.ThinkingBlock).OpenAIResponsesReplay()
	if !ok || replay.EncryptedContent != "cipher" || blocks[1].(llm.TextBlock).Text() != "answer" {
		t.Fatalf("blocks=%#v", blocks)
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

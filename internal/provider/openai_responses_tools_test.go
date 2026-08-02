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

type responsesTextSignatureTest struct {
	Version int    `json:"v"`
	ID      string `json:"id"`
	Phase   string `json:"phase,omitempty"`
}

func responsesReasoningBlock(t *testing.T, text, id, encryptedContent, plaintextContent string, redacted bool) llm.ThinkingBlock {
	t.Helper()
	raw, err := json.Marshal(struct {
		Type             string `json:"type"`
		ID               string `json:"id"`
		EncryptedContent string `json:"encrypted_content,omitempty"`
		Content          string `json:"content,omitempty"`
	}{Type: "reasoning", ID: id, EncryptedContent: encryptedContent, Content: plaintextContent})
	if err != nil {
		t.Fatal(err)
	}
	block, err := llm.NewThinkingBlockWithSignature(text, string(raw), redacted)
	if err != nil {
		t.Fatal(err)
	}
	return block
}

func responsesTextBlock(t *testing.T, text, id, phase string) llm.TextBlock {
	t.Helper()
	raw, err := json.Marshal(responsesTextSignatureTest{Version: 1, ID: id, Phase: phase})
	if err != nil {
		t.Fatal(err)
	}
	block, err := llm.NewTextBlockWithSignature(text, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return block
}

func responsesTextSignatureForTest(t *testing.T, block llm.TextBlock) responsesTextSignatureTest {
	t.Helper()
	signature, ok := block.TextSignature()
	if !ok {
		t.Fatal("missing text signature")
	}
	var value responsesTextSignatureTest
	if err := json.Unmarshal([]byte(signature), &value); err != nil {
		t.Fatalf("decode text signature: %v", err)
	}
	return value
}

func responsesReasoningSignatureForTest(t *testing.T, block llm.ThinkingBlock) map[string]any {
	t.Helper()
	signature, ok := block.ThinkingSignature()
	if !ok {
		t.Fatal("missing reasoning signature")
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(signature), &value); err != nil {
		t.Fatalf("decode reasoning signature: %v", err)
	}
	return value
}

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
	if parallel, ok := firstPayload["parallel_tool_calls"].(bool); !ok || parallel {
		t.Fatalf("parallel_tool_calls = %#v, want false", firstPayload["parallel_tool_calls"])
	}

	text, err := llm.NewTextBlock("I will run it.")
	if err != nil {
		t.Fatal(err)
	}
	mixed, err := llm.NewAssistantToolUseMessageWithMetadata([]llm.AssistantBlock{text, call}, llm.Usage{}, responsesTestTime, &llm.AssistantProvenance{Provider: "openai", API: provider.OpenAIResponsesAPI, Model: "gpt-test"}, nil)
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
	reasoning := responsesReasoningBlock(t, "brief plan", "rs_1", "opaque-secret", "", false)
	call, err := llm.NewToolCallBlock("call_1|fc_1", "bash", []byte(`{"command":"true"}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantToolUseMessageWithMetadata([]llm.AssistantBlock{reasoning, call}, llm.Usage{}, responsesTestTime, &llm.AssistantProvenance{Provider: "openai", API: provider.OpenAIResponsesAPI, Model: "gpt-test"}, nil)
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
	for index, want := range []responsesTextSignatureTest{
		{Version: 1, ID: "msg_commentary", Phase: "commentary"},
		{Version: 1, ID: "msg_final", Phase: "final_answer"},
	} {
		got := responsesTextSignatureForTest(t, blocks[index])
		if got != want {
			t.Fatalf("block %d signature = %#v, want %#v", index, got, want)
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
	for index, want := range []responsesTextSignatureTest{
		{Version: 1, ID: "msg_commentary", Phase: "commentary"},
		{Version: 1, ID: "msg_final", Phase: "final_answer"},
	} {
		wire := input[index+1].(map[string]any)
		if wire["id"] != want.ID || wire["phase"] != want.Phase {
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
	replay := responsesReasoningSignatureForTest(t, message.Blocks()[0].(llm.ThinkingBlock))
	if replay["id"] != "rs_azure" || replay["encrypted_content"] != "terminal-cipher" {
		t.Fatalf("reasoning signature = %#v", replay)
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
	restartedReplay := responsesReasoningSignatureForTest(t, restartedMessage.Blocks()[0].(llm.ThinkingBlock))
	if restartedReplay["id"] != "rs_azure" || restartedReplay["encrypted_content"] != "terminal-cipher" {
		t.Fatalf("restarted reasoning signature = %#v", restartedReplay)
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
	reasoning := responsesReasoningBlock(t, "readable plan", "rs_original", "opaque-secret", "", false)
	answer := responsesTextBlock(t, "answer", "msg_original", "final_answer")
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
			assistant, err := llm.NewAssistantToolUseMessageWithMetadata(
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
	reasoning := responsesReasoningBlock(t, "must not downgrade", "rs_redacted", "opaque-secret", "", true)
	answer := mustTextBlock(t, "answer")
	message, err := llm.NewAssistantRichMessageWithMetadata(
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
	replay := responsesReasoningSignatureForTest(t, blocks[0].(llm.ThinkingBlock))
	if replay["encrypted_content"] != "cipher" || blocks[1].(llm.TextBlock).Text() != "answer" {
		t.Fatalf("blocks=%#v", blocks)
	}
}

func TestOpenAIResponsesProgressDoneAndPlaintextReasoningReplay(t *testing.T) {
	frames := []any{
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "rs_plain"}},
		map[string]any{"type": "response.reasoning_text.delta", "output_index": 0, "item_id": "rs_plain", "delta": "plan"},
		map[string]any{"type": "response.reasoning_text.done", "output_index": 0, "item_id": "rs_plain", "text": "plan"},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "rs_plain", "content": "plan"}},
	}
	frames = append(frames, functionCallSSEEvents(1, "fc_plain", "call_plain", `{"marker":"ok"}`)...)
	frames = append(frames, map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "reasoning", "id": "rs_plain", "content": "plan"}, map[string]any{"type": "function_call", "id": "fc_plain", "call_id": "call_plain", "name": "bash"}}}})
	p := mustResponsesProvider(t, provider.OpenAIResponsesConfig{BaseURL: "https://fixture.test/v1", APIKey: "secret", Client: staticResponsesDoer(responsesHTTPResponse(http.StatusOK, "text/event-stream", responsesSSE(frames...)))})
	events, terminal := collectStream(t, p.Stream(context.Background(), mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "go")})))
	if got, want := eventKinds(events), []string{"start", "thinking_start", "thinking_delta", "thinking_end", "toolcall_start", "toolcall_delta", "toolcall_end", "done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	rich, ok := terminal.(llm.AssistantToolUseMessage)
	if !ok {
		t.Fatalf("terminal = %T", terminal)
	}
	blocks := rich.Blocks()
	replay := responsesReasoningSignatureForTest(t, blocks[0].(llm.ThinkingBlock))
	if replay["content"] != "plan" {
		t.Fatalf("reasoning signature = %#v", replay)
	}
	payload, err := encodeReplayForTest(mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "go"), terminal, mustToolResult(t, "call_plain|fc_plain", "bash", "ok")}))
	if err != nil {
		t.Fatal(err)
	}
	var reasoning map[string]any
	for _, raw := range payload["input"].([]any) {
		item, ok := raw.(map[string]any)
		if ok && item["type"] == "reasoning" {
			reasoning = item
			break
		}
	}
	if reasoning == nil || reasoning["content"] != "plan" || reasoning["encrypted_content"] != nil || reasoning["summary"] != nil {
		t.Fatalf("plaintext replay input = %#v", reasoning)
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
	wantSchema := `{"type":"object","additionalProperties":false,"properties":{},"required":[]}`
	schema := []byte(wantSchema)
	definition, err := provider.NewToolDefinition("one", "first tool", true, schema)
	if err != nil {
		t.Fatal(err)
	}
	schema[2] = 'X'
	if string(definition.ParametersJSON()) != wantSchema {
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

	if _, err := provider.NewToolDefinition(strings.Repeat("a", 64), "valid", false, []byte(`{"type":"object"}`)); err != nil {
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
		if _, err := provider.NewToolDefinition(name, "invalid", false, []byte(`{"type":"object"}`)); !errors.Is(err, provider.ErrInvalidToolDefinition) {
			t.Fatalf("NewToolDefinition(%q) error = %v, want ErrInvalidToolDefinition", name, err)
		}
	}
}

func TestToolDefinitionStrictSchemaAdmission(t *testing.T) {
	t.Parallel()

	valid := []string{
		`{"type":"object","additionalProperties":false,"properties":{},"required":[]}`,
		`{"type":"object","additionalProperties":false,"properties":{"value":{"type":["string","null"]}},"required":["value"]}`,
		`{"type":"object","additionalProperties":false,"properties":{"values":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"id":{"type":"integer"}},"required":["id"]}}},"required":["values"]}`,
		`{"type":"object","additionalProperties":false,"properties":{"entry":{"$ref":"#/$defs/entry"}},"required":["entry"],"$defs":{"entry":{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string"}},"required":["name"]}}}`,
	}
	for index, schema := range valid {
		if _, err := provider.NewToolDefinition("valid", "valid strict schema", true, []byte(schema)); err != nil {
			t.Fatalf("valid strict schema %d rejected: %v", index, err)
		}
	}

	invalid := []struct {
		name   string
		schema string
	}{
		{name: "root type", schema: `{"type":"array","items":{"type":"string"}}`},
		{name: "root anyOf", schema: `{"type":"object","anyOf":[{"type":"object","additionalProperties":false,"properties":{},"required":[]}],"additionalProperties":false,"properties":{},"required":[]}`},
		{name: "missing additional properties", schema: `{"type":"object","properties":{},"required":[]}`},
		{name: "open additional properties", schema: `{"type":"object","additionalProperties":true,"properties":{},"required":[]}`},
		{name: "missing required", schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"type":"string"}}}`},
		{name: "omitted property", schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"type":"string"},"optional":{"type":"string"}},"required":["value"]}`},
		{name: "undeclared required", schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"type":"string"}},"required":["value","other"]}`},
		{name: "duplicate required", schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"type":"string"}},"required":["value","value"]}`},
		{name: "nested object incomplete", schema: `{"type":"object","additionalProperties":false,"properties":{"nested":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}},"required":["nested"]}`},
		{name: "array missing items", schema: `{"type":"object","additionalProperties":false,"properties":{"values":{"type":"array"}},"required":["values"]}`},
		{name: "unsupported composition", schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"type":"string","allOf":[{"type":"string"}]}},"required":["value"]}`},
		{name: "external reference", schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"$ref":"https://example.test/schema"}},"required":["value"]}`},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := provider.NewToolDefinition("invalid", "invalid strict schema", true, []byte(test.schema)); !errors.Is(err, provider.ErrInvalidToolDefinition) {
				t.Fatalf("NewToolDefinition() error = %v, want ErrInvalidToolDefinition", err)
			}
		})
	}

	optional := []byte(`{"type":"object","additionalProperties":false,"properties":{"value":{"type":"string"}},"required":[]}`)
	if _, err := provider.NewToolDefinition("non_strict", "optional property", false, optional); err != nil {
		t.Fatalf("non-strict optional schema rejected: %v", err)
	}
}

func TestToolDefinitionStrictSchemaResolvesLocalJSONPointers(t *testing.T) {
	t.Parallel()

	valid := []struct {
		name   string
		schema string
	}{
		{
			name:   "root recursion",
			schema: `{"type":"object","additionalProperties":false,"properties":{"children":{"type":"array","items":{"$ref":"#"}}},"required":["children"]}`,
		},
		{
			name:   "escaped definition token",
			schema: `{"type":"object","additionalProperties":false,"properties":{"entry":{"$ref":"#/%24defs/a~1b~0c"}},"required":["entry"],"$defs":{"a/b~c":{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string"}},"required":["name"]}}}`,
		},
		{
			name:   "array token",
			schema: `{"type":"object","additionalProperties":false,"properties":{"entry":{"$ref":"#/$defs/choice/anyOf/1"}},"required":["entry"],"$defs":{"choice":{"anyOf":[{"type":"string"},{"type":"object","additionalProperties":false,"properties":{},"required":[]}]}}}`,
		},
		{
			name:   "recursive definition",
			schema: `{"type":"object","additionalProperties":false,"properties":{"node":{"$ref":"#/$defs/node"}},"required":["node"],"$defs":{"node":{"type":"object","additionalProperties":false,"properties":{"value":{"type":"string"},"next":{"anyOf":[{"$ref":"#/$defs/node"},{"type":"null"}]}},"required":["value","next"]}}}`,
		},
	}
	for _, test := range valid {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := provider.NewToolDefinition("valid_ref", "valid local reference", true, []byte(test.schema)); err != nil {
				t.Fatalf("valid strict schema rejected: %v", err)
			}
		})
	}

	invalid := []struct {
		name   string
		schema string
	}{
		{
			name:   "missing target",
			schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"$ref":"#/$defs/missing"}},"required":["value"],"$defs":{}}`,
		},
		{
			name:   "pointer must start with slash",
			schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"$ref":"#$defs/value"}},"required":["value"]}`,
		},
		{
			name:   "invalid tilde escape",
			schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"$ref":"#/$defs/a~2b"}},"required":["value"],"$defs":{}}`,
		},
		{
			name:   "incomplete tilde escape",
			schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"$ref":"#/$defs/value~"}},"required":["value"],"$defs":{}}`,
		},
		{
			name:   "invalid percent escape",
			schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"$ref":"#/$defs/%zz"}},"required":["value"],"$defs":{}}`,
		},
		{
			name:   "noncanonical array token",
			schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"$ref":"#/$defs/choice/anyOf/01"}},"required":["value"],"$defs":{"choice":{"anyOf":[{"type":"string"},{"type":"number"}]}}}`,
		},
		{
			name:   "null type beside reference",
			schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"type":null,"$ref":"#/$defs/value"}},"required":["value"],"$defs":{"value":{"type":"string"}}}`,
		},
		{
			name:   "array token out of range",
			schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"$ref":"#/$defs/choice/anyOf/2"}},"required":["value"],"$defs":{"choice":{"anyOf":[{"type":"string"}]}}}`,
		},
		{
			name:   "scalar target",
			schema: `{"type":"object","description":"not a schema","additionalProperties":false,"properties":{"value":{"$ref":"#/description"}},"required":["value"]}`,
		},
		{
			name:   "array target",
			schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"$ref":"#/required"}},"required":["value"]}`,
		},
		{
			name:   "schema container target",
			schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"$ref":"#/$defs"}},"required":["value"],"$defs":{"value":{"type":"string"}}}`,
		},
		{
			name:   "remote target",
			schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"$ref":"https://example.test/schema"}},"required":["value"]}`,
		},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := provider.NewToolDefinition("invalid_ref", "invalid local reference", true, []byte(test.schema)); !errors.Is(err, provider.ErrInvalidToolDefinition) {
				t.Fatalf("NewToolDefinition() error = %v, want ErrInvalidToolDefinition", err)
			}
		})
	}
}

func TestToolDefinitionStrictAdditionalPropertiesRequiresBooleanFalseEverywhere(t *testing.T) {
	t.Parallel()

	invalid := []struct {
		name   string
		schema string
	}{
		{name: "root null", schema: `{"type":"object","additionalProperties":null,"properties":{},"required":[]}`},
		{name: "root true", schema: `{"type":"object","additionalProperties":true,"properties":{},"required":[]}`},
		{name: "root string", schema: `{"type":"object","additionalProperties":"false","properties":{},"required":[]}`},
		{name: "root number", schema: `{"type":"object","additionalProperties":0,"properties":{},"required":[]}`},
		{name: "nested object", schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"type":"object","additionalProperties":null,"properties":{},"required":[]}},"required":["value"]}`},
		{name: "array items", schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"type":"array","items":{"type":"object","additionalProperties":"false","properties":{},"required":[]}}},"required":["value"]}`},
		{name: "anyOf branch", schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"anyOf":[{"type":"object","additionalProperties":0,"properties":{},"required":[]},{"type":"null"}]}},"required":["value"]}`},
		{name: "definition", schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"$ref":"#/$defs/value"}},"required":["value"],"$defs":{"value":{"type":"object","additionalProperties":true,"properties":{},"required":[]}}}`},
		{name: "reference-only target", schema: `{"type":"object","additionalProperties":false,"properties":{"value":{"$ref":"#/x-target"}},"required":["value"],"x-target":{"type":"object","additionalProperties":null,"properties":{},"required":[]}}`},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := provider.NewToolDefinition("invalid_additional", "invalid additional properties", true, []byte(test.schema)); !errors.Is(err, provider.ErrInvalidToolDefinition) {
				t.Fatalf("NewToolDefinition() error = %v, want ErrInvalidToolDefinition", err)
			}
		})
	}
}

func TestToolDefinitionStrictSchemaTraversalIsBounded(t *testing.T) {
	t.Parallel()

	var nested any = map[string]any{"type": "string"}
	for range 300 {
		nested = map[string]any{"type": "array", "items": nested}
	}
	schema, err := json.Marshal(map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           map[string]any{"value": nested},
		"required":             []string{"value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.NewToolDefinition("too_deep", "deep strict schema", true, schema); !errors.Is(err, provider.ErrInvalidToolDefinition) {
		t.Fatalf("NewToolDefinition() error = %v, want ErrInvalidToolDefinition", err)
	}

	referenceSchema, err := json.Marshal(map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"value": map[string]any{"$ref": "#/" + strings.Repeat("segment/", 300) + "target"},
		},
		"required": []string{"value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.NewToolDefinition("ref_too_deep", "deep local reference", true, referenceSchema); !errors.Is(err, provider.ErrInvalidToolDefinition) {
		t.Fatalf("deep reference error = %v, want ErrInvalidToolDefinition", err)
	}
}

func FuzzToolDefinitionStrictLocalReferenceNeverPanics(f *testing.F) {
	for _, reference := range []string{"#", "#/$defs/value", "#/%24defs/a~1b~0c", "#/$defs/choice/anyOf/0", "#/$defs/missing", "https://example.test/schema"} {
		f.Add(reference)
	}
	f.Fuzz(func(t *testing.T, reference string) {
		schema, err := json.Marshal(map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"value": map[string]any{"$ref": reference},
			},
			"required": []string{"value"},
			"$defs": map[string]any{
				"value": map[string]any{"type": "string"},
				"a/b~c": map[string]any{"type": "number"},
				"choice": map[string]any{
					"anyOf": []any{map[string]any{"type": "boolean"}},
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = provider.NewToolDefinition("fuzz_ref", "fuzz local reference", true, schema)
	})
}

func TestOpenAIResponsesParallelToolCallsCapabilityIsExplicit(t *testing.T) {
	t.Parallel()

	model, err := provider.NewModelRef("openai", provider.OpenAIResponsesAPI, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	definition, err := provider.NewToolDefinition("echo", "Echo input.", false, []byte(`{"type":"object","properties":{"value":{"type":"string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	defaultRequest, err := provider.NewRequestWithTools(model, "", []llm.ConversationMessage{mustUser(t, "echo")}, []provider.ToolDefinition{definition})
	if err != nil {
		t.Fatal(err)
	}
	enabledRequest, err := provider.NewRequestWithOptions(model, "", []llm.ConversationMessage{mustUser(t, "echo")}, provider.RequestOptions{
		Tools:                  []provider.ToolDefinition{definition},
		AllowParallelToolCalls: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := enabledRequest.Tools(); len(got) != 1 || got[0].Name() != definition.Name() {
		t.Fatalf("Tools() = %#v, want preserved echo definition", got)
	}
	if got := enabledRequest.ReplayTarget(); got != (llm.AssistantProvenance{
		Provider: model.Provider(),
		API:      model.API(),
		Model:    model.ID(),
	}) {
		t.Fatalf("ReplayTarget() = %#v, want exact request model provenance", got)
	}
	for _, test := range []struct {
		name    string
		request provider.Request
		want    bool
	}{
		{name: "safe default", request: defaultRequest, want: false},
		{name: "explicit capability", request: enabledRequest, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.request.ParallelToolCalls(); got != test.want {
				t.Fatalf("ParallelToolCalls() = %t, want %t", got, test.want)
			}
			payload, err := encodeReplayForTest(test.request)
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := payload["parallel_tool_calls"].(bool); !ok || got != test.want {
				t.Fatalf("parallel_tool_calls = %#v, want %t", payload["parallel_tool_calls"], test.want)
			}
		})
	}
}

func TestOpenAIResponsesReplayNormalizesAndGatesOriginlessItemIDs(t *testing.T) {
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
	if got, ok := firstFunctions[2]["id"].(string); !ok || got == "fc_native-1" || !isBoundedResponsesFunctionItemID(got) {
		t.Fatalf("originless fc-shaped item id = %v, want stable gated replacement", firstFunctions[2]["id"])
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

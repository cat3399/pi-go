package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/resource"
	"github.com/cat3399/pi-go/internal/session"
)

// This joins the legacy Session admission path to the production-shaped
// Responses retry controller. Both attempts must rebuild the same migrated
// context, while only the accepted turn is appended to durable v3 state.
func TestCoreIntegrationOpensLegacyContextForProductionRetry(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "legacy-retry.jsonl")
	legacy := []byte(
		`{"type":"session","version":2,"id":"legacy-retry","timestamp":"2026-08-01T00:00:00.000Z","cwd":"/workspace"}` + "\n" +
			`{"type":"message","id":"legacy-root","parentId":null,"timestamp":"2026-08-01T00:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"legacy context"}],"timestamp":1785542401000}}` + "\n",
	)
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	entryIDs := []string{"retry-user", "retry-assistant"}
	nextEntryID := 0
	transcript, err := session.Open(path, session.OpenOptions{
		Now: func() time.Time { return agentTestEpoch },
		NewEntryID: func() (string, error) {
			if nextEntryID >= len(entryIDs) {
				return "", fmt.Errorf("unexpected entry id request %d", nextEntryID)
			}
			id := entryIDs[nextEntryID]
			nextEntryID++
			return id, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })

	var requestMu sync.Mutex
	var payloads []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode retry request: %v", err)
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		requestMu.Lock()
		payloads = append(payloads, payload)
		requestNumber := len(payloads)
		requestMu.Unlock()
		switch requestNumber {
		case 1:
			dropContextSSE(t, w)
		case 2:
			writeContextSSE(t, w, "legacy retry final")
		default:
			t.Errorf("unexpected retry request %d", requestNumber)
		}
	}))
	defer server.Close()
	model, implementation := contextRetryProvider(t, server.URL)
	coordinator, err := agent.New(agent.Config{
		Provider: implementation, Transcript: transcript, Model: model,
		Retry: agent.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }},
		Now:   func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Run(context.Background(), "new retry prompt")
	if err != nil || !result.Succeeded() {
		t.Fatalf("legacy retry run = (%#v, %v)", result, err)
	}

	requestMu.Lock()
	requests := append([]map[string]any(nil), payloads...)
	requestMu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("legacy retry requests = %d", len(requests))
	}
	for index, payload := range requests {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		wire := string(encoded)
		if strings.Count(wire, "legacy context") != 1 || strings.Count(wire, "new retry prompt") != 1 {
			t.Fatalf("retry request %d rebuilt wrong migrated context: %s", index+1, wire)
		}
	}
	if entries := transcript.Entries(); len(entries) != 3 {
		t.Fatalf("legacy retry durable entries = %d, want 3", len(entries))
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"version":3`) {
		t.Fatalf("legacy retry session was not v3: %v / %s", err, data)
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := session.Open(path, session.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if messages := reopened.BuildContext().Messages(); len(messages) != 3 {
		t.Fatalf("reopened legacy retry context = %#v", messages)
	}
}

// This is the final local production-shape oracle across trusted resources,
// Responses rich/tool replay, parallel Agent execution, durable Session state,
// and a transient provider retry. The failed attempt must reconstruct the
// complete causal transcript without appending any of it a second time.
func TestCoreIntegrationRetriesRichParallelToolReplayWithoutDuplicateSession(t *testing.T) {
	workingDir, agentDir := t.TempDir(), t.TempDir()
	const trustedPrompt = "trusted core integration system prompt"
	if err := os.WriteFile(filepath.Join(agentDir, "SYSTEM.md"), []byte(trustedPrompt), 0o600); err != nil {
		t.Fatal(err)
	}
	resources, err := resource.New(resource.Config{
		CWD: workingDir, AgentDir: agentDir,
		Tools: []resource.Tool{
			{Name: "slow", Snippet: "Run the slow fixture operation."},
			{Name: "fast", Snippet: "Run the fast fixture operation."},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := resources.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	resourceSnapshot, err := resources.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resourceSnapshot.SystemPrompt, trustedPrompt) {
		t.Fatalf("trusted system prompt = %q", resourceSnapshot.SystemPrompt)
	}

	var requestMu sync.Mutex
	var payloads []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("request path = %q", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		requestMu.Lock()
		payloads = append(payloads, payload)
		requestNumber := len(payloads)
		requestMu.Unlock()
		switch requestNumber {
		case 1:
			writeCoreIntegrationToolSSE(t, w)
		case 2:
			dropContextSSE(t, w)
		case 3:
			writeContextSSE(t, w, "final integrated answer")
		default:
			t.Errorf("unexpected provider request %d", requestNumber)
			http.Error(w, "unexpected fixture request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	model, err := provider.NewModelRef(provider.OpenAIProviderID, provider.OpenAIResponsesAPI, "fixture-integrated")
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{
		BaseURL: server.URL + "/v1", APIKey: "fixture-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	definitions := coreIntegrationToolDefinitions(t)
	toolRuntime := &namedBatchTool{
		started: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
		release: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
	}
	transcript := newSession(t)
	coordinator, err := agent.New(agent.Config{
		Provider: implementation, Transcript: transcript, Model: model,
		SystemPrompt: resourceSnapshot.SystemPrompt, Tool: toolRuntime, Tools: definitions,
		Retry: agent.RetryPolicy{
			MaxAttempts: 2,
			Sleep:       func(context.Context, time.Duration) error { return nil },
		},
		Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}

	var eventMu sync.Mutex
	var events []agent.Event
	toolSettled := make(chan string, 2)
	coordinator.Subscribe(func(_ context.Context, event agent.Event) {
		eventMu.Lock()
		events = append(events, event)
		eventMu.Unlock()
		if event.Kind == agent.EventToolSettled {
			toolSettled <- event.ToolCallID
		}
	})
	type runOutcome struct {
		result agent.Result
		err    error
	}
	done := make(chan runOutcome, 1)
	runContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		result, runErr := coordinator.Run(runContext, "use both trusted tools")
		done <- runOutcome{result: result, err: runErr}
	}()

	waitClosed(t, toolRuntime.started["slow"], "integrated slow tool")
	waitClosed(t, toolRuntime.started["fast"], "integrated fast tool")
	close(toolRuntime.release["fast"])
	if callID := receiveCoreToolSettlement(t, toolSettled); callID != "call_fast|fc_fast" {
		t.Fatalf("first tool settlement = %q", callID)
	}
	close(toolRuntime.release["slow"])
	if callID := receiveCoreToolSettlement(t, toolSettled); callID != "call_slow|fc_slow" {
		t.Fatalf("second tool settlement = %q", callID)
	}

	var outcome runOutcome
	select {
	case outcome = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("integrated run did not settle")
	}
	if outcome.err != nil || !outcome.result.Succeeded() || outcome.result.ProviderTurns() != 3 || outcome.result.ToolExecutions() != 2 {
		t.Fatalf("Run() = success %t, turns %d, tools %d, error %v", outcome.result.Succeeded(), outcome.result.ProviderTurns(), outcome.result.ToolExecutions(), outcome.err)
	}
	terminal, ok := outcome.result.Terminal()
	final, finalOK := terminal.(llm.AssistantTextMessage)
	if !ok || !finalOK || joinContextText(final.Content()) != "final integrated answer" {
		t.Fatalf("terminal = %T %#v", terminal, terminal)
	}

	requestMu.Lock()
	received := append([]map[string]any(nil), payloads...)
	requestMu.Unlock()
	if len(received) != 3 {
		t.Fatalf("provider requests = %d, want 3", len(received))
	}
	for requestIndex, payload := range received {
		if parallel, ok := payload["parallel_tool_calls"].(bool); !ok || !parallel {
			t.Fatalf("request %d parallel_tool_calls = %#v", requestIndex+1, payload["parallel_tool_calls"])
		}
		tools, ok := payload["tools"].([]any)
		if !ok || len(tools) != 2 {
			t.Fatalf("request %d tools = %#v", requestIndex+1, payload["tools"])
		}
		input, ok := payload["input"].([]any)
		if !ok || len(input) < 2 {
			t.Fatalf("request %d input = %#v", requestIndex+1, payload["input"])
		}
		system, ok := input[0].(map[string]any)
		if !ok || system["role"] != "system" || !strings.Contains(system["content"].(string), trustedPrompt) {
			t.Fatalf("request %d system = %#v", requestIndex+1, input[0])
		}
	}
	if !reflect.DeepEqual(received[1]["input"], received[2]["input"]) {
		t.Fatalf("retry reconstructed a different causal transcript:\nfirst=%#v\nretry=%#v", received[1]["input"], received[2]["input"])
	}
	assertCoreIntegrationReplay(t, received[2]["input"])
	assertCoreIntegrationSession(t, transcript)

	eventMu.Lock()
	recordedEvents := append([]agent.Event(nil), events...)
	eventMu.Unlock()
	assertCoreIntegrationLifecycle(t, recordedEvents)
	if err := coordinator.WaitForIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestContextSummarizerRequestDoesNotAdvertiseAgentTools(t *testing.T) {
	payloads := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode summary request: %v", err)
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		payloads <- payload
		writeContextSSE(t, w, "tool-free summary")
	}))
	defer server.Close()
	model, implementation := contextRetryProvider(t, server.URL)
	summarizer, err := provider.NewContextSummarizerWithRetry(
		implementation, model, func() time.Time { return agentTestEpoch }, provider.RetryPolicy{MaxAttempts: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	output, err := summarizer.Summarize(context.Background(), session.SummaryInput{
		SystemPrompt: "summary system", Prompt: "serialize transcript without tools",
	})
	if err != nil || output.Text != "tool-free summary" {
		t.Fatalf("Summarize() = %#v, %v", output, err)
	}
	payload := <-payloads
	if _, advertised := payload["tools"]; advertised {
		t.Fatalf("summarizer advertised Agent tools: %#v", payload["tools"])
	}
	if parallel, ok := payload["parallel_tool_calls"].(bool); !ok || parallel {
		t.Fatalf("summarizer parallel_tool_calls = %#v, want false", payload["parallel_tool_calls"])
	}
}

func coreIntegrationToolDefinitions(t *testing.T) []provider.ToolDefinition {
	t.Helper()
	schema := []byte(`{"type":"object","additionalProperties":false,"properties":{"value":{"type":"string"}},"required":["value"]}`)
	definitions := make([]provider.ToolDefinition, 0, 2)
	for _, name := range []string{"slow", "fast"} {
		definition, err := provider.NewToolDefinition(name, "Core integration fixture tool.", false, schema)
		if err != nil {
			t.Fatal(err)
		}
		definitions = append(definitions, definition)
	}
	return definitions
}

func receiveCoreToolSettlement(t *testing.T, settled <-chan string) string {
	t.Helper()
	select {
	case callID := <-settled:
		return callID
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not settle")
		return ""
	}
}

func assertCoreIntegrationReplay(t *testing.T, raw any) {
	t.Helper()
	input, ok := raw.([]any)
	if !ok || len(input) != 8 {
		t.Fatalf("integrated replay input = %#v", raw)
	}
	reasoning := input[2].(map[string]any)
	commentary := input[3].(map[string]any)
	if reasoning["type"] != "reasoning" || reasoning["id"] != "rs_integrated" || reasoning["encrypted_content"] != "integrated-cipher" {
		t.Fatalf("reasoning replay = %#v", reasoning)
	}
	if commentary["type"] != "message" || commentary["id"] != "msg_integrated" || commentary["phase"] != "commentary" {
		t.Fatalf("text replay = %#v", commentary)
	}
	want := []struct {
		index            int
		typeName, callID string
		itemID, output   string
	}{
		{index: 4, typeName: "function_call", callID: "call_slow", itemID: "fc_slow"},
		{index: 5, typeName: "function_call", callID: "call_fast", itemID: "fc_fast"},
		{index: 6, typeName: "function_call_output", callID: "call_slow", output: "slow"},
		{index: 7, typeName: "function_call_output", callID: "call_fast", output: "fast"},
	}
	for _, expected := range want {
		item, ok := input[expected.index].(map[string]any)
		if !ok || item["type"] != expected.typeName || item["call_id"] != expected.callID ||
			(expected.itemID != "" && item["id"] != expected.itemID) ||
			(expected.output != "" && item["output"] != expected.output) {
			t.Fatalf("replay item %d = %#v", expected.index, input[expected.index])
		}
	}
}

func assertCoreIntegrationSession(t *testing.T, transcript *session.Session) {
	t.Helper()
	entries := transcript.Entries()
	messages := transcript.BuildContext().Messages()
	if len(entries) != 5 || len(messages) != 5 {
		t.Fatalf("durable entries/messages = %d/%d, want 5/5", len(entries), len(messages))
	}
	wantRoles := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleToolResult, llm.RoleToolResult, llm.RoleAssistant}
	for index, want := range wantRoles {
		if messages[index].Role() != want {
			t.Fatalf("message %d role = %s, want %s", index, messages[index].Role(), want)
		}
	}
	assistant, ok := messages[1].(llm.AssistantToolUseMessage)
	if !ok {
		t.Fatalf("tool assistant = %T", messages[1])
	}
	blocks := assistant.Blocks()
	if len(blocks) != 4 {
		t.Fatalf("tool assistant blocks = %#v", blocks)
	}
	reasoning, ok := blocks[0].(llm.ThinkingBlock).ThinkingSignature()
	if !ok || reasoning != `{"type":"reasoning","id":"rs_integrated","encrypted_content":"integrated-cipher"}` {
		t.Fatalf("durable reasoning signature = (%q, %t)", reasoning, ok)
	}
	textSignature, ok := blocks[1].(llm.TextBlock).TextSignature()
	if !ok || textSignature != `{"v":1,"id":"msg_integrated","phase":"commentary"}` {
		t.Fatalf("durable text signature = (%q, %t)", textSignature, ok)
	}
	firstResult := messages[2].(llm.ToolResultMessage)
	secondResult := messages[3].(llm.ToolResultMessage)
	if firstResult.ToolCallID() != "call_slow|fc_slow" || joinContextText(firstResult.Content()) != "slow" ||
		secondResult.ToolCallID() != "call_fast|fc_fast" || joinContextText(secondResult.Content()) != "fast" {
		t.Fatalf("durable tool results = %#v / %#v", firstResult, secondResult)
	}
}

func assertCoreIntegrationLifecycle(t *testing.T, events []agent.Event) {
	t.Helper()
	var retries, settled []agent.Event
	for _, event := range events {
		switch event.Kind {
		case agent.EventRetryScheduled, agent.EventRetryAttempt, agent.EventRetryFinished:
			retries = append(retries, event)
		case agent.EventRunSettled:
			settled = append(settled, event)
		}
	}
	if len(retries) != 3 || retries[0].Kind != agent.EventRetryScheduled || retries[1].Kind != agent.EventRetryAttempt || retries[2].Kind != agent.EventRetryFinished {
		t.Fatalf("retry lifecycle = %+v", retries)
	}
	if retries[0].Turn != 2 || retries[0].RetryAttempt != 2 || retries[0].RetryFailureKind != provider.FailureTransport ||
		retries[1].RetryAttempt != 2 || retries[2].RetryAttempt != 2 || !retries[2].RetrySucceeded ||
		retries[2].RetryFinishReason != provider.RetryFinishSucceeded {
		t.Fatalf("retry reason/settlement = %+v", retries)
	}
	if len(settled) != 1 || settled[0].RunError != nil || settled[0].Terminal == nil || settled[0].Terminal.FinishReason() != llm.FinishStop {
		t.Fatalf("run settlement = %+v", settled)
	}
}

func writeCoreIntegrationToolSSE(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	reasoning := map[string]any{"type": "reasoning", "id": "rs_integrated", "encrypted_content": "integrated-cipher"}
	commentary := map[string]any{"type": "message", "id": "msg_integrated", "role": "assistant", "phase": "commentary", "content": []any{map[string]any{"type": "output_text", "text": "running both tools"}}}
	slow := map[string]any{"type": "function_call", "id": "fc_slow", "call_id": "call_slow", "name": "slow", "arguments": `{"value":"slow"}`}
	fast := map[string]any{"type": "function_call", "id": "fc_fast", "call_id": "call_fast", "name": "fast", "arguments": `{"value":"fast"}`}
	events := []map[string]any{
		{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "rs_integrated"}},
		{"type": "response.reasoning_summary_text.delta", "output_index": 0, "item_id": "rs_integrated", "delta": "parallel plan"},
		{"type": "response.output_item.done", "output_index": 0, "item": reasoning},
		{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"type": "message", "id": "msg_integrated", "role": "assistant", "phase": "commentary", "content": []any{}}},
		{"type": "response.output_text.delta", "output_index": 1, "item_id": "msg_integrated", "delta": "running both tools"},
		{"type": "response.output_item.done", "output_index": 1, "item": commentary},
	}
	events = append(events, coreIntegrationFunctionCallEvents(2, slow)...)
	events = append(events, coreIntegrationFunctionCallEvents(3, fast)...)
	events = append(events, map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"status": "completed", "output": []any{reasoning, commentary, slow, fast},
			"usage": map[string]any{"input_tokens": 4, "output_tokens": 4, "total_tokens": 8},
		},
	})
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			t.Errorf("encode integrated SSE: %v", err)
			return
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			t.Errorf("write integrated SSE: %v", err)
			return
		}
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		t.Errorf("write integrated SSE terminal: %v", err)
	}
}

func coreIntegrationFunctionCallEvents(index int, call map[string]any) []map[string]any {
	itemID := call["id"].(string)
	arguments := call["arguments"].(string)
	return []map[string]any{
		{"type": "response.output_item.added", "output_index": index, "item": map[string]any{
			"type": "function_call", "id": itemID, "call_id": call["call_id"], "name": call["name"], "arguments": "",
		}},
		{"type": "response.function_call_arguments.delta", "output_index": index, "item_id": itemID, "delta": arguments},
		{"type": "response.function_call_arguments.done", "output_index": index, "item_id": itemID, "arguments": arguments},
		{"type": "response.output_item.done", "output_index": index, "item": call},
	}
}

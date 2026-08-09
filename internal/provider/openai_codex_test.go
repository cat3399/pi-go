package provider_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	coderwebsocket "github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestOpenAICodexWebSocketStreamsAndReusesCachedContinuation(t *testing.T) {
	provider.ResetOpenAICodexWebSocketDebugStats("")
	defer provider.CloseOpenAICodexWebSocketSessions("")

	var connections atomic.Int32
	var framesMu sync.Mutex
	var frames []map[string]any
	token := codexTestToken(t, "acct-test")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/codex/responses" || !strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
			t.Errorf("request = %s %s upgrade=%q", request.Method, request.URL.Path, request.Header.Get("Upgrade"))
			http.Error(writer, "websocket required", http.StatusBadRequest)
			return
		}
		connections.Add(1)
		if got := request.Header.Get("OpenAI-Beta"); got != "responses_websockets=2026-02-06" {
			t.Errorf("OpenAI-Beta = %q", got)
		}
		if got := request.Header.Get("chatgpt-account-id"); got != "acct-test" {
			t.Errorf("chatgpt-account-id = %q", got)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q", got)
		}
		connection, err := coderwebsocket.Accept(writer, request, &coderwebsocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer connection.CloseNow()
		for turn := 1; turn <= 2; turn++ {
			var frame map[string]any
			if err := wsjson.Read(request.Context(), connection, &frame); err != nil {
				t.Errorf("read frame %d: %v", turn, err)
				return
			}
			framesMu.Lock()
			frames = append(frames, frame)
			framesMu.Unlock()
			text := "hello"
			if turn == 2 {
				text = "again"
			}
			writeCodexWebSocketText(t, request.Context(), connection, turn, text)
		}
	}))
	defer server.Close()

	model := mustCodexModel(t, server.URL)
	implementation, err := provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{
		BaseURL: server.URL, AccessToken: token, Clock: func() time.Time { return responsesTestTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	request := mustCodexRequest(t, model, "system", []llm.ConversationMessage{mustUser(t, "first")}, provider.TransportAuto, "session-1")
	events, first := collectStream(t, implementation.Stream(context.Background(), request))
	if got, want := eventKinds(events), []string{"start", "text_start", "text_delta", "text_end", "done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first events = %v, want %v", got, want)
	}
	if got := terminalText(t, first); got != "hello" {
		t.Fatalf("first text = %q", got)
	}

	secondRequest := mustCodexRequest(t, model, "system", []llm.ConversationMessage{mustUser(t, "first"), first, mustUser(t, "second")}, provider.TransportAuto, "session-1")
	events, second := collectStream(t, implementation.Stream(context.Background(), secondRequest))
	if got, want := eventKinds(events), []string{"start", "text_start", "text_delta", "text_end", "done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("second events = %v, want %v", got, want)
	}
	if got := terminalText(t, second); got != "again" {
		t.Fatalf("second text = %q", got)
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("connections = %d, want cached connection reuse", got)
	}

	framesMu.Lock()
	captured := append([]map[string]any(nil), frames...)
	framesMu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("frames = %#v", captured)
	}
	firstFrame := captured[0]
	if firstFrame["type"] != "response.create" || firstFrame["instructions"] != "system" || firstFrame["prompt_cache_key"] != "session-1" || firstFrame["parallel_tool_calls"] != true {
		t.Fatalf("first frame = %#v", firstFrame)
	}
	if _, exists := firstFrame["reasoning"]; exists {
		t.Fatalf("off reasoning must be omitted: %#v", firstFrame["reasoning"])
	}
	if _, exists := firstFrame["max_output_tokens"]; exists {
		t.Fatalf("Codex must omit max_output_tokens: %#v", firstFrame["max_output_tokens"])
	}
	secondFrame := captured[1]
	if secondFrame["previous_response_id"] != "resp-1" {
		t.Fatalf("previous_response_id = %#v; frame=%#v", secondFrame["previous_response_id"], secondFrame)
	}
	if input, ok := secondFrame["input"].([]any); !ok || len(input) != 1 {
		t.Fatalf("cached delta input = %#v", secondFrame["input"])
	}
	stats, ok := provider.GetOpenAICodexWebSocketDebugStats("session-1")
	if !ok || stats.ConnectionsCreated != 1 || stats.ConnectionsReused != 1 || stats.DeltaRequests != 1 || stats.LastPreviousResponseID != "resp-1" {
		t.Fatalf("debug stats = %#v/%t", stats, ok)
	}
}

func TestOpenAICodexCachedContinuationUsesNormalizedOutputItems(t *testing.T) {
	provider.ResetOpenAICodexWebSocketDebugStats("")
	defer provider.CloseOpenAICodexWebSocketSessions("")

	var framesMu sync.Mutex
	var frames []map[string]any
	token := codexTestToken(t, "acct-custom")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := coderwebsocket.Accept(writer, request, &coderwebsocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer connection.CloseNow()
		for turn := 1; turn <= 2; turn++ {
			var frame map[string]any
			if err := wsjson.Read(request.Context(), connection, &frame); err != nil {
				t.Errorf("read frame %d: %v", turn, err)
				return
			}
			framesMu.Lock()
			frames = append(frames, frame)
			framesMu.Unlock()
			responseID := fmt.Sprintf("resp-custom-%d", turn)
			if err := wsjson.Write(request.Context(), connection, map[string]any{"type": "response.created", "response": map[string]any{"id": responseID}}); err != nil {
				t.Errorf("write response.created: %v", err)
				return
			}
			if turn == 1 {
				for _, event := range []map[string]any{
					{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "custom_tool_call", "id": "ctc_1", "call_id": "call_1", "name": "sample_tool", "input": ""}},
					{"type": "response.custom_tool_call_input.delta", "output_index": 0, "item_id": "ctc_1", "delta": "abc"},
					{"type": "response.custom_tool_call_input.done", "output_index": 0, "item_id": "ctc_1", "input": "abc"},
					{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "custom_tool_call", "id": "ctc_1", "call_id": "call_1", "name": "sample_tool", "input": "abc", "status": "completed", "future": "ignored"}},
				} {
					if err := wsjson.Write(request.Context(), connection, event); err != nil {
						t.Errorf("write custom tool event: %v", err)
						return
					}
				}
			}
			// The legal terminal event intentionally omits response.output. Cached
			// continuation must be reconstructed from output_item.done events.
			if err := wsjson.Write(request.Context(), connection, map[string]any{
				"type": "response.completed", "response": map[string]any{
					"id": responseID, "status": "completed",
					"usage": map[string]any{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8},
				},
			}); err != nil {
				t.Errorf("write response.completed: %v", err)
				return
			}
		}
	}))
	defer server.Close()

	tool, err := provider.NewToolDefinitionWithConstrainedSampling(
		"sample_tool", "Sample tool",
		[]byte(`{"type":"object","properties":{"payload":{"type":"string"}},"required":["payload"],"additionalProperties":false}`),
		&provider.ConstrainedSampling{Kind: provider.ConstrainedSamplingGrammar, Variants: provider.GrammarVariants{OpenAILark: "start: /[a-z]+/"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	model := mustCodexModel(t, server.URL)
	implementation, err := provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{BaseURL: server.URL, AccessToken: token})
	if err != nil {
		t.Fatal(err)
	}
	newRequest := func(messages []llm.ConversationMessage) provider.Request {
		request, err := provider.NewRequestWithOptions(model, "system", messages, provider.RequestOptions{
			ThinkingLevel: provider.ThinkingOff,
			Tools:         []provider.ToolDefinition{tool},
			Stream:        provider.StreamOptions{Transport: provider.TransportWebsocketCached, SessionID: "custom-session"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return request
	}
	firstUser := mustUser(t, "Use the tool")
	_, first := collectStream(t, implementation.Stream(context.Background(), newRequest([]llm.ConversationMessage{firstUser})))
	toolUse, ok := first.(llm.AssistantToolUseMessage)
	if !ok || len(toolUse.Blocks()) != 1 {
		t.Fatalf("first terminal = %#v", first)
	}
	call := toolUse.Blocks()[0].(llm.ToolCallBlock)
	if call.ID() != "call_1|ctc_1" || string(call.ArgumentsJSON()) != `{"payload":"abc"}` {
		t.Fatalf("custom tool call = %#v", call)
	}
	result := mustToolResult(t, call.ID(), call.Name(), "real result")
	_, _ = collectStream(t, implementation.Stream(context.Background(), newRequest([]llm.ConversationMessage{
		firstUser, first, result, mustUser(t, "Now finish"),
	})))

	framesMu.Lock()
	captured := append([]map[string]any(nil), frames...)
	framesMu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("frames = %#v", captured)
	}
	second := captured[1]
	if second["previous_response_id"] != "resp-custom-1" {
		t.Fatalf("previous_response_id = %#v", second["previous_response_id"])
	}
	input, ok := second["input"].([]any)
	if !ok || len(input) != 2 {
		t.Fatalf("delta input = %#v", second["input"])
	}
	output := input[0].(map[string]any)
	if output["type"] != "custom_tool_call_output" || output["call_id"] != "call_1" || output["output"] != "real result" {
		t.Fatalf("custom output delta = %#v", output)
	}
	if user := input[1].(map[string]any); user["role"] != "user" {
		t.Fatalf("user delta = %#v", user)
	}
}

func TestOpenAICodexCachedDoesNotReuseParserRejectedTerminal(t *testing.T) {
	provider.ResetOpenAICodexWebSocketDebugStats("")
	defer provider.CloseOpenAICodexWebSocketSessions("")

	var connections atomic.Int32
	var framesMu sync.Mutex
	var frames []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := coderwebsocket.Accept(writer, request, &coderwebsocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer connection.CloseNow()
		attempt := connections.Add(1)
		var frame map[string]any
		if err := wsjson.Read(request.Context(), connection, &frame); err != nil {
			t.Errorf("read frame: %v", err)
			return
		}
		framesMu.Lock()
		frames = append(frames, frame)
		framesMu.Unlock()
		item := map[string]any{
			"type": "message", "id": fmt.Sprintf("msg-rejected-%d", attempt), "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": "answer"}},
		}
		if err := wsjson.Write(request.Context(), connection, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item}); err != nil {
			t.Errorf("write output item: %v", err)
			return
		}
		status := "completed"
		if attempt == 1 {
			status = "failed" // Invalid for a response.completed event.
		}
		if err := wsjson.Write(request.Context(), connection, map[string]any{
			"type": "response.completed", "response": map[string]any{
				"id": fmt.Sprintf("resp-rejected-%d", attempt), "status": status,
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		}); err != nil {
			t.Errorf("write terminal: %v", err)
		}
	}))
	defer server.Close()

	model := mustCodexModel(t, server.URL)
	implementation, err := provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{
		BaseURL: server.URL, AccessToken: codexTestToken(t, "acct-rejected"),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := mustCodexRequest(t, model, "", []llm.ConversationMessage{mustUser(t, "hi")}, provider.TransportWebsocketCached, "rejected-session")
	_, first := collectStream(t, implementation.Stream(context.Background(), request))
	if _, ok := first.(llm.AssistantFailureMessage); !ok {
		t.Fatalf("first terminal = %#v, want parser failure", first)
	}
	_, second := collectStream(t, implementation.Stream(context.Background(), request))
	if terminalText(t, second) != "answer" {
		t.Fatalf("second terminal = %#v", second)
	}

	framesMu.Lock()
	captured := append([]map[string]any(nil), frames...)
	framesMu.Unlock()
	if connections.Load() != 2 || len(captured) != 2 {
		t.Fatalf("connections/frames = %d/%d, want a fresh connection", connections.Load(), len(captured))
	}
	if _, cached := captured[1]["previous_response_id"]; cached {
		t.Fatalf("parser-rejected response became a cached continuation: %#v", captured[1])
	}
}

func TestOpenAICodexAutoFallsBackBeforeStartAndPinsSessionToSSE(t *testing.T) {
	provider.ResetOpenAICodexWebSocketDebugStats("")
	defer provider.CloseOpenAICodexWebSocketSessions("")

	var websocketAttempts atomic.Int32
	var sseRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
			websocketAttempts.Add(1)
			http.Error(writer, "websocket unavailable", http.StatusServiceUnavailable)
			return
		}
		sseRequests.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		writeResponsesSSE(t, writer, map[string]any{
			"type": "response.completed", "response": map[string]any{
				"id": "resp-sse", "status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 0, "total_tokens": 1},
			},
		})
	}))
	defer server.Close()

	model := mustCodexModel(t, server.URL)
	implementation, err := provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{
		BaseURL: server.URL, AccessToken: codexTestToken(t, "acct-test"), AccountID: "acct-test", Clock: func() time.Time { return responsesTestTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	request := mustCodexRequest(t, model, "", []llm.ConversationMessage{mustUser(t, "hi")}, provider.TransportAuto, "fallback-session")
	_, first := collectStream(t, implementation.Stream(context.Background(), request))
	if diagnostics := first.Diagnostics(); len(diagnostics) != 1 || diagnostics[0].Type() != "provider_transport_failure" {
		t.Fatalf("fallback diagnostics = %#v", diagnostics)
	}
	_, second := collectStream(t, implementation.Stream(context.Background(), request))
	if len(second.Diagnostics()) != 0 {
		t.Fatalf("pinned SSE request should not invent a new diagnostic: %#v", second.Diagnostics())
	}
	if websocketAttempts.Load() != 1 || sseRequests.Load() != 2 {
		t.Fatalf("attempts websocket=%d sse=%d", websocketAttempts.Load(), sseRequests.Load())
	}
	stats, ok := provider.GetOpenAICodexWebSocketDebugStats("fallback-session")
	if !ok || stats.WebSocketFailures != 1 || stats.SSEFallbacks != 1 || !stats.WebSocketFallbackActive {
		t.Fatalf("fallback stats = %#v/%t", stats, ok)
	}
}

func TestOpenAICodexWebSocketProviderErrorDoesNotFallBack(t *testing.T) {
	provider.ResetOpenAICodexWebSocketDebugStats("")
	defer provider.CloseOpenAICodexWebSocketSessions("")
	var sseRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
			sseRequests.Add(1)
			http.Error(writer, "unexpected SSE", http.StatusInternalServerError)
			return
		}
		connection, err := coderwebsocket.Accept(writer, request, &coderwebsocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer connection.CloseNow()
		var frame map[string]any
		if err := wsjson.Read(request.Context(), connection, &frame); err != nil {
			t.Errorf("read frame: %v", err)
			return
		}
		_ = wsjson.Write(request.Context(), connection, map[string]any{
			"type": "error", "error": map[string]any{"code": "invalid_request", "message": "bad request"},
		})
	}))
	defer server.Close()

	model := mustCodexModel(t, server.URL)
	implementation, err := provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{
		BaseURL: server.URL, AccessToken: codexTestToken(t, "acct-test"), AccountID: "acct-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := mustCodexRequest(t, model, "", []llm.ConversationMessage{mustUser(t, "hi")}, provider.TransportWebsocket, "")
	events, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	if got := eventKinds(events); !reflect.DeepEqual(got, []string{"error"}) {
		t.Fatalf("events = %v", got)
	}
	failure, ok := terminal.(llm.AssistantFailureMessage)
	if !ok || failure.ErrorMessage() != "bad request" {
		t.Fatalf("terminal = %T/%q", terminal, terminalTextIfFailure(terminal))
	}
	var providerFailure *provider.ProviderFailure
	if !errors.As(failure.Failure().Cause(), &providerFailure) || providerFailure.Kind() != provider.FailureInvalidResponse {
		t.Fatalf("failure cause = %#v", failure.Failure().Cause())
	}
	if code, ok := providerFailure.VendorCode(); !ok || code != "invalid_request" {
		t.Fatalf("vendor code = %q/%t", code, ok)
	}
	if sseRequests.Load() != 0 {
		t.Fatalf("provider API error fell back to SSE")
	}
}

func TestOpenAICodexExplicitSSEStopsAtTerminalWhileBodyStaysOpen(t *testing.T) {
	for _, terminalType := range []string{"response.completed", "response.incomplete"} {
		t.Run(terminalType, func(t *testing.T) {
			status := "completed"
			if terminalType == "response.incomplete" {
				status = "incomplete"
			}
			item := map[string]any{
				"type": "message", "id": "msg-sse", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "answer"}},
			}
			body := newResponsesBlockingBody(responsesSSE(
				map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
				map[string]any{"type": terminalType, "response": map[string]any{
					"id": "resp-sse", "status": status,
					"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
				}},
			))
			client := responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
				body.ctx = request.Context()
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       body,
				}, nil
			})
			implementation, err := provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{
				BaseURL: "https://fixture.test", AccessToken: codexTestToken(t, "acct-test"), Client: client,
			})
			if err != nil {
				t.Fatal(err)
			}
			stream := implementation.Stream(context.Background(), mustCodexRequest(
				t, mustCodexModel(t, "https://fixture.test"), "", []llm.ConversationMessage{mustUser(t, "hi")}, provider.TransportSSE, "",
			))
			type collected struct {
				events   []llm.StreamEvent
				terminal llm.AssistantTerminal
				err      error
			}
			finished := make(chan collected, 1)
			go func() {
				events, terminal, err := collectStreamResult(stream)
				finished <- collected{events: events, terminal: terminal, err: err}
			}()
			select {
			case result := <-finished:
				if result.err != nil {
					t.Fatal(result.err)
				}
				if got := eventKinds(result.events); !reflect.DeepEqual(got, []string{"start", "text_start", "text_delta", "text_end", "done"}) {
					t.Fatalf("events = %v", got)
				}
				wantReason := llm.FinishStop
				if terminalType == "response.incomplete" {
					wantReason = llm.FinishLength
				}
				if terminalText(t, result.terminal) != "answer" || result.terminal.FinishReason() != wantReason {
					t.Fatalf("terminal = %#v", result.terminal)
				}
			case <-time.After(time.Second):
				_ = stream.Close()
				t.Fatal("Codex SSE waited for physical EOF after terminal response event")
			}
			if body.closes.Load() != 1 {
				t.Fatalf("body closes = %d, want 1", body.closes.Load())
			}
		})
	}
}

func TestOpenAICodexExplicitTransportModesMatchPiFallbackContract(t *testing.T) {
	provider.ResetOpenAICodexWebSocketDebugStats("")
	defer provider.CloseOpenAICodexWebSocketSessions("")
	var websocketRequests atomic.Int32
	var sseRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
			websocketRequests.Add(1)
			http.Error(writer, "websocket unavailable", http.StatusServiceUnavailable)
			return
		}
		sseRequests.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		writeResponsesSSE(t, writer, map[string]any{
			"type": "response.completed", "response": map[string]any{
				"id": "resp-sse", "status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 0, "total_tokens": 1},
			},
		})
	}))
	defer server.Close()

	model := mustCodexModel(t, server.URL)
	implementation, err := provider.NewOpenAICodexResponsesProvider(provider.OpenAICodexResponsesConfig{
		BaseURL: server.URL, AccessToken: codexTestToken(t, "acct-test"), AccountID: "acct-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := mustCodexRequest(t, model, "", []llm.ConversationMessage{mustUser(t, "hi")}, provider.TransportSSE, "")
	_, direct := collectStream(t, implementation.Stream(context.Background(), request))
	if len(direct.Diagnostics()) != 0 || websocketRequests.Load() != 0 || sseRequests.Load() != 1 {
		t.Fatalf("explicit SSE diagnostics/requests = %#v / websocket=%d sse=%d", direct.Diagnostics(), websocketRequests.Load(), sseRequests.Load())
	}

	request = mustCodexRequest(t, model, "", []llm.ConversationMessage{mustUser(t, "hi")}, provider.TransportWebsocket, "")
	_, fallback := collectStream(t, implementation.Stream(context.Background(), request))
	if diagnostics := fallback.Diagnostics(); len(diagnostics) != 1 || diagnostics[0].Type() != "provider_transport_failure" {
		t.Fatalf("explicit websocket fallback diagnostics = %#v", diagnostics)
	}
	if websocketRequests.Load() != 1 || sseRequests.Load() != 2 {
		t.Fatalf("explicit transport requests websocket=%d sse=%d", websocketRequests.Load(), sseRequests.Load())
	}
}

func mustCodexModel(t *testing.T, baseURL string) provider.Model {
	t.Helper()
	strict, grammar := true, true
	model, err := newModel(provider.ModelSpec{
		Provider: provider.OpenAICodexProviderID, API: provider.OpenAICodexResponsesAPI, ID: "gpt-codex-test", Name: "Codex Test",
		BaseURL: baseURL, Reasoning: true, Input: []provider.InputKind{provider.InputText}, ContextWindow: 128_000, MaxTokens: 16_000,
		Compat: provider.ModelCompat{OpenAIResponses: &provider.OpenAIResponsesCompat{SupportsStrictMode: &strict, SupportsOpenAIGrammarTools: &grammar}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func mustCodexRequest(t *testing.T, model provider.Model, system string, messages []llm.ConversationMessage, transport provider.Transport, sessionID string) provider.Request {
	t.Helper()
	request, err := provider.NewRequestWithOptions(model, system, messages, provider.RequestOptions{
		ThinkingLevel: provider.ThinkingOff,
		Stream:        provider.StreamOptions{Transport: transport, SessionID: sessionID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func codexTestToken(t *testing.T, accountID string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, err := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID}})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func writeCodexWebSocketText(t *testing.T, ctx context.Context, connection *coderwebsocket.Conn, turn int, text string) {
	t.Helper()
	messageID := fmt.Sprintf("msg-%d", turn)
	responseID := fmt.Sprintf("resp-%d", turn)
	item := map[string]any{
		"type": "message", "id": messageID, "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text}},
	}
	events := []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": responseID}},
		{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": messageID, "role": "assistant", "content": []any{}}},
		{"type": "response.output_text.delta", "output_index": 0, "item_id": messageID, "delta": text},
		{"type": "response.output_text.done", "output_index": 0, "item_id": messageID, "text": text},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": map[string]any{
			"id": responseID, "status": "completed", "output": []any{item},
			"usage": map[string]any{"input_tokens": 2, "output_tokens": 1, "total_tokens": 3},
		}},
	}
	for _, event := range events {
		if err := wsjson.Write(ctx, connection, event); err != nil {
			t.Errorf("write event %q: %v", event["type"], err)
			return
		}
	}
}

func terminalTextIfFailure(terminal llm.AssistantTerminal) string {
	if failed, ok := terminal.(llm.AssistantFailureMessage); ok {
		return failed.ErrorMessage()
	}
	return ""
}

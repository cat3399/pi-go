package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
)

var responsesTestTime = time.Date(2026, time.August, 1, 16, 0, 0, 0, time.UTC)

func TestOpenAIResponsesStreamsTextAndNormalizesRequestAndUsage(t *testing.T) {
	prior := mustTextTerminal(t, "prior answer")
	request := mustResponsesRequest(t, "system", []llm.ConversationMessage{
		mustUser(t, "hello"),
		prior,
		mustUser(t, "continue"),
	})
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		if incoming.Method != http.MethodPost || incoming.URL.Path != "/v1/responses" {
			t.Errorf("request = %s %s", incoming.Method, incoming.URL.Path)
		}
		if got := incoming.Header.Get("Authorization"); got != "Bearer test-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := incoming.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q", got)
		}
		if err := json.NewDecoder(incoming.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		writeResponsesSSE(t, writer,
			map[string]any{"type": "response.created", "response": map[string]any{"id": "resp-1"}},
			map[string]any{
				"type": "response.output_item.added", "output_index": 0,
				"item": map[string]any{"type": "message", "id": "msg-1", "role": "assistant", "content": []any{}},
			},
			map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": "msg-1", "delta": "hel"},
			map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": "msg-1", "delta": "lo"},
			map[string]any{
				"type": "response.output_item.done", "output_index": 0,
				"item": map[string]any{
					"type": "message", "id": "msg-1", "role": "assistant", "status": "completed",
					"content": []any{map[string]any{"type": "output_text", "text": "hello"}},
				},
			},
			map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"status": "completed",
					"output": []any{map[string]any{"type": "message"}},
					"usage": map[string]any{
						"input_tokens": 10, "output_tokens": 4, "total_tokens": 14,
						"input_tokens_details":  map[string]any{"cached_tokens": 2, "cache_write_tokens": 1},
						"output_tokens_details": map[string]any{"reasoning_tokens": 1},
					},
				},
			},
		)
	}))
	defer server.Close()

	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: server.URL + "/v1",
		APIKey:  "test-secret",
		Clock:   func() time.Time { return responsesTestTime },
	})
	events, terminal := collectStream(t, implementation.Stream(context.Background(), request))
	if got, want := eventKinds(events), []string{"start", "text_start", "text_delta", "text_delta", "text_end", "done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	message, ok := terminal.(llm.AssistantTextMessage)
	if !ok || terminalText(t, terminal) != "hello" || message.FinishReason() != llm.FinishStop {
		t.Fatalf("terminal = %T/%v", terminal, terminal.FinishReason())
	}
	usage := message.Usage()
	if usage.Input() != 7 || usage.Output() != 4 || usage.CacheRead() != 2 || usage.CacheWrite() != 1 || usage.TotalTokens() != 14 {
		t.Fatalf("usage = input %d output %d read %d write %d total %d", usage.Input(), usage.Output(), usage.CacheRead(), usage.CacheWrite(), usage.TotalTokens())
	}
	if reasoning, ok := usage.Reasoning(); !ok || reasoning != 1 {
		t.Fatalf("reasoning = (%d, %t)", reasoning, ok)
	}
	if message.Timestamp() != responsesTestTime {
		t.Fatalf("timestamp = %v", message.Timestamp())
	}
	assertResponsesRequestPayload(t, received)
}

func TestOpenAIResponsesIncompleteCanFinalizeFromOutputItemDone(t *testing.T) {
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1",
		APIKey:  "secret",
		Client: staticResponsesDoer(responsesHTTPResponse(http.StatusOK, "text/event-stream", responsesSSE(
			map[string]any{
				"type": "response.output_item.done", "output_index": 0,
				"item": map[string]any{
					"type": "message", "id": "msg-final", "role": "assistant", "status": "completed",
					"content": []any{map[string]any{"type": "output_text", "text": "short"}},
				},
			},
			map[string]any{
				"type":     "response.incomplete",
				"response": map[string]any{"status": "incomplete", "usage": map[string]any{"input_tokens": 2, "output_tokens": 3, "total_tokens": 5}},
			},
		))),
		Clock: func() time.Time { return responsesTestTime },
	})
	events, terminal := collectStream(t, implementation.Stream(context.Background(), mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")})))
	if got, want := eventKinds(events), []string{"start", "text_start", "text_delta", "text_end", "done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	message, ok := terminal.(llm.AssistantTextMessage)
	if !ok || terminalText(t, terminal) != "short" || message.FinishReason() != llm.FinishLength {
		t.Fatalf("terminal = %T/%v", terminal, terminal.FinishReason())
	}
}

func TestOpenAIResponsesSettlesTerminalOnlyAtDoneOrEOF(t *testing.T) {
	completed := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"status": "completed",
			"usage":  map[string]any{"input_tokens": 2, "output_tokens": 1, "total_tokens": 3},
		},
	}
	for _, testCase := range []struct {
		name string
		body string
	}{
		{name: "physical EOF", body: responsesSSE(completed)},
		{name: "DONE sentinel", body: responsesSSE(completed) + "data: [DONE]\n\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
				BaseURL: "https://fixture.test/v1",
				APIKey:  "secret",
				Client: staticResponsesDoer(responsesHTTPResponse(
					http.StatusOK,
					"text/event-stream",
					testCase.body,
				)),
			})
			events, terminal := collectStream(t, implementation.Stream(
				context.Background(),
				mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")}),
			))
			if got := eventKinds(events); !reflect.DeepEqual(got, []string{"start", "done"}) {
				t.Fatalf("events = %v", got)
			}
			message, ok := terminal.(llm.AssistantTextMessage)
			if !ok || message.Usage().TotalTokens() != 3 {
				t.Fatalf("terminal = %T, usage = %d", terminal, terminal.Usage().TotalTokens())
			}
		})
	}

	for _, testCase := range []struct {
		name string
		body string
	}{
		{name: "event after terminal", body: responsesSSE(completed, map[string]any{"type": "response.future_progress"})},
		{name: "second terminal", body: responsesSSE(completed, completed)},
		{name: "malformed event after terminal", body: responsesSSE(completed) + "data: {\n\n"},
		{name: "partial malformed line after terminal", body: responsesSSE(completed) + "data: {"},
		{name: "undelimited malformed line after terminal", body: responsesSSE(completed) + "data: {\n"},
		{name: "unterminated terminal frame", body: strings.TrimSuffix(responsesSSE(completed), "\n\n")},
		{name: "terminal frame lacks blank delimiter", body: strings.TrimSuffix(responsesSSE(completed), "\n")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
				BaseURL: "https://fixture.test/v1",
				APIKey:  "secret",
				Client: staticResponsesDoer(responsesHTTPResponse(
					http.StatusOK,
					"text/event-stream",
					testCase.body,
				)),
			})
			_, terminal := collectStream(t, implementation.Stream(
				context.Background(),
				mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")}),
			))
			failure := terminalFailure(t, terminal)
			assertProviderFailure(t, failure, provider.FailureInvalidResponse, provider.ErrOpenAIResponsesStream)
			if strings.Contains(testCase.name, "after terminal") || testCase.name == "second terminal" {
				if failure.Usage().TotalTokens() != 3 {
					t.Fatalf("terminal usage = %d, want 3", failure.Usage().TotalTokens())
				}
			}
		})
	}
}

func TestOpenAIResponsesStreamsSequentialTextAndRefusalItems(t *testing.T) {
	first := map[string]any{
		"type": "message", "id": "msg-0", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": "answer"}},
	}
	second := map[string]any{
		"type": "message", "id": "msg-1", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "refusal", "refusal": "declined"}},
	}
	body := responsesSSE(
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": first},
		map[string]any{"type": "response.output_item.done", "output_index": 1, "item": second},
		map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"status": "completed", "output": []any{first, second}},
		},
	)
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "secret",
		Client: staticResponsesDoer(responsesHTTPResponse(http.StatusOK, "text/event-stream", body)),
	})
	events, terminal := collectStream(t, implementation.Stream(
		context.Background(),
		mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")}),
	))
	if got, want := eventKinds(events), []string{
		"start", "text_start", "text_delta", "text_end", "text_start", "text_delta", "text_end", "done",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	message, ok := terminal.(llm.AssistantTextMessage)
	if !ok || len(message.Content()) != 2 || message.Content()[0].Text() != "answer" || message.Content()[1].Text() != "declined" {
		t.Fatalf("terminal = %#v", terminal)
	}
}

func TestOpenAIResponsesPreservesHTTPFailureMetadata(t *testing.T) {
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1",
		APIKey:  "secret",
		Client: staticResponsesDoer(responsesHTTPResponse(
			http.StatusTooManyRequests,
			"application/json",
			`{"error":{"message":"slow down","code":"rate_limit_exceeded"}}`,
		)),
	})
	events, terminal := collectStream(t, implementation.Stream(context.Background(), mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")})))
	if got := eventKinds(events); !reflect.DeepEqual(got, []string{"error"}) {
		t.Fatalf("events = %v", got)
	}
	failure := terminalFailure(t, terminal)
	assertProviderFailure(t, failure, provider.FailureHTTPStatus, nil)
	var providerFailure *provider.ProviderFailure
	if !errors.As(failure.Failure(), &providerFailure) {
		t.Fatal("missing ProviderFailure")
	}
	if status, ok := providerFailure.HTTPStatus(); !ok || status != 429 {
		t.Fatalf("HTTP status = (%d, %t)", status, ok)
	}
	if code, ok := providerFailure.VendorCode(); !ok || code != "rate_limit_exceeded" {
		t.Fatalf("vendor code = (%q, %t)", code, ok)
	}
	var httpFailure *provider.OpenAIResponsesHTTPError
	if !errors.As(failure.Failure(), &httpFailure) || httpFailure.Status() != 429 || httpFailure.VendorCode() != "rate_limit_exceeded" {
		t.Fatalf("HTTP cause = %T/%v", failure.Failure().Cause(), failure.Failure().Cause())
	}
}

func TestOpenAIResponsesRejectsMalformedAndUnsupportedStreams(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		maxEvent    int
		wantCause   error
	}{
		{name: "wrong content type", contentType: "application/json", body: `{}`},
		{name: "malformed JSON", contentType: "text/event-stream", body: "data: {\n\n"},
		{name: "early EOF", contentType: "text/event-stream", body: responsesSSE(map[string]any{"type": "response.created"})},
		{name: "DONE before terminal", contentType: "text/event-stream", body: "data: [DONE]\n\n"},
		{name: "delta without item", contentType: "text/event-stream", body: responsesSSE(map[string]any{"type": "response.output_text.delta", "output_index": 0, "delta": "x"})},
		{
			name: "final text mismatch", contentType: "text/event-stream",
			body: responsesSSE(
				map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg", "role": "assistant", "content": []any{}}},
				map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": "msg", "delta": "a"},
				map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "b"}}}},
			),
		},
		{
			name: "terminal carries unstreamed text", contentType: "text/event-stream",
			body: responsesSSE(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"status": "completed",
					"output": []any{map[string]any{
						"type": "message", "id": "msg", "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": "lost"}},
					}},
				},
			}),
		},
		{
			name: "terminal with open text item", contentType: "text/event-stream",
			body: responsesSSE(
				map[string]any{
					"type": "response.output_item.added", "output_index": 0,
					"item": map[string]any{"type": "message", "id": "msg", "role": "assistant", "content": []any{}},
				},
				map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}},
			),
		},
		{
			name: "completed response carries error", contentType: "text/event-stream",
			body: responsesSSE(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"status": "completed",
					"error":  map[string]any{"code": "bad", "message": "inconsistent"},
				},
			}),
		},
		{
			name: "terminal message has wrong role", contentType: "text/event-stream",
			body: responsesSSE(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"status": "completed",
					"output": []any{map[string]any{"type": "message", "role": "user"}},
				},
			}),
		},
		{
			name: "invalid usage total", contentType: "text/event-stream",
			body: responsesSSE(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "usage": map[string]any{"input_tokens": 2, "output_tokens": 3, "total_tokens": 99}}}),
		},
		{
			name: "cached usage exceeds input", contentType: "text/event-stream",
			body: responsesSSE(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "usage": map[string]any{"input_tokens": 2, "output_tokens": 0, "input_tokens_details": map[string]any{"cached_tokens": 3}}}}),
		},
		{
			name: "reasoning usage exceeds output", contentType: "text/event-stream",
			body: responsesSSE(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "usage": map[string]any{"input_tokens": 0, "output_tokens": 1, "output_tokens_details": map[string]any{"reasoning_tokens": 2}}}}),
		},
		{
			name: "negative usage", contentType: "text/event-stream",
			body: responsesSSE(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "usage": map[string]any{"input_tokens": -1}}}),
		},
		{
			name: "fractional usage", contentType: "text/event-stream",
			body: responsesSSE(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "usage": map[string]any{"input_tokens": 1.5}}}),
		},
		{
			name: "incomplete reasoning item is invalid", contentType: "text/event-stream",
			body: responsesSSE(
				map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "rs"}},
				map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "reasoning"}}}},
			),
		},
		{
			name: "malformed tool output is explicit", contentType: "text/event-stream",
			body: responsesSSE(
				map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc"}},
				map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}},
			),
		},
		{
			name: "orphan reasoning delta is invalid", contentType: "text/event-stream",
			body: responsesSSE(
				map[string]any{"type": "response.reasoning_text.delta", "output_index": 0, "delta": "hidden"},
				map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}},
			),
		},
		{
			name: "orphan tool delta is invalid", contentType: "text/event-stream",
			body: responsesSSE(
				map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": "{}"},
				map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}},
			),
		},
		{
			name: "unknown message phase", contentType: "text/event-stream",
			body: responsesSSE(map[string]any{
				"type": "response.output_item.added", "output_index": 0,
				"item": map[string]any{
					"type": "message", "id": "msg", "role": "assistant", "phase": "analysis", "content": []any{},
				},
			}),
		},
		{
			name: "message phase changes while open", contentType: "text/event-stream",
			body: responsesSSE(
				map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg", "role": "assistant", "phase": "commentary", "content": []any{}}},
				map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg", "role": "assistant", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "x"}}}},
			),
		},
		{
			name: "output follows final answer", contentType: "text/event-stream",
			body: responsesSSE(
				map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg_final", "role": "assistant", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "done"}}}},
				map[string]any{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"type": "message", "id": "msg_late", "role": "assistant", "phase": "commentary", "content": []any{}}},
			),
		},
		{
			name: "terminal message phase mismatch", contentType: "text/event-stream",
			body: responsesSSE(
				map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg", "role": "assistant", "phase": "commentary", "content": []any{map[string]any{"type": "output_text", "text": "working"}}}},
				map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "id": "msg", "role": "assistant", "phase": "final_answer"}}}},
			),
		},
		{name: "bounded SSE event", contentType: "text/event-stream", body: "data: " + strings.Repeat("x", 128) + "\n\n", maxEvent: 32},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
				BaseURL:       "https://fixture.test/v1",
				APIKey:        "secret",
				Client:        staticResponsesDoer(responsesHTTPResponse(http.StatusOK, testCase.contentType, testCase.body)),
				MaxEventBytes: testCase.maxEvent,
			})
			events, terminal, err := collectStreamResult(implementation.Stream(context.Background(), mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")})))
			if err != nil {
				t.Fatalf("collect stream: %v", err)
			}
			failure := terminalFailure(t, terminal)
			assertProviderFailure(t, failure, provider.FailureInvalidResponse, testCase.wantCause)
			if len(events) == 0 || eventKinds(events)[len(events)-1] != "error" {
				t.Fatalf("events = %v", eventKinds(events))
			}
		})
	}
}

func TestOpenAIResponsesPreservesPartialTextAndVendorError(t *testing.T) {
	body := responsesSSE(
		map[string]any{
			"type": "response.output_item.added", "output_index": 0,
			"item": map[string]any{"type": "message", "id": "msg", "role": "assistant", "content": []any{}},
		},
		map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": "msg", "delta": "partial"},
		map[string]any{
			"type":     "response.failed",
			"response": map[string]any{"status": "failed", "error": map[string]any{"code": "server_error", "message": "upstream failed"}},
		},
	)
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "secret",
		Client: staticResponsesDoer(responsesHTTPResponse(http.StatusOK, "text/event-stream", body)),
	})
	_, terminal := collectStream(t, implementation.Stream(
		context.Background(),
		mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")}),
	))
	failure := terminalFailure(t, terminal)
	if len(failure.Content()) != 1 || failure.Content()[0].Text() != "partial" {
		t.Fatalf("partial content = %#v", failure.Content())
	}
	assertProviderFailure(t, failure, provider.FailureInvalidResponse, nil)
	var providerFailure *provider.ProviderFailure
	if !errors.As(failure.Failure(), &providerFailure) {
		t.Fatal("missing ProviderFailure")
	}
	if code, ok := providerFailure.VendorCode(); !ok || code != "server_error" {
		t.Fatalf("vendor code = (%q, %t)", code, ok)
	}
	var apiFailure *provider.OpenAIResponsesAPIError
	if !errors.As(failure.Failure(), &apiFailure) || apiFailure.Code() != "server_error" || apiFailure.Message() != "upstream failed" {
		t.Fatalf("API cause = %T/%v", failure.Failure().Cause(), failure.Failure().Cause())
	}
}

func TestOpenAIResponsesValidatesConfigurationRoutingAndTextScopeBeforeTransport(t *testing.T) {
	for _, config := range []provider.OpenAIResponsesConfig{
		{BaseURL: "ftp://fixture.test/v1", APIKey: "key"},
		{BaseURL: "https://user@fixture.test/v1", APIKey: "key"},
		{BaseURL: "https://fixture.test/v1", APIKey: "key", MaxEventBytes: -1},
		{BaseURL: "https://fixture.test/v1", APIKey: "key", Client: (*typedNilResponsesDoer)(nil)},
		{BaseURL: "https://fixture.test/v1", APIKey: "key", SystemRole: provider.OpenAIResponsesSystemRole(99)},
	} {
		if _, err := provider.NewOpenAIResponsesProvider(config); !errors.Is(err, provider.ErrInvalidOpenAIResponsesConfig) {
			t.Fatalf("NewOpenAIResponsesProvider(%+v) error = %v", config, err)
		}
	}

	var calls atomic.Uint32
	doer := responsesDoerFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("must not be called")
	})
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "secret", Client: doer,
	})
	for _, apiKey := range []string{"", " ", "bad\nkey", "bad\x00key", string([]byte{0xff})} {
		misconfigured := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
			BaseURL: "https://fixture.test/v1", APIKey: apiKey, Client: doer,
		})
		_, terminal := collectStream(t, misconfigured.Stream(
			context.Background(),
			mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")}),
		))
		assertProviderFailure(t, terminalFailure(t, terminal), provider.FailureConfiguration, provider.ErrInvalidOpenAIResponsesConfig)
	}
	wrongModel, err := provider.NewModelRef("openai", "openai-completions", "model")
	if err != nil {
		t.Fatal(err)
	}
	wrongRequest, err := provider.NewRequest(wrongModel, "", []llm.ConversationMessage{mustUser(t, "hi")})
	if err != nil {
		t.Fatal(err)
	}
	_, wrongTerminal := collectStream(t, implementation.Stream(context.Background(), wrongRequest))
	assertProviderFailure(t, terminalFailure(t, wrongTerminal), provider.FailureConfiguration, provider.ErrOpenAIResponsesRequest)

	if calls.Load() != 0 {
		t.Fatalf("transport calls = %d, want 0", calls.Load())
	}
}

func TestOpenAIResponsesUsesStandardEndpointByDefault(t *testing.T) {
	var endpoint string
	doer := responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
		endpoint = request.URL.String()
		return responsesHTTPResponse(http.StatusOK, "text/event-stream", responsesSSE(
			map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}},
		)), nil
	})
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{APIKey: "secret", Client: doer})
	_, terminal := collectStream(t, implementation.Stream(
		context.Background(),
		mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")}),
	))
	if _, ok := terminal.(llm.AssistantTextMessage); !ok {
		t.Fatalf("terminal = %T", terminal)
	}
	if endpoint != "https://api.openai.com/v1/responses" {
		t.Fatalf("endpoint = %q", endpoint)
	}
}

func TestOpenAIResponsesUsesExplicitDeveloperRoleAndStableReplayIDs(t *testing.T) {
	emptyUser, err := llm.NewUserTextBlocksMessage(nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	prior, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{mustTextBlock(t, "one"), mustTextBlock(t, "two")},
		llm.FinishStop,
		llm.Usage{},
		time.Time{},
	)
	if err != nil {
		t.Fatal(err)
	}
	var received map[string]any
	doer := responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		return responsesHTTPResponse(http.StatusOK, "text/event-stream", responsesSSE(
			map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}},
		)), nil
	})
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "secret", Client: doer,
		SystemRole: provider.OpenAIResponsesSystemRoleDeveloper,
	})
	_, terminal := collectStream(t, implementation.Stream(
		context.Background(),
		mustResponsesRequest(t, "instructions", []llm.ConversationMessage{emptyUser, prior}),
	))
	if _, ok := terminal.(llm.AssistantTextMessage); !ok {
		t.Fatalf("terminal = %T", terminal)
	}
	input := received["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input = %#v", input)
	}
	if role := input[0].(map[string]any)["role"]; role != "developer" {
		t.Fatalf("system role = %v", role)
	}
	for index, wantID := range []string{"msg_pi_0", "msg_pi_0_1"} {
		item := input[index+1].(map[string]any)
		if item["id"] != wantID || item["type"] != "message" || item["status"] != "completed" {
			t.Fatalf("replay item %d = %#v", index, item)
		}
	}
}

func TestOpenAIResponsesSkipsFailedAssistantHistory(t *testing.T) {
	newFailure := func(finish llm.FinishReason, partial string) llm.AssistantFailureMessage {
		t.Helper()
		var content []llm.TextBlock
		if partial != "" {
			content = []llm.TextBlock{mustTextBlock(t, partial)}
		}
		message, err := llm.NewAssistantFailureMessage(content, finish, "failed", llm.Usage{}, time.Time{})
		if err != nil {
			t.Fatalf("NewAssistantFailureMessage() error = %v", err)
		}
		return message
	}

	var received map[string]any
	doer := responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		return responsesHTTPResponse(http.StatusOK, "text/event-stream", responsesSSE(
			map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}},
		)), nil
	})
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "secret", Client: doer,
	})
	messages := []llm.ConversationMessage{
		mustTextTerminal(t, "before"),
		newFailure(llm.FinishError, "error partial"),
		newFailure(llm.FinishError, ""),
		mustUser(t, "after failures"),
		newFailure(llm.FinishAborted, "aborted partial"),
		newFailure(llm.FinishAborted, ""),
		mustTextTerminal(t, "after"),
	}
	_, terminal := collectStream(t, implementation.Stream(
		context.Background(),
		mustResponsesRequest(t, "", messages),
	))
	if _, ok := terminal.(llm.AssistantTextMessage); !ok {
		t.Fatalf("terminal = %T", terminal)
	}

	input := received["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input = %#v", input)
	}
	if got := input[0].(map[string]any)["id"]; got != "msg_pi_0" {
		t.Fatalf("first replay id = %v, want msg_pi_0", got)
	}
	if got := input[1].(map[string]any)["role"]; got != "user" {
		t.Fatalf("middle input role = %v, want user", got)
	}
	if got := input[2].(map[string]any)["id"]; got != "msg_pi_2" {
		t.Fatalf("replay id after failures = %v, want msg_pi_2", got)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, partial := range []string{"error partial", "aborted partial"} {
		if bytes.Contains(encoded, []byte(partial)) {
			t.Fatalf("failed assistant partial %q leaked into request: %s", partial, encoded)
		}
	}
}

func TestOpenAIResponsesMapsTransportErrorAndPanic(t *testing.T) {
	for name, doer := range map[string]provider.HTTPDoer{
		"error": responsesDoerFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") }),
		"panic": responsesDoerFunc(func(*http.Request) (*http.Response, error) { panic("boom") }),
	} {
		t.Run(name, func(t *testing.T) {
			implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
				BaseURL: "https://fixture.test/v1", APIKey: "secret", Client: doer,
			})
			_, terminal := collectStream(t, implementation.Stream(context.Background(), mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")})))
			assertProviderFailure(t, terminalFailure(t, terminal), provider.FailureTransport, nil)
		})
	}
}

func TestOpenAIResponsesClosesResponseReturnedWithTransportError(t *testing.T) {
	sentinel := errors.New("redirect failed")
	body := &responsesFailingBody{prefix: bytes.NewReader(nil), err: io.EOF}
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "secret",
		Client: responsesDoerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusTemporaryRedirect, Body: body}, sentinel
		}),
	})
	_, terminal := collectStream(t, implementation.Stream(
		context.Background(),
		mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")}),
	))
	assertProviderFailure(t, terminalFailure(t, terminal), provider.FailureTransport, sentinel)
	if body.closes.Load() != 1 {
		t.Fatalf("body Close calls = %d, want 1", body.closes.Load())
	}
}

func TestOpenAIResponsesRejectsInvalidHTTPStatus(t *testing.T) {
	body := &responsesFailingBody{prefix: bytes.NewReader(nil), err: io.EOF}
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "secret",
		Client: responsesDoerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 700, Header: http.Header{}, Body: body}, nil
		}),
	})
	_, terminal := collectStream(t, implementation.Stream(
		context.Background(),
		mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")}),
	))
	assertProviderFailure(t, terminalFailure(t, terminal), provider.FailureInvalidResponse, provider.ErrOpenAIResponsesStream)
	if body.closes.Load() != 1 {
		t.Fatalf("body Close calls = %d, want 1", body.closes.Load())
	}
}

func TestOpenAIResponsesMapsMidstreamReadErrorAndRetainsPartialText(t *testing.T) {
	sentinel := errors.New("connection reset")
	body := &responsesFailingBody{
		prefix: bytes.NewReader([]byte(responsesSSE(
			map[string]any{
				"type": "response.output_item.added", "output_index": 0,
				"item": map[string]any{"type": "message", "id": "msg", "role": "assistant", "content": []any{}},
			},
			map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": "msg", "delta": "partial"},
		))),
		err: sentinel,
	}
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "secret",
		Client: responsesDoerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       body,
			}, nil
		}),
	})
	_, terminal := collectStream(t, implementation.Stream(
		context.Background(),
		mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")}),
	))
	failure := terminalFailure(t, terminal)
	assertProviderFailure(t, failure, provider.FailureTransport, sentinel)
	if len(failure.Content()) != 1 || failure.Content()[0].Text() != "partial" {
		t.Fatalf("partial content = %#v", failure.Content())
	}
	if body.closes.Load() != 1 {
		t.Fatalf("body Close calls = %d, want 1", body.closes.Load())
	}
}

func TestOpenAIResponsesBoundsHTTPErrorBody(t *testing.T) {
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "secret", MaxErrorBodyBytes: 32,
		Client: staticResponsesDoer(responsesHTTPResponse(
			http.StatusBadRequest,
			"application/json",
			`{"error":{"message":"`+strings.Repeat("x", 256)+`","code":"too_large"}}`,
		)),
	})
	_, terminal := collectStream(t, implementation.Stream(
		context.Background(),
		mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")}),
	))
	failure := terminalFailure(t, terminal)
	assertProviderFailure(t, failure, provider.FailureHTTPStatus, nil)
	var httpFailure *provider.OpenAIResponsesHTTPError
	if !errors.As(failure.Failure(), &httpFailure) || !httpFailure.BodyTruncated() {
		t.Fatalf("HTTP cause = %T/%v", failure.Failure().Cause(), failure.Failure().Cause())
	}
	if len(failure.ErrorMessage()) > 128 {
		t.Fatalf("failure message was not bounded: %d bytes", len(failure.ErrorMessage()))
	}
}

func TestOpenAIResponsesCancellationStopsHTTPAndRetainsPartialText(t *testing.T) {
	requestCancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		flusher := writer.(http.Flusher)
		writeResponsesSSE(t, writer,
			map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg", "role": "assistant", "content": []any{}}},
			map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": "msg", "delta": "partial"},
		)
		flusher.Flush()
		<-request.Context().Done()
		close(requestCancelled)
	}))
	defer server.Close()

	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{BaseURL: server.URL + "/v1", APIKey: "secret"})
	ctx, cancel := context.WithCancelCause(context.Background())
	stream := implementation.Stream(ctx, mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")}))
	collector := &llm.StreamCollector{}
	for _, want := range []string{"start", "text_start", "text_delta"} {
		event, err := stream.Next()
		if err != nil || eventKind(event) != want {
			t.Fatalf("Next() = %s/%v, want %s", eventKind(event), err, want)
		}
		if err := collector.Accept(event); err != nil {
			t.Fatal(err)
		}
	}
	cause := errors.New("stop request")
	cancel(cause)
	event, err := stream.Next()
	if err != nil || eventKind(event) != "error" {
		t.Fatalf("cancel Next() = %s/%v", eventKind(event), err)
	}
	if err := collector.Accept(event); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next() after cancel = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := collector.Close(); err != nil {
		t.Fatal(err)
	}
	terminal, err := collector.Result()
	if err != nil {
		t.Fatal(err)
	}
	failure := terminalFailure(t, terminal)
	if failure.FinishReason() != llm.FinishAborted || len(failure.Content()) != 1 || failure.Content()[0].Text() != "partial" {
		t.Fatalf("cancel terminal = %v/%v", failure.FinishReason(), failure.Content())
	}
	assertProviderFailure(t, failure, provider.FailureCancelled, cause)
	select {
	case <-requestCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP request did not observe cancellation")
	}
}

func TestOpenAIResponsesCancellationPreemptsQueuedOutputEvents(t *testing.T) {
	item := map[string]any{
		"type": "message", "id": "msg", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": "queued"}},
	}
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "secret",
		Client: staticResponsesDoer(responsesHTTPResponse(
			http.StatusOK,
			"text/event-stream",
			responsesSSE(
				map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
				map[string]any{
					"type": "response.completed",
					"response": map[string]any{
						"status": "completed",
						"output": []any{item},
					},
				},
			),
		)),
	})
	ctx, cancel := context.WithCancelCause(context.Background())
	stream := implementation.Stream(ctx, mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")}))
	collector := &llm.StreamCollector{}
	for _, want := range []string{"start", "text_start"} {
		event, err := stream.Next()
		if err != nil || eventKind(event) != want {
			t.Fatalf("Next() = %s/%v, want %s", eventKind(event), err, want)
		}
		if err := collector.Accept(event); err != nil {
			t.Fatal(err)
		}
	}
	cause := errors.New("cancel queued output")
	cancel(cause)
	event, err := stream.Next()
	if err != nil || eventKind(event) != "error" {
		t.Fatalf("cancel Next() = %s/%v", eventKind(event), err)
	}
	if err := collector.Accept(event); err != nil {
		t.Fatal(err)
	}
	if err := collector.Close(); err != nil {
		t.Fatal(err)
	}
	terminal, err := collector.Result()
	if err != nil {
		t.Fatal(err)
	}
	failure := terminalFailure(t, terminal)
	assertProviderFailure(t, failure, provider.FailureCancelled, cause)
	if len(failure.Content()) != 1 || failure.Content()[0].Text() != "" {
		t.Fatalf("queued text leaked after cancellation: %#v", failure.Content())
	}
}

func TestOpenAIResponsesCancellationAfterTerminalDeclarationWins(t *testing.T) {
	prefix := responsesSSE(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"status": "completed",
			"usage":  map[string]any{"input_tokens": 2, "output_tokens": 1, "total_tokens": 3},
		},
	})
	body := newResponsesBlockingBody(prefix)
	doer := responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
		body.ctx = request.Context()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
		}, nil
	})
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "secret", Client: doer,
	})
	ctx, cancel := context.WithCancelCause(context.Background())
	stream := implementation.Stream(ctx, mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")}))
	collector := &llm.StreamCollector{}
	start, err := stream.Next()
	if err != nil || eventKind(start) != "start" {
		t.Fatalf("first Next() = %s/%v", eventKind(start), err)
	}
	if err := collector.Accept(start); err != nil {
		t.Fatal(err)
	}
	next := make(chan llm.StreamEvent, 1)
	nextErr := make(chan error, 1)
	go func() {
		event, err := stream.Next()
		next <- event
		nextErr <- err
	}()
	select {
	case <-body.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not wait for terminal settlement")
	}
	cause := errors.New("cancel during settlement")
	cancel(cause)
	event := <-next
	if err := <-nextErr; err != nil || eventKind(event) != "error" {
		t.Fatalf("settlement Next() = %s/%v", eventKind(event), err)
	}
	if err := collector.Accept(event); err != nil {
		t.Fatal(err)
	}
	if err := collector.Close(); err != nil {
		t.Fatal(err)
	}
	terminal, err := collector.Result()
	if err != nil {
		t.Fatal(err)
	}
	failure := terminalFailure(t, terminal)
	assertProviderFailure(t, failure, provider.FailureCancelled, cause)
	if failure.FinishReason() != llm.FinishAborted || failure.Usage().TotalTokens() != 3 {
		t.Fatalf("terminal = %v, usage = %d", failure.FinishReason(), failure.Usage().TotalTokens())
	}
	if body.closes.Load() != 1 {
		t.Fatalf("body Close calls = %d, want 1", body.closes.Load())
	}
}

func TestOpenAIResponsesCloseUnblocksPendingRead(t *testing.T) {
	body := newResponsesBlockingBody("")
	doer := responsesDoerFunc(func(request *http.Request) (*http.Response, error) {
		body.ctx = request.Context()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
		}, nil
	})
	implementation := mustResponsesProvider(t, provider.OpenAIResponsesConfig{
		BaseURL: "https://fixture.test/v1", APIKey: "secret", Client: doer,
	})
	stream := implementation.Stream(context.Background(), mustResponsesRequest(t, "", []llm.ConversationMessage{mustUser(t, "hi")}))
	if event, err := stream.Next(); err != nil || eventKind(event) != "start" {
		t.Fatalf("first Next() = %s/%v", eventKind(event), err)
	}
	read := make(chan error, 1)
	go func() {
		_, err := stream.Next()
		read <- err
	}()
	select {
	case <-body.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("Next did not enter the pending read")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-read:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("blocked Next() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock Next")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if body.closes.Load() != 1 {
		t.Fatalf("body Close calls = %d, want 1", body.closes.Load())
	}
}

func assertResponsesRequestPayload(t *testing.T, payload map[string]any) {
	t.Helper()
	if payload["model"] != "test-model" || payload["stream"] != true || payload["store"] != false {
		t.Fatalf("payload identity = %#v", payload)
	}
	input, ok := payload["input"].([]any)
	if !ok || len(input) != 4 {
		t.Fatalf("input = %#v", payload["input"])
	}
	roles := make([]string, len(input))
	for index, raw := range input {
		item := raw.(map[string]any)
		roles[index], _ = item["role"].(string)
	}
	if !reflect.DeepEqual(roles, []string{"system", "user", "assistant", "user"}) {
		t.Fatalf("input roles = %v", roles)
	}
	assistant := input[2].(map[string]any)
	if assistant["type"] != "message" || assistant["status"] != "completed" || assistant["id"] != "msg_pi_1" {
		t.Fatalf("assistant replay = %#v", assistant)
	}
}

func mustResponsesProvider(t *testing.T, config provider.OpenAIResponsesConfig) *provider.OpenAIResponsesProvider {
	t.Helper()
	implementation, err := provider.NewOpenAIResponsesProvider(config)
	if err != nil {
		t.Fatalf("NewOpenAIResponsesProvider() error = %v", err)
	}
	return implementation
}

func mustResponsesRequest(t *testing.T, system string, messages []llm.ConversationMessage) provider.Request {
	t.Helper()
	model, err := provider.NewModelRef(provider.OpenAIProviderID, provider.OpenAIResponsesAPI, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	request, err := provider.NewRequest(model, system, messages)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

type responsesDoerFunc func(*http.Request) (*http.Response, error)

func (function responsesDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return function(request)
}

type typedNilResponsesDoer struct{}

func (*typedNilResponsesDoer) Do(*http.Request) (*http.Response, error) {
	panic("typed nil transport must not be called")
}

type responsesBlockingBody struct {
	prefix      *bytes.Reader
	ctx         context.Context
	blocked     chan struct{}
	closed      chan struct{}
	blockedOnce sync.Once
	closeOnce   sync.Once
	closes      atomic.Uint32
}

type responsesFailingBody struct {
	prefix *bytes.Reader
	err    error
	closes atomic.Uint32
}

func (b *responsesFailingBody) Read(destination []byte) (int, error) {
	if b.prefix.Len() != 0 {
		return b.prefix.Read(destination)
	}
	return 0, b.err
}

func (b *responsesFailingBody) Close() error {
	b.closes.Add(1)
	return nil
}

func newResponsesBlockingBody(prefix string) *responsesBlockingBody {
	return &responsesBlockingBody{
		prefix:  bytes.NewReader([]byte(prefix)),
		blocked: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (b *responsesBlockingBody) Read(destination []byte) (int, error) {
	if b.prefix.Len() != 0 {
		return b.prefix.Read(destination)
	}
	b.blockedOnce.Do(func() { close(b.blocked) })
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.closed:
		return 0, io.EOF
	}
}

func (b *responsesBlockingBody) Close() error {
	b.closes.Add(1)
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func staticResponsesDoer(response *http.Response) provider.HTTPDoer {
	return responsesDoerFunc(func(*http.Request) (*http.Response, error) {
		copy := *response
		if response.Body != nil {
			body, _ := io.ReadAll(response.Body)
			response.Body = io.NopCloser(bytes.NewReader(body))
			copy.Body = io.NopCloser(bytes.NewReader(body))
		}
		return &copy, nil
	})
}

func responsesHTTPResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func responsesSSE(events ...any) string {
	var output strings.Builder
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			panic(err)
		}
		output.WriteString("data: ")
		output.Write(encoded)
		output.WriteString("\n\n")
	}
	return output.String()
}

func writeResponsesSSE(t *testing.T, writer io.Writer, events ...any) {
	t.Helper()
	if _, err := io.WriteString(writer, responsesSSE(events...)); err != nil {
		t.Errorf("write SSE: %v", err)
	}
}

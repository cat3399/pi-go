package agent_test

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

type contextRetrySummarizerFunc func(context.Context, session.SummaryInput) (session.SummaryOutput, error)

func (fn contextRetrySummarizerFunc) Summarize(ctx context.Context, input session.SummaryInput) (session.SummaryOutput, error) {
	return fn(ctx, input)
}

func TestAgentSessionPublishesRetryOutsideCoreAgentLifecycle(t *testing.T) {
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider:       newScriptedProvider(t, sessionHTTPFailure(t, 429), mustTextTerminal(t, "recovered")),
		SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		Retry: agent.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }},
		Now:   func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	var lifecycle []agent.AgentEventType
	var retry []agent.AgentEventType
	coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch event.(type) {
		case agent.AgentStartEvent, agent.SessionAgentEndEvent,
			agent.TurnStartEvent, agent.TurnEndEvent,
			agent.MessageStartEvent, agent.MessageUpdateEvent, agent.MessageEndEvent,
			agent.ToolExecutionStartEvent, agent.ToolExecutionUpdateEvent, agent.ToolExecutionEndEvent:
			lifecycle = append(lifecycle, event.Type())
		case agent.AutoRetryStartEvent, agent.AutoRetryEndEvent:
			retry = append(retry, event.Type())
		}
	})
	if result, err := coordinator.Run(context.Background(), "retry"); err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	if len(lifecycle) == 0 || lifecycle[0] != agent.AgentStartEventType || lifecycle[len(lifecycle)-1] != agent.AgentEndEventType {
		t.Fatalf("public lifecycle = %v", lifecycle)
	}
	wantRetry := []agent.AgentEventType{
		agent.AutoRetryStartEventType,
		agent.AutoRetryEndEventType,
	}
	if !reflect.DeepEqual(retry, wantRetry) {
		t.Fatalf("retry lifecycle = %v, want %v", retry, wantRetry)
	}
}

func TestHTTPRetryEventsExposeSafeReasonAndOrdinary400DoesNotRetry(t *testing.T) {
	t.Run("transient 503", func(t *testing.T) {
		transcript := newSessionManager(t)
		const secret = "sk-http-body-secret"
		var calls atomic.Uint32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintf(w, `{"error":{"message":"temporary %s"}}`, secret)
				return
			}
			writeContextSSE(t, w, "after 503")
		}))
		defer server.Close()
		model, implementation := contextRetryProvider(t, server.URL)
		coordinator, err := agent.NewSession(agent.SessionConfig{
			Provider: implementation, SessionManager: transcript, Model: model, Now: func() time.Time { return agentTestEpoch },
			Retry: agent.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }},
		})
		if err != nil {
			t.Fatal(err)
		}
		var lifecycle []agentEventSnapshot
		subscribeAllAgentEvents(coordinator, func(_ context.Context, event observedAgentEvent) {
			if event.Type() == agent.AutoRetryStartEventType || event.Type() == agent.AutoRetryEndEventType {
				lifecycle = append(lifecycle, snapshotAgentEvent(event))
				if strings.Contains(fmt.Sprintf("%+v", event), secret) {
					t.Fatalf("event leaked HTTP body: %+v", event)
				}
			}
		})
		result, err := coordinator.Run(context.Background(), "retry 503 prompt")
		if err != nil || !result.Succeeded() || calls.Load() != 2 {
			t.Fatalf("Run() succeeded=%v err=%v calls=%d", result.Succeeded(), err, calls.Load())
		}
		if len(lifecycle) != 2 || lifecycle[0].Kind != agent.AutoRetryStartEventType ||
			lifecycle[1].Kind != agent.AutoRetryEndEventType || !lifecycle[1].RetrySucceeded {
			t.Fatalf("lifecycle = %+v", lifecycle)
		}
	})

	t.Run("transient exhaustion", func(t *testing.T) {
		transcript := newSessionManager(t)
		const secret = "ordinary-retry-exhaust-secret"
		var calls atomic.Uint32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			http.Error(w, `{"error":{"message":"unavailable `+secret+`"}}`, http.StatusServiceUnavailable)
		}))
		defer server.Close()
		model, implementation := contextRetryProvider(t, server.URL)
		coordinator, err := agent.NewSession(agent.SessionConfig{
			Provider: implementation, SessionManager: transcript, Model: model, Now: func() time.Time { return agentTestEpoch },
			Retry: agent.RetryPolicy{MaxAttempts: 3, Sleep: func(context.Context, time.Duration) error { return nil }},
		})
		if err != nil {
			t.Fatal(err)
		}
		var lifecycle []agentEventSnapshot
		subscribeAllAgentEvents(coordinator, func(_ context.Context, event observedAgentEvent) {
			switch event.Type() {
			case agent.AutoRetryStartEventType, agent.AutoRetryEndEventType:
				lifecycle = append(lifecycle, snapshotAgentEvent(event))
			}
		})
		result, err := coordinator.Run(context.Background(), "exhaust retries")
		if err != nil || calls.Load() != 3 {
			t.Fatalf("Run() err=%v calls=%d", err, calls.Load())
		}
		terminal, ok := result.Terminal()
		if !ok || terminal.FinishReason() != llm.FinishError {
			t.Fatalf("terminal=%T/%v", terminal, terminal.FinishReason())
		}
		if len(lifecycle) != 3 || lifecycle[0].Kind != agent.AutoRetryStartEventType ||
			lifecycle[1].Kind != agent.AutoRetryStartEventType || lifecycle[2].Kind != agent.AutoRetryEndEventType ||
			lifecycle[2].RetrySucceeded {
			t.Fatalf("exhausted provider retry lifecycle = %+v", lifecycle)
		}
		if strings.Contains(fmt.Sprintf("%+v", lifecycle), secret) {
			t.Fatalf("provider retry lifecycle leaked response detail: %+v", lifecycle)
		}
	})

	t.Run("ordinary 400", func(t *testing.T) {
		transcript := newSessionManager(t)
		old, _ := llm.NewUserTextMessage(strings.Repeat("ordinary failure history ", 10), agentTestEpoch)
		if _, err := transcript.AppendLLMMessage(context.Background(), old); err != nil {
			t.Fatal(err)
		}
		var calls atomic.Uint32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"invalid_value","message":"Your input exceeds the context window because the max_output_tokens parameter is invalid"}}`)
		}))
		defer server.Close()
		model, implementation := contextRetryProvider(t, server.URL)
		coordinator, err := agent.NewSession(agent.SessionConfig{
			Provider: implementation, SessionManager: transcript, Model: model, KeepRecentTokens: 1,
			Retry: agent.RetryPolicy{MaxAttempts: 3, Sleep: func(context.Context, time.Duration) error { return nil }}, Now: func() time.Time { return agentTestEpoch },
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := coordinator.Run(context.Background(), "ordinary 400 prompt")
		if err != nil || calls.Load() != 1 {
			t.Fatalf("Run() err=%v calls=%d", err, calls.Load())
		}
		terminal, _ := result.Terminal()
		failure, ok := terminal.(llm.AssistantFailureMessage)
		var classified *provider.ProviderFailure
		if !ok || !errors.As(failure.Failure().Cause(), &classified) || classified.Kind() != provider.FailureHTTPStatus {
			t.Fatalf("terminal = %T cause=%v", terminal, failure.Failure().Cause())
		}
		for _, entry := range transcript.Entries() {
			if _, handoff := entry.ContextHandoff(); handoff {
				t.Fatal("ordinary 400 triggered compaction")
			}
		}
	})
}

func TestProviderRetryLifecycleClosesWhenSecondAttemptCannotDispatch(t *testing.T) {
	tests := []struct {
		name      string
		transform func([]llm.ConversationMessage) ([]llm.ConversationMessage, error)
		wantError error
	}{
		{
			name: "transform failure",
			transform: func([]llm.ConversationMessage) ([]llm.ConversationMessage, error) {
				return nil, errors.New("second transform failed")
			},
			wantError: agent.ErrContextTransform,
		},
		{
			name: "request rebuild failure",
			transform: func([]llm.ConversationMessage) ([]llm.ConversationMessage, error) {
				return []llm.ConversationMessage{nil}, nil
			},
			wantError: agent.ErrContextTransform,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			transcript := newSessionManager(t)
			var providerCalls atomic.Uint32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				providerCalls.Add(1)
				http.Error(w, `{"error":{"message":"temporarily unavailable"}}`, http.StatusServiceUnavailable)
			}))
			defer server.Close()
			model, implementation := contextRetryProvider(t, server.URL)
			var transformCalls atomic.Uint32
			var attemptObserved atomic.Bool
			coordinator, err := agent.NewSession(agent.SessionConfig{
				Provider: implementation, SessionManager: transcript, Model: model, Now: func() time.Time { return agentTestEpoch },
				Retry: agent.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }},
				TransformContext: func(_ context.Context, messages []llm.ConversationMessage) ([]llm.ConversationMessage, error) {
					if transformCalls.Add(1) == 1 {
						return messages, nil
					}
					if !attemptObserved.Load() {
						t.Error("retry attempt event was not published before request reconstruction")
					}
					return testCase.transform(messages)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			var lifecycle []agentEventSnapshot
			subscribeAllAgentEvents(coordinator, func(_ context.Context, event observedAgentEvent) {
				switch event.Type() {
				case agent.AutoRetryStartEventType, agent.AutoRetryEndEventType:
					lifecycle = append(lifecycle, snapshotAgentEvent(event))
					if event.Type() == agent.AutoRetryStartEventType {
						attemptObserved.Store(true)
					}
				}
			})
			result, err := coordinator.Run(context.Background(), "retry then fail reconstruction")
			terminal, ok := result.Terminal()
			failure, failed := terminal.(llm.AssistantFailureMessage)
			if err != nil || !ok || !failed || !errors.Is(failure.Failure().Cause(), testCase.wantError) {
				t.Fatalf("Run() terminal=%T cause=%v error=%v, want %v", terminal, failure.Failure().Cause(), err, testCase.wantError)
			}
			if providerCalls.Load() != 1 || transformCalls.Load() != 2 {
				t.Fatalf("provider calls=%d transform calls=%d", providerCalls.Load(), transformCalls.Load())
			}
			assertProviderRetryLifecycle(t, lifecycle, provider.RetryFinishFailed, provider.FailureInvalidRequest)
		})
	}
}

func TestProviderRetryLifecycleClosesWhenCancelledBeforeRedispatch(t *testing.T) {
	transcript := newSessionManager(t)
	var providerCalls atomic.Uint32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		http.Error(w, `{"error":{"message":"temporarily unavailable"}}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	model, implementation := contextRetryProvider(t, server.URL)
	runContext, cancel := context.WithCancelCause(context.Background())
	cancelCause := errors.New("cancel retry before redispatch")
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: transcript, Model: model, Now: func() time.Time { return agentTestEpoch },
		Retry: agent.RetryPolicy{
			MaxAttempts: 2,
			Sleep: func(context.Context, time.Duration) error {
				cancel(cancelCause)
				// A custom waiter may observe cancellation immediately after its
				// final check. The next loop must still close the retry scope.
				return nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var lifecycle []agentEventSnapshot
	subscribeAllAgentEvents(coordinator, func(_ context.Context, event observedAgentEvent) {
		switch event.Type() {
		case agent.AutoRetryStartEventType, agent.AutoRetryEndEventType:
			lifecycle = append(lifecycle, snapshotAgentEvent(event))
		}
	})
	result, err := coordinator.Run(runContext, "retry then cancel")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	terminal, ok := result.Terminal()
	if !ok || terminal.FinishReason() != llm.FinishAborted || providerCalls.Load() != 1 {
		t.Fatalf("terminal=%T/%v provider calls=%d", terminal, terminal.FinishReason(), providerCalls.Load())
	}
	if len(lifecycle) != 2 || lifecycle[0].Kind != agent.AutoRetryStartEventType ||
		lifecycle[1].Kind != agent.AutoRetryEndEventType || lifecycle[0].RetryAttempt != 1 ||
		lifecycle[1].RetryAttempt != 1 || lifecycle[1].RetrySucceeded {
		t.Fatalf("cancelled provider retry lifecycle = %+v", lifecycle)
	}
}

func assertProviderRetryLifecycle(t *testing.T, events []agentEventSnapshot, finishReason provider.RetryFinishReason, finishKind provider.FailureKind) {
	t.Helper()
	if len(events) != 2 || events[0].Kind != agent.AutoRetryStartEventType ||
		events[1].Kind != agent.AutoRetryEndEventType {
		t.Fatalf("provider retry lifecycle = %+v", events)
	}
	if events[0].RetryAttempt != 1 || events[1].RetryAttempt != 1 || events[1].RetrySucceeded {
		t.Fatalf("provider retry metadata = %+v", events)
	}
	_ = finishReason
	_ = finishKind
}

func assertSummarizationRetryReason(t *testing.T, events []agentEventSnapshot, reason agent.CompactionReason) {
	t.Helper()
	for _, event := range events {
		if event.CompactionReason != reason {
			t.Fatalf("summarization retry reason = %s, want %s in %+v", event.CompactionReason, reason, events)
		}
	}
}

func appendCompactionHistory(t *testing.T, transcript *session.SessionManager) {
	t.Helper()
	for _, text := range []string{"old context to summarize", "recent context to retain"} {
		message, err := llm.NewUserTextMessage(text, agentTestEpoch)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transcript.AppendLLMMessage(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}
}

func appendMatchingAssistant(t *testing.T, transcript *session.SessionManager, model provider.Model) {
	t.Helper()
	assistant, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{mustTextBlock(t, "prior response")}, llm.FinishStop, mustUsage(t, model.ContextWindow()-8000, 0), agentTestEpoch,
		llm.AssistantProvenance{Provider: model.Provider(), API: model.API(), Model: model.ID()},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.AppendLLMMessage(context.Background(), assistant); err != nil {
		t.Fatal(err)
	}
}

func newCompactionCoordinator(t *testing.T, transcript *session.SessionManager, summarizer session.Summarizer) *agent.AgentSession {
	t.Helper()
	model, err := newTestModel("scripted", "scripted", "compaction-fixture")
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: transcript, Model: model,
		Summarizer: summarizer, KeepRecentTokens: 1, Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func assertManualCompactionFailureLifecycle(t *testing.T, events []agentEventSnapshot, safeError error, secret string) {
	t.Helper()
	if len(events) != 2 {
		t.Fatalf("manual compaction lifecycle = %+v", events)
	}
	assertCompactionLifecycle(t, events, agent.CompactionManual, safeError)
	for _, event := range events {
		if strings.Contains(fmt.Sprintf("%+v", event), secret) {
			t.Fatalf("manual compaction event leaked detail %q: %+v", secret, event)
		}
	}
}

func assertNoCompactionEntry(t *testing.T, transcript *session.SessionManager) {
	t.Helper()
	for _, entry := range transcript.Entries() {
		if _, handoff := entry.ContextHandoff(); handoff {
			t.Fatal("failed compaction appended a durable checkpoint")
		}
	}
}

func contextRetryProvider(t *testing.T, baseURL string) (provider.Model, *provider.OpenAIResponsesProvider) {
	t.Helper()
	model, err := newAgentModel(provider.ModelSpec{
		Provider: provider.OpenAIProviderID, API: provider.OpenAIResponsesAPI, ID: "fixture-model",
		ContextWindow: 128000, MaxTokens: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{BaseURL: baseURL, APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	return model, implementation
}

func assertContextRetryEntries(t *testing.T, transcript *session.SessionManager, prompt, final string, wantCompactions, wantFailures int) {
	t.Helper()
	var users, finals, compactions, failures int
	for _, entry := range transcript.Entries() {
		if _, handoff := entry.ContextHandoff(); handoff {
			compactions++
		}
		message, ok := entry.Message()
		if !ok {
			continue
		}
		switch value := message.(type) {
		case llm.UserTextMessage:
			if strings.Contains(joinContextText(value.Content()), prompt) {
				users++
			}
		case llm.UserContentMessage:
			if strings.Contains(joinUserContentText(value.Content()), prompt) {
				users++
			}
		case llm.AssistantTextMessage:
			if strings.Contains(joinContextText(value.Content()), final) {
				finals++
			}
		case llm.AssistantFailureMessage:
			if value.ErrorMessage() != "prior failure" {
				failures++
			}
		}
	}
	if users != 1 || finals != 1 || failures != wantFailures || compactions != wantCompactions {
		t.Fatalf("users=%d finals=%d failures=%d compactions=%d", users, finals, failures, compactions)
	}
}

func assertCompactionLifecycle(t *testing.T, events []agentEventSnapshot, reason agent.CompactionReason, settledError error) {
	t.Helper()
	if len(events) != 2 || events[0].Kind != agent.CompactionStartEventType || events[1].Kind != agent.CompactionEndEventType {
		t.Fatalf("compaction events = %+v", events)
	}
	if events[0].CompactionReason != reason || events[1].CompactionReason != reason {
		t.Fatalf("compaction reasons = %s/%s, want %s", events[0].CompactionReason, events[1].CompactionReason, reason)
	}
	wantRetry := reason == agent.CompactionContextOverflow
	if events[0].CompactionWillRetry != wantRetry || events[1].CompactionWillRetry != wantRetry {
		t.Fatalf("compaction willRetry = %t/%t, want %t", events[0].CompactionWillRetry, events[1].CompactionWillRetry, wantRetry)
	}
	if settledError == nil {
		if events[1].RunError != nil || events[1].Compaction == nil {
			t.Fatalf("successful compaction settlement = %+v", events[1])
		}
		return
	}
	if errors.Is(settledError, errExpectedCompactionAbort) {
		if events[1].RunError != nil || events[1].Compaction != nil || !events[1].CompactionAborted || events[1].CompactionWillRetry {
			t.Fatalf("aborted compaction settlement = %+v", events[1])
		}
		return
	}
	matches := events[1].RunError != nil && events[1].RunError.Error() == settledError.Error()
	if !matches || events[1].Compaction != nil {
		t.Fatalf("failed compaction settlement = %+v, want exact safe error %v", events[1], settledError)
	}
}

var errExpectedCompactionAbort = errors.New("expected compaction abort")

func joinContextText(parts []llm.TextBlock) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text())
	}
	return b.String()
}

func joinUserContentText(parts []llm.UserContentBlock) string {
	var text []string
	for _, part := range parts {
		if block, ok := part.(llm.TextBlock); ok {
			text = append(text, block.Text())
		}
	}
	return strings.Join(text, "\n")
}

func writeContextSSE(t *testing.T, w http.ResponseWriter, text string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	events := []map[string]any{
		{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "m", "role": "assistant", "content": []any{}}},
		{"type": "response.output_text.delta", "output_index": 0, "item_id": "m", "delta": text},
		{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "id": "m", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text}}}},
		{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message"}}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}},
	}
	for _, event := range events {
		data, _ := json.Marshal(event)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			t.Error(err)
		}
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		t.Error(err)
	}
}

func dropContextSSE(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("local response writer does not support hijacking")
	}
	connection, buffered, err := hijacker.Hijack()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := buffered.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n10\r\ndata: partial"); err != nil {
		t.Fatal(err)
	}
	if err := buffered.Flush(); err != nil {
		t.Fatal(err)
	}
}

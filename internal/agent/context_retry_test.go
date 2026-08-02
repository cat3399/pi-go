package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

// This drives the real OpenAI request/SSE adapter through a local server. The
// first request is the Session summary, the second gets a transient 503, and
// the third succeeds. No retry may duplicate any durable conversation entry.
func TestContextThresholdCompactsThenRetriesProductionProviderWithoutDuplicateTranscript(t *testing.T) {
	transcript := newSession(t)
	old, err := llm.NewUserTextMessage(strings.Repeat("old context ", 12), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Append(context.Background(), old, session.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Uint32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer fixture-key" {
			t.Errorf("authorization = %q", got)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode: %v", err)
		}
		switch calls.Add(1) {
		case 1:
			writeContextSSE(t, w, "summary checkpoint")
		case 2:
			// A truncated chunked SSE response reaches the adapter as a transport
			// stream drop (not a parse/auth failure), so the retry decision is
			// exercised through the same local HTTP path as production.
			dropContextSSE(t, w)
		case 3:
			writeContextSSE(t, w, "final answer")
		default:
			t.Errorf("unexpected provider request %d", calls.Load())
		}
	}))
	defer server.Close()
	model, err := provider.NewModelRef(provider.OpenAIProviderID, provider.OpenAIResponsesAPI, "fixture-model")
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{BaseURL: server.URL + "/v1", APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	summarizer, err := provider.NewContextSummarizer(implementation, model, func() time.Time { return agentTestEpoch })
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := agent.New(agent.Config{
		Provider: implementation, Transcript: transcript, Model: model, Now: func() time.Time { return agentTestEpoch },
		ContextWindow: 2, ContextReserve: 1, KeepRecentTokens: 1, Summarizer: summarizer,
		Retry: agent.RetryPolicy{MaxAttempts: 2, InitialDelay: time.Millisecond, Sleep: func(context.Context, time.Duration) error { return nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Run(context.Background(), "new prompt")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.Succeeded() || calls.Load() != 3 {
		t.Fatalf("result succeeded=%v calls=%d", result.Succeeded(), calls.Load())
	}
	entries := transcript.Entries()
	var users, finals, compactions int
	for _, entry := range entries {
		if entry.Type() == "compaction" {
			compactions++
		}
		message, ok := entry.Message()
		if !ok {
			continue
		}
		switch value := message.(type) {
		case llm.UserTextMessage:
			if strings.Contains(joinContextText(value.Content()), "new prompt") {
				users++
			}
		case llm.AssistantTextMessage:
			if strings.Contains(joinContextText(value.Content()), "final answer") {
				finals++
			}
		}
	}
	if users != 1 || finals != 1 || compactions != 1 {
		t.Fatalf("users=%d finals=%d compactions=%d entries=%d", users, finals, compactions, len(entries))
	}
	if err := coordinator.WaitForIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProviderContextOverflowCompactsOnceRebuildsContextAndPublishesSafeRetryLifecycle(t *testing.T) {
	transcript := newSession(t)
	old, err := llm.NewUserTextMessage(strings.Repeat("historical context ", 10), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Append(context.Background(), old, session.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	const secretEcho = "sk-review-must-not-reach-events"
	var calls atomic.Uint32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"maximum context length exceeded; echoed=%s"}}`, secretEcho)
		case 2:
			writeContextSSE(t, w, "durable overflow summary")
		case 3:
			writeContextSSE(t, w, "accepted after overflow")
		default:
			t.Errorf("unexpected request %d", calls.Load())
		}
	}))
	defer server.Close()
	model, err := provider.NewModelRef(provider.OpenAIProviderID, provider.OpenAIResponsesAPI, "fixture-model")
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{BaseURL: server.URL, APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	summarizer, err := provider.NewContextSummarizerWithRetry(implementation, model, func() time.Time { return agentTestEpoch }, provider.RetryPolicy{
		MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := agent.New(agent.Config{
		Provider: implementation, Transcript: transcript, Model: model,
		KeepRecentTokens: 1, Summarizer: summarizer, Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	var retryEvents []agent.Event
	coordinator.Subscribe(func(_ context.Context, event agent.Event) {
		if event.Kind == agent.EventRetryScheduled || event.Kind == agent.EventRetryAttempt || event.Kind == agent.EventRetryFinished {
			retryEvents = append(retryEvents, event)
			if strings.Contains(fmt.Sprintf("%+v", event), secretEcho) {
				t.Errorf("retry event leaked provider body: %+v", event)
			}
		}
	})
	result, err := coordinator.Run(context.Background(), "new overflow prompt")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	terminal, ok := result.Terminal()
	text, okText := terminal.(llm.AssistantTextMessage)
	if !ok || !okText || text.Usage().TotalTokens() != 2 || result.ProviderTurns() != 2 || calls.Load() != 3 {
		t.Fatalf("terminal=%T usage=%d turns=%d calls=%d", terminal, text.Usage().TotalTokens(), result.ProviderTurns(), calls.Load())
	}
	if len(retryEvents) != 3 || retryEvents[0].Kind != agent.EventRetryScheduled || retryEvents[1].Kind != agent.EventRetryAttempt || retryEvents[2].Kind != agent.EventRetryFinished {
		t.Fatalf("retry events = %+v", retryEvents)
	}
	if retryEvents[0].RetryFailureKind != provider.FailureContextOverflow || retryEvents[0].RetryHTTPStatus != 400 || !retryEvents[2].RetrySucceeded {
		t.Fatalf("retry lifecycle = %+v", retryEvents)
	}
	assertContextRetryEntries(t, transcript, "new overflow prompt", "accepted after overflow", 1)
}

func TestContextSummarizerRetriesTransientStreamDropBeforeSingleCompactionCommit(t *testing.T) {
	transcript := newSession(t)
	old, err := llm.NewUserTextMessage(strings.Repeat("summary retry context ", 10), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Append(context.Background(), old, session.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Uint32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			dropContextSSE(t, w)
		case 2:
			writeContextSSE(t, w, "summary after transient")
		case 3:
			writeContextSSE(t, w, "answer after summary retry")
		default:
			t.Errorf("unexpected request %d", calls.Load())
		}
	}))
	defer server.Close()
	model, implementation := contextRetryProvider(t, server.URL)
	var delays []time.Duration
	summarizer, err := provider.NewContextSummarizerWithRetry(implementation, model, func() time.Time { return agentTestEpoch }, provider.RetryPolicy{
		MaxAttempts: 2, InitialDelay: time.Millisecond, MaxRetryAfter: 10 * time.Millisecond,
		Sleep: func(_ context.Context, delay time.Duration) error { delays = append(delays, delay); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := agent.New(agent.Config{
		Provider: implementation, Transcript: transcript, Model: model, ContextWindow: 2, ContextReserve: 1,
		KeepRecentTokens: 1, Summarizer: summarizer, Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Run(context.Background(), "summary retry prompt")
	if err != nil || !result.Succeeded() {
		t.Fatalf("Run() = succeeded %v, error %v", result.Succeeded(), err)
	}
	if calls.Load() != 3 || len(delays) != 1 || delays[0] != time.Millisecond {
		t.Fatalf("calls=%d delays=%v", calls.Load(), delays)
	}
	assertContextRetryEntries(t, transcript, "summary retry prompt", "answer after summary retry", 1)
}

func TestAbortDuringSummarizerRetrySettlesQueueAndDoesNotCommitCompaction(t *testing.T) {
	transcript := newSession(t)
	old, err := llm.NewUserTextMessage(strings.Repeat("cancel summary context ", 10), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Append(context.Background(), old, session.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"retry me"}}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	model, implementation := contextRetryProvider(t, server.URL)
	sleepEntered := make(chan struct{})
	summarizer, err := provider.NewContextSummarizerWithRetry(implementation, model, func() time.Time { return agentTestEpoch }, provider.RetryPolicy{
		MaxAttempts: 3, InitialDelay: time.Hour,
		Sleep: func(ctx context.Context, _ time.Duration) error {
			select {
			case <-sleepEntered:
			default:
				close(sleepEntered)
			}
			<-ctx.Done()
			return context.Cause(ctx)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := agent.New(agent.Config{
		Provider: implementation, Transcript: transcript, Model: model, ContextWindow: 2, ContextReserve: 1,
		KeepRecentTokens: 1, Summarizer: summarizer, Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	type runOutcome struct {
		result agent.Result
		err    error
	}
	runDone := make(chan runOutcome, 1)
	go func() {
		result, err := coordinator.Run(context.Background(), "cancel summary prompt")
		runDone <- runOutcome{result: result, err: err}
	}()
	select {
	case <-sleepEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("summarizer did not enter retry wait")
	}
	if state := coordinator.State(); state.Phase() != agent.PhaseCompacting {
		t.Fatalf("phase = %s", state.Phase())
	}
	if err := coordinator.Steer("queued while summarizer retries"); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	outcome := <-runDone
	terminal, ok := outcome.result.Terminal()
	if outcome.err != nil || !ok || terminal.FinishReason() != llm.FinishAborted {
		t.Fatalf("Run() terminal=%T/%v error=%v", terminal, terminal.FinishReason(), outcome.err)
	}
	if err := coordinator.WaitForIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	steering, _ := coordinator.Queues()
	if len(steering) != 1 || joinContextText(steering[0].Content()) != "queued while summarizer retries" {
		t.Fatalf("steering queue = %+v", steering)
	}
	for _, entry := range transcript.Entries() {
		if entry.Type() == "compaction" {
			t.Fatal("cancelled summary appended a compaction")
		}
	}
}

func TestSummarizerRetryExhaustionDoesNotAppendCompaction(t *testing.T) {
	transcript := newSession(t)
	old, _ := llm.NewUserTextMessage(strings.Repeat("summary failure context ", 10), agentTestEpoch)
	if _, err := transcript.Append(context.Background(), old, session.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Uint32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":{"message":"still unavailable"}}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	model, implementation := contextRetryProvider(t, server.URL)
	summarizer, err := provider.NewContextSummarizerWithRetry(implementation, model, func() time.Time { return agentTestEpoch }, provider.RetryPolicy{
		MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := agent.New(agent.Config{
		Provider: implementation, Transcript: transcript, Model: model, ContextWindow: 2, ContextReserve: 1,
		KeepRecentTokens: 1, Summarizer: summarizer, Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Run(context.Background(), "summary failure prompt"); err == nil || !errors.Is(err, session.ErrSummaryFailed) {
		t.Fatalf("Run() error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want 2", calls.Load())
	}
	for _, entry := range transcript.Entries() {
		if entry.Type() == "compaction" {
			t.Fatal("failed summary appended a compaction")
		}
	}
}

func TestContextOverflowCompactionRetryOccursAtMostOnce(t *testing.T) {
	transcript := newSession(t)
	old, _ := llm.NewUserTextMessage(strings.Repeat("repeat overflow context ", 10), agentTestEpoch)
	if _, err := transcript.Append(context.Background(), old, session.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Uint32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1, 3:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"context_length_exceeded","message":"maximum context length exceeded"}}`)
		case 2:
			writeContextSSE(t, w, "one summary only")
		default:
			t.Errorf("unexpected request %d", calls.Load())
		}
	}))
	defer server.Close()
	model, implementation := contextRetryProvider(t, server.URL)
	summarizer, err := provider.NewContextSummarizerWithRetry(implementation, model, func() time.Time { return agentTestEpoch }, provider.RetryPolicy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := agent.New(agent.Config{
		Provider: implementation, Transcript: transcript, Model: model,
		KeepRecentTokens: 1, Summarizer: summarizer, Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Run(context.Background(), "repeat overflow prompt")
	if err != nil {
		t.Fatal(err)
	}
	terminal, _ := result.Terminal()
	failure, ok := terminal.(llm.AssistantFailureMessage)
	if !ok || calls.Load() != 3 {
		t.Fatalf("terminal=%T calls=%d", terminal, calls.Load())
	}
	var classified *provider.ProviderFailure
	if !errors.As(failure.Failure().Cause(), &classified) || classified.Kind() != provider.FailureContextOverflow {
		t.Fatalf("terminal cause = %v", failure.Failure().Cause())
	}
	var compactions int
	for _, entry := range transcript.Entries() {
		if entry.Type() == "compaction" {
			compactions++
		}
	}
	if compactions != 1 {
		t.Fatalf("compactions = %d", compactions)
	}
}

func TestHTTPRetryEventsExposeSafeReasonAndOrdinary400DoesNotRetry(t *testing.T) {
	t.Run("transient 503", func(t *testing.T) {
		transcript := newSession(t)
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
		coordinator, err := agent.New(agent.Config{
			Provider: implementation, Transcript: transcript, Model: model, Now: func() time.Time { return agentTestEpoch },
			Retry: agent.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }},
		})
		if err != nil {
			t.Fatal(err)
		}
		var lifecycle []agent.Event
		coordinator.Subscribe(func(_ context.Context, event agent.Event) {
			if event.Kind == agent.EventRetryScheduled || event.Kind == agent.EventRetryAttempt || event.Kind == agent.EventRetryFinished {
				lifecycle = append(lifecycle, event)
				if strings.Contains(fmt.Sprintf("%+v", event), secret) {
					t.Fatalf("event leaked HTTP body: %+v", event)
				}
			}
		})
		result, err := coordinator.Run(context.Background(), "retry 503 prompt")
		if err != nil || !result.Succeeded() || calls.Load() != 2 {
			t.Fatalf("Run() succeeded=%v err=%v calls=%d", result.Succeeded(), err, calls.Load())
		}
		if len(lifecycle) != 3 || lifecycle[0].RetryFailureKind != provider.FailureHTTPStatus || lifecycle[0].RetryHTTPStatus != 503 || !lifecycle[2].RetrySucceeded {
			t.Fatalf("lifecycle = %+v", lifecycle)
		}
	})

	t.Run("ordinary 400", func(t *testing.T) {
		transcript := newSession(t)
		old, _ := llm.NewUserTextMessage(strings.Repeat("ordinary failure history ", 10), agentTestEpoch)
		if _, err := transcript.Append(context.Background(), old, session.AppendOptions{}); err != nil {
			t.Fatal(err)
		}
		var calls atomic.Uint32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"invalid_value","message":"context field is malformed"}}`)
		}))
		defer server.Close()
		model, implementation := contextRetryProvider(t, server.URL)
		summarizer, err := provider.NewContextSummarizerWithRetry(implementation, model, nil, provider.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }})
		if err != nil {
			t.Fatal(err)
		}
		coordinator, err := agent.New(agent.Config{
			Provider: implementation, Transcript: transcript, Model: model, Summarizer: summarizer, KeepRecentTokens: 1,
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
			if entry.Type() == "compaction" {
				t.Fatal("ordinary 400 triggered compaction")
			}
		}
	})
}

func contextRetryProvider(t *testing.T, baseURL string) (provider.ModelRef, *provider.OpenAIResponsesProvider) {
	t.Helper()
	model, err := provider.NewModelRef(provider.OpenAIProviderID, provider.OpenAIResponsesAPI, "fixture-model")
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := provider.NewOpenAIResponsesProvider(provider.OpenAIResponsesConfig{BaseURL: baseURL, APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	return model, implementation
}

func assertContextRetryEntries(t *testing.T, transcript *session.Session, prompt, final string, wantCompactions int) {
	t.Helper()
	var users, finals, compactions, failures int
	for _, entry := range transcript.Entries() {
		if entry.Type() == "compaction" {
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
		case llm.AssistantTextMessage:
			if strings.Contains(joinContextText(value.Content()), final) {
				finals++
			}
		case llm.AssistantFailureMessage:
			failures++
		}
	}
	if users != 1 || finals != 1 || failures != 0 || compactions != wantCompactions {
		t.Fatalf("users=%d finals=%d failures=%d compactions=%d", users, finals, failures, compactions)
	}
}

func joinContextText(parts []llm.TextBlock) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text())
	}
	return b.String()
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

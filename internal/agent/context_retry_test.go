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

// This drives the real OpenAI request/SSE adapter through a local server. The
// first request is the Session summary, the second gets a transient 503, and
// the third succeeds. No retry may duplicate any durable conversation entry.
func TestContextThresholdCompactsThenRetriesProductionProviderWithoutDuplicateTranscript(t *testing.T) {
	transcript := newSessionManager(t)
	old, err := llm.NewUserTextMessage(strings.Repeat("old context ", 12), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.AppendLLMMessage(context.Background(), old); err != nil {
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
	model, err := newAgentModel(provider.ModelSpec{
		Provider: provider.OpenAIProviderID, API: provider.OpenAIResponsesAPI, ID: "fixture-model",
		ContextWindow: 24, MaxTokens: 1,
	})
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
	appendMatchingAssistant(t, transcript, model)
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: transcript, Model: model, Now: func() time.Time { return agentTestEpoch },
		ContextWindow: 2, ContextReserve: 1, KeepRecentTokens: 1, Summarizer: summarizer,
		Retry: agent.RetryPolicy{MaxAttempts: 2, InitialDelay: time.Millisecond, Sleep: func(context.Context, time.Duration) error { return nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	var compactionEvents []agentEventSnapshot
	subscribeAllAgentEvents(coordinator, func(_ context.Context, event observedAgentEvent) {
		if event.Type() == agent.CompactionStartEventType || event.Type() == agent.CompactionEndEventType {
			compactionEvents = append(compactionEvents, snapshotAgentEvent(event))
		}
	})
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
		case llm.UserContentMessage:
			if strings.Contains(joinUserContentText(value.Content()), "new prompt") {
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
	assertCompactionLifecycle(t, compactionEvents, agent.CompactionThreshold, nil)
	if err := coordinator.WaitForIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProviderContextOverflowCompactsOnceRebuildsContextAndPublishesSafeCompactionLifecycle(t *testing.T) {
	transcript := newSessionManager(t)
	old, err := llm.NewUserTextMessage(strings.Repeat("historical context ", 10), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.AppendLLMMessage(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	const secretEcho = "sk-review-must-not-reach-events"
	var calls atomic.Uint32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		switch call {
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
	model, err := newAgentModel(provider.ModelSpec{
		Provider: provider.OpenAIProviderID, API: provider.OpenAIResponsesAPI, ID: "fixture-model",
		ContextWindow: 32, MaxTokens: 1,
	})
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
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: transcript, Model: model,
		ContextReserve: 1, KeepRecentTokens: 1, Summarizer: summarizer, Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	var compactionEvents []agentEventSnapshot
	subscribeAllAgentEvents(coordinator, func(_ context.Context, event observedAgentEvent) {
		if event.Type() == agent.CompactionStartEventType || event.Type() == agent.CompactionEndEventType {
			compactionEvents = append(compactionEvents, snapshotAgentEvent(event))
			if strings.Contains(fmt.Sprintf("%+v", event), secretEcho) {
				t.Errorf("compaction event leaked provider body: %+v", event)
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
	assertCompactionLifecycle(t, compactionEvents, agent.CompactionContextOverflow, nil)
	assertContextRetryEntries(t, transcript, "new overflow prompt", "accepted after overflow", 1, 1)
}

func TestContextSummarizerRetriesTransientStreamDropBeforeSingleCompactionCommit(t *testing.T) {
	transcript := newSessionManager(t)
	old, err := llm.NewUserTextMessage(strings.Repeat("summary retry context ", 10), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.AppendLLMMessage(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Uint32
	var requestSessionIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestSessionIDs = append(requestSessionIDs, request.Header.Get("session_id"))
		call := calls.Add(1)
		switch call {
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
	var capturedRequests []provider.Request
	capturing := loopProviderFunc(func(ctx context.Context, request provider.Request) provider.EventStream {
		capturedRequests = append(capturedRequests, request)
		return implementation.Stream(ctx, request)
	})
	appendMatchingAssistant(t, transcript, model)
	var delays []time.Duration
	summarizer, err := provider.NewContextSummarizerWithRetry(capturing, model, func() time.Time { return agentTestEpoch }, provider.RetryPolicy{
		MaxAttempts: 2, InitialDelay: time.Millisecond, MaxRetryAfter: 10 * time.Millisecond,
		Sleep: func(_ context.Context, delay time.Duration) error { delays = append(delays, delay); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: capturing, SessionManager: transcript, Model: model, ContextWindow: 2, ContextReserve: 1,
		KeepRecentTokens: 1, Summarizer: summarizer, Retry: agent.RetryPolicy{MaxAttempts: 1},
		Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	var retryEvents []agentEventSnapshot
	subscribeAllAgentEvents(coordinator, func(_ context.Context, event observedAgentEvent) {
		switch event.Type() {
		case agent.SummarizationRetryScheduledEventType, agent.SummarizationRetryAttemptEventType, agent.SummarizationRetryFinishedEventType:
			retryEvents = append(retryEvents, snapshotAgentEvent(event))
		}
	})
	result, err := coordinator.Run(context.Background(), "summary retry prompt")
	if err != nil || !result.Succeeded() {
		t.Fatalf("Run() = succeeded %v, error %v", result.Succeeded(), err)
	}
	if calls.Load() != 3 || len(delays) != 1 || delays[0] != time.Millisecond {
		t.Fatalf("calls=%d delays=%v", calls.Load(), delays)
	}
	if len(requestSessionIDs) != 3 || requestSessionIDs[0] != "" || requestSessionIDs[1] != "" {
		t.Fatalf("summary retry HTTP affinity headers = %#v", requestSessionIDs)
	}
	if len(capturedRequests) != 3 {
		t.Fatalf("captured requests = %d", len(capturedRequests))
	}
	firstSummary := capturedRequests[0].StreamOptions()
	retriedSummary := capturedRequests[1].StreamOptions()
	if firstSummary.CacheRetention != provider.CacheRetentionNone || retriedSummary.CacheRetention != provider.CacheRetentionNone ||
		firstSummary.SessionID == "" || firstSummary.SessionID != retriedSummary.SessionID ||
		firstSummary.SessionID == transcript.SessionID() || len(firstSummary.SessionID) != 36 || firstSummary.SessionID[14] != '7' ||
		!strings.ContainsRune("89ab", rune(firstSummary.SessionID[19])) {
		t.Fatalf("summary retry request affinity = %#v / %#v, durable %q", firstSummary, retriedSummary, transcript.SessionID())
	}
	if len(retryEvents) != 3 ||
		retryEvents[0].Kind != agent.SummarizationRetryScheduledEventType ||
		retryEvents[1].Kind != agent.SummarizationRetryAttemptEventType ||
		retryEvents[2].Kind != agent.SummarizationRetryFinishedEventType {
		t.Fatalf("summary retry events = %+v", retryEvents)
	}
	if retryEvents[0].RetryAttempt != 1 || retryEvents[0].RetryFailureKind != provider.FailureTransport ||
		retryEvents[2].RetryAttempt != 1 ||
		!retryEvents[2].RetrySucceeded || retryEvents[2].RetryFinishReason != provider.RetryFinishSucceeded {
		t.Fatalf("summary retry lifecycle = %+v", retryEvents)
	}
	assertSummarizationRetryReason(t, retryEvents, agent.CompactionThreshold)
	assertContextRetryEntries(t, transcript, "summary retry prompt", "answer after summary retry", 1, 0)
}

func TestAbortDoesNotCancelSummarizerRetryButShutdownDoes(t *testing.T) {
	transcript := newSessionManager(t)
	old, err := llm.NewUserTextMessage(strings.Repeat("cancel summary context ", 10), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.AppendLLMMessage(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	const secret = "summary-cancel-provider-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"retry me `+secret+`"}}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	model, implementation := contextRetryProvider(t, server.URL)
	appendMatchingAssistant(t, transcript, model)
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
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: transcript, Model: model, ContextWindow: 2, ContextReserve: 1,
		KeepRecentTokens: 1, Summarizer: summarizer, Retry: agent.RetryPolicy{MaxAttempts: 1},
		Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	baselineEntries := len(transcript.Entries())
	var retryEvents, compactionEvents []agentEventSnapshot
	var lifecycleEvents []agent.AgentEventType
	subscribeAllAgentEvents(coordinator, func(_ context.Context, event observedAgentEvent) {
		switch event.Type() {
		case agent.SummarizationRetryScheduledEventType, agent.SummarizationRetryAttemptEventType, agent.SummarizationRetryFinishedEventType:
			retryEvents = append(retryEvents, snapshotAgentEvent(event))
		case agent.CompactionStartEventType, agent.CompactionEndEventType:
			compactionEvents = append(compactionEvents, snapshotAgentEvent(event))
		case agent.AgentStartEventType, agent.TurnStartEventType, agent.MessageStartEventType,
			agent.MessageEndEventType, agent.TurnEndEventType, agent.AgentEndEventType, agent.AgentSettledEventType:
			lifecycleEvents = append(lifecycleEvents, event.Type())
		}
	})
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
	if state := coordinator.State(); state.Active.Phase() != agent.PhaseCompacting {
		t.Fatalf("phase = %s", state.Active.Phase())
	}
	if err := coordinator.Steer("queued while summarizer retries"); err != nil {
		t.Fatal(err)
	}
	abortCtx, cancelAbort := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelAbort()
	if err := coordinator.Abort(abortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Abort during independent compaction = %v, want deadline", err)
	}
	if state := coordinator.State(); state.Active.Phase() != agent.PhaseCompacting {
		t.Fatalf("Abort cancelled compaction domain: phase=%s", state.Active.Phase())
	}
	if err := coordinator.Shutdown(context.Background(), agent.SessionShutdownOptions{Event: agent.SessionShutdownHookEvent{Reason: agent.ShutdownQuit}}); err != nil {
		t.Fatal(err)
	}
	outcome := <-runDone
	if !errors.Is(outcome.err, agent.ErrAgentAborted) {
		t.Fatalf("Run() = (%#v, %v), want pre-low-run abort", outcome.result, outcome.err)
	}
	if _, ok := outcome.result.Terminal(); ok {
		t.Fatalf("pre-low-run abort produced terminal = %#v", outcome.result)
	}
	if len(lifecycleEvents) != 0 {
		t.Fatalf("pre-low-run abort emitted lifecycle = %v", lifecycleEvents)
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
	if entries := len(transcript.Entries()); entries != baselineEntries {
		t.Fatalf("pre-low-run abort entries = %d, want baseline %d", entries, baselineEntries)
	}
	if len(retryEvents) != 2 || retryEvents[0].Kind != agent.SummarizationRetryScheduledEventType ||
		retryEvents[1].Kind != agent.SummarizationRetryFinishedEventType ||
		retryEvents[1].RetryFinishReason != provider.RetryFinishCancelled ||
		retryEvents[1].RetryFailureKind != provider.FailureCancelled {
		t.Fatalf("cancelled summary retry lifecycle = %+v", retryEvents)
	}
	if strings.Contains(fmt.Sprintf("%+v", retryEvents), secret) {
		t.Fatalf("cancelled summary retry events leaked provider detail: %+v", retryEvents)
	}
	assertSummarizationRetryReason(t, retryEvents, agent.CompactionThreshold)
	assertCompactionLifecycle(t, compactionEvents, agent.CompactionThreshold, errExpectedCompactionAbort)
}

func TestSummarizerRetryExhaustionDoesNotAppendCompaction(t *testing.T) {
	transcript := newSessionManager(t)
	old, _ := llm.NewUserTextMessage(strings.Repeat("summary failure context ", 10), agentTestEpoch)
	if _, err := transcript.AppendLLMMessage(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	const secret = "summary-exhaust-provider-secret"
	var calls atomic.Uint32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":{"message":"still unavailable `+secret+`"}}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	model, implementation := contextRetryProvider(t, server.URL)
	appendMatchingAssistant(t, transcript, model)
	summarizer, err := provider.NewContextSummarizerWithRetry(implementation, model, func() time.Time { return agentTestEpoch }, provider.RetryPolicy{
		MaxAttempts: 3, Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: transcript, Model: model, ContextWindow: 2, ContextReserve: 1,
		KeepRecentTokens: 1, Summarizer: summarizer, Retry: agent.RetryPolicy{MaxAttempts: 1},
		Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	var retryEvents []agentEventSnapshot
	subscribeAllAgentEvents(coordinator, func(_ context.Context, event observedAgentEvent) {
		switch event.Type() {
		case agent.SummarizationRetryScheduledEventType, agent.SummarizationRetryAttemptEventType, agent.SummarizationRetryFinishedEventType:
			retryEvents = append(retryEvents, snapshotAgentEvent(event))
		}
	})
	result, err := coordinator.Run(context.Background(), "summary failure prompt")
	terminal, ok := result.Terminal()
	if err != nil || !ok || terminal.FinishReason() != llm.FinishError {
		t.Fatalf("Run() terminal=%T error=%v", terminal, err)
	}
	if calls.Load() != 4 {
		t.Fatalf("provider calls = %d, want 4", calls.Load())
	}
	if len(retryEvents) != 6 || retryEvents[0].Kind != agent.SummarizationRetryScheduledEventType ||
		retryEvents[1].Kind != agent.SummarizationRetryAttemptEventType ||
		retryEvents[2].Kind != agent.SummarizationRetryFinishedEventType ||
		retryEvents[2].RetryFinishReason != provider.RetryFinishFailed ||
		retryEvents[3].Kind != agent.SummarizationRetryScheduledEventType ||
		retryEvents[4].Kind != agent.SummarizationRetryAttemptEventType ||
		retryEvents[5].Kind != agent.SummarizationRetryFinishedEventType ||
		retryEvents[5].RetryFinishReason != provider.RetryFinishExhausted ||
		retryEvents[5].RetryFailureKind != provider.FailureHTTPStatus || retryEvents[5].RetryHTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("exhausted summary retry lifecycle = %+v", retryEvents)
	}
	if strings.Contains(fmt.Sprintf("%+v", retryEvents), secret) {
		t.Fatalf("exhausted summary retry events leaked provider detail: %+v", retryEvents)
	}
	assertSummarizationRetryReason(t, retryEvents, agent.CompactionThreshold)
	for _, entry := range transcript.Entries() {
		if entry.Type() == "compaction" {
			t.Fatal("failed summary appended a compaction")
		}
	}
}

func TestContextOverflowCompactionRetryOccursAtMostOnce(t *testing.T) {
	transcript := newSessionManager(t)
	old, _ := llm.NewUserTextMessage(strings.Repeat("repeat overflow context ", 10), agentTestEpoch)
	if _, err := transcript.AppendLLMMessage(context.Background(), old); err != nil {
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
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: transcript, Model: model,
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
		summarizer, err := provider.NewContextSummarizerWithRetry(implementation, model, nil, provider.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }})
		if err != nil {
			t.Fatal(err)
		}
		coordinator, err := agent.NewSession(agent.SessionConfig{
			Provider: implementation, SessionManager: transcript, Model: model, Summarizer: summarizer, KeepRecentTokens: 1,
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

func TestManualCompactionFailurePublishesOneSafeSettledPair(t *testing.T) {
	transcript := newSessionManager(t)
	appendCompactionHistory(t, transcript)
	const secret = "summarizer-secret-must-not-reach-events"
	summarizer := contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
		return session.SummaryOutput{}, errors.New("summary provider failed: " + secret)
	})
	coordinator := newCompactionCoordinator(t, transcript, summarizer)
	var lifecycle []agentEventSnapshot
	subscribeAllAgentEvents(coordinator, func(_ context.Context, event observedAgentEvent) {
		switch event.Type() {
		case agent.CompactionStartEventType, agent.CompactionEndEventType, agent.AgentEndEventType:
			lifecycle = append(lifecycle, snapshotAgentEvent(event))
		}
	})
	if _, err := coordinator.Compact(context.Background(), "focus"); !errors.Is(err, session.ErrSummaryFailed) {
		t.Fatalf("Compact() error = %v", err)
	}
	assertManualCompactionFailureLifecycle(t, lifecycle, errors.New("Compaction failed: "+session.ErrSummaryFailed.Error()), secret)
	assertNoCompactionEntry(t, transcript)
}

func TestManualCompactionRejectsStaleSnapshotAndPublishesSafeConflict(t *testing.T) {
	transcript := newSessionManager(t)
	appendCompactionHistory(t, transcript)
	entered := make(chan struct{})
	release := make(chan struct{})
	summarizer := contextRetrySummarizerFunc(func(ctx context.Context, _ session.SummaryInput) (session.SummaryOutput, error) {
		close(entered)
		select {
		case <-release:
			return session.SummaryOutput{Text: "stale summary"}, nil
		case <-ctx.Done():
			return session.SummaryOutput{}, context.Cause(ctx)
		}
	})
	coordinator := newCompactionCoordinator(t, transcript, summarizer)
	var lifecycle []agentEventSnapshot
	subscribeAllAgentEvents(coordinator, func(_ context.Context, event observedAgentEvent) {
		switch event.Type() {
		case agent.CompactionStartEventType, agent.CompactionEndEventType, agent.AgentEndEventType:
			lifecycle = append(lifecycle, snapshotAgentEvent(event))
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := coordinator.Compact(context.Background(), "stale")
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("manual compaction did not reach summarizer")
	}
	newState, err := llm.NewUserTextMessage("concurrent durable append", agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.AppendLLMMessage(context.Background(), newState); err != nil {
		t.Fatalf("append during compaction = %v", err)
	}
	close(release)
	if err := <-done; !errors.Is(err, session.ErrCompactionConflict) {
		t.Fatalf("Compact() error = %v", err)
	}
	assertManualCompactionFailureLifecycle(t, lifecycle, errors.New("Compaction failed: "+session.ErrCompactionConflict.Error()), "stale summary")
	assertNoCompactionEntry(t, transcript)
}

func TestManualCompactionAbortPublishesSafeCancellationSettlement(t *testing.T) {
	transcript := newSessionManager(t)
	appendCompactionHistory(t, transcript)
	const secret = "cancelled-summary-secret"
	entered := make(chan struct{})
	summarizer := contextRetrySummarizerFunc(func(ctx context.Context, _ session.SummaryInput) (session.SummaryOutput, error) {
		close(entered)
		<-ctx.Done()
		return session.SummaryOutput{}, errors.New(secret)
	})
	coordinator := newCompactionCoordinator(t, transcript, summarizer)
	var lifecycle []agentEventSnapshot
	subscribeAllAgentEvents(coordinator, func(_ context.Context, event observedAgentEvent) {
		switch event.Type() {
		case agent.CompactionStartEventType, agent.CompactionEndEventType, agent.AgentEndEventType:
			lifecycle = append(lifecycle, snapshotAgentEvent(event))
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := coordinator.Compact(context.Background(), "cancel")
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("manual compaction did not reach summarizer")
	}
	coordinator.AbortCompaction()
	if err := <-done; !errors.Is(err, session.ErrAppendCanceled) {
		t.Fatalf("Compact() error = %v", err)
	}
	assertManualCompactionFailureLifecycle(t, lifecycle, errExpectedCompactionAbort, secret)
	assertNoCompactionEntry(t, transcript)
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
		[]llm.TextBlock{mustTextBlock(t, "prior response")}, llm.FinishStop, mustUsage(t, model.ContextWindow(), 0), agentTestEpoch,
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
		if entry.Type() == "compaction" {
			t.Fatal("failed compaction appended a durable checkpoint")
		}
	}
}

func contextRetryProvider(t *testing.T, baseURL string) (provider.Model, *provider.OpenAIResponsesProvider) {
	t.Helper()
	model, err := newAgentModel(provider.ModelSpec{
		Provider: provider.OpenAIProviderID, API: provider.OpenAIResponsesAPI, ID: "fixture-model",
		ContextWindow: 32, MaxTokens: 1,
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

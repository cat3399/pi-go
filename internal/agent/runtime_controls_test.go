package agent_test

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

func TestAgentSessionQueueModeControlsChangeRealConsumption(t *testing.T) {
	tests := []struct {
		name string
		mode agent.QueueMode
		want int
	}{
		{name: "all", mode: agent.QueueAll, want: 1},
		{name: "one-at-a-time", mode: agent.QueueOneAtATime, want: 2},
	}
	for _, test := range tests {
		t.Run("steering/"+test.name, func(t *testing.T) {
			implementation := newScriptedProvider(t,
				mustTextTerminal(t, "first"), mustTextTerminal(t, "second"), mustTextTerminal(t, "third"),
			)
			coordinator, err := agent.NewSession(agent.SessionConfig{
				Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
				ThinkingLevel: provider.ThinkingOff,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := coordinator.SetSteeringMode(test.mode); err != nil {
				t.Fatal(err)
			}
			if err := coordinator.Steer("queued one"); err != nil {
				t.Fatal(err)
			}
			if err := coordinator.Steer("queued two"); err != nil {
				t.Fatal(err)
			}
			if _, err := coordinator.Prompt(context.Background(), "start"); err != nil {
				t.Fatal(err)
			}
			if got := len(implementation.Requests()); got != test.want {
				t.Fatalf("provider calls = %d, want %d", got, test.want)
			}
			steering, _ := coordinator.Queues()
			if len(steering) != 0 {
				t.Fatalf("remaining steering = %d", len(steering))
			}
		})
		t.Run("follow-up/"+test.name, func(t *testing.T) {
			implementation := newScriptedProvider(t,
				mustTextTerminal(t, "first"), mustTextTerminal(t, "second"), mustTextTerminal(t, "third"),
			)
			coordinator, err := agent.NewSession(agent.SessionConfig{
				Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
				ThinkingLevel: provider.ThinkingOff,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := coordinator.SetFollowUpMode(test.mode); err != nil {
				t.Fatal(err)
			}
			if err := coordinator.FollowUp("queued one"); err != nil {
				t.Fatal(err)
			}
			if err := coordinator.FollowUp("queued two"); err != nil {
				t.Fatal(err)
			}
			if _, err := coordinator.Prompt(context.Background(), "start"); err != nil {
				t.Fatal(err)
			}
			want := test.want + 1
			if got := len(implementation.Requests()); got != want {
				t.Fatalf("provider calls = %d, want %d", got, want)
			}
			_, followUp := coordinator.Queues()
			if len(followUp) != 0 {
				t.Fatalf("remaining follow-up = %d", len(followUp))
			}
		})
	}
}

func TestAgentSessionRetryRuntimeGateRestoresConfiguredBudget(t *testing.T) {
	disabled := false
	implementation := newScriptedProvider(t,
		sessionHTTPFailure(t, 429),
		sessionHTTPFailure(t, 429), mustTextTerminal(t, "recovered"),
	)
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		ThinkingLevel: provider.ThinkingOff, AutoRetryEnabled: &disabled,
		Retry: agent.RetryPolicy{MaxAttempts: 3, Sleep: func(context.Context, time.Duration) error { return nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ends []agent.SessionAgentEndEvent
	coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if ended, ok := event.(agent.SessionAgentEndEvent); ok {
			ends = append(ends, ended)
		}
	})
	if result, err := coordinator.Prompt(context.Background(), "disabled"); err != nil || result.Succeeded() {
		t.Fatalf("disabled run = %#v, %v", result, err)
	}
	if len(implementation.Requests()) != 1 || len(ends) != 1 || ends[0].WillRetry {
		t.Fatalf("disabled retry calls/events = %d/%#v", len(implementation.Requests()), ends)
	}
	if err := coordinator.SetAutoRetryEnabled(true); err != nil {
		t.Fatal(err)
	}
	if !coordinator.AutoRetryEnabled() {
		t.Fatal("retry did not enable")
	}
	if result, err := coordinator.Prompt(context.Background(), "enabled"); err != nil || !result.Succeeded() {
		t.Fatalf("enabled run = %#v, %v", result, err)
	}
	if len(implementation.Requests()) != 3 {
		t.Fatalf("disabled-to-enabled calls = %d, want 3", len(implementation.Requests()))
	}
	if len(ends) < 3 || !ends[1].WillRetry || ends[2].WillRetry {
		t.Fatalf("enabled retry agent_end events = %#v", ends)
	}
}

func TestAgentSessionRetryReadsRuntimeSettingsAfterEachFailure(t *testing.T) {
	enabled := true
	policy := agent.RetryPolicy{MaxAttempts: 3, InitialDelay: time.Millisecond}
	policy.Sleep = func(context.Context, time.Duration) error {
		enabled = false
		return nil
	}
	implementation := newScriptedProvider(t,
		sessionHTTPFailure(t, 429), sessionHTTPFailure(t, 429), mustTextTerminal(t, "must not run"),
	)
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		ThinkingLevel: provider.ThinkingOff, Retry: policy,
		ResolveRuntimeSettings: func() agent.RuntimeControlSettings {
			return agent.RuntimeControlSettings{
				SteeringMode: agent.QueueOneAtATime, FollowUpMode: agent.QueueOneAtATime,
				AutoCompactionEnabled: true, AutoRetryEnabled: enabled, Retry: policy,
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ends []agent.SessionAgentEndEvent
	coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if ended, ok := event.(agent.SessionAgentEndEvent); ok {
			ends = append(ends, ended)
		}
	})
	if result, err := coordinator.Prompt(context.Background(), "settings change during retry series"); err != nil || result.Succeeded() {
		t.Fatalf("run = %#v, %v", result, err)
	}
	if len(implementation.Requests()) != 2 {
		t.Fatalf("provider calls = %d, want initial + one retry", len(implementation.Requests()))
	}
	if len(ends) != 2 || !ends[0].WillRetry || ends[1].WillRetry {
		t.Fatalf("agent_end retry decisions = %#v", ends)
	}
}

func TestAbortRetryCancelsOnlySleepAndEmitsExactCleanup(t *testing.T) {
	entered := make(chan struct{})
	implementation := newScriptedProvider(t, sessionHTTPFailure(t, 429), mustTextTerminal(t, "next run"))
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		ThinkingLevel: provider.ThinkingOff,
		Retry: agent.RetryPolicy{MaxAttempts: 2, InitialDelay: time.Hour, Sleep: func(ctx context.Context, _ time.Duration) error {
			close(entered)
			<-ctx.Done()
			return context.Cause(ctx)
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var eventsMu sync.Mutex
	var retryEnds []agent.AutoRetryEndEvent
	coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if ended, ok := event.(agent.AutoRetryEndEvent); ok {
			eventsMu.Lock()
			retryEnds = append(retryEnds, ended)
			eventsMu.Unlock()
		}
	})
	type outcome struct {
		result agent.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, runErr := coordinator.Prompt(context.Background(), "cancel retry only")
		done <- outcome{result: result, err: runErr}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("retry sleep did not start")
	}
	if !coordinator.IsRetrying() {
		t.Fatal("IsRetrying false during retry sleep")
	}
	if err := coordinator.SetAutoRetryEnabled(false); err != nil {
		t.Fatal(err)
	}
	if !coordinator.IsRetrying() {
		t.Fatal("disabling retry implicitly interrupted active sleep")
	}
	coordinator.AbortRetry()
	coordinator.AbortRetry()
	got := <-done
	if got.err != nil {
		t.Fatalf("run error = %v", got.err)
	}
	if coordinator.IsRetrying() {
		t.Fatal("retry still active after cancellation")
	}
	eventsMu.Lock()
	ends := append([]agent.AutoRetryEndEvent(nil), retryEnds...)
	eventsMu.Unlock()
	if len(ends) != 1 || ends[0].Success || ends[0].Attempt != 1 || ends[0].FinalError != "Retry cancelled" {
		t.Fatalf("retry cleanup events = %#v", ends)
	}
	if err := coordinator.SetAutoRetryEnabled(true); err != nil {
		t.Fatal(err)
	}
	if result, err := coordinator.Prompt(context.Background(), "run remains usable"); err != nil || !result.Succeeded() {
		t.Fatalf("post-cancel run = %#v, %v", result, err)
	}
}

func TestAbortDuringRetryDrainsFollowUpBeforeReturning(t *testing.T) {
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return agentTestEpoch }})
	if err != nil {
		t.Fatal(err)
	}
	failure, err := provider.FixedResponseStep(sessionHTTPFailure(t, 429))
	if err != nil {
		t.Fatal(err)
	}
	continued := make(chan struct{})
	releaseContinuation := make(chan struct{})
	success, err := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		close(continued)
		select {
		case <-releaseContinuation:
			return mustTextTerminal(t, "continued"), nil
		case <-ctx.Done():
			return mustTextTerminal(t, "cancelled"), context.Cause(ctx)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.SetResponses([]provider.ScriptStep{failure, success}); err != nil {
		t.Fatal(err)
	}
	retrySleep := make(chan struct{})
	model := sessionTestModel(t)
	transcript := newSessionManager(t)
	appendMatchingAssistant(t, transcript, model)
	tail, err := llm.NewUserTextMessage("tail keeps pre-prompt compaction deferred", agentTestEpoch.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.AppendLLMMessage(context.Background(), tail); err != nil {
		t.Fatal(err)
	}
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: transcript, Model: model,
		ContextWindow: 2, ContextReserve: 1, KeepRecentTokens: 1,
		Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
			return session.SummaryOutput{Text: "summary after retry cancellation"}, nil
		}),
		Retry: agent.RetryPolicy{MaxAttempts: 2, InitialDelay: time.Hour, Sleep: func(ctx context.Context, _ time.Duration) error {
			close(retrySleep)
			<-ctx.Done()
			return context.Cause(ctx)
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var eventsMu sync.Mutex
	recording := false
	var sequence []string
	var retryEnd agent.AutoRetryEndEvent
	coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		switch value := event.(type) {
		case agent.AutoRetryEndEvent:
			retryEnd = value
			recording = true
			sequence = append(sequence, "auto_retry_end")
		case agent.AgentStartEvent:
			if recording {
				sequence = append(sequence, "agent_start")
			}
		case agent.TurnStartEvent:
			if recording {
				sequence = append(sequence, "turn_start")
			}
		case agent.SessionQueueUpdateEvent:
			if recording && len(value.FollowUpMessages) == 0 {
				sequence = append(sequence, "queue_update")
			}
		case agent.MessageStartEvent:
			if recording && value.Message.Role() == agentmsg.RoleUser && messageText(t, value.Message.(agentmsg.LLM).Conversation()) == "queued follow-up" {
				sequence = append(sequence, "message_start")
			}
		case agent.SessionAgentEndEvent:
			if recording {
				sequence = append(sequence, "agent_end")
			}
		case agent.AgentSettledEvent:
			if recording {
				sequence = append(sequence, "agent_settled")
			}
		case agent.CompactionStartEvent:
			if recording {
				sequence = append(sequence, "compaction_start")
			}
		case agent.CompactionEndEvent:
			if recording {
				sequence = append(sequence, "compaction_end")
			}
		}
	})
	type outcome struct {
		result agent.Result
		err    error
	}
	runDone := make(chan outcome, 1)
	go func() {
		result, runErr := coordinator.Prompt(context.Background(), "initial")
		runDone <- outcome{result: result, err: runErr}
	}()
	<-retrySleep
	if err := coordinator.FollowUp("queued follow-up"); err != nil {
		t.Fatal(err)
	}
	abortDone := make(chan error, 1)
	go func() { abortDone <- coordinator.Abort(context.Background()) }()
	<-continued
	select {
	case err := <-abortDone:
		t.Fatalf("Abort returned before queued continuation settled: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseContinuation)
	if err := <-abortDone; err != nil {
		t.Fatal(err)
	}
	got := <-runDone
	if got.err != nil || !got.result.Succeeded() {
		t.Fatalf("Prompt = (%#v, %v)", got.result, got.err)
	}
	_, followUp := coordinator.Queues()
	if len(followUp) != 0 || implementation.CallCount() != 2 {
		t.Fatalf("stale follow-up after abort: queue=%#v calls=%d", followUp, implementation.CallCount())
	}
	eventsMu.Lock()
	gotSequence := append([]string(nil), sequence...)
	gotRetryEnd := retryEnd
	eventsMu.Unlock()
	want := []string{"auto_retry_end", "compaction_start", "compaction_end", "agent_start", "turn_start", "queue_update", "message_start", "agent_end", "agent_settled"}
	if !reflect.DeepEqual(gotSequence, want) {
		t.Fatalf("post-retry cancellation sequence = %v, want %v", gotSequence, want)
	}
	if gotRetryEnd.Success || gotRetryEnd.FinalError != "Retry cancelled" {
		t.Fatalf("retry cleanup = %#v", gotRetryEnd)
	}
}

func TestAutoCompactionControlGatesAutomaticButNotManualCompaction(t *testing.T) {
	model, err := newAgentModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "compact-control", ContextWindow: 100, MaxTokens: 99,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("runtime toggle", func(t *testing.T) {
		transcript := newSessionManager(t)
		old, _ := llm.NewUserTextMessage("old context that exceeds threshold", time.UnixMilli(1))
		if _, err := transcript.AppendLLMMessage(context.Background(), old); err != nil {
			t.Fatal(err)
		}
		calls := 0
		coordinator, err := agent.NewSession(agent.SessionConfig{
			Provider:       newScriptedProvider(t, mustTextTerminal(t, "first"), mustTextTerminal(t, "second")),
			SessionManager: transcript, Model: model, ContextWindow: 100, ContextReserve: 99, KeepRecentTokens: 1,
			Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
				calls++
				return session.SummaryOutput{Text: "automatic summary"}, nil
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := coordinator.SetAutoCompactionEnabled(false); err != nil {
			t.Fatal(err)
		}
		if _, err := coordinator.Prompt(context.Background(), "disabled"); err != nil {
			t.Fatal(err)
		}
		if calls != 0 {
			t.Fatalf("disabled automatic compaction calls = %d", calls)
		}
		if err := coordinator.SetAutoCompactionEnabled(true); err != nil {
			t.Fatal(err)
		}
		if _, err := coordinator.Prompt(context.Background(), "enabled"); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatalf("enabled automatic compaction calls = %d", calls)
		}
	})

	t.Run("manual ignores gate", func(t *testing.T) {
		transcript := newSessionManager(t)
		for _, text := range []string{"old", "recent"} {
			message, _ := llm.NewUserTextMessage(text, agentTestEpoch)
			if _, err := transcript.AppendLLMMessage(context.Background(), message); err != nil {
				t.Fatal(err)
			}
		}
		calls := 0
		coordinator, err := agent.NewSession(agent.SessionConfig{
			Provider: newScriptedProvider(t), SessionManager: transcript, Model: model, KeepRecentTokens: 1,
			Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
				calls++
				return session.SummaryOutput{Text: "manual summary"}, nil
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := coordinator.SetAutoCompactionEnabled(false); err != nil {
			t.Fatal(err)
		}
		if result, err := coordinator.Compact(context.Background(), "manual"); err != nil || !result.Committed {
			t.Fatalf("manual compact = %#v, %v", result, err)
		}
		if calls != 1 {
			t.Fatalf("manual compaction calls = %d", calls)
		}
	})
}

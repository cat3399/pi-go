package agent_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
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

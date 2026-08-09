package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestAgentSessionSettledIsIdleAndAcceptsImmediatePrompt(t *testing.T) {
	implementation := newScriptedProvider(t, mustTextTerminal(t, "first"), mustTextTerminal(t, "second"))
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	settlements := 0
	var nested agent.Result
	var nestedErr error
	coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if _, ok := event.(agent.AgentSettledEvent); !ok {
			return
		}
		settlements++
		state := coordinator.State()
		if state.Active.Phase() != agent.PhaseIdle || state.Active.IsStreaming() {
			t.Errorf("settled callback state = %s streaming=%t", state.Active.Phase(), state.Active.IsStreaming())
		}
		if waitErr := coordinator.WaitForIdle(context.Background()); waitErr != nil {
			t.Errorf("WaitForIdle from settled callback = %v", waitErr)
		}
		if settlements == 1 {
			nested, nestedErr = coordinator.Run(context.Background(), "from settled")
		}
	})
	outer, err := coordinator.Run(context.Background(), "initial")
	if err != nil || !outer.Succeeded() {
		t.Fatalf("outer Run = (%#v, %v)", outer, err)
	}
	if nestedErr != nil || !nested.Succeeded() || settlements != 2 || implementation.CallCount() != 2 {
		t.Fatalf("nested Run = (%#v, %v), settlements=%d calls=%d", nested, nestedErr, settlements, implementation.CallCount())
	}
}

func TestAgentSessionIdleGenerationIncludesPromptStartedBySettledObserver(t *testing.T) {
	for _, test := range []struct {
		name string
		wait func(*agent.AgentSession) error
	}{
		{name: "WaitForIdle", wait: func(session *agent.AgentSession) error { return session.WaitForIdle(context.Background()) }},
		{name: "Abort", wait: func(session *agent.AgentSession) error { return session.Abort(context.Background()) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return agentTestEpoch }})
			if err != nil {
				t.Fatal(err)
			}
			enteredA := make(chan struct{})
			releaseA := make(chan struct{})
			enteredB := make(chan struct{})
			releaseB := make(chan struct{})
			first, err := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
				close(enteredA)
				select {
				case <-releaseA:
					return mustTextTerminal(t, "first"), nil
				case <-ctx.Done():
					return mustTextTerminal(t, "cancelled"), context.Cause(ctx)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			second, err := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
				close(enteredB)
				select {
				case <-releaseB:
					return mustTextTerminal(t, "second"), nil
				case <-ctx.Done():
					return mustTextTerminal(t, "cancelled"), context.Cause(ctx)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := implementation.SetResponses([]provider.ScriptStep{first, second}); err != nil {
				t.Fatal(err)
			}
			coordinator, err := agent.NewSession(agent.SessionConfig{
				Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
			})
			if err != nil {
				t.Fatal(err)
			}
			var settlements atomic.Int32
			var nestedErr error
			coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
				if _, ok := event.(agent.AgentSettledEvent); !ok || settlements.Add(1) != 1 {
					return
				}
				_, nestedErr = coordinator.Prompt(context.Background(), "prompt B")
			})
			outerDone := make(chan error, 1)
			go func() {
				_, runErr := coordinator.Prompt(context.Background(), "prompt A")
				outerDone <- runErr
			}()
			<-enteredA
			waitStarted := make(chan struct{})
			waitDone := make(chan error, 1)
			go func() {
				close(waitStarted)
				waitDone <- test.wait(coordinator)
			}()
			<-waitStarted
			select {
			case err := <-waitDone:
				t.Fatalf("wait returned while A was active: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			close(releaseA)
			<-enteredB
			select {
			case err := <-waitDone:
				t.Fatalf("old run completion released wait while B was active: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			close(releaseB)
			if err := <-waitDone; err != nil {
				t.Fatal(err)
			}
			if err := <-outerDone; err != nil || nestedErr != nil {
				t.Fatalf("outer/nested errors = %v / %v", err, nestedErr)
			}
			if state := coordinator.State(); state.Active.Phase() != agent.PhaseIdle {
				t.Fatalf("final phase = %s", state.Active.Phase())
			}
		})
	}
}

func TestAgentSessionShutdownPreventsSettledPromptAndIsIdempotent(t *testing.T) {
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return agentTestEpoch }})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	step, err := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		close(entered)
		<-ctx.Done()
		return mustTextTerminal(t, "cancelled"), context.Cause(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.SetResponses([]provider.ScriptStep{step}); err != nil {
		t.Fatal(err)
	}
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	var nestedErr error
	coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if _, ok := event.(agent.AgentSettledEvent); ok {
			_, nestedErr = coordinator.Prompt(context.Background(), "must be rejected")
		}
	})
	runDone := make(chan error, 1)
	go func() {
		_, runErr := coordinator.Prompt(context.Background(), "active")
		runDone <- runErr
	}()
	<-entered
	if err := coordinator.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-runDone; err != nil {
		t.Fatalf("active prompt returned %v", err)
	}
	if !errors.Is(nestedErr, agent.ErrInvalidRun) {
		t.Fatalf("settled prompt during shutdown = %v", nestedErr)
	}
	if err := coordinator.Close(context.Background()); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

func TestAgentSessionInitialTurnRefreshFailureClosesStartedLifecycle(t *testing.T) {
	resolveErr := errors.New("credentials refresh failed")
	transcript := newSessionManager(t)
	implementation := newScriptedProvider(t, mustTextTerminal(t, "must not run"))
	var sequence []agent.AgentEventType
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: transcript, Model: sessionTestModel(t),
		ResolveStreamOptions: func(context.Context, provider.Model) (provider.StreamOptions, error) {
			wantPrefix := []agent.AgentEventType{
				agent.AgentStartEventType, agent.TurnStartEventType,
				agent.MessageStartEventType, agent.MessageEndEventType,
			}
			if !reflect.DeepEqual(sequence, wantPrefix) {
				t.Errorf("lifecycle before initial refresh = %v, want %v", sequence, wantPrefix)
			}
			return provider.StreamOptions{}, resolveErr
		},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		sequence = append(sequence, event.Type())
	})
	result, err := coordinator.Run(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Run error = %v", err)
	}
	terminal, ok := result.Terminal()
	failure, failureOK := terminal.(llm.AssistantFailureMessage)
	if !ok || !failureOK || !errors.Is(failure.Failure().Cause(), resolveErr) {
		t.Fatalf("Run terminal = %#v", terminal)
	}
	want := []agent.AgentEventType{
		agent.AgentStartEventType, agent.TurnStartEventType,
		agent.MessageStartEventType, agent.MessageEndEventType,
		agent.MessageStartEventType, agent.MessageEndEventType,
		agent.TurnEndEventType, agent.AgentEndEventType, agent.AgentSettledEventType,
	}
	if !reflect.DeepEqual(sequence, want) {
		t.Fatalf("lifecycle = %v, want %v", sequence, want)
	}
	if implementation.CallCount() != 0 {
		t.Fatalf("provider calls = %d", implementation.CallCount())
	}
	messages := transcript.BuildContext().Messages()
	if len(messages) != 2 || messages[0].Role() != llm.RoleUser || messages[1].Role() != llm.RoleAssistant {
		t.Fatalf("durable messages = %#v", messages)
	}
}

func TestAgentSessionLaterTurnRefreshFailureStartsTurnAndDrainsSteering(t *testing.T) {
	resolveErr := errors.New("second credentials refresh failed")
	providerEntered := make(chan struct{})
	providerRelease := make(chan struct{})
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return agentTestEpoch }})
	if err != nil {
		t.Fatal(err)
	}
	first, err := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		close(providerEntered)
		select {
		case <-providerRelease:
			return mustTextTerminal(t, "first"), nil
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	unused, err := provider.FixedResponseStep(mustTextTerminal(t, "must not run"))
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.SetResponses([]provider.ScriptStep{first, unused}); err != nil {
		t.Fatal(err)
	}

	transcript := newSessionManager(t)
	var sequenceMu sync.Mutex
	var sequence []string
	snapshotSequence := func() []string {
		sequenceMu.Lock()
		defer sequenceMu.Unlock()
		return append([]string(nil), sequence...)
	}
	var resolves atomic.Int32
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: transcript, Model: sessionTestModel(t),
		ResolveStreamOptions: func(context.Context, provider.Model) (provider.StreamOptions, error) {
			if resolves.Add(1) == 1 {
				return provider.StreamOptions{}, nil
			}
			want := []string{"turn_start:2", "queue:", "message_start:user:queued", "message_end:user:queued"}
			if got := snapshotSequence(); !reflect.DeepEqual(got, want) {
				t.Errorf("lifecycle before second refresh = %v, want %v", got, want)
			}
			return provider.StreamOptions{}, resolveErr
		},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if label, ok := laterTurnLifecycleLabel(t, event); ok {
			sequenceMu.Lock()
			sequence = append(sequence, label)
			sequenceMu.Unlock()
		}
	})
	type outcome struct {
		result agent.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, runErr := coordinator.Run(context.Background(), "initial")
		done <- outcome{result: result, err: runErr}
	}()
	<-providerEntered
	if err := coordinator.Steer("queued"); err != nil {
		t.Fatal(err)
	}
	close(providerRelease)
	out := <-done
	if out.err != nil {
		t.Fatalf("Run error = %v", out.err)
	}
	terminal, ok := out.result.Terminal()
	failure, failureOK := terminal.(llm.AssistantFailureMessage)
	if !ok || !failureOK || failure.FinishReason() != llm.FinishError || !errors.Is(failure.Failure().Cause(), resolveErr) {
		t.Fatalf("Run terminal = %#v", terminal)
	}
	want := []string{
		"turn_start:2", "queue:", "message_start:user:queued", "message_end:user:queued",
		"message_start:assistant", "message_end:assistant", "turn_end:2", "agent_end", "agent_settled",
	}
	if got := snapshotSequence(); !reflect.DeepEqual(got, want) {
		t.Fatalf("second-turn lifecycle = %v, want %v", got, want)
	}
	if implementation.CallCount() != 1 || resolves.Load() != 2 {
		t.Fatalf("provider/resolve calls = %d/%d, want 1/2", implementation.CallCount(), resolves.Load())
	}
	steering, followUp := coordinator.Queues()
	if len(steering) != 0 || len(followUp) != 0 {
		t.Fatalf("queues after failed second refresh = %d/%d", len(steering), len(followUp))
	}
	assertLaterTurnDurableMessages(t, transcript.BuildContext().Messages(), llm.FinishError)
}

func TestAgentSessionShutdownDuringLaterTurnRefreshSettlesStartedTurn(t *testing.T) {
	providerEntered := make(chan struct{})
	providerRelease := make(chan struct{})
	refreshEntered := make(chan struct{})
	refreshCancelled := make(chan struct{})
	refreshRelease := make(chan struct{})
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return agentTestEpoch }})
	if err != nil {
		t.Fatal(err)
	}
	first, err := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		close(providerEntered)
		select {
		case <-providerRelease:
			return mustTextTerminal(t, "first"), nil
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.SetResponses([]provider.ScriptStep{first}); err != nil {
		t.Fatal(err)
	}

	transcript := newSessionManager(t)
	var sequenceMu sync.Mutex
	var sequence []string
	var resolves atomic.Int32
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: transcript, Model: sessionTestModel(t),
		ResolveStreamOptions: func(ctx context.Context, _ provider.Model) (provider.StreamOptions, error) {
			if resolves.Add(1) == 1 {
				return provider.StreamOptions{}, nil
			}
			close(refreshEntered)
			<-ctx.Done()
			close(refreshCancelled)
			<-refreshRelease
			return provider.StreamOptions{}, nil
		},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if label, ok := laterTurnLifecycleLabel(t, event); ok {
			sequenceMu.Lock()
			sequence = append(sequence, label)
			sequenceMu.Unlock()
		}
	})
	type outcome struct {
		result agent.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, runErr := coordinator.Run(context.Background(), "initial")
		done <- outcome{result: result, err: runErr}
	}()
	<-providerEntered
	if err := coordinator.Steer("queued"); err != nil {
		t.Fatal(err)
	}
	close(providerRelease)
	<-refreshEntered
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- coordinator.Shutdown(context.Background(), agent.SessionShutdownOptions{
			Event: agent.SessionShutdownHookEvent{Reason: agent.ShutdownQuit},
		})
	}()
	<-refreshCancelled
	close(refreshRelease)
	out := <-done
	if out.err != nil {
		t.Fatalf("Run error = %v", out.err)
	}
	terminal, ok := out.result.Terminal()
	failure, failureOK := terminal.(llm.AssistantFailureMessage)
	if !ok || !failureOK || failure.FinishReason() != llm.FinishAborted || !errors.Is(failure.Failure().Cause(), agent.ErrAgentAborted) {
		t.Fatalf("Run terminal = %#v", terminal)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
	want := []string{
		"turn_start:2", "queue:", "message_start:user:queued", "message_end:user:queued",
		"message_start:assistant", "message_end:assistant", "turn_end:2", "agent_end", "agent_settled",
	}
	sequenceMu.Lock()
	got := append([]string(nil), sequence...)
	sequenceMu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("second-turn shutdown lifecycle = %v, want %v", got, want)
	}
	// Pi enters the provider one final time with the already-aborted signal so
	// the stream produces the canonical aborted assistant message for this
	// started turn instead of synthesizing one above the provider boundary.
	if implementation.CallCount() != 2 || resolves.Load() != 2 {
		t.Fatalf("provider/resolve calls = %d/%d, want 2/2", implementation.CallCount(), resolves.Load())
	}
	steering, followUp := coordinator.Queues()
	if len(steering) != 0 || len(followUp) != 0 {
		t.Fatalf("queues after second-refresh shutdown = %d/%d", len(steering), len(followUp))
	}
	assertLaterTurnDurableMessages(t, transcript.BuildContext().Messages(), llm.FinishAborted)
}

func TestAgentSessionLaterTurnRefreshRunsAfterQueuedDeliveryAndFeedsNextContext(t *testing.T) {
	providerEntered := make(chan struct{})
	providerRelease := make(chan struct{})
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return agentTestEpoch }})
	if err != nil {
		t.Fatal(err)
	}
	first, err := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		close(providerEntered)
		select {
		case <-providerRelease:
			return mustTextTerminal(t, "first"), nil
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.FixedResponseStep(mustTextTerminal(t, "second"))
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.SetResponses([]provider.ScriptStep{first, second}); err != nil {
		t.Fatal(err)
	}

	var sequenceMu sync.Mutex
	var sequence []string
	var resolves atomic.Int32
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		ResolveStreamOptions: func(context.Context, provider.Model) (provider.StreamOptions, error) {
			if resolves.Add(1) == 1 {
				return provider.StreamOptions{}, nil
			}
			sequenceMu.Lock()
			got := append([]string(nil), sequence...)
			sequenceMu.Unlock()
			want := []string{"turn_start:2", "queue:", "message_start:user:queued", "message_end:user:queued"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("lifecycle before successful second refresh = %v, want %v", got, want)
			}
			return provider.StreamOptions{Headers: map[string]string{"x-turn": "second"}}, nil
		},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if label, ok := laterTurnLifecycleLabel(t, event); ok {
			sequenceMu.Lock()
			sequence = append(sequence, label)
			sequenceMu.Unlock()
		}
	})
	type outcome struct {
		result agent.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, runErr := coordinator.Run(context.Background(), "initial")
		done <- outcome{result: result, err: runErr}
	}()
	<-providerEntered
	if err := coordinator.Steer("queued"); err != nil {
		t.Fatal(err)
	}
	close(providerRelease)
	out := <-done
	if out.err != nil || !out.result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", out.result, out.err)
	}
	requests := implementation.Requests()
	if len(requests) != 2 || resolves.Load() != 2 {
		t.Fatalf("provider/resolve calls = %d/%d, want 2/2", len(requests), resolves.Load())
	}
	if requests[1].StreamOptions().Headers["x-turn"] != "second" {
		t.Fatalf("second request headers = %#v", requests[1].StreamOptions().Headers)
	}
	queued := 0
	for _, message := range requests[1].Messages() {
		if message.Role() == llm.RoleUser && messageText(t, message) == "queued" {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("queued messages in second provider context = %d, want 1; context=%#v", queued, requests[1].Messages())
	}
}

func laterTurnLifecycleLabel(t *testing.T, event agent.SessionEvent) (string, bool) {
	t.Helper()
	switch value := event.(type) {
	case agent.TurnStartEvent:
		if value.Turn == 2 {
			return "turn_start:2", true
		}
	case agent.SessionQueueUpdateEvent:
		if len(value.SteeringMessages) == 0 && len(value.FollowUpMessages) == 0 {
			return "queue:", true
		}
	case agent.MessageStartEvent:
		if value.Turn == 2 {
			if value.Message.Role() == agentmsg.RoleUser {
				return "message_start:user:" + messageText(t, value.Message.(agentmsg.LLM).Conversation()), true
			}
			return "message_start:assistant", true
		}
	case agent.MessageEndEvent:
		if value.Turn == 2 {
			if value.Message.Role() == agentmsg.RoleUser {
				return "message_end:user:" + messageText(t, value.Message.(agentmsg.LLM).Conversation()), true
			}
			return "message_end:assistant", true
		}
	case agent.TurnEndEvent:
		if value.Turn == 2 {
			return "turn_end:2", true
		}
	case agent.SessionAgentEndEvent:
		return "agent_end", true
	case agent.AgentSettledEvent:
		return "agent_settled", true
	}
	return "", false
}

func assertLaterTurnDurableMessages(t *testing.T, messages []llm.ConversationMessage, finish llm.FinishReason) {
	t.Helper()
	if len(messages) != 4 {
		t.Fatalf("durable messages = %#v, want initial/first/queued/failure", messages)
	}
	wantRoles := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant}
	for index, role := range wantRoles {
		if messages[index].Role() != role {
			t.Fatalf("durable role[%d] = %s, want %s", index, messages[index].Role(), role)
		}
	}
	first, ok := messages[1].(llm.AssistantTextMessage)
	if !ok || messageText(t, messages[0]) != "initial" || onlyText(t, first.Content()) != "first" || messageText(t, messages[2]) != "queued" {
		t.Fatalf("durable context = %#v", messages)
	}
	terminal, ok := messages[3].(llm.AssistantTerminal)
	if !ok || terminal.FinishReason() != finish {
		t.Fatalf("durable failure = %#v, want finish %s", messages[3], finish)
	}
}

func TestAgentSessionShutdownDuringPromptPreparationHasNoOrphanLifecycle(t *testing.T) {
	for _, phase := range []string{"model access", "before_agent_start"} {
		t.Run(phase, func(t *testing.T) {
			entered := make(chan struct{})
			cancelled := make(chan struct{})
			release := make(chan struct{})
			block := func(ctx context.Context) {
				close(entered)
				<-ctx.Done()
				close(cancelled)
				<-release
			}
			transcript := newSessionManager(t)
			implementation := newScriptedProvider(t, mustTextTerminal(t, "must not run"))
			config := agent.SessionConfig{
				Provider: implementation, SessionManager: transcript, Model: sessionTestModel(t),
				Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
			}
			if phase == "model access" {
				config.ValidateModelAccess = func(ctx context.Context, _ provider.Model) error {
					block(ctx)
					return nil
				}
			} else {
				config.Hooks.BeforeAgentStart = func(ctx context.Context, _ agent.BeforeAgentStartEvent) (agent.BeforeAgentStartResult, error) {
					block(ctx)
					return agent.BeforeAgentStartResult{}, nil
				}
			}
			coordinator, err := agent.NewSession(config)
			if err != nil {
				t.Fatal(err)
			}
			var events []agent.AgentEventType
			coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
				events = append(events, event.Type())
			})
			type outcome struct {
				result agent.Result
				err    error
			}
			runDone := make(chan outcome, 1)
			go func() {
				result, runErr := coordinator.Run(context.Background(), "prompt")
				runDone <- outcome{result: result, err: runErr}
			}()
			<-entered
			shutdownDone := make(chan error, 1)
			go func() {
				shutdownDone <- coordinator.Shutdown(context.Background(), agent.SessionShutdownOptions{
					Event: agent.SessionShutdownHookEvent{Reason: agent.ShutdownQuit},
				})
			}()
			<-cancelled
			close(release)
			out := <-runDone
			if !errors.Is(out.err, agent.ErrAgentAborted) {
				t.Fatalf("Run = (%#v, %v), want preflight abort", out.result, out.err)
			}
			if _, ok := out.result.Terminal(); ok {
				t.Fatalf("preflight abort produced terminal = %#v", out.result)
			}
			if err := <-shutdownDone; err != nil {
				t.Fatalf("Shutdown = %v", err)
			}
			if len(events) != 0 {
				t.Fatalf("preflight abort events = %v", events)
			}
			if implementation.CallCount() != 0 || len(transcript.BuildContext().AgentMessages()) != 0 {
				t.Fatalf("preflight abort side effects: calls=%d messages=%#v", implementation.CallCount(), transcript.BuildContext().AgentMessages())
			}
		})
	}
}

func TestAgentSessionConcurrentCloseSharesOneShutdownAttempt(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		Hooks: agent.Hooks{SessionShutdown: func(context.Context, agent.SessionShutdownHookEvent) error {
			if calls.Add(1) == 1 {
				close(entered)
			}
			<-release
			return nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- coordinator.Close(context.Background()) }()
	<-entered
	go func() { second <- coordinator.Close(context.Background()) }()
	select {
	case err := <-second:
		t.Fatalf("second Close bypassed active shutdown: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("session_shutdown hook calls while blocked = %d", got)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("session_shutdown hook calls = %d", got)
	}
}

func TestAgentSessionAssistantTailContinueEmitsQueueUpdateBeforeDelivery(t *testing.T) {
	implementation := newScriptedProvider(t, mustTextTerminal(t, "initial"), mustTextTerminal(t, "continued"))
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, runErr := coordinator.Run(context.Background(), "initial"); runErr != nil || !result.Succeeded() {
		t.Fatalf("initial Run = (%#v, %v)", result, runErr)
	}
	if err := coordinator.Steer("queued continuation"); err != nil {
		t.Fatal(err)
	}
	recording := false
	var sequence []string
	coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if !recording {
			return
		}
		switch value := event.(type) {
		case agent.AgentStartEvent:
			sequence = append(sequence, "agent_start")
		case agent.TurnStartEvent:
			sequence = append(sequence, "turn_start")
		case agent.SessionQueueUpdateEvent:
			if len(value.SteeringMessages) != 0 || len(value.FollowUpMessages) != 0 {
				t.Errorf("drain update retained queues: %#v", value)
			}
			sequence = append(sequence, "queue_update")
		case agent.MessageStartEvent:
			if value.Message.Role() == agentmsg.RoleUser {
				sequence = append(sequence, "message_start")
			}
		}
	})
	recording = true
	result, err := coordinator.Continue(context.Background())
	if err != nil || !result.Succeeded() {
		t.Fatalf("Continue = (%#v, %v)", result, err)
	}
	wantPrefix := []string{"agent_start", "turn_start", "queue_update", "message_start"}
	if len(sequence) < len(wantPrefix) || !reflect.DeepEqual(sequence[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("continuation prefix = %v, want %v", sequence, wantPrefix)
	}
}

func TestAgentSessionFollowUpDrainUpdatesAfterTurnStartBeforeDelivery(t *testing.T) {
	implementation := newScriptedProvider(t, mustTextTerminal(t, "initial"), mustTextTerminal(t, "continued"))
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.FollowUp("queued follow-up"); err != nil {
		t.Fatal(err)
	}
	var sequence []string
	coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch value := event.(type) {
		case agent.TurnStartEvent:
			sequence = append(sequence, "turn_start")
		case agent.SessionQueueUpdateEvent:
			if len(value.SteeringMessages) != 0 || len(value.FollowUpMessages) != 0 {
				t.Errorf("drain update retained queues: %#v", value)
			}
			sequence = append(sequence, "queue_update")
		case agent.MessageStartEvent:
			if value.Message.Role() == agentmsg.RoleUser && messageText(t, value.Message.(agentmsg.LLM).Conversation()) == "queued follow-up" {
				sequence = append(sequence, "message_start")
			}
		}
	})
	result, err := coordinator.Run(context.Background(), "initial")
	if err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	wantSuffix := []string{"turn_start", "queue_update", "message_start"}
	if len(sequence) < len(wantSuffix) || !reflect.DeepEqual(sequence[len(sequence)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("follow-up suffix = %v, want %v", sequence, wantSuffix)
	}
}

func TestAgentSessionQueueAllPublishesRemainingQueueBeforeEachMessage(t *testing.T) {
	for _, followUp := range []bool{false, true} {
		name := "steering"
		if followUp {
			name = "follow-up"
		}
		t.Run(name, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return agentTestEpoch }})
			if err != nil {
				t.Fatal(err)
			}
			first, err := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
				close(entered)
				select {
				case <-release:
					return mustTextTerminal(t, "first"), nil
				case <-ctx.Done():
					return nil, context.Cause(ctx)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			second, err := provider.FixedResponseStep(mustTextTerminal(t, "second"))
			if err != nil {
				t.Fatal(err)
			}
			if err := implementation.SetResponses([]provider.ScriptStep{first, second}); err != nil {
				t.Fatal(err)
			}
			transcript := newSessionManager(t)
			coordinator, err := agent.NewSession(agent.SessionConfig{
				Provider: implementation, SessionManager: transcript, Model: sessionTestModel(t),
				SettlementTimeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			if followUp {
				err = coordinator.SetFollowUpMode(agent.QueueAll)
			} else {
				err = coordinator.SetSteeringMode(agent.QueueAll)
			}
			if err != nil {
				t.Fatal(err)
			}
			var recordDelivery atomic.Bool
			var sequence []string
			coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
				switch value := event.(type) {
				case agent.SessionQueueUpdateEvent:
					if !recordDelivery.Load() {
						return
					}
					remaining := value.Steering
					live, _ := coordinator.Queues()
					rich, _ := coordinator.RichQueues()
					if followUp {
						remaining = value.FollowUp
						_, live = coordinator.Queues()
						_, rich = coordinator.RichQueues()
					}
					liveText := make([]string, len(live))
					for index, message := range live {
						liveText[index] = messageText(t, message)
					}
					if strings.Join(liveText, "\x00") != strings.Join(remaining, "\x00") {
						t.Errorf("live queue = %v, event snapshot = %v", liveText, remaining)
					}
					richText := make([]string, len(rich))
					for index, message := range rich {
						richText[index] = messageText(t, message)
					}
					if strings.Join(richText, "\x00") != strings.Join(remaining, "\x00") {
						t.Errorf("rich queue = %v, event snapshot = %v", richText, remaining)
					}
					sequence = append(sequence, "queue:"+strings.Join(remaining, ","))
				case agent.MessageStartEvent:
					if value.Message.Role() != agentmsg.RoleUser {
						return
					}
					text := messageText(t, value.Message.(agentmsg.LLM).Conversation())
					if text == "one" || text == "two" {
						sequence = append(sequence, "message:"+text)
					}
				}
			})
			type outcome struct {
				result agent.Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, runErr := coordinator.Run(context.Background(), "prompt")
				done <- outcome{result: result, err: runErr}
			}()
			<-entered
			if followUp {
				err = coordinator.FollowUp("one")
				if err == nil {
					err = coordinator.FollowUp("two")
				}
			} else {
				err = coordinator.Steer("one")
				if err == nil {
					err = coordinator.Steer("two")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			recordDelivery.Store(true)
			close(release)
			out := <-done
			if out.err != nil || !out.result.Succeeded() {
				t.Fatalf("Run = (%#v, %v)", out.result, out.err)
			}
			want := []string{"queue:two", "message:one", "queue:", "message:two"}
			if !reflect.DeepEqual(sequence, want) {
				t.Fatalf("delivery sequence = %v, want %v", sequence, want)
			}
			if requests := implementation.Requests(); len(requests) != 2 {
				t.Fatalf("provider requests = %d, want 2", len(requests))
			} else {
				assertQueued := func(label string, messages []llm.ConversationMessage) {
					counts := map[string]int{"one": 0, "two": 0}
					for _, message := range messages {
						if message.Role() != llm.RoleUser {
							continue
						}
						if text := messageText(t, message); text == "one" || text == "two" {
							counts[text]++
						}
					}
					if counts["one"] != 1 || counts["two"] != 1 {
						t.Errorf("%s queued delivery counts = %v", label, counts)
					}
				}
				assertQueued("second provider request", requests[1].Messages())
				assertQueued("durable history", transcript.BuildContext().Messages())
			}
		})
	}
}

func TestAgentSessionMessageHooksObserveOriginalStartAndFinalizedState(t *testing.T) {
	implementation := newScriptedProvider(t, mustTextTerminal(t, "answer"))
	var coordinator *agent.AgentSession
	var sequence []string
	hooks := agent.Hooks{Message: func(_ context.Context, event agent.MessageHookEvent) (agent.MessageHookResult, error) {
		if event.Message.Role() != agentmsg.RoleUser {
			return agent.MessageHookResult{}, nil
		}
		conversation := event.Message.(agentmsg.LLM).Conversation()
		switch event.Type {
		case agent.MessageStartHookEvent:
			streaming, ok := coordinator.State().Active.StreamingMessage()
			if !ok || messageText(t, streaming.(agentmsg.LLM).Conversation()) != "raw user" || len(coordinator.State().Active.Messages()) != 0 {
				t.Fatalf("message_start Agent state = %#v", coordinator.State().Active)
			}
			sequence = append(sequence, "hook_start:"+messageText(t, conversation))
		case agent.MessageEndHookEvent:
			messages := coordinator.State().Active.Messages()
			if len(messages) != 1 || messageText(t, messages[0].(agentmsg.LLM).Conversation()) != "raw user" {
				t.Fatalf("message_end hook state = %#v", messages)
			}
			sequence = append(sequence, "hook_end:"+messageText(t, conversation))
			text, textErr := llm.NewTextBlock("rewritten user")
			if textErr != nil {
				return agent.MessageHookResult{}, textErr
			}
			replacement, replaceErr := llm.NewUserContentMessage([]llm.UserContentBlock{text}, event.Message.Timestamp())
			if replaceErr != nil {
				return agent.MessageHookResult{}, replaceErr
			}
			wrapped, wrapErr := agentmsg.NewLLM(replacement)
			return agent.MessageHookResult{Message: wrapped}, wrapErr
		}
		return agent.MessageHookResult{}, nil
	}}
	var err error
	coordinator, err = agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t), Hooks: hooks,
		SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch value := event.(type) {
		case agent.MessageStartEvent:
			if value.Message.Role() == agentmsg.RoleUser {
				sequence = append(sequence, "public_start:"+messageText(t, value.Message.(agentmsg.LLM).Conversation()))
			}
		case agent.MessageEndEvent:
			if value.Message.Role() == agentmsg.RoleUser {
				messages := coordinator.State().Active.Messages()
				if len(messages) != 1 || messageText(t, messages[0].(agentmsg.LLM).Conversation()) != "rewritten user" {
					t.Fatalf("public message_end state = %#v", messages)
				}
				sequence = append(sequence, "public_end:"+messageText(t, value.Message.(agentmsg.LLM).Conversation()))
			}
		}
	})
	if result, runErr := coordinator.Run(context.Background(), "raw user"); runErr != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, runErr)
	}
	want := []string{"hook_start:raw user", "public_start:raw user", "hook_end:raw user", "public_end:rewritten user"}
	if len(sequence) < len(want) || !reflect.DeepEqual(sequence[:len(want)], want) {
		t.Fatalf("message ordering = %v, want prefix %v", sequence, want)
	}
	request := implementation.Requests()[0].Messages()
	if len(request) != 1 || messageText(t, request[0]) != "rewritten user" {
		t.Fatalf("provider request = %#v", request)
	}
}

func TestAgentSessionAppendCustomEntryEmitsAfterDurableCommit(t *testing.T) {
	manager := newSessionManager(t)
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: manager, Model: sessionTestModel(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	var observed agent.EntryAppendedEvent
	coordinator.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if value, ok := event.(agent.EntryAppendedEvent); ok {
			if _, exists := manager.Entry(value.Entry.ID()); !exists {
				t.Error("entry_appended fired before durable manager publication")
			}
			observed = value
		}
	})
	entry, err := coordinator.AppendCustomEntry(context.Background(), "fixture", json.RawMessage(`{"value":1}`))
	if err != nil || observed.Entry.ID() != entry.ID() {
		t.Fatalf("AppendCustomEntry = (%#v, %v), observed=%#v", entry, err, observed)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := coordinator.AppendCustomEntry(cancelled, "cancelled", nil); err == nil || !errors.Is(err, agent.ErrTranscriptCommit) {
		t.Fatalf("invalid custom entry error = %v", err)
	}
}

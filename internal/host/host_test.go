package host_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/host"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
)

var hostTestEpoch = time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)

type hostHarness struct {
	implementation *provider.ScriptedProvider
	model          provider.Model
	runtime        *agentruntime.Runtime
	host           *host.Host
}

func newHostHarness(t *testing.T, steps ...provider.ScriptStep) *hostHarness {
	t.Helper()
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return hostTestEpoch }})
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.SetResponses(steps); err != nil {
		t.Fatal(err)
	}
	model, err := provider.NewModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "host-model", Name: "Host Model",
		Input: []provider.InputKind{provider.InputText, provider.InputImage}, ContextWindow: 16_000, MaxTokens: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	manager, err := session.InMemorySessionManager(cwd, session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	factory := func(_ context.Context, options agentruntime.CreateOptions) (agentruntime.CreateResult, error) {
		coordinator, err := agent.NewSession(agent.SessionConfig{
			Provider: implementation, SessionManager: options.SessionManager, Model: model,
			ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return hostTestEpoch }, SettlementTimeout: time.Second,
		})
		if err != nil {
			return agentruntime.CreateResult{}, err
		}
		return agentruntime.CreateResult{
			Session:  coordinator,
			Services: &agentruntime.Services{CWD: options.SessionManager.Cwd(), AgentDir: cwd, Provider: implementation},
		}, nil
	}
	runtime, err := agentruntime.Create(context.Background(), factory, agentruntime.InitialOptions{
		CWD: cwd, AgentDir: cwd, SessionManager: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	productHost, err := host.New(context.Background(), runtime)
	if err != nil {
		_ = runtime.Dispose(context.Background())
		t.Fatal(err)
	}
	harness := &hostHarness{implementation: implementation, model: model, runtime: runtime, host: productHost}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := productHost.Dispose(ctx); err != nil {
			t.Errorf("dispose host: %v", err)
		}
	})
	return harness
}

func hostTextTerminal(t *testing.T, text string) llm.AssistantTextMessage {
	t.Helper()
	block, err := llm.NewTextBlock(text)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{block}, llm.FinishStop, llm.Usage{}, hostTestEpoch,
		llm.AssistantProvenance{Provider: "scripted", API: "scripted", Model: "host-model"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return terminal
}

func waitHostEvent(t *testing.T, events <-chan host.Event, predicate func(host.Event) bool) host.Event {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if predicate(event) {
				return event
			}
		case <-timer.C:
			t.Fatal("timed out waiting for Host event")
		}
	}
}

func TestHostStateAndQueueCommandsReadAgentSessionOwners(t *testing.T) {
	harness := newHostHarness(t)
	if err := harness.runtime.Session().FollowUp("later"); err != nil {
		t.Fatal(err)
	}

	result, err := harness.host.Dispatch(context.Background(), host.GetStateCommand{})
	if err != nil {
		t.Fatal(err)
	}
	stateResult, ok := result.(host.GetStateResult)
	if !ok {
		t.Fatalf("get_state result = %T", result)
	}
	state := stateResult.State
	if state.SessionID == "" || state.CWD == "" || state.SessionFile != nil || state.SessionName != nil {
		t.Fatalf("session identity = %#v", state)
	}
	if !state.HasModel || !state.Model.Equal(harness.model) || state.ThinkingLevel != provider.ThinkingOff || state.Phase != agent.PhaseIdle {
		t.Fatalf("model state = %#v", state)
	}
	if state.IsStreaming || state.IsPromptRunning || state.IsBashRunning || state.IsCompacting || state.RetryAttempt != 0 || state.RetryWaiting {
		t.Fatalf("idle activity = %#v", state)
	}
	if state.MessageCount != 0 || state.PendingMessageCount != 1 || !reflect.DeepEqual(state.QueuedMessages.FollowUp, []string{"later"}) {
		t.Fatalf("message state = %#v", state)
	}
	if state.ContextUsage == nil || state.ContextUsage.ContextWindow != harness.model.ContextWindow() || state.ContextUsage.Tokens == nil || *state.ContextUsage.Tokens != 0 {
		t.Fatalf("context usage = %#v", state.ContextUsage)
	}

	events := make(chan host.Event, 4)
	unsubscribe := harness.host.Subscribe(func(_ context.Context, event host.Event) { events <- event })
	defer unsubscribe()
	clearedResult, err := harness.host.Dispatch(context.Background(), host.ClearQueueCommand{})
	if err != nil {
		t.Fatal(err)
	}
	cleared, ok := clearedResult.(host.ClearQueueResult)
	if !ok || !reflect.DeepEqual(cleared.Queue.FollowUp, []string{"later"}) {
		t.Fatalf("clear_queue result = %#v (%T)", clearedResult, clearedResult)
	}
	event := waitHostEvent(t, events, func(event host.Event) bool {
		wrapped, ok := event.Value.(host.AgentSessionEvent)
		return ok && wrapped.Event.Type() == agent.QueueUpdateEventType
	})
	wrapped := event.Value.(host.AgentSessionEvent)
	queueEvent, ok := wrapped.Event.(agent.SessionQueueUpdateEvent)
	if !ok || len(queueEvent.SteeringMessages) != 0 || len(queueEvent.FollowUpMessages) != 0 {
		t.Fatalf("queue event = %#v", wrapped.Event)
	}
	after, err := harness.host.State()
	if err != nil || after.PendingMessageCount != 0 {
		t.Fatalf("state after clear = (%#v, %v)", after, err)
	}
}

func TestHostPromptAcknowledgesPreflightAndPublishesOneOrderedLifecycle(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	step, err := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		close(started)
		select {
		case <-release:
			return hostTextTerminal(t, "done"), nil
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := newHostHarness(t, step)
	events := make(chan host.Event, 64)
	unsubscribe := harness.host.Subscribe(func(_ context.Context, event host.Event) { events <- event })
	defer unsubscribe()

	result, err := harness.host.Dispatch(context.Background(), host.PromptCommand{Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	accepted, ok := result.(host.PromptAcceptedResult)
	if !ok || accepted.OperationID == 0 {
		t.Fatalf("prompt result = %#v (%T)", result, result)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("accepted prompt did not reach provider")
	}
	active, err := harness.host.State()
	if err != nil {
		t.Fatal(err)
	}
	if !active.IsPromptRunning || !active.IsStreaming || active.IsCompacting || active.Phase != agent.PhaseProvider {
		t.Fatalf("active Host state = %#v", active)
	}
	releaseOnce.Do(func() { close(release) })

	var observed []host.Event
	for {
		event := waitHostEvent(t, events, func(host.Event) bool { return true })
		observed = append(observed, event)
		if done, ok := event.Value.(host.PromptDoneEvent); ok {
			if done.OperationID != accepted.OperationID {
				t.Fatalf("prompt_done operation = %d, want %d", done.OperationID, accepted.OperationID)
			}
			break
		}
	}
	if len(observed) < 2 {
		t.Fatalf("prompt events = %#v", observed)
	}
	seenSettled := false
	for index, event := range observed {
		if index > 0 && event.Sequence != observed[index-1].Sequence+1 {
			t.Fatalf("event sequence = %#v", observed)
		}
		if wrapped, ok := event.Value.(host.AgentSessionEvent); ok && wrapped.Event.Type() == agent.AgentSettledEventType {
			seenSettled = true
		}
		if _, ok := event.Value.(host.PromptDoneEvent); ok && index != len(observed)-1 {
			t.Fatal("prompt_done was not terminal in the operation event order")
		}
	}
	if !seenSettled {
		t.Fatalf("ordered events omitted agent_settled: %#v", observed)
	}
	idle, err := harness.host.State()
	if err != nil || idle.IsPromptRunning || idle.IsStreaming || idle.MessageCount != 2 {
		t.Fatalf("settled Host state = (%#v, %v)", idle, err)
	}
}

func TestHostPromptPreflightFailureReturnsErrorAndPublishesErrorBeforeDone(t *testing.T) {
	harness := newHostHarness(t)
	events := make(chan host.Event, 4)
	unsubscribe := harness.host.Subscribe(func(_ context.Context, event host.Event) { events <- event })
	defer unsubscribe()

	_, err := harness.host.Dispatch(context.Background(), host.PromptCommand{
		Message: "bad", Images: []llm.ImageBlock{{}},
	})
	if err == nil {
		t.Fatal("invalid prompt unexpectedly succeeded")
	}
	first := waitHostEvent(t, events, func(host.Event) bool { return true })
	second := waitHostEvent(t, events, func(host.Event) bool { return true })
	promptError, ok := first.Value.(host.PromptErrorEvent)
	if !ok || promptError.Message == "" {
		t.Fatalf("first event = %#v", first)
	}
	promptDone, ok := second.Value.(host.PromptDoneEvent)
	if !ok || promptDone.OperationID != promptError.OperationID || second.Sequence != first.Sequence+1 {
		t.Fatalf("prompt failure events = %#v, %#v", first, second)
	}
	state, stateErr := harness.host.State()
	if stateErr != nil || state.IsPromptRunning || state.IsStreaming || harness.implementation.CallCount() != 0 {
		t.Fatalf("state after rejection = (%#v, %v), calls=%d", state, stateErr, harness.implementation.CallCount())
	}
}

func TestHostReloadRebindsWithoutDuplicatingSessionEvents(t *testing.T) {
	harness := newHostHarness(t)
	events := make(chan host.Event, 4)
	unsubscribe := harness.host.Subscribe(func(_ context.Context, event host.Event) { events <- event })
	defer unsubscribe()
	initial := harness.runtime.Session()
	initialID := initial.SessionManager().SessionID()

	result, err := harness.host.Dispatch(context.Background(), host.ReloadCommand{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.(host.ReloadResult); !ok || harness.runtime.Session() != initial {
		t.Fatalf("reload result/session = %#v / %p", result, harness.runtime.Session())
	}
	if _, err := harness.host.Dispatch(context.Background(), host.ClearQueueCommand{}); err != nil {
		t.Fatal(err)
	}
	event := waitHostEvent(t, events, func(event host.Event) bool {
		wrapped, ok := event.Value.(host.AgentSessionEvent)
		return ok && wrapped.Event.Type() == agent.QueueUpdateEventType
	})
	if event.SessionID != initialID {
		t.Fatalf("rebound event session = %q, want %q", event.SessionID, initialID)
	}
	select {
	case duplicate := <-events:
		t.Fatalf("reload duplicated subscription event: %#v", duplicate)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHostTracksRuntimeReplacementAsOneIdentityAndEventSequence(t *testing.T) {
	harness := newHostHarness(t)
	events := make(chan host.Event, 8)
	unsubscribe := harness.host.Subscribe(func(_ context.Context, event host.Event) { events <- event })
	defer unsubscribe()
	oldID := harness.runtime.Session().SessionManager().SessionID()
	if _, err := harness.host.Dispatch(context.Background(), host.ClearQueueCommand{}); err != nil {
		t.Fatal(err)
	}
	first := waitHostEvent(t, events, func(host.Event) bool { return true })

	replacement, err := harness.runtime.NewSession(context.Background(), agentruntime.NewOptions{})
	if err != nil || replacement.Cancelled {
		t.Fatalf("NewSession() = (%#v, %v)", replacement, err)
	}
	state, err := harness.host.State()
	if err != nil {
		t.Fatal(err)
	}
	if state.SessionID == "" || state.SessionID == oldID {
		t.Fatalf("replacement state = %#v", state)
	}
	if _, err := harness.host.Dispatch(context.Background(), host.ClearQueueCommand{}); err != nil {
		t.Fatal(err)
	}
	second := waitHostEvent(t, events, func(host.Event) bool { return true })
	if first.SessionID != oldID || second.SessionID != state.SessionID || second.Sequence != first.Sequence+1 {
		t.Fatalf("replacement event order = %#v then %#v", first, second)
	}
}

func TestHostDisposeRejectsLaterCommands(t *testing.T) {
	harness := newHostHarness(t)
	if err := harness.host.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := harness.host.Dispatch(context.Background(), host.GetStateCommand{})
	if !errors.Is(err, host.ErrClosed) {
		t.Fatalf("dispatch after dispose error = %v", err)
	}
}

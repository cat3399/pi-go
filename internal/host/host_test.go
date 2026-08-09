package host_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/host"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/resource"
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

type hostToolExecutor struct{}

func (hostToolExecutor) Name() string { return "host-tools" }
func (hostToolExecutor) Execute(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return agent.ToolOutput{Text: "ok"}, nil
}

type hostSummarizer struct {
	started chan struct{}
	wait    bool
}

func (s hostSummarizer) Summarize(ctx context.Context, _ session.SummaryInput) (session.SummaryOutput, error) {
	if s.started != nil {
		close(s.started)
	}
	if s.wait {
		<-ctx.Done()
		return session.SummaryOutput{}, context.Cause(ctx)
	}
	return session.SummaryOutput{Text: "host checkpoint"}, nil
}

type hostBashExecutor struct {
	started chan struct{}
	wait    bool
}

func (e hostBashExecutor) ExecuteBash(ctx context.Context, command string, onChunk func(string)) (agent.BashResult, error) {
	if e.started != nil {
		close(e.started)
	}
	if onChunk != nil {
		onChunk("chunk:" + command)
	}
	if e.wait {
		<-ctx.Done()
		return agent.BashResult{Output: "partial"}, context.Cause(ctx)
	}
	code := 0
	return agent.BashResult{Output: "output:" + command, ExitCode: &code}, nil
}

func newHostHarness(t *testing.T, steps ...provider.ScriptStep) *hostHarness {
	return newHostHarnessWithOptions(t, hostHarnessOptions{}, steps...)
}

type hostHarnessOptions struct {
	Models            []provider.Model
	Configure         func(*agent.SessionConfig)
	ConfigureServices func(string, *agentruntime.Services)
	SetupManager      func(*session.SessionManager)
}

func newHostHarnessWithOptions(t *testing.T, harnessOptions hostHarnessOptions, steps ...provider.ScriptStep) *hostHarness {
	t.Helper()
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return hostTestEpoch }})
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.SetResponses(steps); err != nil {
		t.Fatal(err)
	}
	models := append([]provider.Model(nil), harnessOptions.Models...)
	if len(models) == 0 {
		model, err := provider.NewModel(provider.ModelSpec{
			Provider: "scripted", API: "scripted", ID: "host-model", Name: "Host Model",
			Input: []provider.InputKind{provider.InputText, provider.InputImage}, ContextWindow: 16_000, MaxTokens: 1_000,
		})
		if err != nil {
			t.Fatal(err)
		}
		models = []provider.Model{model}
	}
	model := models[0]
	cwd := t.TempDir()
	manager, err := session.InMemorySessionManager(cwd, session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if harnessOptions.SetupManager != nil {
		harnessOptions.SetupManager(manager)
	}
	factory := func(_ context.Context, options agentruntime.CreateOptions) (agentruntime.CreateResult, error) {
		config := agent.SessionConfig{
			Provider: implementation, SessionManager: options.SessionManager, Model: model,
			AllModels:     append([]provider.Model(nil), models...),
			ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return hostTestEpoch }, SettlementTimeout: time.Second,
		}
		if harnessOptions.Configure != nil {
			harnessOptions.Configure(&config)
		}
		coordinator, err := agent.NewSession(config)
		if err != nil {
			return agentruntime.CreateResult{}, err
		}
		services := &agentruntime.Services{CWD: options.SessionManager.Cwd(), AgentDir: cwd, Provider: implementation}
		if harnessOptions.ConfigureServices != nil {
			harnessOptions.ConfigureServices(options.SessionManager.Cwd(), services)
		}
		return agentruntime.CreateResult{
			Session: coordinator, Services: services,
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

func TestHostPromptPreflightCallbackPrecedesAgentEvents(t *testing.T) {
	harness := newHostHarness(t, mustFixedStep(t, hostTextTerminal(t, "answer")))

	var mu sync.Mutex
	order := make([]string, 0, 2)
	preflight := make(chan host.PromptAcceptedResult, 1)
	unsubscribe := harness.host.Subscribe(func(_ context.Context, event host.Event) {
		if sessionEvent, ok := event.Value.(host.AgentSessionEvent); ok && sessionEvent.Event.Type() == agent.AgentStartEventType {
			mu.Lock()
			order = append(order, "agent_start")
			mu.Unlock()
		}
	})
	defer unsubscribe()

	result, err := harness.host.Dispatch(context.Background(), host.PromptCommand{
		Message: "hello",
		PreflightResult: func(result host.PromptAcceptedResult) {
			mu.Lock()
			order = append(order, "preflight")
			mu.Unlock()
			preflight <- result
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted := result.(host.PromptAcceptedResult)
	if callback := <-preflight; callback != accepted {
		t.Fatalf("preflight result = %#v, dispatch result %#v", callback, accepted)
	}
	if err := harness.runtime.Session().WaitForIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		complete := len(order) >= 2
		mu.Unlock()
		if complete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for agent_start")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(order[:2], []string{"preflight", "agent_start"}) {
		t.Fatalf("preflight/event order = %#v", order)
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

func TestHostSteerAndFollowUpPreserveRichQueuedInput(t *testing.T) {
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
	if _, err := harness.host.Dispatch(context.Background(), host.PromptCommand{Message: "start"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("prompt did not start")
	}
	image, err := llm.NewImageDataBlock("image/png", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := harness.host.Dispatch(context.Background(), host.SteerCommand{Message: "now", Images: []llm.ImageBlock{image}}); err != nil {
		t.Fatal(err)
	} else if _, ok := result.(host.SteerResult); !ok {
		t.Fatalf("steer result = %T", result)
	}
	if result, err := harness.host.Dispatch(context.Background(), host.FollowUpCommand{Message: "later"}); err != nil {
		t.Fatal(err)
	} else if _, ok := result.(host.FollowUpResult); !ok {
		t.Fatalf("follow_up result = %T", result)
	}
	state, err := harness.host.State()
	if err != nil {
		t.Fatal(err)
	}
	if state.PendingMessageCount != 2 || !reflect.DeepEqual(state.QueuedMessages.Steering, []string{"now"}) || !reflect.DeepEqual(state.QueuedMessages.FollowUp, []string{"later"}) {
		t.Fatalf("queued command state = %#v", state.QueuedMessages)
	}
	if len(state.QueuedMessages.SteeringMessages) != 1 {
		t.Fatalf("rich steering queue = %#v", state.QueuedMessages.SteeringMessages)
	}
	if _, err := harness.host.Dispatch(context.Background(), host.ClearQueueCommand{}); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	if err := harness.runtime.Session().WaitForIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHostModelThinkingPolicyAndToolCommands(t *testing.T) {
	modelA, err := provider.NewModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "model-a", Name: "A", Reasoning: true,
		Input: []provider.InputKind{provider.InputText}, ContextWindow: 16_000, MaxTokens: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	modelB, err := provider.NewModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "model-b", Name: "B", Reasoning: true,
		Input: []provider.InputKind{provider.InputText}, ContextWindow: 16_000, MaxTokens: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstTool, err := provider.NewToolDefinition("first", "First tool", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	secondTool, err := provider.NewToolDefinition("second", "Second tool", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	harness := newHostHarnessWithOptions(t, hostHarnessOptions{
		Models: []provider.Model{modelA, modelB},
		Configure: func(config *agent.SessionConfig) {
			config.Tool = hostToolExecutor{}
			config.Tools = []provider.ToolDefinition{firstTool}
			config.AllTools = []provider.ToolDefinition{firstTool, secondTool}
		},
	})

	modelResult, err := harness.host.Dispatch(context.Background(), host.SetModelCommand{Provider: "scripted", ModelID: "model-b"})
	if err != nil {
		t.Fatal(err)
	}
	selected, ok := modelResult.(host.SetModelResult)
	if !ok || !selected.Model.Equal(modelB) {
		t.Fatalf("set_model result = %#v", modelResult)
	}
	if _, err := harness.host.Dispatch(context.Background(), host.SetThinkingLevelCommand{Level: provider.ThinkingHigh}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.host.Dispatch(context.Background(), host.SetAutoCompactionCommand{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.host.Dispatch(context.Background(), host.SetAutoRetryCommand{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	state, err := harness.host.State()
	if err != nil || !state.Model.Equal(modelB) || state.ThinkingLevel != provider.ThinkingHigh || state.AutoCompactionEnabled || state.AutoRetryEnabled {
		t.Fatalf("selected controls = (%#v, %v)", state, err)
	}

	toolsResult, err := harness.host.Dispatch(context.Background(), host.GetToolsCommand{})
	if err != nil {
		t.Fatal(err)
	}
	tools := toolsResult.(host.GetToolsResult).Tools
	if len(tools) != 2 || !tools[0].Active || tools[1].Active {
		t.Fatalf("initial tools = %#v", tools)
	}
	if _, err := harness.host.Dispatch(context.Background(), host.SetToolsCommand{ToolNames: []string{"missing", "second", "second"}}); err != nil {
		t.Fatal(err)
	}
	toolsResult, err = harness.host.Dispatch(context.Background(), host.GetToolsCommand{})
	if err != nil {
		t.Fatal(err)
	}
	tools = toolsResult.(host.GetToolsResult).Tools
	if tools[0].Active || !tools[1].Active || !reflect.DeepEqual(harness.runtime.Session().ActiveToolNames(), []string{"second"}) {
		t.Fatalf("updated tools = %#v / %v", tools, harness.runtime.Session().ActiveToolNames())
	}
	if _, err := harness.host.Dispatch(context.Background(), host.SetModelCommand{Provider: "scripted", ModelID: "missing"}); err == nil || err.Error() != "Model not found: scripted/missing" {
		t.Fatalf("missing model error = %v", err)
	}
}

func TestHostSessionInspectionNameNavigationAndForkCommands(t *testing.T) {
	t.Run("inspection and navigation", func(t *testing.T) {
		harness := newHostHarness(t, mustFixedStep(t, hostTextTerminal(t, "answer")))
		if _, err := harness.runtime.Session().Prompt(context.Background(), "question"); err != nil {
			t.Fatal(err)
		}
		if _, err := harness.host.Dispatch(context.Background(), host.SetSessionNameCommand{Name: "  Named Session  "}); err != nil {
			t.Fatal(err)
		}
		statsResult, err := harness.host.Dispatch(context.Background(), host.GetSessionStatsCommand{})
		if err != nil {
			t.Fatal(err)
		}
		stats := statsResult.(host.GetSessionStatsResult)
		if stats.SessionName == nil || *stats.SessionName != "Named Session" || stats.Stats.UserMessages != 1 || stats.Stats.AssistantMessages != 1 {
			t.Fatalf("session stats = %#v", stats)
		}
		textResult, err := harness.host.Dispatch(context.Background(), host.GetLastAssistantTextCommand{})
		if err != nil {
			t.Fatal(err)
		}
		last := textResult.(host.GetLastAssistantTextResult)
		if last.Text == nil || *last.Text != "answer" {
			t.Fatalf("last assistant text = %#v", last)
		}
		userID := firstUserEntryID(t, harness.runtime.Session().SessionManager())
		navigation, err := harness.host.Dispatch(context.Background(), host.NavigateTreeCommand{TargetID: userID})
		if err != nil {
			t.Fatal(err)
		}
		navigated := navigation.(host.NavigateTreeResult)
		if navigated.Cancelled || navigated.Aborted || navigated.EditorText == nil || *navigated.EditorText != "question" {
			t.Fatalf("navigate_tree result = %#v", navigated)
		}
		if _, err := harness.host.Dispatch(context.Background(), host.SetSessionNameCommand{Name: "   "}); err == nil || err.Error() != "Session name cannot be empty" {
			t.Fatalf("empty session name error = %v", err)
		}
	})

	t.Run("fork replaces the bound runtime session", func(t *testing.T) {
		harness := newHostHarness(t, mustFixedStep(t, hostTextTerminal(t, "answer")))
		if _, err := harness.runtime.Session().Prompt(context.Background(), "fork me"); err != nil {
			t.Fatal(err)
		}
		oldID := harness.runtime.Session().SessionManager().SessionID()
		userID := firstUserEntryID(t, harness.runtime.Session().SessionManager())
		result, err := harness.host.Dispatch(context.Background(), host.ForkCommand{EntryID: userID})
		if err != nil {
			t.Fatal(err)
		}
		forked := result.(host.ForkResult)
		if forked.Cancelled || forked.SelectedText == nil || *forked.SelectedText != "fork me" || forked.SessionID == nil || *forked.SessionID == oldID {
			t.Fatalf("fork result = %#v", forked)
		}
		state, err := harness.host.State()
		if err != nil || state.SessionID != *forked.SessionID || state.MessageCount != 0 {
			t.Fatalf("forked state = (%#v, %v)", state, err)
		}
	})
}

func TestHostCompactionCommandsExposeManualActivityAndAbort(t *testing.T) {
	setup := func(t *testing.T) func(*session.SessionManager) {
		return func(manager *session.SessionManager) {
			for _, text := range []string{"old", "recent"} {
				message, err := llm.NewUserTextMessage(text, hostTestEpoch)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := manager.AppendLLMMessage(context.Background(), message); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	t.Run("success", func(t *testing.T) {
		harness := newHostHarnessWithOptions(t, hostHarnessOptions{
			SetupManager: setup(t),
			Configure: func(config *agent.SessionConfig) {
				config.KeepRecentTokens = 1
				config.KeepRecentTokensSet = true
				config.Summarizer = hostSummarizer{}
			},
		})
		result, err := harness.host.Dispatch(context.Background(), host.CompactCommand{CustomInstructions: "focus"})
		if err != nil {
			t.Fatal(err)
		}
		compacted := result.(host.CompactResult)
		if !compacted.Result.Committed || compacted.Result.Output.Text != "host checkpoint" {
			t.Fatalf("compact result = %#v", compacted)
		}
	})

	t.Run("abort", func(t *testing.T) {
		started := make(chan struct{})
		harness := newHostHarnessWithOptions(t, hostHarnessOptions{
			SetupManager: setup(t),
			Configure: func(config *agent.SessionConfig) {
				config.KeepRecentTokens = 1
				config.KeepRecentTokensSet = true
				config.Summarizer = hostSummarizer{started: started, wait: true}
			},
		})
		done := make(chan error, 1)
		go func() {
			_, compactErr := harness.host.Dispatch(context.Background(), host.CompactCommand{})
			done <- compactErr
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("compaction did not reach summarizer")
		}
		state, err := harness.host.State()
		if err != nil || !state.IsCompacting || state.IsStreaming || state.Phase != agent.PhaseCompacting {
			t.Fatalf("manual compaction state = (%#v, %v)", state, err)
		}
		if _, err := harness.host.Dispatch(context.Background(), host.AbortCompactionCommand{}); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("aborted compaction unexpectedly succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("aborted compaction did not settle")
		}
	})
}

func TestHostBashCommandsUseSessionExecutionAndEvents(t *testing.T) {
	t.Run("execute", func(t *testing.T) {
		harness := newHostHarnessWithOptions(t, hostHarnessOptions{Configure: func(config *agent.SessionConfig) {
			config.StandaloneBash = hostBashExecutor{}
		}})
		events := make(chan host.Event, 8)
		unsubscribe := harness.host.Subscribe(func(_ context.Context, event host.Event) { events <- event })
		defer unsubscribe()
		executionID := "bash-1"
		result, err := harness.host.Dispatch(context.Background(), host.BashCommand{Command: "pwd", ExecutionID: &executionID})
		if err != nil {
			t.Fatal(err)
		}
		bashResult := result.(host.BashResult).Result
		if bashResult.Output != "output:pwd" || bashResult.ExitCode == nil || *bashResult.ExitCode != 0 {
			t.Fatalf("bash result = %#v", bashResult)
		}
		event := waitHostEvent(t, events, func(event host.Event) bool {
			wrapped, ok := event.Value.(host.AgentSessionEvent)
			return ok && wrapped.Event.Type() == agent.BashExecutionUpdateEventType
		})
		update := event.Value.(host.AgentSessionEvent).Event.(agent.BashExecutionUpdateEvent)
		if update.ID == nil || *update.ID != executionID || update.Delta != "chunk:pwd" {
			t.Fatalf("bash update = %#v", update)
		}
		state, err := harness.host.State()
		if err != nil || state.MessageCount != 1 || state.IsBashRunning {
			t.Fatalf("bash state = (%#v, %v)", state, err)
		}
	})

	t.Run("abort", func(t *testing.T) {
		started := make(chan struct{})
		harness := newHostHarnessWithOptions(t, hostHarnessOptions{Configure: func(config *agent.SessionConfig) {
			config.StandaloneBash = hostBashExecutor{started: started, wait: true}
		}})
		done := make(chan struct {
			result host.CommandResult
			err    error
		}, 1)
		go func() {
			result, err := harness.host.Dispatch(context.Background(), host.BashCommand{Command: "long"})
			done <- struct {
				result host.CommandResult
				err    error
			}{result: result, err: err}
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("bash did not start")
		}
		if state, err := harness.host.State(); err != nil || !state.IsBashRunning {
			t.Fatalf("active bash state = (%#v, %v)", state, err)
		}
		if _, err := harness.host.Dispatch(context.Background(), host.AbortBashCommand{}); err != nil {
			t.Fatal(err)
		}
		select {
		case outcome := <-done:
			if outcome.err != nil {
				t.Fatal(outcome.err)
			}
			bashResult := outcome.result.(host.BashResult).Result
			if !bashResult.Cancelled || bashResult.ExitCode != nil {
				t.Fatalf("cancelled bash result = %#v", bashResult)
			}
		case <-time.After(time.Second):
			t.Fatal("bash abort did not settle")
		}
	})
}

func TestHostGetCommandsProjectsPromptAndSkillResources(t *testing.T) {
	harness := newHostHarnessWithOptions(t, hostHarnessOptions{
		ConfigureServices: func(cwd string, services *agentruntime.Services) {
			writeHostFile(t, filepath.Join(cwd, "prompts", "review.md"), "---\ndescription: Review files\n---\nreview $1")
			writeHostFile(t, filepath.Join(cwd, "skills", "audit", "SKILL.md"), "---\nname: audit\ndescription: Audit project\n---\naudit")
			resources, err := resource.New(resource.Config{CWD: cwd, AgentDir: cwd})
			if err != nil {
				t.Fatal(err)
			}
			if err := resources.Reload(context.Background()); err != nil {
				t.Fatal(err)
			}
			services.ResourceService = resources
		},
	})
	result, err := harness.host.Dispatch(context.Background(), host.GetCommandsCommand{})
	if err != nil {
		t.Fatal(err)
	}
	commands := result.(host.GetCommandsResult).Commands
	if len(commands) != 2 || commands[0].Name != "review" || commands[0].Source != host.CommandSourcePrompt || commands[0].Description != "Review files" ||
		commands[1].Name != "skill:audit" || commands[1].Source != host.CommandSourceSkill || commands[1].Description != "Audit project" {
		t.Fatalf("commands = %#v", commands)
	}
	if commands[0].SourceInfo.Path == "" || commands[0].SourceInfo.Scope != resource.ScopeUser || commands[1].SourceInfo.Path == "" {
		t.Fatalf("command source info = %#v", commands)
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

func mustFixedStep(t *testing.T, terminal llm.AssistantTerminal) provider.ScriptStep {
	t.Helper()
	step, err := provider.FixedResponseStep(terminal)
	if err != nil {
		t.Fatal(err)
	}
	return step
}

func firstUserEntryID(t *testing.T, manager *session.SessionManager) string {
	t.Helper()
	for _, entry := range manager.Entries() {
		message, ok := entry.Message()
		if ok && message.Role() == llm.RoleUser {
			return entry.ID()
		}
	}
	t.Fatal("session has no user entry")
	return ""
}

func writeHostFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

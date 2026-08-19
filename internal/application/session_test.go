package application_test

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
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/resource"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
)

var sessionTestEpoch = time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)

type sessionHarness struct {
	implementation *provider.ScriptedProvider
	model          provider.Model
	runtime        *agentruntime.Runtime
	session        *application.ApplicationSession
}

type sessionToolExecutor struct{}

func (sessionToolExecutor) Name() string { return "application-tools" }
func (sessionToolExecutor) Execute(context.Context, string, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return agent.ToolOutput{Text: "ok"}, nil
}

type sessionSummarizer struct {
	started chan struct{}
	wait    bool
}

func (s sessionSummarizer) Summarize(ctx context.Context, _ session.SummaryInput) (session.SummaryOutput, error) {
	if s.started != nil {
		close(s.started)
	}
	if s.wait {
		<-ctx.Done()
		return session.SummaryOutput{}, context.Cause(ctx)
	}
	return session.SummaryOutput{Text: "application checkpoint"}, nil
}

type sessionBashExecutor struct {
	started chan struct{}
	wait    bool
}

func (e sessionBashExecutor) ExecuteBash(ctx context.Context, command string, onChunk func(string)) (agent.BashResult, error) {
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

func newSessionHarness(t *testing.T, steps ...provider.ScriptStep) *sessionHarness {
	return newSessionHarnessWithOptions(t, sessionHarnessOptions{}, steps...)
}

type sessionHarnessOptions struct {
	Models            []provider.Model
	Configure         func(*agent.SessionConfig)
	ConfigureServices func(string, *agentruntime.Services)
	SetupManager      func(*session.SessionManager)
}

func newSessionHarnessWithOptions(t *testing.T, harnessOptions sessionHarnessOptions, steps ...provider.ScriptStep) *sessionHarness {
	t.Helper()
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return sessionTestEpoch }})
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.SetResponses(steps); err != nil {
		t.Fatal(err)
	}
	models := append([]provider.Model(nil), harnessOptions.Models...)
	if len(models) == 0 {
		model, err := provider.NewModel(provider.ModelSpec{
			Provider: "scripted", API: "scripted", ID: "application-model", Name: "Application Model",
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
			ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return sessionTestEpoch }, SettlementTimeout: time.Second,
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
	productSession, err := application.NewApplicationSession(context.Background(), runtime)
	if err != nil {
		_ = runtime.Dispose(context.Background())
		t.Fatal(err)
	}
	harness := &sessionHarness{implementation: implementation, model: model, runtime: runtime, session: productSession}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := productSession.Dispose(ctx); err != nil {
			t.Errorf("dispose application session: %v", err)
		}
	})
	return harness
}

func sessionTextTerminal(t *testing.T, text string) llm.AssistantTextMessage {
	t.Helper()
	block, err := llm.NewTextBlock(text)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{block}, llm.FinishStop, llm.Usage{}, sessionTestEpoch,
		llm.AssistantProvenance{Provider: "scripted", API: "scripted", Model: "application-model"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return terminal
}

func waitApplicationEvent(t *testing.T, events <-chan application.Event, predicate func(application.Event) bool) application.Event {
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
			t.Fatal("timed out waiting for ApplicationSession event")
		}
	}
}

func TestApplicationSessionStateAndQueueCommandsReadAgentSessionOwners(t *testing.T) {
	harness := newSessionHarness(t)
	if err := harness.runtime.Session().FollowUp("later"); err != nil {
		t.Fatal(err)
	}

	result, err := harness.session.Dispatch(context.Background(), application.GetStateCommand{})
	if err != nil {
		t.Fatal(err)
	}
	stateResult, ok := result.(application.GetStateResult)
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

	events := make(chan application.Event, 4)
	unsubscribe := harness.session.Subscribe(func(_ context.Context, event application.Event) { events <- event })
	defer unsubscribe()
	clearedResult, err := harness.session.Dispatch(context.Background(), application.ClearQueueCommand{})
	if err != nil {
		t.Fatal(err)
	}
	cleared, ok := clearedResult.(application.ClearQueueResult)
	if !ok || !reflect.DeepEqual(cleared.Queue.FollowUp, []string{"later"}) {
		t.Fatalf("clear_queue result = %#v (%T)", clearedResult, clearedResult)
	}
	event := waitApplicationEvent(t, events, func(event application.Event) bool {
		wrapped, ok := event.Value.(application.AgentSessionEvent)
		return ok && wrapped.Event.Type() == agent.QueueUpdateEventType
	})
	wrapped := event.Value.(application.AgentSessionEvent)
	queueEvent, ok := wrapped.Event.(agent.SessionQueueUpdateEvent)
	if !ok || len(queueEvent.SteeringMessages) != 0 || len(queueEvent.FollowUpMessages) != 0 {
		t.Fatalf("queue event = %#v", wrapped.Event)
	}
	after, err := harness.session.State()
	if err != nil || after.PendingMessageCount != 0 {
		t.Fatalf("state after clear = (%#v, %v)", after, err)
	}
}

func TestApplicationSessionStartsPromptAndPublishesOneOrderedOperation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	sources := make(chan agent.InputSource, 1)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	step, err := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		close(started)
		select {
		case <-release:
			return sessionTextTerminal(t, "done"), nil
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := newSessionHarnessWithOptions(t, sessionHarnessOptions{Configure: func(config *agent.SessionConfig) {
		config.Hooks.InputHandlers = append(config.Hooks.InputHandlers, func(_ context.Context, event agent.InputEvent) (agent.InputResult, error) {
			sources <- event.Source
			return agent.InputResult{Action: agent.InputContinue}, nil
		})
	}}, step)
	events := make(chan application.Event, 64)
	unsubscribe := harness.session.Subscribe(func(_ context.Context, event application.Event) { events <- event })
	defer unsubscribe()

	result, err := harness.session.Dispatch(context.Background(), application.PromptCommand{Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	startedOperation, ok := result.(application.PromptStartedResult)
	if !ok || startedOperation.OperationID == 0 {
		t.Fatalf("prompt result = %#v (%T)", result, result)
	}
	select {
	case source := <-sources:
		if source != agent.InputInteractive {
			t.Fatalf("default prompt source = %q", source)
		}
	case <-time.After(time.Second):
		t.Fatal("prompt input hook was not called")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("accepted prompt did not reach provider")
	}
	active, err := harness.session.State()
	if err != nil {
		t.Fatal(err)
	}
	if !active.IsPromptRunning || !active.IsStreaming || active.IsCompacting || active.Phase != agent.PhaseProvider {
		t.Fatalf("active ApplicationSession state = %#v", active)
	}
	releaseOnce.Do(func() { close(release) })

	var observed []application.Event
	for {
		event := waitApplicationEvent(t, events, func(application.Event) bool { return true })
		observed = append(observed, event)
		if operation, ok := event.Value.(application.OperationEvent); ok {
			if operation.OperationID != startedOperation.OperationID || operation.Command != application.CommandPrompt || operation.Status != application.OperationCompleted || operation.Error != "" {
				t.Fatalf("operation event = %#v, want completed prompt %d", operation, startedOperation.OperationID)
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
		if wrapped, ok := event.Value.(application.AgentSessionEvent); ok && wrapped.Event.Type() == agent.AgentSettledEventType {
			seenSettled = true
		}
		if _, ok := event.Value.(application.OperationEvent); ok && index != len(observed)-1 {
			t.Fatal("operation completion was not terminal in the operation event order")
		}
	}
	if !seenSettled {
		t.Fatalf("ordered events omitted agent_settled: %#v", observed)
	}
	idle, err := harness.session.State()
	if err != nil || idle.IsPromptRunning || idle.IsStreaming || idle.MessageCount != 2 {
		t.Fatalf("settled ApplicationSession state = (%#v, %v)", idle, err)
	}
}

func TestApplicationSessionRejectsPromptBeforeStartingOperation(t *testing.T) {
	harness := newSessionHarness(t)

	_, err := harness.session.Dispatch(context.Background(), application.PromptCommand{
		Message: "bad source", Source: agent.InputSource("invalid"),
	})
	if !errors.Is(err, application.ErrInvalidCommand) {
		t.Fatalf("invalid prompt source error = %v", err)
	}

	_, err = harness.session.Dispatch(context.Background(), application.PromptCommand{
		Message: "bad", Images: []llm.ImageBlock{{}},
	})
	if err == nil {
		t.Fatal("invalid prompt unexpectedly succeeded")
	}
	state, stateErr := harness.session.State()
	if stateErr != nil || state.IsPromptRunning || state.IsStreaming || harness.implementation.CallCount() != 0 {
		t.Fatalf("state after rejection = (%#v, %v), calls=%d", state, stateErr, harness.implementation.CallCount())
	}
}

func TestApplicationSessionReloadRebindsWithoutDuplicatingSessionEvents(t *testing.T) {
	harness := newSessionHarness(t)
	events := make(chan application.Event, 4)
	unsubscribe := harness.session.Subscribe(func(_ context.Context, event application.Event) { events <- event })
	defer unsubscribe()
	initial := harness.runtime.Session()
	initialID := initial.SessionManager().SessionID()

	result, err := harness.session.Dispatch(context.Background(), application.ReloadCommand{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.(application.ReloadResult); !ok || harness.runtime.Session() != initial {
		t.Fatalf("reload result/session = %#v / %p", result, harness.runtime.Session())
	}
	if _, err := harness.session.Dispatch(context.Background(), application.ClearQueueCommand{}); err != nil {
		t.Fatal(err)
	}
	event := waitApplicationEvent(t, events, func(event application.Event) bool {
		wrapped, ok := event.Value.(application.AgentSessionEvent)
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

func TestApplicationSessionTracksRuntimeReplacementAsOneIdentityAndEventSequence(t *testing.T) {
	harness := newSessionHarness(t)
	events := make(chan application.Event, 8)
	unsubscribe := harness.session.Subscribe(func(_ context.Context, event application.Event) { events <- event })
	defer unsubscribe()
	oldID := harness.runtime.Session().SessionManager().SessionID()
	if _, err := harness.session.Dispatch(context.Background(), application.ClearQueueCommand{}); err != nil {
		t.Fatal(err)
	}
	first := waitApplicationEvent(t, events, func(application.Event) bool { return true })

	replacement, err := harness.runtime.NewSession(context.Background(), agentruntime.NewOptions{})
	if err != nil || replacement.Cancelled {
		t.Fatalf("NewSession() = (%#v, %v)", replacement, err)
	}
	state, err := harness.session.State()
	if err != nil {
		t.Fatal(err)
	}
	if state.SessionID == "" || state.SessionID == oldID {
		t.Fatalf("replacement state = %#v", state)
	}
	if _, err := harness.session.Dispatch(context.Background(), application.ClearQueueCommand{}); err != nil {
		t.Fatal(err)
	}
	second := waitApplicationEvent(t, events, func(application.Event) bool { return true })
	if first.SessionID != oldID || second.SessionID != state.SessionID || second.Sequence != first.Sequence+1 {
		t.Fatalf("replacement event order = %#v then %#v", first, second)
	}
}

func TestApplicationSessionSteerAndFollowUpPreserveRichQueuedInput(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	step, err := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		close(started)
		select {
		case <-release:
			return sessionTextTerminal(t, "done"), nil
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := newSessionHarness(t, step)
	if _, err := harness.session.Dispatch(context.Background(), application.PromptCommand{Message: "start"}); err != nil {
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
	if result, err := harness.session.Dispatch(context.Background(), application.SteerCommand{Message: "now", Images: []llm.ImageBlock{image}}); err != nil {
		t.Fatal(err)
	} else if _, ok := result.(application.SteerResult); !ok {
		t.Fatalf("steer result = %T", result)
	}
	if result, err := harness.session.Dispatch(context.Background(), application.FollowUpCommand{Message: "later"}); err != nil {
		t.Fatal(err)
	} else if _, ok := result.(application.FollowUpResult); !ok {
		t.Fatalf("follow_up result = %T", result)
	}
	state, err := harness.session.State()
	if err != nil {
		t.Fatal(err)
	}
	if state.PendingMessageCount != 2 || !reflect.DeepEqual(state.QueuedMessages.Steering, []string{"now"}) || !reflect.DeepEqual(state.QueuedMessages.FollowUp, []string{"later"}) {
		t.Fatalf("queued command state = %#v", state.QueuedMessages)
	}
	if len(state.QueuedMessages.SteeringMessages) != 1 {
		t.Fatalf("rich steering queue = %#v", state.QueuedMessages.SteeringMessages)
	}
	if _, err := harness.session.Dispatch(context.Background(), application.ClearQueueCommand{}); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	if err := harness.runtime.Session().WaitForIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestApplicationSessionModelThinkingPolicyAndToolCommands(t *testing.T) {
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
	sourceBase := "/workspace/tools"
	harness := newSessionHarnessWithOptions(t, sessionHarnessOptions{
		Models: []provider.Model{modelA, modelB},
		Configure: func(config *agent.SessionConfig) {
			config.Tool = sessionToolExecutor{}
			config.Tools = []provider.ToolDefinition{firstTool}
			config.AllTools = []provider.ToolDefinition{firstTool, secondTool}
			config.ToolMetadata = map[string]agent.ToolMetadata{
				"second": {
					PromptGuidelines: []string{"Keep values exact"},
					SourceInfo: agent.SystemPromptSourceInfo{
						Path: "/workspace/tools/second.go", Source: "fixture", Scope: agent.SystemPromptSourceProject,
						Origin: agent.SystemPromptSourcePackage, BaseDir: &sourceBase,
					},
				},
			}
		},
	})

	modelResult, err := harness.session.Dispatch(context.Background(), application.SetModelCommand{Provider: "scripted", ModelID: "model-b"})
	if err != nil {
		t.Fatal(err)
	}
	selected, ok := modelResult.(application.SetModelResult)
	if !ok || !selected.Model.Equal(modelB) {
		t.Fatalf("set_model result = %#v", modelResult)
	}
	if _, err := harness.session.Dispatch(context.Background(), application.SetThinkingLevelCommand{Level: provider.ThinkingHigh}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.session.Dispatch(context.Background(), application.SetAutoCompactionCommand{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.session.Dispatch(context.Background(), application.SetAutoRetryCommand{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	state, err := harness.session.State()
	if err != nil || !state.Model.Equal(modelB) || state.ThinkingLevel != provider.ThinkingHigh || state.AutoCompactionEnabled || state.AutoRetryEnabled {
		t.Fatalf("selected controls = (%#v, %v)", state, err)
	}
	availableResult, err := harness.session.Dispatch(context.Background(), application.GetAvailableModelsCommand{})
	if err != nil {
		t.Fatal(err)
	}
	available := availableResult.(application.GetAvailableModelsResult)
	if len(available.Models) != 2 {
		t.Fatalf("available models = %#v", available.Models)
	}
	cycledResult, err := harness.session.Dispatch(context.Background(), application.CycleModelCommand{Direction: agent.CycleForward})
	if err != nil {
		t.Fatal(err)
	}
	cycled := cycledResult.(application.CycleModelResult)
	if cycled.Result == nil || !cycled.Result.Model.Equal(modelA) {
		t.Fatalf("cycled model = %#v", cycled.Result)
	}
	levelsResult, err := harness.session.Dispatch(context.Background(), application.GetAvailableThinkingLevelsCommand{})
	if err != nil {
		t.Fatal(err)
	}
	if levels := levelsResult.(application.GetAvailableThinkingLevelsResult).Levels; len(levels) == 0 {
		t.Fatal("available thinking levels are empty")
	}
	thinkingResult, err := harness.session.Dispatch(context.Background(), application.CycleThinkingLevelCommand{})
	if err != nil {
		t.Fatal(err)
	}
	if thinkingResult.(application.CycleThinkingLevelResult).Level == nil {
		t.Fatal("cycle thinking returned no level for reasoning model")
	}
	if _, err := harness.session.Dispatch(context.Background(), application.SetSteeringModeCommand{Mode: agent.QueueAll}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.session.Dispatch(context.Background(), application.SetFollowUpModeCommand{Mode: agent.QueueAll}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.session.Dispatch(context.Background(), application.AbortRetryCommand{}); err != nil {
		t.Fatal(err)
	}
	state, err = harness.session.State()
	if err != nil || state.SteeringMode != agent.QueueAll || state.FollowUpMode != agent.QueueAll {
		t.Fatalf("queue controls = (%#v, %v)", state, err)
	}

	toolsResult, err := harness.session.Dispatch(context.Background(), application.GetToolsCommand{})
	if err != nil {
		t.Fatal(err)
	}
	tools := toolsResult.(application.GetToolsResult).Tools
	if len(tools) != 2 || !tools[0].Active || tools[1].Active {
		t.Fatalf("initial tools = %#v", tools)
	}
	if string(tools[1].Parameters) != `{"type":"object"}` ||
		!reflect.DeepEqual(tools[1].PromptGuidelines, []string{"Keep values exact"}) ||
		tools[1].SourceInfo.Path != "/workspace/tools/second.go" || tools[1].SourceInfo.BaseDir == nil ||
		*tools[1].SourceInfo.BaseDir != sourceBase {
		t.Fatalf("tool inspection metadata = %#v", tools[1])
	}
	if _, err := harness.session.Dispatch(context.Background(), application.SetToolsCommand{ToolNames: []string{"missing", "second", "second"}}); err != nil {
		t.Fatal(err)
	}
	toolsResult, err = harness.session.Dispatch(context.Background(), application.GetToolsCommand{})
	if err != nil {
		t.Fatal(err)
	}
	tools = toolsResult.(application.GetToolsResult).Tools
	if tools[0].Active || !tools[1].Active || !reflect.DeepEqual(harness.runtime.Session().ActiveToolNames(), []string{"second"}) {
		t.Fatalf("updated tools = %#v / %v", tools, harness.runtime.Session().ActiveToolNames())
	}
	if _, err := harness.session.Dispatch(context.Background(), application.SetModelCommand{Provider: "scripted", ModelID: "missing"}); err == nil || err.Error() != "Model not found: scripted/missing" {
		t.Fatalf("missing model error = %v", err)
	}
}

func TestApplicationSessionSessionInspectionNameNavigationAndForkCommands(t *testing.T) {
	t.Run("inspection and navigation", func(t *testing.T) {
		harness := newSessionHarness(t, mustFixedStep(t, sessionTextTerminal(t, "answer")))
		if _, err := harness.runtime.Session().Prompt(context.Background(), "question"); err != nil {
			t.Fatal(err)
		}
		if _, err := harness.session.Dispatch(context.Background(), application.SetSessionNameCommand{Name: "  Named Session  "}); err != nil {
			t.Fatal(err)
		}
		statsResult, err := harness.session.Dispatch(context.Background(), application.GetSessionStatsCommand{})
		if err != nil {
			t.Fatal(err)
		}
		stats := statsResult.(application.GetSessionStatsResult)
		if stats.SessionName == nil || *stats.SessionName != "Named Session" || stats.Stats.UserMessages != 1 || stats.Stats.AssistantMessages != 1 {
			t.Fatalf("session stats = %#v", stats)
		}
		textResult, err := harness.session.Dispatch(context.Background(), application.GetLastAssistantTextCommand{})
		if err != nil {
			t.Fatal(err)
		}
		last := textResult.(application.GetLastAssistantTextResult)
		if last.Text == nil || *last.Text != "answer" {
			t.Fatalf("last assistant text = %#v", last)
		}
		userID := firstUserEntryID(t, harness.runtime.Session().SessionManager())
		navigation, err := harness.session.Dispatch(context.Background(), application.NavigateTreeCommand{TargetID: userID})
		if err != nil {
			t.Fatal(err)
		}
		navigated := navigation.(application.NavigateTreeResult)
		if navigated.Cancelled || navigated.Aborted || navigated.EditorText == nil || *navigated.EditorText != "question" {
			t.Fatalf("navigate_tree result = %#v", navigated)
		}
		if _, err := harness.session.Dispatch(context.Background(), application.SetSessionNameCommand{Name: "   "}); err == nil || err.Error() != "Session name cannot be empty" {
			t.Fatalf("empty session name error = %v", err)
		}
	})

	t.Run("fork replaces the bound runtime session", func(t *testing.T) {
		harness := newSessionHarness(t, mustFixedStep(t, sessionTextTerminal(t, "answer")))
		if _, err := harness.runtime.Session().Prompt(context.Background(), "fork me"); err != nil {
			t.Fatal(err)
		}
		oldID := harness.runtime.Session().SessionManager().SessionID()
		userID := firstUserEntryID(t, harness.runtime.Session().SessionManager())
		result, err := harness.session.Dispatch(context.Background(), application.ForkCommand{EntryID: userID})
		if err != nil {
			t.Fatal(err)
		}
		forked := result.(application.ForkResult)
		if forked.Cancelled || forked.SelectedText == nil || *forked.SelectedText != "fork me" || forked.SessionID == nil || *forked.SessionID == oldID {
			t.Fatalf("fork result = %#v", forked)
		}
		state, err := harness.session.State()
		if err != nil || state.SessionID != *forked.SessionID || state.MessageCount != 0 {
			t.Fatalf("forked state = (%#v, %v)", state, err)
		}
	})
}

func TestApplicationSessionCompactionCommandsExposeManualActivityAndAbort(t *testing.T) {
	setup := func(t *testing.T) func(*session.SessionManager) {
		return func(manager *session.SessionManager) {
			for _, text := range []string{"old", "recent"} {
				message, err := llm.NewUserTextMessage(text, sessionTestEpoch)
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
		harness := newSessionHarnessWithOptions(t, sessionHarnessOptions{
			SetupManager: setup(t),
			Configure: func(config *agent.SessionConfig) {
				config.KeepRecentTokens = 1
				config.KeepRecentTokensSet = true
				config.Summarizer = sessionSummarizer{}
			},
		})
		result, err := harness.session.Dispatch(context.Background(), application.CompactCommand{CustomInstructions: "focus"})
		if err != nil {
			t.Fatal(err)
		}
		compacted := result.(application.CompactResult)
		if !compacted.Result.Committed || compacted.Result.Output.Text != "application checkpoint" {
			t.Fatalf("compact result = %#v", compacted)
		}
	})

	t.Run("abort", func(t *testing.T) {
		started := make(chan struct{})
		harness := newSessionHarnessWithOptions(t, sessionHarnessOptions{
			SetupManager: setup(t),
			Configure: func(config *agent.SessionConfig) {
				config.KeepRecentTokens = 1
				config.KeepRecentTokensSet = true
				config.Summarizer = sessionSummarizer{started: started, wait: true}
			},
		})
		done := make(chan error, 1)
		go func() {
			_, compactErr := harness.session.Dispatch(context.Background(), application.CompactCommand{})
			done <- compactErr
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("compaction did not reach summarizer")
		}
		state, err := harness.session.State()
		if err != nil || !state.IsCompacting || state.IsStreaming || state.Phase != agent.PhaseCompacting {
			t.Fatalf("manual compaction state = (%#v, %v)", state, err)
		}
		if _, err := harness.session.Dispatch(context.Background(), application.AbortCompactionCommand{}); err != nil {
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

func TestApplicationSessionExposesBranchSummaryAbortCommand(t *testing.T) {
	harness := newSessionHarness(t, mustFixedStep(t, sessionTextTerminal(t, "answer")))
	result, err := harness.session.Dispatch(context.Background(), application.AbortBranchSummaryCommand{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.(application.AbortBranchSummaryResult); !ok {
		t.Fatalf("result = %T", result)
	}
}

func TestApplicationSessionBashCommandsUseSessionExecutionAndEvents(t *testing.T) {
	t.Run("execute", func(t *testing.T) {
		harness := newSessionHarnessWithOptions(t, sessionHarnessOptions{Configure: func(config *agent.SessionConfig) {
			config.StandaloneBash = sessionBashExecutor{}
		}})
		events := make(chan application.Event, 8)
		unsubscribe := harness.session.Subscribe(func(_ context.Context, event application.Event) { events <- event })
		defer unsubscribe()
		executionID := "bash-1"
		result, err := harness.session.Dispatch(context.Background(), application.BashCommand{Command: "pwd", ExecutionID: &executionID})
		if err != nil {
			t.Fatal(err)
		}
		bashResult := result.(application.BashResult).Result
		if bashResult.Output != "output:pwd" || bashResult.ExitCode == nil || *bashResult.ExitCode != 0 {
			t.Fatalf("bash result = %#v", bashResult)
		}
		event := waitApplicationEvent(t, events, func(event application.Event) bool {
			wrapped, ok := event.Value.(application.AgentSessionEvent)
			return ok && wrapped.Event.Type() == agent.BashExecutionUpdateEventType
		})
		update := event.Value.(application.AgentSessionEvent).Event.(agent.BashExecutionUpdateEvent)
		if update.ID == nil || *update.ID != executionID || update.Delta != "chunk:pwd" {
			t.Fatalf("bash update = %#v", update)
		}
		state, err := harness.session.State()
		if err != nil || state.MessageCount != 1 || state.IsBashRunning {
			t.Fatalf("bash state = (%#v, %v)", state, err)
		}
	})

	t.Run("abort", func(t *testing.T) {
		started := make(chan struct{})
		harness := newSessionHarnessWithOptions(t, sessionHarnessOptions{Configure: func(config *agent.SessionConfig) {
			config.StandaloneBash = sessionBashExecutor{started: started, wait: true}
		}})
		done := make(chan struct {
			result application.CommandResult
			err    error
		}, 1)
		go func() {
			result, err := harness.session.Dispatch(context.Background(), application.BashCommand{Command: "long"})
			done <- struct {
				result application.CommandResult
				err    error
			}{result: result, err: err}
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("bash did not start")
		}
		if state, err := harness.session.State(); err != nil || !state.IsBashRunning {
			t.Fatalf("active bash state = (%#v, %v)", state, err)
		}
		if _, err := harness.session.Dispatch(context.Background(), application.AbortBashCommand{}); err != nil {
			t.Fatal(err)
		}
		select {
		case outcome := <-done:
			if outcome.err != nil {
				t.Fatal(outcome.err)
			}
			bashResult := outcome.result.(application.BashResult).Result
			if !bashResult.Cancelled || bashResult.ExitCode != nil {
				t.Fatalf("cancelled bash result = %#v", bashResult)
			}
		case <-time.After(time.Second):
			t.Fatal("bash abort did not settle")
		}
	})
}

func TestApplicationSessionGetCommandsProjectsPromptAndSkillResources(t *testing.T) {
	harness := newSessionHarnessWithOptions(t, sessionHarnessOptions{
		ConfigureServices: func(cwd string, services *agentruntime.Services) {
			writeSessionFile(t, filepath.Join(cwd, "prompts", "review.md"), "---\ndescription: Review files\nargument-hint: <path>\n---\nreview $1")
			writeSessionFile(t, filepath.Join(cwd, "skills", "audit", "SKILL.md"), "---\nname: audit\ndescription: Audit project\n---\naudit")
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
	result, err := harness.session.Dispatch(context.Background(), application.GetCommandsCommand{})
	if err != nil {
		t.Fatal(err)
	}
	commands := result.(application.GetCommandsResult).Commands
	if len(commands) != 2 || commands[0].Name != "review" || commands[0].Source != application.CommandSourcePrompt || commands[0].Description != "Review files" || commands[0].ArgumentHint != "<path>" ||
		commands[1].Name != "skill:audit" || commands[1].Source != application.CommandSourceSkill || commands[1].Description != "Audit project" {
		t.Fatalf("commands = %#v", commands)
	}
	if commands[0].SourceInfo.Path == "" || commands[0].SourceInfo.Scope != resource.ScopeUser || commands[1].SourceInfo.Path == "" {
		t.Fatalf("command source info = %#v", commands)
	}
}

func TestApplicationSessionDisposeRejectsLaterCommands(t *testing.T) {
	harness := newSessionHarness(t)
	if err := harness.session.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := harness.session.Dispatch(context.Background(), application.GetStateCommand{})
	if !errors.Is(err, application.ErrClosed) {
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

func writeSessionFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

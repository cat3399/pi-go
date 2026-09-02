package agent_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

type sessionRetrySummarizer struct{}

func (sessionRetrySummarizer) Summarize(ctx context.Context, input session.SummaryInput) (session.SummaryOutput, error) {
	return sessionRetrySummarizer{}.SummarizeWithRetryObserver(ctx, input, nil)
}

func (sessionRetrySummarizer) SummarizeWithRetryObserver(ctx context.Context, _ session.SummaryInput, observe provider.RetryObserver) (session.SummaryOutput, error) {
	if observe != nil {
		observe(ctx, provider.RetryEvent{Kind: provider.RetryScheduled, Attempt: 2, MaxAttempts: 3, Delay: time.Millisecond, FailureKind: provider.FailureHTTPStatus, HTTPStatus: 503, ErrorMessage: "summary unavailable"})
		observe(ctx, provider.RetryEvent{Kind: provider.RetryAttempt, Attempt: 2, MaxAttempts: 3})
		observe(ctx, provider.RetryEvent{Kind: provider.RetryFinished, Attempt: 2, MaxAttempts: 3, Succeeded: true, FinishReason: provider.RetryFinishSucceeded})
	}
	return session.SummaryOutput{Text: "checkpoint"}, nil
}

func TestAgentSessionRefreshesSnapshotBetweenToolTurns(t *testing.T) {
	modelA, err := newAgentModel(provider.ModelSpec{Provider: "scripted", API: "scripted", ID: "model-a", Reasoning: true, Input: []provider.InputKind{provider.InputText}})
	if err != nil {
		t.Fatal(err)
	}
	modelB, err := newAgentModel(provider.ModelSpec{Provider: "scripted", API: "scripted", ID: "model-b", Reasoning: true, Input: []provider.InputKind{provider.InputText}})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := provider.NewToolDefinition("switch", "switches configuration", true, []byte(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	providerImpl := newScriptedProvider(t,
		mustToolUseTerminal(t, "call-switch", "switch", []byte(`{}`)),
		mustTextTerminal(t, "after switch"),
	)
	var runtime *agent.AgentSession
	tool := &fakeTool{name: "switch", execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		if err := runtime.SetModel(modelB); err != nil {
			return agent.ToolOutput{}, err
		}
		if err := runtime.SetThinkingLevel(provider.ThinkingHigh); err != nil {
			return agent.ToolOutput{}, err
		}
		if err := runtime.SetSystemPrompt("new system prompt"); err != nil {
			return agent.ToolOutput{}, err
		}
		return agent.ToolOutput{Text: "switched"}, nil
	}}
	transcript := newSessionManager(t)
	var modelEvents []agent.ModelSelectEvent
	var thinkingEvents []agent.ThinkingLevelSelectEvent
	runtime, err = agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, SessionManager: transcript, Model: modelA, ThinkingLevel: provider.ThinkingOff,
		SystemPrompt: "old system prompt", Tool: tool, Tools: []provider.ToolDefinition{definition},
		Hooks: agent.Hooks{
			ModelSelect: func(_ context.Context, event agent.ModelSelectEvent) error {
				modelEvents = append(modelEvents, event)
				return nil
			},
			ThinkingLevelSelect: func(_ context.Context, event agent.ThinkingLevelSelectEvent) error {
				thinkingEvents = append(thinkingEvents, event)
				return nil
			},
		},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "start")
	if err != nil || !result.Succeeded() {
		t.Fatalf("Run() = (%#v, %v)", result, err)
	}
	requests := providerImpl.Requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	if requests[0].Model().ID() != "model-a" || requests[0].ThinkingLevel() != provider.ThinkingOff || requests[0].SystemPrompt() != "old system prompt" {
		t.Fatalf("first request did not use initial snapshot: %#v", requests[0])
	}
	if requests[1].Model().ID() != "model-b" || requests[1].ThinkingLevel() != provider.ThinkingHigh || requests[1].SystemPrompt() != "new system prompt" {
		t.Fatalf("second request did not refresh snapshot: model=%s thinking=%s prompt=%q", requests[1].Model().ID(), requests[1].ThinkingLevel(), requests[1].SystemPrompt())
	}
	if len(modelEvents) != 1 || modelEvents[0].Model.ID() != "model-b" || modelEvents[0].PreviousModel == nil || modelEvents[0].PreviousModel.ID() != "model-a" || modelEvents[0].Source != agent.ModelSelectSet {
		t.Fatalf("model select hooks = %#v", modelEvents)
	}
	if len(thinkingEvents) != 1 || thinkingEvents[0].Level != provider.ThinkingHigh || thinkingEvents[0].PreviousLevel != provider.ThinkingOff {
		t.Fatalf("thinking select hooks = %#v", thinkingEvents)
	}
	if err := runtime.SetModel(modelB); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetThinkingLevel(provider.ThinkingHigh); err != nil {
		t.Fatal(err)
	}
	if len(modelEvents) != 1 || len(thinkingEvents) != 1 {
		t.Fatalf("unchanged selections emitted hooks: model=%#v thinking=%#v", modelEvents, thinkingEvents)
	}
}

func TestAgentSessionOwnsManagerPersistenceAndSessionMetadata(t *testing.T) {
	manager := newSessionManager(t)
	path, ok := manager.SessionFile()
	if !ok {
		t.Fatal("persistent manager has no reserved path")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session exists before assistant: %v", err)
	}
	modelA := sessionTestModel(t)
	modelB, err := newAgentModel(provider.ModelSpec{Provider: "scripted", API: "scripted", ID: "model-b", Reasoning: true, Input: []provider.InputKind{provider.InputText}})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "done")), SessionManager: manager, Model: modelA,
		ThinkingLevel: provider.ThinkingOff, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	var metadataEvents []string
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if value, ok := event.(agent.SessionInfoChangeEvent); ok {
			if value.Name == nil {
				metadataEvents = append(metadataEvents, "<cleared>")
			} else {
				metadataEvents = append(metadataEvents, *value.Name)
			}
		}
	})
	if err := runtime.SetSessionName(context.Background(), " named\n session "); err != nil {
		t.Fatal(err)
	}
	if got, ok := runtime.SessionName(); !ok || got != "named  session" {
		t.Fatalf("name=%q ok=%v", got, ok)
	}
	if len(metadataEvents) != 1 || metadataEvents[0] != "named  session" {
		t.Fatalf("metadata events=%v", metadataEvents)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata-only session was persisted: %v", err)
	}
	if err := runtime.SetModel(modelB); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetThinkingLevel(provider.ThinkingHigh); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("first assistant did not flush manager: %v", err)
	}
	entries := manager.Entries()
	var sessionInfo, modelChange, thinkingChange bool
	for _, entry := range entries {
		switch entry.Type() {
		case "session_info":
			sessionInfo = true
		case "model_change":
			modelChange = true
		case "thinking_level_change":
			thinkingChange = true
		}
	}
	if !sessionInfo || !modelChange || !thinkingChange {
		t.Fatalf("typed session state missing: info=%v model=%v thinking=%v", sessionInfo, modelChange, thinkingChange)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendSessionInfo(context.Background(), "after close"); !errors.Is(err, session.ErrClosed) {
		t.Fatalf("owned manager remained writable: %v", err)
	}
}

func TestAgentSessionLifecycleHookFailuresAreReportedWithoutBlockingLifecycle(t *testing.T) {
	manager := newSessionManager(t)
	startFailure := errors.New("session start failed")
	shutdownFailure := errors.New("session shutdown failed")
	var reports []agent.ExtensionErrorEvent
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "done")), SessionManager: manager, Model: sessionTestModel(t),
		Hooks: agent.Hooks{
			SessionStart: func(context.Context, agent.SessionStartHookEvent) error { return startFailure },
			SessionShutdown: func(context.Context, agent.SessionShutdownHookEvent) error {
				return shutdownFailure
			},
			ExtensionError: func(_ context.Context, event agent.ExtensionErrorEvent) {
				reports = append(reports, event)
			},
		},
	})
	if err != nil {
		t.Fatalf("NewSession() = %v", err)
	}
	if result, err := runtime.Run(context.Background(), "continue despite lifecycle hook errors"); err != nil || !result.Succeeded() {
		t.Fatalf("Run() = (%#v, %v)", result, err)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if _, err := manager.AppendSessionInfo(context.Background(), "must be closed"); !errors.Is(err, session.ErrClosed) {
		t.Fatalf("manager after Close error = %v, want ErrClosed", err)
	}
	if len(reports) != 2 || reports[0].Event != "session_start" || !errors.Is(reports[0].Cause, startFailure) ||
		reports[1].Event != "session_shutdown" || !errors.Is(reports[1].Cause, shutdownFailure) {
		t.Fatalf("lifecycle hook reports = %#v", reports)
	}
}

func TestAgentSessionMetadataModelAndThinkingEventOrderMatchesOriginal(t *testing.T) {
	manager := newSessionManager(t)
	reasoningModel, err := newAgentModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "reasoning", Reasoning: true,
		Input: []provider.InputKind{provider.InputText},
	})
	if err != nil {
		t.Fatal(err)
	}
	plainModel, err := newAgentModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "plain",
		Input: []provider.InputKind{provider.InputText},
	})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: manager, Model: reasoningModel,
		ThinkingLevel: provider.ThinkingOff, SettlementTimeout: time.Second,
		Hooks: agent.Hooks{
			SessionInfoChanged: func(_ context.Context, event agent.SessionInfoChangedEvent) error {
				order = append(order, "session_info_hook")
				if event.Name == nil || *event.Name != "named" {
					t.Errorf("session info hook name = %#v", event.Name)
				}
				return nil
			},
			ThinkingLevelSelect: func(_ context.Context, event agent.ThinkingLevelSelectEvent) error {
				order = append(order, "thinking_hook")
				stored, ok := manager.BuildContext().ThinkingLevel()
				if !ok || stored != string(event.Level) {
					t.Errorf("thinking hook observed durable level %q/%v, event %q", stored, ok, event.Level)
				}
				return nil
			},
			ModelSelect: func(_ context.Context, event agent.ModelSelectEvent) error {
				order = append(order, "model_hook")
				stored, ok := manager.BuildContext().Model()
				if !ok || stored.Provider != event.Model.Provider() || stored.ModelID != event.Model.ID() {
					t.Errorf("model hook observed durable selection %#v/%v, event %s/%s", stored, ok, event.Model.Provider(), event.Model.ID())
				}
				return nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch event.(type) {
		case agent.SessionInfoChangeEvent:
			order = append(order, "session_info_observer")
			if name, ok := manager.SessionName(); !ok || name != "named" {
				t.Errorf("session observer saw durable name %q/%v", name, ok)
			}
		case agent.ThinkingLevelChangedEvent:
			order = append(order, "thinking_observer")
			stored, ok := manager.BuildContext().ThinkingLevel()
			if !ok || stored != string(runtime.ThinkingLevel()) {
				t.Errorf("thinking observer saw durable level %q/%v, runtime %q", stored, ok, runtime.ThinkingLevel())
			}
		}
	})

	if err := runtime.SetSessionName(context.Background(), " named "); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"session_info_observer", "session_info_hook"}) {
		t.Fatalf("session info order = %v", order)
	}
	order = nil
	if err := runtime.SetThinkingLevel(provider.ThinkingHigh); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"thinking_observer", "thinking_hook"}) {
		t.Fatalf("thinking selection order = %v", order)
	}
	order = nil
	if err := runtime.SetModel(plainModel); err != nil {
		t.Fatal(err)
	}
	if runtime.ThinkingLevel() != provider.ThinkingOff {
		t.Fatalf("model switch thinking level = %q, want off", runtime.ThinkingLevel())
	}
	if !reflect.DeepEqual(order, []string{"thinking_observer", "thinking_hook", "model_hook"}) {
		t.Fatalf("model clamp order = %v", order)
	}
}

func TestAgentSessionRoutesEachModelToItsRegisteredAdapter(t *testing.T) {
	modelA, err := newTestModel("one", "api-one", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	modelB, err := newTestModel("two", "api-two", "model-b")
	if err != nil {
		t.Fatal(err)
	}
	adapterA := newScriptedProvider(t, mustTextTerminal(t, "one"))
	adapterB := newScriptedProvider(t, mustTextTerminal(t, "two"))
	router, err := provider.NewRouter(map[string]provider.Provider{"api-one": adapterA, "api-two": adapterB})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: router, SessionManager: newSessionManager(t), Model: modelA, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := runtime.Run(context.Background(), "first"); err != nil || !result.Succeeded() {
		t.Fatalf("first = (%#v, %v)", result, err)
	}
	if err := runtime.SetModel(modelB); err != nil {
		t.Fatal(err)
	}
	if result, err := runtime.Run(context.Background(), "second"); err != nil || !result.Succeeded() {
		t.Fatalf("second = (%#v, %v)", result, err)
	}
	if got := adapterA.Requests(); len(got) != 1 || got[0].Model().API() != "api-one" {
		t.Fatalf("adapter one requests = %#v", got)
	}
	if got := adapterB.Requests(); len(got) != 1 || got[0].Model().API() != "api-two" {
		t.Fatalf("adapter two requests = %#v", got)
	}
	unsupported, err := newTestModel("three", "api-three", "model-c")
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetModel(unsupported); err == nil {
		t.Fatal("SetModel accepted an unregistered adapter route")
	}
}

func TestAgentSessionPreservesRichToolResultWhenLegacyTextIsEmpty(t *testing.T) {
	model, err := newTestModel("scripted", "scripted", "model")
	if err != nil {
		t.Fatal(err)
	}
	definition, err := provider.NewToolDefinition("rich", "returns rich", true, []byte(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	providerImpl := newScriptedProvider(t, mustToolUseTerminal(t, "call-rich", "rich", []byte(`{}`)), mustTextTerminal(t, "done"))
	block, err := llm.NewImageDataBlock("image/png", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	tool := &fakeTool{name: "rich", execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		return agent.ToolOutput{Content: []llm.ToolResultContentBlock{block}, Details: map[string]any{"kept": true}}, nil
	}}
	transcript := newSessionManager(t)
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: providerImpl, SessionManager: transcript, Model: model, ThinkingLevel: provider.ThinkingOff, Tool: tool, Tools: []provider.ToolDefinition{definition}, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := runtime.Run(context.Background(), "go"); err != nil || !result.Succeeded() {
		t.Fatalf("run = (%#v, %v)", result, err)
	}
	messages := transcript.BuildContext().Messages()
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(messages))
	}
	resultMessage, ok := messages[2].(llm.ToolResultContentMessage)
	if !ok || len(resultMessage.Content()) != 1 {
		t.Fatalf("rich result = %#v", messages[2])
	}
}

func TestAgentSessionKeepsConversationAcrossPrompts(t *testing.T) {
	model, err := newTestModel("scripted", "scripted", "model")
	if err != nil {
		t.Fatal(err)
	}
	providerImpl := newScriptedProvider(t, mustTextTerminal(t, "first"), mustTextTerminal(t, "second"))
	transcript := newSessionManager(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, SessionManager: transcript, Model: model, ThinkingLevel: provider.ThinkingOff,
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := runtime.Prompt(context.Background(), "one"); err != nil || !result.Succeeded() {
		t.Fatalf("first prompt = (%#v, %v)", result, err)
	}
	if result, err := runtime.Prompt(context.Background(), "two"); err != nil || !result.Succeeded() {
		t.Fatalf("second prompt = (%#v, %v)", result, err)
	}
	requests := providerImpl.Requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	if got := requests[1].Messages(); len(got) != 3 {
		t.Fatalf("second request messages = %d, want prior user/assistant plus prompt", len(got))
	}
	for index, message := range []llm.ConversationMessage{requests[0].Messages()[0], requests[1].Messages()[2], transcript.BuildContext().Messages()[0], transcript.BuildContext().Messages()[2]} {
		content, ok := message.(llm.UserContentMessage)
		if !ok || len(content.Content()) != 1 {
			t.Fatalf("string prompt %d normalized to %#v", index, message)
		}
	}
	if state := runtime.State(); state.Active.Phase() != agent.PhaseIdle || state.Model.ID() != "model" {
		t.Fatalf("state after settled prompts = %#v", state)
	}
}

func TestAgentSessionRunContentPreservesImageInTranscriptAndRequest(t *testing.T) {
	model, err := newAgentModel(provider.ModelSpec{Provider: "scripted", API: "scripted", ID: "vision", Input: []provider.InputKind{provider.InputText, provider.InputImage}})
	if err != nil {
		t.Fatal(err)
	}
	implementation := newScriptedProvider(t, mustTextTerminal(t, "seen"))
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: implementation, SessionManager: newSessionManager(t), Model: model, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	image, err := llm.NewImageDataBlock("image/png", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	text, err := llm.NewTextBlock("describe")
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.RunContent(context.Background(), []llm.UserContentBlock{text, image})
	if err != nil || !result.Succeeded() {
		t.Fatalf("RunContent = (%#v, %v)", result, err)
	}
	request := implementation.Requests()
	if len(request) != 1 {
		t.Fatalf("requests = %d", len(request))
	}
	message, ok := request[0].Messages()[0].(llm.UserContentMessage)
	if !ok || len(message.Content()) != 2 {
		t.Fatalf("request content = %#v", request[0].Messages())
	}
}

func TestAgentSessionAdmissionRejectsConcurrentRunContentWithoutGhostMessage(t *testing.T) {
	model, err := newAgentModel(provider.ModelSpec{Provider: "scripted", API: "scripted", ID: "vision", Input: []provider.InputKind{provider.InputText, provider.InputImage}})
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return agentTestEpoch }})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	step, err := provider.FactoryResponseStep(func(context.Context, provider.Request, uint64) (llm.AssistantTerminal, error) {
		close(started)
		<-release
		return mustTextTerminal(t, "done"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.SetResponses([]provider.ScriptStep{step}); err != nil {
		t.Fatal(err)
	}
	transcript := newSessionManager(t)
	var clockCalls atomic.Uint32
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: implementation, SessionManager: transcript, Model: model, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time {
		clockCalls.Add(1)
		return agentTestEpoch
	}, SettlementTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	text, err := llm.NewTextBlock("first")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, runErr := runtime.RunContent(context.Background(), []llm.UserContentBlock{text})
		done <- runErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first RunContent did not reach provider")
	}
	second, err := llm.NewTextBlock("must not append")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.RunContent(context.Background(), []llm.UserContentBlock{second}); !errors.Is(err, agent.ErrBusy) {
		t.Fatalf("concurrent RunContent error = %v, want ErrBusy", err)
	}
	if _, err := runtime.Run(context.Background(), "must not append text"); !errors.Is(err, agent.ErrBusy) {
		t.Fatalf("concurrent Run error = %v, want ErrBusy", err)
	}
	if _, err := runtime.RunMessages(context.Background(), nil); !errors.Is(err, agent.ErrBusy) {
		t.Fatalf("concurrent empty RunMessages error = %v, want ErrBusy", err)
	}
	if _, err := runtime.RunMessages(context.Background(), []agentmsg.Message{nil}); !errors.Is(err, agent.ErrBusy) {
		t.Fatalf("concurrent invalid RunMessages error = %v, want ErrBusy", err)
	}
	if _, err := runtime.Continue(context.Background()); !errors.Is(err, agent.ErrBusy) {
		t.Fatalf("concurrent Continue error = %v, want ErrBusy", err)
	}
	if got := clockCalls.Load(); got != 1 {
		t.Fatalf("clock calls while first run is active = %d, want admitted prompt only", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first RunContent = %v", err)
	}
	messages := transcript.BuildContext().Messages()
	if len(messages) != 2 {
		t.Fatalf("durable messages = %#v, want only initial user and assistant", messages)
	}
	if got := messageText(t, messages[0]); got != "first" {
		t.Fatalf("durable user message = %#v", messages[0])
	}
}

func TestAgentSessionRunMessagesAllowsEmptyBatchAgainstExistingContext(t *testing.T) {
	transcript := newSessionManager(t)
	seed, err := llm.NewUserTextMessage("existing", agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.AppendLLMMessage(context.Background(), seed); err != nil {
		t.Fatal(err)
	}
	providerImpl := newScriptedProvider(t, mustTextTerminal(t, "continued"))
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, SessionManager: transcript, Model: sessionTestModel(t), SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.RunMessages(context.Background(), nil)
	if err != nil || !result.Succeeded() || len(providerImpl.Requests()) != 1 || len(providerImpl.Requests()[0].Messages()) != 1 {
		t.Fatalf("empty RunMessages = (%#v, %v), requests=%#v", result, err, providerImpl.Requests())
	}
	if _, err := runtime.RunMessages(context.Background(), []agentmsg.Message{nil}); !errors.Is(err, agent.ErrInvalidRun) {
		t.Fatalf("idle invalid RunMessages error = %v", err)
	}
	if _, err := runtime.RunMessages(context.Background(), []agentmsg.Message{agentmsg.AssistantPartial{}}); !errors.Is(err, agent.ErrInvalidRun) {
		t.Fatalf("idle partial RunMessages error = %v", err)
	}
	if err := runtime.FollowUpAgentMessage(agentmsg.AssistantPartial{}); !errors.Is(err, agent.ErrInvalidQueueMessage) {
		t.Fatalf("partial queue error = %v", err)
	}
	if runtime.State().Active.Phase() != agent.PhaseIdle {
		t.Fatalf("invalid RunMessages left phase %s", runtime.State().Active.Phase())
	}
}

func TestAgentSessionQueuesRichContentInPriorityOrder(t *testing.T) {
	model, err := newAgentModel(provider.ModelSpec{Provider: "scripted", API: "scripted", ID: "vision", Input: []provider.InputKind{provider.InputText, provider.InputImage}})
	if err != nil {
		t.Fatal(err)
	}
	implementation := newScriptedProvider(t, mustTextTerminal(t, "first"), mustTextTerminal(t, "follow"))
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: implementation, SessionManager: newSessionManager(t), Model: model, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	image, err := llm.NewImageDataBlock("image/png", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.FollowUp("after"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SteerContent([]llm.UserContentBlock{image}); err != nil {
		t.Fatal(err)
	}
	steering, follow := runtime.RichQueues()
	if len(steering) != 1 || len(follow) != 1 {
		t.Fatalf("rich queues = %d/%d", len(steering), len(follow))
	}
	if _, ok := steering[0].(llm.UserContentMessage); !ok {
		t.Fatalf("steering content = %T", steering[0])
	}
	if _, err := runtime.Run(context.Background(), "initial"); err != nil {
		t.Fatal(err)
	}
	requests := implementation.Requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	if _, ok := requests[0].Messages()[1].(llm.UserContentMessage); !ok {
		t.Fatalf("initial request did not include idle steering = %#v", requests[0].Messages())
	}
	followMessage, ok := requests[1].Messages()[3].(llm.UserContentMessage)
	if !ok || len(followMessage.Content()) != 1 || followMessage.Content()[0].(llm.TextBlock).Text() != "after" {
		t.Fatalf("follow request tail = %#v", requests[1].Messages()[3])
	}
}

func TestAgentSessionQueueUpdatesOnlyForActualMutationAndDrain(t *testing.T) {
	model, err := newTestModel("scripted", "scripted", "model")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "initial"), mustTextTerminal(t, "follow")), SessionManager: newSessionManager(t), Model: model,
		ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	type snapshot struct{ steering, followUp int }
	var updates []snapshot
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if update, ok := event.(agent.SessionQueueUpdateEvent); ok {
			updates = append(updates, snapshot{len(update.Steering), len(update.FollowUp)})
		}
	})
	runtime.ClearAllQueues()
	if len(updates) != 0 {
		t.Fatalf("empty clear emitted queue update: %v", updates)
	}
	if err := runtime.FollowUp("after"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "before"); err != nil {
		t.Fatal(err)
	}
	if len(updates) != 2 || updates[0] != (snapshot{followUp: 1}) || updates[1] != (snapshot{}) {
		t.Fatalf("queue updates = %#v, want enqueue then drain", updates)
	}
	runtime.ClearAllQueues()
	if len(updates) != 2 {
		t.Fatalf("empty clear emitted queue update: %v", updates)
	}
}

func TestAgentSessionClearQueueReturnsRecalledMessagesAndAlwaysPublishesEmptyQueue(t *testing.T) {
	model, err := newTestModel("scripted", "scripted", "model")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: model,
		ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Steer("steer one"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.FollowUp("follow one"); err != nil {
		t.Fatal(err)
	}
	if count := runtime.PendingMessageCount(); count != 2 {
		t.Fatalf("pending message count = %d", count)
	}
	var updates []agent.SessionQueueUpdateEvent
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if update, ok := event.(agent.SessionQueueUpdateEvent); ok {
			updates = append(updates, update)
		}
	})

	cleared := runtime.ClearQueue()
	if len(cleared.Steering) != 1 || cleared.Steering[0] != "steer one" ||
		len(cleared.FollowUp) != 1 || cleared.FollowUp[0] != "follow one" ||
		len(cleared.SteeringMessages) != 1 || len(cleared.FollowUpMessages) != 1 {
		t.Fatalf("cleared queue = %#v", cleared)
	}
	if count := runtime.PendingMessageCount(); count != 0 {
		t.Fatalf("pending message count after clear = %d", count)
	}
	if len(updates) != 1 || len(updates[0].Steering) != 0 || len(updates[0].FollowUp) != 0 {
		t.Fatalf("clear queue updates = %#v", updates)
	}

	empty := runtime.ClearQueue()
	if len(empty.Steering) != 0 || len(empty.FollowUp) != 0 || len(updates) != 2 {
		t.Fatalf("empty clear = %#v, updates = %#v", empty, updates)
	}
}

func TestAgentSessionEmitsQueueDrainBeforeQueuedMessageLifecycle(t *testing.T) {
	model, err := newTestModel("scripted", "scripted", "model")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "done")), SessionManager: newSessionManager(t), Model: model,
		ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Steer("queued"); err != nil {
		t.Fatal(err)
	}
	var lifecycle []string
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch value := event.(type) {
		case agent.SessionQueueUpdateEvent:
			lifecycle = append(lifecycle, "queue")
		case agent.MessageStartEvent:
			if message, ok := value.Message.(agentmsg.LLM); ok && message.Conversation().Role() == llm.RoleUser {
				lifecycle = append(lifecycle, "user:"+messageText(t, message.Conversation()))
			}
		}
	})
	if _, err := runtime.Run(context.Background(), "initial"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"user:initial", "queue", "user:queued"}; !reflect.DeepEqual(lifecycle, want) {
		t.Fatalf("queued message lifecycle = %v, want %v", lifecycle, want)
	}
}

func TestAgentSessionContinuesForCustomAgentMessageQueuedAtLowAgentEnd(t *testing.T) {
	providerImpl := newScriptedProvider(t, mustTextTerminal(t, "first"), mustTextTerminal(t, "after custom"))
	transcript := newSessionManager(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, SessionManager: transcript, Model: sessionTestModel(t), SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	queued := false
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if _, ok := event.(agent.SessionAgentEndEvent); !ok || queued {
			return
		}
		queued = true
		custom, customErr := agentmsg.NewCustomText("review", "custom follow-up", false, nil, agentTestEpoch)
		if customErr != nil {
			t.Error(customErr)
			return
		}
		if queueErr := runtime.FollowUpAgentMessage(custom); queueErr != nil {
			t.Error(queueErr)
		}
	})
	result, err := runtime.Run(context.Background(), "go")
	if err != nil || !result.Succeeded() || providerImpl.CallCount() != 2 {
		t.Fatalf("Run = (%#v, %v), calls=%d", result, err, providerImpl.CallCount())
	}
	messages := transcript.BuildContext().AgentMessages()
	if len(messages) != 4 || messages[2].Role() != agentmsg.RoleCustom {
		t.Fatalf("custom continuation messages = %#v", messages)
	}
}

func TestAgentSessionEmitsOrderedLifecycleAndRejectsAfterClose(t *testing.T) {
	model, err := newTestModel("scripted", "scripted", "model")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: newScriptedProvider(t, mustTextTerminal(t, "ok")), SessionManager: newSessionManager(t), Model: model, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if event.Type() != "" {
			types = append(types, string(event.Type()))
		}
	})
	if _, err := runtime.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"agent_start", "turn_start", "message_start", "message_end",
		"message_start", "message_update", "message_update", "message_update", "message_end",
		"turn_end", "agent_end", "agent_settled",
	}
	if !sameStrings(types, want) {
		t.Fatalf("lifecycle events = %v, want %v", types, want)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "no"); !errors.Is(err, agent.ErrInvalidRun) {
		t.Fatalf("Run after Close = %v", err)
	}
}

func TestAgentSessionEmitsExactToolLifecycle(t *testing.T) {
	model, err := newTestModel("scripted", "scripted", "model")
	if err != nil {
		t.Fatal(err)
	}
	definition, err := provider.NewToolDefinition("echo", "echoes", true, []byte(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	call, err := llm.NewToolCallBlock("call", "echo", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	toolUse, err := newAssistantToolUseMessage([]llm.AssistantBlock{call}, mustUsage(t, 2, 1), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, toolUse, mustTextTerminal(t, "ok")), SessionManager: newSessionManager(t),
		Model: model, ThinkingLevel: provider.ThinkingOff,
		Tool: &fakeTool{name: "echo", execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
			return agent.ToolOutput{Text: "done"}, nil
		}},
		Tools: []provider.ToolDefinition{definition}, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) { types = append(types, string(event.Type())) })
	if result, err := runtime.Run(context.Background(), "go"); err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	want := []string{
		"agent_start", "turn_start", "message_start", "message_end",
		"message_start", "message_update", "message_update", "message_update", "message_end",
		"tool_execution_start", "tool_execution_end", "message_start", "message_end", "turn_end",
		"turn_start", "message_start", "message_update", "message_update", "message_update", "message_end",
		"turn_end", "agent_end", "agent_settled",
	}
	if !sameStrings(types, want) {
		t.Fatalf("tool lifecycle = %v, want %v", types, want)
	}
}

func TestAgentSessionSynthesizesMessageLifecycleForTerminalWithoutProgress(t *testing.T) {
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) { types = append(types, string(event.Type())) })
	if result, err := runtime.Run(context.Background(), "go"); err != nil || result.Succeeded() {
		t.Fatalf("Run = (%#v, %v), want settled failure", result, err)
	}
	want := []string{
		"agent_start", "turn_start", "message_start", "message_end",
		"message_start", "message_end", "turn_end", "agent_end", "agent_settled",
	}
	if !sameStrings(types, want) {
		t.Fatalf("terminal-only lifecycle = %v, want %v", types, want)
	}
}

func TestAgentSessionResolvesStreamOptionsForEachTurnModel(t *testing.T) {
	modelA, err := newTestModel("one", "api-one", "a")
	if err != nil {
		t.Fatal(err)
	}
	modelB, err := newTestModel("two", "api-two", "b")
	if err != nil {
		t.Fatal(err)
	}
	a := newScriptedProvider(t, mustTextTerminal(t, "a"))
	b := newScriptedProvider(t, mustTextTerminal(t, "b"))
	router, err := provider.NewModelRouter([]provider.ProviderRegistration{{ID: "one", Adapters: map[string]provider.Provider{"api-one": a}}, {ID: "two", Adapters: map[string]provider.Provider{"api-two": b}}})
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: router, SessionManager: newSessionManager(t), Model: modelA, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		ResolveStreamOptions: func(_ context.Context, model provider.Model) (provider.StreamOptions, error) {
			calls = append(calls, model.Provider())
			return provider.StreamOptions{APIKey: "key-" + model.Provider(), Headers: map[string]string{"X-Provider": model.Provider()}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetModel(modelB); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	if got := a.Requests()[0].StreamOptions(); got.APIKey != "key-one" || got.Headers["X-Provider"] != "one" {
		t.Fatalf("first stream options = %#v", got)
	}
	if got := b.Requests()[0].StreamOptions(); got.APIKey != "key-two" || got.Headers["X-Provider"] != "two" {
		t.Fatalf("second stream options = %#v", got)
	}
	if len(calls) != 2 || calls[0] != "one" || calls[1] != "two" {
		t.Fatalf("resolver calls = %v", calls)
	}
}

func TestAgentSessionPersistsRetryFailureButRemovesItFromRetryContext(t *testing.T) {
	model, err := newTestModel("scripted", "scripted", "model")
	if err != nil {
		t.Fatal(err)
	}
	providerFailure, err := provider.NewProviderFailure(provider.ProviderFailureSpec{Kind: provider.FailureTransport, Message: "temporary", Cause: errors.New("temporary")})
	if err != nil {
		t.Fatal(err)
	}
	failure, err := llm.NewFailure("temporary", providerFailure)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := newAssistantFailureMessageWithFailure(nil, llm.FinishError, failure, llm.Usage{}, agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	implementation := newScriptedProvider(t, failed, mustTextTerminal(t, "recovered"), mustTextTerminal(t, "later"))
	transcript := newSessionManager(t)
	var retryOrdering []string
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: transcript, Model: model, ThinkingLevel: provider.ThinkingOff,
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Retry: agent.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }},
		Hooks: agent.Hooks{Message: func(_ context.Context, event agent.MessageHookEvent) (agent.MessageHookResult, error) {
			standard, ok := event.Message.(agentmsg.LLM)
			if event.Type != agent.MessageEndHookEvent || !ok {
				return agent.MessageHookResult{}, nil
			}
			terminal, ok := standard.Conversation().(llm.AssistantTerminal)
			if !ok || terminal.FinishReason() != llm.FinishStop {
				return agent.MessageHookResult{}, nil
			}
			textMessage, ok := terminal.(llm.AssistantTextMessage)
			if !ok || len(textMessage.Content()) == 0 || textMessage.Content()[0].Text() != "recovered" {
				return agent.MessageHookResult{}, nil
			}
			retryOrdering = append(retryOrdering, "extension_message_end")
			if got := len(transcript.BuildContext().Messages()); got != 2 {
				t.Errorf("durable messages during extension message_end = %d, want user+failed assistant before append", got)
			}
			return agent.MessageHookResult{}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	unsubRetryOrdering := runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch value := event.(type) {
		case agent.MessageEndEvent:
			standard, ok := value.Message.(agentmsg.LLM)
			if !ok {
				return
			}
			terminal, ok := standard.Conversation().(llm.AssistantTerminal)
			if !ok || terminal.FinishReason() != llm.FinishStop {
				return
			}
			retryOrdering = append(retryOrdering, "session_message_end")
			if got := len(transcript.BuildContext().Messages()); got != 2 {
				t.Errorf("durable messages during successful message_end = %d, want user+failed assistant before append", got)
			}
			state := runtime.State().Active.Messages()
			if len(state) != 2 {
				t.Errorf("Agent state during successful message_end = %d, want user+successful assistant", len(state))
			}
		case agent.AutoRetryEndEvent:
			retryOrdering = append(retryOrdering, "auto_retry_end")
			if got := len(transcript.BuildContext().Messages()); got != 3 {
				t.Errorf("durable messages during auto_retry_end = %d, want successful assistant already appended", got)
			}
		}
	})
	result, err := runtime.Run(context.Background(), "retry")
	unsubRetryOrdering()
	if err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	messages := transcript.BuildContext().Messages()
	if len(messages) != 3 {
		t.Fatalf("durable messages = %#v", messages)
	}
	if _, ok := messages[1].(llm.AssistantFailureMessage); !ok {
		t.Fatalf("retry failure was not persisted: %#v", messages)
	}
	if !reflect.DeepEqual(retryOrdering, []string{"extension_message_end", "session_message_end", "auto_retry_end"}) {
		t.Fatalf("successful retry event ordering = %v", retryOrdering)
	}
	requests := implementation.Requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	for index, request := range requests {
		for _, message := range request.Messages() {
			if _, leaked := message.(llm.AssistantFailureMessage); leaked {
				t.Fatalf("request %d contains persisted retry failure", index+1)
			}
		}
	}
	if got := requests[1].Messages(); len(got) != 1 || got[0].Role() != llm.RoleUser {
		t.Fatalf("retry context = %#v", got)
	}
	if _, err := runtime.Run(context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	third := implementation.Requests()[2].Messages()
	if len(third) != 3 {
		t.Fatalf("next context = %#v", third)
	}
	for _, message := range third {
		if _, leaked := message.(llm.AssistantFailureMessage); leaked {
			t.Fatalf("later context contains persisted retry failure")
		}
	}
}

func TestAgentSessionProviderFailureSettlesBeforeNextPrompt(t *testing.T) {
	model, err := newTestModel("scripted", "scripted", "model")
	if err != nil {
		t.Fatal(err)
	}
	failure, err := llm.NewFailure("provider failed", errors.New("fixture provider failure"))
	if err != nil {
		t.Fatal(err)
	}
	failedTerminal, err := newAssistantFailureMessageWithFailure(nil, llm.FinishError, failure, mustUsage(t, 0, 0), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	providerImpl := newScriptedProvider(t, failedTerminal, mustTextTerminal(t, "recovered"))
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, SessionManager: newSessionManager(t), Model: model, ThinkingLevel: provider.ThinkingOff,
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := runtime.Run(context.Background(), "fails")
	if err != nil || first.Succeeded() {
		t.Fatalf("failed prompt = (%#v, %v)", first, err)
	}
	if state := runtime.State(); state.Active.Phase() != agent.PhaseIdle {
		t.Fatalf("state after provider failure = %s", state.Active.Phase())
	}
	second, err := runtime.Run(context.Background(), "recover")
	if err != nil || !second.Succeeded() {
		t.Fatalf("recovery prompt = (%#v, %v)", second, err)
	}
	if got := providerImpl.Requests(); len(got) != 2 || len(got[1].Messages()) != 3 {
		t.Fatalf("recovery request history = %#v", got)
	}
}

func TestAgentSessionAbortDrainsQueuedPromptBeforeSettling(t *testing.T) {
	model, err := newTestModel("scripted", "scripted", "model")
	if err != nil {
		t.Fatal(err)
	}
	providerImpl, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return agentTestEpoch }})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	step, err := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		close(entered)
		<-ctx.Done()
		return mustTextTerminal(t, "unused"), context.Cause(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	continued, err := provider.FixedResponseStep(mustTextTerminal(t, "continued after abort"))
	if err != nil {
		t.Fatal(err)
	}
	if err := providerImpl.SetResponses([]provider.ScriptStep{step, continued}); err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: providerImpl, SessionManager: newSessionManager(t), Model: model, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var started atomic.Bool
	done := make(chan error, 1)
	go func() { started.Store(true); _, err := runtime.Run(context.Background(), "cancel"); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	if !started.Load() {
		t.Fatal("run did not start")
	}
	if err := runtime.Steer("consume queued"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("aborted run returned error: %v", err)
	}
	if state := runtime.State(); state.Active.Phase() != agent.PhaseIdle {
		t.Fatalf("state after abort = %s", state.Active.Phase())
	}
	steering, _ := runtime.Queues()
	if len(steering) != 0 || providerImpl.CallCount() != 2 {
		t.Fatalf("abort left stale queue: steering=%#v calls=%d", steering, providerImpl.CallCount())
	}
	requests := providerImpl.Requests()
	if got := requests[1].Messages(); len(got) < 3 || messageText(t, got[len(got)-1]) != "consume queued" {
		t.Fatalf("queued continuation request = %#v", got)
	}
}

func sessionTestModel(t *testing.T) provider.Model {
	t.Helper()
	model, err := newTestModel("scripted", "scripted", "model")
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func sessionHTTPFailure(t *testing.T, status int) llm.AssistantFailureMessage {
	t.Helper()
	failure, err := provider.NewProviderFailure(provider.ProviderFailureSpec{
		Kind: provider.FailureHTTPStatus, Message: "http failure", Cause: errors.New("fixture"), HTTPStatus: &status,
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := llm.NewFailure("http failure", failure)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := newAssistantFailureMessageWithFailure(nil, llm.FinishError, message, mustUsage(t, 0, 0), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	return terminal
}

func TestAgentSessionRetriesOnlyTransientHTTPFailures(t *testing.T) {
	model := sessionTestModel(t)
	for _, test := range []struct {
		name     string
		status   int
		requests int
		success  bool
	}{
		{name: "retryable_429", status: 429, requests: 2, success: true},
		{name: "nonretryable_400", status: 400, requests: 1, success: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			implementation := newScriptedProvider(t, sessionHTTPFailure(t, test.status), mustTextTerminal(t, "recovered"))
			runtime, err := agent.NewSession(agent.SessionConfig{
				Provider: implementation, SessionManager: newSessionManager(t), Model: model, ThinkingLevel: provider.ThinkingOff,
				Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
				Retry: agent.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := runtime.Run(context.Background(), "retry")
			if err != nil || result.Succeeded() != test.success {
				t.Fatalf("Run = (%#v, %v), want success %t", result, err, test.success)
			}
			if got := len(implementation.Requests()); got != test.requests {
				t.Fatalf("requests = %d, want %d", got, test.requests)
			}
		})
	}
}

func TestAgentSessionRetryLifecycleAndAgentEndContinuation(t *testing.T) {
	model := sessionTestModel(t)
	implementation := newScriptedProvider(t, sessionHTTPFailure(t, 429), mustTextTerminal(t, "recovered"), mustTextTerminal(t, "followed"))
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: model, ThinkingLevel: provider.ThinkingOff,
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Retry: agent.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	var events []agent.SessionEvent
	queued := false
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		events = append(events, event)
		ended, isEnd := event.(agent.SessionAgentEndEvent)
		if isEnd && !queued && !ended.WillRetry {
			queued = true
			if err := runtime.FollowUp("from end"); err != nil {
				t.Errorf("FollowUp from agent_end = %v", err)
			}
		}
	})
	if result, err := runtime.Run(context.Background(), "go"); err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	var controls []string
	var ends []bool
	for _, event := range events {
		switch event.Type() {
		case "agent_start", "agent_end", "auto_retry_start", "auto_retry_end", "agent_settled":
			controls = append(controls, string(event.Type()))
			if ended, ok := event.(agent.SessionAgentEndEvent); ok {
				ends = append(ends, ended.WillRetry)
			}
		}
	}
	want := []string{"agent_start", "agent_end", "auto_retry_start", "agent_start", "auto_retry_end", "agent_end", "agent_start", "agent_end", "agent_settled"}
	if !sameStrings(controls, want) {
		t.Fatalf("lifecycle = %v, want %v", controls, want)
	}
	if len(ends) != 3 || !ends[0] || ends[1] || ends[2] {
		t.Fatalf("agent_end willRetry = %v", ends)
	}
	var retryStart agent.AutoRetryStartEvent
	var retryEnd agent.AutoRetryEndEvent
	var sawRetryStart, sawRetryEnd bool
	for _, event := range events {
		switch value := event.(type) {
		case agent.AutoRetryStartEvent:
			retryStart, sawRetryStart = value, true
		case agent.AutoRetryEndEvent:
			retryEnd, sawRetryEnd = value, true
		}
	}
	if !sawRetryStart || retryStart.Attempt != 1 || retryStart.MaxAttempts != 1 || retryStart.ErrorMessage != "http failure" {
		t.Fatalf("retry start payload = %#v", retryStart)
	}
	if !sawRetryEnd || !retryEnd.Success || retryEnd.Attempt != 1 {
		t.Fatalf("retry end payload = %#v", retryEnd)
	}
	if got := len(implementation.Requests()); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestAgentSessionAbortAndWaitIncludeRetrySleep(t *testing.T) {
	model := sessionTestModel(t)
	entered := make(chan struct{})
	implementation := newScriptedProvider(t, sessionHTTPFailure(t, 429))
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: model, ThinkingLevel: provider.ThinkingOff,
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Retry: agent.RetryPolicy{MaxAttempts: 2, InitialDelay: time.Hour, Sleep: func(ctx context.Context, _ time.Duration) error {
			close(entered)
			<-ctx.Done()
			return context.Cause(ctx)
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result agent.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() { result, err := runtime.Run(context.Background(), "retry"); done <- outcome{result, err} }()
	waitClosed(t, entered, "retry sleep")
	if state := runtime.State(); state.Active.Phase() != agent.PhaseRetryWait {
		t.Fatalf("state during retry wait = %s", state.Active.Phase())
	}
	if err := runtime.Abort(context.Background()); err != nil {
		t.Fatalf("Abort = %v", err)
	}
	if err := runtime.WaitForIdle(context.Background()); err != nil {
		t.Fatalf("WaitForIdle = %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("Run error = %v, want nil cancellation result", got.err)
	}
	if terminal, ok := got.result.Terminal(); !ok || terminal.FinishReason() != llm.FinishError {
		t.Fatalf("Run result = %#v, want previously settled failure", got.result)
	}
}

func TestAgentSessionRetryFinalFailureEndsAfterAgentEnd(t *testing.T) {
	model := sessionTestModel(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, sessionHTTPFailure(t, 429), sessionHTTPFailure(t, 429)), SessionManager: newSessionManager(t),
		Model: model, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Retry: agent.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	var controls []agent.SessionEvent
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch event.Type() {
		case "agent_start", "agent_end", "auto_retry_start", "auto_retry_end", "agent_settled":
			controls = append(controls, event)
		}
	})
	if result, err := runtime.Run(context.Background(), "fail twice"); err != nil || result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	types := make([]string, len(controls))
	for index, event := range controls {
		types[index] = string(event.Type())
	}
	want := []string{"agent_start", "agent_end", "auto_retry_start", "agent_start", "agent_end", "auto_retry_end", "agent_settled"}
	if !sameStrings(types, want) {
		t.Fatalf("lifecycle = %v, want %v", types, want)
	}
	ended, ok := controls[5].(agent.AutoRetryEndEvent)
	if !ok || ended.Success || ended.Attempt != 1 || ended.FinalError != "http failure" {
		t.Fatalf("final retry event = %#v", ended)
	}
}

func TestAgentSessionRetrySeriesSpansIntermediateFailures(t *testing.T) {
	model := sessionTestModel(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, sessionHTTPFailure(t, 429), sessionHTTPFailure(t, 429), mustTextTerminal(t, "third")), SessionManager: newSessionManager(t),
		Model: model, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Retry: agent.RetryPolicy{MaxAttempts: 3, Sleep: func(context.Context, time.Duration) error { return nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	var controls []agent.SessionEvent
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch event.Type() {
		case "agent_end", "auto_retry_start", "auto_retry_end":
			controls = append(controls, event)
		}
	})
	if result, err := runtime.Run(context.Background(), "three"); err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	types := make([]string, len(controls))
	for index, event := range controls {
		types[index] = string(event.Type())
	}
	want := []string{"agent_end", "auto_retry_start", "agent_end", "auto_retry_start", "auto_retry_end", "agent_end"}
	if !sameStrings(types, want) {
		t.Fatalf("retry series events = %v, want %v", types, want)
	}
	firstEnd, firstOK := controls[0].(agent.SessionAgentEndEvent)
	secondEnd, secondOK := controls[2].(agent.SessionAgentEndEvent)
	thirdEnd, thirdOK := controls[5].(agent.SessionAgentEndEvent)
	retryEnd, retryOK := controls[4].(agent.AutoRetryEndEvent)
	if !firstOK || !secondOK || !thirdOK || !retryOK || !firstEnd.WillRetry || !secondEnd.WillRetry || thirdEnd.WillRetry || !retryEnd.Success {
		t.Fatalf("retry lifecycle payloads = %#v", controls)
	}
}

func TestAgentSessionRetryEndsOnToolUseBeforeLowAgentEnd(t *testing.T) {
	model := sessionTestModel(t)
	definition, err := provider.NewToolDefinition("tool", "fixture", true, []byte(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	tool := &fakeTool{name: "tool", execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		return agent.ToolOutput{Text: "ok"}, nil
	}}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider:       newScriptedProvider(t, sessionHTTPFailure(t, 429), mustToolUseTerminal(t, "call", "tool", []byte(`{}`)), mustTextTerminal(t, "done")),
		SessionManager: newSessionManager(t), Model: model, ThinkingLevel: provider.ThinkingOff, Tool: tool, Tools: []provider.ToolDefinition{definition},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Retry: agent.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch event.Type() {
		case "agent_end", "auto_retry_start", "auto_retry_end":
			types = append(types, string(event.Type()))
		}
	})
	if result, err := runtime.Run(context.Background(), "tool retry"); err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	want := []string{"agent_end", "auto_retry_start", "auto_retry_end", "agent_end"}
	if !sameStrings(types, want) {
		t.Fatalf("tool retry lifecycle = %v, want %v", types, want)
	}
}

func TestAgentSessionPrePromptThresholdDoesNotRepeatAfterNewBoundary(t *testing.T) {
	model := sessionTestModel(t)
	transcript := newSessionManager(t)
	oldUser, err := llm.NewUserTextMessage("old request", agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.AppendLLMMessage(context.Background(), oldUser); err != nil {
		t.Fatal(err)
	}
	oldAssistant := mustTextTerminalWithProvenance(t, "old reply", llm.AssistantProvenance{Provider: "scripted", API: "scripted", Model: "model"})
	if _, err := transcript.AppendLLMMessage(context.Background(), oldAssistant); err != nil {
		t.Fatal(err)
	}
	var summaries atomic.Uint32
	hookSawCompactedContext := false
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "new reply")), SessionManager: transcript, Model: model, ThinkingLevel: provider.ThinkingOff,
		ContextWindow: 1, KeepRecentTokens: 1, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
			summaries.Add(1)
			return session.SummaryOutput{Text: "checkpoint"}, nil
		}),
		Hooks: agent.Hooks{BeforeAgentStart: func(_ context.Context, event agent.BeforeAgentStartEvent) (agent.BeforeAgentStartResult, error) {
			for _, message := range event.Messages {
				if message.Role() == agentmsg.RoleCompactionSummary {
					hookSawCompactedContext = true
				}
			}
			return agent.BeforeAgentStartResult{}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var compactTypes []string
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if event.Type() == "compaction_start" || event.Type() == "compaction_end" {
			compactTypes = append(compactTypes, string(event.Type()))
		}
	})
	if result, err := runtime.Run(context.Background(), "new request"); err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	if summaries.Load() != 1 || !sameStrings(compactTypes, []string{"compaction_start", "compaction_end"}) {
		t.Fatalf("summaries/events = %d/%v", summaries.Load(), compactTypes)
	}
	if !hookSawCompactedContext {
		t.Fatal("before_agent_start ran before pre-prompt compaction committed")
	}
}

func TestAgentSessionSkipsStalePreCompactionOverflowError(t *testing.T) {
	boundary := agentTestEpoch.Add(time.Second)
	manager := newBoundarySessionManager(t, boundary)
	model := boundaryCompactionModel(t)
	provenance := llm.AssistantProvenance{Provider: model.Provider(), API: model.API(), Model: model.ID()}
	user, err := llm.NewUserTextMessage("before compaction", boundary)
	if err != nil {
		t.Fatal(err)
	}
	userEntry, err := manager.AppendLLMMessage(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	overflow, err := provider.NewProviderFailure(provider.ProviderFailureSpec{
		Kind: provider.FailureContextOverflow, Message: "stale overflow", Cause: errors.New("stale overflow"),
	})
	if err != nil {
		t.Fatal(err)
	}
	failure, err := llm.NewFailure("stale overflow", overflow)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := llm.NewAssistantFailureMessageWithFailure(nil, llm.FinishError, failure, llm.Usage{}, boundary, provenance)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendCompaction(context.Background(), "checkpoint", userEntry.ID(), 95, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	var summaries atomic.Uint32
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "after boundary")), SessionManager: manager,
		Model: model, ThinkingLevel: provider.ThinkingOff, KeepRecentTokens: 1,
		Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
			summaries.Add(1)
			return session.SummaryOutput{Text: "unexpected"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := runtime.Run(context.Background(), "first prompt after compaction"); err != nil || !result.Succeeded() {
		t.Fatalf("Run() = (%#v, %v)", result, err)
	}
	if summaries.Load() != 0 {
		t.Fatalf("stale overflow retriggered compaction %d times", summaries.Load())
	}
}

func TestAgentSessionSkipsPreCompactionUsageWhenCurrentErrorNeedsFallback(t *testing.T) {
	boundary := agentTestEpoch.Add(2 * time.Second)
	manager := newBoundarySessionManager(t, boundary)
	model := boundaryCompactionModel(t)
	provenance := llm.AssistantProvenance{Provider: model.Provider(), API: model.API(), Model: model.ID()}
	user, err := llm.NewUserTextMessage("before compaction", boundary)
	if err != nil {
		t.Fatal(err)
	}
	userEntry, err := manager.AppendLLMMessage(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	oldUsage := mustUsage(t, 95, 0)
	oldAssistant, err := llm.NewAssistantTextMessage([]llm.TextBlock{mustTextBlock(t, "old")}, llm.FinishStop, oldUsage, boundary, provenance)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), oldAssistant); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendCompaction(context.Background(), "checkpoint", userEntry.ID(), 95, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	postBoundaryFailure, err := llm.NewAssistantFailureMessage(nil, llm.FinishError, "post-boundary error", llm.Usage{}, boundary.Add(time.Millisecond), provenance)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), postBoundaryFailure); err != nil {
		t.Fatal(err)
	}

	var summaries atomic.Uint32
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "after fallback")), SessionManager: manager,
		Model: model, ThinkingLevel: provider.ThinkingOff, KeepRecentTokens: 1,
		Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
			summaries.Add(1)
			return session.SummaryOutput{Text: "unexpected"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := runtime.Run(context.Background(), "fallback prompt"); err != nil || !result.Succeeded() {
		t.Fatalf("Run() = (%#v, %v)", result, err)
	}
	if summaries.Load() != 0 {
		t.Fatalf("pre-compaction usage retriggered threshold compaction %d times", summaries.Load())
	}
}

func newBoundarySessionManager(t *testing.T, at time.Time) *session.SessionManager {
	t.Helper()
	var ids atomic.Uint64
	manager, err := session.InMemorySessionManagerWithOptions(t.TempDir(), session.ManagerOptions{
		NewSession: session.NewSessionOptions{ID: "boundary-session"},
		Now:        func() time.Time { return at },
		NewEntryID: func() (string, error) { return fmt.Sprintf("boundary-%d", ids.Add(1)), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("SessionManager.Close() = %v", err)
		}
	})
	return manager
}

func boundaryCompactionModel(t *testing.T) provider.Model {
	t.Helper()
	model, err := newAgentModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "boundary-model",
		Input: []provider.InputKind{provider.InputText}, ContextWindow: 100, MaxTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func TestAgentSessionOverflowCompactsAndContinuesWithoutRuntimeFailure(t *testing.T) {
	model := sessionTestModel(t)
	transcript := newSessionManager(t)
	history, err := llm.NewUserTextMessage("historical context", agentTestEpoch.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.AppendLLMMessage(context.Background(), history); err != nil {
		t.Fatal(err)
	}
	contextFailure, err := provider.NewProviderFailure(provider.ProviderFailureSpec{Kind: provider.FailureContextOverflow, Message: "overflow", Cause: errors.New("overflow")})
	if err != nil {
		t.Fatal(err)
	}
	failure, err := llm.NewFailure("overflow", contextFailure)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := newAssistantFailureMessageWithFailure(nil, llm.FinishError, failure, mustUsage(t, 0, 0), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	implementation := newScriptedProvider(t, failed, mustTextTerminal(t, "recovered"))
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: transcript, Model: model, ThinkingLevel: provider.ThinkingOff,
		ContextWindow: 1, KeepRecentTokens: 1, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
			return session.SummaryOutput{Text: "checkpoint"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := runtime.Run(context.Background(), "overflow"); err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	requests := implementation.Requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want compact-and-continue", len(requests))
	}
	for _, message := range requests[1].Messages() {
		if _, ok := message.(llm.AssistantFailureMessage); ok {
			t.Fatalf("retry runtime context retained overflow failure: %#v", requests[1].Messages())
		}
	}
	if got := len(transcript.Entries()); got < 4 {
		t.Fatalf("durable history entries = %d, want failure + compaction + recovery", got)
	}
}

func TestAgentSessionCompactsBetweenToolTurnsBeforeNextProviderRequest(t *testing.T) {
	model, err := newAgentModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "mid-turn-compaction",
		Input: []provider.InputKind{provider.InputText}, ContextWindow: 100, MaxTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := provider.NewToolDefinition("read", "read", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	call, err := llm.NewToolCallBlock("call-read", "read", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	toolTurn, err := newAssistantToolUseMessage(
		[]llm.AssistantBlock{mustTextBlock(t, "reading"), call}, mustUsage(t, 70, 5), agentTestEpoch,
	)
	if err != nil {
		t.Fatal(err)
	}

	var summaries atomic.Uint32
	var compactionEnded atomic.Bool
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return agentTestEpoch }})
	if err != nil {
		t.Fatal(err)
	}
	first, err := provider.FixedResponseStep(toolTurn)
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.FactoryResponseStep(func(_ context.Context, request provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		if summaries.Load() != 1 || !compactionEnded.Load() {
			t.Errorf("next provider request started before compaction completed: summaries=%d ended=%t", summaries.Load(), compactionEnded.Load())
		}
		queued := false
		for _, message := range request.Messages() {
			if message.Role() == llm.RoleUser && messageText(t, message) == "queued during compaction" {
				queued = true
				break
			}
		}
		if !queued {
			t.Errorf("next provider request omitted steering queued during compaction: %#v", request.Messages())
		}
		return mustTextTerminal(t, "done"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.SetResponses([]provider.ScriptStep{first, second}); err != nil {
		t.Fatal(err)
	}

	var runtime *agent.AgentSession
	runtime, err = agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: model, ThinkingLevel: provider.ThinkingOff,
		Tool: &fakeTool{name: "read", execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
			return agent.ToolOutput{Text: "12345678901234567890123456789012"}, nil
		}},
		Tools: []provider.ToolDefinition{definition}, ContextReserve: 20, ContextReserveSet: true,
		KeepRecentTokens: 10, KeepRecentTokensSet: true,
		Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
			summaries.Add(1)
			if err := runtime.Steer("queued during compaction"); err != nil {
				return session.SummaryOutput{}, err
			}
			return session.SummaryOutput{Text: "mid-turn checkpoint"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	var compactions []agent.CompactionEndEvent
	agentStarts, agentEnds := 0, 0
	secondTurnRestored := false
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch value := event.(type) {
		case agent.AgentStartEvent:
			agentStarts++
		case agent.SessionAgentEndEvent:
			agentEnds++
		case agent.TurnStartEvent:
			if value.Turn == 2 {
				activity := runtime.Activity()
				secondTurnRestored = activity.Phase == agent.PhaseProvider && !activity.IsCompacting
			}
		case agent.CompactionEndEvent:
			ended := value
			compactionEnded.Store(true)
			compactions = append(compactions, ended)
		}
	})

	result, err := runtime.Run(context.Background(), "start")
	if err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	if implementation.CallCount() != 2 || result.ProviderTurns() != 2 || result.ToolExecutions() != 1 {
		t.Fatalf("calls/turns/tools = %d/%d/%d", implementation.CallCount(), result.ProviderTurns(), result.ToolExecutions())
	}
	if summaries.Load() != 1 {
		t.Fatalf("summaries = %d, want 1", summaries.Load())
	}
	if len(compactions) != 1 || compactions[0].Reason != agent.CompactionThreshold || compactions[0].WillRetry || compactions[0].Aborted {
		t.Fatalf("compaction = %#v", compactions)
	}
	if agentStarts != 1 || agentEnds != 1 || !secondTurnRestored {
		t.Fatalf("same-run lifecycle starts=%d ends=%d secondTurnRestored=%t", agentStarts, agentEnds, secondTurnRestored)
	}
}

func TestAgentSessionSkipsMidRunCompactionAfterTerminatingToolWithoutContinuation(t *testing.T) {
	model, err := newAgentModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "terminating-tool-compaction",
		Input: []provider.InputKind{provider.InputText}, ContextWindow: 100, MaxTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := provider.NewToolDefinition("finish", "finish", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	call, err := llm.NewToolCallBlock("call-finish", "finish", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	toolTurn, err := newAssistantToolUseMessage(
		[]llm.AssistantBlock{mustTextBlock(t, "finishing"), call}, mustUsage(t, 70, 5), agentTestEpoch,
	)
	if err != nil {
		t.Fatal(err)
	}
	implementation := newScriptedProvider(t, toolTurn)
	var summaries atomic.Uint32
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: model, ThinkingLevel: provider.ThinkingOff,
		Tool: &fakeTool{name: "finish", execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
			return agent.ToolOutput{Text: "12345678901234567890123456789012", Terminate: true}, nil
		}},
		Tools: []provider.ToolDefinition{definition}, ContextReserve: 20, ContextReserveSet: true,
		KeepRecentTokens: 10, KeepRecentTokensSet: true,
		Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
			summaries.Add(1)
			return session.SummaryOutput{Text: "unexpected checkpoint"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	compactions := 0
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if _, ok := event.(agent.CompactionStartEvent); ok {
			compactions++
		}
	})

	result, err := runtime.Run(context.Background(), "start")
	if err != nil {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	if implementation.CallCount() != 1 || result.ProviderTurns() != 1 || result.ToolExecutions() != 1 {
		t.Fatalf("calls/turns/tools = %d/%d/%d", implementation.CallCount(), result.ProviderTurns(), result.ToolExecutions())
	}
	if summaries.Load() != 0 || compactions != 0 {
		t.Fatalf("terminating tool triggered compaction: summaries=%d starts=%d", summaries.Load(), compactions)
	}
}

func TestAgentSessionAutoCompactionFailureSettlesOriginalResult(t *testing.T) {
	model := sessionTestModel(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "answer")), SessionManager: newSessionManager(t), Model: model, ThinkingLevel: provider.ThinkingOff,
		ContextWindow: 1, KeepRecentTokens: 1, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
			return session.SummaryOutput{}, errors.New("summary unavailable")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	var ends []agent.CompactionEndEvent
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if ended, ok := event.(agent.CompactionEndEvent); ok {
			ends = append(ends, ended)
		}
	})
	if result, err := runtime.Run(context.Background(), "go"); err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v), want original successful result", result, err)
	}
	if len(ends) != 1 || ends[0].ErrorMessage == "" || ends[0].Aborted {
		t.Fatalf("compaction end = %#v", ends)
	}
}

func TestAgentSessionManualCompactUsesSessionLifecycle(t *testing.T) {
	model := sessionTestModel(t)
	transcript := newSessionManager(t)
	for _, text := range []string{"old", "recent"} {
		message, err := llm.NewUserTextMessage(text, agentTestEpoch)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transcript.AppendLLMMessage(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: transcript, Model: model, ThinkingLevel: provider.ThinkingOff,
		KeepRecentTokens: 1, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
			return session.SummaryOutput{Text: "manual checkpoint"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if event.Type() == "compaction_start" || event.Type() == "compaction_end" || event.Type() == "agent_start" || event.Type() == "agent_settled" {
			types = append(types, string(event.Type()))
		}
	})
	if result, err := runtime.Compact(context.Background(), "focus"); err != nil || !result.Committed {
		t.Fatalf("Compact = (%#v, %v)", result, err)
	}
	if !sameStrings(types, []string{"compaction_start", "compaction_end"}) {
		t.Fatalf("manual lifecycle = %v", types)
	}
	if state := runtime.State(); state.Active.Phase() != agent.PhaseIdle {
		t.Fatalf("manual state = %s", state.Active.Phase())
	}
}

func TestAgentSessionResolvesSummaryFromCurrentRequestSnapshot(t *testing.T) {
	model := sessionTestModel(t)
	transcript, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{ID: "current-summary-session"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	for _, text := range []string{"old", "recent"} {
		message, messageErr := llm.NewUserTextMessage(text, agentTestEpoch)
		if messageErr != nil {
			t.Fatal(messageErr)
		}
		if _, appendErr := transcript.AppendLLMMessage(context.Background(), message); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	var captured agent.SummarizerResolveRequest
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: transcript, Model: model, ThinkingLevel: provider.ThinkingHigh,
		Stream: provider.StreamOptions{APIKey: "stale", SessionID: "stale-session"}, KeepRecentTokens: 1,
		ResolveStreamOptions: func(context.Context, provider.Model) (provider.StreamOptions, error) {
			return provider.StreamOptions{APIKey: "current-key"}, nil
		},
		ResolveSummarizer: func(_ context.Context, request agent.SummarizerResolveRequest) (session.Summarizer, error) {
			captured = request
			return contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
				return session.SummaryOutput{Text: "dynamic checkpoint"}, nil
			}), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Compact(context.Background(), "dynamic"); err != nil {
		t.Fatal(err)
	}
	if !captured.Model.Equal(model) || captured.ThinkingLevel != model.ClampThinkingLevel(provider.ThinkingHigh) ||
		captured.Stream.APIKey != "current-key" || captured.Stream.CacheRetention != "" ||
		captured.Stream.MaxTokens == nil || *captured.Stream.MaxTokens != agent.DefaultCompactionReserveTokens ||
		captured.Stream.SessionID != "stale-session" {
		t.Fatalf("summary request snapshot = %#v", captured)
	}
}

func TestAgentSessionAutoCompactionDoesNotStartWhenPreparationFails(t *testing.T) {
	model := sessionTestModel(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "answer")), SessionManager: newSessionManager(t), Model: model,
		ContextWindow: 1, KeepRecentTokens: 100_000,
		Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
			return session.SummaryOutput{Text: "must not run"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	var compactionEvents int
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if event.Type() == agent.CompactionStartEventType || event.Type() == agent.CompactionEndEventType {
			compactionEvents++
		}
	})
	if result, err := runtime.Run(context.Background(), "too small to compact"); err != nil || !result.Succeeded() {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if compactionEvents != 0 {
		t.Fatalf("preparation failure emitted %d compaction events", compactionEvents)
	}
}

func TestAgentSessionExplicitZeroCompactionReserveDoesNotUseDefaultOrModelMaxTokens(t *testing.T) {
	model, err := newAgentModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "independent-reserve",
		Input: []provider.InputKind{provider.InputText}, ContextWindow: 100, MaxTokens: 99,
	})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{mustTextBlock(t, "below configured threshold")}, llm.FinishStop, mustUsage(t, 50, 0), agentTestEpoch,
		llm.AssistantProvenance{Provider: model.Provider(), API: model.API(), Model: model.ID()},
	)
	if err != nil {
		t.Fatal(err)
	}
	var summaries atomic.Uint32
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, terminal), SessionManager: newSessionManager(t), Model: model,
		ContextReserve: 0, ContextReserveSet: true, KeepRecentTokens: 1,
		Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
			summaries.Add(1)
			return session.SummaryOutput{Text: "unexpected"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := runtime.Run(context.Background(), "remain below 100 tokens"); err != nil || !result.Succeeded() {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if summaries.Load() != 0 {
		t.Fatalf("model maxTokens replaced compaction reserve: summaries=%d", summaries.Load())
	}
}

func TestAgentSessionExplicitZeroKeepRecentTokensReachesCompactionCut(t *testing.T) {
	model := sessionTestModel(t)
	transcript := newSessionManager(t)
	for _, text := range []string{"summarize this", "keep this"} {
		message, err := llm.NewUserTextMessage(text, agentTestEpoch)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transcript.AppendLLMMessage(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: transcript, Model: model,
		KeepRecentTokens: 0, KeepRecentTokensSet: true,
		Summarizer: contextRetrySummarizerFunc(func(_ context.Context, input session.SummaryInput) (session.SummaryOutput, error) {
			if len(input.Messages) != 1 || len(input.RetainedTail) != 1 {
				t.Fatalf("explicit-zero compaction split = %d/%d", len(input.Messages), len(input.RetainedTail))
			}
			return session.SummaryOutput{Text: "zero keep checkpoint"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := runtime.Compact(context.Background(), "zero keep"); err != nil || !result.Committed {
		t.Fatalf("Compact() = %#v, %v", result, err)
	}
}

func TestAgentSessionManualCompactSharesGateAndAbort(t *testing.T) {
	model := sessionTestModel(t)
	transcript := newSessionManager(t)
	for _, text := range []string{"old", "recent"} {
		message, err := llm.NewUserTextMessage(text, agentTestEpoch)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transcript.AppendLLMMessage(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}
	entered := make(chan struct{})
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "must not run")), SessionManager: transcript, Model: model,
		ThinkingLevel: provider.ThinkingOff, KeepRecentTokens: 1, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Summarizer: contextRetrySummarizerFunc(func(ctx context.Context, _ session.SummaryInput) (session.SummaryOutput, error) {
			close(entered)
			<-ctx.Done()
			return session.SummaryOutput{}, context.Cause(ctx)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := runtime.Compact(context.Background(), "manual"); done <- err }()
	waitClosed(t, entered, "manual summarizer")
	if _, err := runtime.Run(context.Background(), "busy"); !errors.Is(err, agent.ErrBusy) {
		t.Fatalf("Run during manual compact = %v", err)
	}
	abortCtx, cancelAbort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if err := runtime.Abort(abortCtx); err != nil {
		cancelAbort()
		t.Fatalf("ordinary Abort waited for manual compaction: %v", err)
	}
	cancelAbort()
	select {
	case err := <-done:
		t.Fatalf("ordinary Abort cancelled manual compaction: %v", err)
	default:
	}
	runtime.AbortCompaction()
	if err := <-done; err == nil {
		t.Fatal("manual compact unexpectedly succeeded after Abort")
	}
	if err := runtime.WaitForIdle(context.Background()); err != nil {
		t.Fatalf("WaitForIdle after manual compact = %v", err)
	}
}

func TestAgentSessionContextCancelDuringRetryReturnsSettledResult(t *testing.T) {
	model := sessionTestModel(t)
	entered := make(chan struct{})
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, sessionHTTPFailure(t, 429)), SessionManager: newSessionManager(t), Model: model,
		ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Retry: agent.RetryPolicy{MaxAttempts: 2, Sleep: func(ctx context.Context, _ time.Duration) error {
			close(entered)
			<-ctx.Done()
			return context.Cause(ctx)
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct {
		result agent.Result
		err    error
	}, 1)
	go func() {
		result, err := runtime.Run(ctx, "retry")
		done <- struct {
			result agent.Result
			err    error
		}{result, err}
	}()
	waitClosed(t, entered, "retry sleep")
	cancel()
	got := <-done
	if got.err != nil {
		t.Fatalf("Run error = %v, want nil", got.err)
	}
	if terminal, ok := got.result.Terminal(); !ok || terminal.FinishReason() != llm.FinishError {
		t.Fatalf("Run result = %#v, want prior failure", got.result)
	}
}

func TestAgentSessionCloseTimeoutKeepsClosingUntilSecondClose(t *testing.T) {
	model := sessionTestModel(t)
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{Clock: func() time.Time { return agentTestEpoch }})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	step, err := provider.FactoryResponseStep(func(context.Context, provider.Request, uint64) (llm.AssistantTerminal, error) {
		close(entered)
		<-release
		return mustTextTerminal(t, "late"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := implementation.SetResponses([]provider.ScriptStep{step}); err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: implementation, SessionManager: newSessionManager(t), Model: model, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { _, err := runtime.Run(context.Background(), "block"); runDone <- err }()
	waitClosed(t, entered, "provider")
	deadline, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := runtime.Close(deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed Close = %v", err)
	}
	if _, err := runtime.Run(context.Background(), "rejected"); !errors.Is(err, agent.ErrInvalidRun) {
		t.Fatalf("Run while closing = %v", err)
	}
	close(release)
	<-runDone
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

func TestAgentSessionPreflightFailureDoesNotEmitSettled(t *testing.T) {
	model := sessionTestModel(t)
	for _, test := range []struct {
		name string
		run  func(*agent.AgentSession) error
		cfg  func() func() time.Time
	}{
		{
			name: "invalid_text",
			run: func(runtime *agent.AgentSession) error {
				_, err := runtime.Run(context.Background(), string([]byte{0xff}))
				return err
			},
			cfg: func() func() time.Time { return func() time.Time { return agentTestEpoch } },
		},
		{
			name: "invalid_rich",
			run: func(runtime *agent.AgentSession) error {
				_, err := runtime.RunContent(context.Background(), []llm.UserContentBlock{nil})
				return err
			},
			cfg: func() func() time.Time { return func() time.Time { return agentTestEpoch } },
		},
		{
			name: "clock_failure",
			run: func(runtime *agent.AgentSession) error {
				_, err := runtime.Run(context.Background(), "valid")
				return err
			},
			cfg: func() func() time.Time { return func() time.Time { return time.Time{} } },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, err := agent.NewSession(agent.SessionConfig{
				Provider: newScriptedProvider(t, mustTextTerminal(t, "unused")), SessionManager: newSessionManager(t), Model: model,
				ThinkingLevel: provider.ThinkingOff, Now: test.cfg(), SettlementTimeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			var controls []string
			runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
				switch event.Type() {
				case "agent_start", "agent_end", "agent_settled":
					controls = append(controls, string(event.Type()))
				}
			})
			if err := test.run(runtime); err == nil {
				t.Fatal("preflight run unexpectedly succeeded")
			}
			if len(controls) != 0 {
				t.Fatalf("preflight lifecycle = %v, want no controls", controls)
			}
		})
	}
}

func TestAgentSessionSteerClockReentryDoesNotDeadlock(t *testing.T) {
	model := sessionTestModel(t)
	var runtime *agent.AgentSession
	clockEntered := make(chan struct{})
	closed := make(chan error, 1)
	runtimeConfig := agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: model, ThinkingLevel: provider.ThinkingOff,
		SettlementTimeout: time.Second,
		Now: func() time.Time {
			close(clockEntered)
			closed <- runtime.Close(context.Background())
			return agentTestEpoch
		},
	}
	var err error
	runtime, err = agent.NewSession(runtimeConfig)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runtime.Steer("reentrant") }()
	waitClosed(t, clockEntered, "clock callback")
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close from clock = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close from clock deadlocked")
	}
	select {
	case err := <-done:
		if !errors.Is(err, agent.ErrInvalidRun) {
			t.Fatalf("Steer after reentrant Close = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Steer deadlocked")
	}
}

func TestAgentSessionObserverSubscriptionOrder(t *testing.T) {
	model := sessionTestModel(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: model, ThinkingLevel: provider.ThinkingOff,
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	var order []int
	for index := 1; index <= 3; index++ {
		value := index
		runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
			if event.Type() == "queue_update" {
				order = append(order, value)
			}
		})
	}
	if err := runtime.FollowUp("ordered"); err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("observer order = %v", order)
	}
}

func TestNewSessionRejectsNilDependenciesAndInvalidRetry(t *testing.T) {
	model := sessionTestModel(t)
	if _, err := agent.NewSession(agent.SessionConfig{Model: model}); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("nil dependencies error = %v", err)
	}
	if _, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: model,
		Retry: agent.RetryPolicy{InitialDelay: -time.Second},
	}); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("invalid retry error = %v", err)
	}
	if _, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: model, SettlementTimeout: -time.Second,
	}); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("negative settlement error = %v", err)
	}
}

func TestAgentSessionNilReceiverMutatorsReturnInvalidRun(t *testing.T) {
	model := sessionTestModel(t)
	var runtime *agent.AgentSession
	for _, call := range []struct {
		name string
		fn   func() error
	}{
		{name: "model", fn: func() error { return runtime.SetModel(model) }},
		{name: "thinking", fn: func() error { return runtime.SetThinkingLevel(provider.ThinkingOff) }},
		{name: "prompt", fn: func() error { return runtime.SetSystemPrompt("x") }},
		{name: "tools", fn: func() error { return runtime.SetTools(nil, nil) }},
		{name: "steer", fn: func() error { return runtime.Steer("x") }},
		{name: "follow", fn: func() error { return runtime.FollowUp("x") }},
	} {
		t.Run(call.name, func(t *testing.T) {
			if err := call.fn(); !errors.Is(err, agent.ErrInvalidRun) {
				t.Fatalf("nil receiver error = %v", err)
			}
		})
	}
}

func TestAgentSessionPrePromptCompactionRequiresMatchingDurableProvenance(t *testing.T) {
	for _, test := range []struct {
		name       string
		provenance llm.AssistantProvenance
		has        bool
		wantStarts int
	}{
		{name: "same provider and model", provenance: llm.AssistantProvenance{Provider: "new", API: "scripted", Model: "model"}, has: true, wantStarts: 1},
		{name: "old provider and model", provenance: llm.AssistantProvenance{Provider: "old", API: "scripted", Model: "old-model"}, has: true, wantStarts: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			transcript := newSessionManager(t)
			oldUser, err := llm.NewUserTextMessage("old request", agentTestEpoch)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := transcript.AppendLLMMessage(context.Background(), oldUser); err != nil {
				t.Fatal(err)
			}
			storedProvenance := llm.AssistantProvenance{Provider: "stored", API: "scripted", Model: "stored-model"}
			if test.has {
				storedProvenance = test.provenance
			}
			storedAssistant := mustTextTerminalWithProvenance(t, "old reply", storedProvenance)
			if _, err := transcript.AppendLLMMessage(context.Background(), storedAssistant); err != nil {
				t.Fatal(err)
			}
			model, err := newTestModel("new", "scripted", "model")
			if err != nil {
				t.Fatal(err)
			}
			runtime, err := agent.NewSession(agent.SessionConfig{
				Provider: newScriptedProvider(t, mustTextTerminal(t, "new reply")), SessionManager: transcript, Model: model,
				ThinkingLevel: provider.ThinkingOff, ContextWindow: 1, KeepRecentTokens: 1,
				Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
					return session.SummaryOutput{Text: "checkpoint"}, nil
				}),
				Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			starts, agentStarted := 0, false
			runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
				if event.Type() == "agent_start" {
					agentStarted = true
				}
				if event.Type() == "compaction_start" && !agentStarted {
					starts++
				}
			})
			if _, err := runtime.Run(context.Background(), "new request"); err != nil {
				t.Fatal(err)
			}
			if starts != test.wantStarts {
				t.Fatalf("pre-prompt compaction starts = %d, want %d", starts, test.wantStarts)
			}
		})
	}
}

func TestAgentSessionSummarizationRetryPayloadUsesRetryBudgetAndCompactionSource(t *testing.T) {
	transcript := newSessionManager(t)
	old, err := llm.NewUserTextMessage("old request", agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.AppendLLMMessage(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	oldAssistant := mustTextTerminalWithProvenance(t, "old reply", llm.AssistantProvenance{Provider: "scripted", API: "scripted", Model: "model"})
	if _, err := transcript.AppendLLMMessage(context.Background(), oldAssistant); err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: transcript, Model: sessionTestModel(t), ThinkingLevel: provider.ThinkingOff,
		Summarizer: sessionRetrySummarizer{}, KeepRecentTokens: 1, Retry: agent.RetryPolicy{MaxAttempts: 3},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	var events []agent.SessionEvent
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch event.Type() {
		case "summarization_retry_scheduled", "summarization_retry_attempt_start", "summarization_retry_finished":
			events = append(events, event)
		}
	})
	if _, err := runtime.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("retry events = %#v", events)
	}
	scheduled, scheduledOK := events[0].(agent.SessionSummarizationRetryScheduledEvent)
	if !scheduledOK || scheduled.Reason != agent.CompactionManual || scheduled.Attempt != 1 || scheduled.MaxAttempts != 2 || scheduled.Delay != time.Millisecond || scheduled.FailureKind != provider.FailureHTTPStatus || scheduled.HTTPStatus != 503 || scheduled.ErrorMessage != "summary unavailable" {
		t.Fatalf("scheduled payload = %#v", scheduled)
	}
	attempt, attemptOK := events[1].(agent.SessionSummarizationRetryAttemptEvent)
	if !attemptOK || attempt.Source != "compaction" || attempt.Reason != agent.CompactionManual {
		t.Fatalf("attempt payload = %#v", attempt)
	}
	ended, endedOK := events[2].(agent.SessionSummarizationRetryFinishedEvent)
	if !endedOK || ended.FinishReason != provider.RetryFinishSucceeded || !ended.Succeeded {
		t.Fatalf("finished payload = %#v", ended)
	}
}

func TestAgentSessionObserverEventsDoNotShareSlices(t *testing.T) {
	definition, err := provider.NewToolDefinition("echo", "echoes", true, []byte(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	implementation := newScriptedProvider(t, mustToolUseTerminal(t, "call", "echo", []byte(`{"value":1}`)), mustTextTerminal(t, "done"), mustTextTerminal(t, "steered"), mustTextTerminal(t, "followed"))
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t), ThinkingLevel: provider.ThinkingOff,
		Tool: &fakeTool{name: "echo", execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
			return agent.ToolOutput{Text: "ok"}, nil
		}}, Tools: []provider.ToolDefinition{definition},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	var secondStarts, secondEnds, secondQueue int
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch value := event.(type) {
		case agent.ToolExecutionStartEvent:
			if len(value.Arguments) != 0 {
				value.Arguments[0] = '!'
			}
		case agent.TurnEndEvent:
			if len(value.ToolResults) != 0 {
				value.ToolResults[0] = nil
			}
		case agent.SessionAgentEndEvent:
			if len(value.Messages) != 0 {
				value.Messages[0] = nil
			}
		case agent.SessionQueueUpdateEvent:
			value.Steering = nil
			value.FollowUp = nil
		}
	})
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch value := event.(type) {
		case agent.ToolExecutionStartEvent:
			if string(value.Arguments) != `{"value":1}` {
				t.Errorf("tool event was mutated: args=%q", value.Arguments)
			}
			secondStarts++
		case agent.TurnEndEvent:
			if len(value.ToolResults) == 0 {
				return
			}
			if len(value.ToolResults) != 1 || value.ToolResults[0] == nil {
				t.Errorf("tool results were mutated: %#v", value.ToolResults)
			}
			secondEnds++
		case agent.SessionAgentEndEvent:
			if len(value.Messages) == 0 || value.Messages[0] == nil {
				t.Errorf("messages were mutated: %#v", value.Messages)
			}
		case agent.SessionQueueUpdateEvent:
			if len(value.Steering)+len(value.FollowUp) == 0 {
				return
			}
			secondQueue++
		}
	})
	if err := runtime.Steer("queued steering"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.FollowUp("queued follow-up"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if secondStarts != 1 || secondEnds == 0 || secondQueue < 2 {
		t.Fatalf("observer coverage starts=%d ends=%d queues=%d", secondStarts, secondEnds, secondQueue)
	}
}

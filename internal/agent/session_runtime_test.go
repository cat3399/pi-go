package agent_test

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

type sessionRetrySummarizer struct{}

// noProvenanceTranscript models a legacy transcript context whose durable
// messages predate assistant provenance. Appends still go to the real Session.
type noProvenanceTranscript struct{ *session.Session }

func (t noProvenanceTranscript) Context() session.Context {
	return session.NewContext(t.Session.Context().Messages())
}

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
	modelA, err := provider.NewModel(provider.ModelSpec{Provider: "scripted", API: "scripted", ID: "model-a", Reasoning: true, Input: []provider.InputKind{provider.InputText}})
	if err != nil {
		t.Fatal(err)
	}
	modelB, err := provider.NewModel(provider.ModelSpec{Provider: "scripted", API: "scripted", ID: "model-b", Reasoning: true, Input: []provider.InputKind{provider.InputText}})
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
	transcript := newSession(t)
	runtime, err = agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, Transcript: transcript, Model: modelA, ThinkingLevel: provider.ThinkingOff,
		SystemPrompt: "old system prompt", Tool: tool, Tools: []provider.ToolDefinition{definition},
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
}

func TestAgentSessionRoutesEachModelToItsRegisteredAdapter(t *testing.T) {
	modelA, err := provider.NewModelRef("one", "api-one", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	modelB, err := provider.NewModelRef("two", "api-two", "model-b")
	if err != nil {
		t.Fatal(err)
	}
	adapterA := newScriptedProvider(t, mustTextTerminal(t, "one"))
	adapterB := newScriptedProvider(t, mustTextTerminal(t, "two"))
	router, err := provider.NewRouter(map[string]provider.Provider{"api-one": adapterA, "api-two": adapterB})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: router, Transcript: newSession(t), Model: modelA, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second})
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
	unsupported, err := provider.NewModelRef("three", "api-three", "model-c")
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetModel(unsupported); err == nil {
		t.Fatal("SetModel accepted an unregistered adapter route")
	}
}

func TestAgentSessionPreservesRichToolResultWhenLegacyTextIsEmpty(t *testing.T) {
	model, err := provider.NewModelRef("scripted", "scripted", "model")
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
	transcript := newSession(t)
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: providerImpl, Transcript: transcript, Model: model, ThinkingLevel: provider.ThinkingOff, Tool: tool, Tools: []provider.ToolDefinition{definition}, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := runtime.Run(context.Background(), "go"); err != nil || !result.Succeeded() {
		t.Fatalf("run = (%#v, %v)", result, err)
	}
	messages := transcript.Context().Messages()
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(messages))
	}
	resultMessage, ok := messages[2].(llm.ToolResultContentMessage)
	if !ok || len(resultMessage.Content()) != 1 {
		t.Fatalf("rich result = %#v", messages[2])
	}
}

func TestAgentSessionKeepsConversationAcrossPrompts(t *testing.T) {
	model, err := provider.NewModelRef("scripted", "scripted", "model")
	if err != nil {
		t.Fatal(err)
	}
	providerImpl := newScriptedProvider(t, mustTextTerminal(t, "first"), mustTextTerminal(t, "second"))
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, Transcript: newSession(t), Model: model, ThinkingLevel: provider.ThinkingOff,
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
	if state := runtime.State(); state.Active.Phase() != agent.PhaseIdle || state.Model.ID() != "model" {
		t.Fatalf("state after settled prompts = %#v", state)
	}
}

func TestAgentSessionRunContentPreservesImageInTranscriptAndRequest(t *testing.T) {
	model, err := provider.NewModel(provider.ModelSpec{Provider: "scripted", API: "scripted", ID: "vision", Input: []provider.InputKind{provider.InputText, provider.InputImage}})
	if err != nil {
		t.Fatal(err)
	}
	implementation := newScriptedProvider(t, mustTextTerminal(t, "seen"))
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: implementation, Transcript: newSession(t), Model: model, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second})
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
	model, err := provider.NewModel(provider.ModelSpec{Provider: "scripted", API: "scripted", ID: "vision", Input: []provider.InputKind{provider.InputText, provider.InputImage}})
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
	transcript := newSession(t)
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: implementation, Transcript: transcript, Model: model, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second})
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
	if _, err := runtime.Continue(context.Background()); !errors.Is(err, agent.ErrBusy) {
		t.Fatalf("concurrent Continue error = %v, want ErrBusy", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first RunContent = %v", err)
	}
	messages := transcript.Context().Messages()
	if len(messages) != 2 {
		t.Fatalf("durable messages = %#v, want only initial user and assistant", messages)
	}
	if got := messageText(t, messages[0]); got != "first" {
		t.Fatalf("durable user message = %#v", messages[0])
	}
}

func TestAgentSessionQueuesRichContentInPriorityOrder(t *testing.T) {
	model, err := provider.NewModel(provider.ModelSpec{Provider: "scripted", API: "scripted", ID: "vision", Input: []provider.InputKind{provider.InputText, provider.InputImage}})
	if err != nil {
		t.Fatal(err)
	}
	implementation := newScriptedProvider(t, mustTextTerminal(t, "first"), mustTextTerminal(t, "follow"))
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: implementation, Transcript: newSession(t), Model: model, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second})
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
	if got := requests[1].Messages()[3].(llm.UserTextMessage).Content()[0].Text(); got != "after" {
		t.Fatalf("follow request tail = %q", got)
	}
}

func TestAgentSessionQueueUpdatesOnlyForActualMutationAndDrain(t *testing.T) {
	model, err := provider.NewModelRef("scripted", "scripted", "model")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "initial"), mustTextTerminal(t, "follow")), Transcript: newSession(t), Model: model,
		ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	type snapshot struct{ steering, followUp int }
	var updates []snapshot
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if event.Type == "queue_update" {
			updates = append(updates, snapshot{len(event.Steering), len(event.FollowUp)})
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

func TestAgentSessionEmitsQueueDrainBeforeQueuedMessageLifecycle(t *testing.T) {
	model, err := provider.NewModelRef("scripted", "scripted", "model")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "done")), Transcript: newSession(t), Model: model,
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
		switch event.Type {
		case "queue_update":
			lifecycle = append(lifecycle, "queue")
		case "message_start":
			if event.Message != nil && event.Message.Role() == llm.RoleUser {
				lifecycle = append(lifecycle, "user:"+messageText(t, event.Message))
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

func TestAgentSessionEmitsOrderedLifecycleAndRejectsAfterClose(t *testing.T) {
	model, err := provider.NewModelRef("scripted", "scripted", "model")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: newScriptedProvider(t, mustTextTerminal(t, "ok")), Transcript: newSession(t), Model: model, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if event.Type != "" {
			types = append(types, event.Type)
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
	model, err := provider.NewModelRef("scripted", "scripted", "model")
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
	toolUse, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{call}, mustUsage(t, 2, 1), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, toolUse, mustTextTerminal(t, "ok")), Transcript: newSession(t),
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
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) { types = append(types, event.Type) })
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
		Provider: newScriptedProvider(t), Transcript: newSession(t), Model: sessionTestModel(t),
		ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) { types = append(types, event.Type) })
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
	modelA, err := provider.NewModelRef("one", "api-one", "a")
	if err != nil {
		t.Fatal(err)
	}
	modelB, err := provider.NewModelRef("two", "api-two", "b")
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
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: router, Transcript: newSession(t), Model: modelA, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		ResolveStreamOptions: func(_ context.Context, model provider.ModelRef) (provider.StreamOptions, error) {
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

func TestAgentSessionPersistsRetryFailureButProjectsItOutOfRetryContext(t *testing.T) {
	model, err := provider.NewModelRef("scripted", "scripted", "model")
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
	failed, err := llm.NewAssistantFailureMessageWithFailure(nil, llm.FinishError, failure, llm.Usage{}, agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	implementation := newScriptedProvider(t, failed, mustTextTerminal(t, "recovered"), mustTextTerminal(t, "later"))
	transcript := newSession(t)
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: implementation, Transcript: transcript, Model: model, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second, Retry: agent.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "retry")
	if err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	messages := transcript.Context().Messages()
	if len(messages) != 3 {
		t.Fatalf("durable messages = %#v", messages)
	}
	if _, ok := messages[1].(llm.AssistantFailureMessage); !ok {
		t.Fatalf("retry failure was not persisted: %#v", messages)
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
	model, err := provider.NewModelRef("scripted", "scripted", "model")
	if err != nil {
		t.Fatal(err)
	}
	failure, err := llm.NewFailure("provider failed", errors.New("fixture provider failure"))
	if err != nil {
		t.Fatal(err)
	}
	failedTerminal, err := llm.NewAssistantFailureMessageWithFailure(nil, llm.FinishError, failure, mustUsage(t, 0, 0), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	providerImpl := newScriptedProvider(t, failedTerminal, mustTextTerminal(t, "recovered"))
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, Transcript: newSession(t), Model: model, ThinkingLevel: provider.ThinkingOff,
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

func TestAgentSessionAbortLeavesQueuedPromptAndSettles(t *testing.T) {
	model, err := provider.NewModelRef("scripted", "scripted", "model")
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
	if err := providerImpl.SetResponses([]provider.ScriptStep{step}); err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: providerImpl, Transcript: newSession(t), Model: model, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second})
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
	if err := runtime.Steer("keep queued"); err != nil {
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
	if len(steering) != 1 {
		t.Fatalf("queued steer lost after abort: %#v", steering)
	}
}

func sessionTestModel(t *testing.T) provider.ModelRef {
	t.Helper()
	model, err := provider.NewModelRef("scripted", "scripted", "model")
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
	terminal, err := llm.NewAssistantFailureMessageWithFailure(nil, llm.FinishError, message, mustUsage(t, 0, 0), agentTestEpoch)
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
				Provider: implementation, Transcript: newSession(t), Model: model, ThinkingLevel: provider.ThinkingOff,
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
		Provider: implementation, Transcript: newSession(t), Model: model, ThinkingLevel: provider.ThinkingOff,
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
		if event.Type == "agent_end" && !queued && !event.WillRetry {
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
		switch event.Type {
		case "agent_start", "agent_end", "auto_retry_start", "auto_retry_end", "agent_settled":
			controls = append(controls, event.Type)
			if event.Type == "agent_end" {
				ends = append(ends, event.WillRetry)
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
	var retryStart, retryEnd *agent.SessionEvent
	for index := range events {
		event := &events[index]
		if event.Type == "auto_retry_start" {
			retryStart = event
		}
		if event.Type == "auto_retry_end" {
			retryEnd = event
		}
	}
	if retryStart == nil || retryStart.RetryAttempt != 1 || retryStart.RetryMaxAttempts != 1 || retryStart.RetryErrorMessage != "http failure" {
		t.Fatalf("retry start payload = %#v", retryStart)
	}
	if retryEnd == nil || !retryEnd.RetrySucceeded || retryEnd.RetryAttempt != 1 || retryEnd.RetryMaxAttempts != 1 {
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
		Provider: implementation, Transcript: newSession(t), Model: model, ThinkingLevel: provider.ThinkingOff,
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
		Provider: newScriptedProvider(t, sessionHTTPFailure(t, 429), sessionHTTPFailure(t, 429)), Transcript: newSession(t),
		Model: model, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Retry: agent.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	var controls []agent.SessionEvent
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch event.Type {
		case "agent_start", "agent_end", "auto_retry_start", "auto_retry_end", "agent_settled":
			controls = append(controls, event)
		}
	})
	if result, err := runtime.Run(context.Background(), "fail twice"); err != nil || result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	types := make([]string, len(controls))
	for index, event := range controls {
		types[index] = event.Type
	}
	want := []string{"agent_start", "agent_end", "auto_retry_start", "agent_start", "agent_end", "auto_retry_end", "agent_settled"}
	if !sameStrings(types, want) {
		t.Fatalf("lifecycle = %v, want %v", types, want)
	}
	ended := controls[5]
	if ended.RetrySucceeded || ended.RetryAttempt != 1 || ended.RetryMaxAttempts != 1 || ended.FinalError != "http failure" {
		t.Fatalf("final retry event = %#v", ended)
	}
}

func TestAgentSessionRetrySeriesSpansIntermediateFailures(t *testing.T) {
	model := sessionTestModel(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, sessionHTTPFailure(t, 429), sessionHTTPFailure(t, 429), mustTextTerminal(t, "third")), Transcript: newSession(t),
		Model: model, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Retry: agent.RetryPolicy{MaxAttempts: 3, Sleep: func(context.Context, time.Duration) error { return nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	var controls []agent.SessionEvent
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch event.Type {
		case "agent_end", "auto_retry_start", "auto_retry_end":
			controls = append(controls, event)
		}
	})
	if result, err := runtime.Run(context.Background(), "three"); err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	types := make([]string, len(controls))
	for index, event := range controls {
		types[index] = event.Type
	}
	want := []string{"agent_end", "auto_retry_start", "agent_end", "auto_retry_start", "auto_retry_end", "agent_end"}
	if !sameStrings(types, want) {
		t.Fatalf("retry series events = %v, want %v", types, want)
	}
	if !controls[0].WillRetry || !controls[2].WillRetry || controls[5].WillRetry || !controls[4].RetrySucceeded {
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
		Provider:   newScriptedProvider(t, sessionHTTPFailure(t, 429), mustToolUseTerminal(t, "call", "tool", []byte(`{}`)), mustTextTerminal(t, "done")),
		Transcript: newSession(t), Model: model, ThinkingLevel: provider.ThinkingOff, Tool: tool, Tools: []provider.ToolDefinition{definition},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Retry: agent.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch event.Type {
		case "agent_end", "auto_retry_start", "auto_retry_end":
			types = append(types, event.Type)
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

func TestAgentSessionPrePromptThresholdCompactsWithoutExtraProvider(t *testing.T) {
	model := sessionTestModel(t)
	transcript := newSession(t)
	oldUser, err := llm.NewUserTextMessage("old request", agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Append(context.Background(), oldUser, session.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	oldAssistant := mustTextTerminal(t, "old reply")
	if _, err := transcript.Append(context.Background(), oldAssistant, session.AppendOptions{Assistant: session.AssistantProvenance{Provider: "scripted", API: "scripted", Model: "model", Cost: session.ZeroUsageCost()}}); err != nil {
		t.Fatal(err)
	}
	var summaries atomic.Uint32
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "new reply")), Transcript: transcript, Model: model, ThinkingLevel: provider.ThinkingOff,
		ContextWindow: 1, KeepRecentTokens: 1, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
			summaries.Add(1)
			return session.SummaryOutput{Text: "checkpoint"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	var compactTypes []string
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if event.Type == "compaction_start" || event.Type == "compaction_end" {
			compactTypes = append(compactTypes, event.Type)
		}
	})
	if result, err := runtime.Run(context.Background(), "new request"); err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	if summaries.Load() != 2 || !sameStrings(compactTypes, []string{"compaction_start", "compaction_end", "compaction_start", "compaction_end"}) {
		t.Fatalf("summaries/events = %d/%v", summaries.Load(), compactTypes)
	}
}

func TestAgentSessionOverflowCompactsAndContinuesWithoutRuntimeFailure(t *testing.T) {
	model := sessionTestModel(t)
	transcript := newSession(t)
	contextFailure, err := provider.NewProviderFailure(provider.ProviderFailureSpec{Kind: provider.FailureContextOverflow, Message: "overflow", Cause: errors.New("overflow")})
	if err != nil {
		t.Fatal(err)
	}
	failure, err := llm.NewFailure("overflow", contextFailure)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := llm.NewAssistantFailureMessageWithFailure(nil, llm.FinishError, failure, mustUsage(t, 0, 0), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	implementation := newScriptedProvider(t, failed, mustTextTerminal(t, "recovered"))
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, Transcript: transcript, Model: model, ThinkingLevel: provider.ThinkingOff,
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

func TestAgentSessionAutoCompactionFailureSettlesOriginalResult(t *testing.T) {
	model := sessionTestModel(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "answer")), Transcript: newSession(t), Model: model, ThinkingLevel: provider.ThinkingOff,
		ContextWindow: 1, KeepRecentTokens: 1, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
		Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
			return session.SummaryOutput{}, errors.New("summary unavailable")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	var ends []agent.SessionEvent
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if event.Type == "compaction_end" {
			ends = append(ends, event)
		}
	})
	if result, err := runtime.Run(context.Background(), "go"); err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v), want original successful result", result, err)
	}
	if len(ends) != 1 || ends[0].CompactionErrorMessage == "" || ends[0].CompactionAborted {
		t.Fatalf("compaction end = %#v", ends)
	}
}

func TestAgentSessionManualCompactUsesSessionLifecycle(t *testing.T) {
	model := sessionTestModel(t)
	transcript := newSession(t)
	for _, text := range []string{"old", "recent"} {
		message, err := llm.NewUserTextMessage(text, agentTestEpoch)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transcript.Append(context.Background(), message, session.AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), Transcript: transcript, Model: model, ThinkingLevel: provider.ThinkingOff,
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
		if event.Type == "compaction_start" || event.Type == "compaction_end" || event.Type == "agent_start" || event.Type == "agent_settled" {
			types = append(types, event.Type)
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

func TestAgentSessionManualCompactSharesGateAndAbort(t *testing.T) {
	model := sessionTestModel(t)
	transcript := newSession(t)
	for _, text := range []string{"old", "recent"} {
		message, err := llm.NewUserTextMessage(text, agentTestEpoch)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transcript.Append(context.Background(), message, session.AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	entered := make(chan struct{})
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "must not run")), Transcript: transcript, Model: model,
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
	if err := runtime.Abort(context.Background()); err != nil {
		t.Fatalf("Abort manual compact = %v", err)
	}
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
		Provider: newScriptedProvider(t, sessionHTTPFailure(t, 429)), Transcript: newSession(t), Model: model,
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
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: implementation, Transcript: newSession(t), Model: model, ThinkingLevel: provider.ThinkingOff, Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second})
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
				Provider: newScriptedProvider(t, mustTextTerminal(t, "unused")), Transcript: newSession(t), Model: model,
				ThinkingLevel: provider.ThinkingOff, Now: test.cfg(), SettlementTimeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			var controls []string
			runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
				switch event.Type {
				case "agent_start", "agent_end", "agent_settled":
					controls = append(controls, event.Type)
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
		Provider: newScriptedProvider(t), Transcript: newSession(t), Model: model, ThinkingLevel: provider.ThinkingOff,
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
		Provider: newScriptedProvider(t), Transcript: newSession(t), Model: model, ThinkingLevel: provider.ThinkingOff,
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	var order []int
	for index := 1; index <= 3; index++ {
		value := index
		runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
			if event.Type == "queue_update" {
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
		Provider: newScriptedProvider(t), Transcript: newSession(t), Model: model,
		Retry: agent.RetryPolicy{InitialDelay: -time.Second},
	}); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("invalid retry error = %v", err)
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
		provenance session.AssistantProvenance
		has        bool
		wantStarts int
	}{
		{name: "same provider and model", provenance: session.AssistantProvenance{Provider: "new", API: "scripted", Model: "model", Cost: session.ZeroUsageCost()}, has: true, wantStarts: 1},
		{name: "old provider and model", provenance: session.AssistantProvenance{Provider: "old", API: "scripted", Model: "old-model", Cost: session.ZeroUsageCost()}, has: true, wantStarts: 0},
		{name: "missing provenance", wantStarts: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			transcript := newSession(t)
			oldUser, err := llm.NewUserTextMessage("old request", agentTestEpoch)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := transcript.Append(context.Background(), oldUser, session.AppendOptions{}); err != nil {
				t.Fatal(err)
			}
			options := session.AppendOptions{Assistant: session.AssistantProvenance{Provider: "stored", API: "scripted", Model: "stored-model", Cost: session.ZeroUsageCost()}}
			if test.has {
				options.Assistant = test.provenance
			}
			if _, err := transcript.Append(context.Background(), mustTextTerminal(t, "old reply"), options); err != nil {
				t.Fatal(err)
			}
			var durable agent.Transcript = transcript
			if !test.has {
				durable = noProvenanceTranscript{Session: transcript}
			}
			model, err := provider.NewModelRef("new", "scripted", "model")
			if err != nil {
				t.Fatal(err)
			}
			runtime, err := agent.NewSession(agent.SessionConfig{
				Provider: newScriptedProvider(t, mustTextTerminal(t, "new reply")), Transcript: durable, Model: model,
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
				if event.Type == "agent_start" {
					agentStarted = true
				}
				if event.Type == "compaction_start" && !agentStarted {
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
	transcript := newSession(t)
	old, err := llm.NewUserTextMessage("old request", agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Append(context.Background(), old, session.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.Append(context.Background(), mustTextTerminal(t, "old reply"), session.AppendOptions{Assistant: session.AssistantProvenance{Provider: "scripted", API: "scripted", Model: "model", Cost: session.ZeroUsageCost()}}); err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), Transcript: transcript, Model: sessionTestModel(t), ThinkingLevel: provider.ThinkingOff,
		Summarizer: sessionRetrySummarizer{}, KeepRecentTokens: 1, Retry: agent.RetryPolicy{MaxAttempts: 3},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	var events []agent.SessionEvent
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch event.Type {
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
	if scheduled := events[0]; scheduled.SummarizationSource != "compaction" || scheduled.CompactionReason != agent.CompactionManual || scheduled.RetryAttempt != 1 || scheduled.RetryMaxAttempts != 2 || scheduled.RetryDelay != time.Millisecond || scheduled.RetryFailureKind != provider.FailureHTTPStatus || scheduled.RetryHTTPStatus != 503 || scheduled.RetryErrorMessage != "summary unavailable" {
		t.Fatalf("scheduled payload = %#v", scheduled)
	}
	if attempt := events[1]; attempt.SummarizationSource != "compaction" || attempt.CompactionReason != agent.CompactionManual || attempt.RetryAttempt != 1 || attempt.RetryMaxAttempts != 2 {
		t.Fatalf("attempt payload = %#v", attempt)
	}
	if ended := events[2]; ended.RetryFinishReason != provider.RetryFinishSucceeded || !ended.RetrySucceeded || ended.RetryMaxAttempts != 2 {
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
		Provider: implementation, Transcript: newSession(t), Model: sessionTestModel(t), ThinkingLevel: provider.ThinkingOff,
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
		switch event.Type {
		case "tool_execution_start":
			if len(event.Event.ToolArguments) != 0 {
				event.Event.ToolArguments[0] = '!'
			}
			if len(event.State.Tools) != 0 {
				event.State.Tools = event.State.Tools[:0]
			}
		case "turn_end":
			if len(event.ToolResults) != 0 {
				event.ToolResults[0] = nil
			}
		case "agent_end":
			if len(event.Messages) != 0 {
				event.Messages[0] = nil
			}
		case "queue_update":
			event.Steering = nil
			event.FollowUp = nil
		}
	})
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch event.Type {
		case "tool_execution_start":
			if string(event.Event.ToolArguments) != `{"value":1}` || len(event.State.Tools) != 1 {
				t.Errorf("tool event was mutated: args=%q tools=%d", event.Event.ToolArguments, len(event.State.Tools))
			}
			secondStarts++
		case "turn_end":
			if len(event.ToolResults) == 0 {
				return
			}
			if len(event.ToolResults) != 1 || event.ToolResults[0] == nil {
				t.Errorf("tool results were mutated: %#v", event.ToolResults)
			}
			secondEnds++
		case "agent_end":
			if len(event.Messages) == 0 || event.Messages[0] == nil {
				t.Errorf("messages were mutated: %#v", event.Messages)
			}
		case "queue_update":
			if len(event.Steering)+len(event.FollowUp) == 0 {
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

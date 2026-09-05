package agent_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestStatefulAgentKeepsInMemoryConversationAndResetPreservesConfiguration(t *testing.T) {
	model := sessionTestModel(t)
	implementation := newScriptedProvider(t, mustTextTerminal(t, "first"), mustTextTerminal(t, "second"))
	runtime, err := agent.New(agent.Config{
		Provider: implementation, Model: model, ThinkingLevel: provider.ThinkingOff,
		SystemPrompt: "system", Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	var endScopes []int
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
		if ended, ok := event.(agent.AgentEndEvent); ok {
			endScopes = append(endScopes, len(ended.Messages))
		}
	})
	if result, runErr := runtime.Run(context.Background(), "one"); runErr != nil || !result.Succeeded() {
		t.Fatalf("first Run = (%#v, %v)", result, runErr)
	}
	if result, runErr := runtime.Run(context.Background(), "two"); runErr != nil || !result.Succeeded() {
		t.Fatalf("second Run = (%#v, %v)", result, runErr)
	}
	state := runtime.State()
	conversation, err := agentmsg.ConvertToLLM(state.Messages())
	if err != nil {
		t.Fatal(err)
	}
	if got := messageRoles(conversation); !reflect.DeepEqual(got, []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant}) {
		t.Fatalf("state roles = %v", got)
	}
	if _, ok := conversation[0].(llm.UserContentMessage); !ok {
		t.Fatalf("string prompt normalized to %T, want UserContentMessage", conversation[0])
	}
	if !reflect.DeepEqual(endScopes, []int{2, 2}) {
		t.Fatalf("agent_end invocation message scopes = %v", endScopes)
	}
	if state.IsStreaming() || state.Phase() != agent.PhaseIdle || state.Model().ID() != model.ID() || state.SystemPrompt() != "system" {
		t.Fatalf("settled state = phase %s streaming %t model %q system %q", state.Phase(), state.IsStreaming(), state.Model().ID(), state.SystemPrompt())
	}
	if err := runtime.Steer("queued"); err != nil {
		t.Fatal(err)
	}
	runtime.Reset()
	state = runtime.State()
	if len(state.Messages()) != 0 || runtime.HasQueuedMessages() || state.Model().ID() != model.ID() || state.SystemPrompt() != "system" || state.ThinkingLevel() != provider.ThinkingOff {
		t.Fatalf("reset state = messages %d queued %t model %q system %q thinking %q", len(state.Messages()), runtime.HasQueuedMessages(), state.Model().ID(), state.SystemPrompt(), state.ThinkingLevel())
	}
}

func TestStatefulAgentCopiesMutableInputsAndAcceptsAllPromptForms(t *testing.T) {
	model := sessionTestModel(t)
	initialLLM, err := llm.NewUserTextMessage("seed", agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := agentmsg.NewLLM(initialLLM)
	if err != nil {
		t.Fatal(err)
	}
	initialMessages := []agentmsg.Message{initial}
	definition, err := provider.NewToolDefinition("echo", "echo", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	definitions := []provider.ToolDefinition{definition}
	runtime, err := agent.New(agent.Config{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "one"), mustTextTerminal(t, "two"), mustTextTerminal(t, "three")),
		Model:    model, InitialMessages: initialMessages, Tool: &fakeTool{name: "echo"}, Tools: definitions,
		Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	initialMessages[0] = nil
	definitions[0] = provider.ToolDefinition{}
	state := runtime.State()
	if len(state.Messages()) != 1 || state.Messages()[0] == nil || len(state.Tools()) != 1 || state.Tools()[0].Name() != "echo" {
		t.Fatalf("construction copies = messages %#v tools %#v", state.Messages(), state.Tools())
	}
	readMessages := state.Messages()
	readTools := state.Tools()
	readMessages[0] = nil
	readTools[0] = provider.ToolDefinition{}
	if runtime.State().Messages()[0] == nil || runtime.State().Tools()[0].Name() != "echo" {
		t.Fatal("state getter slices mutated agent state")
	}
	if err := runtime.SetSystemPrompt("changed"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetThinkingLevel(provider.ThinkingOff); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "text prompt"); err != nil {
		t.Fatal(err)
	}
	contentText, err := llm.NewTextBlock("content prompt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.RunContent(context.Background(), []llm.UserContentBlock{contentText}); err != nil {
		t.Fatal(err)
	}
	messageLLM, err := llm.NewUserTextMessage("message prompt", agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	message, err := agentmsg.NewLLM(messageLLM)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.RunAgentMessages(context.Background(), []agentmsg.Message{message}); err != nil {
		t.Fatal(err)
	}
	if got := runtime.State(); got.SystemPrompt() != "changed" || len(got.Messages()) != 7 {
		t.Fatalf("state after prompt forms = system %q messages %d", got.SystemPrompt(), len(got.Messages()))
	}
}

func TestStatefulAgentResetDuringActiveRunDoesNotReleaseAdmission(t *testing.T) {
	runtime, err := agent.New(agent.Config{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "done")), Model: sessionTestModel(t),
		Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) error {
		if _, ok := event.(agent.AgentStartEvent); ok {
			close(entered)
			<-release
		}
		return nil
	})
	done := make(chan error, 1)
	go func() { _, runErr := runtime.Run(context.Background(), "go"); done <- runErr }()
	waitClosed(t, entered, "agent_start listener")
	if err := runtime.Steer("clear me"); err != nil {
		t.Fatal(err)
	}
	runtime.Reset()
	if runtime.State().IsStreaming() || runtime.HasQueuedMessages() {
		t.Fatalf("reset active runtime flags = streaming %t queued %t", runtime.State().IsStreaming(), runtime.HasQueuedMessages())
	}
	if _, err := runtime.Continue(context.Background()); !errors.Is(err, agent.ErrBusy) {
		t.Fatalf("Continue after active Reset = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestStatefulAgentActiveRunIncludesAgentEndListenerSettlementAndRejectsReentry(t *testing.T) {
	runtime, err := agent.New(agent.Config{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "done")), Model: sessionTestModel(t),
		Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) error {
		if _, ok := event.(agent.AgentEndEvent); ok {
			close(entered)
			<-release
		}
		return nil
	})
	type outcome struct {
		result agent.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, runErr := runtime.Run(context.Background(), "go")
		done <- outcome{result: result, err: runErr}
	}()
	waitClosed(t, entered, "agent_end listener")
	state := runtime.State()
	if !state.IsStreaming() || state.Phase() != agent.PhaseSettling {
		t.Fatalf("state during agent_end = phase %s streaming %t", state.Phase(), state.IsStreaming())
	}
	if _, runErr := runtime.Run(context.Background(), "reenter"); !errors.Is(runErr, agent.ErrBusy) {
		t.Fatalf("reentrant Run error = %v", runErr)
	}
	if _, runErr := runtime.RunContent(context.Background(), nil); !errors.Is(runErr, agent.ErrBusy) {
		t.Fatalf("reentrant RunContent error = %v", runErr)
	}
	stateMessages := runtime.State().Messages()
	if _, runErr := runtime.RunAgentMessages(context.Background(), stateMessages); !errors.Is(runErr, agent.ErrBusy) {
		t.Fatalf("reentrant RunAgentMessages error = %v", runErr)
	}
	if _, runErr := runtime.Continue(context.Background()); !errors.Is(runErr, agent.ErrBusy) {
		t.Fatalf("reentrant Continue error = %v", runErr)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if waitErr := runtime.WaitForIdle(waitCtx); !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("WaitForIdle before listener settlement = %v", waitErr)
	}
	close(release)
	settled := <-done
	if settled.err != nil || !settled.result.Succeeded() {
		t.Fatalf("settled Run = (%#v, %v)", settled.result, settled.err)
	}
	if err := runtime.WaitForIdle(context.Background()); err != nil || runtime.State().IsStreaming() {
		t.Fatalf("idle settlement = %v, streaming %t", err, runtime.State().IsStreaming())
	}
}

func TestStatefulAgentListenerFailureSynthesizesFailureAndStopsLaterListenerForThatEvent(t *testing.T) {
	runtime, err := agent.New(agent.Config{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "unused")), Model: sessionTestModel(t),
		Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	listenerErr := errors.New("listener failed")
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) error {
		if _, ok := event.(agent.TurnStartEvent); ok {
			return listenerErr
		}
		return nil
	})
	var observed []agent.AgentEventType
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
		observed = append(observed, event.Type())
	})
	result, err := runtime.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run error = %v", err)
	}
	terminal, ok := result.Terminal()
	failure, failed := terminal.(llm.AssistantFailureMessage)
	if !ok || !failed || failure.FinishReason() != llm.FinishError || !errors.Is(failure.Failure().Cause(), listenerErr) {
		t.Fatalf("terminal = %T %#v", terminal, terminal)
	}
	want := []agent.AgentEventType{
		agent.AgentStartEventType,
		agent.MessageStartEventType, agent.MessageEndEventType, agent.TurnEndEventType, agent.AgentEndEventType,
	}
	if !reflect.DeepEqual(observed, want) {
		t.Fatalf("later listener events = %v, want %v", observed, want)
	}
	if messages := runtime.State().Messages(); len(messages) != 1 || messages[0].Role() != agentmsg.RoleAssistant {
		t.Fatalf("failure state messages = %#v", messages)
	}
}

func TestStatefulAgentSecondListenerFailureRejectsSyntheticSettlementAndReturnsIdle(t *testing.T) {
	runtime, err := agent.New(agent.Config{
		Provider: newScriptedProvider(t, mustTextTerminal(t, "unused")), Model: sessionTestModel(t),
		Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	firstErr := errors.New("initial listener failure")
	settlementErr := errors.New("synthetic settlement listener failure")
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) error {
		switch event.(type) {
		case agent.TurnStartEvent:
			return firstErr
		case agent.MessageStartEvent:
			return settlementErr
		default:
			return nil
		}
	})
	result, runErr := runtime.Run(context.Background(), "go")
	if !errors.Is(runErr, settlementErr) {
		t.Fatalf("Run error = %v, want synthetic settlement listener failure", runErr)
	}
	if _, ok := result.Terminal(); ok {
		t.Fatalf("Run terminal = %#v, want rejected settlement", result)
	}
	if err := runtime.WaitForIdle(context.Background()); err != nil || runtime.State().Phase() != agent.PhaseIdle || runtime.State().IsStreaming() {
		t.Fatalf("settlement state = phase %s streaming %t error %v", runtime.State().Phase(), runtime.State().IsStreaming(), err)
	}
}

func TestStatefulAgentContinueDrainsAssistantTailQueuesByModeAndPriority(t *testing.T) {
	implementation := newScriptedProvider(t,
		mustTextTerminal(t, "initial"), mustTextTerminal(t, "after steering"), mustTextTerminal(t, "after follow-up"),
	)
	runtime, err := agent.New(agent.Config{
		Provider: implementation, Model: sessionTestModel(t),
		Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetSteeringMode(agent.QueueAll); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetFollowUpMode(agent.QueueOneAtATime); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "initial prompt"); err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"steer one", "steer two"} {
		if err := runtime.Steer(prompt); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.FollowUp("follow one"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	steering, follow := runtime.Queues()
	if len(steering) != 0 || len(follow) != 0 {
		t.Fatalf("queues after continuation = %d/%d", len(steering), len(follow))
	}
	if _, err := runtime.Continue(context.Background()); !errors.Is(err, agent.ErrCannotContinue) {
		t.Fatalf("Continue without queues = %v", err)
	}
	requests := implementation.Requests()
	if len(requests) != 3 {
		t.Fatalf("requests = %d", len(requests))
	}
	second := requests[1].Messages()
	if len(second) < 4 || messageText(t, second[len(second)-2]) != "steer one" || messageText(t, second[len(second)-1]) != "steer two" {
		t.Fatalf("steering continuation = %#v", second)
	}
	third := requests[2].Messages()
	if len(third) == 0 || messageText(t, third[len(third)-1]) != "follow one" {
		t.Fatalf("follow-up continuation = %#v", third)
	}
}

func TestStatefulAgentQueueFIFOHasAndClearOperations(t *testing.T) {
	runtime, err := agent.New(agent.Config{
		Provider: newScriptedProvider(t), Model: sessionTestModel(t), Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"a", "b"} {
		if err := runtime.Steer(value); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"c", "d"} {
		if err := runtime.FollowUp(value); err != nil {
			t.Fatal(err)
		}
	}
	steering, follow := runtime.Queues()
	richSteering, richFollow := runtime.RichQueues()
	if runtime.SteeringMode() != agent.QueueOneAtATime || runtime.FollowUpMode() != agent.QueueOneAtATime ||
		len(richSteering) != 2 || len(richFollow) != 2 {
		t.Fatalf("queue modes/types = %s/%s rich=%d/%d", runtime.SteeringMode(), runtime.FollowUpMode(), len(richSteering), len(richFollow))
	}
	if _, ok := richSteering[0].(llm.UserContentMessage); !ok {
		t.Fatalf("string steering normalized to %T", richSteering[0])
	}
	if !runtime.HasQueuedMessages() || len(steering) != 2 || messageText(t, steering[0]) != "a" || messageText(t, steering[1]) != "b" ||
		len(follow) != 2 || messageText(t, follow[0]) != "c" || messageText(t, follow[1]) != "d" {
		t.Fatalf("FIFO queues = steering %#v follow %#v", steering, follow)
	}
	runtime.ClearSteeringQueue()
	steering, follow = runtime.Queues()
	if len(steering) != 0 || len(follow) != 2 || !runtime.HasQueuedMessages() {
		t.Fatalf("ClearSteeringQueue = %d/%d has %t", len(steering), len(follow), runtime.HasQueuedMessages())
	}
	runtime.ClearAllQueues()
	if runtime.HasQueuedMessages() {
		t.Fatal("ClearAllQueues left queued messages")
	}
	if err := runtime.SetSteeringMode(agent.QueueMode(99)); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("invalid queue mode = %v", err)
	}
}

func TestStatefulAgentAbortIsNonBlockingAndPreservesQueuedMessages(t *testing.T) {
	definition, err := provider.NewToolDefinition("block", "block", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	tool := &fakeTool{name: "block", execute: func(ctx context.Context, _ []byte, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		<-ctx.Done()
		return agent.ToolOutput{}, context.Cause(ctx)
	}}
	runtime, err := agent.New(agent.Config{
		Provider: newScriptedProvider(t, mustToolUseTerminal(t, "call-1", "block", []byte(`{}`))),
		Model:    sessionTestModel(t), Tool: tool, Tools: []provider.ToolDefinition{definition},
		Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
		if _, ok := event.(agent.ToolExecutionStartEvent); ok {
			close(started)
		}
	})
	type outcome struct {
		result agent.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, runErr := runtime.Run(context.Background(), "go")
		done <- outcome{result: result, err: runErr}
	}()
	waitClosed(t, started, "blocking tool")
	signal, active := runtime.Signal()
	if !active || context.Cause(signal) != nil {
		t.Fatalf("active signal = (%v, %t)", context.Cause(signal), active)
	}
	if err := runtime.FollowUp("keep queued"); err != nil {
		t.Fatal(err)
	}
	abortReturned := make(chan error, 1)
	go func() { abortReturned <- runtime.Abort(context.Background()) }()
	select {
	case abortErr := <-abortReturned:
		if abortErr != nil {
			t.Fatal(abortErr)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Abort blocked on run settlement")
	}
	if !errors.Is(context.Cause(signal), agent.ErrAgentAborted) {
		t.Fatalf("signal cause after Abort = %v", context.Cause(signal))
	}
	settled := <-done
	terminal, ok := settled.result.Terminal()
	if settled.err != nil || !ok || terminal.FinishReason() != llm.FinishAborted {
		t.Fatalf("aborted Run = terminal %T error %v", terminal, settled.err)
	}
	_, follow := runtime.Queues()
	if len(follow) != 1 || messageText(t, follow[0]) != "keep queued" {
		t.Fatalf("follow-up queue after abort = %#v", follow)
	}
}

func TestStatefulAgentPromptAdmissionAllowsEmptyAndBusyWinsInvalidInput(t *testing.T) {
	providerImpl := newScriptedProvider(t, mustTextTerminal(t, "empty batch"))
	runtime, err := agent.New(agent.Config{
		Provider: providerImpl, Model: sessionTestModel(t),
		Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.RunAgentMessages(context.Background(), nil)
	if err != nil || !result.Succeeded() || len(providerImpl.Requests()) != 1 || len(providerImpl.Requests()[0].Messages()) != 0 {
		t.Fatalf("empty RunAgentMessages = (%#v, %v), request=%#v", result, err, providerImpl.Requests())
	}
	if _, err := runtime.RunAgentMessages(context.Background(), []agentmsg.Message{nil}); !errors.Is(err, agent.ErrInvalidRun) {
		t.Fatalf("idle nil prompt error = %v", err)
	}
	if runtime.State().Phase() != agent.PhaseIdle {
		t.Fatalf("invalid prompt left phase %s", runtime.State().Phase())
	}

	blocking, release := make(chan struct{}), make(chan struct{})
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
		if _, ok := event.(agent.AgentEndEvent); ok {
			close(blocking)
			<-release
		}
	})
	done := make(chan error, 1)
	go func() { _, runErr := runtime.RunAgentMessages(context.Background(), nil); done <- runErr }()
	waitClosed(t, blocking, "empty batch settlement")
	if _, runErr := runtime.RunAgentMessages(context.Background(), nil); !errors.Is(runErr, agent.ErrBusy) {
		t.Fatalf("active empty prompt = %v", runErr)
	}
	if _, runErr := runtime.RunAgentMessages(context.Background(), []agentmsg.Message{nil}); !errors.Is(runErr, agent.ErrBusy) {
		t.Fatalf("active invalid prompt = %v", runErr)
	}
	close(release)
	if runErr := <-done; runErr != nil {
		t.Fatal(runErr)
	}
}

func TestStatefulAgentRejectsInvalidInitialAndQueuedPartialMessages(t *testing.T) {
	base := agent.Config{Provider: newScriptedProvider(t), Model: sessionTestModel(t)}
	partial := agentmsg.AssistantPartial{}
	var typedNil *agentmsg.AssistantPartial
	for name, messages := range map[string][]agentmsg.Message{
		"nil": {nil}, "typed nil": {typedNil}, "partial": {partial}, "partial pointer": {&partial},
	} {
		t.Run(name, func(t *testing.T) {
			config := base
			config.InitialMessages = messages
			if _, err := agent.New(config); !errors.Is(err, agent.ErrInvalidConfig) {
				t.Fatalf("New error = %v", err)
			}
		})
	}
	runtime, err := agent.New(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.SteerAgentMessage(agentmsg.AssistantPartial{}); !errors.Is(err, agent.ErrInvalidQueueMessage) {
		t.Fatalf("queue partial error = %v", err)
	}
	if err := runtime.SteerAgentMessage(&partial); !errors.Is(err, agent.ErrInvalidQueueMessage) {
		t.Fatalf("queue partial pointer error = %v", err)
	}
	if err := runtime.SteerAgentMessage(typedNil); !errors.Is(err, agent.ErrInvalidQueueMessage) {
		t.Fatalf("queue typed nil error = %v", err)
	}
}

func TestStatefulAgentPreservesThinkingAndBridgesDynamicConversionAndAPIKey(t *testing.T) {
	model := sessionTestModel(t)
	definition, err := provider.NewToolDefinition("echo", "echo", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	providerImpl := newScriptedProvider(t, mustToolUseTerminal(t, "call", "echo", []byte(`{}`)), mustTextTerminal(t, "done"))
	convertCalls, keyCalls := 0, 0
	runtime, err := agent.New(agent.Config{
		Provider: providerImpl, Model: model, ThinkingLevel: provider.ThinkingHigh,
		Tool: &fakeTool{name: "echo"}, Tools: []provider.ToolDefinition{definition},
		ConvertToLLM: func(_ context.Context, messages []agentmsg.Message) ([]llm.ConversationMessage, error) {
			convertCalls++
			return agentmsg.ConvertToLLM(messages)
		},
		GetAPIKey: func(_ context.Context, providerName string) (string, error) {
			keyCalls++
			return fmt.Sprintf("%s-key-%d", providerName, keyCalls), nil
		},
		Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.State().ThinkingLevel() != provider.ThinkingHigh {
		t.Fatalf("initial thinking = %s", runtime.State().ThinkingLevel())
	}
	if _, err := runtime.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	requests := providerImpl.Requests()
	if convertCalls != 2 || keyCalls != 2 || len(requests) != 2 {
		t.Fatalf("dynamic calls convert=%d key=%d requests=%d", convertCalls, keyCalls, len(requests))
	}
	for index, request := range requests {
		if request.ThinkingLevel() != provider.ThinkingHigh || request.StreamOptions().APIKey != fmt.Sprintf("%s-key-%d", model.Provider(), index+1) {
			t.Fatalf("request %d thinking/key = %s/%q", index, request.ThinkingLevel(), request.StreamOptions().APIKey)
		}
	}
	if err := runtime.SetThinkingLevel(provider.ThinkingXHigh); err != nil || runtime.State().ThinkingLevel() != provider.ThinkingXHigh {
		t.Fatalf("SetThinkingLevel = state %s error %v", runtime.State().ThinkingLevel(), err)
	}
}

func TestStatefulAgentPrepareNextTurnReceivesFullToolContextAndOverridesLegacySnapshot(t *testing.T) {
	firstModel := mustLoopModel(t, "prepare-first", provider.CostRates{})
	legacyModel := mustLoopModel(t, "prepare-legacy", provider.CostRates{})
	nextModel := mustLoopModel(t, "prepare-next", provider.CostRates{})
	definition := mustLoopDefinition(t, "echo", `{"type":"object"}`)
	call := mustLoopCall(t, "call", "echo", `{}`)
	providerImpl := mustLoopProvider(t,
		mustLoopToolMessage(t, firstModel, llm.FinishToolUse, call),
		mustLoopTextMessage(t, nextModel, "done", llm.FinishStop, 3),
	)
	injected := mustLoopUser(t, "full-context", 4)
	prepared := mustLoopUser(t, "request-context", 5)
	tool := &fakeTool{name: "echo"}
	var legacyTurns []uint32
	var callbacks []agent.AgentLoopTurnContext
	var runtime *agent.Agent
	runtime, err := agent.New(agent.Config{
		Provider: providerImpl, Model: firstModel, ThinkingLevel: provider.ThinkingLow,
		SystemPrompt: "initial-system", Tool: tool, Tools: []provider.ToolDefinition{definition},
		PrepareTurn: func(_ context.Context, input agent.TurnContext) (agent.TurnSnapshot, error) {
			legacyTurns = append(legacyTurns, input.Turn)
			if input.Turn == 1 {
				return agent.TurnSnapshot{
					Model: firstModel, ThinkingLevel: provider.ThinkingLow, SystemPrompt: "initial-system",
					Tool: tool, Tools: []provider.ToolDefinition{definition},
				}, nil
			}
			return agent.TurnSnapshot{
				Model: legacyModel, ThinkingLevel: provider.ThinkingMedium, SystemPrompt: "legacy-system",
				Tool: tool, Tools: []provider.ToolDefinition{definition},
				Messages: append(agentmsg.Clone(input.Messages), prepared),
			}, nil
		},
		PrepareNextTurn: func(_ context.Context, input agent.AgentLoopTurnContext) (*agent.AgentLoopTurnUpdate, error) {
			callbacks = append(callbacks, input)
			if len(input.ToolResults) == 0 {
				return nil, nil
			}
			if err := runtime.Steer("queued-after-prepare"); err != nil {
				return nil, err
			}
			replacement := input.Context
			replacement.SystemPrompt = "full-system"
			replacement.Messages = append(append([]agentmsg.Message(nil), input.Context.Messages...), injected)
			thinking := provider.ThinkingHigh
			return &agent.AgentLoopTurnUpdate{Context: &replacement, Model: &nextModel, ThinkingLevel: &thinking}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, runErr := runtime.Run(context.Background(), "go"); runErr != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, runErr)
	}
	if !reflect.DeepEqual(legacyTurns, []uint32{1, 2}) {
		t.Fatalf("legacy PrepareTurn calls = %v", legacyTurns)
	}
	if len(callbacks) != 2 {
		t.Fatalf("PrepareNextTurn calls = %d", len(callbacks))
	}
	toolTurn := callbacks[0]
	if _, ok := toolTurn.Message.(llm.AssistantToolUseMessage); !ok || len(toolTurn.ToolResults) != 1 || toolTurn.ToolResults[0].Role() != agentmsg.RoleToolResult {
		t.Fatalf("tool turn message/results = %T/%#v", toolTurn.Message, toolTurn.ToolResults)
	}
	if toolTurn.Context.SystemPrompt != "initial-system" || len(toolTurn.Context.Messages) != 3 || len(toolTurn.NewMessages) != 3 {
		t.Fatalf("tool turn context/newMessages = system %q context %d new %d", toolTurn.Context.SystemPrompt, len(toolTurn.Context.Messages), len(toolTurn.NewMessages))
	}
	requests := providerImpl.Requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	second := requests[1]
	secondMessages := second.Messages()
	if !second.Model().Equal(nextModel) || second.ThinkingLevel() != provider.ThinkingHigh || second.SystemPrompt() != "full-system" ||
		len(secondMessages) != 6 || messageText(t, secondMessages[len(secondMessages)-3]) != "full-context" ||
		messageText(t, secondMessages[len(secondMessages)-2]) != "queued-after-prepare" ||
		messageText(t, secondMessages[len(secondMessages)-1]) != "request-context" {
		t.Fatalf("second request = model %q thinking %q system %q messages %#v", second.Model().ID(), second.ThinkingLevel(), second.SystemPrompt(), secondMessages)
	}
	if _, ok := callbacks[1].Message.(llm.AssistantTextMessage); !ok || len(callbacks[1].ToolResults) != 0 {
		t.Fatalf("terminal callback = %T results %#v", callbacks[1].Message, callbacks[1].ToolResults)
	}
	callbacks[0].Context.Messages[0] = nil
	callbacks[0].NewMessages[0] = nil
	callbacks[0].ToolResults[0] = nil
	for index, message := range runtime.State().Messages() {
		if message == nil {
			t.Fatalf("retained callback snapshot mutated state message %d", index)
		}
	}
}

func TestStatefulAgentPrepareNextTurnRunsForPlainTerminalTurn(t *testing.T) {
	model := mustLoopModel(t, "prepare-terminal", provider.CostRates{})
	providerImpl := mustLoopProvider(t, mustLoopTextMessage(t, model, "done", llm.FinishStop, 2))
	var callback agent.AgentLoopTurnContext
	calls := 0
	runtime, err := agent.New(agent.Config{
		Provider: providerImpl, Model: model,
		PrepareNextTurn: func(_ context.Context, input agent.AgentLoopTurnContext) (*agent.AgentLoopTurnUpdate, error) {
			calls++
			callback = input
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, runErr := runtime.Run(context.Background(), "go"); runErr != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, runErr)
	}
	if calls != 1 || len(callback.ToolResults) != 0 || len(callback.Context.Messages) != 2 || len(callback.NewMessages) != 2 {
		t.Fatalf("plain terminal callback = calls %d message %T results %d context %d new %d", calls, callback.Message, len(callback.ToolResults), len(callback.Context.Messages), len(callback.NewMessages))
	}
	if _, ok := callback.Message.(llm.AssistantTextMessage); !ok {
		t.Fatalf("plain terminal message = %T", callback.Message)
	}
}

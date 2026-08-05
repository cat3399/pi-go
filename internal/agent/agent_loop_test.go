package agent_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestAgentLoopStreamingLifecycle(t *testing.T) {
	model := mustLoopModel(t, "loop-1", provider.CostRates{})
	text, err := llm.NewTextBlock("hello")
	if err != nil {
		t.Fatal(err)
	}
	thinking, err := llm.NewThinkingBlock("considering")
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := llm.NewAssistantRichMessage(
		[]llm.AssistantBlock{thinking, text},
		llm.FinishStop,
		llm.Usage{},
		time.UnixMilli(2),
		loopProvenance(model),
	)
	if err != nil {
		t.Fatal(err)
	}
	scripted := mustLoopProvider(t, terminal)
	prompt := mustLoopUser(t, "hi", 1)

	var events []agent.AgentEventType
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		RunID: 7, Provider: scripted, Model: model,
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			events = append(events, event.Type())
			return nil
		},
		Now: func() time.Time { return time.UnixMilli(3) },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), []agentmsg.Message{prompt}, agent.AgentLoopContext{SystemPrompt: "system"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(result.Messages) != 2 || len(result.Context.Messages) != 2 || result.ProviderTurns != 1 {
		t.Fatalf("unexpected result: messages=%d context=%d turns=%d", len(result.Messages), len(result.Context.Messages), result.ProviderTurns)
	}
	if result.Terminal == nil || result.Terminal.FinishReason() != llm.FinishStop {
		t.Fatalf("terminal = %#v", result.Terminal)
	}
	wantWithoutUpdates := []agent.AgentEventType{
		agent.AgentStartEventType,
		agent.TurnStartEventType,
		agent.MessageStartEventType,
		agent.MessageEndEventType,
		agent.MessageStartEventType,
		agent.MessageEndEventType,
		agent.TurnEndEventType,
		agent.AgentEndEventType,
	}
	var withoutUpdates []agent.AgentEventType
	for _, event := range events {
		if event != agent.MessageUpdateEventType {
			withoutUpdates = append(withoutUpdates, event)
		}
	}
	assertLoopEventTypes(t, withoutUpdates, wantWithoutUpdates)
	if requests := scripted.Requests(); len(requests) != 1 || requests[0].SystemPrompt() != "system" || len(requests[0].Messages()) != 1 {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestAgentLoopParallelCompletionAndSourceOrder(t *testing.T) {
	model := mustLoopModel(t, "loop-1", provider.CostRates{})
	first := mustLoopCall(t, "call-1", "echo", `{"value":"first"}`)
	second := mustLoopCall(t, "call-2", "echo", `{"value":"second"}`)
	toolUse := mustLoopToolMessage(t, model, llm.FinishToolUse, first, second)
	final := mustLoopTextMessage(t, model, "done", llm.FinishStop, 4)
	scripted := mustLoopProvider(t, toolUse, final)
	definition, err := provider.NewToolDefinition("echo", "echo", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	releaseFirst := make(chan struct{})
	executor := &loopNamedTool{definition: definition, execute: func(_ context.Context, arguments any, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		input := arguments.(map[string]any)
		value := input["value"].(string)
		if value == "first" {
			<-releaseFirst
		}
		return agent.ToolOutput{Content: []llm.ToolResultContentBlock{mustLoopText(t, "ok:"+value)}, Details: map[string]string{"value": value}}, nil
	}}

	var mu sync.Mutex
	var endOrder, resultOrder []string
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		RunID: 8, Provider: scripted, Model: model, ToolExecution: agent.ToolExecutionParallel,
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			mu.Lock()
			defer mu.Unlock()
			switch value := event.(type) {
			case agent.ToolExecutionEndEvent:
				endOrder = append(endOrder, value.ToolCallID)
				if value.ToolCallID == "call-2" {
					close(releaseFirst)
				}
			case agent.MessageEndEvent:
				if wrapped, ok := value.Message.(agentmsg.LLM); ok {
					switch result := wrapped.Conversation().(type) {
					case llm.ToolResultMessage:
						resultOrder = append(resultOrder, result.ToolCallID())
					case llm.ToolResultContentMessage:
						resultOrder = append(resultOrder, result.ToolCallID())
					}
				}
			}
			return nil
		},
		Now: func() time.Time { return time.UnixMilli(5) },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "go", 1)}, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{executor}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ProviderTurns != 2 || result.ToolExecutions != 2 {
		t.Fatalf("counts = provider %d tool %d", result.ProviderTurns, result.ToolExecutions)
	}
	assertLoopStrings(t, endOrder, []string{"call-2", "call-1"})
	assertLoopStrings(t, resultOrder, []string{"call-1", "call-2"})
	roles := loopRoles(result.Messages)
	assertLoopStrings(t, roles, []string{"user", "assistant", "toolResult", "toolResult", "assistant"})
}

func TestAgentLoopLengthToolCallsFailAllAndContinue(t *testing.T) {
	model := mustLoopModel(t, "loop-1", provider.CostRates{})
	first := mustLoopCall(t, "call-1", "echo", `{"value":"partial"}`)
	second := mustLoopCall(t, "call-2", "echo", `{"value":"also-partial"}`)
	truncated := mustLoopToolMessage(t, model, llm.FinishLength, first, second)
	final := mustLoopTextMessage(t, model, "recovered", llm.FinishStop, 4)
	scripted := mustLoopProvider(t, truncated, final)
	definition, err := provider.NewToolDefinition("echo", "echo", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	executor := &loopNamedTool{definition: definition}
	var beforeCalls, afterCalls atomic.Uint32
	var sequence []string
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		RunID: 9, Provider: scripted, Model: model,
		BeforeToolCall: func(context.Context, agent.AgentLoopBeforeToolCallContext) (agent.AgentLoopBeforeToolCallResult, error) {
			beforeCalls.Add(1)
			return agent.AgentLoopBeforeToolCallResult{}, nil
		},
		AfterToolCall: func(context.Context, agent.AgentLoopAfterToolCallContext) (agent.AgentLoopAfterToolCallResult, error) {
			afterCalls.Add(1)
			return agent.AgentLoopAfterToolCallResult{}, nil
		},
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			switch value := event.(type) {
			case agent.ToolExecutionStartEvent:
				sequence = append(sequence, "start:"+value.ToolCallID)
			case agent.ToolExecutionEndEvent:
				if !value.IsError || value.Err != agent.ErrTruncatedToolCall {
					t.Fatalf("unexpected truncated outcome: %#v", value)
				}
				sequence = append(sequence, "end:"+value.ToolCallID)
			case agent.MessageEndEvent:
				if wrapped, ok := value.Message.(agentmsg.LLM); ok {
					if result, ok := wrapped.Conversation().(llm.ToolResultMessage); ok {
						if !result.IsError() || len(result.Content()) != 1 || result.Content()[0].Text() == "" || string(result.Details()) != `{}` {
							t.Fatalf("invalid failure result: %#v", result)
						}
						sequence = append(sequence, "result:"+result.ToolCallID())
					}
				}
			}
			return nil
		},
		Now: func() time.Time { return time.UnixMilli(5) },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "go", 1)}, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{executor}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if executor.calls.Load() != 0 || beforeCalls.Load() != 0 || afterCalls.Load() != 0 {
		t.Fatalf("unsafe tool path called: execute=%d before=%d after=%d", executor.calls.Load(), beforeCalls.Load(), afterCalls.Load())
	}
	if result.ProviderTurns != 2 || result.ToolExecutions != 0 {
		t.Fatalf("counts = provider %d tool %d", result.ProviderTurns, result.ToolExecutions)
	}
	assertLoopStrings(t, sequence, []string{"start:call-1", "end:call-1", "result:call-1", "start:call-2", "end:call-2", "result:call-2"})
	assertLoopStrings(t, loopRoles(result.Messages), []string{"user", "assistant", "toolResult", "toolResult", "assistant"})
}

func TestAgentUsesTruncatedToolCallGuard(t *testing.T) {
	model, err := newTestModel("scripted", "scripted", "scripted-1")
	if err != nil {
		t.Fatal(err)
	}
	call := mustLoopCall(t, "call-1", "echo", `{"value":"partial"}`)
	truncated, err := llm.NewAssistantToolUseMessageWithFinishAndMetadata(
		[]llm.AssistantBlock{call}, llm.FinishLength, llm.Usage{}, agentTestEpoch,
		testAssistantProvenance(), nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	final, err := newAssistantTextMessage([]llm.TextBlock{mustLoopText(t, "done")}, llm.FinishStop, llm.Usage{}, agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, truncated, final)
	tool := &fakeTool{name: "echo"}
	definition, err := provider.NewToolDefinition("echo", "echo", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.New(agent.Config{
		Provider: scripted, Model: model,
		Tool: tool, Tools: []provider.ToolDefinition{definition},
		Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	var sequence []string
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
		switch value := event.(type) {
		case agent.ToolExecutionStartEvent:
			sequence = append(sequence, "start:"+value.ToolCallID)
		case agent.ToolExecutionEndEvent:
			sequence = append(sequence, "end:"+value.ToolCallID)
		case agent.MessageEndEvent:
			if wrapped, ok := value.Message.(agentmsg.LLM); ok {
				if result, ok := wrapped.Conversation().(llm.ToolResultMessage); ok {
					sequence = append(sequence, "result:"+result.ToolCallID())
				}
			}
		}
	})
	result, err := runtime.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if tool.CallCount() != 0 || result.ToolExecutions() != 0 || result.ProviderTurns() != 2 {
		t.Fatalf("unsafe core path: calls=%d tool executions=%d provider turns=%d", tool.CallCount(), result.ToolExecutions(), result.ProviderTurns())
	}
	messages, err := agentmsg.ConvertToLLM(runtime.State().Messages())
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("transcript messages = %d", len(messages))
	}
	toolResult, ok := messages[2].(llm.ToolResultMessage)
	if !ok || !toolResult.IsError() || len(toolResult.Content()) != 1 {
		t.Fatalf("tool result = %#v", messages[2])
	}
	assertLoopStrings(t, sequence, []string{"start:call-1", "end:call-1", "result:call-1"})
}

func TestAgentLoopPrepareStopAndQueuesOrder(t *testing.T) {
	firstModel := mustLoopModel(t, "loop-1", provider.CostRates{})
	secondModel := mustLoopModel(t, "loop-2", provider.CostRates{})
	first := mustLoopTextMessage(t, firstModel, "first", llm.FinishStop, 2)
	second := mustLoopTextMessage(t, secondModel, "second", llm.FinishStop, 4)
	scripted := mustLoopProvider(t, first, second)
	steering := mustLoopUser(t, "steer", 3)
	followUp := mustLoopUser(t, "follow", 5)
	var callbacks []string
	steeringPolls := 0
	prepareCalls := 0
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		RunID: 10, Provider: scripted, Model: firstModel,
		PrepareNextTurn: func(_ context.Context, input agent.AgentLoopTurnContext) (*agent.AgentLoopTurnUpdate, error) {
			callbacks = append(callbacks, "prepare")
			prepareCalls++
			if prepareCalls == 1 {
				replacement := input.Context
				replacement.SystemPrompt = "replacement"
				thinking := provider.ThinkingHigh
				return &agent.AgentLoopTurnUpdate{Context: &replacement, Model: &secondModel, ThinkingLevel: &thinking}, nil
			}
			return nil, nil
		},
		ShouldStopAfterTurn: func(_ context.Context, input agent.AgentLoopTurnContext) (bool, error) {
			callbacks = append(callbacks, "stop")
			return len(input.NewMessages) >= 4, nil
		},
		GetSteeringMessages: func(context.Context) ([]agentmsg.Message, error) {
			callbacks = append(callbacks, "steering")
			steeringPolls++
			if steeringPolls == 2 {
				return []agentmsg.Message{steering}, nil
			}
			return nil, nil
		},
		GetFollowUpMessages: func(context.Context) ([]agentmsg.Message, error) {
			callbacks = append(callbacks, "followup")
			return []agentmsg.Message{followUp}, nil
		},
		Now: func() time.Time { return time.UnixMilli(6) },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "start", 1)}, agent.AgentLoopContext{SystemPrompt: "initial"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if scripted.CallCount() != 2 {
		t.Fatalf("provider calls = %d", scripted.CallCount())
	}
	requests := scripted.Requests()
	if !requests[1].Model().Equal(secondModel) || requests[1].SystemPrompt() != "replacement" || requests[1].ThinkingLevel() != provider.ThinkingHigh {
		t.Fatalf("second snapshot = model %s prompt %q thinking %q", requests[1].Model().ID(), requests[1].SystemPrompt(), requests[1].ThinkingLevel())
	}
	assertLoopStrings(t, loopRoles(result.Messages), []string{"user", "assistant", "user", "assistant"})
	assertLoopStrings(t, callbacks, []string{"steering", "prepare", "stop", "steering", "prepare", "stop"})
}

func TestAgentLoopProviderFailureEndsWithoutTurnCallbacksOrQueues(t *testing.T) {
	model := mustLoopModel(t, "loop-1", provider.CostRates{})
	scripted := mustLoopProvider(t)
	var callbackCalls atomic.Uint32
	var events []agent.AgentEventType
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		RunID: 11, Provider: scripted, Model: model,
		PrepareNextTurn: func(context.Context, agent.AgentLoopTurnContext) (*agent.AgentLoopTurnUpdate, error) {
			callbackCalls.Add(1)
			return nil, nil
		},
		ShouldStopAfterTurn: func(context.Context, agent.AgentLoopTurnContext) (bool, error) {
			callbackCalls.Add(1)
			return false, nil
		},
		GetSteeringMessages: func(context.Context) ([]agentmsg.Message, error) {
			callbackCalls.Add(1)
			return nil, nil
		},
		GetFollowUpMessages: func(context.Context) ([]agentmsg.Message, error) {
			callbackCalls.Add(1)
			return nil, nil
		},
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			events = append(events, event.Type())
			return nil
		},
		Now: func() time.Time { return time.UnixMilli(6) },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(context.Background(), []agentmsg.Message{mustLoopUser(t, "start", 1)}, agent.AgentLoopContext{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	// The single initial steering poll happens before provider execution. Error
	// settlement must not call prepare/stop or poll either queue afterwards.
	if callbackCalls.Load() != 1 {
		t.Fatalf("callback calls = %d", callbackCalls.Load())
	}
	if result.Terminal == nil || result.Terminal.FinishReason() != llm.FinishError || len(result.Messages) != 2 {
		t.Fatalf("result = %#v", result)
	}
	assertLoopEventTypes(t, events, []agent.AgentEventType{
		agent.AgentStartEventType, agent.TurnStartEventType,
		agent.MessageStartEventType, agent.MessageEndEventType,
		agent.MessageStartEventType, agent.MessageEndEventType,
		agent.TurnEndEventType, agent.AgentEndEventType,
	})
}

func TestAgentLoopAbortDuringToolSettlesResultsAndTerminal(t *testing.T) {
	model := mustLoopModel(t, "loop-1", provider.CostRates{})
	call := mustLoopCall(t, "call-1", "echo", `{"value":"cancel"}`)
	secondCall := mustLoopCall(t, "call-2", "echo", `{"value":"must-not-start"}`)
	scripted := mustLoopProvider(t, mustLoopToolMessage(t, model, llm.FinishToolUse, call, secondCall), mustLoopTextMessage(t, model, "unused", llm.FinishStop, 4))
	definition, err := provider.NewToolDefinition("echo", "echo", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	executor := &loopNamedTool{definition: definition, execute: func(context.Context, any, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		cancel(agent.ErrAgentAborted)
		return agent.ToolOutput{Text: "late success"}, nil
	}}
	var events []agent.AgentEventType
	var startedCalls []string
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		RunID: 13, Provider: scripted, Model: model, ToolExecution: agent.ToolExecutionSequential,
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			events = append(events, event.Type())
			if started, ok := event.(agent.ToolExecutionStartEvent); ok {
				startedCalls = append(startedCalls, started.ToolCallID)
			}
			return nil
		},
		Now: func() time.Time { return time.UnixMilli(6) },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Run(ctx, []agentmsg.Message{mustLoopUser(t, "start", 1)}, agent.AgentLoopContext{Tools: []agent.AgentLoopTool{executor}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Terminal == nil || result.Terminal.FinishReason() != llm.FinishAborted || result.ToolExecutions != 1 || result.ProviderTurns != 2 {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Messages) != 4 || result.Messages[2].Role() != agentmsg.RoleToolResult {
		t.Fatalf("messages = %v", loopRoles(result.Messages))
	}
	toolResult := result.Messages[2].(agentmsg.LLM).Conversation().(llm.ToolResultMessage)
	if toolResult.IsError() || toolResult.Content()[0].Text() != "late success" {
		t.Fatalf("tool result was overwritten: %#v", toolResult)
	}
	assertLoopStrings(t, startedCalls, []string{"call-1"})
	if len(events) == 0 || events[len(events)-1] != agent.AgentEndEventType {
		t.Fatalf("events did not settle: %v", events)
	}
}

func TestAgentLoopContinueReturnsOnlyInvocationMessages(t *testing.T) {
	model := mustLoopModel(t, "loop-1", provider.CostRates{})
	scripted := mustLoopProvider(t, mustLoopTextMessage(t, model, "continued", llm.FinishStop, 2))
	var endedRoles []string
	loop, err := agent.NewAgentLoop(agent.AgentLoopConfig{
		RunID: 12, Provider: scripted, Model: model,
		Emit: func(_ context.Context, event agent.AgentEvent) error {
			if ended, ok := event.(agent.MessageEndEvent); ok {
				endedRoles = append(endedRoles, string(ended.Message.Role()))
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := loop.Continue(context.Background(), agent.AgentLoopContext{Messages: []agentmsg.Message{mustLoopUser(t, "existing", 1)}})
	if err != nil {
		t.Fatalf("Continue() error = %v", err)
	}
	assertLoopStrings(t, loopRoles(result.Messages), []string{"assistant"})
	assertLoopStrings(t, loopRoles(result.Context.Messages), []string{"user", "assistant"})
	assertLoopStrings(t, endedRoles, []string{"assistant"})
}

type loopNamedTool struct {
	definition provider.ToolDefinition
	calls      atomic.Uint32
	execute    func(context.Context, any, func(agent.ToolUpdate)) (agent.ToolOutput, error)
}

func (t *loopNamedTool) Definition() provider.ToolDefinition { return t.definition }
func (t *loopNamedTool) Execute(ctx context.Context, _ string, arguments any, update func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	t.calls.Add(1)
	if t.execute == nil {
		return agent.ToolOutput{Text: "ok"}, nil
	}
	return t.execute(ctx, arguments, update)
}

func mustLoopModel(t *testing.T, id string, rates provider.CostRates) provider.Model {
	t.Helper()
	model, err := provider.NewModel(provider.ModelSpec{Provider: "scripted", API: "scripted", ID: id, Name: id, Input: []provider.InputKind{provider.InputText}, Cost: rates, ContextWindow: 8192, MaxTokens: 2048})
	if err != nil {
		t.Fatal(err)
	}
	return model
}
func loopProvenance(model provider.Model) llm.AssistantProvenance {
	return llm.AssistantProvenance{Provider: model.Provider(), API: model.API(), Model: model.ID()}
}
func mustLoopProvider(t *testing.T, terminals ...llm.AssistantTerminal) *provider.ScriptedProvider {
	t.Helper()
	scripted, err := provider.NewScriptedProvider(provider.ScriptedConfig{ChunkRunes: 2, Clock: func() time.Time { return time.UnixMilli(10) }})
	if err != nil {
		t.Fatal(err)
	}
	steps := make([]provider.ScriptStep, len(terminals))
	for index, terminal := range terminals {
		steps[index], err = provider.FixedResponseStep(terminal)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := scripted.SetResponses(steps); err != nil {
		t.Fatal(err)
	}
	return scripted
}
func mustLoopUser(t *testing.T, text string, at int64) agentmsg.Message {
	t.Helper()
	message, err := llm.NewUserTextMessage(text, time.UnixMilli(at))
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := agentmsg.NewLLM(message)
	if err != nil {
		t.Fatal(err)
	}
	return wrapped
}
func mustLoopTextMessage(t *testing.T, model provider.Model, text string, finish llm.FinishReason, at int64) llm.AssistantTerminal {
	t.Helper()
	message, err := llm.NewAssistantTextMessage([]llm.TextBlock{mustLoopText(t, text)}, finish, llm.Usage{}, time.UnixMilli(at), loopProvenance(model))
	if err != nil {
		t.Fatal(err)
	}
	return message
}
func mustLoopText(t *testing.T, text string) llm.TextBlock {
	t.Helper()
	block, err := llm.NewTextBlock(text)
	if err != nil {
		t.Fatal(err)
	}
	return block
}
func mustLoopCall(t *testing.T, id, name, raw string) llm.ToolCallBlock {
	t.Helper()
	call, err := llm.NewToolCallBlock(id, name, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return call
}
func mustLoopToolMessage(t *testing.T, model provider.Model, finish llm.FinishReason, calls ...llm.ToolCallBlock) llm.AssistantTerminal {
	t.Helper()
	blocks := make([]llm.AssistantBlock, len(calls))
	for index, call := range calls {
		blocks[index] = call
	}
	message, err := llm.NewAssistantToolUseMessageWithFinishAndMetadata(blocks, finish, llm.Usage{}, time.UnixMilli(2), loopProvenance(model), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return message
}
func loopRoles(messages []agentmsg.Message) []string {
	roles := make([]string, len(messages))
	for index, message := range messages {
		roles[index] = string(message.Role())
	}
	return roles
}
func assertLoopStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
func assertLoopEventTypes(t *testing.T, got, want []agent.AgentEventType) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

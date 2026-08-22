package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

type namedBatchTool struct {
	mu             sync.Mutex
	started        map[string]chan struct{}
	release        map[string]chan struct{}
	mode           agent.ToolExecutionMode
	supportLookups []string
}

type mixedTool struct{}

func (mixedTool) Name() string              { return "mixed" }
func (mixedTool) Supports(name string) bool { return name != "missing" }
func (mixedTool) Execute(context.Context, string, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return agent.ToolOutput{}, nil
}
func (mixedTool) ExecuteNamed(_ context.Context, _ string, name string, _ []byte, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	switch name {
	case "failure":
		return agent.ToolOutput{Text: "failed"}, errors.New("tool failed")
	case "terminate":
		return agent.ToolOutput{Text: "ending", Terminate: true}, nil
	default:
		return agent.ToolOutput{Text: name}, nil
	}
}

func (t *namedBatchTool) Name() string { return "batch" }
func (t *namedBatchTool) Supports(name string) bool {
	t.mu.Lock()
	t.supportLookups = append(t.supportLookups, name)
	t.mu.Unlock()
	return name == "slow" || name == "fast"
}
func (t *namedBatchTool) ToolExecutionMode(string) (agent.ToolExecutionMode, bool) {
	return t.mode, t.mode != 0
}
func (t *namedBatchTool) Execute(context.Context, string, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return agent.ToolOutput{}, nil
}
func (t *namedBatchTool) ExecuteNamed(ctx context.Context, _ string, name string, _ []byte, report func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	close(t.started[name])
	select {
	case <-t.release[name]:
	case <-ctx.Done():
		return agent.ToolOutput{Text: "cancelled"}, context.Cause(ctx)
	}
	report(agent.ToolUpdate{Text: name + " done"})
	return agent.ToolOutput{Text: name}, nil
}

func TestStatePendingToolCallsTracksSequentialAndParallelBatches(t *testing.T) {
	t.Run("sequential changes the legacy single-call view per call", func(t *testing.T) {
		transcript := newSession(t)
		first, err := llm.NewToolCallBlock("one", "slow", []byte(`{"x":1}`))
		if err != nil {
			t.Fatal(err)
		}
		second, err := llm.NewToolCallBlock("two", "fast", []byte(`{"x":2}`))
		if err != nil {
			t.Fatal(err)
		}
		assistant, err := newAssistantToolUseMessage([]llm.AssistantBlock{first, second}, mustUsage(t, 3, 2), agentTestEpoch)
		if err != nil {
			t.Fatal(err)
		}
		scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "done"))
		tool := &namedBatchTool{
			mode:    agent.ToolExecutionSequential,
			started: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
			release: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
		}
		runtime := newAgent(t, transcript, scripted, tool)
		states := make(chan agent.State, 2)
		runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
			if _, ok := event.(agent.ToolExecutionStartEvent); ok {
				states <- runtime.State()
			}
		})
		done := make(chan error, 1)
		go func() { _, err := runtime.Run(context.Background(), "go"); done <- err }()
		waitClosed(t, tool.started["slow"], "first sequential tool")
		assertSinglePendingCall(t, <-states, "one")
		close(tool.release["slow"])
		waitClosed(t, tool.started["fast"], "second sequential tool")
		assertSinglePendingCall(t, <-states, "two")
		close(tool.release["fast"])
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("parallel exposes the full immutable batch and hides legacy singleton", func(t *testing.T) {
		transcript := newSession(t)
		first, err := llm.NewToolCallBlock("one", "slow", []byte(`{"x":1}`))
		if err != nil {
			t.Fatal(err)
		}
		second, err := llm.NewToolCallBlock("two", "fast", []byte(`{"x":2}`))
		if err != nil {
			t.Fatal(err)
		}
		assistant, err := newAssistantToolUseMessage([]llm.AssistantBlock{first, second}, mustUsage(t, 3, 2), agentTestEpoch)
		if err != nil {
			t.Fatal(err)
		}
		scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "done"))
		tool := &namedBatchTool{
			started: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
			release: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
		}
		runtime := newAgent(t, transcript, scripted, tool)
		type stateEvent struct {
			callID string
			state  agent.State
		}
		started := make(chan stateEvent, 2)
		settled := make(chan stateEvent, 2)
		runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
			switch event := event.(type) {
			case agent.ToolExecutionStartEvent:
				started <- stateEvent{callID: event.ToolCallID, state: runtime.State()}
			case agent.ToolExecutionEndEvent:
				settled <- stateEvent{callID: event.ToolCallID, state: runtime.State()}
			}
		})
		done := make(chan error, 1)
		go func() { _, err := runtime.Run(context.Background(), "go"); done <- err }()
		waitClosed(t, tool.started["slow"], "parallel slow tool")
		waitClosed(t, tool.started["fast"], "parallel fast tool")
		firstStart := <-started
		if firstStart.callID != "one" {
			t.Fatalf("first parallel start = %q", firstStart.callID)
		}
		assertSinglePendingCall(t, firstStart.state, "one")
		secondStart := <-started
		if secondStart.callID != "two" {
			t.Fatalf("second parallel start = %q", secondStart.callID)
		}
		if got := secondStart.state.PendingToolCalls(); !reflect.DeepEqual(got, []string{"one", "two"}) {
			t.Fatalf("parallel pending calls = %v", got)
		}
		if call, ok := secondStart.state.PendingToolCall(); ok || call != "" {
			t.Fatalf("legacy pending call for parallel batch = %q/%t", call, ok)
		}
		mutated := secondStart.state.PendingToolCalls()
		mutated[0] = "changed"
		if got := secondStart.state.PendingToolCalls(); !reflect.DeepEqual(got, []string{"one", "two"}) {
			t.Fatalf("pending snapshot mutated through caller slice: %v", got)
		}
		close(tool.release["fast"])
		fastSettled := <-settled
		if fastSettled.callID != "two" {
			t.Fatalf("first parallel settled call = %q", fastSettled.callID)
		}
		assertSinglePendingCall(t, fastSettled.state, "one")
		close(tool.release["slow"])
		slowSettled := <-settled
		if slowSettled.callID != "one" {
			t.Fatalf("second parallel settled call = %q", slowSettled.callID)
		}
		if got := slowSettled.state.PendingToolCalls(); len(got) != 0 {
			t.Fatalf("pending calls after final parallel settlement = %v", got)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func assertSinglePendingCall(t *testing.T, state agent.State, want string) {
	t.Helper()
	if got := state.PendingToolCalls(); !reflect.DeepEqual(got, []string{want}) {
		t.Fatalf("pending calls = %v, want [%s]", got, want)
	}
	if got, ok := state.PendingToolCall(); !ok || got != want {
		t.Fatalf("legacy pending call = %q/%t, want %q/true", got, ok, want)
	}
}

func TestParallelBatchSettlesEventsByCompletionButCommitsSourceOrder(t *testing.T) {
	transcript := newSession(t)
	slow, err := llm.NewToolCallBlock("one", "slow", []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	fast, err := llm.NewToolCallBlock("two", "fast", []byte(`{"x":2}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := newAssistantToolUseMessage([]llm.AssistantBlock{slow, fast}, mustUsage(t, 3, 2), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "complete"))
	tool := &namedBatchTool{started: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})}, release: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})}}
	runtime := newAgent(t, transcript, scripted, tool)
	var mu sync.Mutex
	var settled []string
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
		if event, ok := event.(agent.ToolExecutionEndEvent); ok {
			mu.Lock()
			settled = append(settled, event.ToolName)
			mu.Unlock()
		}
	})
	done := make(chan error, 1)
	go func() { _, err := runtime.Run(context.Background(), "go"); done <- err }()
	<-tool.started["slow"]
	<-tool.started["fast"]
	close(tool.release["fast"])
	time.Sleep(10 * time.Millisecond)
	close(tool.release["slow"])
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotSettled := append([]string(nil), settled...)
	mu.Unlock()
	if !reflect.DeepEqual(gotSettled, []string{"fast", "slow"}) {
		t.Fatalf("settled order = %v, want completion order", gotSettled)
	}
	messages := transcript.Context().Messages()
	if got := []string{toolResultAt(t, messages, 2).ToolCallID(), toolResultAt(t, messages, 3).ToolCallID()}; !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Fatalf("durable tool result order = %v", got)
	}
}

func TestParallelToolPreflightIsSourceOrderedAndSettlesBlockedCallImmediately(t *testing.T) {
	transcript := newSession(t)
	slow, err := llm.NewToolCallBlock("one", "slow", []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	fast, err := llm.NewToolCallBlock("two", "fast", []byte(`{"x":2}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := newAssistantToolUseMessage([]llm.AssistantBlock{slow, fast}, mustUsage(t, 3, 2), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	model, err := newTestModel("scripted", "scripted", "scripted-1")
	if err != nil {
		t.Fatal(err)
	}
	tool := &namedBatchTool{
		started: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
		release: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
	}
	slowDefinition, err := provider.NewToolDefinition("slow", "slow", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	fastDefinition, err := provider.NewToolDefinition("fast", "fast", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	var before, after, lifecycle []string
	var lock sync.Mutex
	runtime, err := agent.New(agent.Config{
		Provider: newScriptedProvider(t, assistant, mustTextTerminal(t, "done")), Model: model,
		Tool: tool, Tools: []provider.ToolDefinition{slowDefinition, fastDefinition}, ToolExecution: agent.ToolExecutionParallel, Now: func() time.Time { return agentTestEpoch },
		BeforeToolCall: func(_ context.Context, input agent.BeforeToolCallContext) (agent.BeforeToolCallResult, error) {
			lock.Lock()
			before = append(before, input.ToolCall.Name())
			lock.Unlock()
			if input.ToolCall.Name() == "fast" {
				return agent.BeforeToolCallResult{Block: true, Reason: "policy blocked"}, nil
			}
			return agent.BeforeToolCallResult{}, nil
		},
		AfterToolCall: func(_ context.Context, input agent.AfterToolCallContext) (agent.AfterToolCallResult, error) {
			lock.Lock()
			after = append(after, input.ToolCall.Name())
			lock.Unlock()
			terminate := false
			return agent.AfterToolCallResult{Details: ptr(any(map[string]any{"hook": input.ToolCall.Name()})), Terminate: &terminate}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	subscribeTestTranscript(t, runtime, transcript)
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
		switch event := event.(type) {
		case agent.ToolExecutionStartEvent:
			lifecycle = append(lifecycle, "start:"+event.ToolName)
		case agent.ToolExecutionEndEvent:
			lifecycle = append(lifecycle, "end:"+event.ToolName)
		}
	})
	done := make(chan error, 1)
	go func() { _, runErr := runtime.Run(context.Background(), "go"); done <- runErr }()
	waitClosed(t, tool.started["slow"], "prepared slow tool")
	select {
	case <-tool.started["fast"]:
		t.Fatal("blocked fast tool was executed")
	default:
	}
	lock.Lock()
	gotBefore := append([]string(nil), before...)
	gotAfter := append([]string(nil), after...)
	gotLifecycle := append([]string(nil), lifecycle...)
	lock.Unlock()
	if !reflect.DeepEqual(gotBefore, []string{"slow", "fast"}) {
		t.Fatalf("before hooks = %v", gotBefore)
	}
	if len(gotAfter) != 0 {
		t.Fatalf("after hooks ran before prepared tool settled: %v", gotAfter)
	}
	if !reflect.DeepEqual(gotLifecycle, []string{"start:slow", "start:fast", "end:fast"}) {
		t.Fatalf("immediate lifecycle = %v", gotLifecycle)
	}
	close(tool.release["slow"])
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	lock.Lock()
	gotLifecycle = append([]string(nil), lifecycle...)
	gotAfter = append([]string(nil), after...)
	lock.Unlock()
	if !reflect.DeepEqual(gotLifecycle, []string{"start:slow", "start:fast", "end:fast", "end:slow"}) {
		t.Fatalf("final lifecycle = %v", gotLifecycle)
	}
	if !reflect.DeepEqual(gotAfter, []string{"slow"}) {
		t.Fatalf("after hooks = %v", gotAfter)
	}
	tool.mu.Lock()
	gotLookups := append([]string(nil), tool.supportLookups...)
	tool.mu.Unlock()
	if !reflect.DeepEqual(gotLookups, []string{"slow", "fast"}) {
		t.Fatalf("tool lookups = %v, want one source-ordered preflight lookup per call", gotLookups)
	}
	results := transcript.Context().Messages()
	if detail := toolResultAt(t, results, 2).Details(); string(detail) != `{"hook":"slow"}` {
		t.Fatalf("slow details = %s", detail)
	}
}

func TestToolHookPanicsBecomeAssociatedErrorResults(t *testing.T) {
	for _, test := range []struct {
		name       string
		before     agent.BeforeToolCallHook
		after      agent.AfterToolCallHook
		wantCalls  uint32
		wantPrefix string
	}{
		{
			name: "before",
			before: func(context.Context, agent.BeforeToolCallContext) (agent.BeforeToolCallResult, error) {
				panic("before boom")
			},
			wantPrefix: "before tool hook panicked: before boom",
		},
		{
			name: "after",
			after: func(context.Context, agent.AfterToolCallContext) (agent.AfterToolCallResult, error) {
				panic("after boom")
			},
			wantCalls:  1,
			wantPrefix: "after tool hook panicked: after boom",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			transcript := newSession(t)
			tool := &fakeTool{name: "echo", execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
				return agent.ToolOutput{Text: "ordinary result"}, nil
			}}
			runtime, err := agent.New(agent.Config{
				Provider: newScriptedProvider(t, mustToolUseTerminal(t, "call", "echo", []byte(`{}`)), mustTextTerminal(t, "done")),
				Model:    sessionTestModel(t), Tool: tool,
				BeforeToolCall: test.before, AfterToolCall: test.after,
				Now: func() time.Time { return agentTestEpoch },
			})
			if err != nil {
				t.Fatal(err)
			}
			subscribeTestTranscript(t, runtime, transcript)
			if result, err := runtime.Run(context.Background(), "go"); err != nil || !result.Succeeded() {
				t.Fatalf("Run = (%#v, %v)", result, err)
			}
			if tool.CallCount() != test.wantCalls {
				t.Fatalf("tool calls = %d, want %d", tool.CallCount(), test.wantCalls)
			}
			result := toolResultAt(t, transcript.Context().Messages(), 2)
			if !result.IsError() || !strings.HasPrefix(onlyText(t, result.Content()), test.wantPrefix) {
				t.Fatalf("hook result = error %t text %q", result.IsError(), onlyText(t, result.Content()))
			}
		})
	}
}

func TestUnmarshalableToolDetailsAreOmittedWithoutBlockingToolContinuation(t *testing.T) {
	transcript := newSession(t)
	tool := &fakeTool{name: "echo", execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		return agent.ToolOutput{Text: "ordinary result", Details: func() {}}, nil
	}}
	runtime, err := agent.New(agent.Config{
		Provider: newScriptedProvider(t,
			mustToolUseTerminal(t, "call", "echo", []byte(`{}`)),
			mustTextTerminal(t, "continued"),
		),
		Model: sessionTestModel(t), Tool: tool,
		Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	subscribeTestTranscript(t, runtime, transcript)
	result, err := runtime.Run(context.Background(), "go")
	if err != nil || !result.Succeeded() || result.ProviderTurns() != 2 || result.ToolExecutions() != 1 {
		t.Fatalf("Run = (%#v, %v), want settled tool continuation", result, err)
	}
	messages := transcript.Context().Messages()
	if roles := messageRoles(messages); !reflect.DeepEqual(roles, []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleToolResult, llm.RoleAssistant}) {
		t.Fatalf("message roles after invalid details = %v", roles)
	}
	toolResult := toolResultAt(t, messages, 2)
	if details := toolResult.Details(); len(details) != 0 || toolResult.IsError() || onlyText(t, toolResult.Content()) != "ordinary result" {
		t.Fatalf("tool result after invalid details = details %s error %t content %q", details, toolResult.IsError(), onlyText(t, toolResult.Content()))
	}
	continued, ok := messages[3].(llm.AssistantTextMessage)
	if !ok {
		t.Fatalf("continued message = %T, want llm.AssistantTextMessage", messages[3])
	}
	if got := onlyText(t, continued.Content()); got != "continued" {
		t.Fatalf("continued assistant text = %q", got)
	}
}

func TestToolDetailsAreIsolatedAcrossHookObserversAndDurability(t *testing.T) {
	transcript := newSession(t)
	updateDetails := map[string]any{"nested": map[string]any{"value": "update"}}
	outputDetails := map[string]any{"nested": map[string]any{"value": "output"}}
	tool := &fakeTool{name: "echo", execute: func(_ context.Context, _ []byte, report func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		report(agent.ToolUpdate{Text: "working", Details: updateDetails})
		return agent.ToolOutput{Text: "done", Details: outputDetails}, nil
	}}
	runtime, err := agent.New(agent.Config{
		Provider: newScriptedProvider(t, mustToolUseTerminal(t, "call", "echo", []byte(`{}`)), mustTextTerminal(t, "finished")),
		Model:    sessionTestModel(t), Tool: tool,
		AfterToolCall: func(_ context.Context, input agent.AfterToolCallContext) (agent.AfterToolCallResult, error) {
			details := input.Result.Details.(map[string]any)
			if got := details["nested"].(map[string]any)["value"]; got != "output" {
				t.Fatalf("after hook details = %#v", details)
			}
			details["nested"].(map[string]any)["value"] = "hook-mutated"
			return agent.AfterToolCallResult{}, nil
		},
		Now: func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	subscribeTestTranscript(t, runtime, transcript)

	mutateDetails := func(value any, replacement string) {
		value.(map[string]any)["nested"].(map[string]any)["value"] = replacement
	}
	readDetails := func(value any) any {
		return value.(map[string]any)["nested"].(map[string]any)["value"]
	}
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
		switch value := event.(type) {
		case agent.ToolExecutionUpdateEvent:
			mutateDetails(updateDetails, "source-mutated")
			mutateDetails(value.PartialResult.Details, "observer-mutated")
		case agent.ToolExecutionEndEvent:
			mutateDetails(outputDetails, "source-mutated")
			mutateDetails(value.Result.Details, "observer-mutated")
		}
	})
	var updates, ends int
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
		switch value := event.(type) {
		case agent.ToolExecutionUpdateEvent:
			updates++
			if got := readDetails(value.PartialResult.Details); got != "update" {
				t.Errorf("second update observer saw %v", got)
			}
		case agent.ToolExecutionEndEvent:
			ends++
			if got := readDetails(value.Result.Details); got != "output" {
				t.Errorf("second end observer saw %v", got)
			}
		}
	})
	if result, err := runtime.Run(context.Background(), "go"); err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	if updates != 1 || ends != 1 {
		t.Fatalf("tool event counts = updates %d, ends %d", updates, ends)
	}
	stored := toolResultAt(t, transcript.Context().Messages(), 2)
	var details map[string]map[string]string
	if err := json.Unmarshal(stored.Details(), &details); err != nil {
		t.Fatal(err)
	}
	if got := details["nested"]["value"]; got != "output" {
		t.Fatalf("durable details = %#v", details)
	}
}

func ptr[T any](value T) *T { return &value }

func TestSteeringFollowUpContinueAndTransformBoundaries(t *testing.T) {
	transcript := newSession(t)
	scripted := newScriptedProvider(t, mustTextTerminal(t, "first"), mustTextTerminal(t, "followed"), mustTextTerminal(t, "continued"))
	runtime := newAgent(t, transcript, scripted, nil)
	if err := runtime.Steer("steer now"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.FollowUp("follow later"); err != nil {
		t.Fatal(err)
	}
	if steering, followUp := runtime.Queues(); len(steering) != 1 || len(followUp) != 1 {
		t.Fatalf("queue snapshot = %d/%d", len(steering), len(followUp))
	}
	if _, err := runtime.Run(context.Background(), "initial"); err != nil {
		t.Fatal(err)
	}
	if scripted.CallCount() != 2 {
		t.Fatalf("provider calls after queues = %d", scripted.CallCount())
	}
	if err := runtime.Steer("continue"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if scripted.CallCount() != 3 {
		t.Fatalf("provider calls after continue = %d", scripted.CallCount())
	}
	if steering, followUp := runtime.Queues(); len(steering) != 0 || len(followUp) != 0 {
		t.Fatalf("queues after drain = %d/%d", len(steering), len(followUp))
	}
	if got := messageRoles(transcript.Context().Messages()); !reflect.DeepEqual(got, []llm.Role{llm.RoleUser, llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant}) {
		t.Fatalf("transcript roles = %v", got)
	}
}

func TestTransformContextIsProviderOnlyAndFailsBeforeProvider(t *testing.T) {
	t.Run("provider sees replacement snapshot, transcript does not", func(t *testing.T) {
		transcript := newSession(t)
		scripted := newScriptedProvider(t, mustTextTerminal(t, "done"))
		model, err := newTestModel("scripted", "scripted", "scripted-1")
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := agent.New(agent.Config{Provider: scripted, Model: model, Now: func() time.Time { return agentTestEpoch }, TransformContext: func(_ context.Context, messages []llm.ConversationMessage) ([]llm.ConversationMessage, error) {
			if len(messages) != 1 {
				t.Fatalf("transform input messages = %d", len(messages))
			}
			return nil, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		subscribeTestTranscript(t, runtime, transcript)
		if _, err := runtime.Run(context.Background(), "durable"); err != nil {
			t.Fatal(err)
		}
		if got := scripted.Requests()[0].Messages(); len(got) != 0 {
			t.Fatalf("provider request retained transformed messages: %d", len(got))
		}
		if got := transcript.Context().Messages(); len(got) != 2 || got[0].Role() != llm.RoleUser {
			t.Fatalf("transform changed transcript: %#v", got)
		}
	})
	t.Run("error is explicit and provider is not called", func(t *testing.T) {
		transcript := newSession(t)
		scripted := newScriptedProvider(t, mustTextTerminal(t, "unused"))
		model, err := newTestModel("scripted", "scripted", "scripted-1")
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := agent.New(agent.Config{Provider: scripted, Model: model, Now: func() time.Time { return agentTestEpoch }, TransformContext: func(context.Context, []llm.ConversationMessage) ([]llm.ConversationMessage, error) {
			return nil, context.DeadlineExceeded
		}})
		if err != nil {
			t.Fatal(err)
		}
		subscribeTestTranscript(t, runtime, transcript)
		result, err := runtime.Run(context.Background(), "blocked")
		terminal, ok := result.Terminal()
		failure, failed := terminal.(llm.AssistantFailureMessage)
		if err != nil || !ok || !failed || !errors.Is(failure.Failure().Cause(), agent.ErrContextTransform) {
			t.Fatalf("Run terminal=%T cause=%v error=%v, want synthetic ErrContextTransform", terminal, failure.Failure().Cause(), err)
		}
		if scripted.CallCount() != 0 {
			t.Fatalf("provider called after transform failure: %d", scripted.CallCount())
		}
	})
}

func TestToolLevelSequentialOverrideDowngradesWholeBatch(t *testing.T) {
	transcript := newSession(t)
	slow, err := llm.NewToolCallBlock("one", "slow", []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	fast, err := llm.NewToolCallBlock("two", "fast", []byte(`{"x":2}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := newAssistantToolUseMessage([]llm.AssistantBlock{slow, fast}, mustUsage(t, 3, 2), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "done"))
	tool := &namedBatchTool{mode: agent.ToolExecutionSequential, started: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})}, release: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})}}
	runtime := newAgent(t, transcript, scripted, tool)
	done := make(chan error, 1)
	go func() { _, err := runtime.Run(context.Background(), "go"); done <- err }()
	<-tool.started["slow"]
	select {
	case <-tool.started["fast"]:
		t.Fatal("fast tool started before sequential override predecessor settled")
	case <-time.After(20 * time.Millisecond):
	}
	close(tool.release["slow"])
	<-tool.started["fast"]
	close(tool.release["fast"])
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMixedToolBatchPreservesEverySourceResultAndOnlyAllTerminateStops(t *testing.T) {
	transcript := newSession(t)
	calls := make([]llm.AssistantBlock, 0, 4)
	for _, name := range []string{"ok", "missing", "failure", "terminate"} {
		call, err := llm.NewToolCallBlock(name+"-id", name, []byte(`{"x":1}`))
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	assistant, err := newAssistantToolUseMessage(calls, mustUsage(t, 3, 2), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "continued"))
	runtime := newAgent(t, transcript, scripted, mixedTool{})
	if _, err := runtime.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if scripted.CallCount() != 2 {
		t.Fatalf("partial terminate stopped batch/provider: %d calls", scripted.CallCount())
	}
	messages := transcript.Context().Messages()
	if got := []string{toolResultAt(t, messages, 2).ToolCallID(), toolResultAt(t, messages, 3).ToolCallID(), toolResultAt(t, messages, 4).ToolCallID(), toolResultAt(t, messages, 5).ToolCallID()}; !reflect.DeepEqual(got, []string{"ok-id", "missing-id", "failure-id", "terminate-id"}) {
		t.Fatalf("mixed source order = %v", got)
	}
	if !toolResultAt(t, messages, 3).IsError() || !toolResultAt(t, messages, 4).IsError() {
		t.Fatalf("missing/failure were not durable errors")
	}
}

func TestTerminatingBatchStillDrainsInitialSteeringAndFollowUp(t *testing.T) {
	transcript := newSession(t)
	first, err := llm.NewToolCallBlock("first", "terminate", []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := llm.NewToolCallBlock("second", "terminate", []byte(`{"x":2}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := newAssistantToolUseMessage([]llm.AssistantBlock{first, second}, mustUsage(t, 3, 2), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "followed"))
	runtime := newAgent(t, transcript, scripted, mixedTool{})
	if err := runtime.Steer("steer after terminate"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.FollowUp("follow after terminate"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	if scripted.CallCount() != 2 {
		t.Fatalf("provider calls = %d, want initial steering plus terminating batch and follow-up drain", scripted.CallCount())
	}
	if got := messageRoles(transcript.Context().Messages()); !reflect.DeepEqual(got, []llm.Role{
		llm.RoleUser, llm.RoleUser, llm.RoleAssistant, llm.RoleToolResult, llm.RoleToolResult,
		llm.RoleUser, llm.RoleAssistant,
	}) {
		t.Fatalf("terminating batch transcript roles = %v", got)
	}
}

func TestBatchCompletionClearsPendingToolBeforeNextTurnStarted(t *testing.T) {
	transcript := newSession(t)
	scripted := newScriptedProvider(t,
		mustToolUseTerminal(t, "call", "bash", []byte(`{"command":"one"}`)),
		mustTextTerminal(t, "done"),
	)
	runtime := newAgent(t, transcript, scripted, &fakeTool{name: "bash"})
	var observed bool
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
		started, ok := event.(agent.TurnStartEvent)
		if !ok || started.Turn != 2 {
			return
		}
		observed = true
		state := runtime.State()
		if state.Phase() != agent.PhaseProvider {
			t.Errorf("phase at next turn start = %s, want provider", state.Phase())
		}
		if pending, ok := state.PendingToolCall(); ok || pending != "" {
			t.Errorf("pending call at next turn start = %q/%t", pending, ok)
		}
		if pending := state.PendingToolCalls(); len(pending) != 0 {
			t.Errorf("pending calls at next turn start = %v", pending)
		}
	})
	if _, err := runtime.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if !observed {
		t.Fatal("did not observe second turn start")
	}
}

func TestContinueBusyAdmissionDoesNotDrainQueuedMessage(t *testing.T) {
	transcript := newSession(t)
	scripted := newScriptedProvider(t,
		mustTextTerminal(t, "first"),
		mustTextTerminal(t, "second"),
		mustTextTerminal(t, "drained"),
	)
	runtime := newAgent(t, transcript, scripted, nil)
	if _, err := runtime.Run(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	release := make(chan struct{})
	blockNextRun := true
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
		if _, ok := event.(agent.AgentStartEvent); !blockNextRun || !ok {
			return
		}
		close(blocked)
		<-release
	})
	if err := runtime.Steer("must remain queued"); err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { _, err := runtime.Run(context.Background(), "second"); runDone <- err }()
	waitClosed(t, blocked, "second run admission before prompt commit")
	if _, err := runtime.Continue(context.Background()); !errors.Is(err, agent.ErrBusy) {
		t.Fatalf("Continue while active error = %v, want busy", err)
	}
	if steering, followUp := runtime.Queues(); len(steering) != 1 || len(followUp) != 0 {
		t.Fatalf("failed Continue drained queues: steering=%d follow-up=%d", len(steering), len(followUp))
	}
	close(release)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

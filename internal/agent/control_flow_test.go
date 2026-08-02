package agent_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

type namedBatchTool struct {
	mu      sync.Mutex
	started map[string]chan struct{}
	release map[string]chan struct{}
	mode    agent.ToolExecutionMode
}

type mixedTool struct{}

func (mixedTool) Name() string              { return "mixed" }
func (mixedTool) Supports(name string) bool { return name != "missing" }
func (mixedTool) Execute(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return agent.ToolOutput{}, nil
}
func (mixedTool) ExecuteNamed(_ context.Context, name string, _ []byte, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	switch name {
	case "failure":
		return agent.ToolOutput{Text: "failed"}, errors.New("tool failed")
	case "terminate":
		return agent.ToolOutput{Text: "ending", Terminate: true}, nil
	default:
		return agent.ToolOutput{Text: name}, nil
	}
}

func (t *namedBatchTool) Name() string              { return "batch" }
func (t *namedBatchTool) Supports(name string) bool { return name == "slow" || name == "fast" }
func (t *namedBatchTool) ToolExecutionMode(string) (agent.ToolExecutionMode, bool) {
	return t.mode, t.mode != 0
}
func (t *namedBatchTool) Execute(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return agent.ToolOutput{}, nil
}
func (t *namedBatchTool) ExecuteNamed(ctx context.Context, name string, _ []byte, report func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	close(t.started[name])
	select {
	case <-t.release[name]:
	case <-ctx.Done():
		return agent.ToolOutput{Text: "cancelled"}, context.Cause(ctx)
	}
	report(agent.ToolUpdate{Text: name + " done"})
	return agent.ToolOutput{Text: name}, nil
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
	assistant, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{slow, fast}, mustUsage(t, 3, 2), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "complete"))
	tool := &namedBatchTool{started: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})}, release: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})}}
	runtime := newAgent(t, transcript, scripted, tool)
	var mu sync.Mutex
	var settled []string
	runtime.Subscribe(func(_ context.Context, event agent.Event) {
		if event.Kind == agent.EventToolSettled {
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

func TestSteeringFollowUpContinueAndTransformBoundaries(t *testing.T) {
	transcript := newSession(t)
	scripted := newScriptedProvider(t, mustTextTerminal(t, "first"), mustTextTerminal(t, "steered"), mustTextTerminal(t, "followed"), mustTextTerminal(t, "continued"))
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
	if scripted.CallCount() != 3 {
		t.Fatalf("provider calls after queues = %d", scripted.CallCount())
	}
	if err := runtime.Steer("continue"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if scripted.CallCount() != 4 {
		t.Fatalf("provider calls after continue = %d", scripted.CallCount())
	}
	if steering, followUp := runtime.Queues(); len(steering) != 0 || len(followUp) != 0 {
		t.Fatalf("queues after drain = %d/%d", len(steering), len(followUp))
	}
	if got := messageRoles(transcript.Context().Messages()); !reflect.DeepEqual(got, []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant}) {
		t.Fatalf("transcript roles = %v", got)
	}
}

func TestTransformContextIsProviderOnlyAndFailsBeforeProvider(t *testing.T) {
	t.Run("provider sees replacement snapshot, transcript does not", func(t *testing.T) {
		transcript := newSession(t)
		scripted := newScriptedProvider(t, mustTextTerminal(t, "done"))
		model, err := provider.NewModelRef("scripted", "scripted", "scripted-1")
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := agent.New(agent.Config{Provider: scripted, Transcript: transcript, Model: model, Now: func() time.Time { return agentTestEpoch }, TransformContext: func(_ context.Context, messages []llm.ConversationMessage) ([]llm.ConversationMessage, error) {
			if len(messages) != 1 {
				t.Fatalf("transform input messages = %d", len(messages))
			}
			return nil, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
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
		model, err := provider.NewModelRef("scripted", "scripted", "scripted-1")
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := agent.New(agent.Config{Provider: scripted, Transcript: transcript, Model: model, Now: func() time.Time { return agentTestEpoch }, TransformContext: func(context.Context, []llm.ConversationMessage) ([]llm.ConversationMessage, error) {
			return nil, context.DeadlineExceeded
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Run(context.Background(), "blocked"); !errors.Is(err, agent.ErrContextTransform) {
			t.Fatalf("Run error = %v", err)
		}
		if scripted.CallCount() != 0 {
			t.Fatalf("provider called after transform failure: %d", scripted.CallCount())
		}
	})
}

func TestAbortWaitsForParallelWorkersAndCommitsCancelledBatch(t *testing.T) {
	transcript := newSession(t)
	slow, err := llm.NewToolCallBlock("one", "slow", []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	fast, err := llm.NewToolCallBlock("two", "fast", []byte(`{"x":2}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{slow, fast}, mustUsage(t, 3, 2), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "must not run"))
	tool := &namedBatchTool{started: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})}, release: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})}}
	runtime := newAgent(t, transcript, scripted, tool)
	runDone := make(chan error, 1)
	go func() { _, err := runtime.Run(context.Background(), "go"); runDone <- err }()
	<-tool.started["slow"]
	<-tool.started["fast"]
	if err := runtime.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if scripted.CallCount() != 1 {
		t.Fatalf("provider calls after abort = %d", scripted.CallCount())
	}
	messages := transcript.Context().Messages()
	if got := []string{toolResultAt(t, messages, 2).ToolCallID(), toolResultAt(t, messages, 3).ToolCallID()}; !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Fatalf("cancel result order = %v", got)
	}
	if terminal := failureAt(t, messages, 4); terminal.FinishReason() != llm.FinishAborted {
		t.Fatalf("abort terminal = %s", terminal.FinishReason())
	}
	if state := runtime.State(); state.Phase() != agent.PhaseIdle {
		t.Fatalf("state after abort = %s", state.Phase())
	}
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
	assistant, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{slow, fast}, mustUsage(t, 3, 2), agentTestEpoch)
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
	assistant, err := llm.NewAssistantToolUseMessage(calls, mustUsage(t, 3, 2), agentTestEpoch)
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

func TestTerminatingBatchStillDrainsSteeringThenFollowUp(t *testing.T) {
	transcript := newSession(t)
	first, err := llm.NewToolCallBlock("first", "terminate", []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := llm.NewToolCallBlock("second", "terminate", []byte(`{"x":2}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{first, second}, mustUsage(t, 3, 2), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "steered"), mustTextTerminal(t, "followed"))
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
	if scripted.CallCount() != 3 {
		t.Fatalf("provider calls = %d, want terminating batch plus steering/follow-up drain", scripted.CallCount())
	}
	if got := messageRoles(transcript.Context().Messages()); !reflect.DeepEqual(got, []llm.Role{
		llm.RoleUser, llm.RoleAssistant, llm.RoleToolResult, llm.RoleToolResult,
		llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant,
	}) {
		t.Fatalf("terminating batch transcript roles = %v", got)
	}
}

func TestSequentialCancellationAssociatesUnstartedCallsAndStopsProvider(t *testing.T) {
	transcript := newSession(t)
	first, err := llm.NewToolCallBlock("first", "slow", []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := llm.NewToolCallBlock("second", "fast", []byte(`{"x":2}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{first, second}, mustUsage(t, 3, 2), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "must not run"))
	tool := &namedBatchTool{
		started: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
		release: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
	}
	model, err := provider.NewModelRef("scripted", "scripted", "scripted-1")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.New(agent.Config{
		Provider:      scripted,
		Transcript:    transcript,
		Model:         model,
		Tool:          tool,
		ToolExecution: agent.ToolExecutionSequential,
		Now:           func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { _, err := runtime.Run(context.Background(), "go"); runDone <- err }()
	waitClosed(t, tool.started["slow"], "first sequential tool start")
	select {
	case <-tool.started["fast"]:
		t.Fatal("second sequential tool started before cancellation")
	default:
	}
	if err := runtime.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if scripted.CallCount() != 1 {
		t.Fatalf("provider calls after sequential cancellation = %d", scripted.CallCount())
	}
	select {
	case <-tool.started["fast"]:
		t.Fatal("second sequential tool started after cancellation")
	default:
	}
	messages := transcript.Context().Messages()
	if got := []string{toolResultAt(t, messages, 2).ToolCallID(), toolResultAt(t, messages, 3).ToolCallID()}; !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("cancelled result associations = %v", got)
	}
	if !toolResultAt(t, messages, 2).IsError() || !toolResultAt(t, messages, 3).IsError() {
		t.Fatalf("cancelled batch results must be durable errors")
	}
	if failure := failureAt(t, messages, 4); failure.FinishReason() != llm.FinishAborted {
		t.Fatalf("terminal finish = %s, want aborted", failure.FinishReason())
	}
}

func TestCancelledSequentialBatchCommitFaultStopsBeforeSuccessor(t *testing.T) {
	base := newSession(t)
	transcript := &selectiveFailingTranscript{
		base: base,
		fail: func(message llm.ConversationMessage) bool {
			result, ok := message.(llm.ToolResultMessage)
			return ok && result.ToolCallID() == "second"
		},
	}
	first, err := llm.NewToolCallBlock("first", "slow", []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := llm.NewToolCallBlock("second", "fast", []byte(`{"x":2}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{first, second}, mustUsage(t, 3, 2), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "must not run"))
	tool := &namedBatchTool{
		started: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
		release: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
	}
	model, err := provider.NewModelRef("scripted", "scripted", "scripted-1")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.New(agent.Config{
		Provider:      scripted,
		Transcript:    transcript,
		Model:         model,
		Tool:          tool,
		ToolExecution: agent.ToolExecutionSequential,
		Now:           func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { _, err := runtime.Run(context.Background(), "go"); runDone <- err }()
	waitClosed(t, tool.started["slow"], "first sequential tool start")
	if err := runtime.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-runDone; !errors.Is(err, agent.ErrTranscriptCommit) {
		t.Fatalf("Run error = %v, want transcript commit fault", err)
	}
	if scripted.CallCount() != 1 {
		t.Fatalf("provider calls after tool-result fault = %d", scripted.CallCount())
	}
	if got := messageRoles(base.Context().Messages()); !reflect.DeepEqual(got, []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleToolResult}) {
		t.Fatalf("durable messages after second result fault = %v", got)
	}
	if result := toolResultAt(t, base.Context().Messages(), 2); result.ToolCallID() != "first" || !result.IsError() {
		t.Fatalf("first durable cancellation result = id %q error %t", result.ToolCallID(), result.IsError())
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
	runtime.Subscribe(func(_ context.Context, event agent.Event) {
		if event.Kind != agent.EventTurnStarted || event.Turn != 2 {
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
	runtime.Subscribe(func(_ context.Context, event agent.Event) {
		if !blockNextRun || event.Kind != agent.EventRunStarted {
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

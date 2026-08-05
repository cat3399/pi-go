package agent_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
	"github.com/cat3399/pi-go/internal/tool"
)

func TestRunToolLoopCommitsCausalOrder(t *testing.T) {
	transcript := newSession(t)
	first := mustToolUseTerminal(t, "call-1", "bash", []byte(`{"command":"printf ok"}`))
	final := mustTextTerminal(t, "done")
	scripted := newScriptedProvider(t, first, final)

	executor := &fakeTool{name: "bash"}
	executor.execute = func(
		_ context.Context,
		arguments []byte,
		report func(agent.ToolUpdate),
	) (agent.ToolOutput, error) {
		if got := string(arguments); got != `{"command":"printf ok"}` {
			return agent.ToolOutput{}, fmt.Errorf("arguments = %s", got)
		}
		if roles := messageRoles(transcript.Context().Messages()); !reflect.DeepEqual(
			roles,
			[]llm.Role{llm.RoleUser, llm.RoleAssistant},
		) {
			return agent.ToolOutput{}, fmt.Errorf("tool observed roles %v", roles)
		}
		report(agent.ToolUpdate{Text: "working"})
		return agent.ToolOutput{Text: "ok"}, nil
	}
	runtime := newAgent(t, transcript, scripted, executor)

	var lifecycle []string
	var providerProgress int
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
		switch event := event.(type) {
		case agent.MessageStartEvent:
			partial, ok := event.Message.(agentmsg.AssistantPartial)
			if !ok {
				return
			}
			providerProgress++
			if partial.Snapshot().FinishReason() != llm.FinishPending {
				t.Errorf("provider progress exposed terminal finish %s", partial.Snapshot().FinishReason())
			}
			return
		case agent.MessageUpdateEvent:
			providerProgress++
			partial := event.AssistantMessageEvent.Partial()
			if partial.FinishReason() != llm.FinishPending {
				t.Errorf("provider progress exposed terminal finish %s", partial.FinishReason())
			}
			if reflect.TypeOf(partial.ProviderEvent()) != reflect.TypeOf(event.AssistantMessageEvent.Event()) {
				t.Error("assistantMessageEvent partial/raw event diverged")
			}
			return
		case agent.ToolExecutionUpdateEvent:
			return
		case agent.AgentStartEvent:
			lifecycle = append(lifecycle, "run")
		case agent.TurnStartEvent:
			lifecycle = append(lifecycle, fmt.Sprintf("turn:%d", event.Turn))
			if event.Turn == 2 {
				if got := len(transcript.Context().Messages()); got != 3 {
					t.Errorf("ProviderTurn2 started with %d durable messages, want 3", got)
				}
			}
		case agent.MessageEndEvent:
			standard := event.Message.(agentmsg.LLM).Conversation()
			label := "message:" + standard.Role().String()
			if assistant, ok := standard.(llm.AssistantTerminal); ok {
				label += ":" + assistant.FinishReason().String()
			}
			lifecycle = append(lifecycle, label)
		case agent.ToolExecutionStartEvent:
			lifecycle = append(lifecycle, "tool:start")
			if got := len(transcript.Context().Messages()); got != 2 {
				t.Errorf("tool started with %d durable messages, want 2", got)
			}
		case agent.ToolExecutionEndEvent:
			lifecycle = append(lifecycle, "tool:end")
		case agent.TurnEndEvent:
			lifecycle = append(lifecycle, fmt.Sprintf("turn-end:%d", event.Turn))
		case agent.AgentEndEvent:
			lifecycle = append(lifecycle, "run-end")
		}
	})

	result, err := runtime.Run(context.Background(), "run it")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.Succeeded() || result.ProviderTurns() != 2 || result.ToolExecutions() != 1 {
		t.Fatalf("result = success %t, provider turns %d, tools %d", result.Succeeded(), result.ProviderTurns(), result.ToolExecutions())
	}
	if executor.CallCount() != 1 || scripted.CallCount() != 2 {
		t.Fatalf("calls = tool %d, provider %d", executor.CallCount(), scripted.CallCount())
	}
	if providerProgress == 0 {
		t.Fatal("provider stream produced no observable partial snapshots")
	}

	wantLifecycle := []string{
		"run",
		"turn:1",
		"message:user",
		"message:assistant:toolUse",
		"tool:start",
		"tool:end",
		"message:toolResult",
		"turn-end:1",
		"turn:2",
		"message:assistant:stop",
		"turn-end:2",
		"run-end",
	}
	if !reflect.DeepEqual(lifecycle, wantLifecycle) {
		t.Fatalf("lifecycle = %v, want %v", lifecycle, wantLifecycle)
	}

	messages := transcript.Context().Messages()
	if roles := messageRoles(messages); !reflect.DeepEqual(roles, []llm.Role{
		llm.RoleUser,
		llm.RoleAssistant,
		llm.RoleToolResult,
		llm.RoleAssistant,
	}) {
		t.Fatalf("durable roles = %v", roles)
	}
	toolResult := toolResultAt(t, messages, 2)
	if toolResult.IsError() || toolResult.ToolCallID() != "call-1" || toolResult.ToolName() != "bash" || onlyText(t, toolResult.Content()) != "ok" {
		t.Fatalf("tool result = id %q, name %q, error %t, text %q", toolResult.ToolCallID(), toolResult.ToolName(), toolResult.IsError(), onlyText(t, toolResult.Content()))
	}
	provenance, ok := transcript.Context().AssistantProvenance()
	if !ok || provenance.Provider != "scripted" || provenance.API != "scripted" || provenance.Model != "scripted-1" || provenance.Cost != session.ZeroUsageCost() {
		t.Fatalf("assistant provenance = (%#v, %t)", provenance, ok)
	}

	requests := scripted.Requests()
	if got := messageRoles(requests[0].Messages()); !reflect.DeepEqual(got, []llm.Role{llm.RoleUser}) {
		t.Fatalf("request 1 roles = %v", got)
	}
	if got := messageRoles(requests[1].Messages()); !reflect.DeepEqual(got, []llm.Role{
		llm.RoleUser,
		llm.RoleAssistant,
		llm.RoleToolResult,
	}) {
		t.Fatalf("request 2 roles = %v", got)
	}
	requestResult := toolResultAt(t, requests[1].Messages(), 2)
	if requestResult.ToolCallID() != "call-1" || onlyText(t, requestResult.Content()) != "ok" {
		t.Fatalf("request 2 tool result = %#v", requestResult)
	}
	state := runtime.State()
	if state.Phase() != agent.PhaseIdle {
		t.Fatalf("state after Run = %s, want idle", state.Phase())
	}
}

func TestAgentCanRunAgainOnlyAfterPriorSettlement(t *testing.T) {
	transcript := newSession(t)
	scripted := newScriptedProvider(t, mustTextTerminal(t, "first"), mustTextTerminal(t, "second"))
	runtime := newAgent(t, transcript, scripted, nil)

	first, err := runtime.Run(context.Background(), "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := runtime.Run(context.Background(), "two")
	if err != nil {
		t.Fatal(err)
	}
	if first.RunID() != 1 || second.RunID() != 2 || !first.Succeeded() || !second.Succeeded() {
		t.Fatalf("results = first id/success %d/%t, second %d/%t", first.RunID(), first.Succeeded(), second.RunID(), second.Succeeded())
	}
	if roles := messageRoles(transcript.Context().Messages()); !reflect.DeepEqual(roles, []llm.Role{
		llm.RoleUser,
		llm.RoleAssistant,
		llm.RoleUser,
		llm.RoleAssistant,
	}) {
		t.Fatalf("two-run transcript roles = %v", roles)
	}
	requests := scripted.Requests()
	if len(requests) != 2 || len(requests[0].Messages()) != 1 || len(requests[1].Messages()) != 3 {
		t.Fatalf("request snapshots = %d calls, message counts %d/%d", len(requests), len(requests[0].Messages()), len(requests[1].Messages()))
	}
}

func TestNewAndRunRejectInvalidPreflightWithoutState(t *testing.T) {
	transcript := newSession(t)
	scripted := newScriptedProvider(t, mustTextTerminal(t, "unused"))
	model, err := newTestModel("scripted", "scripted", "scripted-1")
	if err != nil {
		t.Fatal(err)
	}
	blankTool := &fakeTool{name: " \t"}

	for name, config := range map[string]agent.Config{
		"nil provider":        {Model: model},
		"zero model":          {Provider: scripted},
		"invalid system":      {Provider: scripted, Model: model, SystemPrompt: string([]byte{0xff})},
		"blank tool":          {Provider: scripted, Model: model, Tool: blankTool},
		"panicking tool name": {Provider: scripted, Model: model, Tool: panickingNameTool{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := agent.New(config); !errors.Is(err, agent.ErrInvalidConfig) {
				t.Fatalf("agent.New() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
	if _, err := agent.NewBashExecutor(nil); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("NewBashExecutor(nil) error = %v", err)
	}

	runtime := newAgent(t, transcript, scripted, nil)
	if _, err := runtime.Run(nil, "hello"); !errors.Is(err, agent.ErrInvalidRun) {
		t.Fatalf("Run(nil) error = %v, want ErrInvalidRun", err)
	}
	if len(transcript.Context().Messages()) != 0 || scripted.CallCount() != 0 {
		t.Fatalf("invalid preflight changed state: messages %d, calls %d", len(transcript.Context().Messages()), scripted.CallCount())
	}

	var badClockCalls atomic.Uint32
	badClock, err := agent.New(agent.Config{
		Provider: scripted,
		Model:    model,
		Now: func() time.Time {
			if badClockCalls.Add(1) == 1 {
				panic("clock broke")
			}
			return agentTestEpoch
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := badClock.Run(context.Background(), "hello"); !errors.Is(err, agent.ErrInvalidRun) || !errors.Is(err, agent.ErrInvariant) {
		t.Fatalf("Run(panicking clock) error = %v", err)
	}
	if badClock.State().Phase() != agent.PhaseIdle || len(transcript.Context().Messages()) != 0 || scripted.CallCount() != 0 {
		t.Fatal("panicking preflight clock started a run")
	}
	if result, err := badClock.Run(context.Background(), "recovered"); err != nil || !result.Succeeded() {
		t.Fatalf("Run after panicking preflight = success %t, error %v", result.Succeeded(), err)
	}
}

func TestProviderTerminalFailureAbortAndContractFailure(t *testing.T) {
	t.Run("expected error event", func(t *testing.T) {
		transcript := newSession(t)
		scripted := newScriptedProvider(t)
		runtime := newAgent(t, transcript, scripted, nil)

		result, err := runtime.Run(context.Background(), "hello")
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		terminal, ok := result.Terminal()
		if !ok || terminal.FinishReason() != llm.FinishError {
			t.Fatalf("terminal = (%T, %t)", terminal, ok)
		}
		failure := failureAt(t, transcript.Context().Messages(), 1)
		if failure.ErrorMessage() != provider.ErrQueueExhausted.Error() {
			t.Fatalf("failure text = %q", failure.ErrorMessage())
		}
		if scripted.CallCount() != 1 || result.ProviderTurns() != 1 || result.ToolExecutions() != 0 {
			t.Fatalf("counts = provider %d/%d, tools %d", scripted.CallCount(), result.ProviderTurns(), result.ToolExecutions())
		}
	})

	t.Run("pre-cancelled context settles an aborted run", func(t *testing.T) {
		transcript := newSession(t)
		scripted := newScriptedProvider(t, mustTextTerminal(t, "unused"))
		runtime := newAgent(t, transcript, scripted, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		result, err := runtime.Run(ctx, "hello")
		terminal, ok := result.Terminal()
		if err != nil || !ok || terminal.FinishReason() != llm.FinishAborted {
			t.Fatalf("pre-cancelled Run = terminal %T error %v", terminal, err)
		}
		if len(transcript.Context().Messages()) != 2 || scripted.CallCount() != 1 || runtime.State().Phase() != agent.PhaseIdle {
			t.Fatalf("settled run state: messages %d, calls %d, phase %s", len(transcript.Context().Messages()), scripted.CallCount(), runtime.State().Phase())
		}
	})

	t.Run("raw stream failure is normalized once", func(t *testing.T) {
		transcript := newSession(t)
		broken := &brokenProvider{stream: &brokenStream{err: errors.New("read exploded")}}
		runtime := newAgent(t, transcript, broken, nil)

		result, err := runtime.Run(context.Background(), "hello")
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		terminal, ok := result.Terminal()
		if !ok || terminal.FinishReason() != llm.FinishError {
			t.Fatalf("terminal = (%T, %t)", terminal, ok)
		}
		messages := transcript.Context().Messages()
		if len(messages) != 2 || failureAt(t, messages, 1).ErrorMessage() != "Provider stream failed" {
			t.Fatalf("messages = %d, terminal = %#v", len(messages), messages[len(messages)-1])
		}
		if broken.stream.closeCalls.Load() != 1 {
			t.Fatalf("stream Close calls = %d, want 1", broken.stream.closeCalls.Load())
		}
	})

	t.Run("panicking stream close is attempted once", func(t *testing.T) {
		transcript := newSession(t)
		scripted := newScriptedProvider(t, mustTextTerminal(t, "discarded"))
		panicking := &panicCloseProvider{inner: scripted}
		runtime := newAgent(t, transcript, panicking, nil)

		result, err := runtime.Run(context.Background(), "hello")
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		terminal, ok := result.Terminal()
		if !ok || terminal.FinishReason() != llm.FinishError {
			t.Fatalf("terminal = (%T, %t)", terminal, ok)
		}
		if panicking.closeCalls.Load() != 1 {
			t.Fatalf("stream Close calls = %d, want 1", panicking.closeCalls.Load())
		}
		messages := transcript.Context().Messages()
		if len(messages) != 2 || !strings.Contains(failureAt(t, messages, 1).ErrorMessage(), "close broke") {
			t.Fatalf("messages = %#v", messages)
		}
	})

	t.Run("length terminal never executes a tool", func(t *testing.T) {
		transcript := newSession(t)
		executor := &fakeTool{name: "bash"}
		scripted := newScriptedProvider(t, mustLengthTerminal(t, "truncated"))
		runtime := newAgent(t, transcript, scripted, executor)

		result, err := runtime.Run(context.Background(), "hello")
		if err != nil || !result.Succeeded() {
			t.Fatalf("Run() = success %t, error %v", result.Succeeded(), err)
		}
		if executor.CallCount() != 0 || result.ToolExecutions() != 0 {
			t.Fatalf("length terminal executed tool %d/%d times", executor.CallCount(), result.ToolExecutions())
		}
	})
}

func TestToolFailuresBecomeAssociatedErrorResultsAndContinue(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		assertToolFailureContinues(t, "unknown", []byte(`{}`), &fakeTool{name: "bash"}, func(result llm.ToolResultMessage) {
			if !result.IsError() || onlyText(t, result.Content()) != "Tool unknown not found" {
				t.Fatalf("missing result = error %t, text %q", result.IsError(), onlyText(t, result.Content()))
			}
		})
	})

	t.Run("execute error preserves output", func(t *testing.T) {
		cause := errors.New("tool failed")
		executor := &fakeTool{
			name: "bash",
			execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
				return agent.ToolOutput{Text: "captured output\nexit 9"}, cause
			},
		}
		assertToolFailureContinues(t, "bash", []byte(`{"command":"exit 9"}`), executor, func(result llm.ToolResultMessage) {
			if !result.IsError() || onlyText(t, result.Content()) != cause.Error() {
				t.Fatalf("execute result = error %t, text %q", result.IsError(), onlyText(t, result.Content()))
			}
		})
	})

	t.Run("panic", func(t *testing.T) {
		executor := &fakeTool{
			name: "bash",
			execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
				panic("kaboom")
			},
		}
		assertToolFailureContinues(t, "bash", []byte(`{"command":"panic"}`), executor, func(result llm.ToolResultMessage) {
			if !result.IsError() || !strings.Contains(onlyText(t, result.Content()), "kaboom") {
				t.Fatalf("panic result = error %t, text %q", result.IsError(), onlyText(t, result.Content()))
			}
		})
	})

	t.Run("invalid bash arguments", func(t *testing.T) {
		bash, err := tool.NewBash(tool.BashOptions{WorkingDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		executor, err := agent.NewBashExecutor(bash)
		if err != nil {
			t.Fatal(err)
		}
		assertToolFailureContinues(t, "bash", []byte(`{}`), executor, func(result llm.ToolResultMessage) {
			if !result.IsError() || !strings.Contains(onlyText(t, result.Content()), "command is required") {
				t.Fatalf("invalid result = error %t, text %q", result.IsError(), onlyText(t, result.Content()))
			}
		})
	})
}

func assertToolFailureContinues(
	t *testing.T,
	name string,
	arguments []byte,
	executor agent.ToolExecutor,
	assertResult func(llm.ToolResultMessage),
) {
	t.Helper()
	transcript := newSession(t)
	scripted := newScriptedProvider(
		t,
		mustToolUseTerminal(t, "call-failure", name, arguments),
		mustTextTerminal(t, "explained"),
	)
	runtime := newAgent(t, transcript, scripted, executor)

	result, err := runtime.Run(context.Background(), "run")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.Succeeded() || result.ProviderTurns() != 2 {
		t.Fatalf("result = success %t, provider turns %d", result.Succeeded(), result.ProviderTurns())
	}
	messages := transcript.Context().Messages()
	if len(messages) != 4 {
		t.Fatalf("message count = %d, want 4", len(messages))
	}
	toolResult := toolResultAt(t, messages, 2)
	if toolResult.ToolCallID() != "call-failure" || toolResult.ToolName() != name {
		t.Fatalf("association = (%q, %q)", toolResult.ToolCallID(), toolResult.ToolName())
	}
	assertResult(toolResult)
	if scripted.CallCount() != 2 {
		t.Fatalf("provider calls = %d, want 2", scripted.CallCount())
	}
}

func TestLateToolUpdatesAreDiscardedAfterSettlement(t *testing.T) {
	transcript := newSession(t)
	var late func(agent.ToolUpdate)
	executor := &fakeTool{
		name: "bash",
		execute: func(_ context.Context, _ []byte, report func(agent.ToolUpdate)) (agent.ToolOutput, error) {
			report(agent.ToolUpdate{Text: "running"})
			late = report
			return agent.ToolOutput{Text: "ok"}, nil
		},
	}
	scripted := newScriptedProvider(
		t,
		mustToolUseTerminal(t, "call-update", "bash", []byte(`{"command":"ok"}`)),
		mustTextTerminal(t, "done"),
	)
	runtime := newAgent(t, transcript, scripted, executor)
	var progress atomic.Uint32
	var total atomic.Uint32
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
		total.Add(1)
		if _, ok := event.(agent.ToolExecutionUpdateEvent); ok {
			progress.Add(1)
		}
	})

	if _, err := runtime.Run(context.Background(), "run"); err != nil {
		t.Fatal(err)
	}
	before := total.Load()
	if late == nil || progress.Load() != 1 {
		t.Fatalf("accepted progress = %d, callback nil %t", progress.Load(), late == nil)
	}
	late(agent.ToolUpdate{Text: "too late"})
	if progress.Load() != 1 || total.Load() != before {
		t.Fatalf("late update changed events: progress %d, total %d/%d", progress.Load(), total.Load(), before)
	}
}

func TestBusyAndWaitForIdleIncludeObserverSettlement(t *testing.T) {
	transcript := newSession(t)
	scripted := newScriptedProvider(t, mustTextTerminal(t, "done"))
	runtime := newAgent(t, transcript, scripted, nil)
	settling := make(chan struct{})
	release := make(chan struct{})
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
		if _, ok := event.(agent.AgentEndEvent); ok {
			close(settling)
			<-release
		}
	})

	runDone := make(chan struct{})
	var runErr error
	go func() {
		_, runErr = runtime.Run(context.Background(), "first")
		close(runDone)
	}()
	waitClosed(t, settling, "run-settled observer")
	if phase := runtime.State().Phase(); phase != agent.PhaseSettling {
		t.Fatalf("phase while final observer blocks = %s, want settling", phase)
	}
	if _, err := runtime.Run(context.Background(), "second"); !errors.Is(err, agent.ErrBusy) {
		t.Fatalf("second Run error = %v, want ErrBusy", err)
	}
	waitContext, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	if err := runtime.WaitForIdle(waitContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitForIdle(cancelled) error = %v", err)
	}
	assertOpen(t, runDone, "Run")
	close(release)
	waitClosed(t, runDone, "Run")
	if runErr != nil {
		t.Fatalf("Run() error = %v", runErr)
	}
	if err := runtime.WaitForIdle(context.Background()); err != nil || runtime.State().Phase() != agent.PhaseIdle {
		t.Fatalf("settled state = phase %s, wait error %v", runtime.State().Phase(), err)
	}
	if err := runtime.Abort(context.Background()); err != nil {
		t.Fatalf("Abort(idle) error = %v", err)
	}
}

func TestBusyRunDoesNotInvokeOrBlockOnSharedClock(t *testing.T) {
	scripted := newScriptedProvider(t, mustTextTerminal(t, "done"))
	model, err := newTestModel("scripted", "scripted", "scripted-1")
	if err != nil {
		t.Fatal(err)
	}
	clockEntered := make(chan struct{})
	releaseClock := make(chan struct{})
	var clockCalls atomic.Uint32
	runtime, err := agent.New(agent.Config{
		Provider:     scripted,
		Model:        model,
		SystemPrompt: "system",
		Now: func() time.Time {
			if clockCalls.Add(1) == 1 {
				close(clockEntered)
				<-releaseClock
			}
			return agentTestEpoch
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, runErr := runtime.Run(context.Background(), "first")
		firstDone <- runErr
	}()
	waitClosed(t, clockEntered, "first run clock")

	secondDone := make(chan error, 1)
	go func() {
		_, runErr := runtime.Run(context.Background(), "second")
		secondDone <- runErr
	}()
	select {
	case runErr := <-secondDone:
		if !errors.Is(runErr, agent.ErrBusy) {
			t.Fatalf("second Run error = %v, want ErrBusy", runErr)
		}
	case <-time.After(2 * time.Second):
		close(releaseClock)
		<-secondDone
		t.Fatal("busy Run blocked on the active run clock")
	}
	if calls := clockCalls.Load(); calls != 1 {
		t.Fatalf("clock calls before releasing active run = %d, want 1", calls)
	}

	close(releaseClock)
	if runErr := <-firstDone; runErr != nil {
		t.Fatalf("first Run error = %v", runErr)
	}
	if calls := clockCalls.Load(); calls != 1 {
		t.Fatalf("total clock calls = %d, want prompt timestamp only", calls)
	}
}

func TestProviderTwoCancellationUsesProviderTerminal(t *testing.T) {
	transcript := newSession(t)
	providerTwoStarted := make(chan struct{})
	first, err := provider.FixedResponseStep(mustToolUseTerminal(
		t,
		"call-provider-cancel",
		"bash",
		[]byte(`{"command":"ok"}`),
	))
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.FactoryResponseStep(func(
		ctx context.Context,
		_ provider.Request,
		_ uint64,
	) (llm.AssistantTerminal, error) {
		close(providerTwoStarted)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t)
	if err := scripted.SetResponses([]provider.ScriptStep{first, second}); err != nil {
		t.Fatal(err)
	}
	executor := &fakeTool{
		name: "bash",
		execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
			return agent.ToolOutput{Text: "normal"}, nil
		},
	}
	runtime := newAgent(t, transcript, scripted, executor)

	type outcome struct {
		result agent.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := runtime.Run(context.Background(), "run")
		done <- outcome{result: result, err: err}
	}()
	waitClosed(t, providerTwoStarted, "provider turn two")
	if err := runtime.Abort(context.Background()); err != nil {
		t.Fatalf("Abort() error = %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("Run() error = %v", got.err)
	}
	terminal, ok := got.result.Terminal()
	if !ok || terminal.FinishReason() != llm.FinishAborted {
		t.Fatalf("terminal = (%T, %t)", terminal, ok)
	}
	if scripted.CallCount() != 2 || got.result.ProviderTurns() != 2 {
		t.Fatalf("provider calls = %d/%d, want 2", scripted.CallCount(), got.result.ProviderTurns())
	}
	messages := transcript.Context().Messages()
	if len(messages) != 4 || toolResultAt(t, messages, 2).IsError() || failureAt(t, messages, 3).FinishReason() != llm.FinishAborted {
		t.Fatalf("provider cancellation transcript = %#v", messages)
	}
}

func TestMultipleToolCallsExecuteAsOneBatch(t *testing.T) {
	t.Run("multiple calls in first response", func(t *testing.T) {
		transcript := newSession(t)
		callOne, err := llm.NewToolCallBlock("one", "bash", []byte(`{"command":"one"}`))
		if err != nil {
			t.Fatal(err)
		}
		callTwo, err := llm.NewToolCallBlock("two", "bash", []byte(`{"command":"two"}`))
		if err != nil {
			t.Fatal(err)
		}
		terminal, err := newAssistantToolUseMessage(
			[]llm.AssistantBlock{callOne, callTwo},
			mustUsage(t, 1, 1),
			agentTestEpoch,
		)
		if err != nil {
			t.Fatal(err)
		}
		executor := &fakeTool{name: "bash"}
		scripted := newScriptedProvider(t, terminal, mustTextTerminal(t, "done"))
		runtime := newAgent(t, transcript, scripted, executor)

		result, err := runtime.Run(context.Background(), "run")
		if err != nil {
			t.Fatal(err)
		}
		if executor.CallCount() != 2 || result.ToolExecutions() != 2 {
			t.Fatalf("batch calls executed %d/%d tools", executor.CallCount(), result.ToolExecutions())
		}
		messages := transcript.Context().Messages()
		if !reflect.DeepEqual(messageRoles(messages), []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleToolResult, llm.RoleToolResult, llm.RoleAssistant}) {
			t.Fatalf("batch transcript = %#v", messages)
		}
	})

	t.Run("tool request in second response", func(t *testing.T) {
		transcript := newSession(t)
		executor := &fakeTool{
			name: "bash",
			execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
				return agent.ToolOutput{Text: "first done"}, nil
			},
		}
		scripted := newScriptedProvider(
			t,
			mustToolUseTerminal(t, "first", "bash", []byte(`{"command":"one"}`)),
			mustToolUseTerminal(t, "second", "bash", []byte(`{"command":"two"}`)),
		)
		runtime := newAgent(t, transcript, scripted, executor)

		result, err := runtime.Run(context.Background(), "run")
		if err != nil {
			t.Fatal(err)
		}
		if executor.CallCount() != 2 || result.ToolExecutions() != 2 || scripted.CallCount() != 3 {
			t.Fatalf("counts = tool %d/%d, provider %d", executor.CallCount(), result.ToolExecutions(), scripted.CallCount())
		}
		messages := transcript.Context().Messages()
		if !reflect.DeepEqual(messageRoles(messages), []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleToolResult, llm.RoleAssistant, llm.RoleToolResult, llm.RoleAssistant}) {
			t.Fatalf("second batch transcript = %#v", messages)
		}
	})
}

type brokenProvider struct {
	stream *brokenStream
}

func (p *brokenProvider) Stream(context.Context, provider.Request) provider.EventStream {
	return p.stream
}

type brokenStream struct {
	err        error
	closeCalls atomic.Uint32
}

func (s *brokenStream) Next() (llm.StreamEvent, error) { return nil, s.err }
func (s *brokenStream) Close() error {
	s.closeCalls.Add(1)
	return nil
}

var _ provider.Provider = (*brokenProvider)(nil)
var _ provider.EventStream = (*brokenStream)(nil)

type panicCloseProvider struct {
	inner      provider.Provider
	closeCalls atomic.Uint32
}

func (p *panicCloseProvider) Stream(ctx context.Context, request provider.Request) provider.EventStream {
	return &panicCloseStream{inner: p.inner.Stream(ctx, request), closeCalls: &p.closeCalls}
}

type panicCloseStream struct {
	inner      provider.EventStream
	closeCalls *atomic.Uint32
}

func (s *panicCloseStream) Next() (llm.StreamEvent, error) { return s.inner.Next() }
func (s *panicCloseStream) Close() error {
	s.closeCalls.Add(1)
	_ = s.inner.Close()
	panic("close broke")
}

var _ provider.Provider = (*panicCloseProvider)(nil)
var _ provider.EventStream = (*panicCloseStream)(nil)

type panickingNameTool struct{}

func (panickingNameTool) Name() string { panic("name broke") }
func (panickingNameTool) Execute(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return agent.ToolOutput{}, nil
}

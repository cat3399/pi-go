package agent_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
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
		"nil provider":        {Transcript: transcript, Model: model},
		"nil transcript":      {Provider: scripted, Model: model},
		"zero model":          {Provider: scripted, Transcript: transcript},
		"invalid system":      {Provider: scripted, Transcript: transcript, Model: model, SystemPrompt: string([]byte{0xff})},
		"blank tool":          {Provider: scripted, Transcript: transcript, Model: model, Tool: blankTool},
		"panicking tool name": {Provider: scripted, Transcript: transcript, Model: model, Tool: panickingNameTool{}},
		"negative settlement": {Provider: scripted, Transcript: transcript, Model: model, SettlementTimeout: -time.Second},
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
		Provider:   scripted,
		Transcript: transcript,
		Model:      model,
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

	t.Run("preflight cancellation starts no run", func(t *testing.T) {
		transcript := newSession(t)
		scripted := newScriptedProvider(t, mustTextTerminal(t, "unused"))
		runtime := newAgent(t, transcript, scripted, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := runtime.Run(ctx, "hello")
		requireErrorIs(t, err, agent.ErrInvalidRun)
		if len(transcript.Context().Messages()) != 0 || scripted.CallCount() != 0 || runtime.State().Phase() != agent.PhaseIdle {
			t.Fatalf("rejected run changed state: messages %d, calls %d, phase %s", len(transcript.Context().Messages()), scripted.CallCount(), runtime.State().Phase())
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
		if len(messages) != 2 || failureAt(t, messages, 1).ErrorMessage() != "Provider stream failed" {
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
			if !result.IsError() || onlyText(t, result.Content()) != "captured output\nexit 9" {
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
	transcript := newSession(t)
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
		Transcript:   transcript,
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

func TestToolCancellationWaitsAndCommitsClosedTranscript(t *testing.T) {
	transcript := newSession(t)
	started := make(chan struct{})
	cancelObserved := make(chan struct{})
	release := make(chan struct{})
	executor := &fakeTool{
		name: "bash",
		execute: func(ctx context.Context, _ []byte, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
			close(started)
			<-ctx.Done()
			close(cancelObserved)
			<-release
			// A normal late resolution must lose to the earlier cancellation.
			return agent.ToolOutput{Text: "normal late result"}, nil
		},
	}
	scripted := newScriptedProvider(
		t,
		mustToolUseTerminal(t, "call-cancel", "bash", []byte(`{"command":"block"}`)),
		mustTextTerminal(t, "must not be consumed"),
	)
	runtime := newAgent(t, transcript, scripted, executor)

	type runOutcome struct {
		result agent.Result
		err    error
	}
	runResult := make(chan runOutcome, 1)
	go func() {
		result, err := runtime.Run(context.Background(), "run")
		runResult <- runOutcome{result: result, err: err}
	}()
	waitClosed(t, started, "tool start")
	if _, err := runtime.Run(context.Background(), "busy"); !errors.Is(err, agent.ErrBusy) {
		t.Fatalf("concurrent Run error = %v, want ErrBusy", err)
	}

	abortOne := make(chan error, 1)
	abortTwo := make(chan error, 1)
	go func() { abortOne <- runtime.Abort(context.Background()) }()
	waitClosed(t, cancelObserved, "tool cancellation")
	go func() { abortTwo <- runtime.Abort(context.Background()) }()
	select {
	case err := <-abortOne:
		t.Fatalf("Abort returned before tool settled: %v", err)
	default:
	}
	select {
	case outcome := <-runResult:
		t.Fatalf("Run returned before tool settled: %#v", outcome)
	default:
	}
	close(release)

	var outcome runOutcome
	select {
	case outcome = <-runResult:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cancelled Run")
	}
	if outcome.err != nil {
		t.Fatalf("cancelled Run error = %v", outcome.err)
	}
	if err := <-abortOne; err != nil {
		t.Fatalf("first Abort error = %v", err)
	}
	if err := <-abortTwo; err != nil {
		t.Fatalf("second Abort error = %v", err)
	}
	terminal, ok := outcome.result.Terminal()
	if !ok || terminal.FinishReason() != llm.FinishAborted || outcome.result.ProviderTurns() != 1 || outcome.result.ToolExecutions() != 1 {
		t.Fatalf("outcome = terminal %T/%v, provider %d, tool %d", terminal, terminal.FinishReason(), outcome.result.ProviderTurns(), outcome.result.ToolExecutions())
	}
	if scripted.CallCount() != 1 || scripted.PendingResponses() != 1 {
		t.Fatalf("provider = calls %d, pending %d", scripted.CallCount(), scripted.PendingResponses())
	}

	messages := transcript.Context().Messages()
	if roles := messageRoles(messages); !reflect.DeepEqual(roles, []llm.Role{
		llm.RoleUser,
		llm.RoleAssistant,
		llm.RoleToolResult,
		llm.RoleAssistant,
	}) {
		t.Fatalf("cancel transcript roles = %v", roles)
	}
	toolResult := toolResultAt(t, messages, 2)
	if !toolResult.IsError() || onlyText(t, toolResult.Content()) != "Tool execution cancelled" || toolResult.ToolCallID() != "call-cancel" {
		t.Fatalf("cancel tool result = error %t, text %q, id %q", toolResult.IsError(), onlyText(t, toolResult.Content()), toolResult.ToolCallID())
	}
	failure := failureAt(t, messages, 3)
	if failure.FinishReason() != llm.FinishAborted || failure.ErrorMessage() != "Run cancelled during tool execution" || len(failure.Content()) != 0 || failure.Usage().TotalTokens() != 0 {
		t.Fatalf("cancel terminal = finish %s, text %q, content %d, usage %d", failure.FinishReason(), failure.ErrorMessage(), len(failure.Content()), failure.Usage().TotalTokens())
	}
}

func TestCancellationAfterNormalToolResultPreservesResultAndSkipsProviderTwo(t *testing.T) {
	transcript := newSession(t)
	executor := &fakeTool{
		name: "bash",
		execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
			return agent.ToolOutput{Text: "normal"}, nil
		},
	}
	scripted := newScriptedProvider(
		t,
		mustToolUseTerminal(t, "call-normal", "bash", []byte(`{"command":"ok"}`)),
		mustTextTerminal(t, "must not run"),
	)
	runtime := newAgent(t, transcript, scripted, executor)
	runContext, cancelRun := context.WithCancel(context.Background())
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
		if ended, ok := event.(agent.MessageEndEvent); ok && ended.Message.Role() == agentmsg.RoleToolResult {
			cancelRun()
		}
	})

	result, err := runtime.Run(runContext, "run")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	terminal, ok := result.Terminal()
	if !ok || terminal.FinishReason() != llm.FinishAborted {
		t.Fatalf("terminal = (%T, %t)", terminal, ok)
	}
	if scripted.CallCount() != 1 || result.ProviderTurns() != 1 {
		t.Fatalf("provider calls = %d/%d, want 1", scripted.CallCount(), result.ProviderTurns())
	}
	messages := transcript.Context().Messages()
	toolResult := toolResultAt(t, messages, 2)
	if toolResult.IsError() || onlyText(t, toolResult.Content()) != "normal" {
		t.Fatalf("normal result changed after cancellation: error %t, text %q", toolResult.IsError(), onlyText(t, toolResult.Content()))
	}
	if failureAt(t, messages, 3).ErrorMessage() != "Run cancelled during tool execution" {
		t.Fatalf("unexpected cancellation terminal")
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

func TestFinalTerminalAndAbortHaveOneAcceptanceBoundary(t *testing.T) {
	t.Run("abort before acceptance replaces provider success", func(t *testing.T) {
		transcript := newSession(t)
		providerImpl := newCloseBarrierProvider(t, "success")
		runtime := newAgent(t, transcript, providerImpl, nil)
		type outcome struct {
			result agent.Result
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			result, err := runtime.Run(context.Background(), "run")
			done <- outcome{result: result, err: err}
		}()
		waitClosed(t, providerImpl.closeEntered, "provider Close")
		abortDone := make(chan error, 1)
		go func() { abortDone <- runtime.Abort(context.Background()) }()
		var runContext context.Context
		select {
		case runContext = <-providerImpl.runContext:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for provider context")
		}
		waitClosed(t, runContext.Done(), "abort visibility")
		close(providerImpl.releaseClose)

		got := <-done
		if got.err != nil {
			t.Fatalf("Run() error = %v", got.err)
		}
		if err := <-abortDone; err != nil {
			t.Fatalf("Abort() error = %v", err)
		}
		terminal, ok := got.result.Terminal()
		if !ok || terminal.FinishReason() != llm.FinishAborted {
			t.Fatalf("terminal = (%T, %t), want aborted", terminal, ok)
		}
		messages := transcript.Context().Messages()
		if len(messages) != 2 || failureAt(t, messages, 1).FinishReason() != llm.FinishAborted {
			t.Fatalf("pre-acceptance transcript = %#v", messages)
		}
	})

	t.Run("abort after acceptance preserves provider success", func(t *testing.T) {
		base := newSession(t)
		transcript := &blockingFinalTranscript{
			base:    base,
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
		scripted := newScriptedProvider(t, mustTextTerminal(t, "success"))
		runtime := newAgent(t, transcript, scripted, nil)
		runContext := make(chan context.Context, 1)
		runtime.Subscribe(func(ctx context.Context, event agent.AgentEvent) {
			if _, ok := event.(agent.AgentStartEvent); ok {
				runContext <- ctx
			}
		})
		type outcome struct {
			result agent.Result
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			result, err := runtime.Run(context.Background(), "run")
			done <- outcome{result: result, err: err}
		}()
		waitClosed(t, transcript.entered, "final append")
		activeContext := <-runContext
		abortDone := make(chan error, 1)
		go func() { abortDone <- runtime.Abort(context.Background()) }()
		waitClosed(t, activeContext.Done(), "post-acceptance abort")
		close(transcript.release)

		got := <-done
		if got.err != nil || !got.result.Succeeded() {
			t.Fatalf("Run() = success %t, error %v", got.result.Succeeded(), got.err)
		}
		if err := <-abortDone; err != nil {
			t.Fatalf("Abort() error = %v", err)
		}
		messages := base.Context().Messages()
		if len(messages) != 2 {
			t.Fatalf("post-acceptance message count = %d, want 2", len(messages))
		}
		if final, ok := messages[1].(llm.AssistantTextMessage); !ok || onlyText(t, final.Content()) != "success" {
			t.Fatalf("post-acceptance final = %T", messages[1])
		}
	})
}

func TestTranscriptFailureIsFatalAndPreventsToolSideEffect(t *testing.T) {
	base := newSession(t)
	transcript := &failingTranscript{base: base}
	executor := &fakeTool{name: "bash"}
	scripted := newScriptedProvider(
		t,
		mustToolUseTerminal(t, "call-no-side-effect", "bash", []byte(`{"command":"danger"}`)),
	)
	runtime := newAgent(t, transcript, scripted, executor)
	settled := make(chan struct{})
	runtime.Subscribe(func(_ context.Context, event agent.AgentEvent) {
		if ended, ok := event.(agent.AgentEndEvent); ok {
			if !errors.Is(ended.Err, agent.ErrTranscriptCommit) || ended.Terminal != nil {
				t.Errorf("fatal settlement = terminal %T, error %v", ended.Terminal, ended.Err)
			}
			close(settled)
		}
	})

	result, err := runtime.Run(context.Background(), "run")
	requireErrorIs(t, err, agent.ErrTranscriptCommit)
	if _, ok := result.Terminal(); ok {
		t.Fatal("fatal transcript error reported a durable terminal")
	}
	if executor.CallCount() != 0 || result.ToolExecutions() != 0 {
		t.Fatalf("tool side effect ran after failed assistant commit: %d/%d", executor.CallCount(), result.ToolExecutions())
	}
	if roles := messageRoles(base.Context().Messages()); !reflect.DeepEqual(roles, []llm.Role{llm.RoleUser}) {
		t.Fatalf("durable roles after failed assistant commit = %v", roles)
	}
	waitClosed(t, settled, "fatal run-settled event")
	if runtime.State().Phase() != agent.PhaseIdle {
		t.Fatalf("phase after fatal run = %s", runtime.State().Phase())
	}
}

func TestTranscriptCommitFailuresStopCausalSuccessors(t *testing.T) {
	t.Run("tool result failure prevents provider two", func(t *testing.T) {
		base := newSession(t)
		transcript := &selectiveFailingTranscript{
			base: base,
			fail: func(message llm.ConversationMessage) bool {
				_, ok := message.(llm.ToolResultMessage)
				return ok
			},
		}
		executor := &fakeTool{
			name: "bash",
			execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
				return agent.ToolOutput{Text: "ok"}, nil
			},
		}
		scripted := newScriptedProvider(
			t,
			mustToolUseTerminal(t, "call-result-fails", "bash", []byte(`{"command":"ok"}`)),
			mustTextTerminal(t, "must not run"),
		)
		runtime := newAgent(t, transcript, scripted, executor)

		result, err := runtime.Run(context.Background(), "run")
		requireErrorIs(t, err, agent.ErrTranscriptCommit)
		if _, ok := result.Terminal(); ok {
			t.Fatal("failed ToolResult commit reported a durable terminal")
		}
		if scripted.CallCount() != 1 || result.ProviderTurns() != 1 || executor.CallCount() != 1 {
			t.Fatalf("counts = provider %d/%d, tool %d", scripted.CallCount(), result.ProviderTurns(), executor.CallCount())
		}
		if roles := messageRoles(base.Context().Messages()); !reflect.DeepEqual(roles, []llm.Role{llm.RoleUser, llm.RoleAssistant}) {
			t.Fatalf("durable roles after ToolResult failure = %v", roles)
		}
	})

	t.Run("final assistant failure is fatal", func(t *testing.T) {
		base := newSession(t)
		transcript := &selectiveFailingTranscript{
			base: base,
			fail: func(message llm.ConversationMessage) bool {
				_, ok := message.(llm.AssistantTextMessage)
				return ok
			},
		}
		scripted := newScriptedProvider(t, mustTextTerminal(t, "not durable"))
		runtime := newAgent(t, transcript, scripted, nil)

		result, err := runtime.Run(context.Background(), "run")
		requireErrorIs(t, err, agent.ErrTranscriptCommit)
		if _, ok := result.Terminal(); ok {
			t.Fatal("failed final assistant commit reported a durable terminal")
		}
		if scripted.CallCount() != 1 || result.ProviderTurns() != 1 {
			t.Fatalf("provider counts = %d/%d", scripted.CallCount(), result.ProviderTurns())
		}
		if roles := messageRoles(base.Context().Messages()); !reflect.DeepEqual(roles, []llm.Role{llm.RoleUser}) {
			t.Fatalf("durable roles after final failure = %v", roles)
		}
	})
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

type failingTranscript struct {
	base *session.Session
	once sync.Once
}

func (t *failingTranscript) Context() session.Context { return t.base.Context() }

func (t *failingTranscript) Append(
	ctx context.Context,
	message llm.ConversationMessage,
	options session.AppendOptions,
) (session.Entry, error) {
	if _, ok := message.(llm.AssistantToolUseMessage); ok {
		failed := false
		t.once.Do(func() { failed = true })
		if failed {
			return session.Entry{}, errors.New("injected append failure")
		}
	}
	return t.base.Append(ctx, message, options)
}

var _ agent.Transcript = (*failingTranscript)(nil)
var _ provider.Provider = (*brokenProvider)(nil)
var _ provider.EventStream = (*brokenStream)(nil)

type selectiveFailingTranscript struct {
	base *session.Session
	fail func(llm.ConversationMessage) bool
	once sync.Once
}

func (t *selectiveFailingTranscript) Context() session.Context { return t.base.Context() }

func (t *selectiveFailingTranscript) Append(
	ctx context.Context,
	message llm.ConversationMessage,
	options session.AppendOptions,
) (session.Entry, error) {
	shouldFail := false
	if t.fail != nil && t.fail(message) {
		t.once.Do(func() { shouldFail = true })
	}
	if shouldFail {
		return session.Entry{}, errors.New("injected selective append failure")
	}
	return t.base.Append(ctx, message, options)
}

var _ agent.Transcript = (*selectiveFailingTranscript)(nil)

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

type closeBarrierProvider struct {
	stream       *closeBarrierStream
	runContext   chan context.Context
	closeEntered chan struct{}
	releaseClose chan struct{}
}

func newCloseBarrierProvider(t *testing.T, text string) *closeBarrierProvider {
	t.Helper()
	textStart, err := llm.NewTextStartEvent(0)
	if err != nil {
		t.Fatal(err)
	}
	textDelta, err := llm.NewTextDeltaEvent(0, text)
	if err != nil {
		t.Fatal(err)
	}
	textEnd, err := llm.NewTextEndEvent(0, text)
	if err != nil {
		t.Fatal(err)
	}
	done, err := llm.NewDoneEvent(llm.FinishStop, mustUsage(t, 1, 1), agentTestEpoch, testAssistantProvenance())
	if err != nil {
		t.Fatal(err)
	}
	closeEntered := make(chan struct{})
	releaseClose := make(chan struct{})
	return &closeBarrierProvider{
		stream: &closeBarrierStream{
			events:       []llm.StreamEvent{newStartEvent(t), textStart, textDelta, textEnd, done},
			closeEntered: closeEntered,
			releaseClose: releaseClose,
		},
		runContext:   make(chan context.Context, 1),
		closeEntered: closeEntered,
		releaseClose: releaseClose,
	}
}

func (p *closeBarrierProvider) Stream(ctx context.Context, _ provider.Request) provider.EventStream {
	p.runContext <- ctx
	return p.stream
}

type closeBarrierStream struct {
	events       []llm.StreamEvent
	next         int
	closeEntered chan struct{}
	releaseClose chan struct{}
}

func (s *closeBarrierStream) Next() (llm.StreamEvent, error) {
	if s.next >= len(s.events) {
		return nil, io.EOF
	}
	event := s.events[s.next]
	s.next++
	return event, nil
}

func (s *closeBarrierStream) Close() error {
	close(s.closeEntered)
	<-s.releaseClose
	return nil
}

type blockingFinalTranscript struct {
	base    *session.Session
	entered chan struct{}
	release chan struct{}
}

func (t *blockingFinalTranscript) Context() session.Context { return t.base.Context() }

func (t *blockingFinalTranscript) Append(
	ctx context.Context,
	message llm.ConversationMessage,
	options session.AppendOptions,
) (session.Entry, error) {
	if _, ok := message.(llm.AssistantTextMessage); ok {
		close(t.entered)
		<-t.release
	}
	return t.base.Append(ctx, message, options)
}

var _ provider.Provider = (*closeBarrierProvider)(nil)
var _ provider.EventStream = (*closeBarrierStream)(nil)
var _ agent.Transcript = (*blockingFinalTranscript)(nil)

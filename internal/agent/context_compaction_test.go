package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

func manualContextFixture(t *testing.T, configure func(*agent.SessionConfig), responses ...provider.ScriptStep) (*agent.AgentSession, *provider.ScriptedProvider, *session.SessionManager) {
	t.Helper()
	manager := newSessionManager(t)
	_, _ = manager.AppendLLMMessage(context.Background(), mustTextTerminal(t, "existing task progress"))
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	_ = implementation.SetResponses(responses)
	definition, _ := provider.NewToolDefinition("work", "existing tool", false, []byte(`{"type":"object"}`))
	var runtime *agent.AgentSession
	maximum := uint64(9000)
	config := agent.SessionConfig{Provider: implementation, SessionManager: manager, Model: handoffModel(t),
		SystemPrompt: "stable task system", Tools: []provider.ToolDefinition{definition}, Stream: provider.StreamOptions{SessionID: "same-session", MaxTokens: &maximum},
		Retry: agent.RetryPolicy{MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }},
		Tool: &fakeTool{name: "work", executeWithID: func(ctx context.Context, id string, _ []byte, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
			if id == "submit" {
				value, err := runtime.RequestContextChange(ctx, agent.ContextChangeRequest{ID: "cli-submit", Mode: "handoff", Handoff: "CLI handoff: check module B next"})
				if err == nil && value.State != "accepted" {
					t.Errorf("submission = %#v", value)
				}
				return agent.ToolOutput{Text: "CLI accepted"}, err
			}
			return agent.ToolOutput{Text: "ordinary work receipt"}, nil
		}},
	}
	if configure != nil {
		configure(&config)
	}
	runtime, err = agent.NewSession(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ConfigureContextAccess(handoffInstructions(), nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	return runtime, implementation, manager
}

func fixedContextStep(t *testing.T, message llm.AssistantTerminal) provider.ScriptStep {
	t.Helper()
	step, err := provider.FixedResponseStep(message)
	if err != nil {
		t.Fatal(err)
	}
	return step
}

func assertNormalHandoffRequest(t *testing.T, request provider.Request) {
	t.Helper()
	if choice, ok := request.ToolChoice(); ok && choice.Mode == "none" {
		t.Error("handoff disabled tools")
	}
	if maximum := request.StreamOptions().MaxTokens; maximum == nil || *maximum != 9000 {
		t.Error("handoff replaced the ordinary output limit")
	}
	if len(request.Tools()) != 1 || request.SystemPrompt() != "stable task system" || request.StreamOptions().SessionID != "same-session" {
		t.Error("handoff changed request configuration")
	}
}

func TestManualHandoffUsesOrdinaryToolsAndRetryUntilCLISubmission(t *testing.T) {
	step, _ := provider.FactoryResponseStep(func(_ context.Context, request provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		assertNormalHandoffRequest(t, request)
		text := contextRequestText(t, request)
		if !strings.Contains(text, "existing task progress") || !strings.Contains(text, "submit-handoff --stdin") || !strings.Contains(text, "focus on pending checks") {
			t.Fatal(text)
		}
		return mustToolUseTerminal(t, "inspect", "work", []byte(`{}`)), nil
	})
	runtime, implementation, manager := manualContextFixture(t, nil, step,
		fixedContextStep(t, sessionHTTPFailure(t, 503)), fixedContextStep(t, mustToolUseTerminal(t, "submit", "work", []byte(`{}`))))
	starts, ends, retries, settled := 0, 0, 0, 0
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch e := event.(type) {
		case agent.CompactionStartEvent:
			starts++
		case agent.CompactionEndEvent:
			ends++
			if e.Result == nil || !e.Result.Committed {
				t.Error("no committed handoff")
			}
		case agent.AutoRetryStartEvent:
			retries++
		case agent.AgentSettledEvent:
			settled++
		}
	})
	result, err := runtime.Compact(context.Background(), "focus on pending checks")
	if err != nil || !result.Committed {
		t.Fatalf("compact: %#v %v", result, err)
	}
	if implementation.CallCount() != 3 || starts != 1 || ends != 1 || retries != 1 || settled != 1 {
		t.Fatalf("calls/events: %d %d %d %d %d", implementation.CallCount(), starts, ends, retries, settled)
	}
	for _, request := range implementation.Requests() {
		assertNormalHandoffRequest(t, request)
	}
	entries := manager.Entries()
	last := entries[len(entries)-1]
	if handoff, ok := last.ContextHandoff(); !ok || handoff.RequestID != "cli-submit" || handoff.Text != "CLI handoff: check module B next" {
		t.Fatal("checkpoint not written by CLI")
	}
	if role, _ := entries[len(entries)-2].MessageRole(); role != "toolResult" {
		t.Fatal("handoff overtook tool receipt")
	}
	if len(runtime.State().Active.Messages()) != 1 || runtime.Activity().IsStreaming || runtime.Activity().IsCompacting {
		t.Fatal("manual operation did not settle at the handoff")
	}
}

func TestActiveManualHandoffQueuesReminderAndContinuesAfterSubmission(t *testing.T) {
	var runtime *agent.AgentSession
	request := agent.ContextChangeRequest{ID: "manual-request", Mode: "compact", Instructions: "preserve the latest correction"}
	first, _ := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		for range 2 {
			value, err := runtime.RequestContextChange(ctx, request)
			if err != nil || value.State != "accepted" {
				t.Errorf("queue: %#v %v", value, err)
			}
		}
		return mustTextTerminal(t, "work completed so far"), nil
	})
	submit, _ := provider.FactoryResponseStep(func(_ context.Context, request provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		assertNormalHandoffRequest(t, request)
		if text := contextRequestText(t, request); strings.Count(text, "preserve the latest correction") != 1 {
			t.Fatal(text)
		}
		if err := runtime.Steer("new correction during CLI handoff"); err != nil {
			t.Fatal(err)
		}
		return mustToolUseTerminal(t, "submit", "work", []byte(`{}`)), nil
	})
	last, _ := provider.FactoryResponseStep(func(_ context.Context, request provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		text := contextRequestText(t, request)
		if !strings.Contains(text, "new correction during CLI handoff") || !strings.Contains(text, "search-original-history") || strings.Contains(text, "existing task progress") {
			t.Fatal(text)
		}
		return mustTextTerminal(t, "done"), nil
	})
	var implementation *provider.ScriptedProvider
	runtime, implementation, _ = manualContextFixture(t, nil, first, submit, last)
	if result, err := runtime.Run(context.Background(), "current task"); err != nil || !result.Succeeded() {
		t.Fatalf("run: %#v %v", result, err)
	}
	status, err := runtime.RequestContextChange(context.Background(), request)
	if err != nil || status.State != "completed" || status.RequestID != request.ID || implementation.CallCount() != 3 {
		t.Fatalf("replay: %#v %v", status, err)
	}
}

func TestManualHandoffDoesNotCommitPlainTextOrProviderFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain text", true: "provider failure"}[failure], func(t *testing.T) {
			var message llm.AssistantTerminal = mustTextTerminal(t, "this is not a CLI submission")
			if failure {
				message = sessionHTTPFailure(t, 400)
			}
			runtime, implementation, manager := manualContextFixture(t, nil, fixedContextStep(t, message))
			if result, err := runtime.Compact(context.Background(), ""); err == nil || result.Committed {
				t.Fatalf("unexpected checkpoint: %#v %v", result, err)
			}
			window, _ := manager.CurrentContextWindow()
			status, _ := runtime.ContextStatus()
			if window != session.InitialContextWindow || implementation.CallCount() != 1 || status.Change == nil || status.Change.State != "failed" {
				t.Fatalf("status: %#v", status)
			}
		})
	}
}

func TestManualHandoffAbortCancelsNormalProviderAndKeepsWindow(t *testing.T) {
	entered := make(chan struct{})
	step, _ := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		close(entered)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	})
	runtime, _, manager := manualContextFixture(t, nil, step)
	done := make(chan error, 1)
	go func() { _, err := runtime.Compact(context.Background(), ""); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("manual Agent request did not start")
	}
	runtime.AbortCompaction()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("abort did not settle")
	}
	status, _ := runtime.ContextStatus()
	if window, _ := manager.CurrentContextWindow(); window != session.InitialContextWindow || status.Change == nil || status.Change.State != "cancelled" || runtime.Activity().IsCompacting {
		t.Fatalf("aborted status: %#v", status)
	}
}

func TestCLIHandoffHooksRunOnlyAtSubmissionAndPreserveCommitRules(t *testing.T) {
	for _, mode := range []string{"default", "override", "cancel", "hook error", "conflict"} {
		t.Run(mode, func(t *testing.T) {
			var manager *session.SessionManager
			before, after := 0, 0
			runtime, _, transcript := manualContextFixture(t, func(config *agent.SessionConfig) {
				config.CompactionEnabled = new(bool) // manual requests ignore the automatic-reminder gate
				config.Hooks.SessionBeforeCompact = func(ctx context.Context, event agent.SessionBeforeCompactEvent) (agent.SessionBeforeCompactResult, error) {
					before++
					if event.CustomInstructions == nil || *event.CustomInstructions != "focus" {
						t.Error("lost manual instructions")
					}
					result := agent.SessionBeforeCompactResult{}
					switch mode {
					case "override":
						result.Compaction = &agent.ExtensionCompactionResult{Summary: "extension handoff", TokensBefore: 17}
					case "cancel":
						result.Cancel = agent.HookCancel{Cancel: boolPointer(true)}
					case "hook error":
						return result, errors.New("extension failed")
					case "conflict":
						_, _ = manager.AppendLLMMessage(ctx, mustTextTerminal(t, "concurrent branch edit"))
					}
					return result, nil
				}
				config.Hooks.SessionCompact = func(context.Context, agent.SessionCompactEvent) error { after++; return nil }
			}, fixedContextStep(t, mustToolUseTerminal(t, "submit", "work", []byte(`{}`))))
			manager = transcript
			result, err := runtime.Compact(context.Background(), "focus")
			failed := mode == "cancel" || mode == "conflict"
			if mode == "cancel" && !errors.Is(err, agent.ErrAgentAborted) {
				t.Fatalf("extension cancellation lost its public error: %v", err)
			}
			if (err != nil) != failed || result.Committed == failed || before != 1 || after != map[bool]int{true: 0, false: 1}[failed] {
				t.Fatalf("result committed=%v err=%v hooks=%d/%d", result.Committed, err, before, after)
			}
			if mode == "override" && (result.Output.Text != "extension handoff" || !result.Output.FromExtension) {
				t.Fatal("override lost")
			}
			if failed {
				if window, _ := manager.CurrentContextWindow(); window != session.InitialContextWindow {
					t.Fatal("failed handoff changed window")
				}
			}
		})
	}
}

func boolPointer(value bool) *bool { return &value }

func TestCancelledQueuedHandoffDoesNotLeakIntoNextPrompt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var runtime *agent.AgentSession
	first, _ := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		if _, err := runtime.RequestContextChange(ctx, agent.ContextChangeRequest{Mode: "compact", Instructions: "cancelled handoff focus"}); err != nil {
			t.Error(err)
		}
		if err := runtime.Steer("user correction survives cancellation"); err != nil {
			t.Error(err)
		}
		cancel()
		return nil, context.Canceled
	})
	last, _ := provider.FactoryResponseStep(func(_ context.Context, request provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		text := contextRequestText(t, request)
		if strings.Contains(text, "cancelled handoff focus") || !strings.Contains(text, "user correction survives cancellation") {
			t.Fatal(text)
		}
		return mustTextTerminal(t, "next result"), nil
	})
	runtime, _, _ = manualContextFixture(t, nil, first, last)
	_, _ = runtime.Run(ctx, "cancel this run")
	if result, err := runtime.Run(context.Background(), "next prompt"); err != nil || !result.Succeeded() {
		t.Fatalf("next run: %#v %v", result, err)
	}
}

func TestBudgetReminderEscalatesOnceAndKeepsNormalTools(t *testing.T) {
	var runtime *agent.AgentSession
	makeStep := func(id string, tokens uint64, soft, urgent int) provider.ScriptStep {
		step, _ := provider.FactoryResponseStep(func(_ context.Context, request provider.Request, _ uint64) (llm.AssistantTerminal, error) {
			assertNormalHandoffRequest(t, request)
			text := contextRequestText(t, request)
			if strings.Count(text, "tokens remaining for work") != soft || strings.Count(text, "working budget") != urgent {
				t.Fatal(text)
			}
			if id == "final" {
				return mustTextTerminal(t, "done without unnecessary handoff"), nil
			}
			call, _ := llm.NewToolCallBlock(id, "work", []byte(`{}`))
			return newAssistantToolUseMessage([]llm.AssistantBlock{call}, mustUsage(t, tokens, 1), agentTestEpoch)
		})
		return step
	}
	runtime, _, manager := manualContextFixture(t, nil, makeStep("soft", 95000, 0, 0), makeStep("urgent", 113000, 1, 0), makeStep("lower-usage", 95000, 1, 1), makeStep("final", 0, 1, 1))
	if result, err := runtime.Run(context.Background(), "task"); err != nil || !result.Succeeded() {
		t.Fatalf("run: %#v %v", result, err)
	}
	if window, _ := manager.CurrentContextWindow(); window != session.InitialContextWindow {
		t.Fatal("reminder alone switched window")
	}
}

func TestManualHandoffAbortInterruptsOrdinaryRetryWait(t *testing.T) {
	waiting := make(chan struct{})
	runtime, _, manager := manualContextFixture(t, func(config *agent.SessionConfig) {
		config.Retry = agent.RetryPolicy{MaxAttempts: 3, Sleep: func(ctx context.Context, _ time.Duration) error {
			close(waiting)
			<-ctx.Done()
			return context.Cause(ctx)
		}}
	}, fixedContextStep(t, sessionHTTPFailure(t, 503)))
	done := make(chan error, 1)
	go func() { _, err := runtime.Compact(context.Background(), ""); done <- err }()
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not start")
	}
	runtime.AbortCompaction()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("retry cancellation succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retry cancellation blocked")
	}
	if window, _ := manager.CurrentContextWindow(); window != session.InitialContextWindow {
		t.Fatal("retry cancellation changed window")
	}
}

func TestHandoffExtensionOverrideReplaysOriginalSubmissionAfterRestart(t *testing.T) {
	runtime, implementation, manager := manualContextFixture(t, func(config *agent.SessionConfig) {
		config.Hooks.SessionBeforeCompact = func(context.Context, agent.SessionBeforeCompactEvent) (agent.SessionBeforeCompactResult, error) {
			return agent.SessionBeforeCompactResult{Compaction: &agent.ExtensionCompactionResult{Summary: "extension checkpoint"}}, nil
		}
	})
	request := agent.ContextChangeRequest{ID: "stable-submission", Mode: "handoff", Handoff: "original CLI handoff"}
	first, err := runtime.RequestContextChange(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	count := len(manager.Entries())
	replay, err := runtime.RequestContextChange(context.Background(), request)
	if err != nil || replay.WindowID != first.WindowID || len(manager.Entries()) != count || implementation.CallCount() != 0 {
		t.Fatalf("replay: %#v %v", replay, err)
	}
	path, _ := manager.SessionFile()
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := session.OpenSessionManager(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	restored, err := agent.NewSession(agent.SessionConfig{Provider: implementation, SessionManager: reopened, Model: handoffModel(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close(context.Background())
	replay, err = restored.RequestContextChange(context.Background(), request)
	if err != nil || replay.WindowID != first.WindowID || len(reopened.Entries()) != count {
		t.Fatalf("restart replay: %#v %v", replay, err)
	}
	request.Handoff = "different CLI content"
	if _, err := restored.RequestContextChange(context.Background(), request); err == nil {
		t.Fatal("conflicting payload accepted")
	}
}

func TestHandoffCannotConsumeAnUndeliveredManualRequest(t *testing.T) {
	var runtime *agent.AgentSession
	first := fixedContextStep(t, mustToolUseTerminal(t, "early", "work", []byte(`{}`)))
	second, _ := provider.FactoryResponseStep(func(_ context.Context, request provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		if text := contextRequestText(t, request); !strings.Contains(text, "late manual focus") {
			t.Fatal("handoff discarded an undelivered request: ", text)
		}
		return mustToolUseTerminal(t, "submit", "work", []byte(`{}`)), nil
	})
	runtime, _, _ = manualContextFixture(t, func(config *agent.SessionConfig) {
		config.Tool = &fakeTool{name: "work", executeWithID: func(ctx context.Context, id string, _ []byte, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
			if id == "early" {
				_, err := runtime.RequestContextChange(ctx, agent.ContextChangeRequest{Mode: "compact", Instructions: "late manual focus"})
				if err != nil {
					return agent.ToolOutput{}, err
				}
			}
			_, err := runtime.RequestContextChange(ctx, agent.ContextChangeRequest{ID: id, Mode: "handoff", Handoff: "handoff " + id})
			return agent.ToolOutput{Text: "accepted"}, err
		}}
	}, first, second, fixedContextStep(t, mustTextTerminal(t, "done")))
	if result, err := runtime.Run(context.Background(), "current task"); err != nil || !result.Succeeded() {
		t.Fatalf("run: %#v %v", result, err)
	}
}

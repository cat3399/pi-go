package agent_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

func TestHandoffRunsAfterParallelToolReceiptsBeforeQueuedInputs(t *testing.T) {
	ctx := context.Background()
	manager := newSessionManager(t)
	changeCall, _ := llm.NewToolCallBlock("change", "work", []byte(`{}`))
	slowCall, _ := llm.NewToolCallBlock("slow", "work", []byte(`{}`))
	terminal, err := newAssistantToolUseMessage([]llm.AssistantBlock{changeCall, slowCall}, mustUsage(t, 100000, 30), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, terminal, mustTextTerminal(t, "restored answer"), mustTextTerminal(t, "follow-up answer"))
	model, err := newAgentModel(provider.ModelSpec{Provider: "scripted", API: "scripted", ID: "scripted-1", ContextWindow: 128000, MaxTokens: 4000})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := provider.NewToolDefinition("work", "work", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	requested := make(chan struct{})
	var summaries atomic.Int32
	var runtime *agent.AgentSession
	executor := &fakeTool{name: "work", executeWithID: func(ctx context.Context, id string, _ []byte, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		if id == "change" {
			value, err := runtime.RequestContextChange(ctx, agent.ContextChangeRequest{ID: "handoff-one", Mode: "handoff", Handoff: "short handoff: proceed with module B"})
			if err != nil || value.State != "accepted" {
				t.Errorf("accept = %#v, %v", value, err)
			}
			if duplicate, err := runtime.RequestContextChange(ctx, agent.ContextChangeRequest{ID: "handoff-one", Mode: "handoff", Handoff: "short handoff: proceed with module B"}); err != nil || duplicate.RequestID != value.RequestID {
				t.Error("pending retry", err)
			}
			if err := runtime.Steer("new correction while switching"); err != nil {
				t.Error(err)
			}
			if err := runtime.FollowUp("queued follow-up"); err != nil {
				t.Error(err)
			}
			if err := runtime.SetSystemPrompt("updated system configuration"); err != nil {
				t.Error(err)
			}
			close(requested)
			return agent.ToolOutput{Text: "request accepted"}, nil
		}
		<-requested
		for _, entry := range manager.Entries() {
			if _, ok := entry.ContextHandoff(); ok {
				t.Error("checkpoint committed before parallel tool settled")
			}
		}
		return agent.ToolOutput{Text: "last parallel receipt"}, nil
	}}
	runtime, err = agent.NewSession(agent.SessionConfig{Provider: scripted, SessionManager: manager, Model: model, Tool: executor, Tools: []provider.ToolDefinition{definition}, ToolExecution: agent.ToolExecutionParallel, SystemPrompt: "original system configuration", Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
		summaries.Add(1)
		return session.SummaryOutput{Text: "unexpected summary"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(ctx)
	result, err := runtime.Run(ctx, "old task body that must not be retained")
	if err != nil || !result.Succeeded() {
		t.Fatalf("run = %#v %v", result, err)
	}
	requests := scripted.Requests()
	if len(requests) != 3 {
		t.Fatalf("requests = %d", len(requests))
	}
	if requests[1].SystemPrompt() != "updated system configuration" {
		t.Fatal("handoff pinned stale runtime configuration")
	}
	second := contextRequestText(t, requests[1])
	if strings.Contains(second, "old task body") || strings.Contains(second, "last parallel receipt") || !strings.Contains(second, "short handoff") || !strings.Contains(second, "new correction while switching") {
		t.Fatalf("next context %s", second)
	}
	if !strings.Contains(contextRequestText(t, requests[2]), "queued follow-up") {
		t.Fatal("follow-up queue lost")
	}
	if summaries.Load() != 0 {
		t.Fatal("handoff used a summarizer or stale usage triggered compaction")
	}
	boundary, toolResults, steering := -1, 0, -1
	for i, entry := range manager.Entries() {
		if _, ok := entry.ContextHandoff(); ok {
			boundary = i
		}
		if role, _ := entry.MessageRole(); role == "toolResult" {
			toolResults++
			if boundary >= 0 {
				t.Error("tool receipt after boundary")
			}
		}
		if strings.Contains(string(entry.RawJSON()), "new correction while switching") {
			steering = i
		}
	}
	if boundary < 0 || toolResults != 2 || steering < boundary {
		t.Fatalf("boundary=%d receipts=%d steering=%d", boundary, toolResults, steering)
	}
	view, err := manager.ReadHistoryView(session.HistoryViewQuery{View: "raw", Search: "last parallel receipt"})
	if err != nil || len(view.Records) != 1 {
		t.Fatal("original tool receipt unavailable", err)
	}
	status, err := runtime.ContextStatus()
	if err != nil || status.Change == nil || status.Change.State != "completed" || status.EstimatedTokens > 10000 {
		t.Fatalf("status %#v %v", status, err)
	}
	count := len(manager.Entries())
	if _, err := runtime.RequestContextChange(ctx, agent.ContextChangeRequest{ID: "handoff-one", Mode: "handoff", Handoff: "short handoff: proceed with module B"}); err != nil || len(manager.Entries()) != count {
		t.Fatal("completed retry duplicated checkpoint", err)
	}
}

func contextRequestText(t *testing.T, request provider.Request) string {
	t.Helper()
	return session.SerializeConversation(request.Messages())
}

func TestHandoffCancelledRunDoesNotSwitchNextPrompt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := newSessionManager(t)
	scripted := newScriptedProvider(t, mustToolUseTerminal(t, "cancel", "work", []byte(`{}`)), mustTextTerminal(t, "next task"))
	definition, _ := provider.NewToolDefinition("work", "work", false, []byte(`{"type":"object"}`))
	var runtime *agent.AgentSession
	executor := &fakeTool{name: "work", execute: func(ctx context.Context, _ []byte, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		if _, err := runtime.RequestContextChange(ctx, agent.ContextChangeRequest{ID: "cancelled", Mode: "handoff", Handoff: "must not leak"}); err != nil {
			t.Error(err)
		}
		cancel()
		return agent.ToolOutput{Text: "cancelled"}, context.Canceled
	}}
	var err error
	runtime, err = agent.NewSession(agent.SessionConfig{Provider: scripted, SessionManager: manager, Model: sessionTestModel(t), Tool: executor, Tools: []provider.ToolDefinition{definition}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	_, _ = runtime.Run(ctx, "cancel this run")
	status, err := runtime.ContextStatus()
	if err != nil || status.Change == nil || status.Change.State != "cancelled" || status.WindowID != session.InitialContextWindow {
		t.Fatalf("status %#v %v", status, err)
	}
	for _, entry := range manager.Entries() {
		if _, ok := entry.ContextHandoff(); ok {
			t.Fatal("aborted tool batch changed context")
		}
	}
	_, _ = runtime.Run(context.Background(), "next task")
	for _, entry := range manager.Entries() {
		if _, ok := entry.ContextHandoff(); ok {
			t.Fatal("old request leaked into next task")
		}
	}
}

func TestContextReminderKeepsStablePrefixAndDoesNotRepeat(t *testing.T) {
	manager := newSessionManager(t)
	old, err := llm.NewUserTextMessage(strings.Repeat("old text ", 440), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	definition, _ := provider.NewToolDefinition("work", "work", false, []byte(`{"type":"object"}`))
	model, err := newAgentModel(provider.ModelSpec{Provider: "scripted", API: "scripted", ID: "scripted-1", ContextWindow: 1200, MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, mustToolUseTerminal(t, "one", "work", []byte(`{}`)), mustToolUseTerminal(t, "two", "work", []byte(`{}`)), mustTextTerminal(t, "finished"))
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: scripted, SessionManager: manager, Model: model, Tool: &fakeTool{name: "work", execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		return agent.ToolOutput{Text: "done"}, nil
	}}, Tools: []provider.ToolDefinition{definition}, ContextReserve: 100, SystemPrompt: "stable system"})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	if err := runtime.ConfigureContextAccess(agent.ContextAccessInstructions{Handoff: "session CLI entry", History: "history entry"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "continue"); err != nil {
		t.Fatal(err)
	}
	for _, request := range scripted.Requests() {
		text := contextRequestText(t, request)
		if strings.Count(text, "tokens remaining for work") != 1 {
			t.Fatalf("notice duplicated or missing: %s", text)
		}
		if request.SystemPrompt() != "stable system" {
			t.Fatal("system prefix changed")
		}
	}
	view, err := manager.ReadHistoryView(session.HistoryViewQuery{Search: "tokens remaining for work"})
	if err != nil || len(view.Records) != 0 {
		t.Fatal("notice became user dialogue", err)
	}
}

func contextThreshold(value uint64) *uint64 { return &value }

func TestContextThresholdsPreferExplicitTokensAndKeepLegacyFallback(t *testing.T) {
	for _, test := range []struct {
		name                                 string
		reserve                              uint64
		handoff, reminder                    *uint64
		wantBudget, wantReminder, wantMargin uint64
	}{
		{"legacy", 16384, nil, nil, 111616, 89292, 16384},
		{"explicit legacy zero", 0, nil, nil, 128000, 102400, 0},
		{"legacy oversized reserve", 200000, nil, nil, 96000, 76800, 32000},
		{"explicit handoff", 16384, contextThreshold(50000), nil, 50000, 40000, 78000},
		{"explicit thresholds", 200000, contextThreshold(50000), contextThreshold(45000), 50000, 45000, 78000},
		{"explicit reminder with legacy budget", 16384, nil, contextThreshold(60000), 111616, 60000, 16384},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, err := agent.NewSession(agent.SessionConfig{Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: handoffModel(t), ContextReserve: test.reserve, ContextReserveSet: true, HandoffAtTokens: test.handoff, ReminderAtTokens: test.reminder})
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close(context.Background())
			status, err := runtime.ContextStatus()
			if err != nil || status.RequestBudget != test.wantBudget || status.ReminderTokens != test.wantReminder || status.ReserveTokens != test.wantMargin {
				t.Fatalf("status = %#v, %v", status, err)
			}
		})
	}
}

func TestContextThresholdReloadUsesNewValuesAndClearsRemovedOverrides(t *testing.T) {
	settings := agent.RuntimeControlSettings{SteeringMode: agent.QueueOneAtATime, FollowUpMode: agent.QueueOneAtATime, AutoCompactionEnabled: true, AutoRetryEnabled: true, Retry: agent.RetryPolicy{MaxAttempts: 1}, CompactionReserveTokens: 16384, CompactionReserveSet: true}
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: handoffModel(t), HandoffAtTokens: contextThreshold(50000), ReminderAtTokens: contextThreshold(40000), ResolveRuntimeSettings: func() agent.RuntimeControlSettings { return settings }})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	settings.CompactionHandoffAtTokens, settings.CompactionReminderAtTokens = contextThreshold(60000), contextThreshold(55000)
	if err := runtime.Reload(context.Background(), agent.ReloadOptions{}); err != nil {
		t.Fatal(err)
	}
	*settings.CompactionHandoffAtTokens = 70000
	status, err := runtime.ContextStatus()
	if err != nil || status.RequestBudget != 60000 || status.ReminderTokens != 55000 {
		t.Fatalf("reloaded thresholds: %#v %v", status, err)
	}
	settings.CompactionHandoffAtTokens, settings.CompactionReminderAtTokens = nil, nil
	if err := runtime.Reload(context.Background(), agent.ReloadOptions{}); err != nil {
		t.Fatal(err)
	}
	status, err = runtime.ContextStatus()
	if err != nil || status.RequestBudget != 111616 || status.ReminderTokens != 89292 {
		t.Fatalf("legacy fallback after reload: %#v %v", status, err)
	}
}

func TestExplicitContextThresholdsDriveRemindersAndReevaluateAfterReload(t *testing.T) {
	var runtime *agent.AgentSession
	settings := agent.RuntimeControlSettings{SteeringMode: agent.QueueOneAtATime, FollowUpMode: agent.QueueOneAtATime, AutoCompactionEnabled: true, AutoRetryEnabled: true, Retry: agent.RetryPolicy{MaxAttempts: 1}, CompactionHandoffAtTokens: contextThreshold(50000), CompactionReminderAtTokens: contextThreshold(45000)}
	step := func(id string, tokens uint64, wantSoft, wantUrgent int, reload bool) provider.ScriptStep {
		value, _ := provider.FactoryResponseStep(func(_ context.Context, request provider.Request, _ uint64) (llm.AssistantTerminal, error) {
			text := contextRequestText(t, request)
			if strings.Count(text, "tokens remaining for work") != wantSoft || strings.Count(text, "working budget") != wantUrgent {
				t.Fatal(text)
			}
			if reload {
				settings.CompactionReminderAtTokens = contextThreshold(40000)
				if err := runtime.Reload(context.Background(), agent.ReloadOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if id == "final" {
				return mustTextTerminal(t, "done"), nil
			}
			call, _ := llm.NewToolCallBlock(id, "work", []byte(`{}`))
			return newAssistantToolUseMessage([]llm.AssistantBlock{call}, mustUsage(t, tokens, 1), agentTestEpoch)
		})
		return value
	}
	runtime, _, manager := manualContextFixture(t, func(config *agent.SessionConfig) {
		config.HandoffAtTokens, config.ReminderAtTokens = contextThreshold(50000), contextThreshold(45000)
		config.ResolveRuntimeSettings = func() agent.RuntimeControlSettings { return settings }
	}, step("below", 42000, 0, 0, false), step("cross-soft", 46000, 0, 0, false), step("reload", 46000, 1, 0, true), step("cross-budget", 51000, 2, 0, false), step("final", 0, 2, 1, false))
	if result, err := runtime.Run(context.Background(), "task"); err != nil || !result.Succeeded() {
		t.Fatalf("run: %#v %v", result, err)
	}
	if window, _ := manager.CurrentContextWindow(); window != session.InitialContextWindow {
		t.Fatal("reminders switched context without a CLI submission")
	}
}

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
	old, err := llm.NewUserTextMessage(strings.Repeat("old text ", 500), agentTestEpoch)
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
	if err := runtime.ConfigureContextAccess("session CLI entry", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "continue"); err != nil {
		t.Fatal(err)
	}
	for _, request := range scripted.Requests() {
		text := contextRequestText(t, request)
		if strings.Count(text, "Context usage is approximately") != 1 {
			t.Fatalf("notice duplicated or missing: %s", text)
		}
		if request.SystemPrompt() != "stable system\n\nsession CLI entry" {
			t.Fatal("system prefix changed")
		}
	}
	view, err := manager.ReadHistoryView(session.HistoryViewQuery{Search: "Context usage"})
	if err != nil || len(view.Records) != 0 {
		t.Fatal("notice became user dialogue", err)
	}
}

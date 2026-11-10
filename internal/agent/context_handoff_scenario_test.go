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

func handoffModel(t *testing.T) provider.Model {
	t.Helper()
	model, err := newAgentModel(provider.ModelSpec{Provider: "scripted", API: "scripted", ID: "scripted-1", ContextWindow: 128000, MaxTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func handoffInstructions() agent.ContextAccessInstructions {
	return agent.ContextAccessInstructions{Handoff: "submit-handoff --stdin", History: "search-original-history; read-original-entry"}
}

func TestContextBudgetAllowsFinalAnswerWithoutCompaction(t *testing.T) {
	manager := newSessionManager(t)
	prior, _ := newAssistantTextMessage([]llm.TextBlock{mustTextBlock(t, "prior work")}, llm.FinishStop, mustUsage(t, 100000, 1), agentTestEpoch)
	_, _ = manager.AppendLLMMessage(context.Background(), prior)
	final, _ := newAssistantTextMessage([]llm.TextBlock{mustTextBlock(t, "done")}, llm.FinishStop, mustUsage(t, 120000, 1), agentTestEpoch.Add(time.Second))
	implementation := newScriptedProvider(t, final)
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: implementation, SessionManager: manager, Model: handoffModel(t), SystemPrompt: "minimal system"})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	if err := runtime.ConfigureContextAccess(handoffInstructions(), nil, nil); err != nil {
		t.Fatal(err)
	}
	compactions := 0
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if _, ok := event.(agent.CompactionStartEvent); ok {
			compactions++
		}
	})
	result, err := runtime.Run(context.Background(), "finish this task")
	if err != nil || !result.Succeeded() {
		t.Fatalf("run: %#v %v", result, err)
	}
	if len(implementation.Requests()) != 1 || compactions != 0 {
		t.Fatal("final answer caused extra work")
	}
	request := implementation.Requests()[0]
	text := contextRequestText(t, request)
	if request.SystemPrompt() != "minimal system" || !strings.Contains(text, "submit-handoff --stdin") || strings.Contains(text, "search-original-history") {
		t.Fatalf("instructions appeared at the wrong boundary: %s / %s", request.SystemPrompt(), text)
	}
}

func TestHardBudgetContinuesMainModelPrefixThenRecoversQueuedInput(t *testing.T) {
	manager := newSessionManager(t)
	definition, _ := provider.NewToolDefinition("work", "work", false, []byte(`{"type":"object"}`))
	call, _ := llm.NewToolCallBlock("work-1", "work", []byte(`{}`))
	working, _ := newAssistantToolUseMessage([]llm.AssistantBlock{call}, mustUsage(t, 112000, 5), agentTestEpoch)
	implementation, _ := provider.NewScriptedProvider(provider.ScriptedConfig{})
	first, _ := provider.FixedResponseStep(working)
	var runtime *agent.AgentSession
	write, _ := provider.FactoryResponseStep(func(_ context.Context, request provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		choice, present := request.ToolChoice()
		if present && choice.Mode == "none" || request.SystemPrompt() != "minimal system" || len(request.Tools()) != 1 {
			t.Error("handoff changed system/tools or disabled tools")
		}
		if request.StreamOptions().SessionID != "same-session" {
			t.Error("handoff changed cache attribution")
		}
		text := contextRequestText(t, request)
		if !strings.Contains(text, "original task body") || !strings.Contains(text, "tool receipt") || !strings.Contains(text, "brief handoff") {
			t.Fatalf("handoff did not continue the working context: %s", text)
		}
		if err := runtime.Steer("correction arriving during handoff"); err != nil {
			t.Fatal(err)
		}
		return mustToolUseTerminal(t, "submit", "work", []byte(`{}`)), nil
	})
	last, _ := provider.FactoryResponseStep(func(_ context.Context, request provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		text := contextRequestText(t, request)
		if request.SystemPrompt() != "minimal system" || strings.Contains(text, "original task body") || strings.Contains(text, "tool receipt") ||
			!strings.Contains(text, "Module A is done") || !strings.Contains(text, "search-original-history") || !strings.Contains(text, "correction arriving during handoff") {
			t.Fatalf("wrong recovered context: %s", text)
		}
		return mustTextTerminal(t, "completed B"), nil
	})
	_ = implementation.SetResponses([]provider.ScriptStep{first, write, last})
	var err error
	runtime, err = agent.NewSession(agent.SessionConfig{Provider: implementation, SessionManager: manager, Model: handoffModel(t), SystemPrompt: "minimal system",
		Tools: []provider.ToolDefinition{definition}, Tool: &fakeTool{name: "work", executeWithID: func(ctx context.Context, id string, _ []byte, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
			if id == "submit" {
				_, err := runtime.RequestContextChange(ctx, agent.ContextChangeRequest{Mode: "handoff", Handoff: "Module A is done. Verify module B next."})
				return agent.ToolOutput{Text: "accepted"}, err
			}
			return agent.ToolOutput{Text: "tool receipt"}, nil
		}},
		Stream: provider.StreamOptions{SessionID: "same-session"}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	_ = runtime.ConfigureContextAccess(handoffInstructions(), nil, nil)
	starts, ends := 0, 0
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		switch event := event.(type) {
		case agent.CompactionStartEvent:
			starts++
		case agent.CompactionEndEvent:
			ends++
			if event.Result == nil {
				t.Error("missing visible compaction result")
			}
		}
	})
	result, err := runtime.Run(context.Background(), "original task body")
	if err != nil || !result.Succeeded() {
		t.Fatalf("run: %#v %v", result, err)
	}
	if implementation.CallCount() != 3 || starts != 1 || ends != 1 {
		t.Fatalf("calls/events = %d/%d/%d", implementation.CallCount(), starts, ends)
	}
	for _, entry := range manager.Entries() {
		if entry.Type() == "compaction" {
			t.Fatal("old summary policy used")
		}
	}
	view, err := manager.ReadHistoryView(session.HistoryViewQuery{Search: "tool receipt", View: "raw"})
	if err != nil || len(view.Records) == 0 {
		t.Fatal("original tool receipt unavailable", err)
	}
}

func TestOverflowPreservesOriginalWindowWithoutAutomaticCheckpoint(t *testing.T) {
	manager := newSessionManager(t)
	_, _ = manager.AppendLLMMessage(context.Background(), mustTextTerminal(t, "earlier work"))
	failure, _ := provider.NewProviderFailure(provider.ProviderFailureSpec{Kind: provider.FailureContextOverflow, Message: "overflow", Cause: errors.New("overflow")})
	llmFailure, _ := llm.NewFailure("overflow", failure)
	failed, _ := newAssistantFailureMessageWithFailure(nil, llm.FinishError, llmFailure, mustUsage(t, 0, 0), agentTestEpoch.Add(time.Hour))
	implementation := newScriptedProvider(t, failed)
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: implementation, SessionManager: manager, Model: handoffModel(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	_ = runtime.ConfigureContextAccess(handoffInstructions(), nil, nil)
	result, err := runtime.Run(context.Background(), "unprocessed user correction")
	if err != nil || result.Succeeded() || implementation.CallCount() != 1 {
		t.Fatalf("run: %#v %v calls=%d", result, err, implementation.CallCount())
	}
	window, _ := manager.CurrentContextWindow()
	if window != session.InitialContextWindow {
		t.Fatal("overflow switched context without a CLI handoff")
	}
	view, err := manager.ReadHistoryView(session.HistoryViewQuery{View: "raw", Search: "unprocessed user correction"})
	if err != nil || len(view.Records) != 1 {
		t.Fatal("original input lost", err)
	}
}

func TestRepeatedHandoffsPreserveUnprocessedInputAcrossRestart(t *testing.T) {
	manager := newSessionManager(t)
	user, _ := llm.NewUserTextMessage("pending correction with exact wording", agentTestEpoch)
	_, _ = manager.AppendLLMMessage(context.Background(), user)
	implementation := newScriptedProvider(t, mustTextTerminal(t, "first handoff"), mustTextTerminal(t, "second handoff"))
	runtime, err := agent.NewSession(agent.SessionConfig{Provider: implementation, SessionManager: manager, Model: handoffModel(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	for _, handoff := range []string{"first handoff", "second handoff"} {
		if _, err := runtime.RequestContextChange(context.Background(), agent.ContextChangeRequest{Mode: "handoff", Handoff: handoff}); err != nil {
			t.Fatal(err)
		}
	}
	path, _ := manager.SessionFile()
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := session.OpenSessionManager(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	messages := reopened.BuildContext().Messages()
	if len(messages) != 2 || !strings.Contains(messageText(t, messages[0]), "second handoff") || messageText(t, messages[1]) != "pending correction with exact wording" {
		t.Fatalf("repeated checkpoint lost pending input: %#v", messages)
	}
	view, err := reopened.ReadHistoryView(session.HistoryViewQuery{})
	if err != nil || len(view.Records) != 1 {
		t.Fatalf("history duplicated pending input: %#v %v", view, err)
	}
}

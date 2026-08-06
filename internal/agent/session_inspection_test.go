package agent_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

func TestAgentSessionStatsAggregateEveryUsageBearingEntry(t *testing.T) {
	manager := newSessionManager(t)
	root := appendInspectionMessage(t, manager, mustInspectionUser(t, "hello", 1))

	callOne, err := llm.NewToolCallBlock("call-1", "read", []byte(`{"path":"one"}`))
	if err != nil {
		t.Fatal(err)
	}
	callTwo, err := llm.NewToolCallBlock("call-2", "read", []byte(`{"path":"two"}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := newAssistantToolUseMessage(
		[]llm.AssistantBlock{callOne, mustTextBlock(t, "working"), callTwo},
		mustInspectionUsage(t, 1, 2, 3, 4, 0.5), inspectionTime(2),
	)
	if err != nil {
		t.Fatal(err)
	}
	appendInspectionMessage(t, manager, assistant)

	toolUsage := mustInspectionUsage(t, 10, 20, 30, 40, 1)
	toolResult, err := llm.NewToolResultMessageWithMetadata(
		"call-1", "read", []llm.TextBlock{mustTextBlock(t, "result")}, false, inspectionTime(3),
		llm.ToolResultMetadata{Usage: &toolUsage},
	)
	if err != nil {
		t.Fatal(err)
	}
	appendInspectionMessage(t, manager, toolResult)
	bash, err := agentmsg.NewBashExecution(agentmsg.BashExecution{
		Command: "pwd", Output: "/work", At: inspectionTime(4),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendMessage(context.Background(), bash); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.AppendCompaction(
		context.Background(), "checkpoint", root.ID(), 10, nil, nil,
		mustInspectionSummaryUsage(t, 100, 200, 300, 400, 2),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BranchWithSummary(
		context.Background(), nil, "branch checkpoint", nil, nil,
		mustInspectionSummaryUsage(t, 1000, 2000, 3000, 4000, 3),
	); err != nil {
		t.Fatal(err)
	}
	aborted, err := newAssistantFailureMessage(
		nil, llm.FinishAborted, "cancelled", mustInspectionUsage(t, 7, 0, 0, 0, 0.25), inspectionTime(5),
	)
	if err != nil {
		t.Fatal(err)
	}
	appendInspectionMessage(t, manager, aborted)

	runtime := newInspectionSession(t, manager, inspectionModel(t))
	stats, err := runtime.GetSessionStats()
	if err != nil {
		t.Fatalf("GetSessionStats() error = %v", err)
	}
	if stats.SessionFile == nil || *stats.SessionFile == "" || stats.SessionID != manager.SessionID() {
		t.Fatalf("session identity = (%v, %q)", stats.SessionFile, stats.SessionID)
	}
	if stats.UserMessages != 1 || stats.AssistantMessages != 2 || stats.ToolCalls != 2 || stats.ToolResults != 1 || stats.TotalMessages != 5 {
		t.Fatalf("message stats = %#v", stats)
	}
	wantTokens := agent.SessionTokenTotals{Input: 1118, Output: 2222, CacheRead: 3333, CacheWrite: 4444, Total: 11117}
	if stats.Tokens != wantTokens {
		t.Fatalf("tokens = %#v, want %#v", stats.Tokens, wantTokens)
	}
	if math.Abs(stats.Cost-6.75) > 1e-12 {
		t.Fatalf("cost = %v, want 6.75", stats.Cost)
	}
	if stats.ContextUsage == nil {
		t.Fatal("context usage is absent with a selected model")
	}
}

func TestAgentSessionStatsAndContextUsageAcrossCompaction(t *testing.T) {
	t.Run("ordinary usage is exposed by stats and direct inspection", func(t *testing.T) {
		manager := newSessionManager(t)
		appendInspectionMessage(t, manager, mustInspectionUser(t, "hello", 1))
		appendInspectionMessage(t, manager, mustInspectionAssistant(t, "hi", mustInspectionUsage(t, 200, 0, 0, 0, 0), 2))
		runtime := newInspectionSession(t, manager, inspectionModel(t))

		direct, present, err := runtime.GetContextUsage()
		if err != nil || !present || direct.Tokens == nil || direct.Percent == nil {
			t.Fatalf("GetContextUsage() = (%#v, %t, %v)", direct, present, err)
		}
		stats, err := runtime.GetSessionStats()
		if err != nil {
			t.Fatal(err)
		}
		if stats.ContextUsage == nil || stats.ContextUsage.Tokens == nil || stats.ContextUsage.Percent == nil ||
			*stats.ContextUsage.Tokens != *direct.Tokens || *stats.ContextUsage.Percent != *direct.Percent {
			t.Fatalf("stats/direct context = (%#v, %#v)", stats.ContextUsage, direct)
		}
		if *direct.Tokens != 200 || direct.ContextWindow != 200_000 || *direct.Percent != 0.1 {
			t.Fatalf("context usage = %#v", direct)
		}
	})

	t.Run("unknown immediately after compaction while totals retain excluded history", func(t *testing.T) {
		manager := newSessionManager(t)
		appendInspectionMessage(t, manager, mustInspectionUser(t, "first", 1))
		appendInspectionMessage(t, manager, mustInspectionAssistant(t, "response1", mustInspectionUsage(t, 180_000, 0, 0, 0, 0), 2))
		kept := appendInspectionMessage(t, manager, mustInspectionUser(t, "second", 3))
		appendInspectionMessage(t, manager, mustInspectionAssistant(t, "response2", mustInspectionUsage(t, 195_000, 0, 0, 0, 0), 4))
		if _, err := manager.AppendCompaction(context.Background(), "summary", kept.ID(), 195_000, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		appendInspectionMessage(t, manager, mustInspectionUser(t, "third", 5))

		runtime := newInspectionSession(t, manager, inspectionModel(t))
		stats, err := runtime.GetSessionStats()
		if err != nil {
			t.Fatal(err)
		}
		if stats.Tokens.Input != 375_000 {
			t.Fatalf("all-entry input = %d, want 375000", stats.Tokens.Input)
		}
		assertUnknownInspectionContext(t, stats.ContextUsage, 200_000)
	})

	t.Run("successful post-compaction usage becomes the current boundary", func(t *testing.T) {
		manager := inspectionCompactedManager(t)
		appendInspectionMessage(t, manager, mustInspectionUser(t, "third", 5))
		appendInspectionMessage(t, manager, mustInspectionAssistant(t, "response3", mustInspectionUsage(t, 25_000, 0, 0, 0, 0), 6))
		runtime := newInspectionSession(t, manager, inspectionModel(t))

		usage, present, err := runtime.GetContextUsage()
		if err != nil || !present || usage.Tokens == nil || usage.Percent == nil {
			t.Fatalf("GetContextUsage() = (%#v, %t, %v)", usage, present, err)
		}
		if *usage.Tokens != 25_000 || *usage.Percent != 12.5 {
			t.Fatalf("context usage = %#v", usage)
		}
	})

	t.Run("zero usage alone does not make post-compaction context known", func(t *testing.T) {
		manager := inspectionCompactedManager(t)
		appendInspectionMessage(t, manager, mustInspectionUser(t, "third", 5))
		appendInspectionMessage(t, manager, mustInspectionAssistant(t, "partial", mustInspectionUsage(t, 0, 0, 0, 0, 0), 6))
		runtime := newInspectionSession(t, manager, inspectionModel(t))
		usage, present, err := runtime.GetContextUsage()
		if err != nil || !present {
			t.Fatalf("GetContextUsage() = (%#v, %t, %v)", usage, present, err)
		}
		assertUnknownInspectionContext(t, &usage, 200_000)
	})

	t.Run("zero usage after a valid response remains estimated trailing context", func(t *testing.T) {
		manager := inspectionCompactedManager(t)
		appendInspectionMessage(t, manager, mustInspectionUser(t, "third", 5))
		appendInspectionMessage(t, manager, mustInspectionAssistant(t, "response3", mustInspectionUsage(t, 25_000, 0, 0, 0, 0), 6))
		appendInspectionMessage(t, manager, mustInspectionUser(t, "continue", 7))
		appendInspectionMessage(t, manager, mustInspectionAssistant(t, "partial", mustInspectionUsage(t, 0, 0, 0, 0, 0), 8))
		runtime := newInspectionSession(t, manager, inspectionModel(t))

		usage, present, err := runtime.GetContextUsage()
		if err != nil || !present || usage.Tokens == nil || usage.Percent == nil {
			t.Fatalf("GetContextUsage() = (%#v, %t, %v)", usage, present, err)
		}
		if *usage.Tokens <= 25_000 {
			t.Fatalf("tokens = %d, want trailing estimate after 25000", *usage.Tokens)
		}
	})

	t.Run("aborted and error usage do not establish a post-compaction boundary", func(t *testing.T) {
		for _, finish := range []llm.FinishReason{llm.FinishAborted, llm.FinishError} {
			t.Run(finish.String(), func(t *testing.T) {
				manager := inspectionCompactedManager(t)
				failure, err := newAssistantFailureMessage(nil, finish, "failed", mustInspectionUsage(t, 50_000, 0, 0, 0, 0), inspectionTime(9))
				if err != nil {
					t.Fatal(err)
				}
				appendInspectionMessage(t, manager, failure)
				runtime := newInspectionSession(t, manager, inspectionModel(t))
				usage, present, err := runtime.GetContextUsage()
				if err != nil || !present {
					t.Fatalf("GetContextUsage() = (%#v, %t, %v)", usage, present, err)
				}
				assertUnknownInspectionContext(t, &usage, 200_000)
			})
		}
	})

	t.Run("compaction on an unselected sibling does not poison current usage", func(t *testing.T) {
		manager := newSessionManager(t)
		root := appendInspectionMessage(t, manager, mustInspectionUser(t, "root", 1))
		kept := appendInspectionMessage(t, manager, mustInspectionUser(t, "compacted branch", 2))
		appendInspectionMessage(t, manager, mustInspectionAssistant(t, "old", mustInspectionUsage(t, 1000, 0, 0, 0, 0), 3))
		if _, err := manager.AppendCompaction(context.Background(), "summary", kept.ID(), 1000, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := manager.Branch(root.ID()); err != nil {
			t.Fatal(err)
		}
		appendInspectionMessage(t, manager, mustInspectionAssistant(t, "selected", mustInspectionUsage(t, 50, 0, 0, 0, 0), 4))
		runtime := newInspectionSession(t, manager, inspectionModel(t))

		usage, present, err := runtime.GetContextUsage()
		if err != nil || !present || usage.Tokens == nil || *usage.Tokens != 50 {
			t.Fatalf("GetContextUsage() = (%#v, %t, %v)", usage, present, err)
		}
	})
}

func TestAgentSessionContextUsageAbsentWithoutModel(t *testing.T) {
	manager := newSessionManager(t)
	runtime := newInspectionSession(t, manager, provider.Model{})
	if usage, present, err := runtime.GetContextUsage(); err != nil || present {
		t.Fatalf("GetContextUsage() = (%#v, %t, %v), want absent", usage, present, err)
	}
	stats, err := runtime.GetSessionStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.ContextUsage != nil {
		t.Fatalf("stats context usage = %#v, want nil", stats.ContextUsage)
	}
}

func TestAgentSessionGetLastAssistantText(t *testing.T) {
	manager := newSessionManager(t)
	appendInspectionMessage(t, manager, mustInspectionAssistant(t, "  older answer  ", mustInspectionUsage(t, 1, 0, 0, 0, 0), 1))
	aborted, err := newAssistantFailureMessage(nil, llm.FinishAborted, "cancelled", mustInspectionUsage(t, 0, 0, 0, 0, 0), inspectionTime(2))
	if err != nil {
		t.Fatal(err)
	}
	appendInspectionMessage(t, manager, aborted)
	runtime := newInspectionSession(t, manager, inspectionModel(t))

	if text, ok := runtime.GetLastAssistantText(); !ok || text != "older answer" {
		t.Fatalf("GetLastAssistantText() = (%q, %t)", text, ok)
	}
	multi, err := newAssistantTextMessage(
		[]llm.TextBlock{mustTextBlock(t, "  hello "), mustTextBlock(t, "world  ")},
		llm.FinishStop, mustInspectionUsage(t, 2, 0, 0, 0, 0), inspectionTime(3),
	)
	if err != nil {
		t.Fatal(err)
	}
	appendInspectionMessage(t, manager, multi)
	if err := runtime.ReloadMessagesFromSession(); err != nil {
		t.Fatal(err)
	}
	if text, ok := runtime.GetLastAssistantText(); !ok || text != "hello world" {
		t.Fatalf("GetLastAssistantText() = (%q, %t)", text, ok)
	}
}

func TestAgentSessionGetUserMessagesForForkingUsesAllEntries(t *testing.T) {
	manager := newSessionManager(t)
	first, err := llm.NewUserTextBlocksMessage(
		[]llm.TextBlock{mustTextBlock(t, "one"), mustTextBlock(t, "two")}, inspectionTime(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	root := appendInspectionMessage(t, manager, first)
	originalAssistant := appendInspectionMessage(t, manager, mustInspectionAssistant(t, "original", mustInspectionUsage(t, 1, 0, 0, 0, 0), 2))
	if err := manager.Branch(root.ID()); err != nil {
		t.Fatal(err)
	}
	image, err := llm.NewImageURLBlock("image/png", "https://example.com/image.png")
	if err != nil {
		t.Fatal(err)
	}
	rich, err := llm.NewUserContentMessage(
		[]llm.UserContentBlock{mustTextBlock(t, "three"), image, mustTextBlock(t, "four")}, inspectionTime(3),
	)
	if err != nil {
		t.Fatal(err)
	}
	second := appendInspectionMessage(t, manager, rich)
	appendInspectionMessage(t, manager, mustInspectionUser(t, "", 4))
	if err := manager.Branch(originalAssistant.ID()); err != nil {
		t.Fatal(err)
	}

	runtime := newInspectionSession(t, manager, inspectionModel(t))
	messages := runtime.GetUserMessagesForForking()
	if len(messages) != 2 {
		t.Fatalf("fork messages = %#v", messages)
	}
	if messages[0] != (agent.UserMessageForForking{EntryID: root.ID(), Text: "onetwo"}) ||
		messages[1] != (agent.UserMessageForForking{EntryID: second.ID(), Text: "threefour"}) {
		t.Fatalf("fork messages = %#v", messages)
	}
}

func inspectionCompactedManager(t *testing.T) *session.SessionManager {
	t.Helper()
	manager := newSessionManager(t)
	appendInspectionMessage(t, manager, mustInspectionUser(t, "first", 1))
	appendInspectionMessage(t, manager, mustInspectionAssistant(t, "response1", mustInspectionUsage(t, 180_000, 0, 0, 0, 0), 2))
	kept := appendInspectionMessage(t, manager, mustInspectionUser(t, "second", 3))
	appendInspectionMessage(t, manager, mustInspectionAssistant(t, "response2", mustInspectionUsage(t, 195_000, 0, 0, 0, 0), 4))
	if _, err := manager.AppendCompaction(context.Background(), "summary", kept.ID(), 195_000, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	return manager
}

func newInspectionSession(t *testing.T, manager *session.SessionManager, model provider.Model) *agent.AgentSession {
	t.Helper()
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: manager, Model: model,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func inspectionModel(t *testing.T) provider.Model {
	t.Helper()
	model, err := newAgentModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "stats-model", Name: "stats-model",
		Input: []provider.InputKind{provider.InputText}, ContextWindow: 200_000, MaxTokens: 8_192,
	})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func appendInspectionMessage(t *testing.T, manager *session.SessionManager, message llm.ConversationMessage) session.Entry {
	t.Helper()
	entry, err := manager.AppendLLMMessage(context.Background(), message)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func mustInspectionUser(t *testing.T, text string, tick int64) llm.UserTextMessage {
	t.Helper()
	message, err := llm.NewUserTextMessage(text, inspectionTime(tick))
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func mustInspectionAssistant(t *testing.T, text string, usage llm.Usage, tick int64) llm.AssistantTextMessage {
	t.Helper()
	message, err := newAssistantTextMessage(
		[]llm.TextBlock{mustTextBlock(t, text)}, llm.FinishStop, usage, inspectionTime(tick),
	)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func mustInspectionUsage(t *testing.T, input, output, cacheRead, cacheWrite uint64, totalCost float64) llm.Usage {
	t.Helper()
	cost := llm.Cost{Total: totalCost}
	usage, err := llm.NewUsage(llm.UsageSpec{
		Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite, Cost: &cost,
	})
	if err != nil {
		t.Fatal(err)
	}
	return usage
}

func mustInspectionSummaryUsage(t *testing.T, input, output, cacheRead, cacheWrite uint64, totalCost float64) *session.CompactionUsage {
	t.Helper()
	usage := mustInspectionUsage(t, input, output, cacheRead, cacheWrite, 99)
	return &session.CompactionUsage{Usage: usage, Cost: session.UsageCostFromLLM(llm.Cost{Total: totalCost})}
}

func assertUnknownInspectionContext(t *testing.T, usage *agent.ContextUsage, contextWindow uint64) {
	t.Helper()
	if usage == nil || usage.ContextWindow != contextWindow || usage.Tokens != nil || usage.Percent != nil {
		t.Fatalf("context usage = %#v, want unknown with window %d", usage, contextWindow)
	}
}

func inspectionTime(tick int64) time.Time {
	return agentTestEpoch.Add(time.Duration(tick) * time.Millisecond)
}

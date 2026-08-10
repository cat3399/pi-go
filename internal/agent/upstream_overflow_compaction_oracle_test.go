package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	catalogmodel "github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/provider"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
)

type upstreamOverflowCompactionResponse struct {
	Text         string `json:"text"`
	InputTokens  uint64 `json:"inputTokens"`
	OutputTokens uint64 `json:"outputTokens"`
}

type upstreamOverflowCompactionScenario struct {
	Name             string                             `json:"name"`
	SessionID        string                             `json:"sessionId"`
	SystemPrompt     string                             `json:"systemPrompt"`
	FirstPrompt      string                             `json:"firstPrompt"`
	OverflowPrompt   string                             `json:"overflowPrompt"`
	ErrorMessage     string                             `json:"errorMessage"`
	ReserveTokens    uint64                             `json:"reserveTokens"`
	KeepRecentTokens uint64                             `json:"keepRecentTokens"`
	SeedResponse     upstreamOverflowCompactionResponse `json:"seedResponse"`
	SummaryResponse  upstreamOverflowCompactionResponse `json:"summaryResponse"`
	RecoveryResponse upstreamOverflowCompactionResponse `json:"recoveryResponse"`
}

// TestUpstreamAgentSessionOverflowCompactionOracle fixes the complete overflow
// recovery pipeline: durable failure, live-state removal, automatic summary,
// context rebuild, Agent continue, one final settlement, and reopen history.
func TestUpstreamAgentSessionOverflowCompactionOracle(t *testing.T) {
	var corpus upstreamWorkflowCorpus
	if err := json.Unmarshal(upstreamWorkflowCorpusJSON, &corpus); err != nil {
		t.Fatalf("decode workflow corpus: %v", err)
	}
	expectedRoot, err := decodeWorkflowJSON(upstreamWorkflowOracleJSON)
	if err != nil {
		t.Fatalf("decode workflow oracle: %v", err)
	}
	expectedObject, ok := expectedRoot.(map[string]any)
	if !ok {
		t.Fatal("workflow oracle root is not an object")
	}
	expectedScenario, ok := expectedObject["overflowCompactionScenario"].(map[string]any)
	if !ok {
		t.Fatal("workflow oracle overflowCompactionScenario is not an object")
	}
	scenario := corpus.OverflowCompaction
	if scenario.ErrorMessage == "" || scenario.ReserveTokens == 0 || scenario.KeepRecentTokens == 0 {
		t.Fatal("overflow compaction corpus no longer covers typed overflow recovery")
	}

	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	agentDir := filepath.Join(root, "agent")
	sessionDir := filepath.Join(root, "sessions")
	for _, directory := range []string{cwd, agentDir, sessionDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create overflow compaction workflow directory: %v", err)
		}
	}

	var clockTick atomic.Int64
	var entrySequence atomic.Uint64
	manager, err := session.CreateSessionManagerWithOptions(cwd, sessionDir, session.ManagerOptions{
		NewSession: session.NewSessionOptions{ID: scenario.SessionID},
		Now: func() time.Time {
			return agentTestEpoch.Add(time.Duration(clockTick.Add(1)) * time.Millisecond)
		},
		NewEntryID: func() (string, error) {
			return fmt.Sprintf("go-overflow-compaction-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("create overflow compaction session manager: %v", err)
	}
	managerOwned := true
	defer func() {
		if managerOwned {
			_ = manager.Close()
		}
	}()

	selected := catalogmodel.Model{
		Provider: "faux", API: "anthropic-messages", ID: "faux-1", Name: "Faux Model",
		BaseURL: "http://localhost:0", Input: []provider.InputKind{provider.InputText, provider.InputImage},
		Cost: provider.CostRates{}, ContextWindow: 128_000, MaxTokens: 16_384,
	}
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{
		ChunkRunes: 3,
		Clock:      func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatalf("construct overflow compaction provider: %v", err)
	}
	responseStep := func(label string, response upstreamOverflowCompactionResponse) provider.ScriptStep {
		terminal, terminalErr := newAssistantTextMessage(
			[]llm.TextBlock{mustTextBlock(t, response.Text)},
			llm.FinishStop,
			mustUsage(t, response.InputTokens, response.OutputTokens),
			agentTestEpoch,
		)
		if terminalErr != nil {
			t.Fatalf("construct %s response: %v", label, terminalErr)
		}
		step, stepErr := provider.FixedResponseStep(terminal)
		if stepErr != nil {
			t.Fatalf("construct %s response step: %v", label, stepErr)
		}
		return step
	}
	providerFailure, err := provider.NewProviderFailure(provider.ProviderFailureSpec{
		Kind: provider.FailureContextOverflow, Message: scenario.ErrorMessage,
		Cause: errors.New("fixture context overflow"),
	})
	if err != nil {
		t.Fatalf("construct overflow provider failure: %v", err)
	}
	failure, err := llm.NewFailure(scenario.ErrorMessage, providerFailure)
	if err != nil {
		t.Fatalf("construct overflow LLM failure: %v", err)
	}
	overflow, err := newAssistantFailureMessageWithFailure(
		[]llm.TextBlock{mustTextBlock(t, "")},
		llm.FinishError,
		failure,
		llm.Usage{},
		agentTestEpoch,
	)
	if err != nil {
		t.Fatalf("construct overflow response: %v", err)
	}
	overflowStep, err := provider.FixedResponseStep(overflow)
	if err != nil {
		t.Fatalf("construct overflow response step: %v", err)
	}
	steps := []provider.ScriptStep{
		responseStep("seed", scenario.SeedResponse),
		overflowStep,
		responseStep("summary", scenario.SummaryResponse),
		responseStep("recovery", scenario.RecoveryResponse),
	}
	if err := implementation.SetResponses(steps); err != nil {
		t.Fatalf("set overflow compaction responses: %v", err)
	}

	enabled := true
	disabled := false
	reserveTokens := scenario.ReserveTokens
	keepRecentTokens := scenario.KeepRecentTokens
	providerMaxRetries := uint64(0)
	off := provider.ThinkingOff
	created, err := agentruntime.CreateAgentSession(context.Background(), agentruntime.SessionFactoryOptions{
		Services: &agentruntime.Services{
			CWD: cwd, AgentDir: agentDir, Provider: implementation,
		},
		Provider:       implementation,
		SessionManager: manager,
		AllModels:      []catalogmodel.Model{selected},
		Availability: catalogmodel.Availability{
			HasConfiguredAuth: func(string) bool { return true },
			SupportsRoute:     func(catalogmodel.Model) bool { return true },
		},
		ExplicitModel:         &selected,
		ExplicitThinkingLevel: &off,
		Settings: catalogmodel.Settings{
			Transport: provider.TransportSSE,
			Compaction: catalogmodel.CompactionSettings{
				Enabled: &enabled, ReserveTokens: &reserveTokens, KeepRecentTokens: &keepRecentTokens,
			},
			Retry: catalogmodel.RetrySettings{
				Enabled:  &disabled,
				Provider: catalogmodel.ProviderRetrySettings{MaxRetries: &providerMaxRetries},
			},
		},
		BaseConfig: agent.SessionConfig{
			SystemPrompt: scenario.SystemPrompt + "\nCurrent working directory: " + cwd,
			Stream: provider.StreamOptions{
				SessionID: scenario.SessionID,
				Transport: provider.TransportSSE,
			},
			ResolveSummarizer: func(_ context.Context, request agent.SummarizerResolveRequest) (session.Summarizer, error) {
				return provider.NewContextSummarizerWithOptions(
					implementation,
					request.Model,
					func() time.Time { return agentTestEpoch },
					provider.ContextSummarizerOptions{
						ThinkingLevel: request.ThinkingLevel,
						Stream:        request.Stream,
						Retry:         request.Retry,
					},
				)
			},
			Retry: agent.RetryPolicy{
				Sleep: func(context.Context, time.Duration) error { return nil },
			},
			Now:               func() time.Time { return agentTestEpoch },
			SettlementTimeout: time.Second,
		},
	})
	if err != nil {
		t.Fatalf("create Go overflow compaction AgentSession: %v", err)
	}
	runtime := created.Session
	runtimeOwned := true
	defer func() {
		if runtimeOwned {
			_ = runtime.Close(context.Background())
		}
	}()

	var eventMu sync.Mutex
	var observed []agent.SessionEvent
	var settledSnapshots []any
	unsubscribe := runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		eventMu.Lock()
		observed = append(observed, event)
		if _, settled := event.(agent.AgentSettledEvent); settled {
			activity := runtime.Activity()
			queue := runtime.PendingQueue()
			settledSnapshots = append(settledSnapshots, map[string]any{
				"isStreaming":         activity.IsStreaming,
				"isIdle":              activity.Phase == agent.PhaseIdle,
				"isCompacting":        activity.IsCompacting,
				"steering":            append([]string{}, queue.Steering...),
				"followUp":            append([]string{}, queue.FollowUp...),
				"pendingMessageCount": len(queue.SteeringMessages) + len(queue.FollowUpMessages),
			})
		}
		eventMu.Unlock()
	})

	seed, err := runtime.Prompt(context.Background(), scenario.FirstPrompt)
	if err != nil || !seed.Succeeded() {
		t.Fatalf("seed overflow compaction prompt = (%#v, %v)", seed, err)
	}
	eventMu.Lock()
	seedSettledCount := len(settledSnapshots)
	eventMu.Unlock()
	seedReturn := upstreamOverflowReturnSnapshot(runtime, seedSettledCount)
	recovered, err := runtime.Prompt(context.Background(), scenario.OverflowPrompt)
	if err != nil || !recovered.Succeeded() {
		t.Fatalf("overflow recovery prompt = (%#v, %v)", recovered, err)
	}
	eventMu.Lock()
	overflowSettledCount := len(settledSnapshots)
	events := append([]agent.SessionEvent(nil), observed...)
	settled := append([]any(nil), settledSnapshots...)
	eventMu.Unlock()
	overflowReturn := upstreamOverflowReturnSnapshot(runtime, overflowSettledCount)
	if implementation.CallCount() != 4 || implementation.PendingResponses() != 0 {
		t.Fatalf("overflow compaction provider calls/pending = %d/%d, want 4/0", implementation.CallCount(), implementation.PendingResponses())
	}
	if len(settled) != 2 {
		t.Fatalf("overflow compaction settled events = %d, want 2", len(settled))
	}
	unsubscribe()

	entries := manager.Entries()
	entryIDs := workflowEntryIDs(entries)
	header := manager.Header()
	sessionFile, ok := manager.SessionFile()
	if !ok {
		t.Fatal("overflow compaction manager has no persistent session file")
	}
	stats, err := runtime.GetSessionStats()
	if err != nil {
		t.Fatalf("overflow compaction stats: %v", err)
	}
	providerInputs, err := normalizeWorkflowProviderInputs(implementation.Requests(), root, cwd, scenario.SessionID)
	if err != nil {
		t.Fatalf("normalize overflow compaction provider inputs: %v", err)
	}
	normalizedEvents, err := normalizeWorkflowEvents(events, entryIDs)
	if err != nil {
		t.Fatalf("normalize overflow compaction events: %v", err)
	}
	state := runtime.State()
	selectedRef, selectedOK := runtime.SelectedModel()
	if !selectedOK {
		t.Fatal("overflow compaction session lost its selected model")
	}
	finalMessages, err := normalizeWorkflowAgentMessages(state.Active.Messages())
	if err != nil {
		t.Fatalf("normalize final overflow compaction messages: %v", err)
	}
	normalizedEntries, err := normalizeWorkflowEntries(entries, entryIDs)
	if err != nil {
		t.Fatalf("normalize overflow compaction entries: %v", err)
	}
	finalActivity := runtime.Activity()
	finalThinkingLevel := runtime.ThinkingLevel()
	finalActiveTools := append([]string{}, runtime.ActiveToolNames()...)
	finalSystemPrompt := runtime.SystemPrompt()
	finalPendingMessageCount := runtime.PendingMessageCount()
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("close overflow compaction runtime: %v", err)
	}
	runtimeOwned = false
	managerOwned = false

	fileHeader, fileEntries, err := normalizeWorkflowJSONL(sessionFile, entryIDs, root, cwd)
	if err != nil {
		t.Fatalf("normalize overflow compaction JSONL: %v", err)
	}
	reopened, err := session.OpenSessionManagerWithOptions(sessionFile, sessionDir, "", session.ManagerOptions{
		Now: func() time.Time { return agentTestEpoch },
		NewEntryID: func() (string, error) {
			return fmt.Sprintf("reopened-overflow-compaction-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("reopen overflow compaction session: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	reopenedEntries, err := normalizeWorkflowEntries(reopened.Entries(), entryIDs)
	if err != nil {
		t.Fatalf("normalize reopened overflow compaction entries: %v", err)
	}
	reopenedContext := reopened.BuildContext()
	reopenedMessages, err := normalizeWorkflowAgentMessages(reopenedContext.AgentMessages())
	if err != nil {
		t.Fatalf("normalize reopened overflow compaction messages: %v", err)
	}
	reopenedModel, hasReopenedModel := reopenedContext.Model()
	reopenedThinking, hasReopenedThinking := reopenedContext.ThinkingLevel()
	if !hasReopenedModel || !hasReopenedThinking {
		t.Fatalf("reopened overflow compaction selection = model:%t thinking:%t", hasReopenedModel, hasReopenedThinking)
	}

	actualScenario := map[string]any{
		"name":  scenario.Name,
		"input": scenario,
		"actions": map[string]any{
			"seedReturn":       seedReturn,
			"overflowReturn":   overflowReturn,
			"settledSnapshots": settled,
		},
		"providerInputs": providerInputs,
		"events":         normalizedEvents,
		"finalState": map[string]any{
			"isStreaming":         finalActivity.IsStreaming,
			"pendingMessageCount": finalPendingMessageCount,
			"model": map[string]any{
				"provider": selectedRef.Provider(), "api": selectedRef.API(), "id": selectedRef.ID(),
			},
			"thinkingLevel": string(finalThinkingLevel),
			"activeTools":   finalActiveTools,
			"systemPrompt":  normalizeWorkflowPath(finalSystemPrompt, root, cwd),
			"messages":      finalMessages,
			"stats":         normalizeWorkflowStats(stats),
		},
		"session": map[string]any{
			"header":      normalizeWorkflowHeader(header, root, cwd),
			"entries":     normalizedEntries,
			"fileEntries": fileEntries,
			"reopened": map[string]any{
				"header":  normalizeWorkflowHeader(reopened.Header(), root, cwd),
				"entries": reopenedEntries,
				"context": map[string]any{
					"messages": reopenedMessages,
					"model": map[string]any{
						"provider": reopenedModel.Provider, "modelId": reopenedModel.ModelID,
					},
					"thinkingLevel": reopenedThinking,
				},
			},
		},
	}
	if difference := workflowJSONDifference("overflowCompactionScenario", expectedScenario, canonicalWorkflowJSON(t, actualScenario)); difference != "" {
		t.Fatalf("Go overflow compaction workflow differs from pinned TypeScript oracle: %s", difference)
	}
	if difference := workflowJSONDifference(
		"overflowCompactionScenario.session.header",
		expectedScenario["session"].(map[string]any)["header"],
		fileHeader,
	); difference != "" {
		t.Fatalf("physical overflow compaction header differs from pinned TypeScript oracle: %s", difference)
	}
}

func upstreamOverflowReturnSnapshot(runtime *agent.AgentSession, settledEventCount int) map[string]any {
	activity := runtime.Activity()
	queue := runtime.PendingQueue()
	return map[string]any{
		"isStreaming":         activity.IsStreaming,
		"isIdle":              activity.Phase == agent.PhaseIdle,
		"isCompacting":        activity.IsCompacting,
		"settledEventCount":   settledEventCount,
		"steering":            append([]string{}, queue.Steering...),
		"followUp":            append([]string{}, queue.FollowUp...),
		"pendingMessageCount": len(queue.SteeringMessages) + len(queue.FollowUpMessages),
	}
}

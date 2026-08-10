package agent_test

import (
	"context"
	"encoding/json"
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

type upstreamManualCompactionScenario struct {
	Name               string `json:"name"`
	SessionID          string `json:"sessionId"`
	SystemPrompt       string `json:"systemPrompt"`
	FirstPrompt        string `json:"firstPrompt"`
	SecondPrompt       string `json:"secondPrompt"`
	CustomInstructions string `json:"customInstructions"`
	ReserveTokens      uint64 `json:"reserveTokens"`
	KeepRecentTokens   uint64 `json:"keepRecentTokens"`
	Responses          []struct {
		Text         string `json:"text"`
		InputTokens  uint64 `json:"inputTokens"`
		OutputTokens uint64 `json:"outputTokens"`
	} `json:"responses"`
}

// TestUpstreamAgentSessionManualCompactionOracle exercises compaction through
// the production AgentSession and the production provider-backed summarizer.
// It fixes summary request isolation, public lifecycle/result fields, durable
// v3 compaction data, live context rebuild, and reopen behavior to upstream.
func TestUpstreamAgentSessionManualCompactionOracle(t *testing.T) {
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
	expectedScenario, ok := expectedObject["manualCompactionScenario"].(map[string]any)
	if !ok {
		t.Fatal("workflow oracle manualCompactionScenario is not an object")
	}
	scenario := corpus.ManualCompaction
	if len(scenario.Responses) != 3 || scenario.ReserveTokens == 0 || scenario.KeepRecentTokens == 0 {
		t.Fatal("manual compaction corpus no longer covers two turns plus one real summary call")
	}

	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	agentDir := filepath.Join(root, "agent")
	sessionDir := filepath.Join(root, "sessions")
	for _, directory := range []string{cwd, agentDir, sessionDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create manual compaction workflow directory: %v", err)
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
			return fmt.Sprintf("go-manual-compaction-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("create manual compaction session manager: %v", err)
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
		t.Fatalf("construct manual compaction provider: %v", err)
	}
	steps := make([]provider.ScriptStep, 0, len(scenario.Responses))
	for index, response := range scenario.Responses {
		terminal, terminalErr := newAssistantTextMessage(
			[]llm.TextBlock{mustTextBlock(t, response.Text)},
			llm.FinishStop,
			mustUsage(t, response.InputTokens, response.OutputTokens),
			agentTestEpoch,
		)
		if terminalErr != nil {
			t.Fatalf("construct manual compaction response %d: %v", index, terminalErr)
		}
		step, stepErr := provider.FixedResponseStep(terminal)
		if stepErr != nil {
			t.Fatalf("construct manual compaction response step %d: %v", index, stepErr)
		}
		steps = append(steps, step)
	}
	if err := implementation.SetResponses(steps); err != nil {
		t.Fatalf("set manual compaction responses: %v", err)
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
		t.Fatalf("create Go manual compaction AgentSession: %v", err)
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
	unsubscribe := runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		eventMu.Lock()
		observed = append(observed, event)
		eventMu.Unlock()
	})

	first, err := runtime.Prompt(context.Background(), scenario.FirstPrompt)
	if err != nil || !first.Succeeded() {
		t.Fatalf("first manual compaction prompt = (%#v, %v)", first, err)
	}
	second, err := runtime.Prompt(context.Background(), scenario.SecondPrompt)
	if err != nil || !second.Succeeded() {
		t.Fatalf("second manual compaction prompt = (%#v, %v)", second, err)
	}
	beforeActivity := runtime.Activity()
	beforeQueue := runtime.PendingQueue()
	beforeCompact := map[string]any{
		"isStreaming":         beforeActivity.IsStreaming,
		"isIdle":              beforeActivity.Phase == agent.PhaseIdle,
		"isCompacting":        beforeActivity.IsCompacting,
		"steering":            append([]string{}, beforeQueue.Steering...),
		"followUp":            append([]string{}, beforeQueue.FollowUp...),
		"pendingMessageCount": len(beforeQueue.SteeringMessages) + len(beforeQueue.FollowUpMessages),
	}
	compactResult, err := runtime.Compact(context.Background(), scenario.CustomInstructions)
	if err != nil || !compactResult.Committed {
		t.Fatalf("manual compaction = (%#v, %v)", compactResult, err)
	}
	afterActivity := runtime.Activity()
	afterQueue := runtime.PendingQueue()
	afterCompact := map[string]any{
		"isStreaming":         afterActivity.IsStreaming,
		"isIdle":              afterActivity.Phase == agent.PhaseIdle,
		"isCompacting":        afterActivity.IsCompacting,
		"steering":            append([]string{}, afterQueue.Steering...),
		"followUp":            append([]string{}, afterQueue.FollowUp...),
		"pendingMessageCount": len(afterQueue.SteeringMessages) + len(afterQueue.FollowUpMessages),
	}
	if implementation.CallCount() != 3 || implementation.PendingResponses() != 0 {
		t.Fatalf("manual compaction provider calls/pending = %d/%d, want 3/0", implementation.CallCount(), implementation.PendingResponses())
	}
	unsubscribe()
	eventMu.Lock()
	events := append([]agent.SessionEvent(nil), observed...)
	eventMu.Unlock()

	entries := manager.Entries()
	entryIDs := workflowEntryIDs(entries)
	header := manager.Header()
	sessionFile, ok := manager.SessionFile()
	if !ok {
		t.Fatal("manual compaction manager has no persistent session file")
	}
	stats, err := runtime.GetSessionStats()
	if err != nil {
		t.Fatalf("manual compaction stats: %v", err)
	}
	providerInputs, err := normalizeWorkflowProviderInputs(implementation.Requests(), root, cwd, scenario.SessionID)
	if err != nil {
		t.Fatalf("normalize manual compaction provider inputs: %v", err)
	}
	normalizedEvents, err := normalizeWorkflowEvents(events, entryIDs)
	if err != nil {
		t.Fatalf("normalize manual compaction events: %v", err)
	}
	normalizedCompactResult, err := normalizeWorkflowCompactionResult(compactResult, entryIDs)
	if err != nil {
		t.Fatalf("normalize manual compaction result: %v", err)
	}
	state := runtime.State()
	selectedRef, selectedOK := runtime.SelectedModel()
	if !selectedOK {
		t.Fatal("manual compaction session lost its selected model")
	}
	finalMessages, err := normalizeWorkflowAgentMessages(state.Active.Messages())
	if err != nil {
		t.Fatalf("normalize final manual compaction messages: %v", err)
	}
	normalizedEntries, err := normalizeWorkflowEntries(entries, entryIDs)
	if err != nil {
		t.Fatalf("normalize manual compaction entries: %v", err)
	}
	finalActivity := runtime.Activity()
	finalThinkingLevel := runtime.ThinkingLevel()
	finalActiveTools := append([]string{}, runtime.ActiveToolNames()...)
	finalSystemPrompt := runtime.SystemPrompt()
	finalPendingMessageCount := runtime.PendingMessageCount()
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("close manual compaction runtime: %v", err)
	}
	runtimeOwned = false
	managerOwned = false

	fileHeader, fileEntries, err := normalizeWorkflowJSONL(sessionFile, entryIDs, root, cwd)
	if err != nil {
		t.Fatalf("normalize manual compaction JSONL: %v", err)
	}
	reopened, err := session.OpenSessionManagerWithOptions(sessionFile, sessionDir, "", session.ManagerOptions{
		Now: func() time.Time { return agentTestEpoch },
		NewEntryID: func() (string, error) {
			return fmt.Sprintf("reopened-manual-compaction-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("reopen manual compaction session: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	reopenedEntries, err := normalizeWorkflowEntries(reopened.Entries(), entryIDs)
	if err != nil {
		t.Fatalf("normalize reopened manual compaction entries: %v", err)
	}
	reopenedContext := reopened.BuildContext()
	reopenedMessages, err := normalizeWorkflowAgentMessages(reopenedContext.AgentMessages())
	if err != nil {
		t.Fatalf("normalize reopened manual compaction messages: %v", err)
	}
	reopenedModel, hasReopenedModel := reopenedContext.Model()
	reopenedThinking, hasReopenedThinking := reopenedContext.ThinkingLevel()
	if !hasReopenedModel || !hasReopenedThinking {
		t.Fatalf("reopened manual compaction selection = model:%t thinking:%t", hasReopenedModel, hasReopenedThinking)
	}

	actualScenario := map[string]any{
		"name":  scenario.Name,
		"input": scenario,
		"actions": map[string]any{
			"beforeCompact": beforeCompact,
			"compactReturn": normalizedCompactResult,
			"afterCompact":  afterCompact,
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
	if difference := workflowJSONDifference("manualCompactionScenario", expectedScenario, canonicalWorkflowJSON(t, actualScenario)); difference != "" {
		t.Fatalf("Go manual compaction workflow differs from pinned TypeScript oracle: %s", difference)
	}
	if difference := workflowJSONDifference(
		"manualCompactionScenario.session.header",
		expectedScenario["session"].(map[string]any)["header"],
		fileHeader,
	); difference != "" {
		t.Fatalf("physical manual compaction header differs from pinned TypeScript oracle: %s", difference)
	}
}

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

type upstreamQueueAbortScenario struct {
	Name          string            `json:"name"`
	SessionID     string            `json:"sessionId"`
	SystemPrompt  string            `json:"systemPrompt"`
	InitialPrompt string            `json:"initialPrompt"`
	SteeringMode  string            `json:"steeringMode"`
	FollowUpMode  string            `json:"followUpMode"`
	Recalled      upstreamQueuePair `json:"recalled"`
	Surviving     struct {
		Steering []string `json:"steering"`
		FollowUp []string `json:"followUp"`
	} `json:"surviving"`
	AbortError string `json:"abortError"`
	Responses  []struct {
		Text         string `json:"text"`
		InputTokens  uint64 `json:"inputTokens"`
		OutputTokens uint64 `json:"outputTokens"`
	} `json:"responses"`
}

type upstreamQueuePair struct {
	Steering string `json:"steering"`
	FollowUp string `json:"followUp"`
}

type upstreamQueueRunOutcome struct {
	result agent.Result
	err    error
}

// TestUpstreamAgentSessionQueueAbortOracle fixes the complete product-level
// queue contract against coding-agent: streaming prompt delivery, queue
// recall, mixed drain modes, abort continuation, final settlement, durable
// history, and reopen all run through the production CreateAgentSession path.
func TestUpstreamAgentSessionQueueAbortOracle(t *testing.T) {
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
	expectedScenario, ok := expectedObject["queueAbortScenario"].(map[string]any)
	if !ok {
		t.Fatal("workflow oracle queueAbortScenario is not an object")
	}
	scenario := corpus.QueueAbortScenario
	if scenario.SteeringMode != catalogmodel.QueueModeAll || scenario.FollowUpMode != catalogmodel.QueueModeOneAtATime ||
		len(scenario.Surviving.Steering) != 2 || len(scenario.Surviving.FollowUp) != 2 || len(scenario.Responses) != 3 {
		t.Fatal("queue/abort corpus no longer covers mixed queue modes and three continuation responses")
	}

	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	agentDir := filepath.Join(root, "agent")
	sessionDir := filepath.Join(root, "sessions")
	for _, directory := range []string{cwd, agentDir, sessionDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create queue/abort workflow directory: %v", err)
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
			return fmt.Sprintf("go-queue-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("create queue/abort session manager: %v", err)
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
		t.Fatalf("construct queue/abort provider: %v", err)
	}
	providerEntered := make(chan struct{})
	blocking, err := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		close(providerEntered)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	})
	if err != nil {
		t.Fatalf("construct blocking response: %v", err)
	}
	steps := []provider.ScriptStep{blocking}
	for index, response := range scenario.Responses {
		terminal, terminalErr := newAssistantTextMessage(
			[]llm.TextBlock{mustTextBlock(t, response.Text)},
			llm.FinishStop,
			mustUsage(t, response.InputTokens, response.OutputTokens),
			agentTestEpoch,
		)
		if terminalErr != nil {
			t.Fatalf("construct queue response %d: %v", index, terminalErr)
		}
		step, stepErr := provider.FixedResponseStep(terminal)
		if stepErr != nil {
			t.Fatalf("construct queue response step %d: %v", index, stepErr)
		}
		steps = append(steps, step)
	}
	if err := implementation.SetResponses(steps); err != nil {
		t.Fatalf("set queue/abort responses: %v", err)
	}

	disabled := false
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
			Transport:    provider.TransportSSE,
			SteeringMode: scenario.SteeringMode,
			FollowUpMode: scenario.FollowUpMode,
			Compaction:   catalogmodel.CompactionSettings{Enabled: &disabled},
			Retry:        catalogmodel.RetrySettings{Enabled: &disabled},
		},
		BaseConfig: agent.SessionConfig{
			SystemPrompt: scenario.SystemPrompt + "\nCurrent working directory: " + cwd,
			Stream: provider.StreamOptions{
				SessionID: scenario.SessionID,
				Transport: provider.TransportSSE,
			},
			Now:               func() time.Time { return agentTestEpoch },
			SettlementTimeout: time.Second,
		},
	})
	if err != nil {
		t.Fatalf("create Go queue/abort AgentSession: %v", err)
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
				"steering":            append([]string{}, queue.Steering...),
				"followUp":            append([]string{}, queue.FollowUp...),
				"pendingMessageCount": len(queue.SteeringMessages) + len(queue.FollowUpMessages),
			})
		}
		eventMu.Unlock()
	})

	runDone := make(chan upstreamQueueRunOutcome, 1)
	go func() {
		result, runErr := runtime.Prompt(context.Background(), scenario.InitialPrompt)
		runDone <- upstreamQueueRunOutcome{result: result, err: runErr}
	}()
	select {
	case <-providerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("queue/abort workflow did not enter the first provider call")
	}

	queued, err := runtime.PromptWithOptions(context.Background(), scenario.Recalled.Steering, agent.PromptOptions{
		StreamingBehavior: agent.StreamingSteer,
	})
	if err != nil || !queued.Handled() {
		t.Fatalf("queue recalled steering prompt = (%#v, %v)", queued, err)
	}
	queued, err = runtime.PromptWithOptions(context.Background(), scenario.Recalled.FollowUp, agent.PromptOptions{
		StreamingBehavior: agent.StreamingFollowUp,
	})
	if err != nil || !queued.Handled() {
		t.Fatalf("queue recalled follow-up prompt = (%#v, %v)", queued, err)
	}
	queueBeforeClear := upstreamQueueSnapshot(runtime)
	cleared := runtime.ClearQueue()
	clearResult := map[string]any{
		"steering": append([]string{}, cleared.Steering...),
		"followUp": append([]string{}, cleared.FollowUp...),
	}
	queueAfterClear := upstreamQueueSnapshot(runtime)

	for _, text := range scenario.Surviving.Steering {
		if err := runtime.Steer(text); err != nil {
			t.Fatalf("queue surviving steering %q: %v", text, err)
		}
	}
	for _, text := range scenario.Surviving.FollowUp {
		if err := runtime.FollowUp(text); err != nil {
			t.Fatalf("queue surviving follow-up %q: %v", text, err)
		}
	}
	queueBeforeAbort := upstreamQueueSnapshot(runtime)
	abortContext, cancelAbort := context.WithTimeout(context.Background(), 5*time.Second)
	if err := runtime.Abort(abortContext); err != nil {
		cancelAbort()
		t.Fatalf("abort queue workflow: %v", err)
	}
	cancelAbort()
	var runOutcome upstreamQueueRunOutcome
	select {
	case runOutcome = <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("initial queue/abort prompt did not return after settlement")
	}
	if runOutcome.err != nil {
		t.Fatalf("initial queue/abort prompt returned error: %v", runOutcome.err)
	}

	eventMu.Lock()
	events := append([]agent.SessionEvent(nil), observed...)
	settled := append([]any(nil), settledSnapshots...)
	eventMu.Unlock()
	abortActivity := runtime.Activity()
	abortQueue := runtime.PendingQueue()
	abortReturn := map[string]any{
		"isStreaming":         abortActivity.IsStreaming,
		"isIdle":              abortActivity.Phase == agent.PhaseIdle,
		"settledEventCount":   len(settled),
		"steering":            append([]string{}, abortQueue.Steering...),
		"followUp":            append([]string{}, abortQueue.FollowUp...),
		"pendingMessageCount": len(abortQueue.SteeringMessages) + len(abortQueue.FollowUpMessages),
	}
	if implementation.CallCount() != 4 || implementation.PendingResponses() != 0 {
		t.Fatalf("queue/abort provider calls/pending = %d/%d, want 4/0", implementation.CallCount(), implementation.PendingResponses())
	}
	if len(settled) != 1 {
		t.Fatalf("queue/abort settled events = %d, want 1", len(settled))
	}
	unsubscribe()

	entries := manager.Entries()
	entryIDs := workflowEntryIDs(entries)
	header := manager.Header()
	sessionFile, ok := manager.SessionFile()
	if !ok {
		t.Fatal("queue/abort manager has no persistent session file")
	}
	stats, err := runtime.GetSessionStats()
	if err != nil {
		t.Fatalf("queue/abort stats: %v", err)
	}
	providerInputs, err := normalizeWorkflowProviderInputs(implementation.Requests(), root, cwd)
	if err != nil {
		t.Fatalf("normalize queue/abort provider inputs: %v", err)
	}
	normalizedEvents, err := normalizeWorkflowEvents(events, entryIDs)
	if err != nil {
		t.Fatalf("normalize queue/abort events: %v", err)
	}
	state := runtime.State()
	selectedRef, selectedOK := runtime.SelectedModel()
	if !selectedOK {
		t.Fatal("queue/abort session lost its selected model")
	}
	finalMessages, err := normalizeWorkflowAgentMessages(state.Active.Messages())
	if err != nil {
		t.Fatalf("normalize final queue/abort messages: %v", err)
	}
	normalizedEntries, err := normalizeWorkflowEntries(entries, entryIDs)
	if err != nil {
		t.Fatalf("normalize queue/abort entries: %v", err)
	}
	finalActivity := runtime.Activity()
	finalActiveTools := append([]string{}, runtime.ActiveToolNames()...)
	finalSystemPrompt := runtime.SystemPrompt()
	finalThinkingLevel := runtime.ThinkingLevel()
	finalPendingMessageCount := runtime.PendingMessageCount()
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("close queue/abort runtime: %v", err)
	}
	runtimeOwned = false
	managerOwned = false

	fileHeader, fileEntries, err := normalizeWorkflowJSONL(sessionFile, entryIDs, root, cwd)
	if err != nil {
		t.Fatalf("normalize queue/abort JSONL: %v", err)
	}
	reopened, err := session.OpenSessionManagerWithOptions(sessionFile, sessionDir, "", session.ManagerOptions{
		Now: func() time.Time { return agentTestEpoch },
		NewEntryID: func() (string, error) {
			return fmt.Sprintf("reopened-queue-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("reopen queue/abort session: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	reopenedEntries, err := normalizeWorkflowEntries(reopened.Entries(), entryIDs)
	if err != nil {
		t.Fatalf("normalize reopened queue/abort entries: %v", err)
	}
	reopenedContext := reopened.BuildContext()
	reopenedMessages, err := normalizeWorkflowMessages(reopenedContext.Messages())
	if err != nil {
		t.Fatalf("normalize reopened queue/abort messages: %v", err)
	}
	reopenedModel, hasReopenedModel := reopenedContext.Model()
	reopenedThinking, hasReopenedThinking := reopenedContext.ThinkingLevel()
	if !hasReopenedModel || !hasReopenedThinking {
		t.Fatalf("reopened queue/abort selection = model:%t thinking:%t", hasReopenedModel, hasReopenedThinking)
	}

	actualScenario := map[string]any{
		"name":  scenario.Name,
		"input": scenario,
		"actions": map[string]any{
			"queueBeforeClear": queueBeforeClear,
			"clearResult":      clearResult,
			"queueAfterClear":  queueAfterClear,
			"queueBeforeAbort": queueBeforeAbort,
			"abortReturn":      abortReturn,
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
	if difference := workflowJSONDifference("queueAbortScenario", expectedScenario, canonicalWorkflowJSON(t, actualScenario)); difference != "" {
		t.Fatalf("Go queue/abort AgentSession workflow differs from pinned TypeScript oracle: %s", difference)
	}
	if difference := workflowJSONDifference(
		"queueAbortScenario.session.header",
		expectedScenario["session"].(map[string]any)["header"],
		fileHeader,
	); difference != "" {
		t.Fatalf("physical queue/abort header differs from pinned TypeScript oracle: %s", difference)
	}
}

func upstreamQueueSnapshot(runtime *agent.AgentSession) map[string]any {
	queue := runtime.PendingQueue()
	return map[string]any{
		"steering":            append([]string{}, queue.Steering...),
		"followUp":            append([]string{}, queue.FollowUp...),
		"pendingMessageCount": len(queue.SteeringMessages) + len(queue.FollowUpMessages),
	}
}

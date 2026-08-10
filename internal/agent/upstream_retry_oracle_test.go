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

type upstreamRetryScenario struct {
	Name         string `json:"name"`
	SessionID    string `json:"sessionId"`
	SystemPrompt string `json:"systemPrompt"`
	Prompt       string `json:"prompt"`
	MaxRetries   uint64 `json:"maxRetries"`
	BaseDelayMS  uint64 `json:"baseDelayMs"`
	RetryAfterMS uint64 `json:"retryAfterMs"`
	Failure      struct {
		Message    string `json:"message"`
		HTTPStatus int    `json:"httpStatus"`
	} `json:"failure"`
	Response struct {
		Text         string `json:"text"`
		InputTokens  uint64 `json:"inputTokens"`
		OutputTokens uint64 `json:"outputTokens"`
	} `json:"response"`
}

// TestUpstreamAgentSessionRetryOracle pins the product-level automatic retry
// contract. In particular, a provider Retry-After has already been consumed by
// the adapter and must not replace coding-agent's independent Agent retry
// backoff. The failed assistant remains durable while the live retry context
// excludes it.
func TestUpstreamAgentSessionRetryOracle(t *testing.T) {
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
	expectedScenario, ok := expectedObject["retryScenario"].(map[string]any)
	if !ok {
		t.Fatal("workflow oracle retryScenario is not an object")
	}
	scenario := corpus.RetryScenario
	if scenario.MaxRetries != 2 || scenario.BaseDelayMS == 0 || scenario.RetryAfterMS <= scenario.BaseDelayMS || scenario.Failure.HTTPStatus != 429 {
		t.Fatal("retry corpus no longer distinguishes Agent backoff from provider Retry-After")
	}

	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	agentDir := filepath.Join(root, "agent")
	sessionDir := filepath.Join(root, "sessions")
	for _, directory := range []string{cwd, agentDir, sessionDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create retry workflow directory: %v", err)
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
			return fmt.Sprintf("go-retry-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("create retry session manager: %v", err)
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
		t.Fatalf("construct retry provider: %v", err)
	}
	retryAfter := time.Duration(scenario.RetryAfterMS) * time.Millisecond
	providerFailure, err := provider.NewProviderFailure(provider.ProviderFailureSpec{
		Kind:       provider.FailureHTTPStatus,
		Message:    scenario.Failure.Message,
		Cause:      errors.New("fixture transient overload"),
		HTTPStatus: &scenario.Failure.HTTPStatus,
		RetryAfter: &retryAfter,
	})
	if err != nil {
		t.Fatalf("construct retry provider failure: %v", err)
	}
	failure, err := llm.NewFailure(scenario.Failure.Message, providerFailure)
	if err != nil {
		t.Fatalf("construct retry LLM failure: %v", err)
	}
	failed, err := newAssistantFailureMessageWithFailure(
		[]llm.TextBlock{mustTextBlock(t, "")},
		llm.FinishError,
		failure,
		llm.Usage{},
		agentTestEpoch,
	)
	if err != nil {
		t.Fatalf("construct retry failure response: %v", err)
	}
	recovered, err := newAssistantTextMessage(
		[]llm.TextBlock{mustTextBlock(t, scenario.Response.Text)},
		llm.FinishStop,
		mustUsage(t, scenario.Response.InputTokens, scenario.Response.OutputTokens),
		agentTestEpoch,
	)
	if err != nil {
		t.Fatalf("construct retry success response: %v", err)
	}
	failedStep, err := provider.FixedResponseStep(failed)
	if err != nil {
		t.Fatalf("construct retry failure step: %v", err)
	}
	recoveredStep, err := provider.FixedResponseStep(recovered)
	if err != nil {
		t.Fatalf("construct retry success step: %v", err)
	}
	if err := implementation.SetResponses([]provider.ScriptStep{failedStep, recoveredStep}); err != nil {
		t.Fatalf("set retry responses: %v", err)
	}

	enabled := true
	disabled := false
	maxRetries := scenario.MaxRetries
	baseDelayMS := scenario.BaseDelayMS
	providerMaxRetries := uint64(0)
	providerMaxRetryDelayMS := scenario.RetryAfterMS
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
			Transport:  provider.TransportSSE,
			Compaction: catalogmodel.CompactionSettings{Enabled: &disabled},
			Retry: catalogmodel.RetrySettings{
				Enabled: &enabled, MaxRetries: &maxRetries, BaseDelayMS: &baseDelayMS,
				Provider: catalogmodel.ProviderRetrySettings{
					MaxRetries: &providerMaxRetries, MaxRetryDelayMS: &providerMaxRetryDelayMS,
				},
			},
		},
		BaseConfig: agent.SessionConfig{
			SystemPrompt: scenario.SystemPrompt + "\nCurrent working directory: " + cwd,
			Stream: provider.StreamOptions{
				SessionID: scenario.SessionID,
				Transport: provider.TransportSSE,
			},
			Retry: agent.RetryPolicy{
				Sleep: func(context.Context, time.Duration) error { return nil },
			},
			Now:               func() time.Time { return agentTestEpoch },
			SettlementTimeout: time.Second,
		},
	})
	if err != nil {
		t.Fatalf("create Go retry AgentSession: %v", err)
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
				"isRetrying":          runtime.IsRetrying(),
				"steering":            append([]string{}, queue.Steering...),
				"followUp":            append([]string{}, queue.FollowUp...),
				"pendingMessageCount": len(queue.SteeringMessages) + len(queue.FollowUpMessages),
			})
		}
		eventMu.Unlock()
	})

	result, err := runtime.Prompt(context.Background(), scenario.Prompt)
	if err != nil || !result.Succeeded() {
		t.Fatalf("retry workflow prompt = (%#v, %v)", result, err)
	}
	if implementation.CallCount() != 2 || implementation.PendingResponses() != 0 {
		t.Fatalf("retry provider calls/pending = %d/%d, want 2/0", implementation.CallCount(), implementation.PendingResponses())
	}

	eventMu.Lock()
	events := append([]agent.SessionEvent(nil), observed...)
	settled := append([]any(nil), settledSnapshots...)
	eventMu.Unlock()
	activity := runtime.Activity()
	queue := runtime.PendingQueue()
	promptReturn := map[string]any{
		"isStreaming":         activity.IsStreaming,
		"isIdle":              activity.Phase == agent.PhaseIdle,
		"isRetrying":          runtime.IsRetrying(),
		"settledEventCount":   len(settled),
		"steering":            append([]string{}, queue.Steering...),
		"followUp":            append([]string{}, queue.FollowUp...),
		"pendingMessageCount": len(queue.SteeringMessages) + len(queue.FollowUpMessages),
	}
	if len(settled) != 1 {
		t.Fatalf("retry settled events = %d, want 1", len(settled))
	}
	unsubscribe()

	entries := manager.Entries()
	entryIDs := workflowEntryIDs(entries)
	header := manager.Header()
	sessionFile, ok := manager.SessionFile()
	if !ok {
		t.Fatal("retry manager has no persistent session file")
	}
	stats, err := runtime.GetSessionStats()
	if err != nil {
		t.Fatalf("retry stats: %v", err)
	}
	providerInputs, err := normalizeWorkflowProviderInputs(implementation.Requests(), root, cwd, scenario.SessionID)
	if err != nil {
		t.Fatalf("normalize retry provider inputs: %v", err)
	}
	normalizedEvents, err := normalizeWorkflowEvents(events, entryIDs)
	if err != nil {
		t.Fatalf("normalize retry events: %v", err)
	}
	state := runtime.State()
	selectedRef, selectedOK := runtime.SelectedModel()
	if !selectedOK {
		t.Fatal("retry session lost its selected model")
	}
	finalMessages, err := normalizeWorkflowAgentMessages(state.Active.Messages())
	if err != nil {
		t.Fatalf("normalize final retry messages: %v", err)
	}
	normalizedEntries, err := normalizeWorkflowEntries(entries, entryIDs)
	if err != nil {
		t.Fatalf("normalize retry entries: %v", err)
	}
	finalActivity := runtime.Activity()
	finalThinkingLevel := runtime.ThinkingLevel()
	finalActiveTools := append([]string{}, runtime.ActiveToolNames()...)
	finalSystemPrompt := runtime.SystemPrompt()
	finalPendingMessageCount := runtime.PendingMessageCount()
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("close retry runtime: %v", err)
	}
	runtimeOwned = false
	managerOwned = false

	fileHeader, fileEntries, err := normalizeWorkflowJSONL(sessionFile, entryIDs, root, cwd)
	if err != nil {
		t.Fatalf("normalize retry JSONL: %v", err)
	}
	reopened, err := session.OpenSessionManagerWithOptions(sessionFile, sessionDir, "", session.ManagerOptions{
		Now: func() time.Time { return agentTestEpoch },
		NewEntryID: func() (string, error) {
			return fmt.Sprintf("reopened-retry-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("reopen retry session: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	reopenedEntries, err := normalizeWorkflowEntries(reopened.Entries(), entryIDs)
	if err != nil {
		t.Fatalf("normalize reopened retry entries: %v", err)
	}
	reopenedContext := reopened.BuildContext()
	reopenedMessages, err := normalizeWorkflowAgentMessages(reopenedContext.AgentMessages())
	if err != nil {
		t.Fatalf("normalize reopened retry messages: %v", err)
	}
	reopenedModel, hasReopenedModel := reopenedContext.Model()
	reopenedThinking, hasReopenedThinking := reopenedContext.ThinkingLevel()
	if !hasReopenedModel || !hasReopenedThinking {
		t.Fatalf("reopened retry selection = model:%t thinking:%t", hasReopenedModel, hasReopenedThinking)
	}

	actualScenario := map[string]any{
		"name":  scenario.Name,
		"input": scenario,
		"actions": map[string]any{
			"promptReturn":     promptReturn,
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
	if difference := workflowJSONDifference("retryScenario", expectedScenario, canonicalWorkflowJSON(t, actualScenario)); difference != "" {
		t.Fatalf("Go retry AgentSession workflow differs from pinned TypeScript oracle: %s", difference)
	}
	if difference := workflowJSONDifference(
		"retryScenario.session.header",
		expectedScenario["session"].(map[string]any)["header"],
		fileHeader,
	); difference != "" {
		t.Fatalf("physical retry header differs from pinned TypeScript oracle: %s", difference)
	}
}

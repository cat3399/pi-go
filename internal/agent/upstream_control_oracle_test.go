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

type upstreamModelControlScenario struct {
	Name                 string                        `json:"name"`
	SessionID            string                        `json:"sessionId"`
	SystemPrompt         string                        `json:"systemPrompt"`
	InitialThinkingLevel provider.ThinkingLevel        `json:"initialThinkingLevel"`
	ClampRequest         provider.ThinkingLevel        `json:"clampRequest"`
	Models               []upstreamModelControlModel   `json:"models"`
	ScopedThinkingLevel  provider.ThinkingLevel        `json:"scopedThinkingLevel"`
	SteeringMode         string                        `json:"steeringMode"`
	FollowUpMode         string                        `json:"followUpMode"`
	Prompt               string                        `json:"prompt"`
	Response             upstreamControlOracleResponse `json:"response"`
}

type upstreamModelControlModel struct {
	ID               string                             `json:"id"`
	Name             string                             `json:"name"`
	Reasoning        bool                               `json:"reasoning"`
	ThinkingLevelMap map[provider.ThinkingLevel]*string `json:"thinkingLevelMap,omitempty"`
}

type upstreamControlOracleResponse struct {
	Text         string `json:"text"`
	InputTokens  uint64 `json:"inputTokens"`
	OutputTokens uint64 `json:"outputTokens"`
}

type upstreamRetryAbortScenario struct {
	Name         string `json:"name"`
	SessionID    string `json:"sessionId"`
	SystemPrompt string `json:"systemPrompt"`
	FirstPrompt  string `json:"firstPrompt"`
	SecondPrompt string `json:"secondPrompt"`
	MaxRetries   uint64 `json:"maxRetries"`
	BaseDelayMS  uint64 `json:"baseDelayMs"`
	Failure      struct {
		Message    string `json:"message"`
		HTTPStatus int    `json:"httpStatus"`
	} `json:"failure"`
	Response upstreamControlOracleResponse `json:"response"`
}

type upstreamControlSettingsState struct {
	DefaultProvider      string
	DefaultModel         string
	DefaultThinkingLevel provider.ThinkingLevel
	SteeringMode         agent.QueueMode
	FollowUpMode         agent.QueueMode
}

type upstreamControlRunOutcome struct {
	result agent.Result
	err    error
}

// TestUpstreamAgentSessionModelThinkingQueueControlOracle freezes the complete
// scoped model, thinking clamp/cycle, queue-mode setter, persistence, hook, and
// next-request behavior through both production createAgentSession paths.
func TestUpstreamAgentSessionModelThinkingQueueControlOracle(t *testing.T) {
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
	expectedScenario, ok := expectedObject["modelControlScenario"].(map[string]any)
	if !ok {
		t.Fatal("workflow oracle modelControlScenario is not an object")
	}
	scenario := corpus.ModelControl
	if len(scenario.Models) != 3 || !scenario.Models[0].Reasoning || scenario.Models[1].Reasoning || !scenario.Models[2].Reasoning {
		t.Fatal("model control corpus no longer contains reasoning, plain, and reasoning models in order")
	}

	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	agentDir := filepath.Join(root, "agent")
	sessionDir := filepath.Join(root, "sessions")
	for _, directory := range []string{cwd, agentDir, sessionDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create model control workflow directory: %v", err)
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
			return fmt.Sprintf("go-model-control-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("create model control session manager: %v", err)
	}
	managerOwned := true
	defer func() {
		if managerOwned {
			_ = manager.Close()
		}
	}()

	models := make([]catalogmodel.Model, len(scenario.Models))
	for index, input := range scenario.Models {
		models[index] = catalogmodel.Model{
			Provider: "faux", API: provider.AnthropicMessagesAPI, ID: input.ID, Name: input.Name,
			BaseURL: "http://localhost:0", Reasoning: input.Reasoning,
			ThinkingLevelMap: cloneUpstreamThinkingLevelMap(input.ThinkingLevelMap),
			Input:            []provider.InputKind{provider.InputText, provider.InputImage},
			Cost:             provider.CostRates{}, ContextWindow: 128_000, MaxTokens: 16_384,
		}
	}
	modelRefs := make([]provider.Model, len(models))
	for index, model := range models {
		modelRefs[index], err = model.Ref()
		if err != nil {
			t.Fatalf("construct model control model %d: %v", index, err)
		}
	}

	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{
		ChunkRunes: 3,
		Clock:      func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatalf("construct model control provider: %v", err)
	}
	terminal, err := newAssistantTextMessage(
		[]llm.TextBlock{mustTextBlock(t, scenario.Response.Text)},
		llm.FinishStop,
		mustUsage(t, scenario.Response.InputTokens, scenario.Response.OutputTokens),
		agentTestEpoch,
	)
	if err != nil {
		t.Fatalf("construct model control response: %v", err)
	}
	responseStep, err := provider.FixedResponseStep(terminal)
	if err != nil {
		t.Fatalf("construct model control response step: %v", err)
	}
	if err := implementation.SetResponses([]provider.ScriptStep{responseStep}); err != nil {
		t.Fatalf("set model control response: %v", err)
	}

	settingsState := upstreamControlSettingsState{
		SteeringMode: agent.QueueOneAtATime,
		FollowUpMode: agent.QueueAll,
	}
	var settingsMu sync.Mutex
	persistSettings := func(_ context.Context, update agent.SettingsUpdate) (agent.SettingsWriteResult, error) {
		settingsMu.Lock()
		before := settingsState
		applyUpstreamControlSettings(&settingsState, update)
		settingsMu.Unlock()
		return agent.SettingsWriteResult{Undo: func(context.Context) error {
			settingsMu.Lock()
			settingsState = before
			settingsMu.Unlock()
			return nil
		}}, nil
	}

	var controlMu sync.Mutex
	var controlActions []string
	hooks := agent.Hooks{
		ModelSelect: func(_ context.Context, event agent.ModelSelectEvent) error {
			previous := "none"
			if event.PreviousModel != nil {
				previous = event.PreviousModel.ID()
			}
			controlMu.Lock()
			controlActions = append(controlActions, fmt.Sprintf("model_select:%s->%s:%s", previous, event.Model.ID(), event.Source))
			controlMu.Unlock()
			return nil
		},
		ThinkingLevelSelect: func(_ context.Context, event agent.ThinkingLevelSelectEvent) error {
			controlMu.Lock()
			controlActions = append(controlActions, fmt.Sprintf("thinking_level_select:%s->%s", event.PreviousLevel, event.Level))
			controlMu.Unlock()
			return nil
		},
	}
	disabled := false
	initialThinking := scenario.InitialThinkingLevel
	scopedThinking := scenario.ScopedThinkingLevel
	created, err := agentruntime.CreateAgentSession(context.Background(), agentruntime.SessionFactoryOptions{
		Services: &agentruntime.Services{CWD: cwd, AgentDir: agentDir, Provider: implementation},
		Provider: implementation, SessionManager: manager,
		AllModels: models,
		Availability: catalogmodel.Availability{
			HasConfiguredAuth: func(providerID string) bool { return providerID == "faux" },
			SupportsRoute:     func(catalogmodel.Model) bool { return true },
		},
		ExplicitModel:         &models[0],
		ExplicitThinkingLevel: &initialThinking,
		ScopedModels: []catalogmodel.ScopedModel{
			{Model: models[0]},
			{Model: models[1]},
			{Model: models[2], ThinkingLevel: &scopedThinking},
		},
		Settings: catalogmodel.Settings{
			Transport:    provider.TransportSSE,
			SteeringMode: catalogmodel.QueueModeOneAtATime,
			FollowUpMode: catalogmodel.QueueModeAll,
			Compaction:   catalogmodel.CompactionSettings{Enabled: &disabled},
			Retry:        catalogmodel.RetrySettings{Enabled: &disabled},
		},
		PersistSettings: persistSettings,
		BaseConfig: agent.SessionConfig{
			SystemPrompt: scenario.SystemPrompt + "\nCurrent working directory: " + cwd,
			Stream: provider.StreamOptions{
				SessionID: scenario.SessionID,
				Transport: provider.TransportSSE,
			},
			Hooks:             hooks,
			Now:               func() time.Time { return agentTestEpoch },
			SettlementTimeout: time.Second,
		},
	})
	if err != nil {
		t.Fatalf("create Go model control AgentSession: %v", err)
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

	initial := upstreamModelControlSnapshot(t, runtime)
	if err := runtime.SetThinkingLevel(scenario.ClampRequest); err != nil {
		t.Fatalf("clamp model control thinking: %v", err)
	}
	afterClamp := upstreamModelControlSnapshot(t, runtime)
	thinkingCycleRef, err := runtime.CycleThinkingLevel()
	if err != nil {
		t.Fatalf("cycle model control thinking: %v", err)
	}
	var thinkingCycle any
	if thinkingCycleRef != nil {
		thinkingCycle = string(*thinkingCycleRef)
	}
	afterThinkingCycle := upstreamModelControlSnapshot(t, runtime)
	scopedPlainRef, err := runtime.CycleModel(context.Background(), agent.CycleForward)
	if err != nil {
		t.Fatalf("cycle to scoped plain model: %v", err)
	}
	scopedPlain := normalizeUpstreamModelCycle(scopedPlainRef)
	afterScopedPlain := upstreamModelControlSnapshot(t, runtime)
	plainThinkingRef, err := runtime.CycleThinkingLevel()
	if err != nil {
		t.Fatalf("cycle plain-model thinking: %v", err)
	}
	var plainThinkingCycle any
	if plainThinkingRef != nil {
		plainThinkingCycle = string(*plainThinkingRef)
	}
	scopedReasoningRef, err := runtime.CycleModel(context.Background(), agent.CycleForward)
	if err != nil {
		t.Fatalf("cycle to scoped reasoning model: %v", err)
	}
	scopedReasoning := normalizeUpstreamModelCycle(scopedReasoningRef)
	afterScopedReasoning := upstreamModelControlSnapshot(t, runtime)
	if err := runtime.SetModel(modelRefs[0]); err != nil {
		t.Fatalf("direct-set initial model: %v", err)
	}
	afterDirectSet := upstreamModelControlSnapshot(t, runtime)
	if err := runtime.SetSteeringMode(upstreamOracleQueueMode(t, scenario.SteeringMode)); err != nil {
		t.Fatalf("set steering mode: %v", err)
	}
	if err := runtime.SetFollowUpMode(upstreamOracleQueueMode(t, scenario.FollowUpMode)); err != nil {
		t.Fatalf("set follow-up mode: %v", err)
	}
	afterQueueModes := upstreamModelControlSnapshot(t, runtime)
	settingsMu.Lock()
	settings := normalizeUpstreamControlSettings(settingsState)
	settingsMu.Unlock()
	result, err := runtime.Prompt(context.Background(), scenario.Prompt)
	if err != nil || !result.Succeeded() {
		t.Fatalf("model control prompt = (%#v, %v)", result, err)
	}
	promptActivity := runtime.Activity()
	promptQueue := runtime.PendingQueue()
	promptReturn := map[string]any{
		"isStreaming":         promptActivity.IsStreaming,
		"isIdle":              promptActivity.Phase == agent.PhaseIdle,
		"steering":            append([]string{}, promptQueue.Steering...),
		"followUp":            append([]string{}, promptQueue.FollowUp...),
		"pendingMessageCount": len(promptQueue.SteeringMessages) + len(promptQueue.FollowUpMessages),
	}
	if implementation.CallCount() != 1 || implementation.PendingResponses() != 0 {
		t.Fatalf("model control provider calls/pending = %d/%d, want 1/0", implementation.CallCount(), implementation.PendingResponses())
	}

	unsubscribe()
	eventMu.Lock()
	events := append([]agent.SessionEvent(nil), observed...)
	eventMu.Unlock()
	controlMu.Lock()
	normalizedControlActions := append([]string(nil), controlActions...)
	controlMu.Unlock()
	entries := manager.Entries()
	entryIDs := workflowEntryIDs(entries)
	header := manager.Header()
	sessionFile, ok := manager.SessionFile()
	if !ok {
		t.Fatal("model control manager has no persistent session file")
	}
	stats, err := runtime.GetSessionStats()
	if err != nil {
		t.Fatalf("model control stats: %v", err)
	}
	providerInputs, err := normalizeWorkflowProviderInputs(implementation.Requests(), root, cwd, scenario.SessionID)
	if err != nil {
		t.Fatalf("normalize model control provider inputs: %v", err)
	}
	normalizedEvents, err := normalizeWorkflowEvents(events, entryIDs)
	if err != nil {
		t.Fatalf("normalize model control events: %v", err)
	}
	normalizedEntries, err := normalizeWorkflowEntries(entries, entryIDs)
	if err != nil {
		t.Fatalf("normalize model control entries: %v", err)
	}
	state := runtime.State()
	selected, selectedOK := runtime.SelectedModel()
	if !selectedOK {
		t.Fatal("model control session lost its selected model")
	}
	finalMessages, err := normalizeWorkflowAgentMessages(state.Active.Messages())
	if err != nil {
		t.Fatalf("normalize final model control messages: %v", err)
	}
	finalActivity := runtime.Activity()
	finalThinking := runtime.ThinkingLevel()
	finalActiveTools := append([]string{}, runtime.ActiveToolNames()...)
	finalSystemPrompt := runtime.SystemPrompt()
	finalPendingMessageCount := runtime.PendingMessageCount()
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("close model control runtime: %v", err)
	}
	runtimeOwned = false
	managerOwned = false

	fileHeader, fileEntries, err := normalizeWorkflowJSONL(sessionFile, entryIDs, root, cwd)
	if err != nil {
		t.Fatalf("normalize model control JSONL: %v", err)
	}
	reopened, err := session.OpenSessionManagerWithOptions(sessionFile, sessionDir, "", session.ManagerOptions{
		Now: func() time.Time { return agentTestEpoch },
		NewEntryID: func() (string, error) {
			return fmt.Sprintf("reopened-model-control-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("reopen model control session: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	reopenedEntries, err := normalizeWorkflowEntries(reopened.Entries(), entryIDs)
	if err != nil {
		t.Fatalf("normalize reopened model control entries: %v", err)
	}
	reopenedContext := reopened.BuildContext()
	reopenedMessages, err := normalizeWorkflowAgentMessages(reopenedContext.AgentMessages())
	if err != nil {
		t.Fatalf("normalize reopened model control messages: %v", err)
	}
	reopenedModel, hasReopenedModel := reopenedContext.Model()
	reopenedThinking, hasReopenedThinking := reopenedContext.ThinkingLevel()
	if !hasReopenedModel || !hasReopenedThinking {
		t.Fatalf("reopened model control selection = model:%t thinking:%t", hasReopenedModel, hasReopenedThinking)
	}

	actualScenario := map[string]any{
		"name":  scenario.Name,
		"input": scenario,
		"actions": map[string]any{
			"initial":              initial,
			"afterClamp":           afterClamp,
			"thinkingCycle":        thinkingCycle,
			"afterThinkingCycle":   afterThinkingCycle,
			"scopedPlain":          scopedPlain,
			"afterScopedPlain":     afterScopedPlain,
			"plainThinkingCycle":   plainThinkingCycle,
			"scopedReasoning":      scopedReasoning,
			"afterScopedReasoning": afterScopedReasoning,
			"afterDirectSet":       afterDirectSet,
			"afterQueueModes":      afterQueueModes,
			"settings":             settings,
			"promptReturn":         promptReturn,
			"controlActions":       normalizedControlActions,
		},
		"providerInputs": providerInputs,
		"events":         normalizedEvents,
		"finalState": map[string]any{
			"isStreaming":         finalActivity.IsStreaming,
			"pendingMessageCount": finalPendingMessageCount,
			"model": map[string]any{
				"provider": selected.Provider(), "api": selected.API(), "id": selected.ID(),
			},
			"thinkingLevel": string(finalThinking),
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
	if difference := workflowJSONDifference("modelControlScenario", expectedScenario, canonicalWorkflowJSON(t, actualScenario)); difference != "" {
		t.Fatalf("Go model/thinking/queue control workflow differs from pinned TypeScript oracle: %s", difference)
	}
	if difference := workflowJSONDifference(
		"modelControlScenario.session.header",
		expectedScenario["session"].(map[string]any)["header"],
		fileHeader,
	); difference != "" {
		t.Fatalf("physical model control header differs from pinned TypeScript oracle: %s", difference)
	}
}

// TestUpstreamAgentSessionAbortRetryOracle fixes the cancellation boundary:
// abortRetry ends only the active retry delay, emits one cleanup event, settles
// the failed run, and leaves the same AgentSession usable for a later prompt.
func TestUpstreamAgentSessionAbortRetryOracle(t *testing.T) {
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
	expectedScenario, ok := expectedObject["retryAbortScenario"].(map[string]any)
	if !ok {
		t.Fatal("workflow oracle retryAbortScenario is not an object")
	}
	scenario := corpus.RetryAbort
	if scenario.MaxRetries != 2 || scenario.BaseDelayMS < 60_000 || scenario.Failure.HTTPStatus != 429 {
		t.Fatal("retry cancellation corpus no longer guarantees an observable transient retry delay")
	}

	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	agentDir := filepath.Join(root, "agent")
	sessionDir := filepath.Join(root, "sessions")
	for _, directory := range []string{cwd, agentDir, sessionDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create retry cancellation workflow directory: %v", err)
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
			return fmt.Sprintf("go-retry-abort-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("create retry cancellation session manager: %v", err)
	}
	managerOwned := true
	defer func() {
		if managerOwned {
			_ = manager.Close()
		}
	}()

	selected := catalogmodel.Model{
		Provider: "faux", API: provider.AnthropicMessagesAPI, ID: "faux-1", Name: "Faux Model",
		BaseURL: "http://localhost:0", Input: []provider.InputKind{provider.InputText, provider.InputImage},
		Cost: provider.CostRates{}, ContextWindow: 128_000, MaxTokens: 16_384,
	}
	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{
		ChunkRunes: 3,
		Clock:      func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatalf("construct retry cancellation provider: %v", err)
	}
	providerFailure, err := provider.NewProviderFailure(provider.ProviderFailureSpec{
		Kind: provider.FailureHTTPStatus, Message: scenario.Failure.Message,
		Cause: errors.New("fixture transient overload"), HTTPStatus: &scenario.Failure.HTTPStatus,
	})
	if err != nil {
		t.Fatalf("construct retry cancellation provider failure: %v", err)
	}
	failure, err := llm.NewFailure(scenario.Failure.Message, providerFailure)
	if err != nil {
		t.Fatalf("construct retry cancellation LLM failure: %v", err)
	}
	failed, err := newAssistantFailureMessageWithFailure(
		[]llm.TextBlock{mustTextBlock(t, "")},
		llm.FinishError,
		failure,
		llm.Usage{},
		agentTestEpoch,
	)
	if err != nil {
		t.Fatalf("construct retry cancellation failure response: %v", err)
	}
	recovered, err := newAssistantTextMessage(
		[]llm.TextBlock{mustTextBlock(t, scenario.Response.Text)},
		llm.FinishStop,
		mustUsage(t, scenario.Response.InputTokens, scenario.Response.OutputTokens),
		agentTestEpoch,
	)
	if err != nil {
		t.Fatalf("construct retry cancellation success response: %v", err)
	}
	failedStep, err := provider.FixedResponseStep(failed)
	if err != nil {
		t.Fatalf("construct retry cancellation failure step: %v", err)
	}
	recoveredStep, err := provider.FixedResponseStep(recovered)
	if err != nil {
		t.Fatalf("construct retry cancellation success step: %v", err)
	}
	if err := implementation.SetResponses([]provider.ScriptStep{failedStep, recoveredStep}); err != nil {
		t.Fatalf("set retry cancellation responses: %v", err)
	}

	retrySleepEntered := make(chan struct{})
	var retrySleepOnce sync.Once
	enabled := true
	disabled := false
	maxRetries := scenario.MaxRetries
	baseDelayMS := scenario.BaseDelayMS
	providerMaxRetries := uint64(0)
	off := provider.ThinkingOff
	created, err := agentruntime.CreateAgentSession(context.Background(), agentruntime.SessionFactoryOptions{
		Services: &agentruntime.Services{CWD: cwd, AgentDir: agentDir, Provider: implementation},
		Provider: implementation, SessionManager: manager,
		AllModels: []catalogmodel.Model{selected},
		Availability: catalogmodel.Availability{
			HasConfiguredAuth: func(providerID string) bool { return providerID == "faux" },
			SupportsRoute:     func(catalogmodel.Model) bool { return true },
		},
		ExplicitModel:         &selected,
		ExplicitThinkingLevel: &off,
		Settings: catalogmodel.Settings{
			Transport:  provider.TransportSSE,
			Compaction: catalogmodel.CompactionSettings{Enabled: &disabled},
			Retry: catalogmodel.RetrySettings{
				Enabled: &enabled, MaxRetries: &maxRetries, BaseDelayMS: &baseDelayMS,
				Provider: catalogmodel.ProviderRetrySettings{MaxRetries: &providerMaxRetries},
			},
		},
		BaseConfig: agent.SessionConfig{
			SystemPrompt: scenario.SystemPrompt + "\nCurrent working directory: " + cwd,
			Stream: provider.StreamOptions{
				SessionID: scenario.SessionID,
				Transport: provider.TransportSSE,
			},
			Retry: agent.RetryPolicy{Sleep: func(ctx context.Context, _ time.Duration) error {
				retrySleepOnce.Do(func() { close(retrySleepEntered) })
				<-ctx.Done()
				return context.Cause(ctx)
			}},
			Now:               func() time.Time { return agentTestEpoch },
			SettlementTimeout: time.Second,
		},
	})
	if err != nil {
		t.Fatalf("create Go retry cancellation AgentSession: %v", err)
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

	firstDone := make(chan upstreamControlRunOutcome, 1)
	go func() {
		result, runErr := runtime.Prompt(context.Background(), scenario.FirstPrompt)
		firstDone <- upstreamControlRunOutcome{result: result, err: runErr}
	}()
	select {
	case <-retrySleepEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("retry cancellation workflow did not enter retry sleep")
	}
	beforeActivity := runtime.Activity()
	beforeQueue := runtime.PendingQueue()
	beforeAbortRetry := map[string]any{
		"isStreaming":         beforeActivity.IsStreaming,
		"isIdle":              beforeActivity.Phase == agent.PhaseIdle,
		"isRetrying":          runtime.IsRetrying(),
		"steering":            append([]string{}, beforeQueue.Steering...),
		"followUp":            append([]string{}, beforeQueue.FollowUp...),
		"pendingMessageCount": len(beforeQueue.SteeringMessages) + len(beforeQueue.FollowUpMessages),
	}
	runtime.AbortRetry()
	runtime.AbortRetry()
	var first upstreamControlRunOutcome
	select {
	case first = <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("retry cancellation prompt did not settle")
	}
	if first.err != nil || first.result.Succeeded() {
		t.Fatalf("retry cancellation first prompt = (%#v, %v)", first.result, first.err)
	}
	eventMu.Lock()
	firstSettledCount := len(settledSnapshots)
	eventMu.Unlock()
	firstActivity := runtime.Activity()
	firstQueue := runtime.PendingQueue()
	firstPromptReturn := map[string]any{
		"isStreaming":         firstActivity.IsStreaming,
		"isIdle":              firstActivity.Phase == agent.PhaseIdle,
		"isRetrying":          runtime.IsRetrying(),
		"settledEventCount":   firstSettledCount,
		"steering":            append([]string{}, firstQueue.Steering...),
		"followUp":            append([]string{}, firstQueue.FollowUp...),
		"pendingMessageCount": len(firstQueue.SteeringMessages) + len(firstQueue.FollowUpMessages),
	}
	second, err := runtime.Prompt(context.Background(), scenario.SecondPrompt)
	if err != nil || !second.Succeeded() {
		t.Fatalf("retry cancellation second prompt = (%#v, %v)", second, err)
	}
	eventMu.Lock()
	secondSettledCount := len(settledSnapshots)
	eventMu.Unlock()
	secondActivity := runtime.Activity()
	secondQueue := runtime.PendingQueue()
	secondPromptReturn := map[string]any{
		"isStreaming":         secondActivity.IsStreaming,
		"isIdle":              secondActivity.Phase == agent.PhaseIdle,
		"isRetrying":          runtime.IsRetrying(),
		"settledEventCount":   secondSettledCount,
		"steering":            append([]string{}, secondQueue.Steering...),
		"followUp":            append([]string{}, secondQueue.FollowUp...),
		"pendingMessageCount": len(secondQueue.SteeringMessages) + len(secondQueue.FollowUpMessages),
	}
	if implementation.CallCount() != 2 || implementation.PendingResponses() != 0 {
		t.Fatalf("retry cancellation provider calls/pending = %d/%d, want 2/0", implementation.CallCount(), implementation.PendingResponses())
	}

	unsubscribe()
	eventMu.Lock()
	events := append([]agent.SessionEvent(nil), observed...)
	settled := append([]any(nil), settledSnapshots...)
	eventMu.Unlock()
	if len(settled) != 2 {
		t.Fatalf("retry cancellation settled events = %d, want 2", len(settled))
	}
	entries := manager.Entries()
	entryIDs := workflowEntryIDs(entries)
	header := manager.Header()
	sessionFile, ok := manager.SessionFile()
	if !ok {
		t.Fatal("retry cancellation manager has no persistent session file")
	}
	stats, err := runtime.GetSessionStats()
	if err != nil {
		t.Fatalf("retry cancellation stats: %v", err)
	}
	providerInputs, err := normalizeWorkflowProviderInputs(implementation.Requests(), root, cwd, scenario.SessionID)
	if err != nil {
		t.Fatalf("normalize retry cancellation provider inputs: %v", err)
	}
	normalizedEvents, err := normalizeWorkflowEvents(events, entryIDs)
	if err != nil {
		t.Fatalf("normalize retry cancellation events: %v", err)
	}
	normalizedEntries, err := normalizeWorkflowEntries(entries, entryIDs)
	if err != nil {
		t.Fatalf("normalize retry cancellation entries: %v", err)
	}
	state := runtime.State()
	selectedRef, selectedOK := runtime.SelectedModel()
	if !selectedOK {
		t.Fatal("retry cancellation session lost its selected model")
	}
	finalMessages, err := normalizeWorkflowAgentMessages(state.Active.Messages())
	if err != nil {
		t.Fatalf("normalize final retry cancellation messages: %v", err)
	}
	finalActivity := runtime.Activity()
	finalThinking := runtime.ThinkingLevel()
	finalActiveTools := append([]string{}, runtime.ActiveToolNames()...)
	finalSystemPrompt := runtime.SystemPrompt()
	finalPendingMessageCount := runtime.PendingMessageCount()
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("close retry cancellation runtime: %v", err)
	}
	runtimeOwned = false
	managerOwned = false

	fileHeader, fileEntries, err := normalizeWorkflowJSONL(sessionFile, entryIDs, root, cwd)
	if err != nil {
		t.Fatalf("normalize retry cancellation JSONL: %v", err)
	}
	reopened, err := session.OpenSessionManagerWithOptions(sessionFile, sessionDir, "", session.ManagerOptions{
		Now: func() time.Time { return agentTestEpoch },
		NewEntryID: func() (string, error) {
			return fmt.Sprintf("reopened-retry-abort-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("reopen retry cancellation session: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	reopenedEntries, err := normalizeWorkflowEntries(reopened.Entries(), entryIDs)
	if err != nil {
		t.Fatalf("normalize reopened retry cancellation entries: %v", err)
	}
	reopenedContext := reopened.BuildContext()
	reopenedMessages, err := normalizeWorkflowAgentMessages(reopenedContext.AgentMessages())
	if err != nil {
		t.Fatalf("normalize reopened retry cancellation messages: %v", err)
	}
	reopenedModel, hasReopenedModel := reopenedContext.Model()
	reopenedThinking, hasReopenedThinking := reopenedContext.ThinkingLevel()
	if !hasReopenedModel || !hasReopenedThinking {
		t.Fatalf("reopened retry cancellation selection = model:%t thinking:%t", hasReopenedModel, hasReopenedThinking)
	}

	actualScenario := map[string]any{
		"name":  scenario.Name,
		"input": scenario,
		"actions": map[string]any{
			"beforeAbortRetry":   beforeAbortRetry,
			"firstPromptReturn":  firstPromptReturn,
			"secondPromptReturn": secondPromptReturn,
			"settledSnapshots":   settled,
		},
		"providerInputs": providerInputs,
		"events":         normalizedEvents,
		"finalState": map[string]any{
			"isStreaming":         finalActivity.IsStreaming,
			"pendingMessageCount": finalPendingMessageCount,
			"model": map[string]any{
				"provider": selectedRef.Provider(), "api": selectedRef.API(), "id": selectedRef.ID(),
			},
			"thinkingLevel": string(finalThinking),
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
	if difference := workflowJSONDifference("retryAbortScenario", expectedScenario, canonicalWorkflowJSON(t, actualScenario)); difference != "" {
		t.Fatalf("Go retry cancellation workflow differs from pinned TypeScript oracle: %s", difference)
	}
	if difference := workflowJSONDifference(
		"retryAbortScenario.session.header",
		expectedScenario["session"].(map[string]any)["header"],
		fileHeader,
	); difference != "" {
		t.Fatalf("physical retry cancellation header differs from pinned TypeScript oracle: %s", difference)
	}
}

func cloneUpstreamThinkingLevelMap(values map[provider.ThinkingLevel]*string) map[provider.ThinkingLevel]*string {
	if values == nil {
		return nil
	}
	result := make(map[provider.ThinkingLevel]*string, len(values))
	for level, value := range values {
		if value == nil {
			result[level] = nil
			continue
		}
		copy := *value
		result[level] = &copy
	}
	return result
}

func applyUpstreamControlSettings(state *upstreamControlSettingsState, update agent.SettingsUpdate) {
	if update.DefaultProvider != nil {
		state.DefaultProvider = *update.DefaultProvider
	}
	if update.DefaultModel != nil {
		state.DefaultModel = *update.DefaultModel
	}
	if update.DefaultThinkingLevel != nil {
		state.DefaultThinkingLevel = *update.DefaultThinkingLevel
	}
	if update.SteeringMode != nil {
		state.SteeringMode = *update.SteeringMode
	}
	if update.FollowUpMode != nil {
		state.FollowUpMode = *update.FollowUpMode
	}
}

func normalizeUpstreamControlSettings(state upstreamControlSettingsState) map[string]any {
	return map[string]any{
		"defaultProvider":      state.DefaultProvider,
		"defaultModel":         state.DefaultModel,
		"defaultThinkingLevel": string(state.DefaultThinkingLevel),
		"steeringMode":         state.SteeringMode.String(),
		"followUpMode":         state.FollowUpMode.String(),
	}
}

func upstreamModelControlSnapshot(t *testing.T, runtime *agent.AgentSession) map[string]any {
	t.Helper()
	selected, ok := runtime.SelectedModel()
	if !ok {
		t.Fatal("model control snapshot has no selected model")
	}
	scoped := runtime.ScopedModels()
	normalizedScoped := make([]any, 0, len(scoped))
	for _, candidate := range scoped {
		var thinking any
		if candidate.ThinkingLevel != nil {
			thinking = string(*candidate.ThinkingLevel)
		}
		normalizedScoped = append(normalizedScoped, map[string]any{
			"model": map[string]any{
				"provider": candidate.Model.Provider(), "api": candidate.Model.API(), "id": candidate.Model.ID(),
			},
			"thinkingLevel": thinking,
		})
	}
	levels := runtime.AvailableThinkingLevels()
	normalizedLevels := make([]string, len(levels))
	for index, level := range levels {
		normalizedLevels[index] = string(level)
	}
	return map[string]any{
		"model": map[string]any{
			"provider": selected.Provider(), "api": selected.API(), "id": selected.ID(),
		},
		"thinkingLevel":           string(runtime.ThinkingLevel()),
		"availableThinkingLevels": normalizedLevels,
		"supportsThinking":        runtime.SupportsThinking(),
		"steeringMode":            runtime.SteeringMode().String(),
		"followUpMode":            runtime.FollowUpMode().String(),
		"scopedModels":            normalizedScoped,
	}
}

func normalizeUpstreamModelCycle(result *agent.ModelCycleResult) any {
	if result == nil {
		return nil
	}
	return map[string]any{
		"model": map[string]any{
			"provider": result.Model.Provider(), "api": result.Model.API(), "id": result.Model.ID(),
		},
		"thinkingLevel": string(result.ThinkingLevel),
		"isScoped":      result.IsScoped,
	}
}

func upstreamOracleQueueMode(t *testing.T, value string) agent.QueueMode {
	t.Helper()
	switch value {
	case "all":
		return agent.QueueAll
	case "one-at-a-time":
		return agent.QueueOneAtATime
	default:
		t.Fatalf("invalid oracle queue mode %q", value)
		return agent.QueueOneAtATime
	}
}

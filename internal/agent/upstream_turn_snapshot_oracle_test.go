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

type upstreamTurnSnapshotScenario struct {
	Name                 string                         `json:"name"`
	SessionID            string                         `json:"sessionId"`
	InitialSystemPrompt  string                         `json:"initialSystemPrompt"`
	ReloadedSystemPrompt string                         `json:"reloadedSystemPrompt"`
	InitialPrompt        string                         `json:"initialPrompt"`
	PostReloadPrompt     string                         `json:"postReloadPrompt"`
	InitialThinkingLevel string                         `json:"initialThinkingLevel"`
	NextThinkingLevel    string                         `json:"nextThinkingLevel"`
	InitialModel         upstreamTurnSnapshotModel      `json:"initialModel"`
	NextModel            upstreamTurnSnapshotModel      `json:"nextModel"`
	InitialTool          upstreamTurnSnapshotTool       `json:"initialTool"`
	NextTool             upstreamTurnSnapshotTool       `json:"nextTool"`
	Responses            []upstreamTurnSnapshotResponse `json:"responses"`
}

type upstreamTurnSnapshotModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type upstreamTurnSnapshotTool struct {
	Name        string `json:"name"`
	CallID      string `json:"callId,omitempty"`
	Description string `json:"description"`
	Result      string `json:"result,omitempty"`
}

type upstreamTurnSnapshotResponse struct {
	Text         string `json:"text"`
	ToolCall     bool   `json:"toolCall"`
	InputTokens  uint64 `json:"inputTokens"`
	OutputTokens uint64 `json:"outputTokens"`
}

type upstreamTurnSnapshotActions struct {
	mu      sync.Mutex
	entries []string
}

func (a *upstreamTurnSnapshotActions) append(value string) {
	a.mu.Lock()
	a.entries = append(a.entries, value)
	a.mu.Unlock()
}

func (a *upstreamTurnSnapshotActions) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string{}, a.entries...)
}

type upstreamTurnSnapshotResources struct {
	mu         sync.Mutex
	generation int
	cwd        string
	initial    string
	reloaded   string
	actions    *upstreamTurnSnapshotActions
}

func (r *upstreamTurnSnapshotResources) BuildSystemPrompt(names []string) (string, agent.BuildSystemPromptOptions, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	custom := r.initial
	if r.generation > 0 {
		custom = r.reloaded
	}
	return custom + "\nCurrent working directory: " + r.cwd, agent.BuildSystemPromptOptions{
		CWD: r.cwd, CustomPrompt: &custom, SelectedTools: append([]string(nil), names...),
	}, nil
}

func (r *upstreamTurnSnapshotResources) ExpandPromptInput(text string) (string, error) {
	return text, nil
}

func (r *upstreamTurnSnapshotResources) Reload(context.Context) error {
	r.actions.append("resource_reload")
	r.mu.Lock()
	r.generation++
	r.mu.Unlock()
	return nil
}

func (r *upstreamTurnSnapshotResources) Generation() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.generation
}

type upstreamTurnSnapshotRunOutcome struct {
	result agent.Result
	err    error
}

type upstreamTurnSnapshotExecutor struct {
	initialName   string
	initialResult string
	nextName      string
	calls         atomic.Uint32
	mu            sync.Mutex
	runs          []any
}

func (*upstreamTurnSnapshotExecutor) Name() string { return "turn-snapshot-catalog" }

func (e *upstreamTurnSnapshotExecutor) Supports(name string) bool {
	return name == e.initialName || name == e.nextName
}

func (e *upstreamTurnSnapshotExecutor) Execute(_ context.Context, toolCallID string, _ []byte, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return e.execute(toolCallID, e.initialName)
}

func (e *upstreamTurnSnapshotExecutor) ExecuteNamed(_ context.Context, toolCallID, name string, _ []byte, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return e.execute(toolCallID, name)
}

func (e *upstreamTurnSnapshotExecutor) execute(toolCallID, name string) (agent.ToolOutput, error) {
	e.calls.Add(1)
	e.mu.Lock()
	e.runs = append(e.runs, map[string]any{"toolCallId": toolCallID, "toolName": name})
	e.mu.Unlock()
	if name == e.initialName {
		return agent.ToolOutput{Text: e.initialResult, Details: map[string]any{"generation": "initial"}}, nil
	}
	return agent.ToolOutput{Text: "next snapshot tool executed", Details: map[string]any{"generation": "next"}}, nil
}

func (e *upstreamTurnSnapshotExecutor) CallCount() uint32 { return e.calls.Load() }

func (e *upstreamTurnSnapshotExecutor) Runs() []any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]any(nil), e.runs...)
}

// TestUpstreamAgentSessionTurnSnapshotOracle pins the production boundary used
// by every future surface: controls may change while a provider request is in
// flight, but that request keeps its immutable model/thinking/prompt/tool
// snapshot. A tool turn in the same run observes the new state, and idle reload
// refreshes resources without replacing Agent or durable conversation state.
func TestUpstreamAgentSessionTurnSnapshotOracle(t *testing.T) {
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
	expectedScenario, ok := expectedObject["turnSnapshotScenario"].(map[string]any)
	if !ok {
		t.Fatal("workflow oracle turnSnapshotScenario is not an object")
	}
	scenario := corpus.TurnSnapshot
	if len(scenario.Responses) != 3 || !scenario.Responses[0].ToolCall ||
		scenario.InitialThinkingLevel != string(provider.ThinkingLow) ||
		scenario.NextThinkingLevel != string(provider.ThinkingHigh) {
		t.Fatal("turn snapshot corpus no longer covers the in-flight/tool-turn/reload transition")
	}

	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	agentDir := filepath.Join(root, "agent")
	sessionDir := filepath.Join(root, "sessions")
	for _, directory := range []string{cwd, agentDir, sessionDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create turn snapshot workflow directory: %v", err)
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
			return fmt.Sprintf("go-turn-snapshot-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("create turn snapshot session manager: %v", err)
	}
	managerOwned := true
	defer func() {
		if managerOwned {
			_ = manager.Close()
		}
	}()

	initialModel := catalogmodel.Model{
		Provider: "faux", API: "anthropic-messages", ID: scenario.InitialModel.ID, Name: scenario.InitialModel.Name,
		BaseURL: "http://localhost:0", Reasoning: true,
		Input: []provider.InputKind{provider.InputText, provider.InputImage}, Cost: provider.CostRates{},
		ContextWindow: 128_000, MaxTokens: 16_384,
	}
	nextModel := initialModel
	nextModel.ID = scenario.NextModel.ID
	nextModel.Name = scenario.NextModel.Name
	nextModelRef, err := nextModel.Ref()
	if err != nil {
		t.Fatalf("construct next turn snapshot model: %v", err)
	}

	initialDefinition, err := provider.NewToolDefinition(
		scenario.InitialTool.Name,
		scenario.InitialTool.Description,
		false,
		[]byte(`{"type":"object","properties":{},"additionalProperties":false}`),
	)
	if err != nil {
		t.Fatalf("construct initial turn snapshot tool: %v", err)
	}
	nextDefinition, err := provider.NewToolDefinition(
		scenario.NextTool.Name,
		scenario.NextTool.Description,
		false,
		[]byte(`{"type":"object","properties":{},"additionalProperties":false}`),
	)
	if err != nil {
		t.Fatalf("construct next turn snapshot tool: %v", err)
	}

	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{
		ChunkRunes: 3,
		Clock:      func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatalf("construct turn snapshot provider: %v", err)
	}
	toolCall, err := llm.NewToolCallBlock(scenario.InitialTool.CallID, scenario.InitialTool.Name, []byte(`{}`))
	if err != nil {
		t.Fatalf("construct turn snapshot tool call: %v", err)
	}
	firstResponse := scenario.Responses[0]
	firstTerminal, err := newAssistantToolUseMessage(
		[]llm.AssistantBlock{mustTextBlock(t, firstResponse.Text), toolCall},
		mustUsage(t, firstResponse.InputTokens, firstResponse.OutputTokens),
		agentTestEpoch,
	)
	if err != nil {
		t.Fatalf("construct first turn snapshot response: %v", err)
	}
	providerEntered := make(chan struct{})
	releaseProvider := make(chan struct{})
	defer func() {
		select {
		case <-releaseProvider:
		default:
			close(releaseProvider)
		}
	}()
	blocking, err := provider.FactoryResponseStep(func(context.Context, provider.Request, uint64) (llm.AssistantTerminal, error) {
		close(providerEntered)
		<-releaseProvider
		return firstTerminal, nil
	})
	if err != nil {
		t.Fatalf("construct blocking turn snapshot response: %v", err)
	}
	steps := []provider.ScriptStep{blocking}
	for index, response := range scenario.Responses[1:] {
		terminal, terminalErr := newAssistantTextMessage(
			[]llm.TextBlock{mustTextBlock(t, response.Text)},
			llm.FinishStop,
			mustUsage(t, response.InputTokens, response.OutputTokens),
			agentTestEpoch,
		)
		if terminalErr != nil {
			t.Fatalf("construct turn snapshot response %d: %v", index+2, terminalErr)
		}
		step, stepErr := provider.FixedResponseStep(terminal)
		if stepErr != nil {
			t.Fatalf("construct turn snapshot response step %d: %v", index+2, stepErr)
		}
		steps = append(steps, step)
	}
	if err := implementation.SetResponses(steps); err != nil {
		t.Fatalf("set turn snapshot responses: %v", err)
	}

	actions := &upstreamTurnSnapshotActions{}
	resources := &upstreamTurnSnapshotResources{
		cwd: cwd, initial: scenario.InitialSystemPrompt, reloaded: scenario.ReloadedSystemPrompt, actions: actions,
	}
	executor := &upstreamTurnSnapshotExecutor{
		initialName: scenario.InitialTool.Name, initialResult: scenario.InitialTool.Result, nextName: scenario.NextTool.Name,
	}
	disabled := false
	initialThinking := provider.ThinkingLevel(scenario.InitialThinkingLevel)
	created, err := agentruntime.CreateAgentSession(context.Background(), agentruntime.SessionFactoryOptions{
		Services: &agentruntime.Services{
			CWD: cwd, AgentDir: agentDir, Provider: implementation, Tool: executor,
			Tools: []provider.ToolDefinition{initialDefinition, nextDefinition},
		},
		Provider:       implementation,
		SessionManager: manager,
		AllModels:      []catalogmodel.Model{initialModel, nextModel},
		Availability: catalogmodel.Availability{
			HasConfiguredAuth: func(string) bool { return true },
			SupportsRoute:     func(catalogmodel.Model) bool { return true },
		},
		ExplicitModel:         &initialModel,
		ExplicitThinkingLevel: &initialThinking,
		Settings: catalogmodel.Settings{
			Transport:  provider.TransportSSE,
			Compaction: catalogmodel.CompactionSettings{Enabled: &disabled},
			Retry:      catalogmodel.RetrySettings{Enabled: &disabled},
		},
		BaseConfig: agent.SessionConfig{
			Tool: executor, Tools: []provider.ToolDefinition{initialDefinition},
			AllTools:  []provider.ToolDefinition{initialDefinition, nextDefinition},
			Resources: resources,
			Stream:    provider.StreamOptions{SessionID: scenario.SessionID, Transport: provider.TransportSSE},
			Hooks: agent.Hooks{
				SessionStart: func(_ context.Context, event agent.SessionStartHookEvent) error {
					actions.append("session_start:" + string(event.Reason))
					return nil
				},
				SessionShutdown: func(_ context.Context, event agent.SessionShutdownHookEvent) error {
					actions.append("session_shutdown:" + string(event.Reason))
					return nil
				},
				ModelSelect: func(_ context.Context, event agent.ModelSelectEvent) error {
					previous := "none"
					if event.PreviousModel != nil {
						previous = event.PreviousModel.ID()
					}
					actions.append(fmt.Sprintf("model_select:%s->%s:%s", previous, event.Model.ID(), event.Source))
					return nil
				},
				ThinkingLevelSelect: func(_ context.Context, event agent.ThinkingLevelSelectEvent) error {
					actions.append(fmt.Sprintf("thinking_level_select:%s->%s", event.PreviousLevel, event.Level))
					return nil
				},
			},
			Now:               func() time.Time { return agentTestEpoch },
			SettlementTimeout: time.Second,
		},
	})
	if err != nil {
		t.Fatalf("create Go turn snapshot AgentSession: %v", err)
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
			settledSnapshots = append(settledSnapshots, upstreamTurnSnapshotState(runtime, root, cwd))
		}
		eventMu.Unlock()
	})

	firstRunDone := make(chan upstreamTurnSnapshotRunOutcome, 1)
	go func() {
		result, runErr := runtime.Prompt(context.Background(), scenario.InitialPrompt)
		firstRunDone <- upstreamTurnSnapshotRunOutcome{result: result, err: runErr}
	}()
	select {
	case <-providerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("turn snapshot workflow did not enter the first provider call")
	}
	if err := runtime.SetModel(nextModelRef); err != nil {
		t.Fatalf("switch model during first provider request: %v", err)
	}
	if err := runtime.SetThinkingLevel(provider.ThinkingLevel(scenario.NextThinkingLevel)); err != nil {
		t.Fatalf("switch thinking during first provider request: %v", err)
	}
	if err := runtime.SetActiveToolsByName([]string{scenario.NextTool.Name}); err != nil {
		t.Fatalf("switch tools during first provider request: %v", err)
	}
	duringFirstRequest := upstreamTurnSnapshotState(runtime, root, cwd)
	close(releaseProvider)
	select {
	case outcome := <-firstRunDone:
		if outcome.err != nil || !outcome.result.Succeeded() {
			t.Fatalf("first turn snapshot prompt = (%#v, %v)", outcome.result, outcome.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first turn snapshot prompt did not settle")
	}
	afterFirstRun := upstreamTurnSnapshotState(runtime, root, cwd)

	if err := runtime.SetActiveToolsByName([]string{scenario.NextTool.Name, scenario.InitialTool.Name}); err != nil {
		t.Fatalf("prepare turn snapshot tools for reload: %v", err)
	}
	beforeReload := upstreamTurnSnapshotState(runtime, root, cwd)
	if err := runtime.Reload(context.Background(), agent.ReloadOptions{BeforeSessionStart: func(context.Context) error {
		actions.append("before_session_start")
		return nil
	}}); err != nil {
		t.Fatalf("reload turn snapshot runtime: %v", err)
	}
	afterReload := upstreamTurnSnapshotState(runtime, root, cwd)
	result, err := runtime.Prompt(context.Background(), scenario.PostReloadPrompt)
	if err != nil || !result.Succeeded() {
		t.Fatalf("post-reload turn snapshot prompt = (%#v, %v)", result, err)
	}
	eventMu.Lock()
	settledEventCount := len(settledSnapshots)
	eventMu.Unlock()
	postActivity := runtime.Activity()
	promptReturn := map[string]any{
		"isStreaming":       postActivity.IsStreaming,
		"isIdle":            postActivity.Phase == agent.PhaseIdle,
		"settledEventCount": settledEventCount,
	}
	if implementation.CallCount() != 3 || implementation.PendingResponses() != 0 {
		t.Fatalf("turn snapshot provider calls/pending = %d/%d, want 3/0", implementation.CallCount(), implementation.PendingResponses())
	}
	if executor.CallCount() != 1 {
		t.Fatalf("turn snapshot tool calls = %d, want 1", executor.CallCount())
	}
	if settledEventCount != 2 {
		t.Fatalf("turn snapshot settled events = %d, want 2", settledEventCount)
	}

	unsubscribe()
	eventMu.Lock()
	events := append([]agent.SessionEvent(nil), observed...)
	settled := append([]any(nil), settledSnapshots...)
	eventMu.Unlock()
	entries := manager.Entries()
	entryIDs := workflowEntryIDs(entries)
	header := manager.Header()
	sessionFile, ok := manager.SessionFile()
	if !ok {
		t.Fatal("turn snapshot manager has no persistent session file")
	}
	stats, err := runtime.GetSessionStats()
	if err != nil {
		t.Fatalf("turn snapshot stats: %v", err)
	}
	providerInputs, err := normalizeWorkflowProviderInputs(implementation.Requests(), root, cwd, scenario.SessionID)
	if err != nil {
		t.Fatalf("normalize turn snapshot provider inputs: %v", err)
	}
	normalizedEvents, err := normalizeWorkflowEvents(events, entryIDs)
	if err != nil {
		t.Fatalf("normalize turn snapshot events: %v", err)
	}
	state := runtime.State()
	selectedRef, selectedOK := runtime.SelectedModel()
	if !selectedOK {
		t.Fatal("turn snapshot session lost its selected model")
	}
	finalMessages, err := normalizeWorkflowAgentMessages(state.Active.Messages())
	if err != nil {
		t.Fatalf("normalize final turn snapshot messages: %v", err)
	}
	normalizedEntries, err := normalizeWorkflowEntries(entries, entryIDs)
	if err != nil {
		t.Fatalf("normalize turn snapshot entries: %v", err)
	}
	finalActivity := runtime.Activity()
	finalThinkingLevel := runtime.ThinkingLevel()
	finalActiveTools := append([]string{}, runtime.ActiveToolNames()...)
	finalSystemPrompt := runtime.SystemPrompt()
	finalPendingMessageCount := runtime.PendingMessageCount()
	controlActions := actions.snapshot()
	loadedResourceGeneration := resources.Generation()
	normalizedToolRuns := executor.Runs()

	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("close turn snapshot runtime: %v", err)
	}
	runtimeOwned = false
	managerOwned = false

	fileHeader, fileEntries, err := normalizeWorkflowJSONL(sessionFile, entryIDs, root, cwd)
	if err != nil {
		t.Fatalf("normalize turn snapshot JSONL: %v", err)
	}
	reopened, err := session.OpenSessionManagerWithOptions(sessionFile, sessionDir, "", session.ManagerOptions{
		Now: func() time.Time { return agentTestEpoch },
		NewEntryID: func() (string, error) {
			return fmt.Sprintf("reopened-turn-snapshot-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("reopen turn snapshot session: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	reopenedEntries, err := normalizeWorkflowEntries(reopened.Entries(), entryIDs)
	if err != nil {
		t.Fatalf("normalize reopened turn snapshot entries: %v", err)
	}
	reopenedContext := reopened.BuildContext()
	reopenedMessages, err := normalizeWorkflowAgentMessages(reopenedContext.AgentMessages())
	if err != nil {
		t.Fatalf("normalize reopened turn snapshot messages: %v", err)
	}
	reopenedModel, hasReopenedModel := reopenedContext.Model()
	reopenedThinking, hasReopenedThinking := reopenedContext.ThinkingLevel()
	if !hasReopenedModel || !hasReopenedThinking {
		t.Fatalf("reopened turn snapshot selection = model:%t thinking:%t", hasReopenedModel, hasReopenedThinking)
	}

	actualScenario := map[string]any{
		"name":  scenario.Name,
		"input": scenario,
		"actions": map[string]any{
			"duringFirstRequest":       duringFirstRequest,
			"afterFirstRun":            afterFirstRun,
			"beforeReload":             beforeReload,
			"afterReload":              afterReload,
			"promptReturn":             promptReturn,
			"settledSnapshots":         settled,
			"controlActions":           controlActions,
			"loadedResourceGeneration": loadedResourceGeneration,
		},
		"providerInputs": providerInputs,
		"toolRuns":       normalizedToolRuns,
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
	if difference := workflowJSONDifference("turnSnapshotScenario", expectedScenario, canonicalWorkflowJSON(t, actualScenario)); difference != "" {
		t.Fatalf("Go turn snapshot AgentSession workflow differs from pinned TypeScript oracle: %s", difference)
	}
	if difference := workflowJSONDifference(
		"turnSnapshotScenario.session.header",
		expectedScenario["session"].(map[string]any)["header"],
		fileHeader,
	); difference != "" {
		t.Fatalf("physical turn snapshot header differs from pinned TypeScript oracle: %s", difference)
	}
}

func upstreamTurnSnapshotState(runtime *agent.AgentSession, root, cwd string) map[string]any {
	activity := runtime.Activity()
	selected, _ := runtime.SelectedModel()
	return map[string]any{
		"isStreaming":   activity.IsStreaming,
		"isIdle":        activity.Phase == agent.PhaseIdle,
		"model":         selected.ID(),
		"thinkingLevel": string(runtime.ThinkingLevel()),
		"activeTools":   append([]string{}, runtime.ActiveToolNames()...),
		"systemPrompt":  normalizeWorkflowPath(runtime.SystemPrompt(), root, cwd),
	}
}

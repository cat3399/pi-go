package agent_test

import (
	"bytes"
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

type upstreamRuntimeReplacementScenario struct {
	Name            string                          `json:"name"`
	SourceSessionID string                          `json:"sourceSessionId"`
	SystemPrompt    string                          `json:"systemPrompt"`
	InitialPrompt   string                          `json:"initialPrompt"`
	NewPrompt       string                          `json:"newPrompt"`
	ResumePrompt    string                          `json:"resumePrompt"`
	ImportPrompt    string                          `json:"importPrompt"`
	AbortError      string                          `json:"abortError"`
	Responses       []upstreamControlOracleResponse `json:"responses"`
}

// TestUpstreamAgentSessionRuntimeReplacementOracle runs the pinned TypeScript
// and Go production runtime factories through reload, active-run replacement,
// new, resume, JSONL import, and dispose. It fixes lifecycle hooks, host
// callbacks, provider contexts, durable outgoing settlement, and every final
// session projection without depending on a Surface transport.
func TestUpstreamAgentSessionRuntimeReplacementOracle(t *testing.T) {
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
	expectedScenario, ok := expectedObject["runtimeReplacementScenario"].(map[string]any)
	if !ok {
		t.Fatal("workflow oracle runtimeReplacementScenario is not an object")
	}
	scenario := corpus.RuntimeReplacement
	if len(scenario.Responses) != 3 || scenario.AbortError != "scripted provider request aborted" {
		t.Fatal("runtime replacement corpus no longer covers new, resume, import, and a canonical aborted source")
	}

	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	agentDir := filepath.Join(root, "agent")
	sessionDir := filepath.Join(root, "sessions")
	externalDir := filepath.Join(root, "external")
	for _, directory := range []string{cwd, agentDir, sessionDir, externalDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create runtime replacement workflow directory: %v", err)
		}
	}

	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{
		ChunkRunes: 3,
		Clock:      func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatalf("construct runtime replacement provider: %v", err)
	}
	providerEntered := make(chan struct{})
	var providerEnteredOnce sync.Once
	blocking, err := provider.FactoryResponseStep(func(ctx context.Context, _ provider.Request, _ uint64) (llm.AssistantTerminal, error) {
		providerEnteredOnce.Do(func() { close(providerEntered) })
		<-ctx.Done()
		return nil, context.Cause(ctx)
	})
	if err != nil {
		t.Fatalf("construct runtime replacement blocking response: %v", err)
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
			t.Fatalf("construct runtime replacement response %d: %v", index, terminalErr)
		}
		step, stepErr := provider.FixedResponseStep(terminal)
		if stepErr != nil {
			t.Fatalf("construct runtime replacement response step %d: %v", index, stepErr)
		}
		steps = append(steps, step)
	}
	if err := implementation.SetResponses(steps); err != nil {
		t.Fatalf("set runtime replacement responses: %v", err)
	}

	selected := catalogmodel.Model{
		Provider: "faux", API: provider.AnthropicMessagesAPI, ID: "faux-1", Name: "Faux Model",
		BaseURL: "http://localhost:0", Input: []provider.InputKind{provider.InputText, provider.InputImage},
		Cost: provider.CostRates{}, ContextWindow: 128_000, MaxTokens: 16_384,
	}
	disabled := false
	off := provider.ThinkingOff

	var actionMu sync.Mutex
	var lifecycle []map[string]any
	var hostActions []map[string]any
	owners := make(map[*agent.AgentSession]string)
	generations := []string{"source", "new", "source-resume", "import"}
	factoryCalls := 0
	appendLifecycle := func(action map[string]any) {
		actionMu.Lock()
		lifecycle = append(lifecycle, action)
		actionMu.Unlock()
	}
	appendHostAction := func(action map[string]any) {
		actionMu.Lock()
		hostActions = append(hostActions, action)
		actionMu.Unlock()
	}
	factory := func(ctx context.Context, options agentruntime.CreateOptions) (agentruntime.CreateResult, error) {
		owner := fmt.Sprintf("generation-%d", factoryCalls)
		if factoryCalls < len(generations) {
			owner = generations[factoryCalls]
		}
		factoryCalls++
		reason := agent.SessionStartup
		if options.SessionStartEvent != nil {
			reason = options.SessionStartEvent.Reason
		}
		factoryAction := map[string]any{"type": "factory", "owner": owner, "reason": string(reason)}
		if sessionFile, present := options.SessionManager.SessionFile(); present {
			factoryAction["sessionFile"] = sessionFile
		}
		if options.SessionStartEvent != nil && options.SessionStartEvent.PreviousSessionFile != nil {
			factoryAction["previousSessionFile"] = *options.SessionStartEvent.PreviousSessionFile
		}
		appendHostAction(factoryAction)

		hooks := agent.Hooks{
			SessionBeforeSwitch: func(_ context.Context, event agent.SessionBeforeSwitchEvent) (agent.SessionBeforeSwitchResult, error) {
				action := map[string]any{
					"type": "session_before_switch", "owner": owner, "reason": string(event.Reason),
				}
				if event.TargetSessionFile != nil {
					action["targetSessionFile"] = *event.TargetSessionFile
				}
				appendLifecycle(action)
				return agent.SessionBeforeSwitchResult{}, nil
			},
			SessionShutdown: func(_ context.Context, event agent.SessionShutdownHookEvent) error {
				action := map[string]any{
					"type": "session_shutdown", "owner": owner, "reason": string(event.Reason),
				}
				if event.TargetSessionFile != nil {
					action["targetSessionFile"] = *event.TargetSessionFile
				}
				appendLifecycle(action)
				return nil
			},
			SessionStart: func(_ context.Context, event agent.SessionStartHookEvent) error {
				action := map[string]any{"type": "session_start", "owner": owner, "reason": string(event.Reason)}
				if event.PreviousSessionFile != nil {
					action["previousSessionFile"] = *event.PreviousSessionFile
				}
				appendLifecycle(action)
				return nil
			},
		}
		created, createErr := agentruntime.CreateAgentSession(ctx, agentruntime.SessionFactoryOptions{
			Services:       &agentruntime.Services{CWD: options.CWD, AgentDir: options.AgentDir, Provider: implementation},
			Provider:       implementation,
			SessionManager: options.SessionManager,
			AllModels:      []catalogmodel.Model{selected},
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
					Enabled: &disabled,
				},
			},
			BaseConfig: agent.SessionConfig{
				SystemPrompt: scenario.SystemPrompt + "\nCurrent working directory: " + options.CWD,
				Stream: provider.StreamOptions{
					SessionID: options.SessionManager.SessionID(),
					Transport: provider.TransportSSE,
				},
				Hooks:             hooks,
				Now:               func() time.Time { return agentTestEpoch },
				SettlementTimeout: time.Second,
			},
			SessionStartEvent: options.SessionStartEvent,
		})
		if createErr != nil {
			return agentruntime.CreateResult{}, createErr
		}
		actionMu.Lock()
		owners[created.Session] = owner
		actionMu.Unlock()
		return agentruntime.CreateResult{
			Session: created.Session,
			Services: &agentruntime.Services{
				CWD: options.CWD, AgentDir: options.AgentDir, Provider: implementation,
			},
		}, nil
	}

	var clockTick atomic.Int64
	var entrySequence atomic.Uint64
	sourceManager, err := session.CreateSessionManagerWithOptions(cwd, sessionDir, session.ManagerOptions{
		NewSession: session.NewSessionOptions{ID: scenario.SourceSessionID},
		Now: func() time.Time {
			return agentTestEpoch.Add(time.Duration(clockTick.Add(1)) * time.Millisecond)
		},
		NewEntryID: func() (string, error) {
			return fmt.Sprintf("go-runtime-replacement-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("create runtime replacement source manager: %v", err)
	}
	runtimeHost, err := agentruntime.Create(context.Background(), factory, agentruntime.InitialOptions{
		CWD: cwd, AgentDir: agentDir, SessionManager: sourceManager,
	})
	if err != nil {
		t.Fatalf("create Go AgentSession Runtime: %v", err)
	}
	runtimeOwned := true
	defer func() {
		if runtimeOwned {
			_ = runtimeHost.Dispose(context.Background())
		}
	}()
	sourceSession := runtimeHost.Session()
	sourceFile, ok := sourceSession.SessionManager().SessionFile()
	if !ok {
		t.Fatal("runtime replacement source has no session file")
	}

	rebindCalls := 0
	runtimeHost.SetRebindSession(func(_ context.Context, replacement *agent.AgentSession) error {
		actionMu.Lock()
		owner := owners[replacement]
		actionMu.Unlock()
		reason := "replacement"
		if rebindCalls == 0 {
			reason = "reload"
		}
		rebindCalls++
		action := map[string]any{"type": "rebind", "owner": owner, "reason": reason}
		if sessionFile, present := replacement.SessionManager().SessionFile(); present {
			action["sessionFile"] = sessionFile
		}
		appendHostAction(action)
		return nil
	})
	if err := runtimeHost.Reload(context.Background()); err != nil {
		t.Fatalf("reload source runtime: %v", err)
	}
	runtimeHost.SetBeforeSessionInvalidate(func() {
		current := runtimeHost.Session()
		actionMu.Lock()
		owner := owners[current]
		actionMu.Unlock()
		action := map[string]any{"type": "invalidate", "owner": owner}
		if sessionFile, present := current.SessionManager().SessionFile(); present {
			action["sessionFile"] = sessionFile
		}
		appendHostAction(action)
	})

	initialRunDone := make(chan upstreamControlRunOutcome, 1)
	go func() {
		result, runErr := sourceSession.Prompt(context.Background(), scenario.InitialPrompt)
		initialRunDone <- upstreamControlRunOutcome{result: result, err: runErr}
	}()
	select {
	case <-providerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime replacement source provider did not start")
	}
	newResult, err := runtimeHost.NewSession(context.Background(), agentruntime.NewOptions{
		WithSession: func(context.Context, *agent.AgentSession) error {
			appendHostAction(map[string]any{"type": "with_session", "owner": "new"})
			return nil
		},
	})
	if err != nil || newResult.Cancelled {
		t.Fatalf("runtime replacement NewSession = (%#v, %v)", newResult, err)
	}
	var sourceRun upstreamControlRunOutcome
	select {
	case sourceRun = <-initialRunDone:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime replacement source prompt did not settle")
	}
	if sourceRun.err != nil || sourceRun.result.Succeeded() {
		t.Fatalf("runtime replacement source prompt = (%#v, %v)", sourceRun.result, sourceRun.err)
	}
	sourceActivity := sourceSession.Activity()
	sourceQueue := sourceSession.PendingQueue()
	sourceRunReturn := map[string]any{
		"isStreaming":         sourceActivity.IsStreaming,
		"isIdle":              sourceActivity.Phase == agent.PhaseIdle,
		"steering":            append([]string{}, sourceQueue.Steering...),
		"followUp":            append([]string{}, sourceQueue.FollowUp...),
		"pendingMessageCount": len(sourceQueue.SteeringMessages) + len(sourceQueue.FollowUpMessages),
	}
	afterNew := upstreamRuntimeReplacementSnapshot(t, runtimeHost, "new")
	if result, promptErr := runtimeHost.Session().Prompt(context.Background(), scenario.NewPrompt); promptErr != nil || !result.Succeeded() {
		t.Fatalf("runtime replacement new prompt = (%#v, %v)", result, promptErr)
	}
	newFile, ok := runtimeHost.Session().SessionManager().SessionFile()
	if !ok {
		t.Fatal("runtime replacement new session has no file")
	}
	newData, err := os.ReadFile(newFile)
	if err != nil {
		t.Fatalf("read runtime replacement new session: %v", err)
	}
	externalImportFile := filepath.Join(externalDir, "runtime-import.jsonl")
	if err := os.WriteFile(externalImportFile, newData, 0o600); err != nil {
		t.Fatalf("write runtime replacement external import: %v", err)
	}

	switchResult, err := runtimeHost.SwitchSession(context.Background(), sourceFile, agentruntime.SwitchOptions{
		WithSession: func(context.Context, *agent.AgentSession) error {
			appendHostAction(map[string]any{"type": "with_session", "owner": "source-resume"})
			return nil
		},
	})
	if err != nil || switchResult.Cancelled {
		t.Fatalf("runtime replacement SwitchSession = (%#v, %v)", switchResult, err)
	}
	afterSwitch := upstreamRuntimeReplacementSnapshot(t, runtimeHost, "source-resume")
	if result, promptErr := runtimeHost.Session().Prompt(context.Background(), scenario.ResumePrompt); promptErr != nil || !result.Succeeded() {
		t.Fatalf("runtime replacement resume prompt = (%#v, %v)", result, promptErr)
	}

	importResult, err := runtimeHost.ImportFromJSONL(context.Background(), externalImportFile, "")
	if err != nil || importResult.Cancelled {
		t.Fatalf("runtime replacement ImportFromJSONL = (%#v, %v)", importResult, err)
	}
	afterImport := upstreamRuntimeReplacementSnapshot(t, runtimeHost, "import")
	if result, promptErr := runtimeHost.Session().Prompt(context.Background(), scenario.ImportPrompt); promptErr != nil || !result.Succeeded() {
		t.Fatalf("runtime replacement import prompt = (%#v, %v)", result, promptErr)
	}
	importFile, ok := runtimeHost.Session().SessionManager().SessionFile()
	if !ok {
		t.Fatal("runtime replacement imported session has no file")
	}
	importData, err := os.ReadFile(importFile)
	if err != nil {
		t.Fatalf("read runtime replacement imported session: %v", err)
	}
	finalStats, err := runtimeHost.Session().GetSessionStats()
	if err != nil {
		t.Fatalf("runtime replacement final stats: %v", err)
	}
	finalStateSnapshot := runtimeHost.Session().State()
	finalSelected, finalSelectedOK := runtimeHost.Session().SelectedModel()
	if !finalSelectedOK {
		t.Fatal("runtime replacement final session lost its model")
	}
	finalMessages, err := normalizeWorkflowAgentMessages(finalStateSnapshot.Active.Messages())
	if err != nil {
		t.Fatalf("normalize runtime replacement final messages: %v", err)
	}
	finalActivity := runtimeHost.Session().Activity()
	finalThinking := runtimeHost.Session().ThinkingLevel()
	finalActiveTools := append([]string{}, runtimeHost.Session().ActiveToolNames()...)
	finalSystemPrompt := runtimeHost.Session().SystemPrompt()
	finalPendingMessageCount := runtimeHost.Session().PendingMessageCount()
	if err := runtimeHost.Dispose(context.Background()); err != nil {
		t.Fatalf("dispose runtime replacement host: %v", err)
	}
	runtimeOwned = false
	if implementation.CallCount() != 4 || implementation.PendingResponses() != 0 || factoryCalls != 4 {
		t.Fatalf(
			"runtime replacement provider calls/pending/factory = %d/%d/%d, want 4/0/4",
			implementation.CallCount(), implementation.PendingResponses(), factoryCalls,
		)
	}

	fileLabels := map[string]string{
		sourceFile:         "<source-session-file>",
		newFile:            "<new-session-file>",
		importFile:         "<import-session-file>",
		externalImportFile: "<external-import-file>",
	}
	actionMu.Lock()
	normalizedLifecycle := normalizeUpstreamRuntimeActions(lifecycle, fileLabels, root, cwd)
	normalizedHostActions := normalizeUpstreamRuntimeActions(hostActions, fileLabels, root, cwd)
	actionMu.Unlock()
	afterNew = normalizeUpstreamRuntimeSnapshot(afterNew, fileLabels, scenario.SourceSessionID, root, cwd)
	afterSwitch = normalizeUpstreamRuntimeSnapshot(afterSwitch, fileLabels, scenario.SourceSessionID, root, cwd)
	afterImport = normalizeUpstreamRuntimeSnapshot(afterImport, fileLabels, scenario.SourceSessionID, root, cwd)
	providerInputs, err := normalizeWorkflowProviderInputsWithForeignLabel(
		implementation.Requests(), root, cwd, scenario.SourceSessionID, "<replacement-session-id>",
	)
	if err != nil {
		t.Fatalf("normalize runtime replacement provider inputs: %v", err)
	}
	normalizedStats := normalizeWorkflowStats(finalStats)
	normalizedStats["sessionId"] = "<replacement-session-id>"
	finalState := map[string]any{
		"isStreaming":         finalActivity.IsStreaming,
		"pendingMessageCount": finalPendingMessageCount,
		"model": map[string]any{
			"provider": finalSelected.Provider(), "api": finalSelected.API(), "id": finalSelected.ID(),
		},
		"thinkingLevel": string(finalThinking),
		"activeTools":   finalActiveTools,
		"systemPrompt":  normalizeWorkflowPath(finalSystemPrompt, root, cwd),
		"messages":      finalMessages,
		"stats":         normalizedStats,
	}

	sourceProjection, err := upstreamRuntimeProjection(sourceFile, sessionDir, scenario.SourceSessionID, root, cwd)
	if err != nil {
		t.Fatalf("project runtime replacement source: %v", err)
	}
	createdProjection, err := upstreamRuntimeProjection(newFile, sessionDir, scenario.SourceSessionID, root, cwd)
	if err != nil {
		t.Fatalf("project runtime replacement created session: %v", err)
	}
	importedProjection, err := upstreamRuntimeProjection(importFile, sessionDir, scenario.SourceSessionID, root, cwd)
	if err != nil {
		t.Fatalf("project runtime replacement imported session: %v", err)
	}
	actualScenario := map[string]any{
		"name":  scenario.Name,
		"input": scenario,
		"actions": map[string]any{
			"sourceRunReturn": sourceRunReturn,
			"newResult":       map[string]any{"cancelled": newResult.Cancelled},
			"switchResult":    map[string]any{"cancelled": switchResult.Cancelled},
			"importResult":    map[string]any{"cancelled": importResult.Cancelled},
			"afterNew":        afterNew,
			"afterSwitch":     afterSwitch,
			"afterImport":     afterImport,
			"lifecycle":       normalizedLifecycle,
			"hostActions":     normalizedHostActions,
			"files": map[string]any{
				"source":                  "<source-session-file>",
				"created":                 "<new-session-file>",
				"imported":                "<import-session-file>",
				"external":                "<external-import-file>",
				"allDistinct":             len(fileLabels) == 4,
				"importStartsWithCreated": bytes.HasPrefix(importData, newData),
			},
		},
		"providerInputs": providerInputs,
		"finalState":     finalState,
		"sessions": map[string]any{
			"source":   sourceProjection,
			"created":  createdProjection,
			"imported": importedProjection,
		},
	}
	if difference := workflowJSONDifference(
		"runtimeReplacementScenario", expectedScenario, canonicalWorkflowJSON(t, actualScenario),
	); difference != "" {
		t.Fatalf("Go Runtime replacement workflow differs from pinned TypeScript oracle: %s", difference)
	}
}

func upstreamRuntimeReplacementSnapshot(t *testing.T, runtimeHost *agentruntime.Runtime, owner string) map[string]any {
	t.Helper()
	current := runtimeHost.Session()
	selected, ok := current.SelectedModel()
	if !ok {
		t.Fatal("runtime replacement snapshot has no selected model")
	}
	state := current.State()
	roles := make([]string, 0, len(state.Active.Messages()))
	for _, message := range state.Active.Messages() {
		roles = append(roles, string(message.Role()))
	}
	snapshot := map[string]any{
		"owner":     owner,
		"sessionId": current.SessionManager().SessionID(),
		"cwd":       runtimeHost.CWD(),
		"model": map[string]any{
			"provider": selected.Provider(), "api": selected.API(), "id": selected.ID(),
		},
		"thinkingLevel": string(current.ThinkingLevel()),
		"messageRoles":  roles,
	}
	if sessionFile, present := current.SessionManager().SessionFile(); present {
		snapshot["sessionFile"] = sessionFile
	}
	return snapshot
}

func normalizeUpstreamRuntimeSnapshot(
	snapshot map[string]any,
	fileLabels map[string]string,
	sourceSessionID, root, cwd string,
) map[string]any {
	normalized := normalizeUpstreamRuntimeAction(snapshot, fileLabels, root, cwd)
	normalized["cwd"] = normalizeWorkflowPath(fmt.Sprint(snapshot["cwd"]), root, cwd)
	if snapshot["sessionId"] != sourceSessionID {
		normalized["sessionId"] = "<replacement-session-id>"
	}
	return normalized
}

func normalizeUpstreamRuntimeActions(
	actions []map[string]any,
	fileLabels map[string]string,
	root, cwd string,
) []any {
	result := make([]any, 0, len(actions))
	for _, action := range actions {
		result = append(result, normalizeUpstreamRuntimeAction(action, fileLabels, root, cwd))
	}
	return result
}

func normalizeUpstreamRuntimeAction(
	action map[string]any,
	fileLabels map[string]string,
	root, cwd string,
) map[string]any {
	normalized := make(map[string]any, len(action))
	for key, value := range action {
		switch key {
		case "sessionFile", "targetSessionFile", "previousSessionFile":
			path := fmt.Sprint(value)
			if label, ok := fileLabels[path]; ok {
				normalized[key] = label
			} else {
				normalized[key] = normalizeWorkflowPath(path, root, cwd)
			}
		default:
			normalized[key] = value
		}
	}
	return normalized
}

func upstreamRuntimeProjection(path, sessionDir, sourceSessionID, root, cwd string) (map[string]any, error) {
	manager, err := session.OpenSessionManager(path, sessionDir, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = manager.Close() }()
	entries := manager.Entries()
	entryIDs := workflowEntryIDs(entries)
	normalizedEntries, err := normalizeWorkflowEntries(entries, entryIDs)
	if err != nil {
		return nil, err
	}
	_, fileEntries, err := normalizeWorkflowJSONL(path, entryIDs, root, cwd)
	if err != nil {
		return nil, err
	}
	contextProjection := manager.BuildContext()
	messages, err := normalizeWorkflowAgentMessages(contextProjection.AgentMessages())
	if err != nil {
		return nil, err
	}
	model, hasModel := contextProjection.Model()
	thinking, hasThinking := contextProjection.ThinkingLevel()
	if !hasModel || !hasThinking {
		return nil, fmt.Errorf("runtime projection %s lacks model/thinking", path)
	}
	header := normalizeWorkflowHeader(manager.Header(), root, cwd)
	if manager.SessionID() != sourceSessionID {
		header["id"] = "<replacement-session-id>"
	}
	return map[string]any{
		"header":      header,
		"entries":     normalizedEntries,
		"fileEntries": fileEntries,
		"context": map[string]any{
			"messages": messages,
			"model": map[string]any{
				"provider": model.Provider, "modelId": model.ModelID,
			},
			"thinkingLevel": thinking,
		},
	}, nil
}

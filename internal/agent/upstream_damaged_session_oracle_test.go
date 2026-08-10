package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

type upstreamDamagedSessionScenario struct {
	Name               string                         `json:"name"`
	SessionID          string                         `json:"sessionId"`
	SystemPrompt       string                         `json:"systemPrompt"`
	RootPrompt         string                         `json:"rootPrompt"`
	RootResponse       upstreamDamagedSessionResponse `json:"rootResponse"`
	MalformedLine      string                         `json:"malformedLine"`
	OrphanPrompt       string                         `json:"orphanPrompt"`
	ContinuationPrompt string                         `json:"continuationPrompt"`
	Response           upstreamDamagedSessionResponse `json:"response"`
}

type upstreamDamagedSessionResponse struct {
	Text         string `json:"text"`
	InputTokens  uint64 `json:"inputTokens"`
	OutputTokens uint64 `json:"outputTokens"`
}

// TestUpstreamDamagedSessionResumeContinueOracle verifies the product path,
// not only SessionManager's parser: a persisted v3 log containing a malformed
// physical line and an orphan root is opened by Runtime.Factory, continued by
// AgentSession, appended without rewriting evidence, and opened again.
func TestUpstreamDamagedSessionResumeContinueOracle(t *testing.T) {
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
	expectedScenario, ok := expectedObject["damagedSessionScenario"].(map[string]any)
	if !ok {
		t.Fatal("workflow oracle damagedSessionScenario is not an object")
	}
	scenario := corpus.DamagedSession

	root := t.TempDir()
	scenarioRoot := filepath.Join(root, "damaged-session")
	cwd := filepath.Join(scenarioRoot, "project")
	agentDir := filepath.Join(scenarioRoot, "agent")
	sessionDir := filepath.Join(scenarioRoot, "sessions")
	for _, directory := range []string{cwd, agentDir, sessionDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create damaged session directory: %v", err)
		}
	}

	selected := catalogmodel.Model{
		Provider: "faux", API: "anthropic-messages", ID: "faux-1", Name: "Faux Model",
		BaseURL: "http://localhost:0", Input: []provider.InputKind{provider.InputText, provider.InputImage},
		Cost: provider.CostRates{}, ContextWindow: 128_000, MaxTokens: 16_384,
	}
	sourceData, err := encodeDamagedSessionSeed(cwd, selected, scenario)
	if err != nil {
		t.Fatalf("encode damaged session seed: %v", err)
	}
	sessionFile := filepath.Join(sessionDir, "damaged.jsonl")
	if err := os.WriteFile(sessionFile, sourceData, 0o600); err != nil {
		t.Fatalf("write damaged session seed: %v", err)
	}

	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{
		ChunkRunes: 3,
		Clock:      func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatalf("construct damaged session provider: %v", err)
	}
	response, err := newAssistantTextMessage(
		[]llm.TextBlock{mustTextBlock(t, scenario.Response.Text)},
		llm.FinishStop,
		mustUsage(t, scenario.Response.InputTokens, scenario.Response.OutputTokens),
		agentTestEpoch,
	)
	if err != nil {
		t.Fatalf("construct damaged session response: %v", err)
	}
	step, err := provider.FixedResponseStep(response)
	if err != nil {
		t.Fatalf("construct damaged session response step: %v", err)
	}
	if err := implementation.SetResponses([]provider.ScriptStep{step}); err != nil {
		t.Fatalf("set damaged session response: %v", err)
	}

	var clockTick atomic.Int64
	var entrySequence atomic.Uint64
	manager, err := session.OpenSessionManagerWithOptions(sessionFile, sessionDir, "", session.ManagerOptions{
		Now: func() time.Time {
			return agentTestEpoch.Add(time.Duration(clockTick.Add(1)) * time.Millisecond)
		},
		NewEntryID: func() (string, error) {
			return fmt.Sprintf("go-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("open damaged session manager: %v", err)
	}

	disabled := false
	off := provider.ThinkingOff
	factory := func(ctx context.Context, options agentruntime.CreateOptions) (agentruntime.CreateResult, error) {
		services := &agentruntime.Services{
			CWD: options.CWD, AgentDir: options.AgentDir, Provider: implementation,
		}
		return agentruntime.CreateAgentSession(ctx, agentruntime.SessionFactoryOptions{
			Services:       services,
			Provider:       implementation,
			SessionManager: options.SessionManager,
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
				Retry:      catalogmodel.RetrySettings{Enabled: &disabled},
			},
			BaseConfig: agent.SessionConfig{
				SystemPrompt: scenario.SystemPrompt + "\nCurrent working directory: " + options.CWD,
				Tools:        []provider.ToolDefinition{},
				AllTools:     []provider.ToolDefinition{},
				Stream: provider.StreamOptions{
					SessionID: options.SessionManager.SessionID(),
					Transport: provider.TransportSSE,
				},
				Now:               func() time.Time { return agentTestEpoch },
				SettlementTimeout: time.Second,
			},
			SessionStartEvent: options.SessionStartEvent,
		})
	}
	runtimeHost, err := agentruntime.Create(context.Background(), factory, agentruntime.InitialOptions{
		CWD: cwd, AgentDir: agentDir, SessionManager: manager,
	})
	if err != nil {
		_ = manager.Close()
		t.Fatalf("create damaged session runtime: %v", err)
	}
	runtimeOwned := true
	defer func() {
		if runtimeOwned {
			_ = runtimeHost.Dispose(context.Background())
		}
	}()

	agentSession := runtimeHost.Session()
	var eventMu sync.Mutex
	var observed []agent.SessionEvent
	unsubscribe := agentSession.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		eventMu.Lock()
		observed = append(observed, event)
		eventMu.Unlock()
	})
	beforeResume := captureTreeForkProjection(agentSession.SessionManager())
	assertTreeForkPrompt(t, agentSession, scenario.ContinuationPrompt, "damaged session continuation")
	if implementation.CallCount() != 1 || implementation.PendingResponses() != 0 {
		t.Fatalf("damaged session provider calls/pending = %d/%d, want 1/0", implementation.CallCount(), implementation.PendingResponses())
	}

	finalProjection := captureTreeForkProjection(agentSession.SessionManager())
	stats, err := agentSession.GetSessionStats()
	if err != nil {
		t.Fatalf("damaged session stats: %v", err)
	}
	state := agentSession.State()
	finalMessages, err := normalizeWorkflowAgentMessages(state.Active.Messages())
	if err != nil {
		t.Fatalf("normalize damaged session final messages: %v", err)
	}
	selectedRef, selectedOK := agentSession.SelectedModel()
	if !selectedOK {
		t.Fatal("damaged session lost selected model")
	}
	finalIsStreaming := agentSession.Activity().IsStreaming
	finalPendingMessageCount := agentSession.PendingMessageCount()
	finalThinkingLevel := agentSession.ThinkingLevel()
	finalActiveTools := agentSession.ActiveToolNames()
	finalSystemPrompt := agentSession.SystemPrompt()
	afterData, err := os.ReadFile(sessionFile)
	if err != nil {
		t.Fatalf("read continued damaged session: %v", err)
	}
	eventMu.Lock()
	events := append([]agent.SessionEvent(nil), observed...)
	eventMu.Unlock()
	unsubscribe()

	entryIDs := workflowEntryIDs(finalProjection.entries)
	entryIDs["missing-parent"] = "<missing-parent>"
	providerInputs, err := normalizeWorkflowProviderInputs(implementation.Requests(), scenarioRoot, cwd, scenario.SessionID)
	if err != nil {
		t.Fatalf("normalize damaged session provider inputs: %v", err)
	}
	normalizedEvents, err := normalizeWorkflowEvents(events, entryIDs)
	if err != nil {
		t.Fatalf("normalize damaged session events: %v", err)
	}
	physicalLinesBefore, err := normalizeDamagedPhysicalLines(sourceData, finalProjection.entries, entryIDs, scenarioRoot, cwd)
	if err != nil {
		t.Fatalf("normalize damaged source lines: %v", err)
	}
	physicalLinesAfter, err := normalizeDamagedPhysicalLines(afterData, finalProjection.entries, entryIDs, scenarioRoot, cwd)
	if err != nil {
		t.Fatalf("normalize continued damaged lines: %v", err)
	}

	if err := runtimeHost.Dispose(context.Background()); err != nil {
		t.Fatalf("dispose damaged session runtime: %v", err)
	}
	runtimeOwned = false
	reopened, err := session.OpenSessionManager(sessionFile, sessionDir, "")
	if err != nil {
		t.Fatalf("reopen continued damaged session: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	reopenedProjection := captureTreeForkProjection(reopened)

	normalizedBeforeResume, err := normalizeDamagedProjection(beforeResume, entryIDs, scenarioRoot, cwd)
	if err != nil {
		t.Fatalf("normalize damaged session before-resume projection: %v", err)
	}
	normalizedFinal, err := normalizeDamagedProjection(finalProjection, entryIDs, scenarioRoot, cwd)
	if err != nil {
		t.Fatalf("normalize damaged session final projection: %v", err)
	}
	normalizedReopened, err := normalizeDamagedProjection(reopenedProjection, entryIDs, scenarioRoot, cwd)
	if err != nil {
		t.Fatalf("normalize damaged session reopened projection: %v", err)
	}
	normalizedFinal["physicalLinesBefore"] = physicalLinesBefore
	normalizedFinal["physicalLinesAfter"] = physicalLinesAfter
	normalizedFinal["reopened"] = normalizedReopened
	actualScenario := map[string]any{
		"name":           scenario.Name,
		"input":          scenario,
		"providerInputs": providerInputs,
		"events":         normalizedEvents,
		"actions": map[string]any{
			"sourcePrefixPreserved":    bytes.HasPrefix(afterData, sourceData),
			"malformedLineCountBefore": damagedMalformedLineCount(physicalLinesBefore),
			"malformedLineCountAfter":  damagedMalformedLineCount(physicalLinesAfter),
		},
		"beforeResume": normalizedBeforeResume,
		"finalState": map[string]any{
			"isStreaming":         finalIsStreaming,
			"pendingMessageCount": finalPendingMessageCount,
			"model": map[string]any{
				"provider": selectedRef.Provider(), "api": selectedRef.API(), "id": selectedRef.ID(),
			},
			"thinkingLevel": string(finalThinkingLevel),
			"activeTools":   finalActiveTools,
			"systemPrompt":  normalizeWorkflowPath(finalSystemPrompt, scenarioRoot, cwd),
			"messages":      finalMessages,
			"stats":         normalizeWorkflowStats(stats),
		},
		"session": normalizedFinal,
	}
	if difference := workflowJSONDifference(
		"damagedSessionScenario",
		expectedScenario,
		canonicalWorkflowJSON(t, actualScenario),
	); difference != "" {
		t.Fatalf("Go damaged-session resume workflow differs from pinned TypeScript oracle: %s", difference)
	}
}

func encodeDamagedSessionSeed(
	cwd string,
	model catalogmodel.Model,
	scenario upstreamDamagedSessionScenario,
) ([]byte, error) {
	zeroCost := map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 0}
	usage := map[string]any{
		"input": scenario.RootResponse.InputTokens, "output": scenario.RootResponse.OutputTokens,
		"cacheRead": 0, "cacheWrite": 0,
		"totalTokens": scenario.RootResponse.InputTokens + scenario.RootResponse.OutputTokens,
		"cost":        zeroCost,
	}
	records := []any{
		map[string]any{
			"type": "session", "version": 3, "id": scenario.SessionID,
			"timestamp": "2026-08-10T00:00:00.000Z", "cwd": cwd,
		},
		map[string]any{
			"type": "model_change", "id": "seed-model", "parentId": nil,
			"timestamp": "2026-08-10T00:00:01.000Z", "provider": model.Provider, "modelId": model.ID,
		},
		map[string]any{
			"type": "thinking_level_change", "id": "seed-thinking", "parentId": "seed-model",
			"timestamp": "2026-08-10T00:00:02.000Z", "thinkingLevel": "off",
		},
		map[string]any{
			"type": "message", "id": "seed-user", "parentId": "seed-thinking",
			"timestamp": "2026-08-10T00:00:03.000Z",
			"message":   map[string]any{"role": "user", "content": scenario.RootPrompt, "timestamp": 1000},
		},
		map[string]any{
			"type": "message", "id": "seed-assistant", "parentId": "seed-user",
			"timestamp": "2026-08-10T00:00:04.000Z",
			"message": map[string]any{
				"role": "assistant", "content": []any{map[string]any{"type": "text", "text": scenario.RootResponse.Text}},
				"api": model.API, "provider": model.Provider, "model": model.ID,
				"usage": usage, "stopReason": "stop", "timestamp": 2000,
			},
		},
		scenario.MalformedLine,
		map[string]any{
			"type": "message", "id": "seed-orphan", "parentId": "missing-parent",
			"timestamp": "2026-08-10T00:00:05.000Z",
			"message":   map[string]any{"role": "user", "content": scenario.OrphanPrompt, "timestamp": 3000},
		},
	}
	var result bytes.Buffer
	for _, record := range records {
		if line, ok := record.(string); ok {
			result.WriteString(line)
			result.WriteByte('\n')
			continue
		}
		line, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		result.Write(line)
		result.WriteByte('\n')
	}
	return result.Bytes(), nil
}

func normalizeDamagedProjection(
	projection treeForkProjection,
	ids map[string]string,
	root, cwd string,
) (map[string]any, error) {
	entries, err := normalizeWorkflowEntries(projection.entries, ids)
	if err != nil {
		return nil, err
	}
	tree, err := normalizeTreeForkTree(projection.tree, ids)
	if err != nil {
		return nil, err
	}
	contextValue, err := normalizeDamagedContext(projection.context)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"header":  normalizeWorkflowHeader(projection.header, root, cwd),
		"leafId":  normalizeTreeForkLeaf(projection.leafID, projection.hasLeaf, ids),
		"entries": entries, "tree": tree, "context": contextValue,
	}, nil
}

func normalizeDamagedContext(value session.Context) (map[string]any, error) {
	messages, err := normalizeWorkflowAgentMessages(value.AgentMessages())
	if err != nil {
		return nil, err
	}
	var normalizedModel any
	if modelValue, hasModel := value.Model(); hasModel {
		normalizedModel = map[string]any{"provider": modelValue.Provider, "modelId": modelValue.ModelID}
	}
	thinkingLevel, hasThinkingLevel := value.ThinkingLevel()
	if !hasThinkingLevel {
		return nil, fmt.Errorf("damaged session context has no thinking level")
	}
	return map[string]any{
		"messages": messages, "model": normalizedModel, "thinkingLevel": thinkingLevel,
	}, nil
}

func normalizeDamagedPhysicalLines(
	data []byte,
	entries []session.Entry,
	ids map[string]string,
	root, cwd string,
) ([]any, error) {
	byID := make(map[string]session.Entry, len(entries))
	for _, entry := range entries {
		byID[entry.ID()] = entry
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	result := make([]any, 0, len(lines))
	for index, line := range lines {
		decoded, err := decodeWorkflowJSON([]byte(line))
		if err != nil {
			result = append(result, map[string]any{"kind": "malformed", "text": line})
			continue
		}
		raw, ok := decoded.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("physical line %d is not an object", index+1)
		}
		if raw["type"] == "session" {
			result = append(result, map[string]any{
				"kind": "header",
				"value": map[string]any{
					"type": raw["type"], "version": raw["version"], "id": raw["id"],
					"cwd": normalizeWorkflowPath(fmt.Sprint(raw["cwd"]), root, cwd),
				},
			})
			continue
		}
		entry, exists := byID[fmt.Sprint(raw["id"])]
		if !exists {
			return nil, fmt.Errorf("physical line %d has unknown entry id %v", index+1, raw["id"])
		}
		normalized, err := normalizeWorkflowEntry(entry, ids)
		if err != nil {
			return nil, fmt.Errorf("physical line %d: %w", index+1, err)
		}
		result = append(result, map[string]any{"kind": "entry", "value": normalized})
	}
	return result, nil
}

func damagedMalformedLineCount(lines []any) int {
	count := 0
	for _, line := range lines {
		object, ok := line.(map[string]any)
		if ok && object["kind"] == "malformed" {
			count++
		}
	}
	return count
}

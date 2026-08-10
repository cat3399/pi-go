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

type upstreamTreeForkScenario struct {
	Name            string                     `json:"name"`
	SourceSessionID string                     `json:"sourceSessionId"`
	SystemPrompt    string                     `json:"systemPrompt"`
	RootPrompt      string                     `json:"rootPrompt"`
	AbandonedPrompt string                     `json:"abandonedPrompt"`
	BranchPrompt    string                     `json:"branchPrompt"`
	Responses       []upstreamTreeForkResponse `json:"responses"`
}

type upstreamTreeForkResponse struct {
	Text         string `json:"text"`
	InputTokens  uint64 `json:"inputTokens"`
	OutputTokens uint64 `json:"outputTokens"`
}

type treeForkPoint struct {
	leafID  string
	hasLeaf bool
	context session.Context
}

type treeForkProjection struct {
	header      session.Header
	leafID      string
	hasLeaf     bool
	entries     []session.Entry
	tree        []session.TreeNode
	context     session.Context
	fileEntries []any
}

// TestUpstreamTreeNavigationRuntimeForkOracle pins the production boundary
// shared by AgentSession.navigateTree(), AgentSessionRuntime.fork(), and the
// v3 JSONL SessionManager. It intentionally uses Runtime.Factory replacement
// instead of constructing the forked transcript in test code.
func TestUpstreamTreeNavigationRuntimeForkOracle(t *testing.T) {
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
	expectedScenario, ok := expectedObject["treeForkScenario"].(map[string]any)
	if !ok {
		t.Fatal("workflow oracle treeForkScenario is not an object")
	}
	scenario := corpus.TreeFork
	if len(scenario.Responses) != 4 {
		t.Fatalf("tree/fork response count = %d, want 4", len(scenario.Responses))
	}

	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	agentDir := filepath.Join(root, "agent")
	sessionDir := filepath.Join(root, "sessions")
	for _, directory := range []string{cwd, agentDir, sessionDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create tree/fork directory: %v", err)
		}
	}

	var clockTick atomic.Int64
	var entrySequence atomic.Uint64
	sourceManager, err := session.CreateSessionManagerWithOptions(cwd, sessionDir, session.ManagerOptions{
		NewSession: session.NewSessionOptions{ID: scenario.SourceSessionID},
		Now: func() time.Time {
			return agentTestEpoch.Add(time.Duration(clockTick.Add(1)) * time.Millisecond)
		},
		NewEntryID: func() (string, error) {
			return fmt.Sprintf("go-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("create source session manager: %v", err)
	}

	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{
		ChunkRunes: 3,
		Clock:      func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		_ = sourceManager.Close()
		t.Fatalf("construct tree/fork provider: %v", err)
	}
	steps := make([]provider.ScriptStep, 0, len(scenario.Responses))
	for index, response := range scenario.Responses {
		message, messageErr := newAssistantTextMessage(
			[]llm.TextBlock{mustTextBlock(t, response.Text)},
			llm.FinishStop,
			mustUsage(t, response.InputTokens, response.OutputTokens),
			agentTestEpoch,
		)
		if messageErr != nil {
			_ = sourceManager.Close()
			t.Fatalf("construct tree/fork response %d: %v", index, messageErr)
		}
		step, stepErr := provider.FixedResponseStep(message)
		if stepErr != nil {
			_ = sourceManager.Close()
			t.Fatalf("construct tree/fork response step %d: %v", index, stepErr)
		}
		steps = append(steps, step)
	}
	if err := implementation.SetResponses(steps); err != nil {
		_ = sourceManager.Close()
		t.Fatalf("set tree/fork responses: %v", err)
	}

	selected := catalogmodel.Model{
		Provider: "faux", API: "anthropic-messages", ID: "faux-1", Name: "Faux Model",
		BaseURL: "http://localhost:0", Input: []provider.InputKind{provider.InputText, provider.InputImage},
		Cost: provider.CostRates{}, ContextWindow: 128_000, MaxTokens: 16_384,
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
		CWD: cwd, AgentDir: agentDir, SessionManager: sourceManager,
	})
	if err != nil {
		_ = sourceManager.Close()
		t.Fatalf("create tree/fork runtime: %v", err)
	}
	runtimeOwned := true
	defer func() {
		if runtimeOwned {
			_ = runtimeHost.Dispose(context.Background())
		}
	}()

	sourceSession := runtimeHost.Session()
	var sourceEventMu sync.Mutex
	var sourceObserved []agent.SessionEvent
	unsubscribeSource := sourceSession.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		sourceEventMu.Lock()
		sourceObserved = append(sourceObserved, event)
		sourceEventMu.Unlock()
	})

	assertTreeForkPrompt(t, sourceSession, scenario.RootPrompt, "root")
	assertTreeForkPrompt(t, sourceSession, scenario.AbandonedPrompt, "abandoned branch")
	users := treeForkUserEntries(sourceSession.SessionManager().Entries())
	if len(users) != 2 {
		t.Fatalf("source user entry count before navigation = %d, want 2", len(users))
	}
	abandonedUser := users[1]
	sourceBeforeNavigation := captureTreeForkPoint(sourceSession.SessionManager())
	navigation, err := sourceSession.NavigateTree(context.Background(), abandonedUser.ID(), agent.NavigateTreeOptions{})
	if err != nil {
		t.Fatalf("navigate source tree: %v", err)
	}
	sourceAfterNavigation := captureTreeForkPoint(sourceSession.SessionManager())
	assertTreeForkPrompt(t, sourceSession, scenario.BranchPrompt, "replacement branch")
	users = treeForkUserEntries(sourceSession.SessionManager().Entries())
	if len(users) != 3 {
		t.Fatalf("source user entry count after navigation = %d, want 3", len(users))
	}
	branchUser := users[2]

	sourceSessionFile, ok := sourceSession.SessionManager().SessionFile()
	if !ok {
		t.Fatal("tree/fork source session has no persistent file")
	}
	sourceStats, err := sourceSession.GetSessionStats()
	if err != nil {
		t.Fatalf("source session stats: %v", err)
	}
	sourceProjection := captureTreeForkProjection(sourceSession.SessionManager())

	forkResult, err := runtimeHost.Fork(context.Background(), branchUser.ID(), agentruntime.ForkOptions{})
	if err != nil {
		t.Fatalf("fork replacement branch: %v", err)
	}
	forkSession := runtimeHost.Session()
	replacedSession := forkSession != sourceSession
	forkSessionFile, ok := forkSession.SessionManager().SessionFile()
	if !ok {
		t.Fatal("tree/fork replacement session has no persistent file")
	}
	forkSessionID := forkSession.SessionManager().SessionID()
	var forkEventMu sync.Mutex
	var forkObserved []agent.SessionEvent
	unsubscribeFork := forkSession.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		forkEventMu.Lock()
		forkObserved = append(forkObserved, event)
		forkEventMu.Unlock()
	})
	if forkResult.SelectedText == nil {
		t.Fatal("fork(before) did not return selected user text")
	}
	assertTreeForkPrompt(t, forkSession, *forkResult.SelectedText, "forked replacement")
	if implementation.CallCount() != 4 || implementation.PendingResponses() != 0 {
		t.Fatalf("tree/fork provider calls/pending = %d/%d, want 4/0", implementation.CallCount(), implementation.PendingResponses())
	}

	forkProjection := captureTreeForkProjection(forkSession.SessionManager())
	forkStats, err := forkSession.GetSessionStats()
	if err != nil {
		t.Fatalf("fork session stats: %v", err)
	}
	state := forkSession.State()
	finalMessages, err := normalizeWorkflowAgentMessages(state.Active.Messages())
	if err != nil {
		t.Fatalf("normalize final fork messages: %v", err)
	}
	selectedRef, selectedOK := forkSession.SelectedModel()
	if !selectedOK {
		t.Fatal("fork session lost selected model")
	}
	finalIsStreaming := forkSession.Activity().IsStreaming
	finalPendingMessageCount := forkSession.PendingMessageCount()
	finalThinkingLevel := forkSession.ThinkingLevel()
	finalActiveTools := forkSession.ActiveToolNames()
	finalSystemPrompt := forkSession.SystemPrompt()

	sourceEventMu.Lock()
	sourceEvents := append([]agent.SessionEvent(nil), sourceObserved...)
	sourceEventMu.Unlock()
	forkEventMu.Lock()
	forkEvents := append([]agent.SessionEvent(nil), forkObserved...)
	forkEventMu.Unlock()
	unsubscribeSource()
	unsubscribeFork()

	entryIDs := make(map[string]string, len(sourceProjection.entries)+len(forkProjection.entries))
	addWorkflowEntryIDs(sourceProjection.entries, entryIDs)
	addWorkflowEntryIDs(forkProjection.entries, entryIDs)

	sourceFileHeader, sourceFileEntries, err := normalizeWorkflowJSONL(sourceSessionFile, entryIDs, root, cwd)
	if err != nil {
		t.Fatalf("normalize source JSONL: %v", err)
	}
	forkFileHeader, forkFileEntries, err := normalizeWorkflowJSONL(forkSessionFile, entryIDs, root, cwd)
	if err != nil {
		t.Fatalf("normalize fork JSONL: %v", err)
	}
	sourceProjection.fileEntries = sourceFileEntries
	forkProjection.fileEntries = forkFileEntries

	providerInputs, err := normalizeWorkflowProviderInputsWithForeignLabel(
		implementation.Requests(), root, cwd, scenario.SourceSessionID, "<fork-session-id>",
	)
	if err != nil {
		t.Fatalf("normalize tree/fork provider inputs: %v", err)
	}
	normalizedSourceEvents, err := normalizeWorkflowEvents(sourceEvents, entryIDs)
	if err != nil {
		t.Fatalf("normalize source events: %v", err)
	}
	normalizedForkEvents, err := normalizeWorkflowEvents(forkEvents, entryIDs)
	if err != nil {
		t.Fatalf("normalize fork events: %v", err)
	}

	if err := runtimeHost.Dispose(context.Background()); err != nil {
		t.Fatalf("dispose tree/fork runtime: %v", err)
	}
	runtimeOwned = false

	reopenedSource, err := session.OpenSessionManager(sourceSessionFile, sessionDir, "")
	if err != nil {
		t.Fatalf("reopen source session: %v", err)
	}
	defer func() { _ = reopenedSource.Close() }()
	reopenedFork, err := session.OpenSessionManager(forkSessionFile, sessionDir, "")
	if err != nil {
		t.Fatalf("reopen fork session: %v", err)
	}
	defer func() { _ = reopenedFork.Close() }()
	reopenedSourceProjection := captureTreeForkProjection(reopenedSource)
	reopenedSourceProjection.fileEntries = sourceFileEntries
	reopenedForkProjection := captureTreeForkProjection(reopenedFork)
	reopenedForkProjection.fileEntries = forkFileEntries

	normalizedSource, err := normalizeTreeForkProjection(sourceProjection, entryIDs, root, cwd, forkSessionID, sourceSessionFile)
	if err != nil {
		t.Fatalf("normalize source projection: %v", err)
	}
	normalizedSourceReopened, err := normalizeTreeForkProjection(reopenedSourceProjection, entryIDs, root, cwd, forkSessionID, sourceSessionFile)
	if err != nil {
		t.Fatalf("normalize reopened source projection: %v", err)
	}
	normalizedSource["stats"] = normalizeWorkflowStats(sourceStats)
	normalizedSource["reopened"] = normalizedSourceReopened
	normalizedFork, err := normalizeTreeForkProjection(forkProjection, entryIDs, root, cwd, forkSessionID, sourceSessionFile)
	if err != nil {
		t.Fatalf("normalize fork projection: %v", err)
	}
	normalizedForkReopened, err := normalizeTreeForkProjection(reopenedForkProjection, entryIDs, root, cwd, forkSessionID, sourceSessionFile)
	if err != nil {
		t.Fatalf("normalize reopened fork projection: %v", err)
	}
	normalizedFork["reopened"] = normalizedForkReopened

	normalizedForkStats := normalizeWorkflowStats(forkStats)
	normalizedForkStats["sessionId"] = "<fork-session-id>"
	normalizedBeforeNavigation, err := normalizeTreeForkPoint(sourceBeforeNavigation, entryIDs)
	if err != nil {
		t.Fatalf("normalize source point before navigation: %v", err)
	}
	normalizedAfterNavigation, err := normalizeTreeForkPoint(sourceAfterNavigation, entryIDs)
	if err != nil {
		t.Fatalf("normalize source point after navigation: %v", err)
	}
	actualScenario := map[string]any{
		"name":           scenario.Name,
		"input":          scenario,
		"providerInputs": providerInputs,
		"sourceEvents":   normalizedSourceEvents,
		"forkEvents":     normalizedForkEvents,
		"actions": map[string]any{
			"navigation": map[string]any{
				"cancelled":  navigation.Cancelled,
				"editorText": navigation.EditorText,
				"targetId":   entryIDs[abandonedUser.ID()],
			},
			"fork": map[string]any{
				"cancelled":         forkResult.Cancelled,
				"selectedText":      forkResult.SelectedText,
				"targetId":          entryIDs[branchUser.ID()],
				"replacedSession":   replacedSession,
				"sourceSessionFile": "<source-session-file>",
				"forkSessionFile":   "<fork-session-file>",
				"forkSessionId":     "<fork-session-id>",
			},
			"sourceBeforeNavigation": normalizedBeforeNavigation,
			"sourceAfterNavigation":  normalizedAfterNavigation,
		},
		"finalState": map[string]any{
			"isStreaming":         finalIsStreaming,
			"pendingMessageCount": finalPendingMessageCount,
			"model": map[string]any{
				"provider": selectedRef.Provider(), "api": selectedRef.API(), "id": selectedRef.ID(),
			},
			"thinkingLevel": string(finalThinkingLevel),
			"activeTools":   finalActiveTools,
			"systemPrompt":  normalizeWorkflowPath(finalSystemPrompt, root, cwd),
			"messages":      finalMessages,
			"stats":         normalizedForkStats,
		},
		"source": normalizedSource,
		"fork":   normalizedFork,
	}
	actual := canonicalWorkflowJSON(t, actualScenario)
	if difference := workflowJSONDifference("treeForkScenario", expectedScenario, actual); difference != "" {
		t.Fatalf("Go tree/navigation/fork workflow differs from pinned TypeScript oracle: %s", difference)
	}
	expectedSourceHeader := expectedScenario["source"].(map[string]any)["header"]
	expectedForkHeader := expectedScenario["fork"].(map[string]any)["header"]
	if difference := workflowJSONDifference(
		"treeForkScenario.source.physicalHeader",
		expectedSourceHeader,
		normalizeTreeForkHeaderMap(sourceFileHeader, root, cwd, forkSessionID, sourceSessionFile),
	); difference != "" {
		t.Fatalf("physical source header differs from pinned TypeScript oracle: %s", difference)
	}
	if difference := workflowJSONDifference(
		"treeForkScenario.fork.physicalHeader",
		expectedForkHeader,
		normalizeTreeForkHeaderMap(forkFileHeader, root, cwd, forkSessionID, sourceSessionFile),
	); difference != "" {
		t.Fatalf("physical fork header differs from pinned TypeScript oracle: %s", difference)
	}
}

func assertTreeForkPrompt(t *testing.T, runtime *agent.AgentSession, prompt, label string) {
	t.Helper()
	outcome, err := runtime.Prompt(context.Background(), prompt)
	if err != nil || !outcome.Succeeded() {
		t.Fatalf("%s prompt = (%#v, %v)", label, outcome, err)
	}
}

func treeForkUserEntries(entries []session.Entry) []session.Entry {
	result := make([]session.Entry, 0)
	for _, entry := range entries {
		message, ok := entry.Message()
		if ok && message.Role() == llm.RoleUser {
			result = append(result, entry)
		}
	}
	return result
}

func captureTreeForkPoint(manager *session.SessionManager) treeForkPoint {
	leafID, hasLeaf := manager.LeafID()
	return treeForkPoint{leafID: leafID, hasLeaf: hasLeaf, context: manager.BuildContext()}
}

func captureTreeForkProjection(manager *session.SessionManager) treeForkProjection {
	leafID, hasLeaf := manager.LeafID()
	return treeForkProjection{
		header: manager.Header(), leafID: leafID, hasLeaf: hasLeaf,
		entries: manager.Entries(), tree: manager.Tree(), context: manager.BuildContext(),
	}
}

func normalizeTreeForkPoint(point treeForkPoint, ids map[string]string) (map[string]any, error) {
	contextValue, err := normalizeTreeForkContext(point.context)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"leafId":  normalizeTreeForkLeaf(point.leafID, point.hasLeaf, ids),
		"context": contextValue,
	}, nil
}

func normalizeTreeForkProjection(
	projection treeForkProjection,
	ids map[string]string,
	root, cwd, forkSessionID, sourceSessionFile string,
) (map[string]any, error) {
	entries, err := normalizeWorkflowEntries(projection.entries, ids)
	if err != nil {
		return nil, err
	}
	tree, err := normalizeTreeForkTree(projection.tree, ids)
	if err != nil {
		return nil, err
	}
	contextValue, err := normalizeTreeForkContext(projection.context)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"header":  normalizeTreeForkHeader(projection.header, root, cwd, forkSessionID, sourceSessionFile),
		"leafId":  normalizeTreeForkLeaf(projection.leafID, projection.hasLeaf, ids),
		"entries": entries, "fileEntries": projection.fileEntries,
		"tree": tree, "context": contextValue,
	}, nil
}

func normalizeTreeForkTree(nodes []session.TreeNode, ids map[string]string) ([]any, error) {
	result := make([]any, 0, len(nodes))
	for index, node := range nodes {
		entry, err := normalizeWorkflowEntry(node.Entry, ids)
		if err != nil {
			return nil, fmt.Errorf("tree node %d: %w", index, err)
		}
		children, err := normalizeTreeForkTree(node.Children, ids)
		if err != nil {
			return nil, err
		}
		var label any
		if node.Label != nil {
			label = *node.Label
		}
		result = append(result, map[string]any{"entry": entry, "label": label, "children": children})
	}
	return result, nil
}

func normalizeTreeForkContext(value session.Context) (map[string]any, error) {
	messages, err := normalizeWorkflowAgentMessages(value.AgentMessages())
	if err != nil {
		return nil, err
	}
	modelValue, hasModel := value.Model()
	thinkingLevel, hasThinkingLevel := value.ThinkingLevel()
	if !hasModel || !hasThinkingLevel {
		return nil, fmt.Errorf("tree/fork context selection = model:%t thinking:%t", hasModel, hasThinkingLevel)
	}
	return map[string]any{
		"messages":      messages,
		"model":         map[string]any{"provider": modelValue.Provider, "modelId": modelValue.ModelID},
		"thinkingLevel": thinkingLevel,
	}, nil
}

func normalizeTreeForkLeaf(leafID string, hasLeaf bool, ids map[string]string) any {
	if !hasLeaf {
		return nil
	}
	return ids[leafID]
}

func normalizeTreeForkHeader(header session.Header, root, cwd, forkSessionID, sourceSessionFile string) map[string]any {
	result := normalizeWorkflowHeader(header, root, cwd)
	if parentSession, ok := header.ParentSession(); ok {
		result["parentSession"] = normalizeWorkflowPath(parentSession, root, cwd)
	}
	return normalizeTreeForkHeaderMap(result, root, cwd, forkSessionID, sourceSessionFile)
}

func normalizeTreeForkHeaderMap(header map[string]any, root, cwd, forkSessionID, sourceSessionFile string) map[string]any {
	result := make(map[string]any, len(header))
	for key, value := range header {
		result[key] = value
	}
	if fmt.Sprint(result["id"]) == forkSessionID {
		result["id"] = "<fork-session-id>"
	}
	if parentSession, ok := result["parentSession"]; ok {
		if fmt.Sprint(parentSession) == normalizeWorkflowPath(sourceSessionFile, root, cwd) {
			result["parentSession"] = "<source-session-file>"
		}
	}
	return result
}

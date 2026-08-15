package agent_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	catalogmodel "github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/provider"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
)

//go:embed testdata/upstream_workflow_corpus.json
var upstreamWorkflowCorpusJSON []byte

//go:embed testdata/upstream_workflow_oracle.json
var upstreamWorkflowOracleJSON []byte

type upstreamWorkflowCorpus struct {
	UpstreamCommit     string                             `json:"upstreamCommit"`
	NodeVersion        string                             `json:"nodeVersion"`
	Scenario           upstreamWorkflowScenario           `json:"scenario"`
	QueueAbortScenario upstreamQueueAbortScenario         `json:"queueAbortScenario"`
	RetryScenario      upstreamRetryScenario              `json:"retryScenario"`
	ModelControl       upstreamModelControlScenario       `json:"modelControlScenario"`
	RetryAbort         upstreamRetryAbortScenario         `json:"retryAbortScenario"`
	ManualCompaction   upstreamManualCompactionScenario   `json:"manualCompactionScenario"`
	OverflowCompaction upstreamOverflowCompactionScenario `json:"overflowCompactionScenario"`
	TurnSnapshot       upstreamTurnSnapshotScenario       `json:"turnSnapshotScenario"`
	TreeFork           upstreamTreeForkScenario           `json:"treeForkScenario"`
	DamagedSession     upstreamDamagedSessionScenario     `json:"damagedSessionScenario"`
	RequestAssembly    upstreamRequestAssemblyScenario    `json:"requestAssemblyScenario"`
}

type upstreamWorkflowScenario struct {
	Name         string `json:"name"`
	SessionID    string `json:"sessionId"`
	SystemPrompt string `json:"systemPrompt"`
	FirstPrompt  string `json:"firstPrompt"`
	Image        struct {
		MIMEType string `json:"mimeType"`
		Base64   string `json:"base64"`
	} `json:"image"`
	Tool struct {
		Name        string `json:"name"`
		CallID      string `json:"callId"`
		Description string `json:"description"`
		Argument    string `json:"argument"`
		Result      string `json:"result"`
	} `json:"tool"`
	SecondPrompt string `json:"secondPrompt"`
	Responses    []struct {
		Text         string `json:"text"`
		ToolCall     bool   `json:"toolCall"`
		InputTokens  uint64 `json:"inputTokens"`
		OutputTokens uint64 `json:"outputTokens"`
	} `json:"responses"`
}

// TestUpstreamAgentSessionWorkflowOracle is intentionally a workflow-level
// contract. The expected value is produced by the pinned TypeScript
// createAgentSession() implementation; the Go side runs the production
// CreateAgentSession factory with deterministic provider/tool inputs.
func TestUpstreamAgentSessionWorkflowOracle(t *testing.T) {
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
	if expectedObject["upstreamCommit"] != corpus.UpstreamCommit {
		t.Fatalf("oracle commit = %v, corpus commit = %s", expectedObject["upstreamCommit"], corpus.UpstreamCommit)
	}
	expectedScenario, ok := expectedObject["scenario"].(map[string]any)
	if !ok {
		t.Fatal("workflow oracle scenario is not an object")
	}
	if len(corpus.Scenario.Responses) != 3 || !corpus.Scenario.Responses[0].ToolCall {
		t.Fatal("workflow corpus no longer describes the three-response tool scenario")
	}

	root := t.TempDir()
	cwd := filepath.Join(root, "project")
	agentDir := filepath.Join(root, "agent")
	sessionDir := filepath.Join(root, "sessions")
	for _, directory := range []string{cwd, agentDir, sessionDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create workflow directory: %v", err)
		}
	}

	var clockTick atomic.Int64
	var entrySequence atomic.Uint64
	manager, err := session.CreateSessionManagerWithOptions(cwd, sessionDir, session.ManagerOptions{
		NewSession: session.NewSessionOptions{ID: corpus.Scenario.SessionID},
		Now: func() time.Time {
			return agentTestEpoch.Add(time.Duration(clockTick.Add(1)) * time.Millisecond)
		},
		NewEntryID: func() (string, error) {
			return fmt.Sprintf("go-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("create workflow session manager: %v", err)
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
	definition, err := provider.NewToolDefinition(
		corpus.Scenario.Tool.Name,
		corpus.Scenario.Tool.Description,
		false,
		[]byte(`{"type":"object","required":["text"],"properties":{"text":{"type":"string"}},"additionalProperties":false}`),
	)
	if err != nil {
		t.Fatalf("construct workflow tool definition: %v", err)
	}

	implementation, err := provider.NewScriptedProvider(provider.ScriptedConfig{
		ChunkRunes: 3,
		Clock:      func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatalf("construct workflow provider: %v", err)
	}
	toolArguments, err := json.Marshal(map[string]string{"text": corpus.Scenario.Tool.Argument})
	if err != nil {
		t.Fatalf("encode workflow tool arguments: %v", err)
	}
	toolCall, err := llm.NewToolCallBlock(corpus.Scenario.Tool.CallID, corpus.Scenario.Tool.Name, toolArguments)
	if err != nil {
		t.Fatalf("construct workflow tool call: %v", err)
	}
	toolResponse := corpus.Scenario.Responses[0]
	toolTerminal, err := newAssistantToolUseMessage(
		[]llm.AssistantBlock{mustTextBlock(t, toolResponse.Text), toolCall},
		mustUsage(t, toolResponse.InputTokens, toolResponse.OutputTokens),
		agentTestEpoch,
	)
	if err != nil {
		t.Fatalf("construct workflow tool response: %v", err)
	}
	steps := make([]provider.ScriptStep, 0, len(corpus.Scenario.Responses))
	for index, response := range corpus.Scenario.Responses {
		var terminal llm.AssistantTerminal
		if response.ToolCall {
			terminal = toolTerminal
		} else {
			message, messageErr := newAssistantTextMessage(
				[]llm.TextBlock{mustTextBlock(t, response.Text)},
				llm.FinishStop,
				mustUsage(t, response.InputTokens, response.OutputTokens),
				agentTestEpoch,
			)
			if messageErr != nil {
				t.Fatalf("construct response %d: %v", index, messageErr)
			}
			terminal = message
		}
		step, stepErr := provider.FixedResponseStep(terminal)
		if stepErr != nil {
			t.Fatalf("construct response step %d: %v", index, stepErr)
		}
		steps = append(steps, step)
	}
	if err := implementation.SetResponses(steps); err != nil {
		t.Fatalf("set workflow responses: %v", err)
	}

	var toolRunMu sync.Mutex
	var toolRuns []any
	echo := &fakeTool{name: corpus.Scenario.Tool.Name, executeWithID: func(_ context.Context, toolCallID string, arguments []byte, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		decoded, decodeErr := decodeWorkflowJSON(arguments)
		if decodeErr != nil {
			return agent.ToolOutput{}, decodeErr
		}
		toolRunMu.Lock()
		toolRuns = append(toolRuns, map[string]any{"toolCallId": toolCallID, "arguments": decoded})
		toolRunMu.Unlock()
		return agent.ToolOutput{
			Text: corpus.Scenario.Tool.Result,
			Details: map[string]any{
				"text": corpus.Scenario.Tool.Argument,
			},
		}, nil
	}}

	disabled := false
	off := provider.ThinkingOff
	systemPrompt := corpus.Scenario.SystemPrompt + "\nCurrent working directory: " + cwd
	created, err := agentruntime.CreateAgentSession(context.Background(), agentruntime.SessionFactoryOptions{
		Services: &agentruntime.Services{
			CWD: cwd, AgentDir: agentDir, Provider: implementation, Tool: echo,
			Tools: []provider.ToolDefinition{definition},
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
			Retry:      catalogmodel.RetrySettings{Enabled: &disabled},
		},
		BaseConfig: agent.SessionConfig{
			SystemPrompt: systemPrompt,
			Tool:         echo,
			Tools:        []provider.ToolDefinition{definition},
			AllTools:     []provider.ToolDefinition{definition},
			Stream: provider.StreamOptions{
				SessionID: corpus.Scenario.SessionID,
				Transport: provider.TransportSSE,
			},
			Now:               func() time.Time { return agentTestEpoch },
			SettlementTimeout: time.Second,
		},
	})
	if err != nil {
		t.Fatalf("create Go workflow AgentSession: %v", err)
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

	imageData, err := base64.StdEncoding.DecodeString(corpus.Scenario.Image.Base64)
	if err != nil {
		t.Fatalf("decode workflow image: %v", err)
	}
	textBlock := mustTextBlock(t, corpus.Scenario.FirstPrompt)
	imageBlock, err := llm.NewImageDataBlock(corpus.Scenario.Image.MIMEType, imageData)
	if err != nil {
		t.Fatalf("construct workflow image: %v", err)
	}
	first, err := runtime.PromptContent(context.Background(), []llm.UserContentBlock{textBlock, imageBlock})
	if err != nil || !first.Succeeded() {
		t.Fatalf("first workflow prompt = (%#v, %v)", first, err)
	}
	second, err := runtime.Prompt(context.Background(), corpus.Scenario.SecondPrompt)
	if err != nil || !second.Succeeded() {
		t.Fatalf("second workflow prompt = (%#v, %v)", second, err)
	}
	if implementation.CallCount() != 3 || implementation.PendingResponses() != 0 {
		t.Fatalf("workflow provider calls/pending = %d/%d, want 3/0", implementation.CallCount(), implementation.PendingResponses())
	}
	if echo.CallCount() != 1 {
		t.Fatalf("workflow tool calls = %d, want 1", echo.CallCount())
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
		t.Fatal("workflow manager has no persistent session file")
	}
	stats, err := runtime.GetSessionStats()
	if err != nil {
		t.Fatalf("workflow stats: %v", err)
	}
	providerInputs, err := normalizeWorkflowProviderInputs(implementation.Requests(), root, cwd, corpus.Scenario.SessionID)
	if err != nil {
		t.Fatalf("normalize workflow provider inputs: %v", err)
	}
	normalizedEvents, err := normalizeWorkflowEvents(events, entryIDs)
	if err != nil {
		t.Fatalf("normalize workflow events: %v", err)
	}
	state := runtime.State()
	selectedRef, selectedOK := runtime.SelectedModel()
	if !selectedOK {
		t.Fatal("workflow session lost its selected model")
	}
	finalMessages, err := normalizeWorkflowAgentMessages(state.Active.Messages())
	if err != nil {
		t.Fatalf("normalize final workflow messages: %v", err)
	}
	finalIsStreaming := runtime.Activity().IsStreaming
	finalPendingMessageCount := runtime.PendingMessageCount()
	finalThinkingLevel := runtime.ThinkingLevel()
	finalActiveTools := runtime.ActiveToolNames()
	finalSystemPrompt := runtime.SystemPrompt()
	normalizedEntries, err := normalizeWorkflowEntries(entries, entryIDs)
	if err != nil {
		t.Fatalf("normalize workflow entries: %v", err)
	}
	normalizedStats := normalizeWorkflowStats(stats)

	toolRunMu.Lock()
	normalizedToolRuns := append([]any(nil), toolRuns...)
	toolRunMu.Unlock()
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("close workflow runtime: %v", err)
	}
	runtimeOwned = false
	managerOwned = false

	fileHeader, fileEntries, err := normalizeWorkflowJSONL(sessionFile, entryIDs, root, cwd)
	if err != nil {
		t.Fatalf("normalize workflow JSONL: %v", err)
	}
	reopened, err := session.OpenSessionManagerWithOptions(sessionFile, sessionDir, "", session.ManagerOptions{
		Now: func() time.Time { return agentTestEpoch },
		NewEntryID: func() (string, error) {
			return fmt.Sprintf("reopened-entry-%d", entrySequence.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("reopen workflow session: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	reopenedEntries, err := normalizeWorkflowEntries(reopened.Entries(), entryIDs)
	if err != nil {
		t.Fatalf("normalize reopened entries: %v", err)
	}
	reopenedContext := reopened.BuildContext()
	reopenedMessages, err := normalizeWorkflowAgentMessages(reopenedContext.AgentMessages())
	if err != nil {
		t.Fatalf("normalize reopened messages: %v", err)
	}
	reopenedModel, hasReopenedModel := reopenedContext.Model()
	reopenedThinking, hasReopenedThinking := reopenedContext.ThinkingLevel()
	if !hasReopenedModel || !hasReopenedThinking {
		t.Fatalf("reopened workflow selection = model:%t thinking:%t", hasReopenedModel, hasReopenedThinking)
	}

	actualScenario := map[string]any{
		"name":           corpus.Scenario.Name,
		"input":          corpus.Scenario,
		"providerInputs": providerInputs,
		"toolRuns":       normalizedToolRuns,
		"events":         normalizedEvents,
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
			"stats":         normalizedStats,
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
	if difference := workflowJSONDifference("scenario", expectedScenario, canonicalWorkflowJSON(t, actualScenario)); difference != "" {
		t.Fatalf("Go AgentSession workflow differs from pinned TypeScript oracle: %s", difference)
	}
	if difference := workflowJSONDifference("scenario.session.header", expectedScenario["session"].(map[string]any)["header"], fileHeader); difference != "" {
		t.Fatalf("physical workflow header differs from pinned TypeScript oracle: %s", difference)
	}
}

func decodeWorkflowJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func canonicalWorkflowJSON(t *testing.T, value any) any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal workflow value: %v", err)
	}
	canonical, err := decodeWorkflowJSON(data)
	if err != nil {
		t.Fatalf("canonicalize workflow value: %v", err)
	}
	return canonical
}

func normalizeWorkflowPath(value, root, cwd string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, cwd, "<cwd>"), root, "<root>")
}

func normalizeWorkflowMessage(message llm.ConversationMessage) (map[string]any, error) {
	wrapped, err := agentmsg.NewLLM(message)
	if err != nil {
		return nil, err
	}
	raw, err := session.MarshalAgentMessage(wrapped)
	if err != nil {
		return nil, err
	}
	decoded, err := decodeWorkflowJSON(raw)
	if err != nil {
		return nil, err
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("message %T did not encode as an object", message)
	}
	return normalizeWorkflowMessageObject(object)
}

func normalizeWorkflowMessageObject(object map[string]any) (map[string]any, error) {
	delete(object, "timestamp")
	if object["role"] == "user" {
		if text, ok := object["content"].(string); ok {
			object["content"] = []any{map[string]any{"type": "text", "text": text}}
		}
	}
	return object, nil
}

func normalizeWorkflowMessages(messages []llm.ConversationMessage) ([]any, error) {
	result := make([]any, 0, len(messages))
	for index, message := range messages {
		normalized, err := normalizeWorkflowMessage(message)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", index, err)
		}
		result = append(result, normalized)
	}
	return result, nil
}

func normalizeWorkflowAgentMessages(messages []agentmsg.Message) ([]any, error) {
	result := make([]any, 0, len(messages))
	for index, message := range messages {
		var normalized map[string]any
		switch value := message.(type) {
		case agentmsg.LLM:
			var err error
			normalized, err = normalizeWorkflowMessage(value.Conversation())
			if err != nil {
				return nil, fmt.Errorf("agent message %d: %w", index, err)
			}
		case agentmsg.CompactionSummary:
			normalized = map[string]any{
				"role": "compactionSummary", "summary": value.Summary, "tokensBefore": value.TokensBefore,
			}
		default:
			return nil, fmt.Errorf("agent message %d has unsupported type %T", index, message)
		}
		result = append(result, normalized)
	}
	return result, nil
}

func normalizeWorkflowProviderInputs(requests []provider.Request, root, cwd, sessionID string) ([]any, error) {
	return normalizeWorkflowProviderInputsWithForeignLabel(requests, root, cwd, sessionID, "<summary-session-id>")
}

func normalizeWorkflowProviderInputsWithThinkingBudgets(requests []provider.Request, root, cwd, sessionID string) ([]any, error) {
	result, err := normalizeWorkflowProviderInputs(requests, root, cwd, sessionID)
	if err != nil {
		return nil, err
	}
	for index, request := range requests {
		input := result[index].(map[string]any)
		stream := input["stream"].(map[string]any)
		budgets := request.StreamOptions().ThinkingBudgets
		if budgets == nil {
			stream["thinkingBudgets"] = nil
			continue
		}
		normalized := make(map[string]any, len(budgets))
		for level, value := range budgets {
			normalized[string(level)] = value
		}
		stream["thinkingBudgets"] = normalized
	}
	return result, nil
}

func normalizeWorkflowProviderInputsWithForeignLabel(requests []provider.Request, root, cwd, sessionID, foreignSessionIDLabel string) ([]any, error) {
	result := make([]any, 0, len(requests))
	for requestIndex, request := range requests {
		messages, err := normalizeWorkflowMessages(request.Messages())
		if err != nil {
			return nil, fmt.Errorf("request %d messages: %w", requestIndex, err)
		}
		tools := make([]any, 0, len(request.Tools()))
		for toolIndex, definition := range request.Tools() {
			parameters, err := decodeWorkflowJSON(definition.ParametersJSON())
			if err != nil {
				return nil, fmt.Errorf("request %d tool %d schema: %w", requestIndex, toolIndex, err)
			}
			tools = append(tools, map[string]any{
				"name": definition.Name(), "description": definition.Description(), "parameters": parameters,
			})
		}
		stream := request.StreamOptions()
		requestSessionID := stream.SessionID
		if requestSessionID != "" && requestSessionID != sessionID {
			requestSessionID = foreignSessionIDLabel
		}
		var reasoning any
		if level := request.ThinkingLevel(); level != "" && level != provider.ThinkingOff {
			reasoning = string(level)
		}
		model := request.Model()
		result = append(result, map[string]any{
			"model":        map[string]any{"provider": model.Provider(), "api": model.API(), "id": model.ID()},
			"systemPrompt": normalizeWorkflowPath(request.SystemPrompt(), root, cwd),
			"messages":     messages,
			"tools":        tools,
			"stream": map[string]any{
				"sessionId": requestSessionID, "reasoning": reasoning, "transport": string(stream.Transport),
			},
		})
	}
	return result, nil
}

func normalizeWorkflowEvents(events []agent.SessionEvent, ids map[string]string) ([]any, error) {
	result := make([]any, 0, len(events))
	for index, event := range events {
		normalized, err := normalizeWorkflowEvent(event, ids)
		if err != nil {
			return nil, fmt.Errorf("event %d: %w", index, err)
		}
		result = append(result, normalized)
	}
	return result, nil
}

func normalizeWorkflowEvent(event agent.SessionEvent, ids map[string]string) (map[string]any, error) {
	switch value := event.(type) {
	case agent.AgentStartEvent:
		return map[string]any{"type": "agent_start"}, nil
	case agent.TurnStartEvent:
		return map[string]any{"type": "turn_start"}, nil
	case agent.AgentSettledEvent:
		return map[string]any{"type": "agent_settled"}, nil
	case agent.SessionAgentEndEvent:
		roles := make([]any, 0, len(value.Messages))
		for _, message := range value.Messages {
			roles = append(roles, string(message.Role()))
		}
		return map[string]any{"type": "agent_end", "messageRoles": roles, "willRetry": value.WillRetry}, nil
	case agent.TurnEndEvent:
		toolRoles := make([]any, 0, len(value.ToolResults))
		for _, message := range value.ToolResults {
			toolRoles = append(toolRoles, string(message.Role()))
		}
		return map[string]any{
			"type": "turn_end", "messageRole": string(value.Message.Role()), "toolResultRoles": toolRoles,
		}, nil
	case agent.MessageStartEvent:
		return map[string]any{"type": "message_start", "role": string(value.Message.Role())}, nil
	case agent.MessageUpdateEvent:
		providerEvent, err := normalizeWorkflowProviderEvent(value.AssistantMessageEvent.Event())
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"type": "message_update", "role": string(value.Message.Role()), "providerEvent": providerEvent,
		}, nil
	case agent.MessageEndEvent:
		wrapped, ok := value.Message.(agentmsg.LLM)
		if !ok {
			return nil, fmt.Errorf("message_end has unsupported message %T", value.Message)
		}
		message, err := normalizeWorkflowMessage(wrapped.Conversation())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "message_end", "message": message}, nil
	case agent.ToolExecutionStartEvent:
		arguments, err := decodeWorkflowJSON(value.Arguments)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"type": "tool_execution_start", "toolCallId": value.ToolCallID,
			"toolName": value.ToolName, "arguments": arguments,
		}, nil
	case agent.ToolExecutionUpdateEvent:
		arguments, err := decodeWorkflowJSON(value.Arguments)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"type": "tool_execution_update", "toolCallId": value.ToolCallID,
			"toolName": value.ToolName, "arguments": arguments,
			"partialResult": normalizeWorkflowToolOutput(agent.ToolOutput{
				Text: value.PartialResult.Text, Content: value.PartialResult.Content,
				Details: value.PartialResult.Details, Usage: value.PartialResult.Usage,
				AddedToolNames: value.PartialResult.AddedToolNames, Terminate: value.PartialResult.Terminate,
			}),
		}, nil
	case agent.ToolExecutionEndEvent:
		return map[string]any{
			"type": "tool_execution_end", "toolCallId": value.ToolCallID,
			"toolName": value.ToolName, "result": normalizeWorkflowToolOutput(value.Result), "isError": value.IsError,
		}, nil
	case agent.EntryAppendedEvent:
		entry, err := normalizeWorkflowEntry(value.Entry, ids)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "entry_appended", "entry": entry}, nil
	case agent.SessionQueueUpdateEvent:
		return map[string]any{"type": "queue_update", "steering": value.Steering, "followUp": value.FollowUp}, nil
	case agent.ThinkingLevelChangedEvent:
		return map[string]any{"type": "thinking_level_changed", "level": string(value.Level)}, nil
	case agent.AutoRetryStartEvent:
		return map[string]any{
			"type": "auto_retry_start", "attempt": value.Attempt, "maxAttempts": value.MaxAttempts,
			"delayMs": value.Delay.Milliseconds(), "errorMessage": value.ErrorMessage,
		}, nil
	case agent.AutoRetryEndEvent:
		normalized := map[string]any{
			"type": "auto_retry_end", "success": value.Success, "attempt": value.Attempt,
		}
		if value.FinalError != "" {
			normalized["finalError"] = value.FinalError
		}
		return normalized, nil
	case agent.CompactionStartEvent:
		return map[string]any{"type": "compaction_start", "reason": value.Reason.String()}, nil
	case agent.CompactionEndEvent:
		normalized := map[string]any{
			"type": "compaction_end", "reason": value.Reason.String(),
			"aborted": value.Aborted, "willRetry": value.WillRetry,
		}
		if value.Result != nil {
			result, err := normalizeWorkflowCompactionResult(*value.Result, ids)
			if err != nil {
				return nil, err
			}
			normalized["result"] = result
		}
		if value.ErrorMessage != "" {
			normalized["errorMessage"] = value.ErrorMessage
		}
		return normalized, nil
	default:
		return nil, fmt.Errorf("unsupported workflow event %T (%s)", event, event.Type())
	}
}

func normalizeWorkflowProviderEvent(event llm.StreamEvent) (map[string]any, error) {
	switch value := event.(type) {
	case llm.TextStartEvent:
		return map[string]any{"type": "text_start", "contentIndex": value.ContentIndex()}, nil
	case llm.TextDeltaEvent:
		return map[string]any{"type": "text_delta", "contentIndex": value.ContentIndex(), "delta": value.Delta()}, nil
	case llm.TextEndEvent:
		return map[string]any{"type": "text_end", "contentIndex": value.ContentIndex(), "content": value.Content()}, nil
	case llm.ToolCallStartEvent:
		return map[string]any{"type": "toolcall_start", "contentIndex": value.ContentIndex()}, nil
	case llm.ToolCallDeltaEvent:
		return map[string]any{"type": "toolcall_delta", "contentIndex": value.ContentIndex(), "delta": string(value.Delta())}, nil
	case llm.ToolCallEndEvent:
		call := value.ToolCall()
		arguments, err := decodeWorkflowJSON(call.ArgumentsJSON())
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"type": "toolcall_end", "contentIndex": value.ContentIndex(),
			"toolCall": map[string]any{
				"type": "toolCall", "id": call.ID(), "name": call.Name(), "arguments": arguments,
			},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported provider update event %T", event)
	}
}

func normalizeWorkflowToolOutput(output agent.ToolOutput) map[string]any {
	content := make([]any, 0, max(1, len(output.Content)))
	if output.Content == nil {
		content = append(content, map[string]any{"type": "text", "text": output.Text})
	} else {
		for _, block := range output.Content {
			switch value := block.(type) {
			case llm.TextBlock:
				content = append(content, map[string]any{"type": "text", "text": value.Text()})
			case llm.ImageBlock:
				content = append(content, map[string]any{
					"type": "image", "mimeType": value.MediaType(), "data": base64.StdEncoding.EncodeToString(value.Data()),
				})
			}
		}
	}
	return map[string]any{"content": content, "details": output.Details}
}

func normalizeWorkflowCompactionUsage(value *session.CompactionUsage) any {
	if value == nil {
		return nil
	}
	usage := value.Usage
	return map[string]any{
		"input": usage.Input(), "output": usage.Output(),
		"cacheRead": usage.CacheRead(), "cacheWrite": usage.CacheWrite(),
		"totalTokens": usage.TotalTokens(),
		"cost": map[string]any{
			"input": value.Cost.Input, "output": value.Cost.Output,
			"cacheRead": value.Cost.CacheRead, "cacheWrite": value.Cost.CacheWrite,
			"total": value.Cost.Total,
		},
	}
}

func normalizeWorkflowCompactionResult(value session.CompactResult, ids map[string]string) (map[string]any, error) {
	firstKeptEntryID := value.Input.FirstKeptEntryID
	tokensBefore := value.Input.TokensBefore
	if value.Output.FromExtension {
		firstKeptEntryID = value.Output.FirstKeptEntryID
		tokensBefore = value.Output.TokensBefore
	}
	firstKept, ok := ids[firstKeptEntryID]
	if !ok {
		return nil, fmt.Errorf("compaction first kept id %q is outside normalized log", firstKeptEntryID)
	}
	normalized := map[string]any{
		"summary": value.Output.Text, "firstKeptEntryId": firstKept,
		"tokensBefore": tokensBefore, "estimatedTokensAfter": value.EstimatedTokensAfter,
	}
	if value.Output.Usage != nil {
		normalized["usage"] = normalizeWorkflowCompactionUsage(value.Output.Usage)
	}
	if len(value.Output.Details) != 0 {
		details, err := decodeWorkflowJSON(value.Output.Details)
		if err != nil {
			return nil, fmt.Errorf("compaction result details: %w", err)
		}
		normalized["details"] = details
	}
	return normalized, nil
}

func workflowEntryIDs(entries []session.Entry) map[string]string {
	ids := make(map[string]string, len(entries))
	addWorkflowEntryIDs(entries, ids)
	return ids
}

func addWorkflowEntryIDs(entries []session.Entry, ids map[string]string) {
	for _, entry := range entries {
		if _, exists := ids[entry.ID()]; exists {
			continue
		}
		ids[entry.ID()] = fmt.Sprintf("entry-%d", len(ids)+1)
	}
}

func normalizeWorkflowEntries(entries []session.Entry, ids map[string]string) ([]any, error) {
	result := make([]any, 0, len(entries))
	for index, entry := range entries {
		normalized, err := normalizeWorkflowEntry(entry, ids)
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", index, err)
		}
		result = append(result, normalized)
	}
	return result, nil
}

func normalizeWorkflowEntry(entry session.Entry, ids map[string]string) (map[string]any, error) {
	id, ok := ids[entry.ID()]
	if !ok {
		return nil, fmt.Errorf("entry id %q is outside normalized log", entry.ID())
	}
	var parent any
	if parentID, hasParent := entry.ParentID(); hasParent {
		normalizedParent, exists := ids[parentID]
		if !exists {
			return nil, fmt.Errorf("parent id %q is outside normalized log", parentID)
		}
		parent = normalizedParent
	}
	base := map[string]any{"type": entry.Type(), "id": id, "parentId": parent}
	switch payload := entry.Payload().(type) {
	case session.ModelChangePayload:
		base["provider"] = payload.Provider
		base["modelId"] = payload.ModelID
	case session.ThinkingLevelChangePayload:
		base["thinkingLevel"] = payload.ThinkingLevel
	case session.MessagePayload:
		wrapped, ok := payload.Message.(agentmsg.LLM)
		if !ok {
			return nil, fmt.Errorf("message entry contains %T", payload.Message)
		}
		message, err := normalizeWorkflowMessage(wrapped.Conversation())
		if err != nil {
			return nil, err
		}
		base["message"] = message
	case session.CompactionPayload:
		firstKept, exists := ids[payload.Record.FirstKeptEntryID]
		if !exists {
			return nil, fmt.Errorf("compaction first kept id %q is outside normalized log", payload.Record.FirstKeptEntryID)
		}
		base["summary"] = payload.Record.Summary
		base["firstKeptEntryId"] = firstKept
		base["tokensBefore"] = payload.Record.TokensBefore
		if len(payload.Details) != 0 {
			details, err := decodeWorkflowJSON(payload.Details)
			if err != nil {
				return nil, fmt.Errorf("compaction details: %w", err)
			}
			base["details"] = details
		}
		if payload.Record.Usage != nil {
			base["usage"] = normalizeWorkflowCompactionUsage(payload.Record.Usage)
		}
		if payload.HasFromHook {
			base["fromHook"] = payload.FromHook
		}
	default:
		return nil, fmt.Errorf("unsupported workflow entry payload %T", payload)
	}
	return base, nil
}

func normalizeWorkflowHeader(header session.Header, root, cwd string) map[string]any {
	return map[string]any{
		"type": "session", "version": header.Version(), "id": header.ID(),
		"cwd": normalizeWorkflowPath(header.WorkingDir(), root, cwd),
	}
}

func normalizeWorkflowJSONL(path string, ids map[string]string, root, cwd string) (map[string]any, []any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 1 {
		return nil, nil, fmt.Errorf("session JSONL is empty")
	}
	headerValue, err := decodeWorkflowJSON([]byte(lines[0]))
	if err != nil {
		return nil, nil, fmt.Errorf("header: %w", err)
	}
	rawHeader, ok := headerValue.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("header is not an object")
	}
	header := map[string]any{
		"type": rawHeader["type"], "version": rawHeader["version"], "id": rawHeader["id"],
		"cwd": normalizeWorkflowPath(fmt.Sprint(rawHeader["cwd"]), root, cwd),
	}
	if parentSession, exists := rawHeader["parentSession"]; exists {
		header["parentSession"] = normalizeWorkflowPath(fmt.Sprint(parentSession), root, cwd)
	}
	entries := make([]any, 0, len(lines)-1)
	for index, line := range lines[1:] {
		decoded, err := decodeWorkflowJSON([]byte(line))
		if err != nil {
			return nil, nil, fmt.Errorf("entry line %d: %w", index+2, err)
		}
		raw, ok := decoded.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("entry line %d is not an object", index+2)
		}
		id, ok := ids[fmt.Sprint(raw["id"])]
		if !ok {
			return nil, nil, fmt.Errorf("entry line %d has unknown id", index+2)
		}
		var parent any
		if raw["parentId"] != nil {
			var exists bool
			parent, exists = ids[fmt.Sprint(raw["parentId"])]
			if !exists {
				return nil, nil, fmt.Errorf("entry line %d has unknown parent", index+2)
			}
		}
		normalized := map[string]any{"type": raw["type"], "id": id, "parentId": parent}
		switch raw["type"] {
		case "model_change":
			normalized["provider"], normalized["modelId"] = raw["provider"], raw["modelId"]
		case "thinking_level_change":
			normalized["thinkingLevel"] = raw["thinkingLevel"]
		case "message":
			message, ok := raw["message"].(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("entry line %d message is not an object", index+2)
			}
			message, err = normalizeWorkflowMessageObject(message)
			if err != nil {
				return nil, nil, err
			}
			normalized["message"] = message
		case "compaction":
			firstKept, exists := ids[fmt.Sprint(raw["firstKeptEntryId"])]
			if !exists {
				return nil, nil, fmt.Errorf("entry line %d has unknown compaction firstKeptEntryId", index+2)
			}
			normalized["summary"] = raw["summary"]
			normalized["firstKeptEntryId"] = firstKept
			normalized["tokensBefore"] = raw["tokensBefore"]
			if details, exists := raw["details"]; exists {
				normalized["details"] = details
			}
			if usage, exists := raw["usage"]; exists {
				normalized["usage"] = usage
			}
			if fromHook, exists := raw["fromHook"]; exists {
				normalized["fromHook"] = fromHook
			}
		default:
			return nil, nil, fmt.Errorf("entry line %d has unsupported type %v", index+2, raw["type"])
		}
		entries = append(entries, normalized)
	}
	return header, entries, nil
}

func normalizeWorkflowStats(stats agent.SessionStats) map[string]any {
	var sessionFile any
	if stats.SessionFile != nil {
		sessionFile = "<session-file>"
	}
	var contextUsage any
	if stats.ContextUsage != nil {
		var tokens any
		if stats.ContextUsage.Tokens != nil {
			tokens = *stats.ContextUsage.Tokens
		}
		var percent any
		if stats.ContextUsage.Percent != nil {
			percent = *stats.ContextUsage.Percent
		}
		contextUsage = map[string]any{
			"tokens": tokens, "contextWindow": stats.ContextUsage.ContextWindow, "percent": percent,
		}
	}
	return map[string]any{
		"sessionFile": sessionFile, "sessionId": stats.SessionID,
		"userMessages": stats.UserMessages, "assistantMessages": stats.AssistantMessages,
		"toolCalls": stats.ToolCalls, "toolResults": stats.ToolResults, "totalMessages": stats.TotalMessages,
		"tokens": map[string]any{
			"input": stats.Tokens.Input, "output": stats.Tokens.Output,
			"cacheRead": stats.Tokens.CacheRead, "cacheWrite": stats.Tokens.CacheWrite, "total": stats.Tokens.Total,
		},
		"cost": stats.Cost, "contextUsage": contextUsage,
	}
}

func workflowJSONDifference(path string, expected, actual any) string {
	if reflect.DeepEqual(expected, actual) {
		return ""
	}
	switch expectedValue := expected.(type) {
	case map[string]any:
		actualValue, ok := actual.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: expected object, got %T (%v)", path, actual, actual)
		}
		keys := make([]string, 0, len(expectedValue)+len(actualValue))
		seen := make(map[string]struct{}, len(expectedValue)+len(actualValue))
		for key := range expectedValue {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
		for key := range actualValue {
			if _, exists := seen[key]; !exists {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			expectedChild, expectedExists := expectedValue[key]
			actualChild, actualExists := actualValue[key]
			if !expectedExists {
				return fmt.Sprintf("%s.%s: unexpected value %v", path, key, actualChild)
			}
			if !actualExists {
				return fmt.Sprintf("%s.%s: missing; expected %v", path, key, expectedChild)
			}
			if difference := workflowJSONDifference(path+"."+key, expectedChild, actualChild); difference != "" {
				return difference
			}
		}
	case []any:
		actualValue, ok := actual.([]any)
		if !ok {
			return fmt.Sprintf("%s: expected array, got %T (%v)", path, actual, actual)
		}
		if len(expectedValue) != len(actualValue) {
			return fmt.Sprintf("%s: expected length %d, got %d", path, len(expectedValue), len(actualValue))
		}
		for index := range expectedValue {
			if difference := workflowJSONDifference(fmt.Sprintf("%s[%d]", path, index), expectedValue[index], actualValue[index]); difference != "" {
				return difference
			}
		}
	}
	return fmt.Sprintf("%s: expected %T(%v), got %T(%v)", path, expected, expected, actual, actual)
}

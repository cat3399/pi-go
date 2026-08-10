package agent_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

func newTestModel(providerID, api, id string) (provider.Model, error) {
	return newAgentModel(provider.ModelSpec{
		Provider:      providerID,
		API:           api,
		ID:            id,
		Name:          id,
		BaseURL:       "",
		Input:         []provider.InputKind{provider.InputText},
		Cost:          provider.CostRates{},
		ContextWindow: 2,
		MaxTokens:     1,
	})
}

func newAgentModel(spec provider.ModelSpec) (provider.Model, error) {
	if spec.Name == "" {
		spec.Name = spec.ID
	}
	if len(spec.Input) == 0 {
		spec.Input = []provider.InputKind{provider.InputText}
	}
	if spec.ContextWindow == 0 {
		spec.ContextWindow = 2
	}
	if spec.MaxTokens == 0 {
		spec.MaxTokens = 1
	}
	return provider.NewModel(spec)
}

func testAssistantProvenance() llm.AssistantProvenance {
	return llm.AssistantProvenance{Provider: "scripted", API: "scripted", Model: "scripted-1"}
}

func newAssistantTextMessage(content []llm.TextBlock, finish llm.FinishReason, usage llm.Usage, timestamp time.Time) (llm.AssistantTextMessage, error) {
	return llm.NewAssistantTextMessage(content, finish, usage, timestamp, testAssistantProvenance())
}

func newAssistantToolUseMessage(content []llm.AssistantBlock, usage llm.Usage, timestamp time.Time) (llm.AssistantToolUseMessage, error) {
	return llm.NewAssistantToolUseMessage(content, usage, timestamp, testAssistantProvenance())
}

func newAssistantRichMessage(content []llm.AssistantBlock, finish llm.FinishReason, usage llm.Usage, timestamp time.Time) (llm.AssistantRichMessage, error) {
	return llm.NewAssistantRichMessage(content, finish, usage, timestamp, testAssistantProvenance())
}

func newAssistantFailureMessage(content []llm.TextBlock, finish llm.FinishReason, message string, usage llm.Usage, timestamp time.Time) (llm.AssistantFailureMessage, error) {
	return llm.NewAssistantFailureMessage(content, finish, message, usage, timestamp, testAssistantProvenance())
}

func newAssistantFailureMessageWithFailure(content []llm.TextBlock, finish llm.FinishReason, failure llm.Failure, usage llm.Usage, timestamp time.Time) (llm.AssistantFailureMessage, error) {
	return llm.NewAssistantFailureMessageWithFailure(content, finish, failure, usage, timestamp, testAssistantProvenance())
}

func newErrorEventWithFailure(reason llm.FinishReason, failure llm.Failure, usage llm.Usage, timestamp time.Time) (llm.ErrorEvent, error) {
	return llm.NewErrorEventWithFailure(reason, failure, usage, timestamp, testAssistantProvenance())
}

func newStartEvent(t *testing.T) llm.StartEvent {
	t.Helper()
	event, err := llm.NewStartEvent(testAssistantProvenance(), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

var agentTestEpoch = time.Date(2026, time.August, 1, 10, 11, 12, 0, time.UTC)

type fakeTool struct {
	name          string
	execute       func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error)
	executeWithID func(context.Context, string, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error)
	calls         atomic.Uint32
}

// agentEventSnapshot keeps assertions about both public lifecycle and separate
// control-plane metadata concise without widening AgentEvent in production.
type agentEventSnapshot struct {
	Kind                agent.AgentEventType
	RunID               uint64
	Turn                uint32
	Message             llm.ConversationMessage
	AgentMessage        agentmsg.Message
	ProviderSnapshot    llm.StreamSnapshot
	ProviderEvent       llm.StreamEvent
	ToolCallID          string
	ToolName            string
	ToolArguments       []byte
	ToolUpdate          agent.ToolUpdate
	ToolOutput          agent.ToolOutput
	ToolError           error
	Terminal            llm.AssistantTerminal
	RunError            error
	RetryAttempt        uint32
	RetryDelay          time.Duration
	RetryFailureKind    provider.FailureKind
	RetryHTTPStatus     int
	RetrySucceeded      bool
	RetryFinishReason   provider.RetryFinishReason
	CompactionReason    agent.CompactionReason
	CompactionWillRetry bool
	CompactionAborted   bool
	Compaction          *session.CompactResult
}

func snapshotAgentEvent(event any) agentEventSnapshot {
	typed, ok := event.(interface{ Type() agent.AgentEventType })
	if !ok {
		return agentEventSnapshot{}
	}
	result := agentEventSnapshot{Kind: typed.Type()}
	switch value := event.(type) {
	case agent.AgentStartEvent:
		result.RunID = value.RunID
	case agent.AgentEndEvent:
		result.RunID, result.Turn, result.Terminal, result.RunError = value.RunID, value.Turn, value.Terminal, value.Err
	case agent.SessionAgentEndEvent:
		result.Terminal = value.Terminal
	case agent.TurnStartEvent:
		result.RunID, result.Turn = value.RunID, value.Turn
	case agent.TurnEndEvent:
		result.RunID, result.Turn = value.RunID, value.Turn
		if standard, ok := value.Message.(agentmsg.LLM); ok {
			result.Message = standard.Conversation()
			result.Terminal, _ = result.Message.(llm.AssistantTerminal)
		}
	case agent.MessageStartEvent:
		result.RunID, result.Turn, result.AgentMessage = value.RunID, value.Turn, value.Message
		if partial, ok := value.Message.(agentmsg.AssistantPartial); ok {
			result.ProviderSnapshot = partial.Snapshot()
			result.ProviderEvent = partial.ProviderEvent()
		}
	case agent.MessageUpdateEvent:
		result.RunID, result.Turn, result.AgentMessage = value.RunID, value.Turn, value.Message
		result.ProviderEvent = value.AssistantMessageEvent.Event()
		result.ProviderSnapshot = value.AssistantMessageEvent.Partial().Snapshot()
	case agent.MessageEndEvent:
		result.RunID, result.Turn, result.AgentMessage = value.RunID, value.Turn, value.Message
		if standard, ok := value.Message.(agentmsg.LLM); ok {
			result.Message = standard.Conversation()
			result.Terminal, _ = result.Message.(llm.AssistantTerminal)
		}
	case agent.ToolExecutionStartEvent:
		result.RunID, result.Turn = value.RunID, value.Turn
		result.ToolCallID, result.ToolName, result.ToolArguments = value.ToolCallID, value.ToolName, value.Arguments
	case agent.ToolExecutionUpdateEvent:
		result.RunID, result.Turn = value.RunID, value.Turn
		result.ToolCallID, result.ToolName, result.ToolArguments, result.ToolUpdate = value.ToolCallID, value.ToolName, value.Arguments, value.PartialResult
	case agent.ToolExecutionEndEvent:
		result.RunID, result.Turn = value.RunID, value.Turn
		result.ToolCallID, result.ToolName, result.ToolArguments = value.ToolCallID, value.ToolName, value.Arguments
		result.ToolOutput, result.ToolError = value.Result, value.Err
	case agent.QueueUpdateEvent:
		result.RunID, result.Turn = value.RunID, value.Turn
	case agent.CompactionStartEvent:
		result.RunID, result.Turn = value.RunID, value.Turn
		result.CompactionReason, result.CompactionWillRetry = value.Reason, value.WillRetry
	case agent.CompactionEndEvent:
		result.RunID, result.Turn = value.RunID, value.Turn
		result.CompactionReason, result.CompactionWillRetry = value.Reason, value.WillRetry
		result.CompactionAborted, result.Compaction, result.RunError = value.Aborted, value.Result, value.Err
	case agent.ProviderRetryScheduledEvent:
		result.RunID, result.Turn, result.RetryAttempt, result.RetryDelay = value.RunID, value.Turn, value.Attempt, value.Delay
		result.RetryFailureKind, result.RetryHTTPStatus = value.FailureKind, value.HTTPStatus
	case agent.ProviderRetryAttemptEvent:
		result.RunID, result.Turn, result.RetryAttempt = value.RunID, value.Turn, value.Attempt
	case agent.ProviderRetryFinishedEvent:
		result.RunID, result.Turn, result.RetryAttempt = value.RunID, value.Turn, value.Attempt
		result.RetryFailureKind, result.RetryHTTPStatus = value.FailureKind, value.HTTPStatus
		result.RetrySucceeded, result.RetryFinishReason = value.Succeeded, value.FinishReason
	case agent.SummarizationRetryScheduledEvent:
		result.RunID, result.Turn, result.RetryAttempt, result.RetryDelay = value.RunID, value.Turn, value.Attempt, value.Delay
		result.CompactionReason, result.RetryFailureKind, result.RetryHTTPStatus = value.Reason, value.FailureKind, value.HTTPStatus
	case agent.SummarizationRetryAttemptEvent:
		result.RunID, result.Turn, result.RetryAttempt, result.CompactionReason = value.RunID, value.Turn, value.Attempt, value.Reason
	case agent.SummarizationRetryFinishedEvent:
		result.RunID, result.Turn, result.RetryAttempt, result.CompactionReason = value.RunID, value.Turn, value.Attempt, value.Reason
		result.RetryFailureKind, result.RetryHTTPStatus = value.FailureKind, value.HTTPStatus
		result.RetrySucceeded, result.RetryFinishReason = value.Succeeded, value.FinishReason
	case agent.AutoRetryStartEvent:
		result.RetryAttempt, result.RetryDelay = value.Attempt, value.Delay
	case agent.AutoRetryEndEvent:
		result.RetryAttempt, result.RetrySucceeded = value.Attempt, value.Success
	case agent.SessionSummarizationRetryScheduledEvent:
		result.RetryAttempt, result.RetryDelay, result.CompactionReason = value.Attempt, value.Delay, value.Reason
		result.RetryFailureKind, result.RetryHTTPStatus = value.FailureKind, value.HTTPStatus
	case agent.SessionSummarizationRetryAttemptEvent:
		result.CompactionReason = value.Reason
	case agent.SessionSummarizationRetryFinishedEvent:
		result.RetryAttempt, result.CompactionReason = value.Attempt, value.Reason
		result.RetryFailureKind, result.RetryHTTPStatus = value.FailureKind, value.HTTPStatus
		result.RetrySucceeded, result.RetryFinishReason = value.Succeeded, value.FinishReason
	}
	return result
}

type observedAgentEvent interface{ Type() agent.AgentEventType }

func subscribeAllAgentEvents(runtime any, observer func(context.Context, observedAgentEvent)) func() {
	switch value := runtime.(type) {
	case *agent.Agent:
		unsubscribeEvents := value.Subscribe(func(ctx context.Context, event agent.AgentEvent) {
			observer(ctx, event)
		})
		unsubscribeControl := value.SubscribeControl(func(ctx context.Context, event agent.AgentControlEvent) {
			observer(ctx, event)
		})
		return func() {
			unsubscribeEvents()
			unsubscribeControl()
		}
	case *agent.AgentSession:
		return value.Subscribe(func(ctx context.Context, event agent.SessionEvent) {
			observer(ctx, event)
		})
	default:
		return func() {}
	}
}

func (t *fakeTool) Name() string { return t.name }

func (t *fakeTool) Execute(
	ctx context.Context,
	toolCallID string,
	arguments []byte,
	report func(agent.ToolUpdate),
) (agent.ToolOutput, error) {
	t.calls.Add(1)
	if t.executeWithID != nil {
		return t.executeWithID(ctx, toolCallID, append([]byte(nil), arguments...), report)
	}
	if t.execute == nil {
		return agent.ToolOutput{}, nil
	}
	return t.execute(ctx, append([]byte(nil), arguments...), report)
}

func (t *fakeTool) CallCount() uint32 { return t.calls.Load() }

func newSession(t *testing.T) *session.Session {
	t.Helper()
	directory := t.TempDir()
	var clockTick atomic.Int64
	var id atomic.Uint64
	transcript, err := session.Create(filepath.Join(directory, "session.jsonl"), session.CreateOptions{
		ID:         "session-agent-test",
		WorkingDir: directory,
		Now: func() time.Time {
			return agentTestEpoch.Add(time.Duration(clockTick.Add(1)) * time.Millisecond)
		},
		NewEntryID: func() (string, error) {
			return fmt.Sprintf("entry-%d", id.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("session.Create() error = %v", err)
	}
	t.Cleanup(func() {
		if err := transcript.Close(); err != nil {
			t.Errorf("Session.Close() error = %v", err)
		}
	})
	return transcript
}

func newSessionManager(t *testing.T) *session.SessionManager {
	t.Helper()
	directory := t.TempDir()
	var clockTick atomic.Int64
	var id atomic.Uint64
	manager, err := session.CreateSessionManagerWithOptions(directory, directory, session.ManagerOptions{
		NewSession: session.NewSessionOptions{ID: "session-agent-test"},
		Now: func() time.Time {
			return agentTestEpoch.Add(time.Duration(clockTick.Add(1)) * time.Millisecond)
		},
		NewEntryID: func() (string, error) {
			return fmt.Sprintf("entry-%d", id.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("session.CreateSessionManagerWithOptions() error = %v", err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("SessionManager.Close() error = %v", err)
		}
	})
	return manager
}

func newScriptedProvider(t *testing.T, terminals ...llm.AssistantTerminal) *provider.ScriptedProvider {
	t.Helper()
	scripted, err := provider.NewScriptedProvider(provider.ScriptedConfig{
		ChunkRunes: 2,
		Clock:      func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatalf("provider.NewScriptedProvider() error = %v", err)
	}
	steps := make([]provider.ScriptStep, len(terminals))
	for index, terminal := range terminals {
		steps[index], err = provider.FixedResponseStep(terminal)
		if err != nil {
			t.Fatalf("provider.FixedResponseStep(%d) error = %v", index, err)
		}
	}
	if err := scripted.SetResponses(steps); err != nil {
		t.Fatalf("ScriptedProvider.SetResponses() error = %v", err)
	}
	return scripted
}

func newAgent(
	t *testing.T,
	transcript agent.Transcript,
	providerImpl provider.Provider,
	tool agent.ToolExecutor,
) *agent.Agent {
	t.Helper()
	model, err := newTestModel("scripted", "scripted", "scripted-1")
	if err != nil {
		t.Fatal(err)
	}
	var definitions []provider.ToolDefinition
	if tool != nil {
		var names []string
		if named, ok := tool.(agent.NamedToolExecutor); ok {
			for _, candidate := range []string{tool.Name(), "bash", "echo", "slow", "fast", "terminate"} {
				if named.Supports(candidate) {
					names = append(names, candidate)
				}
			}
		} else {
			names = append(names, tool.Name())
		}
		for _, name := range names {
			definition, definitionErr := provider.NewToolDefinition(name, name, false, []byte(`{"type":"object"}`))
			if definitionErr != nil {
				t.Fatal(definitionErr)
			}
			definitions = append(definitions, definition)
		}
	}
	var tick atomic.Int64
	runtime, err := agent.New(agent.Config{
		Provider:     providerImpl,
		Model:        model,
		SystemPrompt: "You are deterministic.",
		Tool:         tool,
		Tools:        definitions,
		Now: func() time.Time {
			return agentTestEpoch.Add(time.Duration(tick.Add(1)) * time.Millisecond)
		},
	})
	if err != nil {
		t.Fatalf("agent.New() error = %v", err)
	}
	subscribeTestTranscript(t, runtime, transcript)
	return runtime
}

func subscribeTestTranscript(t *testing.T, runtime *agent.Agent, transcript agent.Transcript) {
	t.Helper()
	runtime.Subscribe(func(ctx context.Context, event agent.AgentEvent) error {
		ended, ok := event.(agent.MessageEndEvent)
		if !ok {
			return nil
		}
		if standard, ok := ended.Message.(agentmsg.LLM); ok {
			_, err := transcript.Append(context.WithoutCancel(ctx), standard.Conversation(), session.AppendOptions{})
			return err
		}
		durable, ok := transcript.(agent.AgentMessageTranscript)
		if !ok {
			return agent.ErrTranscriptCommit
		}
		_, err := durable.AppendAgentMessage(context.WithoutCancel(ctx), ended.Message, session.AppendOptions{})
		return err
	})
}

func mustUsage(t *testing.T, input, output uint64) llm.Usage {
	t.Helper()
	usage, err := llm.NewUsage(llm.UsageSpec{Input: input, Output: output})
	if err != nil {
		t.Fatal(err)
	}
	return usage
}

func mustTextBlock(t *testing.T, text string) llm.TextBlock {
	t.Helper()
	block, err := llm.NewTextBlock(text)
	if err != nil {
		t.Fatal(err)
	}
	return block
}

func mustTextTerminal(t *testing.T, text string) llm.AssistantTextMessage {
	t.Helper()
	terminal, err := newAssistantTextMessage(
		[]llm.TextBlock{mustTextBlock(t, text)},
		llm.FinishStop,
		mustUsage(t, 2, 1),
		agentTestEpoch,
	)
	if err != nil {
		t.Fatal(err)
	}
	return terminal
}

func mustTextTerminalWithProvenance(t *testing.T, text string, provenance llm.AssistantProvenance) llm.AssistantTextMessage {
	t.Helper()
	terminal, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{mustTextBlock(t, text)},
		llm.FinishStop,
		mustUsage(t, 2, 1),
		agentTestEpoch,
		provenance,
	)
	if err != nil {
		t.Fatal(err)
	}
	return terminal
}

func mustLengthTerminal(t *testing.T, text string) llm.AssistantTextMessage {
	t.Helper()
	terminal, err := newAssistantTextMessage(
		[]llm.TextBlock{mustTextBlock(t, text)},
		llm.FinishLength,
		mustUsage(t, 2, 2),
		agentTestEpoch,
	)
	if err != nil {
		t.Fatal(err)
	}
	return terminal
}

func mustToolUseTerminal(t *testing.T, id, name string, arguments []byte) llm.AssistantToolUseMessage {
	t.Helper()
	call, err := llm.NewToolCallBlock(id, name, arguments)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := newAssistantToolUseMessage(
		[]llm.AssistantBlock{mustTextBlock(t, "running"), call},
		mustUsage(t, 4, 2),
		agentTestEpoch,
	)
	if err != nil {
		t.Fatal(err)
	}
	return terminal
}

func messageRoles(messages []llm.ConversationMessage) []llm.Role {
	roles := make([]llm.Role, len(messages))
	for index, message := range messages {
		roles[index] = message.Role()
	}
	return roles
}

func toolResultAt(t *testing.T, messages []llm.ConversationMessage, index int) llm.ToolResultMessage {
	t.Helper()
	if index < 0 || index >= len(messages) {
		t.Fatalf("tool result index %d outside %d messages", index, len(messages))
	}
	result, ok := messages[index].(llm.ToolResultMessage)
	if !ok {
		t.Fatalf("message %d = %T, want llm.ToolResultMessage", index, messages[index])
	}
	return result
}

func failureAt(t *testing.T, messages []llm.ConversationMessage, index int) llm.AssistantFailureMessage {
	t.Helper()
	if index < 0 || index >= len(messages) {
		t.Fatalf("failure index %d outside %d messages", index, len(messages))
	}
	failure, ok := messages[index].(llm.AssistantFailureMessage)
	if !ok {
		t.Fatalf("message %d = %T, want llm.AssistantFailureMessage", index, messages[index])
	}
	return failure
}

func onlyText(t *testing.T, blocks []llm.TextBlock) string {
	t.Helper()
	if len(blocks) != 1 {
		t.Fatalf("text block count = %d, want 1", len(blocks))
	}
	return blocks[0].Text()
}

func messageText(t *testing.T, message llm.ConversationMessage) string {
	t.Helper()
	switch value := message.(type) {
	case llm.UserTextMessage:
		return onlyText(t, value.Content())
	case llm.UserContentMessage:
		for _, block := range value.Content() {
			if text, ok := block.(llm.TextBlock); ok {
				return text.Text()
			}
		}
	}
	t.Fatalf("message %T has no user text", message)
	return ""
}

func waitClosed(t *testing.T, channel <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func assertOpen(t *testing.T, channel <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-channel:
		t.Fatalf("%s completed before settlement barrier", label)
	default:
	}
}

func requireErrorIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want errors.Is(%v)", err, target)
	}
}

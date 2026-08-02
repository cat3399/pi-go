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
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

var agentTestEpoch = time.Date(2026, time.August, 1, 10, 11, 12, 0, time.UTC)

type fakeTool struct {
	name    string
	execute func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error)
	calls   atomic.Uint32
}

func (t *fakeTool) Name() string { return t.name }

func (t *fakeTool) Execute(
	ctx context.Context,
	arguments []byte,
	report func(agent.ToolUpdate),
) (agent.ToolOutput, error) {
	t.calls.Add(1)
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
	model, err := provider.NewModelRef("scripted", "scripted", "scripted-1")
	if err != nil {
		t.Fatal(err)
	}
	var tick atomic.Int64
	runtime, err := agent.New(agent.Config{
		Provider:          providerImpl,
		Transcript:        transcript,
		Model:             model,
		SystemPrompt:      "You are deterministic.",
		Tool:              tool,
		SettlementTimeout: time.Second,
		Now: func() time.Time {
			return agentTestEpoch.Add(time.Duration(tick.Add(1)) * time.Millisecond)
		},
	})
	if err != nil {
		t.Fatalf("agent.New() error = %v", err)
	}
	return runtime
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
	terminal, err := llm.NewAssistantTextMessage(
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

func mustLengthTerminal(t *testing.T, text string) llm.AssistantTextMessage {
	t.Helper()
	terminal, err := llm.NewAssistantTextMessage(
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
	terminal, err := llm.NewAssistantToolUseMessage(
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

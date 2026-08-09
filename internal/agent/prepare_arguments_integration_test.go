package agent_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/tool"
)

type recordingRegistryExecutor struct {
	*agent.RegistryExecutor
	executed []byte
}

func (e *recordingRegistryExecutor) ExecuteNamed(ctx context.Context, name string, arguments []byte, report func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	e.executed = append([]byte(nil), arguments...)
	return e.RegistryExecutor.ExecuteNamed(ctx, name, arguments, report)
}

func TestAgentSessionRegistryPreparesEditArgumentsBeforeSchemaAndHooks(t *testing.T) {
	for _, test := range []struct {
		name      string
		arguments string
	}{
		{
			name:      "stringified edits",
			arguments: `{"path":"target.txt","edits":"[{\"oldText\":\"one\",\"newText\":\"ONE\"}]"}`,
		},
		{
			name:      "legacy top-level edit",
			arguments: `{"path":"target.txt","oldText":"one","newText":"ONE"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(root+"/target.txt", []byte("one\ntwo\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			suite, err := tool.NewFilesystemSuite(tool.FilesystemOptions{WorkingDir: root})
			if err != nil {
				t.Fatal(err)
			}
			registry, err := tool.NewFilesystemRegistry(suite)
			if err != nil {
				t.Fatal(err)
			}
			baseExecutor, err := agent.NewRegistryExecutor(registry)
			if err != nil {
				t.Fatal(err)
			}
			executor := &recordingRegistryExecutor{RegistryExecutor: baseExecutor}
			var editDefinition provider.ToolDefinition
			for _, specification := range registry.Specifications() {
				if specification.Name() != tool.EditToolName {
					continue
				}
				editDefinition, err = provider.NewToolDefinition(
					specification.Name(), specification.Description(), specification.Strict(), specification.ParametersJSON(),
				)
				if err != nil {
					t.Fatal(err)
				}
			}
			if editDefinition.Name() == "" {
				t.Fatal("edit definition not found")
			}
			implementation := newScriptedProvider(t,
				mustToolUseTerminal(t, "edit-call", tool.EditToolName, []byte(test.arguments)),
				mustTextTerminal(t, "done"),
			)
			var beforeArguments []byte
			coordinator, err := agent.NewSession(agent.SessionConfig{
				Provider: implementation, SessionManager: newSessionManager(t), Model: sessionTestModel(t),
				Tool: executor, Tools: []provider.ToolDefinition{editDefinition},
				Hooks: agent.Hooks{ToolCall: func(_ context.Context, event agent.BeforeToolCallContext) (agent.BeforeToolCallResult, error) {
					beforeArguments = append([]byte(nil), event.Arguments...)
					return agent.BeforeToolCallResult{}, nil
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := coordinator.Run(context.Background(), "edit it")
			if err != nil || !result.Succeeded() || result.ToolExecutions() != 1 {
				t.Fatalf("Run = (%#v, %v)", result, err)
			}
			assertPreparedEditArguments(t, beforeArguments)
			assertPreparedEditArguments(t, executor.executed)
			content, err := os.ReadFile(root + "/target.txt")
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != "ONE\ntwo\n" {
				t.Fatalf("edited content = %q", content)
			}
			messages := coordinator.State().Active.Messages()
			if len(messages) < 3 {
				t.Fatalf("agent messages = %#v", messages)
			}
			resultMessage, ok := messages[2].(interface {
				Conversation() llm.ConversationMessage
			})
			if !ok {
				t.Fatalf("tool result message = %T", messages[2])
			}
			if toolResult, ok := resultMessage.Conversation().(llm.ToolResultMessage); !ok || toolResult.IsError() {
				t.Fatalf("tool result = %#v", resultMessage.Conversation())
			}
		})
	}
}

func assertPreparedEditArguments(t *testing.T, raw []byte) {
	t.Helper()
	var arguments map[string]any
	if err := json.Unmarshal(raw, &arguments); err != nil {
		t.Fatalf("prepared arguments %q: %v", raw, err)
	}
	if _, exists := arguments["oldText"]; exists {
		t.Fatalf("legacy oldText reached hook/executor: %#v", arguments)
	}
	if _, exists := arguments["newText"]; exists {
		t.Fatalf("legacy newText reached hook/executor: %#v", arguments)
	}
	edits, ok := arguments["edits"].([]any)
	if !ok || len(edits) != 1 {
		t.Fatalf("prepared edits = %#v", arguments["edits"])
	}
	edit, ok := edits[0].(map[string]any)
	if !ok || edit["oldText"] != "one" || edit["newText"] != "ONE" {
		t.Fatalf("prepared edit = %#v", edits[0])
	}
}

package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

func TestBeforeAgentStartAndMessageHooksMutateOneDurableRun(t *testing.T) {
	model, err := newTestModel("scripted", "scripted", "model")
	if err != nil {
		t.Fatal(err)
	}
	injected, err := agentmsg.NewCustomText("fixture", "injected", true, json.RawMessage(`{"source":"hook"}`), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	sequence := make([]string, 0, 6)
	hooks := agent.Hooks{
		BeforeAgentStart: func(_ context.Context, event agent.BeforeAgentStartEvent) (agent.BeforeAgentStartResult, error) {
			if event.Prompt != "hello" || event.SystemPrompt != "base" || len(event.Messages) != 0 || len(event.PromptMessages) != 1 || len(event.Images) != 0 {
				t.Fatalf("before_agent_start = %#v", event)
			}
			override := "hook system"
			return agent.BeforeAgentStartResult{ExtraMessages: []agentmsg.Message{injected}, SystemPrompt: &override}, nil
		},
		Message: func(_ context.Context, event agent.MessageHookEvent) (agent.MessageHookResult, error) {
			sequence = append(sequence, string(event.Type)+":"+string(event.Message.Role()))
			if event.Type == agent.MessageEndHookEvent && event.Message.Role() == agentmsg.RoleCustom {
				replacement, replaceErr := agentmsg.NewCustomText("fixture", "replaced", true, json.RawMessage(`{"source":"message_end"}`), event.Message.Timestamp())
				return agent.MessageHookResult{Message: replacement}, replaceErr
			}
			if event.Type == agent.MessageEndHookEvent && event.Message.Role() == agentmsg.RoleAssistant {
				wrapped := event.Message.(agentmsg.LLM).Conversation().(llm.AssistantTextMessage)
				replacement, replaceErr := llm.NewAssistantTextMessage(
					[]llm.TextBlock{mustTextBlock(t, "rewritten")},
					wrapped.FinishReason(),
					wrapped.Usage(),
					wrapped.Timestamp(),
					wrapped.AssistantProvenance(),
				)
				if replaceErr != nil {
					return agent.MessageHookResult{}, replaceErr
				}
				value, replaceErr := agentmsg.NewLLM(replacement)
				return agent.MessageHookResult{Message: value}, replaceErr
			}
			return agent.MessageHookResult{}, nil
		},
	}
	transcript := newSessionManager(t)
	providerImpl := newScriptedProvider(t, mustTextTerminal(t, "original"))
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, SessionManager: transcript, Model: model, SystemPrompt: "base", Hooks: hooks,
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "hello")
	if err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	requests := providerImpl.Requests()
	if len(requests) != 1 || requests[0].SystemPrompt() != "hook system" {
		t.Fatalf("requests = %#v", requests)
	}
	requestMessages := requests[0].Messages()
	if len(requestMessages) != 2 || userText(t, requestMessages[0]) != "hello" || userText(t, requestMessages[1]) != "replaced" {
		t.Fatalf("provider message order = %#v", requestMessages)
	}
	wantSequence := []string{
		"message_start:user", "message_end:user",
		"message_start:custom", "message_end:custom",
		"message_start:assistant",
		"message_update:assistant", "message_update:assistant", "message_update:assistant",
		"message_update:assistant", "message_update:assistant", "message_update:assistant",
		"message_end:assistant",
	}
	if !reflect.DeepEqual(sequence, wantSequence) {
		t.Fatalf("hook sequence = %v, want %v", sequence, wantSequence)
	}
	durable := transcript.BuildContext().AgentMessages()
	if len(durable) != 3 || durable[0].Role() != agentmsg.RoleUser || durable[1].Role() != agentmsg.RoleCustom || durable[2].Role() != agentmsg.RoleAssistant {
		t.Fatalf("durable roles = %#v", durable)
	}
	custom := durable[1].(agentmsg.Custom)
	if text, ok := custom.StringContent(); !ok || text != "replaced" || string(custom.Details()) != `{"source":"message_end"}` {
		t.Fatalf("durable custom = %#v", custom)
	}
	if got := durable[2].(agentmsg.LLM).Conversation().(llm.AssistantTextMessage).Content()[0].Text(); got != "rewritten" {
		t.Fatalf("durable assistant = %q", got)
	}
	terminal, ok := result.Terminal()
	if !ok {
		t.Fatal("Run result has no terminal")
	}
	text, ok := terminal.(llm.AssistantTextMessage)
	if !ok || len(text.Content()) != 1 || text.Content()[0].Text() != "rewritten" {
		t.Fatalf("Run terminal = %#v", terminal)
	}
}

func TestMessageEndReplacementControlsToolExecution(t *testing.T) {
	model, _ := newTestModel("scripted", "scripted", "model")
	definition, err := provider.NewToolDefinition("blocked-by-message-end", "fixture", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	providerImpl := newScriptedProvider(t, mustToolUseTerminal(t, "call", "blocked-by-message-end", []byte(`{}`)))
	tool := &fakeTool{name: "blocked-by-message-end"}
	transcript := newSessionManager(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, SessionManager: transcript, Model: model, Tool: tool, Tools: []provider.ToolDefinition{definition},
		Hooks: agent.Hooks{Message: func(_ context.Context, event agent.MessageHookEvent) (agent.MessageHookResult, error) {
			if event.Type != agent.MessageEndHookEvent || event.Message.Role() != agentmsg.RoleAssistant {
				return agent.MessageHookResult{}, nil
			}
			conversation := event.Message.(agentmsg.LLM).Conversation()
			toolUse, ok := conversation.(llm.AssistantToolUseMessage)
			if !ok {
				return agent.MessageHookResult{}, nil
			}
			replacement, replaceErr := llm.NewAssistantTextMessage(
				[]llm.TextBlock{mustTextBlock(t, "tool skipped")}, llm.FinishStop, toolUse.Usage(), toolUse.Timestamp(), toolUse.AssistantProvenance(),
			)
			if replaceErr != nil {
				return agent.MessageHookResult{}, replaceErr
			}
			wrapped, replaceErr := agentmsg.NewLLM(replacement)
			return agent.MessageHookResult{Message: wrapped}, replaceErr
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "go")
	if err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	if tool.CallCount() != 0 || providerImpl.CallCount() != 1 || result.ToolExecutions() != 0 {
		t.Fatalf("replacement still executed: tool=%d provider=%d result=%d", tool.CallCount(), providerImpl.CallCount(), result.ToolExecutions())
	}
	terminal, ok := result.Terminal()
	text, textOK := terminal.(llm.AssistantTextMessage)
	if !ok || !textOK || text.Content()[0].Text() != "tool skipped" {
		t.Fatalf("Run terminal = %#v", terminal)
	}
	messages := transcript.BuildContext().Messages()
	if len(messages) != 2 {
		t.Fatalf("durable messages = %#v", messages)
	}
	if _, ok := messages[1].(llm.AssistantTextMessage); !ok {
		t.Fatalf("durable assistant = %#v", messages[1])
	}
}

func TestMessageEndSameRoleToolResultIdentityReplacementPropagates(t *testing.T) {
	model, _ := newTestModel("scripted", "scripted", "model")
	definition, _ := provider.NewToolDefinition("identity", "fixture", false, []byte(`{"type":"object"}`))
	transcript := newSessionManager(t)
	tool := &fakeTool{name: "identity", execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		return agent.ToolOutput{Text: "executed"}, nil
	}}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider:       newScriptedProvider(t, mustToolUseTerminal(t, "call", "identity", []byte(`{}`))),
		SessionManager: transcript, Model: model, Tool: tool, Tools: []provider.ToolDefinition{definition},
		Hooks: agent.Hooks{Message: func(_ context.Context, event agent.MessageHookEvent) (agent.MessageHookResult, error) {
			if event.Type != agent.MessageEndHookEvent || event.Message.Role() != agentmsg.RoleToolResult {
				return agent.MessageHookResult{}, nil
			}
			replacement, replaceErr := llm.NewToolResultMessage("different-call", "identity", []llm.TextBlock{mustTextBlock(t, "bad")}, false, event.Message.Timestamp())
			if replaceErr != nil {
				return agent.MessageHookResult{}, replaceErr
			}
			wrapped, replaceErr := agentmsg.NewLLM(replacement)
			return agent.MessageHookResult{Message: wrapped}, replaceErr
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "go")
	terminal, ok := result.Terminal()
	failure, failed := terminal.(llm.AssistantFailureMessage)
	if err != nil || !ok || !failed || !errors.Is(failure.Failure().Cause(), provider.ErrInvalidRequest) {
		t.Fatalf("Run terminal=%T cause=%v error=%v, want downstream invalid request", terminal, failure.Failure().Cause(), err)
	}
	if tool.CallCount() != 1 {
		t.Fatalf("tool calls = %d", tool.CallCount())
	}
	messages := transcript.BuildContext().Messages()
	toolResult, propagated := messages[2].(llm.ToolResultMessage)
	if len(messages) != 4 || !propagated || toolResult.ToolCallID() != "different-call" || messages[3].Role() != llm.RoleAssistant {
		t.Fatalf("same-role replacement did not propagate: %#v", messages)
	}
}

func TestSessionMessageEndIgnoresRoleMismatch(t *testing.T) {
	model := sessionTestModel(t)
	transcript := newSessionManager(t)
	providerImpl := newScriptedProvider(t, mustTextTerminal(t, "done"))
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, SessionManager: transcript, Model: model,
		Hooks: agent.Hooks{Message: func(_ context.Context, event agent.MessageHookEvent) (agent.MessageHookResult, error) {
			if event.Type != agent.MessageEndHookEvent || event.Message.Role() != agentmsg.RoleUser {
				return agent.MessageHookResult{}, nil
			}
			replacement, replaceErr := agentmsg.NewLLM(mustTextTerminal(t, "wrong role"))
			return agent.MessageHookResult{Message: replacement}, replaceErr
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "original")
	if err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	requestMessage := providerImpl.Requests()[0].Messages()[0]
	durableMessage := transcript.BuildContext().Messages()[0]
	if requestMessage.Role() != llm.RoleUser || durableMessage.Role() != llm.RoleUser || messageText(t, durableMessage) != "original" {
		t.Fatalf("role mismatch propagated request=%#v durable=%#v", requestMessage, durableMessage)
	}
}

func TestSessionSyntheticFailureUsesMessageEndReplacementAndErrorBoundary(t *testing.T) {
	transformErr := errors.New("transform failed")
	t.Run("replacement", func(t *testing.T) {
		transcript := newSessionManager(t)
		var sequence []string
		runtime, err := agent.NewSession(agent.SessionConfig{
			Provider: newScriptedProvider(t), SessionManager: transcript, Model: sessionTestModel(t),
			TransformContext: func(context.Context, []llm.ConversationMessage) ([]llm.ConversationMessage, error) {
				return nil, transformErr
			},
			Hooks: agent.Hooks{Message: func(_ context.Context, event agent.MessageHookEvent) (agent.MessageHookResult, error) {
				sequence = append(sequence, string(event.Type)+":"+string(event.Message.Role()))
				if event.Type != agent.MessageEndHookEvent || event.Message.Role() != agentmsg.RoleAssistant {
					return agent.MessageHookResult{}, nil
				}
				failure, ok := event.Message.(agentmsg.LLM).Conversation().(llm.AssistantFailureMessage)
				if !ok || !errors.Is(failure.Failure().Cause(), agent.ErrContextTransform) {
					t.Fatalf("synthetic hook message = %#v", event.Message)
				}
				replacement, replaceErr := llm.NewAssistantTextMessage(
					[]llm.TextBlock{mustTextBlock(t, "recovered")}, llm.FinishStop, llm.Usage{}, failure.Timestamp(), failure.AssistantProvenance(),
				)
				if replaceErr != nil {
					return agent.MessageHookResult{}, replaceErr
				}
				wrapped, replaceErr := agentmsg.NewLLM(replacement)
				return agent.MessageHookResult{Message: wrapped}, replaceErr
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := runtime.Run(context.Background(), "go")
		terminal, ok := result.Terminal()
		text, textOK := terminal.(llm.AssistantTextMessage)
		if err != nil || !ok || !textOK || text.Content()[0].Text() != "recovered" {
			t.Fatalf("Run terminal = (%#v, %v)", terminal, err)
		}
		if got := transcript.BuildContext().Messages(); len(got) != 2 || got[1].(llm.AssistantTextMessage).Content()[0].Text() != "recovered" {
			t.Fatalf("durable replacement = %#v", got)
		}
		want := []string{"message_start:user", "message_end:user", "message_start:assistant", "message_end:assistant"}
		if !reflect.DeepEqual(sequence, want) {
			t.Fatalf("synthetic hook sequence = %v, want %v", sequence, want)
		}
	})

	t.Run("error", func(t *testing.T) {
		hookErr := errors.New("synthetic message hook failed")
		runtime, err := agent.NewSession(agent.SessionConfig{
			Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: sessionTestModel(t),
			TransformContext: func(context.Context, []llm.ConversationMessage) ([]llm.ConversationMessage, error) {
				return nil, transformErr
			},
			Hooks: agent.Hooks{Message: func(_ context.Context, event agent.MessageHookEvent) (agent.MessageHookResult, error) {
				if event.Type == agent.MessageEndHookEvent && event.Message.Role() == agentmsg.RoleAssistant {
					return agent.MessageHookResult{}, hookErr
				}
				return agent.MessageHookResult{}, nil
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Run(context.Background(), "go"); !errors.Is(err, hookErr) {
			t.Fatalf("Run error = %v", err)
		}
		if runtime.State().Active.Phase() != agent.PhaseIdle {
			t.Fatalf("hook error left phase %s", runtime.State().Active.Phase())
		}
	})
}

func TestBeforeAgentStartCancellationNeedsNoReasonAndDoesNotPersist(t *testing.T) {
	model, _ := newTestModel("scripted", "scripted", "model")
	providerImpl := newScriptedProvider(t, mustTextTerminal(t, "must not run"))
	cancel := true
	transcript := newSessionManager(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, SessionManager: transcript, Model: model,
		Hooks: agent.Hooks{BeforeAgentStart: func(context.Context, agent.BeforeAgentStartEvent) (agent.BeforeAgentStartResult, error) {
			return agent.BeforeAgentStartResult{Cancel: agent.HookCancel{Cancel: &cancel}}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "blocked"); !errors.Is(err, agent.ErrAgentAborted) {
		t.Fatalf("Run error = %v", err)
	}
	if providerImpl.CallCount() != 0 || len(transcript.BuildContext().AgentMessages()) != 0 {
		t.Fatalf("cancelled run called provider or persisted: calls=%d messages=%#v", providerImpl.CallCount(), transcript.BuildContext().AgentMessages())
	}
}

func TestContextHookPresencePreservesFullAgentMessages(t *testing.T) {
	model, _ := newTestModel("scripted", "scripted", "model")
	transcript := newSessionManager(t)
	opaque, err := agentmsg.NewOpaque(agentmsg.OpaqueSpec{Type: "futureRole", Data: json.RawMessage(`{"role":"futureRole","timestamp":1,"opaque":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.AppendMessage(context.Background(), opaque); err != nil {
		t.Fatal(err)
	}
	providerImpl := newScriptedProvider(t, mustTextTerminal(t, "done"))
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, SessionManager: transcript, Model: model,
		Hooks: agent.Hooks{Context: func(_ context.Context, event agent.ContextHookEvent) (agent.ContextHookResult, error) {
			if len(event.Messages) != 2 {
				t.Fatalf("context messages = %#v", event.Messages)
			}
			if _, ok := event.Messages[0].(agentmsg.OpaqueMessage); !ok {
				t.Fatalf("opaque message lost before hook: %#v", event.Messages[0])
			}
			event.Messages[0] = nil
			return agent.ContextHookResult{}, nil // absence means unchanged
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "visible"); err != nil {
		t.Fatal(err)
	}
	requestMessages := providerImpl.Requests()[0].Messages()
	if len(requestMessages) != 1 || userText(t, requestMessages[0]) != "visible" {
		t.Fatalf("nil context result changed projection = %#v", requestMessages)
	}

	emptyProvider := newScriptedProvider(t, mustTextTerminal(t, "done"))
	emptyTranscript := newSessionManager(t)
	emptyRuntime, err := agent.NewSession(agent.SessionConfig{
		Provider: emptyProvider, SessionManager: emptyTranscript, Model: model,
		Hooks: agent.Hooks{Context: func(context.Context, agent.ContextHookEvent) (agent.ContextHookResult, error) {
			empty := []agentmsg.Message{}
			return agent.ContextHookResult{Messages: &empty}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := emptyRuntime.Run(context.Background(), "cleared"); err != nil {
		t.Fatal(err)
	}
	if got := emptyProvider.Requests()[0].Messages(); len(got) != 0 {
		t.Fatalf("present empty replacement did not clear provider context: %#v", got)
	}
}

func TestToolHooksChainMutationsBeforeExecutionAndPersistence(t *testing.T) {
	model, _ := newTestModel("scripted", "scripted", "model")
	definition, err := provider.NewToolDefinition("mutate", "fixture", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	providerImpl := newScriptedProvider(t,
		mustToolUseTerminal(t, "call-1", "mutate", []byte(`{"step":1}`)),
		mustTextTerminal(t, "done"),
	)
	tool := &fakeTool{name: "mutate", execute: func(_ context.Context, arguments []byte, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		if string(arguments) != `{"step":3}` {
			t.Fatalf("tool arguments = %s", arguments)
		}
		return agent.ToolOutput{Text: "tool"}, nil
	}}
	step2 := json.RawMessage(`{"step":2}`)
	step3 := json.RawMessage(`{"step":3}`)
	firstText := []llm.ToolResultContentBlock{mustTextBlock(t, "first")}
	secondText := []llm.ToolResultContentBlock{mustTextBlock(t, "second")}
	markedError := true
	clearedError := false
	transcript := newSessionManager(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, SessionManager: transcript, Model: model, Tool: tool, Tools: []provider.ToolDefinition{definition},
		BeforeToolCall: func(_ context.Context, event agent.BeforeToolCallContext) (agent.BeforeToolCallResult, error) {
			if string(event.Arguments) != `{"step":1}` {
				t.Fatalf("base before args = %s", event.Arguments)
			}
			return agent.BeforeToolCallResult{Arguments: &step2}, nil
		},
		AfterToolCall: func(_ context.Context, event agent.AfterToolCallContext) (agent.AfterToolCallResult, error) {
			if event.Result.Text != "tool" || event.IsError {
				t.Fatalf("base after event = %#v", event)
			}
			return agent.AfterToolCallResult{Content: &firstText, IsError: &markedError}, nil
		},
		Hooks: agent.Hooks{
			ToolCall: func(_ context.Context, event agent.BeforeToolCallContext) (agent.BeforeToolCallResult, error) {
				if string(event.Arguments) != `{"step":2}` || string(event.ToolCall.ArgumentsJSON()) != `{"step":2}` {
					t.Fatalf("chained before event = %#v", event)
				}
				return agent.BeforeToolCallResult{Arguments: &step3}, nil
			},
			ToolResult: func(_ context.Context, event agent.AfterToolCallContext) (agent.AfterToolCallResult, error) {
				if !event.IsError || len(event.Result.Content) != 1 || event.Result.Content[0].(llm.TextBlock).Text() != "first" {
					t.Fatalf("chained after event = %#v", event)
				}
				return agent.AfterToolCallResult{Content: &secondText, IsError: &clearedError}, nil
			},
		},
		SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := runtime.Run(context.Background(), "go"); err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	if tool.CallCount() != 1 {
		t.Fatalf("tool calls = %d", tool.CallCount())
	}
	messages := transcript.BuildContext().Messages()
	resultMessage, ok := messages[2].(llm.ToolResultMessage)
	if !ok || resultMessage.IsError() || len(resultMessage.Content()) != 1 || resultMessage.Content()[0].Text() != "second" {
		t.Fatalf("durable tool result = %#v", messages[2])
	}
	requests := providerImpl.Requests()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d", len(requests))
	}
	toolUse := requests[1].Messages()[1].(llm.AssistantToolUseMessage)
	sourceCall := toolUse.Blocks()[1].(llm.ToolCallBlock)
	if string(sourceCall.ArgumentsJSON()) != `{"step":1}` {
		// The assistant source message remains immutable; only execution and the
		// associated result use the hook-mutated arguments.
		t.Fatalf("assistant source arguments changed = %s", sourceCall.ArgumentsJSON())
	}
}

func TestToolCallHookCanBlockBeforeExecution(t *testing.T) {
	model, _ := newTestModel("scripted", "scripted", "model")
	definition, _ := provider.NewToolDefinition("blocked", "fixture", false, []byte(`{"type":"object"}`))
	providerImpl := newScriptedProvider(t, mustToolUseTerminal(t, "call", "blocked", []byte(`{}`)), mustTextTerminal(t, "done"))
	tool := &fakeTool{name: "blocked"}
	transcript := newSessionManager(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, SessionManager: transcript, Model: model, Tool: tool, Tools: []provider.ToolDefinition{definition},
		Hooks: agent.Hooks{ToolCall: func(context.Context, agent.BeforeToolCallContext) (agent.BeforeToolCallResult, error) {
			return agent.BeforeToolCallResult{Block: true, Reason: "denied"}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if tool.CallCount() != 0 {
		t.Fatalf("blocked tool executed %d times", tool.CallCount())
	}
	result := transcript.BuildContext().Messages()[2].(llm.ToolResultMessage)
	if !result.IsError() || result.Content()[0].Text() != "denied" {
		t.Fatalf("blocked result = %#v", result)
	}
}

func TestSessionLifecycleAndTreeHooksRunOnRealOperations(t *testing.T) {
	model, _ := newTestModel("scripted", "scripted", "model")
	transcript := newSessionManager(t)
	firstMessage, _ := llm.NewUserTextMessage("first", time.UnixMilli(1))
	first, err := transcript.AppendLLMMessage(context.Background(), firstMessage)
	if err != nil {
		t.Fatal(err)
	}
	secondMessage, _ := llm.NewUserTextMessage("second", time.UnixMilli(2))
	second, err := transcript.AppendLLMMessage(context.Background(), secondMessage)
	if err != nil {
		t.Fatal(err)
	}
	if err := transcript.Branch(first.ID()); err != nil {
		t.Fatal(err)
	}
	alternateMessage, _ := llm.NewUserTextMessage("alternate", time.UnixMilli(3))
	alternate, err := transcript.AppendLLMMessage(context.Background(), alternateMessage)
	if err != nil {
		t.Fatal(err)
	}
	var lifecycle []string
	var treeBefore []agent.SessionBeforeTreeEvent
	var treeAfter []agent.SessionTreeEvent
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: transcript, Model: model,
		Hooks: agent.Hooks{
			SessionStart: func(_ context.Context, event agent.SessionStartHookEvent) error {
				lifecycle = append(lifecycle, "start:"+string(event.Reason))
				return nil
			},
			SessionShutdown: func(_ context.Context, event agent.SessionShutdownHookEvent) error {
				lifecycle = append(lifecycle, "shutdown:"+string(event.Reason))
				return nil
			},
			SessionBeforeTree: func(_ context.Context, event agent.SessionBeforeTreeEvent) (agent.SessionBeforeTreeResult, error) {
				treeBefore = append(treeBefore, event)
				label := "selected"
				return agent.SessionBeforeTreeResult{Label: &label}, nil
			},
			SessionTree: func(_ context.Context, event agent.SessionTreeEvent) error {
				treeAfter = append(treeAfter, event)
				return nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.SelectLeaf(context.Background(), second.ID()); err != nil {
		t.Fatal(err)
	}
	if len(treeBefore) != 1 || len(treeAfter) != 1 {
		t.Fatalf("tree hooks = %#v / %#v", treeBefore, treeAfter)
	}
	preparation := treeBefore[0].Preparation
	if preparation.TargetID != second.ID() || preparation.OldLeafID == nil || *preparation.OldLeafID != alternate.ID() || preparation.CommonAncestorID == nil || *preparation.CommonAncestorID != first.ID() || len(preparation.EntriesToSummarize) != 1 || preparation.EntriesToSummarize[0].ID() != alternate.ID() || preparation.UserWantsSummary || treeAfter[0].NewLeafID == nil || *treeAfter[0].NewLeafID == second.ID() || treeAfter[0].FromExtension != nil {
		t.Fatalf("tree hooks = %#v / %#v", treeBefore, treeAfter)
	}
	leaf, ok := transcript.LeafEntry()
	labelPayload, labelOK := leaf.Payload().(session.LabelPayload)
	if !ok || !labelOK || labelPayload.TargetID != second.ID() || labelPayload.Label == nil || *labelPayload.Label != "selected" || treeAfter[0].NewLeafID == nil || *treeAfter[0].NewLeafID != leaf.ID() {
		t.Fatalf("tree label leaf = %#v / %#v", leaf, treeAfter[0])
	}
	if got := transcript.BuildContext().Messages(); len(got) != 2 || userText(t, got[1]) != "second" {
		t.Fatalf("selected context = %#v", got)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"start:startup", "shutdown:quit"}; !reflect.DeepEqual(lifecycle, want) {
		t.Fatalf("lifecycle = %v, want %v", lifecycle, want)
	}
}

func TestManualAndAutomaticCompactionHooksRunAtCommitBoundaries(t *testing.T) {
	model, _ := newAgentModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "model",
		ContextWindow: 100, MaxTokens: 99,
	})
	t.Run("manual", func(t *testing.T) {
		transcript := newSessionManager(t)
		for index, text := range []string{"old one", "old two", "recent"} {
			message, _ := llm.NewUserTextMessage(text, time.UnixMilli(int64(index+1)))
			if _, err := transcript.AppendLLMMessage(context.Background(), message); err != nil {
				t.Fatal(err)
			}
		}
		var phases []string
		var summaryInput session.SummaryInput
		summarizer := contextRetrySummarizerFunc(func(_ context.Context, input session.SummaryInput) (session.SummaryOutput, error) {
			summaryInput = input
			return session.SummaryOutput{Text: "summary"}, nil
		})
		runtime, err := agent.NewSession(agent.SessionConfig{
			Provider: newScriptedProvider(t), SessionManager: transcript, Model: model, Summarizer: summarizer, KeepRecentTokens: 1,
			Hooks: agent.Hooks{SessionBeforeCompact: func(_ context.Context, event agent.SessionBeforeCompactEvent) (agent.SessionBeforeCompactResult, error) {
				if event.Reason != agent.CompactionManual {
					t.Fatalf("manual reason = %q", event.Reason)
				}
				phases = append(phases, "before")
				if event.CustomInstructions == nil || *event.CustomInstructions != "caller instructions" || event.Preparation.FirstKeptEntryID == "" {
					t.Fatalf("manual preparation = %#v", event)
				}
				return agent.SessionBeforeCompactResult{}, nil
			}, SessionCompact: func(_ context.Context, event agent.SessionCompactEvent) error {
				phases = append(phases, "after")
				return nil
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result, err := runtime.Compact(context.Background(), "caller instructions"); err != nil || !result.Committed {
			t.Fatalf("Compact = (%#v, %v)", result, err)
		}
		if !reflect.DeepEqual(phases, []string{"before", "after"}) || summaryInput.Instructions != "caller instructions" {
			t.Fatalf("manual hook phases/input = %v/%#v", phases, summaryInput)
		}
	})

	t.Run("automatic threshold", func(t *testing.T) {
		transcript := newSessionManager(t)
		old, _ := llm.NewUserTextMessage("old context that exceeds threshold", time.UnixMilli(1))
		if _, err := transcript.AppendLLMMessage(context.Background(), old); err != nil {
			t.Fatal(err)
		}
		var phases []string
		summarizer := contextRetrySummarizerFunc(func(_ context.Context, _ session.SummaryInput) (session.SummaryOutput, error) {
			return session.SummaryOutput{Text: "automatic summary"}, nil
		})
		runtime, err := agent.NewSession(agent.SessionConfig{
			Provider: newScriptedProvider(t, mustTextTerminal(t, "done")), SessionManager: transcript, Model: model,
			ContextWindow: 100, ContextReserve: 99, KeepRecentTokens: 1, Summarizer: summarizer,
			Hooks: agent.Hooks{SessionBeforeCompact: func(_ context.Context, event agent.SessionBeforeCompactEvent) (agent.SessionBeforeCompactResult, error) {
				if event.Reason == agent.CompactionThreshold {
					phases = append(phases, "before")
				}
				return agent.SessionBeforeCompactResult{}, nil
			}, SessionCompact: func(_ context.Context, event agent.SessionCompactEvent) error {
				if event.Reason == agent.CompactionThreshold {
					phases = append(phases, "after")
				}
				return nil
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Run(context.Background(), "new"); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(phases, []string{"before", "after"}) {
			t.Fatalf("automatic phases = %v", phases)
		}
	})
}

func TestCompactionExtensionOverrideAndSettlementOrdering(t *testing.T) {
	model, _ := newTestModel("scripted", "scripted", "model")
	newTranscript := func(t *testing.T) *session.SessionManager {
		transcript := newSessionManager(t)
		for index, text := range []string{"old one", "old two", "recent"} {
			message, _ := llm.NewUserTextMessage(text, time.UnixMilli(int64(index+1)))
			if _, err := transcript.AppendLLMMessage(context.Background(), message); err != nil {
				t.Fatal(err)
			}
		}
		return transcript
	}
	t.Run("full override", func(t *testing.T) {
		transcript := newTranscript(t)
		var sequence []string
		var summarizerCalls int
		var after agent.SessionCompactEvent
		runtime, err := agent.NewSession(agent.SessionConfig{
			Provider: newScriptedProvider(t), SessionManager: transcript, Model: model, KeepRecentTokens: 1,
			Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
				summarizerCalls++
				return session.SummaryOutput{Text: "default must not run"}, nil
			}),
			Hooks: agent.Hooks{
				SessionBeforeCompact: func(_ context.Context, event agent.SessionBeforeCompactEvent) (agent.SessionBeforeCompactResult, error) {
					sequence = append(sequence, "hook-before")
					return agent.SessionBeforeCompactResult{Compaction: &agent.ExtensionCompactionResult{
						Summary: "extension summary", FirstKeptEntryID: event.Preparation.FirstKeptEntryID,
						TokensBefore: event.Preparation.TokensBefore, Details: json.RawMessage(`{"owner":"extension"}`),
					}}, nil
				},
				SessionCompact: func(_ context.Context, event agent.SessionCompactEvent) error {
					sequence = append(sequence, "hook-after")
					after = event
					return nil
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
			if event.Type() == agent.CompactionStartEventType {
				sequence = append(sequence, "observer-start")
			}
			if event.Type() == agent.CompactionEndEventType {
				sequence = append(sequence, "observer-end")
			}
		})
		result, err := runtime.Compact(context.Background(), "focus")
		if err != nil || !result.Committed {
			t.Fatalf("Compact = (%#v, %v)", result, err)
		}
		if !reflect.DeepEqual(sequence, []string{"observer-start", "hook-before", "hook-after", "observer-end"}) {
			t.Fatalf("compaction ordering = %v", sequence)
		}
		if summarizerCalls != 0 || !result.Output.FromExtension || after.CompactionEntry.ID() != result.Entry.ID() || !after.FromExtension || after.Result.EstimatedTokensAfter == nil {
			t.Fatalf("extension result = calls=%d result=%#v after=%#v", summarizerCalls, result, after)
		}
		payload, ok := result.Entry.Payload().(session.CompactionPayload)
		if !ok || !payload.HasFromHook || !payload.FromHook || string(payload.Details) != `{"owner":"extension"}` || payload.Record.Summary != "extension summary" {
			t.Fatalf("durable compaction payload = %#v", result.Entry.Payload())
		}
	})

	for _, test := range []struct {
		name        string
		result      agent.SessionBeforeCompactResult
		hookErr     error
		wantAborted bool
	}{
		{name: "cancel", result: agent.SessionBeforeCompactResult{Cancel: agent.HookCancel{Cancel: boolPtr(true)}}, wantAborted: true},
		{name: "error", hookErr: errors.New("extension failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			transcript := newTranscript(t)
			beforeEntries := len(transcript.Entries())
			var sequence []string
			var settlement agent.CompactionEndEvent
			var settled bool
			runtime, err := agent.NewSession(agent.SessionConfig{
				Provider: newScriptedProvider(t), SessionManager: transcript, Model: model, KeepRecentTokens: 1,
				Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
					t.Fatal("default summarizer ran after extension settlement")
					return session.SummaryOutput{}, nil
				}),
				Hooks: agent.Hooks{SessionBeforeCompact: func(context.Context, agent.SessionBeforeCompactEvent) (agent.SessionBeforeCompactResult, error) {
					sequence = append(sequence, "hook-before")
					return test.result, test.hookErr
				}, SessionCompact: func(context.Context, agent.SessionCompactEvent) error {
					t.Fatal("after hook ran without a committed compaction")
					return nil
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
				if event.Type() == agent.CompactionStartEventType {
					sequence = append(sequence, "observer-start")
				}
				if event.Type() == agent.CompactionEndEventType {
					sequence = append(sequence, "observer-end")
					settlement, settled = event.(agent.CompactionEndEvent)
				}
			})
			_, compactErr := runtime.Compact(context.Background(), "")
			if test.wantAborted && !errors.Is(compactErr, agent.ErrAgentAborted) {
				t.Fatalf("cancel error = %v", compactErr)
			}
			if !test.wantAborted && (compactErr == nil || !errors.Is(compactErr, test.hookErr)) {
				t.Fatalf("hook error = %v", compactErr)
			}
			if !reflect.DeepEqual(sequence, []string{"observer-start", "hook-before", "observer-end"}) || !settled || settlement.Aborted != test.wantAborted || len(transcript.Entries()) != beforeEntries {
				t.Fatalf("settlement = sequence=%v event=%#v entries=%d", sequence, settlement, len(transcript.Entries()))
			}
		})
	}

	t.Run("automatic cancel pairs observer settlement", func(t *testing.T) {
		var sequence []string
		var settlement agent.CompactionEndEvent
		var settled bool
		runtime, err := agent.NewSession(agent.SessionConfig{
			Provider: newScriptedProvider(t, mustTextTerminal(t, "done")), SessionManager: newSessionManager(t), Model: model,
			ContextWindow: 1, KeepRecentTokens: 1,
			Summarizer: contextRetrySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
				t.Fatal("automatic summarizer ran after cancellation")
				return session.SummaryOutput{}, nil
			}),
			Hooks: agent.Hooks{SessionBeforeCompact: func(context.Context, agent.SessionBeforeCompactEvent) (agent.SessionBeforeCompactResult, error) {
				sequence = append(sequence, "hook-before")
				return agent.SessionBeforeCompactResult{Cancel: agent.HookCancel{Cancel: boolPtr(true)}}, nil
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
			if event.Type() == agent.CompactionStartEventType {
				sequence = append(sequence, "observer-start")
			}
			if event.Type() == agent.CompactionEndEventType {
				sequence = append(sequence, "observer-end")
				settlement, settled = event.(agent.CompactionEndEvent)
			}
		})
		if result, err := runtime.Run(context.Background(), "go"); err != nil || !result.Succeeded() {
			t.Fatalf("Run = (%#v, %v)", result, err)
		}
		if !reflect.DeepEqual(sequence, []string{"observer-start", "hook-before", "observer-end"}) || !settled || !settlement.Aborted {
			t.Fatalf("automatic cancellation settlement = %v / %#v", sequence, settlement)
		}
	})
}

func boolPtr(value bool) *bool                    { return &value }
func sessionAppendOptions() session.AppendOptions { return session.AppendOptions{} }

func userText(t *testing.T, message llm.ConversationMessage) string {
	t.Helper()
	switch value := message.(type) {
	case llm.UserTextMessage:
		return value.Content()[0].Text()
	case llm.UserContentMessage:
		return value.Content()[0].(llm.TextBlock).Text()
	default:
		t.Fatalf("message = %T", message)
		return ""
	}
}

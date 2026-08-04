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

func TestP0BeforeAgentStartAndMessageHooksMutateOneDurableRun(t *testing.T) {
	model, err := provider.NewModelRef("scripted", "scripted", "model")
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
			if event.Prompt != "hello" || event.SystemPrompt != "base" || len(event.Messages) != 0 {
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
				replacement, replaceErr := llm.NewAssistantTextMessage([]llm.TextBlock{mustTextBlock(t, "rewritten")}, wrapped.FinishReason(), wrapped.Usage(), wrapped.Timestamp())
				if replaceErr != nil {
					return agent.MessageHookResult{}, replaceErr
				}
				value, replaceErr := agentmsg.NewLLM(replacement)
				return agent.MessageHookResult{Message: value}, replaceErr
			}
			return agent.MessageHookResult{}, nil
		},
	}
	transcript := newSession(t)
	providerImpl := newScriptedProvider(t, mustTextTerminal(t, "original"))
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, Transcript: transcript, Model: model, SystemPrompt: "base", Hooks: hooks,
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
		"message_start:assistant", "message_end:assistant",
	}
	if !reflect.DeepEqual(sequence, wantSequence) {
		t.Fatalf("hook sequence = %v, want %v", sequence, wantSequence)
	}
	durable := transcript.Context().AgentMessages()
	if len(durable) != 3 || durable[0].Role() != agentmsg.RoleUser || durable[1].Role() != agentmsg.RoleCustom || durable[2].Role() != agentmsg.RoleAssistant {
		t.Fatalf("durable roles = %#v", durable)
	}
	custom := durable[1].(agentmsg.Custom)
	if custom.StringContent == nil || *custom.StringContent != "replaced" || string(custom.Details) != `{"source":"message_end"}` {
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

func TestP0MessageEndReplacementControlsToolExecution(t *testing.T) {
	model, _ := provider.NewModelRef("scripted", "scripted", "model")
	definition, err := provider.NewToolDefinition("blocked-by-message-end", "fixture", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	providerImpl := newScriptedProvider(t, mustToolUseTerminal(t, "call", "blocked-by-message-end", []byte(`{}`)))
	tool := &fakeTool{name: "blocked-by-message-end"}
	transcript := newSession(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, Transcript: transcript, Model: model, Tool: tool, Tools: []provider.ToolDefinition{definition},
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
				[]llm.TextBlock{mustTextBlock(t, "tool skipped")}, llm.FinishStop, toolUse.Usage(), toolUse.Timestamp(),
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
	messages := transcript.Context().Messages()
	if len(messages) != 2 {
		t.Fatalf("durable messages = %#v", messages)
	}
	if _, ok := messages[1].(llm.AssistantTextMessage); !ok {
		t.Fatalf("durable assistant = %#v", messages[1])
	}
}

func TestP0MessageEndCannotPersistMismatchedToolResultIdentity(t *testing.T) {
	model, _ := provider.NewModelRef("scripted", "scripted", "model")
	definition, _ := provider.NewToolDefinition("identity", "fixture", false, []byte(`{"type":"object"}`))
	transcript := newSession(t)
	tool := &fakeTool{name: "identity", execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		return agent.ToolOutput{Text: "executed"}, nil
	}}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider:   newScriptedProvider(t, mustToolUseTerminal(t, "call", "identity", []byte(`{}`))),
		Transcript: transcript, Model: model, Tool: tool, Tools: []provider.ToolDefinition{definition},
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
	if _, err := runtime.Run(context.Background(), "go"); !errors.Is(err, agent.ErrInvariant) {
		t.Fatalf("Run error = %v, want ErrInvariant", err)
	}
	if tool.CallCount() != 1 {
		t.Fatalf("tool calls = %d", tool.CallCount())
	}
	messages := transcript.Context().Messages()
	if len(messages) != 2 {
		t.Fatalf("mismatched tool result reached durable history: %#v", messages)
	}
}

func TestP0BeforeAgentStartCancellationNeedsNoReasonAndDoesNotPersist(t *testing.T) {
	model, _ := provider.NewModelRef("scripted", "scripted", "model")
	providerImpl := newScriptedProvider(t, mustTextTerminal(t, "must not run"))
	cancel := true
	transcript := newSession(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, Transcript: transcript, Model: model,
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
	if providerImpl.CallCount() != 0 || len(transcript.Context().AgentMessages()) != 0 {
		t.Fatalf("cancelled run called provider or persisted: calls=%d messages=%#v", providerImpl.CallCount(), transcript.Context().AgentMessages())
	}
}

func TestP0ContextHookPresencePreservesFullAgentMessages(t *testing.T) {
	model, _ := provider.NewModelRef("scripted", "scripted", "model")
	transcript := newSession(t)
	opaque, err := agentmsg.NewOpaque(agentmsg.OpaqueMessage{Type: "futureRole", Data: json.RawMessage(`{"role":"futureRole","timestamp":1,"opaque":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transcript.AppendAgentMessage(context.Background(), opaque, sessionAppendOptions()); err != nil {
		t.Fatal(err)
	}
	providerImpl := newScriptedProvider(t, mustTextTerminal(t, "done"))
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, Transcript: transcript, Model: model,
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
	emptyTranscript := newSession(t)
	emptyRuntime, err := agent.NewSession(agent.SessionConfig{
		Provider: emptyProvider, Transcript: emptyTranscript, Model: model,
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

func TestP0ToolHooksChainMutationsBeforeExecutionAndPersistence(t *testing.T) {
	model, _ := provider.NewModelRef("scripted", "scripted", "model")
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
	transcript := newSession(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, Transcript: transcript, Model: model, Tool: tool, Tools: []provider.ToolDefinition{definition},
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
	messages := transcript.Context().Messages()
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

func TestP0ToolCallHookCanBlockBeforeExecution(t *testing.T) {
	model, _ := provider.NewModelRef("scripted", "scripted", "model")
	definition, _ := provider.NewToolDefinition("blocked", "fixture", false, []byte(`{"type":"object"}`))
	providerImpl := newScriptedProvider(t, mustToolUseTerminal(t, "call", "blocked", []byte(`{}`)), mustTextTerminal(t, "done"))
	tool := &fakeTool{name: "blocked"}
	transcript := newSession(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, Transcript: transcript, Model: model, Tool: tool, Tools: []provider.ToolDefinition{definition},
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
	result := transcript.Context().Messages()[2].(llm.ToolResultMessage)
	if !result.IsError() || result.Content()[0].Text() != "denied" {
		t.Fatalf("blocked result = %#v", result)
	}
}

func TestP0SessionLifecycleAndTreeHooksRunOnRealOperations(t *testing.T) {
	model, _ := provider.NewModelRef("scripted", "scripted", "model")
	transcript := newSession(t)
	firstMessage, _ := llm.NewUserTextMessage("first", time.UnixMilli(1))
	first, err := transcript.Append(context.Background(), firstMessage, session.AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secondMessage, _ := llm.NewUserTextMessage("second", time.UnixMilli(2))
	second, err := transcript.Append(context.Background(), secondMessage, session.AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := transcript.SelectLeaf(first.ID()); err != nil {
		t.Fatal(err)
	}
	alternateMessage, _ := llm.NewUserTextMessage("alternate", time.UnixMilli(3))
	if _, err := transcript.Append(context.Background(), alternateMessage, session.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	var lifecycle []string
	var tree []agent.SessionTreeHookEvent
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), Transcript: transcript, Model: model,
		Hooks: agent.Hooks{
			SessionStart: func(_ context.Context, event agent.SessionStartHookEvent) error {
				lifecycle = append(lifecycle, "start:"+string(event.Reason))
				return nil
			},
			SessionShutdown: func(_ context.Context, event agent.SessionShutdownHookEvent) error {
				lifecycle = append(lifecycle, "shutdown:"+string(event.Reason))
				return nil
			},
			SessionTree: func(_ context.Context, event agent.SessionTreeHookEvent) (agent.SessionTreeHookResult, error) {
				tree = append(tree, event)
				return agent.SessionTreeHookResult{}, nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.SelectLeaf(context.Background(), second.ID()); err != nil {
		t.Fatal(err)
	}
	if len(tree) != 2 || !tree[0].Before || tree[1].Before || tree[0].OldLeafID == tree[0].NewLeafID || tree[1].NewLeafID != second.ID() {
		t.Fatalf("tree hooks = %#v", tree)
	}
	if got := transcript.Context().Messages(); len(got) != 2 || userText(t, got[1]) != "second" {
		t.Fatalf("selected context = %#v", got)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"start:startup", "shutdown:quit"}; !reflect.DeepEqual(lifecycle, want) {
		t.Fatalf("lifecycle = %v, want %v", lifecycle, want)
	}
}

func TestP0ManualAndAutomaticCompactionHooksRunAtCommitBoundaries(t *testing.T) {
	model, _ := provider.NewModelRef("scripted", "scripted", "model")
	t.Run("manual", func(t *testing.T) {
		transcript := newSession(t)
		for index, text := range []string{"old one", "old two", "recent"} {
			message, _ := llm.NewUserTextMessage(text, time.UnixMilli(int64(index+1)))
			if _, err := transcript.Append(context.Background(), message, session.AppendOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		var phases []bool
		var summaryInput session.SummaryInput
		summarizer := contextRetrySummarizerFunc(func(_ context.Context, input session.SummaryInput) (session.SummaryOutput, error) {
			summaryInput = input
			return session.SummaryOutput{Text: "summary"}, nil
		})
		runtime, err := agent.NewSession(agent.SessionConfig{
			Provider: newScriptedProvider(t), Transcript: transcript, Model: model, Summarizer: summarizer, KeepRecentTokens: 1,
			Hooks: agent.Hooks{SessionCompact: func(_ context.Context, event agent.SessionCompactHookEvent) (agent.SessionCompactHookResult, error) {
				if event.Reason != agent.CompactionManual {
					t.Fatalf("manual reason = %q", event.Reason)
				}
				phases = append(phases, event.Before)
				if event.Before {
					replacement := "hook instructions"
					return agent.SessionCompactHookResult{Instructions: &replacement}, nil
				}
				return agent.SessionCompactHookResult{}, nil
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result, err := runtime.Compact(context.Background(), "caller instructions"); err != nil || !result.Committed {
			t.Fatalf("Compact = (%#v, %v)", result, err)
		}
		if !reflect.DeepEqual(phases, []bool{true, false}) || summaryInput.Instructions != "hook instructions" {
			t.Fatalf("manual hook phases/input = %v/%#v", phases, summaryInput)
		}
	})

	t.Run("automatic threshold", func(t *testing.T) {
		transcript := newSession(t)
		old, _ := llm.NewUserTextMessage("old context that exceeds threshold", time.UnixMilli(1))
		if _, err := transcript.Append(context.Background(), old, session.AppendOptions{}); err != nil {
			t.Fatal(err)
		}
		var phases []bool
		summarizer := contextRetrySummarizerFunc(func(_ context.Context, _ session.SummaryInput) (session.SummaryOutput, error) {
			return session.SummaryOutput{Text: "automatic summary"}, nil
		})
		runtime, err := agent.NewSession(agent.SessionConfig{
			Provider: newScriptedProvider(t, mustTextTerminal(t, "done")), Transcript: transcript, Model: model,
			ContextWindow: 100, ContextReserve: 99, KeepRecentTokens: 1, Summarizer: summarizer,
			Hooks: agent.Hooks{SessionCompact: func(_ context.Context, event agent.SessionCompactHookEvent) (agent.SessionCompactHookResult, error) {
				if event.Reason == agent.CompactionThreshold {
					phases = append(phases, event.Before)
				}
				return agent.SessionCompactHookResult{}, nil
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Run(context.Background(), "new"); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(phases, []bool{true, false}) {
			t.Fatalf("automatic phases = %v", phases)
		}
	})
}

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

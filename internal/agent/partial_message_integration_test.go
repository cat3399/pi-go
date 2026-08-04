package agent_test

import (
	"context"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestStreamingAssistantPartialReachesHooksAndObserversWithoutPersistence(t *testing.T) {
	model, _ := provider.NewModelRef("scripted", "scripted", "model")
	thinking, err := llm.NewThinkingBlock("plan")
	if err != nil {
		t.Fatal(err)
	}
	call, err := llm.NewToolCallBlock("call-1", "fixture", []byte(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	toolUse, err := llm.NewAssistantToolUseMessage(
		[]llm.AssistantBlock{thinking, mustTextBlock(t, "working"), call},
		mustUsage(t, 5, 3), agentTestEpoch,
	)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := provider.NewToolDefinition("fixture", "fixture", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	tool := &fakeTool{name: "fixture", execute: func(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		return agent.ToolOutput{Content: []llm.ToolResultContentBlock{mustTextBlock(t, "ok")}}, nil
	}}
	transcript := newSession(t)
	var hookPartials, observerPartials []agentmsg.AssistantPartial
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t, toolUse, mustTextTerminal(t, "done")), Transcript: transcript,
		Model: model, Tool: tool, Tools: []provider.ToolDefinition{definition},
		Hooks: agent.Hooks{Message: func(_ context.Context, event agent.MessageHookEvent) (agent.MessageHookResult, error) {
			if (event.Type == agent.MessageStartHookEvent || event.Type == agent.MessageUpdateHookEvent) && event.Message.Role() == agentmsg.RoleAssistant {
				partial, ok := event.Message.(agentmsg.AssistantPartial)
				if !ok {
					t.Fatalf("streaming hook message = %T, want AssistantPartial", event.Message)
				}
				if event.ProviderEvent == nil || partial.ProviderEvent() == nil {
					t.Fatal("streaming hook lost provider event")
				}
				hookPartials = append(hookPartials, partial)
			}
			return agent.MessageHookResult{}, nil
		}},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	unsubscribe := runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if event.Type != "message_start" && event.Type != "message_update" {
			return
		}
		if partial, ok := event.AgentMessage.(agentmsg.AssistantPartial); ok {
			observerPartials = append(observerPartials, partial)
		}
	})
	defer unsubscribe()
	result, err := runtime.Run(context.Background(), "go")
	if err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	if len(hookPartials) == 0 || len(observerPartials) != len(hookPartials) {
		t.Fatalf("partial counts hook/observer = %d/%d", len(hookPartials), len(observerPartials))
	}
	seen := map[llm.AssistantBlockKind]bool{}
	seenRawToolDelta := false
	for _, partial := range hookPartials {
		if partial.Role() != agentmsg.RoleAssistant || partial.Snapshot().Terminal() {
			t.Fatalf("invalid partial = %#v", partial)
		}
		if partial.API() != "scripted" || partial.Provider() != "scripted" || partial.Model() != "model" {
			t.Fatalf("partial provenance = %q/%q/%q", partial.API(), partial.Provider(), partial.Model())
		}
		if !partial.Timestamp().Equal(agentTestEpoch) || partial.FinishReason() != llm.FinishPending {
			t.Fatalf("partial timestamp/reason = %v/%q", partial.Timestamp(), partial.FinishReason())
		}
		usage := partial.Usage()
		cost, hasCost := usage.Cost()
		if usage.Input() != 0 || usage.Output() != 0 || usage.CacheRead() != 0 || usage.CacheWrite() != 0 || usage.TotalTokens() != 0 || !hasCost || cost != (llm.Cost{}) {
			t.Fatalf("partial usage = %#v, cost=%#v present=%t", usage, cost, hasCost)
		}
		if active, ok := partial.Snapshot().ActiveBlock(); ok {
			seen[active.Kind()] = true
			if _, _, arguments, toolOK := active.ToolCall(); toolOK && len(arguments) != 0 {
				seenRawToolDelta = true
				arguments[0] = 'x'
				_, _, again, _ := active.ToolCall()
				if len(again) != 0 && again[0] == 'x' {
					t.Fatal("partial tool arguments accessor leaked mutable storage")
				}
			}
		}
	}
	if !seen[llm.AssistantBlockThinking] || !seen[llm.AssistantBlockText] || !seen[llm.AssistantBlockToolCall] || !seenRawToolDelta {
		t.Fatalf("streaming rich kinds = %#v rawToolDelta=%t", seen, seenRawToolDelta)
	}
	for _, message := range transcript.Context().AgentMessages() {
		if _, partial := message.(agentmsg.AssistantPartial); partial {
			t.Fatal("partial assistant was persisted")
		}
	}
}

func TestTurnAndToolExecutionHooksMirrorOriginalLifecycle(t *testing.T) {
	model, _ := provider.NewModelRef("scripted", "scripted", "model")
	call, err := llm.NewToolCallBlock("call-1", "fixture", []byte(`{"value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	toolUse, err := llm.NewAssistantToolUseMessage(
		[]llm.AssistantBlock{mustTextBlock(t, "working"), call},
		mustUsage(t, 5, 3), agentTestEpoch,
	)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := provider.NewToolDefinition("fixture", "fixture", false, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	tool := &fakeTool{name: "fixture", execute: func(_ context.Context, _ []byte, report func(agent.ToolUpdate)) (agent.ToolOutput, error) {
		report(agent.ToolUpdate{Text: "half"})
		return agent.ToolOutput{Content: []llm.ToolResultContentBlock{mustTextBlock(t, "ok")}}, nil
	}}

	var turns []agent.TurnLifecycleEvent
	var tools []agent.ToolExecutionLifecycleEvent
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider:   newScriptedProvider(t, toolUse, mustTextTerminal(t, "done")),
		Transcript: newSession(t), Model: model, Tool: tool,
		Tools: []provider.ToolDefinition{definition}, ToolExecution: agent.ToolExecutionSequential,
		Hooks: agent.Hooks{
			Turn: func(_ context.Context, event agent.TurnLifecycleEvent) error {
				turns = append(turns, event)
				return nil
			},
			ToolExecution: func(_ context.Context, event agent.ToolExecutionLifecycleEvent) error {
				tools = append(tools, event)
				return nil
			},
		},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "go")
	if err != nil || !result.Succeeded() {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}

	if len(turns) != 4 {
		t.Fatalf("turn hook count = %d, want 4: %#v", len(turns), turns)
	}
	if turns[0].Type != agent.TurnStartHookEvent || turns[0].TurnIndex != 0 || !turns[0].Timestamp.Equal(agentTestEpoch) || turns[0].Message != nil || len(turns[0].ToolResults) != 0 {
		t.Fatalf("first turn_start = %#v", turns[0])
	}
	if turns[1].Type != agent.TurnEndHookEvent || turns[1].TurnIndex != 0 || turns[1].Message == nil || turns[1].Message.Role() != agentmsg.RoleAssistant || len(turns[1].ToolResults) != 1 || turns[1].ToolResults[0].Role() != agentmsg.RoleToolResult {
		t.Fatalf("first turn_end = %#v", turns[1])
	}
	if turns[2].Type != agent.TurnStartHookEvent || turns[2].TurnIndex != 1 || !turns[2].Timestamp.Equal(agentTestEpoch) {
		t.Fatalf("second turn_start = %#v", turns[2])
	}
	if turns[3].Type != agent.TurnEndHookEvent || turns[3].TurnIndex != 1 || turns[3].Message == nil || turns[3].Message.Role() != agentmsg.RoleAssistant || len(turns[3].ToolResults) != 0 {
		t.Fatalf("second turn_end = %#v", turns[3])
	}

	if len(tools) != 3 {
		t.Fatalf("tool hook count = %d, want 3: %#v", len(tools), tools)
	}
	if tools[0].Type != agent.ToolExecutionStartHookEvent || tools[0].ToolCallID != "call-1" || tools[0].ToolName != "fixture" || string(tools[0].Arguments) != `{"value":1}` || tools[0].Update != nil || tools[0].Result != nil {
		t.Fatalf("tool start = %#v", tools[0])
	}
	if tools[1].Type != agent.ToolExecutionUpdateHookEvent || tools[1].Update == nil || tools[1].Update.Text != "half" || tools[1].Result != nil {
		t.Fatalf("tool update = %#v", tools[1])
	}
	if tools[2].Type != agent.ToolExecutionEndHookEvent || tools[2].Result == nil || tools[2].IsError || tools[2].Update != nil || len(tools[2].Result.Content) != 1 {
		t.Fatalf("tool end = %#v", tools[2])
	}
}

func TestBeforeAgentStartReceivesRichMultiMessagePrompt(t *testing.T) {
	model, _ := provider.NewModelRef("scripted", "scripted", "model")
	text, _ := llm.NewTextBlock("look")
	image, err := llm.NewImageDataBlock("image/png", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	user, err := llm.NewUserContentMessage([]llm.UserContentBlock{text, image}, agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := agentmsg.NewLLM(user)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := agentmsg.NewCustomText("pending", "pending context", false, nil, agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	injected, err := agentmsg.NewCustomText("extension", "extension context", false, nil, agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	customPrompt := "raw system prompt"
	appendPrompt := "appendix"
	sourceBase := "/skills"
	selectedTools := []string{"read"}
	toolSnippets := map[string]string{"read": "Read files"}
	promptGuidelines := []string{"Keep paths exact"}
	contextFiles := []agent.SystemPromptContextFile{{Path: "/workspace/AGENTS.md", Content: "rules"}}
	skills := []agent.SystemPromptSkill{{
		Name: "review", Description: "Review code", FilePath: "/skills/review/SKILL.md", BaseDir: "/skills/review",
		SourceInfo: agent.SystemPromptSourceInfo{Path: "/skills/review/SKILL.md", Source: "fixture", Scope: agent.SystemPromptSourceUser, Origin: agent.SystemPromptSourceTopLevel, BaseDir: &sourceBase},
	}}
	implementation := newScriptedProvider(t, mustTextTerminal(t, "done"))
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, Transcript: newSession(t), Model: model,
		SystemPromptOptions: agent.BuildSystemPromptOptions{
			CustomPrompt: &customPrompt, SelectedTools: selectedTools, ToolSnippets: toolSnippets,
			PromptGuidelines: promptGuidelines, AppendSystemPrompt: &appendPrompt, CWD: "/workspace",
			ContextFiles: contextFiles, Skills: skills,
		},
		Hooks: agent.Hooks{BeforeAgentStart: func(_ context.Context, event agent.BeforeAgentStartEvent) (agent.BeforeAgentStartResult, error) {
			if event.Prompt != "look" || len(event.Images) != 1 || len(event.PromptMessages) != 2 || event.PromptMessages[0].Role() != agentmsg.RoleUser || event.PromptMessages[1].Role() != agentmsg.RoleCustom {
				t.Fatalf("rich before_agent_start = %#v", event)
			}
			options := event.SystemPromptOptions
			if options.CustomPrompt == nil || *options.CustomPrompt != "raw system prompt" || options.AppendSystemPrompt == nil || *options.AppendSystemPrompt != "appendix" || options.CWD != "/workspace" || len(options.SelectedTools) != 1 || options.SelectedTools[0] != "read" || options.ToolSnippets["read"] != "Read files" || len(options.PromptGuidelines) != 1 || len(options.ContextFiles) != 1 || len(options.Skills) != 1 || options.Skills[0].SourceInfo.BaseDir == nil || *options.Skills[0].SourceInfo.BaseDir != "/skills" {
				t.Fatalf("structured system prompt options = %#v", options)
			}
			return agent.BeforeAgentStartResult{ExtraMessages: []agentmsg.Message{injected}}, nil
		}},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	selectedTools[0] = "mutated"
	toolSnippets["read"] = "mutated"
	promptGuidelines[0] = "mutated"
	contextFiles[0].Content = "mutated"
	skills[0].SourceInfo.BaseDir = nil
	if result, err := runtime.RunMessages(context.Background(), []agentmsg.Message{wrapper, pending}); err != nil || !result.Succeeded() {
		t.Fatalf("RunMessages = (%#v, %v)", result, err)
	}
	requests := implementation.Requests()
	if len(requests) != 1 || len(requests[0].Messages()) != 3 {
		t.Fatalf("provider rich multi-message request = %#v", requests)
	}
	first, ok := requests[0].Messages()[0].(llm.UserContentMessage)
	if !ok || len(first.Content()) != 2 {
		t.Fatalf("provider rich prompt = %#v", requests[0].Messages()[0])
	}
	if got := userText(t, requests[0].Messages()[1]); got != "pending context" {
		t.Fatalf("pending projection = %q", got)
	}
	if got := userText(t, requests[0].Messages()[2]); got != "extension context" {
		t.Fatalf("extension projection = %q", got)
	}
}

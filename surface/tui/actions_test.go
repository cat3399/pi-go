package tui

import (
	"testing"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestPlanInputPreservesInteractiveQueueSemantics(t *testing.T) {
	idle, err := planInput("hello", application.State{}, false)
	if err != nil {
		t.Fatal(err)
	}
	idlePrompt, ok := idle.command.(application.PromptCommand)
	if !ok || idlePrompt.StreamingBehavior != "" || idlePrompt.Source != agent.InputInteractive {
		t.Fatalf("idle prompt = %#v", idle.command)
	}

	streaming := application.State{IsStreaming: true}
	steer, err := planInput("change direction", streaming, false)
	if err != nil {
		t.Fatal(err)
	}
	if prompt := steer.command.(application.PromptCommand); prompt.StreamingBehavior != agent.StreamingSteer {
		t.Fatalf("steer behavior = %q", prompt.StreamingBehavior)
	}
	follow, err := planInput("afterwards", streaming, true)
	if err != nil {
		t.Fatal(err)
	}
	if prompt := follow.command.(application.PromptCommand); prompt.StreamingBehavior != agent.StreamingFollowUp {
		t.Fatalf("follow-up behavior = %q", prompt.StreamingBehavior)
	}
}

func TestPlanInputKeepsSurfaceActionsOutOfCoreCommands(t *testing.T) {
	resume, err := planInput("/resume session-123", application.State{}, false)
	if err != nil || resume.kind != inputActionOpenSession || resume.sessionID != "session-123" || resume.command != nil {
		t.Fatalf("resume = %#v, err=%v", resume, err)
	}
	newSession, err := planInput("/new", application.State{}, false)
	if err != nil || newSession.kind != inputActionNewSession || newSession.command != nil {
		t.Fatalf("new = %#v, err=%v", newSession, err)
	}
	resumeSelector, err := planInput("/resume", application.State{}, false)
	if err != nil || resumeSelector.kind != inputActionSessionSelector {
		t.Fatalf("resume selector = %#v, err=%v", resumeSelector, err)
	}
	modelSelector, err := planInput("/model deepseek-v4", application.State{}, false)
	if err != nil || modelSelector.kind != inputActionModelSelector || modelSelector.query != "deepseek-v4" {
		t.Fatalf("model selector = %#v, err=%v", modelSelector, err)
	}
	exactModel, err := planInput("/model openrouter/anthropic/claude-sonnet", application.State{}, false)
	setModel, ok := exactModel.command.(application.SetModelCommand)
	if err != nil || exactModel.kind != inputActionDispatch || !ok ||
		setModel.Provider != "openrouter" || setModel.ModelID != "anthropic/claude-sonnet" {
		t.Fatalf("exact model = %#v, err=%v", exactModel, err)
	}
	thinkingSelector, err := planInput("/thinking", application.State{}, false)
	if err != nil || thinkingSelector.kind != inputActionThinkingSelector {
		t.Fatalf("thinking selector = %#v, err=%v", thinkingSelector, err)
	}
	toolsSelector, err := planInput("/tools", application.State{}, false)
	if err != nil || toolsSelector.kind != inputActionToolsSelector {
		t.Fatalf("tools selector = %#v, err=%v", toolsSelector, err)
	}
}

func TestPlanInputMapsCoreCommandsWithoutLosingArguments(t *testing.T) {
	tests := []struct {
		input string
		check func(application.Command) bool
	}{
		{"!! printf hi", func(command application.Command) bool {
			value, ok := command.(application.BashCommand)
			return ok && value.Command == "printf hi" && value.ExcludeFromContext
		}},
		{"/thinking high", func(command application.Command) bool {
			value, ok := command.(application.SetThinkingLevelCommand)
			return ok && value.Level == provider.ThinkingHigh
		}},
		{"/compact retain decisions", func(command application.Command) bool {
			value, ok := command.(application.CompactCommand)
			return ok && value.CustomInstructions == "retain decisions"
		}},
		{"/copy", func(command application.Command) bool {
			_, ok := command.(application.GetLastAssistantTextCommand)
			return ok
		}},
	}
	for _, test := range tests {
		action, err := planInput(test.input, application.State{}, false)
		if err != nil || action.kind != inputActionDispatch || !test.check(action.command) {
			t.Errorf("planInput(%q) = %#v, %v", test.input, action, err)
		}
	}
}

func TestUnknownSlashCommandStillReachesPromptPipeline(t *testing.T) {
	action, err := planInput("/extension-command value", application.State{}, false)
	if err != nil {
		t.Fatal(err)
	}
	prompt, ok := action.command.(application.PromptCommand)
	if !ok || prompt.Message != "/extension-command value" {
		t.Fatalf("action = %#v", action)
	}
}

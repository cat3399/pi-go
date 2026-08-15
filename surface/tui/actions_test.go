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
		{"/model deepseek/deepseek-v4-flash", func(command application.Command) bool {
			value, ok := command.(application.SetModelCommand)
			return ok && value.Provider == "deepseek" && value.ModelID == "deepseek-v4-flash"
		}},
		{"/thinking high", func(command application.Command) bool {
			value, ok := command.(application.SetThinkingLevelCommand)
			return ok && value.Level == provider.ThinkingHigh
		}},
		{"/compact retain decisions", func(command application.Command) bool {
			value, ok := command.(application.CompactCommand)
			return ok && value.CustomInstructions == "retain decisions"
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

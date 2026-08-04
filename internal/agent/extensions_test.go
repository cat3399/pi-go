package agent

import (
	"context"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

func TestExtensionHookContractsCloneAndProjectContext(t *testing.T) {
	message, err := llm.NewUserTextMessage("original", time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := agentmsg.NewLLM(message)
	if err != nil {
		t.Fatal(err)
	}
	transform := contextHookTransform(func(_ context.Context, event ContextHookEvent) (ContextHookResult, error) {
		if len(event.Messages) != 1 {
			t.Fatalf("messages = %d", len(event.Messages))
		}
		custom, err := agentmsg.NewCustomText("extension", "rich", true, []byte(`{"kept":true}`), time.UnixMilli(2))
		if err != nil {
			return ContextHookResult{}, err
		}
		replacement := append(event.Messages, custom)
		return ContextHookResult{Messages: &replacement}, nil
	})
	result, err := transform(context.Background(), []agentmsg.Message{wrapper})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || len(*result) != 2 {
		t.Fatalf("projection = %#v", result)
	}
	cancel := true
	if err := (HookCancel{Cancel: &cancel}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestExtensionHookSurfaceIsTyped(t *testing.T) {
	var hooks Hooks
	hooks.BeforeAgentStart = func(context.Context, BeforeAgentStartEvent) (BeforeAgentStartResult, error) {
		cancel := true
		return BeforeAgentStartResult{Cancel: HookCancel{Cancel: &cancel}}, nil
	}
	hooks.SessionTree = func(context.Context, SessionTreeEvent) error { return nil }
	hooks.SessionShutdown = func(context.Context, SessionShutdownHookEvent) error { return nil }
	if hooks.BeforeAgentStart == nil || hooks.SessionTree == nil || hooks.SessionShutdown == nil {
		t.Fatal("typed hooks were not assignable")
	}
}

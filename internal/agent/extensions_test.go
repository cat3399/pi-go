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
	result, err := invokeContextHook(context.Background(), func(_ context.Context, event ContextHookEvent) (ContextHookResult, error) {
		if len(event.Messages) != 1 {
			t.Fatalf("messages = %d", len(event.Messages))
		}
		custom, err := agentmsg.NewCustomText("extension", "rich", true, []byte(`{"kept":true}`), time.UnixMilli(2))
		if err != nil {
			return ContextHookResult{}, err
		}
		return ContextHookResult{Messages: append(event.Messages, custom)}, nil
	}, []llm.ConversationMessage{message})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 {
		t.Fatalf("projection = %#v", result)
	}
	if err := (HookCancel{Cancel: true}).Validate(); err == nil {
		t.Fatal("cancel without reason accepted")
	}
	if err := (HookCancel{Cancel: true, Reason: "policy"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestExtensionHookSurfaceIsTyped(t *testing.T) {
	var hooks Hooks
	hooks.BeforeAgentStart = func(context.Context, BeforeAgentStartEvent) (BeforeAgentStartResult, error) {
		return BeforeAgentStartResult{Cancel: HookCancel{Cancel: true, Reason: "blocked"}}, nil
	}
	hooks.SessionTree = func(context.Context, SessionTreeHookEvent) (SessionTreeHookResult, error) {
		return SessionTreeHookResult{}, nil
	}
	hooks.SessionSwitch = func(context.Context, SessionSwitchHookEvent) (SessionSwitchHookResult, error) {
		return SessionSwitchHookResult{}, nil
	}
	hooks.SessionShutdown = func(context.Context, SessionShutdownHookEvent) error { return nil }
	if hooks.BeforeAgentStart == nil || hooks.SessionTree == nil || hooks.SessionSwitch == nil || hooks.SessionShutdown == nil {
		t.Fatal("typed hooks were not assignable")
	}
}

package agent_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

func TestModelLessAgentSessionConstructionAndPreflight(t *testing.T) {
	cwd := t.TempDir()
	manager, err := session.CreateSessionManager(cwd, t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	providerImpl, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	startCalls := 0
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, SessionManager: manager, ThinkingLevel: provider.ThinkingHigh,
		InitializeSessionState: true,
		Hooks: agent.Hooks{SessionStart: func(context.Context, agent.SessionStartHookEvent) error {
			startCalls++
			entries := manager.Entries()
			if len(entries) != 1 {
				t.Fatalf("entries visible to session_start = %d, want 1", len(entries))
			}
			if change, ok := entries[0].Payload().(session.ThinkingLevelChangePayload); !ok || change.ThinkingLevel != "off" {
				t.Fatalf("initial entry = %#v", entries[0].Payload())
			}
			return nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	if startCalls != 1 || runtime.HasModel() || runtime.ThinkingLevel() != provider.ThinkingOff {
		t.Fatalf("constructed state: starts=%d model=%t thinking=%q", startCalls, runtime.HasModel(), runtime.ThinkingLevel())
	}
	if _, ok := runtime.SelectedModel(); ok {
		t.Fatal("SelectedModel reports zero model as selected")
	}
	path, ok := manager.SessionFile()
	if !ok {
		t.Fatal("persistent manager did not reserve a path")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata-only construction created session file: %v", err)
	}
	entryCount := len(manager.Entries())
	if _, err := runtime.Prompt(context.Background(), "must not persist"); !errors.Is(err, agent.ErrNoModelSelected) {
		t.Fatalf("Prompt error = %v", err)
	}
	if _, err := runtime.Continue(context.Background()); !errors.Is(err, agent.ErrNoModelSelected) {
		t.Fatalf("Continue error = %v", err)
	}
	if _, err := runtime.Compact(context.Background(), "blocked"); !errors.Is(err, agent.ErrNoModelSelected) || err.Error() != "No model selected." {
		t.Fatalf("Compact error = %v", err)
	}
	if len(manager.Entries()) != entryCount || providerImpl.CallCount() != 0 {
		t.Fatalf("preflight side effects: entries=%d calls=%d", len(manager.Entries()), providerImpl.CallCount())
	}
	terminal := mustTextTerminal(t, "revived")
	step, err := provider.FixedResponseStep(terminal)
	if err != nil {
		t.Fatal(err)
	}
	if err := providerImpl.SetResponses([]provider.ScriptStep{step}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetModel(sessionTestModel(t)); err != nil {
		t.Fatal(err)
	}
	if !runtime.HasModel() {
		t.Fatal("SetModel did not revive AgentSession")
	}
	if _, err := runtime.Prompt(context.Background(), "now execute"); err != nil {
		t.Fatal(err)
	}
	if providerImpl.CallCount() != 1 {
		t.Fatalf("provider calls after SetModel = %d, want 1", providerImpl.CallCount())
	}
}

func TestAgentSessionInitialStateForExistingHistoryDoesNotWriteModel(t *testing.T) {
	manager, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	user, err := llm.NewUserTextMessage("existing", time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	providerImpl, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	model := sessionTestModel(t)
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: providerImpl, SessionManager: manager, Model: model,
		ThinkingLevel: provider.ThinkingMedium, InitializeSessionState: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	entries := manager.Entries()
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want message + thinking", len(entries))
	}
	if _, ok := entries[1].Payload().(session.ThinkingLevelChangePayload); !ok {
		t.Fatalf("last entry = %#v, want thinking change", entries[1].Payload())
	}
}

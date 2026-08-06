package agent_test

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestAgentSessionModelAccessFailsBeforePromptSideEffects(t *testing.T) {
	manager := newSessionManager(t)
	path, ok := manager.SessionFile()
	if !ok {
		t.Fatal("persistent manager has no reserved path")
	}
	implementation := newScriptedProvider(t)
	model := sessionTestModel(t)
	var accessCalls atomic.Uint32
	var beforeAgentCalls atomic.Uint32
	accessErr := &agent.ModelAccessError{Message: "model access denied by fixture"}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: implementation, SessionManager: manager, Model: model, ThinkingLevel: provider.ThinkingOff,
		InitializeSessionState: true,
		ValidateModelAccess: func(context.Context, provider.Model) error {
			accessCalls.Add(1)
			return accessErr
		},
		Hooks: agent.Hooks{BeforeAgentStart: func(context.Context, agent.BeforeAgentStartEvent) (agent.BeforeAgentStartResult, error) {
			beforeAgentCalls.Add(1)
			return agent.BeforeAgentStartResult{}, nil
		}},
		Now: func() time.Time { return agentTestEpoch }, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	baseline := manager.Entries()
	if len(baseline) != 2 {
		t.Fatalf("initial metadata entries = %d, want 2", len(baseline))
	}

	checks := []struct {
		name string
		run  func() error
	}{
		{name: "run", run: func() error { _, err := runtime.Run(context.Background(), "prompt"); return err }},
		{name: "content", run: func() error { _, err := runtime.RunContent(context.Background(), nil); return err }},
		{name: "messages", run: func() error { _, err := runtime.RunMessages(context.Background(), []agentmsg.Message{nil}); return err }},
		{name: "continue", run: func() error { _, err := runtime.Continue(context.Background()); return err }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); !errors.Is(err, agent.ErrModelAccess) || err.Error() != accessErr.Error() {
				t.Fatalf("access error = %v", err)
			}
		})
	}
	if got := accessCalls.Load(); got != uint32(len(checks)) {
		t.Fatalf("access calls = %d, want %d", got, len(checks))
	}
	if beforeAgentCalls.Load() != 0 {
		t.Fatalf("before_agent_start calls = %d", beforeAgentCalls.Load())
	}
	if requests := implementation.Requests(); len(requests) != 0 {
		t.Fatalf("provider requests = %d", len(requests))
	}
	if entries := manager.Entries(); len(entries) != len(baseline) {
		t.Fatalf("entries changed from %d to %d", len(baseline), len(entries))
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata-only session persisted: %v", err)
	}
}

func TestAgentSessionManualCompactValidatesAccessAfterStartBeforeHooks(t *testing.T) {
	manager := newSessionManager(t)
	model := sessionTestModel(t)
	var beforeCompactCalls atomic.Uint32
	var startEvents atomic.Uint32
	accessErr := &agent.ModelAccessError{Message: "compact access denied"}
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: manager, Model: model,
		ThinkingLevel: provider.ThinkingOff, Summarizer: sessionRetrySummarizer{},
		ValidateModelAccess: func(context.Context, provider.Model) error { return accessErr },
		Hooks: agent.Hooks{SessionBeforeCompact: func(context.Context, agent.SessionBeforeCompactEvent) (agent.SessionBeforeCompactResult, error) {
			beforeCompactCalls.Add(1)
			return agent.SessionBeforeCompactResult{}, nil
		}},
		SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if event.Type() == agent.CompactionStartEventType {
			startEvents.Add(1)
		}
	})
	baseline := len(manager.Entries())
	if _, err := runtime.Compact(context.Background(), "manual"); !errors.Is(err, agent.ErrModelAccess) || err.Error() != accessErr.Error() {
		t.Fatalf("Compact error = %v", err)
	}
	if startEvents.Load() != 1 {
		t.Fatalf("compaction_start events = %d", startEvents.Load())
	}
	if beforeCompactCalls.Load() != 0 {
		t.Fatalf("session_before_compact calls = %d", beforeCompactCalls.Load())
	}
	if got := len(manager.Entries()); got != baseline {
		t.Fatalf("entries changed from %d to %d", baseline, got)
	}
}

func TestAgentSessionManualCompactUnavailableUsesOriginalFailurePrefix(t *testing.T) {
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t), Model: sessionTestModel(t),
		ThinkingLevel: provider.ThinkingOff,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	var errorMessage string
	runtime.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if compact, ok := event.(agent.CompactionEndEvent); ok {
			errorMessage = compact.ErrorMessage
		}
	})
	if _, err := runtime.Compact(context.Background(), "manual"); !errors.Is(err, agent.ErrCompactionUnavailable) {
		t.Fatalf("Compact error = %v", err)
	}
	if want := "Compaction failed: " + agent.ErrCompactionUnavailable.Error(); errorMessage != want {
		t.Fatalf("compaction_end error = %q, want %q", errorMessage, want)
	}
}

func TestAgentSessionSetModelValidatesAccessBeforeStateEntryAndHook(t *testing.T) {
	initial := sessionTestModel(t)
	replacement, err := provider.NewModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: "blocked", Name: "Blocked",
		Input: []provider.InputKind{provider.InputText}, ContextWindow: 16_000, MaxTokens: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := newSessionManager(t)
	var hookCalls atomic.Uint32
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: manager, Model: initial,
		ThinkingLevel: provider.ThinkingOff,
		ValidateModelSelection: func(_ context.Context, selected provider.Model) error {
			if selected.Equal(replacement) {
				return &agent.ModelAccessError{Message: "replacement access denied"}
			}
			return nil
		},
		Hooks: agent.Hooks{ModelSelect: func(context.Context, agent.ModelSelectEvent) error {
			hookCalls.Add(1)
			return nil
		}},
		SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	baseline := len(manager.Entries())
	if err := runtime.SetModel(replacement); !errors.Is(err, agent.ErrModelAccess) || err.Error() != "replacement access denied" {
		t.Fatalf("SetModel error = %v", err)
	}
	selected, ok := runtime.SelectedModel()
	if !ok || !selected.Equal(initial) {
		t.Fatalf("selected model changed = %#v, %t", selected, ok)
	}
	if got := len(manager.Entries()); got != baseline {
		t.Fatalf("entries changed from %d to %d", baseline, got)
	}
	if hookCalls.Load() != 0 {
		t.Fatalf("model_select calls = %d", hookCalls.Load())
	}
}

func TestAgentSessionModelLessGuidancePrecedesAccessValidator(t *testing.T) {
	var accessCalls atomic.Uint32
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t),
		NoModelSelectedMessage: "complete model-less guidance",
		ValidateModelAccess: func(context.Context, provider.Model) error {
			accessCalls.Add(1)
			return &agent.ModelAccessError{Message: "must not win"}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	if _, err := runtime.Run(context.Background(), "prompt"); err == nil || err.Error() != "complete model-less guidance" {
		t.Fatalf("Run error = %v", err)
	}
	if accessCalls.Load() != 0 {
		t.Fatalf("access validator calls = %d", accessCalls.Load())
	}
}

package model

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

func TestSettingsCommitUnknownReconcilesRuntimeAndAgentForward(t *testing.T) {
	runtime, agentDir := unknownSettingsRuntime(t)
	runtime.faults.afterRename = func() error { return errors.New("directory sync acknowledgement lost") }
	a, b := unknownAgentModel(t, "a"), unknownAgentModel(t, "b")
	manager := unknownAgentManager(t)
	events := 0
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: unknownAgentProvider(t), SessionManager: manager, Model: a, ThinkingLevel: provider.ThinkingHigh,
		PersistSettings: unknownSettingsPersistence(runtime),
		Hooks:           agent.Hooks{ModelSelect: func(context.Context, agent.ModelSelectEvent) error { events++; return nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })
	err = coordinator.SetModel(b)
	if !errors.Is(err, ErrCommitUnknown) || errors.Is(err, agent.ErrTranscriptCommit) {
		t.Fatalf("SetModel error = %v", err)
	}
	selected, ok := coordinator.SelectedModel()
	entries := manager.Entries()
	if !ok || !selected.Equal(b) || len(entries) != 1 || entries[0].Type() != "model_change" || events != 1 {
		t.Fatalf("forward state: model=%s present=%t entries=%d events=%d", selected.ID(), ok, len(entries), events)
	}
	if got := runtime.Snapshot().Settings.DefaultModel; got != "b" {
		t.Fatalf("runtime snapshot = %q", got)
	}
	data, readErr := os.ReadFile(filepath.Join(agentDir, "settings.json"))
	if readErr != nil || !strings.Contains(string(data), `"defaultModel": "b"`) {
		t.Fatalf("settings file = %s, %v", data, readErr)
	}
}

func TestSettingsCommitUnknownThenDefiniteTranscriptFailureCompensatesForward(t *testing.T) {
	runtime, agentDir := unknownSettingsRuntime(t)
	runtime.faults.afterRename = func() error { return errors.New("directory sync acknowledgement lost") }
	a, b := unknownAgentModel(t, "a"), unknownAgentModel(t, "b")
	manager := unknownAgentManager(t)
	events := 0
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: unknownAgentProvider(t), SessionManager: manager, Model: a, ThinkingLevel: provider.ThinkingHigh,
		PersistSettings: unknownSettingsPersistence(runtime),
		Hooks:           agent.Hooks{ModelSelect: func(context.Context, agent.ModelSelectEvent) error { events++; return nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	err = coordinator.SetModel(b)
	if !errors.Is(err, ErrCommitUnknown) || !errors.Is(err, agent.ErrTranscriptCommit) {
		t.Fatalf("SetModel error = %v", err)
	}
	selected, ok := coordinator.SelectedModel()
	if !ok || !selected.Equal(a) || len(manager.Entries()) != 0 || events != 0 {
		t.Fatalf("compensated state: model=%s present=%t entries=%d events=%d", selected.ID(), ok, len(manager.Entries()), events)
	}
	if got := runtime.Snapshot().Settings.DefaultModel; got != "a" {
		t.Fatalf("runtime snapshot after compensation = %q", got)
	}
	data, readErr := os.ReadFile(filepath.Join(agentDir, "settings.json"))
	if readErr != nil || !strings.Contains(string(data), `"defaultModel": "a"`) {
		t.Fatalf("settings file after compensation = %s, %v", data, readErr)
	}
}

func TestRuntimeControlSettingsCommitUnknownPublishesForward(t *testing.T) {
	runtime, agentDir := unknownSettingsRuntime(t)
	runtime.faults.afterRename = func() error { return errors.New("directory sync acknowledgement lost") }
	selected := unknownAgentModel(t, "a")
	manager := unknownAgentManager(t)
	enabled := true
	coordinator, err := agent.NewSession(agent.SessionConfig{
		Provider: unknownAgentProvider(t), SessionManager: manager, Model: selected,
		SteeringMode: agent.QueueOneAtATime, FollowUpMode: agent.QueueOneAtATime,
		CompactionEnabled: &enabled, AutoRetryEnabled: &enabled,
		Retry:           agent.RetryPolicy{MaxAttempts: 4},
		PersistSettings: unknownSettingsPersistence(runtime),
		ResolveRuntimeSettings: func() agent.RuntimeControlSettings {
			settings := runtime.Snapshot().Settings
			return agent.RuntimeControlSettings{
				SteeringMode:          queueModeForAgentTest(settings.SteeringModeOrDefault()),
				FollowUpMode:          queueModeForAgentTest(settings.FollowUpModeOrDefault()),
				AutoCompactionEnabled: settings.Compaction.EnabledOrDefault(),
				AutoRetryEnabled:      settings.Retry.EnabledOrDefault(),
				Retry: agent.RetryPolicy{
					MaxAttempts: uint32(settings.Retry.MaxRetriesOrDefault()) + 1,
				},
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })

	for name, change := range map[string]func() error{
		"steering":   func() error { return coordinator.SetSteeringMode(agent.QueueAll) },
		"follow-up":  func() error { return coordinator.SetFollowUpMode(agent.QueueAll) },
		"compaction": func() error { return coordinator.SetAutoCompactionEnabled(false) },
		"retry":      func() error { return coordinator.SetAutoRetryEnabled(false) },
	} {
		if err := change(); !errors.Is(err, ErrCommitUnknown) {
			t.Fatalf("%s error = %v", name, err)
		}
	}
	if coordinator.SteeringMode() != agent.QueueAll || coordinator.FollowUpMode() != agent.QueueAll ||
		coordinator.AutoCompactionEnabled() || coordinator.AutoRetryEnabled() {
		t.Fatalf("forward controls = %s/%s compaction=%t retry=%t", coordinator.SteeringMode(), coordinator.FollowUpMode(), coordinator.AutoCompactionEnabled(), coordinator.AutoRetryEnabled())
	}
	settings := runtime.Snapshot().Settings
	if settings.SteeringMode != QueueModeAll || settings.FollowUpMode != QueueModeAll ||
		settings.Compaction.Enabled == nil || *settings.Compaction.Enabled ||
		settings.Retry.Enabled == nil || *settings.Retry.Enabled {
		t.Fatalf("runtime settings = %#v", settings)
	}
	data, readErr := os.ReadFile(filepath.Join(agentDir, "settings.json"))
	if readErr != nil || !strings.Contains(string(data), `"steeringMode": "all"`) ||
		!strings.Contains(string(data), `"followUpMode": "all"`) {
		t.Fatalf("settings file = %s, %v", data, readErr)
	}
}

func unknownSettingsRuntime(t *testing.T) (*Runtime, string) {
	t.Helper()
	agentDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(`{"defaultProvider":"scripted","defaultModel":"a","defaultThinkingLevel":"high"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(Options{AgentDir: agentDir, WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return runtime, agentDir
}

func unknownSettingsPersistence(runtime *Runtime) agent.SettingsPersistence {
	return func(ctx context.Context, update agent.SettingsUpdate) (agent.SettingsWriteResult, error) {
		var previous Settings
		err := runtime.SetGlobalSettings(ctx, func(settings *Settings) error {
			previous.DefaultProvider = settings.DefaultProvider
			previous.DefaultModel = settings.DefaultModel
			previous.DefaultThinkingLevel = settings.DefaultThinkingLevel
			previous.SteeringMode = settings.SteeringMode
			previous.FollowUpMode = settings.FollowUpMode
			previous.Compaction.Enabled = cloneBoolPointer(settings.Compaction.Enabled)
			previous.Retry.Enabled = cloneBoolPointer(settings.Retry.Enabled)
			if update.DefaultProvider != nil {
				settings.DefaultProvider = *update.DefaultProvider
			}
			if update.DefaultModel != nil {
				settings.DefaultModel = *update.DefaultModel
			}
			if update.DefaultThinkingLevel != nil {
				settings.DefaultThinkingLevel = *update.DefaultThinkingLevel
			}
			if update.SteeringMode != nil {
				settings.SteeringMode = update.SteeringMode.String()
			}
			if update.FollowUpMode != nil {
				settings.FollowUpMode = update.FollowUpMode.String()
			}
			if update.AutoCompactionEnabled != nil {
				settings.Compaction.Enabled = cloneBoolPointer(update.AutoCompactionEnabled)
			}
			if update.AutoRetryEnabled != nil {
				settings.Retry.Enabled = cloneBoolPointer(update.AutoRetryEnabled)
			}
			return nil
		})
		unknown := errors.Is(err, ErrCommitUnknown)
		if err != nil && !unknown {
			return agent.SettingsWriteResult{}, err
		}
		undo := func(undoCtx context.Context) error {
			return runtime.SetGlobalSettings(undoCtx, func(settings *Settings) error {
				if update.DefaultProvider != nil {
					settings.DefaultProvider = previous.DefaultProvider
				}
				if update.DefaultModel != nil {
					settings.DefaultModel = previous.DefaultModel
				}
				if update.DefaultThinkingLevel != nil {
					settings.DefaultThinkingLevel = previous.DefaultThinkingLevel
				}
				if update.SteeringMode != nil {
					settings.SteeringMode = previous.SteeringMode
				}
				if update.FollowUpMode != nil {
					settings.FollowUpMode = previous.FollowUpMode
				}
				if update.AutoCompactionEnabled != nil {
					settings.Compaction.Enabled = cloneBoolPointer(previous.Compaction.Enabled)
				}
				if update.AutoRetryEnabled != nil {
					settings.Retry.Enabled = cloneBoolPointer(previous.Retry.Enabled)
				}
				return nil
			})
		}
		return agent.SettingsWriteResult{Undo: undo, CommitUnknown: unknown}, err
	}
}

func queueModeForAgentTest(value string) agent.QueueMode {
	if value == QueueModeAll {
		return agent.QueueAll
	}
	return agent.QueueOneAtATime
}

func unknownAgentModel(t *testing.T, id string) provider.Model {
	t.Helper()
	value, err := provider.NewModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: id, Name: id, Reasoning: true,
		Input: []provider.InputKind{provider.InputText}, ContextWindow: 16_000, MaxTokens: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func unknownAgentProvider(t *testing.T) provider.Provider {
	t.Helper()
	value, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func unknownAgentManager(t *testing.T) *session.SessionManager {
	t.Helper()
	value, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = value.Close() })
	return value
}

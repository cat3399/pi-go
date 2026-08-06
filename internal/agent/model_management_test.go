package agent_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

func controlModel(t *testing.T, id string, reasoning bool, mapping map[provider.ThinkingLevel]*string) provider.Model {
	t.Helper()
	model, err := newAgentModel(provider.ModelSpec{
		Provider: "scripted", API: "scripted", ID: id, Name: id, Reasoning: reasoning,
		ThinkingLevelMap: mapping, Input: []provider.InputKind{provider.InputText}, ContextWindow: 16_000, MaxTokens: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func controlSession(t *testing.T, initial provider.Model, thinking provider.ThinkingLevel, configure func(*agent.SessionConfig)) (*agent.AgentSession, *session.SessionManager) {
	t.Helper()
	manager := newSessionManager(t)
	config := agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: manager, Model: initial, ThinkingLevel: thinking,
		DefaultThinkingLevel: provider.ThinkingMedium,
		SettlementTimeout:    time.Second,
	}
	if configure != nil {
		configure(&config)
	}
	runtime, err := agent.NewSession(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	return runtime, manager
}

func TestAgentSessionScopedModelsAreCopiedAndCycleFiltersAvailability(t *testing.T) {
	a := controlModel(t, "a", true, nil)
	b := controlModel(t, "b", true, nil)
	c := controlModel(t, "c", true, nil)
	low := provider.ThinkingLow
	scope := []agent.ScopedModel{{Model: a}, {Model: b, ThinkingLevel: &low}, {Model: c}}
	runtime, _ := controlSession(t, a, provider.ThinkingHigh, func(config *agent.SessionConfig) {
		config.ScopedModels = scope
		config.AllModels = []provider.Model{c, b, a}
		config.ModelAvailable = func(_ context.Context, candidate provider.Model) (bool, error) {
			return candidate.ID() != "b", nil
		}
	})
	scope[0].Model = c
	low = provider.ThinkingMax
	snapshot := runtime.ScopedModels()
	if len(snapshot) != 3 || snapshot[0].Model.ID() != "a" || snapshot[1].ThinkingLevel == nil || *snapshot[1].ThinkingLevel != provider.ThinkingLow {
		t.Fatalf("stored scope aliases caller: %#v", snapshot)
	}
	snapshot[0].Model = b
	updatedLow := provider.ThinkingLow
	updated := []agent.ScopedModel{{Model: a}, {Model: b, ThinkingLevel: &updatedLow}, {Model: c}}
	runtime.SetScopedModels(updated)
	updated[0].Model = c
	updatedLow = provider.ThinkingMax
	if got := runtime.ScopedModels(); len(got) != 3 || got[0].Model.ID() != "a" || got[1].ThinkingLevel == nil || *got[1].ThinkingLevel != provider.ThinkingLow {
		t.Fatalf("updated scope aliases caller: %#v", got)
	}
	result, err := runtime.CycleModel(context.Background(), agent.CycleForward)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Model.ID() != "c" || result.ThinkingLevel != provider.ThinkingHigh || !result.IsScoped {
		t.Fatalf("filtered scoped cycle = %#v", result)
	}
	back, err := runtime.CycleModel(context.Background(), agent.CycleBackward)
	if err != nil {
		t.Fatal(err)
	}
	if back == nil || back.Model.ID() != "a" || !back.IsScoped {
		t.Fatalf("backward scoped cycle = %#v", back)
	}
}

func TestAgentSessionScopedThinkingOverrideAndInheritance(t *testing.T) {
	a := controlModel(t, "a", true, nil)
	b := controlModel(t, "b", true, nil)
	c := controlModel(t, "c", true, nil)
	low := provider.ThinkingLow
	runtime, _ := controlSession(t, a, provider.ThinkingHigh, func(config *agent.SessionConfig) {
		config.ScopedModels = []agent.ScopedModel{{Model: a}, {Model: b, ThinkingLevel: &low}, {Model: c}}
	})
	first, err := runtime.CycleModel(context.Background(), agent.CycleForward)
	if err != nil || first == nil || first.Model.ID() != "b" || first.ThinkingLevel != provider.ThinkingLow {
		t.Fatalf("explicit cycle = %#v, %v", first, err)
	}
	second, err := runtime.CycleModel(context.Background(), agent.CycleForward)
	if err != nil || second == nil || second.Model.ID() != "c" || second.ThinkingLevel != provider.ThinkingLow {
		t.Fatalf("inherited cycle = %#v, %v", second, err)
	}
}

func TestAgentSessionAvailableCycleCurrentNotFoundMatchesPiIndexSemantics(t *testing.T) {
	outside := controlModel(t, "outside", true, nil)
	a := controlModel(t, "a", true, nil)
	b := controlModel(t, "b", true, nil)
	c := controlModel(t, "c", true, nil)
	newRuntime := func(t *testing.T) *agent.AgentSession {
		runtime, _ := controlSession(t, outside, provider.ThinkingMedium, func(config *agent.SessionConfig) {
			config.AllModels = []provider.Model{a, b, c}
		})
		return runtime
	}
	forward, err := newRuntime(t).CycleModel(context.Background(), agent.CycleForward)
	if err != nil || forward == nil || forward.Model.ID() != "b" || forward.IsScoped {
		t.Fatalf("forward current-not-found = %#v, %v", forward, err)
	}
	backward, err := newRuntime(t).CycleModel(context.Background(), agent.CycleBackward)
	if err != nil || backward == nil || backward.Model.ID() != "c" || backward.IsScoped {
		t.Fatalf("backward current-not-found = %#v, %v", backward, err)
	}
}

func TestAgentSessionUnscopedCycleRefreshesAvailableModelsAndResolverFailureIsClean(t *testing.T) {
	a := controlModel(t, "a", true, nil)
	b := controlModel(t, "b", true, nil)
	c := controlModel(t, "c", true, nil)
	available := []provider.Model{a, b}
	var resolveErr error
	settingsWrites := 0
	runtime, manager := controlSession(t, a, provider.ThinkingMedium, func(config *agent.SessionConfig) {
		config.AllModels = []provider.Model{a}
		config.ResolveAvailableModels = func(context.Context) ([]provider.Model, error) {
			if resolveErr != nil {
				return nil, resolveErr
			}
			return append([]provider.Model(nil), available...), nil
		}
		config.PersistSettings = func(context.Context, agent.SettingsUpdate) (agent.SettingsWriteResult, error) {
			settingsWrites++
			return agent.SettingsWriteResult{Undo: func(context.Context) error { return nil }}, nil
		}
	})
	first, err := runtime.CycleModel(context.Background(), agent.CycleForward)
	if err != nil || first == nil || first.Model.ID() != "b" {
		t.Fatalf("first dynamic cycle = %#v, %v", first, err)
	}
	if err := runtime.SetModel(a); err != nil {
		t.Fatal(err)
	}
	available = []provider.Model{a, c}
	second, err := runtime.CycleModel(context.Background(), agent.CycleForward)
	if err != nil || second == nil || second.Model.ID() != "c" {
		t.Fatalf("refreshed dynamic cycle = %#v, %v", second, err)
	}
	beforeEntries, beforeSettings := len(manager.Entries()), settingsWrites
	resolveErr = errors.New("availability refresh failed")
	if result, err := runtime.CycleModel(context.Background(), agent.CycleForward); result != nil || !errors.Is(err, resolveErr) {
		t.Fatalf("failed resolver cycle = %#v, %v", result, err)
	}
	selected, _ := runtime.SelectedModel()
	if !selected.Equal(c) || len(manager.Entries()) != beforeEntries || settingsWrites != beforeSettings {
		t.Fatalf("resolver failure leaked state: model=%s entries=%d settings=%d", selected.ID(), len(manager.Entries()), settingsWrites)
	}
}

func TestAgentSessionNonReasoningSwitchRestoresThinkingPreference(t *testing.T) {
	plain := controlModel(t, "plain", false, nil)
	reasoning := controlModel(t, "reasoning", true, nil)
	runtime, _ := controlSession(t, plain, provider.ThinkingOff, func(config *agent.SessionConfig) {
		config.AllModels = []provider.Model{plain, reasoning}
		config.DefaultThinkingLevel = provider.ThinkingHigh
	})
	result, err := runtime.CycleModel(context.Background(), agent.CycleForward)
	if err != nil || result == nil || result.Model.ID() != "reasoning" || result.ThinkingLevel != provider.ThinkingHigh {
		t.Fatalf("restored preference = %#v, %v", result, err)
	}
	if err := runtime.SetModel(plain); err != nil {
		t.Fatal(err)
	}
	if runtime.ThinkingLevel() != provider.ThinkingOff {
		t.Fatalf("plain thinking = %q", runtime.ThinkingLevel())
	}
	if err := runtime.SetModel(reasoning); err != nil {
		t.Fatal(err)
	}
	if runtime.ThinkingLevel() != provider.ThinkingHigh {
		t.Fatalf("direct switch did not restore preference: %q", runtime.ThinkingLevel())
	}
}

func TestAgentSessionCycleAbsentForAtMostOneAvailableModel(t *testing.T) {
	a := controlModel(t, "a", true, nil)
	b := controlModel(t, "b", true, nil)
	runtime, manager := controlSession(t, a, provider.ThinkingMedium, func(config *agent.SessionConfig) {
		config.AllModels = []provider.Model{a, b}
		config.ModelAvailable = func(_ context.Context, candidate provider.Model) (bool, error) { return candidate.ID() == "a", nil }
	})
	before := len(manager.Entries())
	result, err := runtime.CycleModel(context.Background(), agent.CycleForward)
	if err != nil || result != nil || len(manager.Entries()) != before {
		t.Fatalf("single available cycle = %#v, %v entries=%d", result, err, len(manager.Entries()))
	}
}

func TestAgentSessionThinkingLevelsClampAndCycle(t *testing.T) {
	disabled := (*string)(nil)
	reasoning := controlModel(t, "reasoning", true, map[provider.ThinkingLevel]*string{provider.ThinkingLow: disabled})
	runtime, _ := controlSession(t, reasoning, provider.ThinkingOff, nil)
	want := []provider.ThinkingLevel{provider.ThinkingOff, provider.ThinkingMinimal, provider.ThinkingMedium, provider.ThinkingHigh}
	if got := runtime.AvailableThinkingLevels(); !reflect.DeepEqual(got, want) || !runtime.SupportsThinking() {
		t.Fatalf("thinking capabilities = %v supports=%t", got, runtime.SupportsThinking())
	}
	if err := runtime.SetThinkingLevel(provider.ThinkingLow); err != nil {
		t.Fatal(err)
	}
	if runtime.ThinkingLevel() != provider.ThinkingMedium {
		t.Fatalf("clamped low = %q", runtime.ThinkingLevel())
	}
	next, err := runtime.CycleThinkingLevel()
	if err != nil || next == nil || *next != provider.ThinkingHigh {
		t.Fatalf("cycle thinking = %v, %v", next, err)
	}
	plain := controlModel(t, "plain", false, nil)
	plainRuntime, _ := controlSession(t, plain, provider.ThinkingOff, nil)
	if plainRuntime.SupportsThinking() || !reflect.DeepEqual(plainRuntime.AvailableThinkingLevels(), []provider.ThinkingLevel{provider.ThinkingOff}) {
		t.Fatalf("plain capabilities = %v supports=%t", plainRuntime.AvailableThinkingLevels(), plainRuntime.SupportsThinking())
	}
	if next, err := plainRuntime.CycleThinkingLevel(); err != nil || next != nil {
		t.Fatalf("plain cycle = %v, %v", next, err)
	}
}

func TestAgentSessionCycleThinkingWithNoSupportedLevelsIsSafeAndOff(t *testing.T) {
	disabled := (*string)(nil)
	model := controlModel(t, "no-thinking-levels", true, map[provider.ThinkingLevel]*string{
		provider.ThinkingOff: disabled, provider.ThinkingMinimal: disabled,
		provider.ThinkingLow: disabled, provider.ThinkingMedium: disabled, provider.ThinkingHigh: disabled,
	})
	runtime, _ := controlSession(t, model, provider.ThinkingHigh, nil)
	if levels := runtime.AvailableThinkingLevels(); len(levels) != 0 || runtime.ThinkingLevel() != provider.ThinkingOff {
		t.Fatalf("empty-level model = levels %v thinking %q", levels, runtime.ThinkingLevel())
	}
	if next, err := runtime.CycleThinkingLevel(); err != nil || next != nil || runtime.ThinkingLevel() != provider.ThinkingOff {
		t.Fatalf("empty-level cycle = %v, %v thinking=%q", next, err, runtime.ThinkingLevel())
	}
}

func TestAgentSessionRejectsInvalidNonEmptyDefaultThinking(t *testing.T) {
	_, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t),
		Model: controlModel(t, "reasoning", true, nil), ThinkingLevel: provider.ThinkingOff,
		DefaultThinkingLevel: provider.ThinkingLevel("invalid"),
	})
	if !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("NewSession error = %v", err)
	}
}

func TestAgentSessionModelLessThinkingSurfaceMatchesPi(t *testing.T) {
	runtime, err := agent.NewSession(agent.SessionConfig{
		Provider: newScriptedProvider(t), SessionManager: newSessionManager(t),
		DefaultThinkingLevel: provider.ThinkingMedium, SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	want := []provider.ThinkingLevel{
		provider.ThinkingOff, provider.ThinkingMinimal, provider.ThinkingLow,
		provider.ThinkingMedium, provider.ThinkingHigh,
	}
	if got := runtime.AvailableThinkingLevels(); !reflect.DeepEqual(got, want) || runtime.SupportsThinking() {
		t.Fatalf("model-less capabilities = %v supports=%t", got, runtime.SupportsThinking())
	}
	if err := runtime.SetThinkingLevel(provider.ThinkingHigh); err != nil {
		t.Fatal(err)
	}
	if runtime.ThinkingLevel() != provider.ThinkingHigh {
		t.Fatalf("model-less thinking = %q", runtime.ThinkingLevel())
	}
	if err := runtime.SetThinkingLevel(provider.ThinkingXHigh); err != nil || runtime.ThinkingLevel() != provider.ThinkingOff {
		t.Fatalf("model-less xhigh clamp = %q, %v", runtime.ThinkingLevel(), err)
	}
	if err := runtime.SetThinkingLevel(provider.ThinkingHigh); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetThinkingLevel(provider.ThinkingMax); err != nil || runtime.ThinkingLevel() != provider.ThinkingOff {
		t.Fatalf("model-less max clamp = %q, %v", runtime.ThinkingLevel(), err)
	}
}

func TestAgentSessionCycleHookSourceAndSettingsFailureAreAtomic(t *testing.T) {
	a := controlModel(t, "a", true, nil)
	b := controlModel(t, "b", true, nil)
	settingsErr := errors.New("settings unavailable")
	var hooks []agent.ModelSelectEvent
	runtime, manager := controlSession(t, a, provider.ThinkingMedium, func(config *agent.SessionConfig) {
		config.AllModels = []provider.Model{a, b}
		config.PersistSettings = func(context.Context, agent.SettingsUpdate) (agent.SettingsWriteResult, error) {
			return agent.SettingsWriteResult{}, settingsErr
		}
		config.Hooks.ModelSelect = func(_ context.Context, event agent.ModelSelectEvent) error {
			hooks = append(hooks, event)
			return nil
		}
	})
	before := len(manager.Entries())
	if result, err := runtime.CycleModel(context.Background(), agent.CycleForward); !errors.Is(err, settingsErr) || result != nil {
		t.Fatalf("failed cycle = %#v, %v", result, err)
	}
	selected, _ := runtime.SelectedModel()
	if !selected.Equal(a) || runtime.ThinkingLevel() != provider.ThinkingMedium || len(manager.Entries()) != before || len(hooks) != 0 {
		t.Fatalf("failed cycle leaked state: model=%s thinking=%s entries=%d hooks=%d", selected.ID(), runtime.ThinkingLevel(), len(manager.Entries()), len(hooks))
	}

	runtime, _ = controlSession(t, a, provider.ThinkingMedium, func(config *agent.SessionConfig) {
		config.AllModels = []provider.Model{a, b}
		config.Hooks.ModelSelect = func(_ context.Context, event agent.ModelSelectEvent) error {
			hooks = append(hooks, event)
			return nil
		}
	})
	result, err := runtime.CycleModel(context.Background(), agent.CycleForward)
	if err != nil || result == nil || len(hooks) != 1 || hooks[0].Source != agent.ModelSelectCycle {
		t.Fatalf("successful cycle hook = %#v hooks=%#v err=%v", result, hooks, err)
	}
}

func TestAgentSessionTranscriptFailureRollsBackSettingsAndState(t *testing.T) {
	a := controlModel(t, "a", true, nil)
	b := controlModel(t, "b", true, nil)
	defaults := struct {
		provider string
		model    string
		thinking provider.ThinkingLevel
	}{provider: "scripted", model: "a", thinking: provider.ThinkingHigh}
	updates := 0
	runtime, manager := controlSession(t, a, provider.ThinkingHigh, func(config *agent.SessionConfig) {
		config.DefaultThinkingLevel = provider.ThinkingHigh
		config.PersistSettings = func(_ context.Context, update agent.SettingsUpdate) (agent.SettingsWriteResult, error) {
			updates++
			before := defaults
			if update.DefaultProvider != nil {
				defaults.provider = *update.DefaultProvider
			}
			if update.DefaultModel != nil {
				defaults.model = *update.DefaultModel
			}
			if update.DefaultThinkingLevel != nil {
				defaults.thinking = *update.DefaultThinkingLevel
			}
			undo := func(context.Context) error {
				updates++
				defaults = before
				return nil
			}
			return agent.SettingsWriteResult{Undo: undo}, nil
		}
	})
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SetModel(b); !errors.Is(err, agent.ErrTranscriptCommit) {
		t.Fatalf("SetModel error = %v", err)
	}
	selected, _ := runtime.SelectedModel()
	if !selected.Equal(a) || runtime.ThinkingLevel() != provider.ThinkingHigh {
		t.Fatalf("state leaked after transcript failure: %s/%s", selected.ID(), runtime.ThinkingLevel())
	}
	if updates != 2 || defaults.provider != "scripted" || defaults.model != "a" || defaults.thinking != provider.ThinkingHigh {
		t.Fatalf("settings rollback = updates %d defaults %#v", updates, defaults)
	}
}

package agentruntime_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/provider"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
)

type factorySummarizerFunc func(context.Context, session.SummaryInput) (session.SummaryOutput, error)

func (f factorySummarizerFunc) Summarize(ctx context.Context, input session.SummaryInput) (session.SummaryOutput, error) {
	return f(ctx, input)
}

func factoryCatalogModel(providerID, id string) model.Model {
	return model.Model{
		Provider: providerID, API: "scripted", ID: id, Name: id, Reasoning: true,
		Input: []provider.InputKind{provider.InputText}, ContextWindow: 16_000, MaxTokens: 1_000,
	}
}

func factoryProvider(t *testing.T) provider.Provider {
	t.Helper()
	value, err := provider.NewScriptedProvider(provider.ScriptedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func factoryManager(t *testing.T) *session.SessionManager {
	t.Helper()
	value, err := session.InMemorySessionManager(t.TempDir(), session.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = value.Close() })
	return value
}

func availableFactoryModels(auth map[string]bool) model.Availability {
	return model.Availability{
		HasConfiguredAuth: func(providerID string) bool { return auth[providerID] },
		SupportsRoute:     func(model.Model) bool { return true },
	}
}

func createFactorySession(t *testing.T, manager *session.SessionManager, options agentruntime.SessionFactoryOptions) agentruntime.CreateResult {
	t.Helper()
	options.Services = &agentruntime.Services{CWD: manager.Cwd(), AgentDir: t.TempDir()}
	options.Provider = factoryProvider(t)
	options.SessionManager = manager
	result, err := agentruntime.CreateAgentSession(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = result.Session.Close(context.Background()) })
	return result
}

func selectedFactoryIdentity(t *testing.T, result agentruntime.CreateResult) string {
	t.Helper()
	selected, ok := result.Session.SelectedModel()
	if !ok {
		return ""
	}
	return selected.Provider() + "/" + selected.ID()
}

func TestCreateAgentSessionBootstrapSelectionMatrix(t *testing.T) {
	t.Run("explicit model and thinking take priority without eager auth", func(t *testing.T) {
		manager := factoryManager(t)
		explicit := factoryCatalogModel("explicit", "chosen")
		fallback := factoryCatalogModel("openai", "gpt-5.5")
		high := provider.ThinkingHigh
		result := createFactorySession(t, manager, agentruntime.SessionFactoryOptions{
			AllModels: []model.Model{fallback}, Availability: availableFactoryModels(map[string]bool{}),
			ExplicitModel: &explicit, ExplicitThinkingLevel: &high,
			ScopedModels: []model.ScopedModel{{Model: fallback}},
			Settings:     model.Settings{DefaultProvider: fallback.Provider, DefaultModel: fallback.ID, DefaultThinkingLevel: provider.ThinkingLow},
		})
		if got := selectedFactoryIdentity(t, result); got != "explicit/chosen" || result.Session.ThinkingLevel() != provider.ThinkingHigh {
			t.Fatalf("selection = %q thinking=%q", got, result.Session.ThinkingLevel())
		}
		entries := manager.Entries()
		if len(entries) != 2 {
			t.Fatalf("new session entries = %d, want model + thinking", len(entries))
		}
		modelEntry, modelOK := entries[0].Payload().(session.ModelChangePayload)
		thinkingEntry, thinkingOK := entries[1].Payload().(session.ThinkingLevelChangePayload)
		if !modelOK || modelEntry.Provider != "explicit" || modelEntry.ModelID != "chosen" || !thinkingOK || thinkingEntry.ThinkingLevel != "high" {
			t.Fatalf("initial entries = %#v / %#v", entries[0].Payload(), entries[1].Payload())
		}
	})

	t.Run("new session scope prefers settings default", func(t *testing.T) {
		manager := factoryManager(t)
		first := factoryCatalogModel("scope", "first")
		preferred := factoryCatalogModel("scope", "preferred")
		scopeThinking := provider.ThinkingHigh
		result := createFactorySession(t, manager, agentruntime.SessionFactoryOptions{
			AllModels: []model.Model{first, preferred}, Availability: availableFactoryModels(map[string]bool{"scope": true}),
			ScopedModels: []model.ScopedModel{{Model: first}, {Model: preferred, ThinkingLevel: &scopeThinking}},
			Settings:     model.Settings{DefaultProvider: "scope", DefaultModel: "preferred", DefaultThinkingLevel: provider.ThinkingLow},
		})
		if got := selectedFactoryIdentity(t, result); got != "scope/preferred" || result.Session.ThinkingLevel() != provider.ThinkingHigh {
			t.Fatalf("selection = %q thinking=%q", got, result.Session.ThinkingLevel())
		}
	})

	t.Run("explicit thinking overrides selected scope suffix", func(t *testing.T) {
		manager := factoryManager(t)
		scoped := factoryCatalogModel("scope", "selected")
		scopeThinking := provider.ThinkingLow
		explicitThinking := provider.ThinkingHigh
		result := createFactorySession(t, manager, agentruntime.SessionFactoryOptions{
			AllModels: []model.Model{scoped}, Availability: availableFactoryModels(map[string]bool{"scope": true}),
			ScopedModels:          []model.ScopedModel{{Model: scoped, ThinkingLevel: &scopeThinking}},
			ExplicitThinkingLevel: &explicitThinking,
			Settings:              model.Settings{DefaultThinkingLevel: provider.ThinkingMedium},
		})
		if got := selectedFactoryIdentity(t, result); got != "scope/selected" || result.Session.ThinkingLevel() != provider.ThinkingHigh {
			t.Fatalf("selection = %q thinking=%q", got, result.Session.ThinkingLevel())
		}
	})

	t.Run("fallback uses ordered provider default before catalog order", func(t *testing.T) {
		manager := factoryManager(t)
		catalogFirst := factoryCatalogModel("custom", "first")
		providerDefault := factoryCatalogModel("openai", "gpt-5.5")
		result := createFactorySession(t, manager, agentruntime.SessionFactoryOptions{
			AllModels:    []model.Model{catalogFirst, providerDefault},
			Availability: availableFactoryModels(map[string]bool{"custom": true, "openai": true}),
		})
		if got := selectedFactoryIdentity(t, result); got != "openai/gpt-5.5" {
			t.Fatalf("ordered provider fallback = %q", got)
		}
	})

	t.Run("existing session restores model and branch thinking", func(t *testing.T) {
		manager := factoryManager(t)
		if _, err := manager.AppendModelChange(context.Background(), "saved", "model"); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.AppendThinkingLevelChange(context.Background(), "high"); err != nil {
			t.Fatal(err)
		}
		appendFactoryUser(t, manager)
		before := len(manager.Entries())
		saved := factoryCatalogModel("saved", "model")
		result := createFactorySession(t, manager, agentruntime.SessionFactoryOptions{
			AllModels: []model.Model{saved}, Availability: availableFactoryModels(map[string]bool{"saved": true}),
			Settings: model.Settings{DefaultThinkingLevel: provider.ThinkingLow},
		})
		if got := selectedFactoryIdentity(t, result); got != "saved/model" || result.Session.ThinkingLevel() != provider.ThinkingHigh {
			t.Fatalf("restored = %q thinking=%q", got, result.Session.ThinkingLevel())
		}
		if len(manager.Entries()) != before {
			t.Fatalf("existing complete history gained bootstrap entries: %d -> %d", before, len(manager.Entries()))
		}
	})

	t.Run("existing fallback ignores scope and uses settings default", func(t *testing.T) {
		manager := factoryManager(t)
		if _, err := manager.AppendModelChange(context.Background(), "gone", "old"); err != nil {
			t.Fatal(err)
		}
		appendFactoryUser(t, manager)
		scoped := factoryCatalogModel("scope", "ignored")
		settings := factoryCatalogModel("settings", "default")
		result := createFactorySession(t, manager, agentruntime.SessionFactoryOptions{
			AllModels: []model.Model{scoped, settings}, Availability: availableFactoryModels(map[string]bool{"scope": true, "settings": true}),
			ScopedModels: []model.ScopedModel{{Model: scoped}},
			Settings:     model.Settings{DefaultProvider: "settings", DefaultModel: "default", DefaultThinkingLevel: provider.ThinkingLow},
		})
		if got := selectedFactoryIdentity(t, result); got != "settings/default" {
			t.Fatalf("fallback selection = %q", got)
		}
		if result.ModelFallbackMessage == nil || *result.ModelFallbackMessage != "Could not restore model gone/old. Using settings/default" {
			t.Fatalf("fallback message = %#v", result.ModelFallbackMessage)
		}
		entries := manager.Entries()
		if _, ok := entries[len(entries)-1].Payload().(session.ThinkingLevelChangePayload); !ok {
			t.Fatalf("missing-history bootstrap entry = %#v", entries[len(entries)-1].Payload())
		}
		for _, entry := range entries[2:] {
			if _, ok := entry.Payload().(session.ModelChangePayload); ok {
				t.Fatal("fallback model was persisted into existing history")
			}
		}
	})
}

func TestCreateAgentSessionNoAvailableModelIsRealModelLessSession(t *testing.T) {
	manager := factoryManager(t)
	staleBaseModel, err := factoryCatalogModel("stale", "base-config").Ref()
	if err != nil {
		t.Fatal(err)
	}
	result := createFactorySession(t, manager, agentruntime.SessionFactoryOptions{
		Availability: model.Availability{HasConfiguredAuth: func(string) bool { return false }, SupportsRoute: func(model.Model) bool { return true }},
		Settings:     model.Settings{DefaultThinkingLevel: provider.ThinkingHigh},
		DocsDir:      "/installed/docs",
		BaseConfig:   agent.SessionConfig{Model: staleBaseModel},
	})
	if result.Session.HasModel() || result.Session.ThinkingLevel() != provider.ThinkingOff {
		t.Fatalf("model-less result = hasModel %t thinking %q", result.Session.HasModel(), result.Session.ThinkingLevel())
	}
	if result.ModelFallbackMessage == nil || !strings.HasPrefix(*result.ModelFallbackMessage, "No models available. Use /login") || !strings.Contains(*result.ModelFallbackMessage, "/installed/docs/providers.md") {
		t.Fatalf("no-model guidance = %#v", result.ModelFallbackMessage)
	}
	entries := manager.Entries()
	if len(entries) != 1 {
		t.Fatalf("model-less bootstrap entries = %d, want thinking only", len(entries))
	}
	_, err = result.Session.Prompt(context.Background(), "blocked")
	wantPromptError := "No model selected.\n\nUse /login to log into a provider via OAuth or API key. See:\n  /installed/docs/providers.md\n  /installed/docs/models.md\n\nThen use /model to select a model."
	if !errors.Is(err, agent.ErrNoModelSelected) || err.Error() != wantPromptError {
		t.Fatalf("Prompt error = %q, want %q with sentinel", err, wantPromptError)
	}
	_, err = result.Session.Continue(context.Background())
	if !errors.Is(err, agent.ErrNoModelSelected) || err.Error() != wantPromptError {
		t.Fatalf("Continue error = %q, want %q with sentinel", err, wantPromptError)
	}
}

func TestModelLessProductSessionCompactSettlesBeforeHooksOrPersistence(t *testing.T) {
	manager := factoryManager(t)
	summarizerCalls := 0
	beforeCompactCalls := 0
	result := createFactorySession(t, manager, agentruntime.SessionFactoryOptions{
		Availability: model.Availability{HasConfiguredAuth: func(string) bool { return false }, SupportsRoute: func(model.Model) bool { return true }},
		DocsDir:      "/installed/docs",
		BaseConfig: agent.SessionConfig{
			Summarizer: factorySummarizerFunc(func(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
				summarizerCalls++
				return session.SummaryOutput{Text: "must not run"}, nil
			}),
			Hooks: agent.Hooks{SessionBeforeCompact: func(context.Context, agent.SessionBeforeCompactEvent) (agent.SessionBeforeCompactResult, error) {
				beforeCompactCalls++
				return agent.SessionBeforeCompactResult{}, nil
			}},
		},
	})
	beforeEntries := len(manager.Entries())
	var lifecycle []agent.SessionEvent
	result.Session.Subscribe(func(_ context.Context, event agent.SessionEvent) {
		if event.Type() == agent.CompactionStartEventType || event.Type() == agent.CompactionEndEventType {
			lifecycle = append(lifecycle, event)
		}
	})
	_, err := result.Session.Compact(context.Background(), "must not summarize")
	wantError := "No model selected.\n\nUse /login to log into a provider via OAuth or API key. See:\n  /installed/docs/providers.md\n  /installed/docs/models.md\n\nThen use /model to select a model."
	if !errors.Is(err, agent.ErrNoModelSelected) || err.Error() != wantError {
		t.Fatalf("Compact error = %q, want %q with sentinel", err, wantError)
	}
	if summarizerCalls != 0 || beforeCompactCalls != 0 || len(manager.Entries()) != beforeEntries {
		t.Fatalf("model-less compact side effects: summarizer=%d beforeHook=%d entries=%d->%d", summarizerCalls, beforeCompactCalls, beforeEntries, len(manager.Entries()))
	}
	if len(lifecycle) != 2 || lifecycle[0].Type() != agent.CompactionStartEventType || lifecycle[1].Type() != agent.CompactionEndEventType {
		t.Fatalf("compact lifecycle = %#v", lifecycle)
	}
	ended, ok := lifecycle[1].(agent.CompactionEndEvent)
	if !ok || ended.Result != nil || ended.Aborted || ended.ErrorMessage != "Compaction failed: "+wantError ||
		ended.Err == nil || ended.Err.Error() != "Compaction failed: "+wantError || !errors.Is(ended.Err, agent.ErrNoModelSelected) {
		t.Fatalf("compaction end = %#v", lifecycle[1])
	}
	if result.Session.State().Active.Phase() != agent.PhaseIdle {
		t.Fatalf("session phase after compact = %s", result.Session.State().Active.Phase())
	}
}

func TestCreateAgentSessionExplicitModelRequiresRegisteredRoute(t *testing.T) {
	manager := factoryManager(t)
	explicit := factoryCatalogModel("explicit", "unsupported")
	_, err := agentruntime.CreateAgentSession(context.Background(), agentruntime.SessionFactoryOptions{
		Services: &agentruntime.Services{CWD: manager.Cwd()}, Provider: factoryProvider(t), SessionManager: manager,
		ExplicitModel: &explicit,
		Availability:  model.Availability{HasConfiguredAuth: func(string) bool { return true }, SupportsRoute: func(model.Model) bool { return false }},
	})
	if err == nil || !strings.Contains(err.Error(), "not supported by a registered provider route") {
		t.Fatalf("CreateAgentSession error = %v", err)
	}
}

func TestCreateAgentSessionThinkingEntryUsesOnlyActiveBranch(t *testing.T) {
	manager := factoryManager(t)
	rootMessage, err := llm.NewUserTextMessage("root", time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	root, err := manager.AppendLLMMessage(context.Background(), rootMessage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendThinkingLevelChange(context.Background(), "high"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Branch(root.ID()); err != nil {
		t.Fatal(err)
	}
	currentMessage, err := llm.NewUserTextMessage("current branch", time.UnixMilli(2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), currentMessage); err != nil {
		t.Fatal(err)
	}

	explicit := factoryCatalogModel("explicit", "active")
	before := len(manager.Entries())
	result := createFactorySession(t, manager, agentruntime.SessionFactoryOptions{
		ExplicitModel: &explicit,
		Availability:  model.Availability{HasConfiguredAuth: func(string) bool { return false }, SupportsRoute: func(model.Model) bool { return true }},
		Settings:      model.Settings{DefaultThinkingLevel: provider.ThinkingLow},
	})
	if result.Session.ThinkingLevel() != provider.ThinkingLow {
		t.Fatalf("thinking = %q, want active-branch fallback low", result.Session.ThinkingLevel())
	}
	entries := manager.Entries()
	if len(entries) != before+1 {
		t.Fatalf("entries = %d, want one active-branch thinking append after %d", len(entries), before)
	}
	change, ok := entries[len(entries)-1].Payload().(session.ThinkingLevelChangePayload)
	if !ok || change.ThinkingLevel != "low" {
		t.Fatalf("active-branch append = %#v", entries[len(entries)-1].Payload())
	}
}

func TestCreateAgentSessionMetadataOnlyBranchIsStillNewSession(t *testing.T) {
	manager := factoryManager(t)
	if _, err := manager.AppendThinkingLevelChange(context.Background(), "high"); err != nil {
		t.Fatal(err)
	}
	scoped := factoryCatalogModel("scope", "new")
	scopeThinking := provider.ThinkingLow
	result := createFactorySession(t, manager, agentruntime.SessionFactoryOptions{
		AllModels: []model.Model{scoped}, Availability: availableFactoryModels(map[string]bool{"scope": true}),
		ScopedModels: []model.ScopedModel{{Model: scoped, ThinkingLevel: &scopeThinking}},
		Settings:     model.Settings{DefaultThinkingLevel: provider.ThinkingMedium},
	})
	if got := selectedFactoryIdentity(t, result); got != "scope/new" || result.Session.ThinkingLevel() != provider.ThinkingLow {
		t.Fatalf("metadata-only bootstrap = %q thinking=%q", got, result.Session.ThinkingLevel())
	}
	entries := manager.Entries()
	if len(entries) != 3 {
		t.Fatalf("metadata-only entries = %d, want old thinking + new model/thinking", len(entries))
	}
	if _, ok := entries[1].Payload().(session.ModelChangePayload); !ok {
		t.Fatalf("metadata-only session was treated as existing: %#v", entries[1].Payload())
	}
}

func appendFactoryUser(t *testing.T, manager *session.SessionManager) {
	t.Helper()
	message, err := llm.NewUserTextMessage("existing", time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
}

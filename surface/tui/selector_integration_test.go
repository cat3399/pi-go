package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/provider"
)

type selectorTestAPI struct {
	modelTestAPI
	models       application.ModelsSnapshot
	modelErr     error
	sessions     []application.SessionInfo
	sessionErr   error
	tools        []application.ToolInfo
	commands     []application.Command
	openedState  application.State
	openSnapshot application.SessionSnapshot
	providers    []application.ProviderAuthInfo
	providerErr  error
	savedID      string
	savedKey     string
	deletedID    string
	deletedType  string
	authErr      error
	oauthID      string
	oauthErr     error
}

func (a *selectorTestAPI) ListModelProviders(context.Context, string) ([]application.ProviderAuthInfo, error) {
	return append([]application.ProviderAuthInfo(nil), a.providers...), a.providerErr
}

func (a *selectorTestAPI) SetProviderAPIKey(_ context.Context, providerID, apiKey string) error {
	a.savedID, a.savedKey = providerID, apiKey
	return a.authErr
}

func (a *selectorTestAPI) DeleteProviderCredential(_ context.Context, providerID, credentialType string) error {
	a.deletedID, a.deletedType = providerID, credentialType
	return a.authErr
}

func (a *selectorTestAPI) StartProviderOAuth(_ context.Context, providerID string) (*application.ProviderOAuthLogin, error) {
	a.oauthID = providerID
	return nil, a.oauthErr
}

func (a *selectorTestAPI) ListModels(context.Context, string) (application.ModelsSnapshot, error) {
	return a.models, a.modelErr
}

func (a *selectorTestAPI) ListSessions() ([]application.SessionInfo, error) {
	return append([]application.SessionInfo(nil), a.sessions...), a.sessionErr
}

func (a *selectorTestAPI) Dispatch(_ context.Context, sessionID string, command application.Command) (application.CommandResult, error) {
	a.commands = append(a.commands, command)
	switch command.(type) {
	case application.GetToolsCommand:
		return application.GetToolsResult{Tools: append([]application.ToolInfo(nil), a.tools...)}, nil
	case application.SetToolsCommand:
		return application.SetToolsResult{}, nil
	case application.SetThinkingLevelCommand:
		return application.SetThinkingLevelResult{}, nil
	case application.SetModelCommand:
		return application.SetModelResult{}, nil
	case application.GetStateCommand:
		state := a.openedState
		if state.SessionID == "" {
			state = application.State{SessionID: sessionID, CWD: "/workspace"}
		}
		return application.GetStateResult{State: state}, nil
	default:
		return nil, application.ErrInvalidCommand
	}
}

func (a *selectorTestAPI) SnapshotSession(sessionID, _ string) (application.SessionSnapshot, error) {
	if a.openSnapshot.SessionID != "" {
		return a.openSnapshot, nil
	}
	state := application.State{SessionID: sessionID, CWD: "/workspace"}
	return application.SessionSnapshot{
		SessionID: sessionID, Info: application.SessionInfo{ID: sessionID, CWD: "/workspace"}, LiveState: &state,
	}, nil
}

func selectorModelsFixture() application.ModelsSnapshot {
	return application.ModelsSnapshot{Models: []application.AvailableModel{
		{Provider: "deepseek", ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash"},
		{Provider: "openai", ID: "gpt-5", Name: "GPT-5"},
		{Provider: "openrouter", ID: "anthropic/claude-sonnet", Name: "Claude Sonnet"},
	}}
}

func selectorCommandAt(t *testing.T, command tea.Cmd, index int) tea.Cmd {
	t.Helper()
	if command == nil {
		t.Fatal("expected batched command, got nil")
	}
	message := command()
	batch, ok := message.(tea.BatchMsg)
	if !ok {
		return func() tea.Msg { return message }
	}
	if index < 0 || index >= len(batch) {
		t.Fatalf("batch length = %d, index = %d", len(batch), index)
	}
	return batch[index]
}

func TestModelSelectorLoadsFiltersAndDispatchesSelection(t *testing.T) {
	api := &selectorTestAPI{models: selectorModelsFixture()}
	model := newModelWithAPIForTest(t, api)
	open := model.openModelSelector("")
	loaded, ok := selectorCommandAt(t, open, 1)().(selectorLoadedMsg)
	if !ok {
		t.Fatalf("load message has unexpected type")
	}
	if command := model.handleSelectorLoaded(loaded); command != nil {
		t.Fatal("empty-query selector selected a model automatically")
	}
	if model.selector == nil || len(model.selector.items) != 3 {
		t.Fatalf("selector = %#v", model.selector)
	}

	model.selector.input.SetValue("openrouter sonnet")
	model.selector.refilter(true)
	selected, ok := model.selector.Selected()
	if !ok || selected.Key != "openrouter/anthropic/claude-sonnet" {
		t.Fatalf("filtered model = %#v, %t", selected, ok)
	}
	dispatch := model.applySelectorSelection()
	finished, ok := selectorCommandAt(t, dispatch, 1)().(commandFinishedMsg)
	if !ok || finished.err != nil {
		t.Fatalf("dispatch result = %#v", finished)
	}
	selectedCommand, ok := api.commands[len(api.commands)-1].(application.SetModelCommand)
	if !ok || selectedCommand.Provider != "openrouter" || selectedCommand.ModelID != "anthropic/claude-sonnet" {
		t.Fatalf("selected command = %#v", api.commands[len(api.commands)-1])
	}
	if model.selector != nil {
		t.Fatal("selector remained open after model selection")
	}
}

func TestLoginSelectorStoresMaskedAPIKeyWithoutEchoingIt(t *testing.T) {
	api := &selectorTestAPI{providers: []application.ProviderAuthInfo{{
		ID: "deepseek", Name: "DeepSeek", SupportsAPIKey: true, ModelCount: 2,
	}}}
	model := newModelWithAPIForTest(t, api)
	open := model.openLoginSelector("deepseek")
	loaded := selectorCommandAt(t, open, 1)().(selectorLoadedMsg)
	model.handleSelectorLoaded(loaded)
	if model.selector == nil || model.selector.kind != selectorLoginAPIKey ||
		model.selector.input.EchoMode != textinput.EchoPassword {
		t.Fatalf("API key selector = %#v", model.selector)
	}
	model.selector.input.SetValue("secret-test-key")
	save := model.applySelectorSelection()
	message, ok := selectorCommandAt(t, save, 1)().(providerAuthMutationMsg)
	if !ok || message.err != nil || api.savedID != "deepseek" || api.savedKey != "secret-test-key" {
		t.Fatalf("API key save = %#v / %q / %q", message, api.savedID, api.savedKey)
	}
	if strings.Contains(fmt.Sprintf("%#v", message), "secret-test-key") {
		t.Fatalf("API key escaped into completion message: %#v", message)
	}
	if model.selector != nil {
		t.Fatal("API key selector remained open after save")
	}
}

func TestSessionSelectorFiltersSortsAndTogglesFullPaths(t *testing.T) {
	now := time.Now()
	sessions := []application.SessionInfo{
		{ID: "session-1", CWD: "/workspace/current", FirstMessage: "unnamed", MessageCount: 2, Modified: now},
		{ID: "session-2", CWD: "/workspace/alpha", Name: "Alpha", MessageCount: 8, Modified: now.Add(-time.Hour)},
		{ID: "session-3", CWD: "/workspace/beta", Name: "Beta", MessageCount: 4, Modified: now.Add(time.Hour)},
	}
	model := newModelForTest(t)
	model.sessionNamedOnly = true
	model.sessionSortMode = "messages"
	model.sessionShowPath = true
	items := model.sessionSelectorItems(sessions)
	if len(items) != 2 || items[0].Key != "session-2" || items[0].Badge != "/workspace/alpha" {
		t.Fatalf("filtered session items = %#v", items)
	}

	api := &selectorTestAPI{sessions: sessions}
	model = newModelWithAPIForTest(t, api)
	open := model.openSessionSelector("")
	model.handleSelectorLoaded(selectorCommandAt(t, open, 1)().(selectorLoadedMsg))
	command := model.handleSelectorKey(tea.KeyPressMsg(tea.Key{Code: 'n', Mod: tea.ModCtrl}))
	if command == nil || !model.sessionNamedOnly {
		t.Fatalf("named filter toggle = %t, command=%v", model.sessionNamedOnly, command != nil)
	}
	loaded := command().(selectorLoadedMsg)
	model.handleSelectorLoaded(loaded)
	if len(model.selector.items) != 2 || !strings.Contains(model.selector.notice, "named only") {
		t.Fatalf("named selector = %#v", model.selector)
	}
}

func TestLoginSelectorOffersOAuthAndAPIKeyMethods(t *testing.T) {
	api := &selectorTestAPI{providers: []application.ProviderAuthInfo{{
		ID: "openai-codex", Name: "OpenAI Codex", SupportsAPIKey: true, SupportsOAuth: true,
		OAuthName: "ChatGPT Plus/Pro",
	}}}
	model := newModelWithAPIForTest(t, api)
	open := model.openLoginSelector("")
	loaded := selectorCommandAt(t, open, 1)().(selectorLoadedMsg)
	model.handleSelectorLoaded(loaded)
	model.applySelectorSelection()
	if model.selector == nil || model.selector.kind != selectorLoginMethod || len(model.selector.items) != 2 {
		t.Fatalf("login method selector = %#v", model.selector)
	}
	if !model.selector.SelectKey("api_key") {
		t.Fatal("API key login method is missing")
	}
	model.applySelectorSelection()
	if model.selector == nil || model.selector.kind != selectorLoginAPIKey {
		t.Fatalf("selected login method opened %#v", model.selector)
	}
}

func TestLogoutSelectorDeletesOnlyExpectedStoredCredentialType(t *testing.T) {
	api := &selectorTestAPI{providers: []application.ProviderAuthInfo{
		{ID: "anthropic", Name: "Anthropic", Configured: true, Source: "auth.json", CredentialType: "oauth"},
		{ID: "deepseek", Name: "DeepSeek", Configured: true, Source: "environment"},
	}}
	model := newModelWithAPIForTest(t, api)
	open := model.openLogoutSelector("anthropic")
	loaded := selectorCommandAt(t, open, 1)().(selectorLoadedMsg)
	model.handleSelectorLoaded(loaded)
	if model.selector == nil || model.selector.kind != selectorLogoutConfirm || len(model.selector.items) != 2 {
		t.Fatalf("logout confirmation = %#v", model.selector)
	}
	if !model.selector.SelectKey("logout") {
		t.Fatal("logout confirmation item is missing")
	}
	remove := model.applySelectorSelection()
	message, ok := selectorCommandAt(t, remove, 1)().(providerAuthMutationMsg)
	if !ok || message.err != nil || api.deletedID != "anthropic" || api.deletedType != "oauth" {
		t.Fatalf("logout = %#v / %q / %q", message, api.deletedID, api.deletedType)
	}
}

func TestOAuthLoginStartFailureStaysLocalToAuthFlow(t *testing.T) {
	api := &selectorTestAPI{
		providers: []application.ProviderAuthInfo{{
			ID: "openai-codex", Name: "OpenAI Codex", SupportsOAuth: true,
		}},
		oauthErr: errors.New("callback port unavailable"),
	}
	model := newModelWithAPIForTest(t, api)
	open := model.openLoginSelector("openai-codex")
	loaded := selectorCommandAt(t, open, 1)().(selectorLoadedMsg)
	start := model.handleSelectorLoaded(loaded)
	message, ok := selectorCommandAt(t, start, 1)().(providerOAuthStartedMsg)
	if !ok || message.err == nil || api.oauthID != "openai-codex" {
		t.Fatalf("OAuth start = %#v / %q", message, api.oauthID)
	}
	_, _ = model.Update(message)
	if model.status.level != statusError || !strings.Contains(model.status.text, "callback port unavailable") {
		t.Fatalf("OAuth failure status = %#v", model.status)
	}
}

func TestModelSelectorExactReferenceSelectsImmediately(t *testing.T) {
	api := &selectorTestAPI{models: selectorModelsFixture()}
	model := newModelWithAPIForTest(t, api)
	open := model.openModelSelector("GPT-5")
	loaded := selectorCommandAt(t, open, 1)().(selectorLoadedMsg)
	dispatch := model.handleSelectorLoaded(loaded)
	if dispatch == nil || model.selector != nil {
		t.Fatalf("exact model did not close and dispatch: selector=%#v command=%v", model.selector, dispatch != nil)
	}
	_ = selectorCommandAt(t, dispatch, 1)()
	selected, ok := api.commands[len(api.commands)-1].(application.SetModelCommand)
	if !ok || selected.Provider != "openai" || selected.ModelID != "gpt-5" {
		t.Fatalf("exact model command = %#v", api.commands[len(api.commands)-1])
	}
}

func TestModelSelectorDoesNotAutoSelectQueryTypedWhileLoading(t *testing.T) {
	api := &selectorTestAPI{models: selectorModelsFixture()}
	model := newModelWithAPIForTest(t, api)
	open := model.openModelSelector("")
	loaded := selectorCommandAt(t, open, 1)().(selectorLoadedMsg)
	model.selector.input.SetValue("gpt-5")
	model.selector.refilter(true)
	if command := model.handleSelectorLoaded(loaded); command != nil {
		t.Fatal("search typed inside selector was auto-submitted")
	}
	if model.selector == nil {
		t.Fatal("selector closed after an in-selector search")
	}
	selected, ok := model.selector.Selected()
	if !ok || selected.Key != "openai/gpt-5" {
		t.Fatalf("typed search selection = %#v, %t", selected, ok)
	}
}

func TestFailedCanonicalModelSelectionFallsBackToFilteredSelector(t *testing.T) {
	model := newModelForTest(t)
	commands := model.handleCommandFinished(commandFinishedMsg{
		sessionID: model.sessionID, sessionGeneration: model.sessionGeneration, request: 1,
		command: application.SetModelCommand{Provider: "openai", ModelID: "gpt"},
		draft:   "/model openai/gpt", err: errors.New("Model not found: openai/gpt"),
	})
	if model.selector == nil || model.selector.kind != selectorModels || model.selector.Query() != "openai/gpt" {
		t.Fatalf("model fallback selector = %#v", model.selector)
	}
	if len(commands) != 1 || commands[0] == nil {
		t.Fatalf("model fallback commands = %#v", commands)
	}
}

func TestFailedCanonicalModelSelectionDoesNotReplaceActiveSelector(t *testing.T) {
	model := newModelForTest(t)
	_ = model.openToolsSelector()
	generation := model.selectorGeneration
	model.handleCommandFinished(commandFinishedMsg{
		sessionID: model.sessionID, sessionGeneration: model.sessionGeneration, request: 1,
		command: application.SetModelCommand{Provider: "openai", ModelID: "gpt"},
		draft:   "/model openai/gpt", err: errors.New("Model not found: openai/gpt"),
	})
	if model.selector == nil || model.selector.kind != selectorTools || model.selectorGeneration != generation {
		t.Fatalf("model fallback replaced active selector: %#v", model.selector)
	}
	if model.composer.Value() != "/model openai/gpt" {
		t.Fatalf("failed model draft was not preserved behind selector: %q", model.composer.Value())
	}
}

func TestCommandCompletionClosesOnlyAffectedSelector(t *testing.T) {
	modelSpec, err := provider.NewModel(provider.ModelSpec{
		Provider: "fixture", API: "openai-completions", ID: "reasoning", Name: "Reasoning",
		Reasoning: true, ContextWindow: 100_000, MaxTokens: 8_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		open       func(*Model) tea.Cmd
		command    application.Command
		result     application.CommandResult
		wantClosed bool
	}{
		{
			name: "reload closes tools", open: func(model *Model) tea.Cmd { return model.openToolsSelector() },
			command: application.ReloadCommand{}, result: application.ReloadResult{}, wantClosed: true,
		},
		{
			name: "set tools closes tools", open: func(model *Model) tea.Cmd { return model.openToolsSelector() },
			command: application.SetToolsCommand{}, result: application.SetToolsResult{}, wantClosed: true,
		},
		{
			name: "set model closes thinking", open: func(model *Model) tea.Cmd { return model.openThinkingSelector() },
			command: application.SetModelCommand{}, result: application.SetModelResult{Model: modelSpec}, wantClosed: true,
		},
		{
			name: "set thinking closes thinking", open: func(model *Model) tea.Cmd { return model.openThinkingSelector() },
			command: application.SetThinkingLevelCommand{Level: provider.ThinkingHigh},
			result:  application.SetThinkingLevelResult{}, wantClosed: true,
		},
		{
			name: "set model preserves tools", open: func(model *Model) tea.Cmd { return model.openToolsSelector() },
			command: application.SetModelCommand{}, result: application.SetModelResult{Model: modelSpec}, wantClosed: false,
		},
		{
			name: "set tools preserves models", open: func(model *Model) tea.Cmd { return model.openModelSelector("") },
			command: application.SetToolsCommand{}, result: application.SetToolsResult{}, wantClosed: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := newModelForTest(t)
			_ = test.open(model)
			generation := model.selectorGeneration
			model.handleCommandFinished(commandFinishedMsg{
				sessionID: model.sessionID, sessionGeneration: model.sessionGeneration, request: 1,
				command: test.command, result: test.result,
			})
			if test.wantClosed {
				if model.selector != nil || model.selectorGeneration <= generation {
					t.Fatalf("affected selector remained open: %#v (generation %d -> %d)", model.selector, generation, model.selectorGeneration)
				}
				return
			}
			if model.selector == nil || model.selectorGeneration != generation {
				t.Fatalf("unrelated selector changed: %#v (generation %d -> %d)", model.selector, generation, model.selectorGeneration)
			}
		})
	}
}

func TestStateRefreshReconcilesOpenThinkingSelector(t *testing.T) {
	modelSpec, err := provider.NewModel(provider.ModelSpec{
		Provider: "fixture", API: "openai-completions", ID: "reasoning", Name: "Reasoning",
		Reasoning: true, ContextWindow: 100_000, MaxTokens: 8_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	model := newModelForTest(t)
	model.state.Model, model.state.HasModel = modelSpec, true
	model.state.ThinkingLevel = provider.ThinkingLow
	_ = model.openThinkingSelector()
	generation := model.selectorGeneration

	state := model.state
	state.ThinkingLevel = provider.ThinkingHigh
	_, _ = model.Update(stateLoadedMsg{
		sessionID: model.sessionID, sessionGeneration: model.sessionGeneration,
		request: model.projectionGeneration, active: true, state: state,
	})
	if model.selector == nil || model.selector.kind != selectorThinking || model.selectorGeneration <= generation {
		t.Fatalf("thinking selector was not refreshed: %#v", model.selector)
	}
	current := ""
	for _, item := range model.selector.items {
		if item.Current {
			current = item.Key
		}
	}
	if current != string(provider.ThinkingHigh) {
		t.Fatalf("refreshed thinking current level = %q, items=%#v", current, model.selector.items)
	}
}

func TestThinkingAgentEventReconcilesOpenSelector(t *testing.T) {
	modelSpec, err := provider.NewModel(provider.ModelSpec{
		Provider: "fixture", API: "openai-completions", ID: "reasoning", Name: "Reasoning",
		Reasoning: true, ContextWindow: 100_000, MaxTokens: 8_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	model := newModelForTest(t)
	model.state.Model, model.state.HasModel = modelSpec, true
	model.state.ThinkingLevel = provider.ThinkingLow
	_ = model.openThinkingSelector()
	generation := model.selectorGeneration

	model.applyAgentEvent(agent.ThinkingLevelChangedEvent{Level: provider.ThinkingHigh})
	if model.selector == nil || model.selectorGeneration <= generation {
		t.Fatalf("thinking event did not refresh selector: %#v", model.selector)
	}
	current := ""
	for _, item := range model.selector.items {
		if item.Current {
			current = item.Key
		}
	}
	if current != string(provider.ThinkingHigh) {
		t.Fatalf("thinking event current level = %q, items=%#v", current, model.selector.items)
	}
}

func TestSnapshotStateReplacementReconcilesOpenModelSelector(t *testing.T) {
	modelA, err := provider.NewModel(provider.ModelSpec{
		Provider: "fixture", API: "openai-completions", ID: "model-a", Name: "A",
		ContextWindow: 100_000, MaxTokens: 8_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	modelB, err := provider.NewModel(provider.ModelSpec{
		Provider: "fixture", API: "openai-completions", ID: "model-b", Name: "B",
		ContextWindow: 100_000, MaxTokens: 8_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	api := &selectorTestAPI{models: application.ModelsSnapshot{Models: []application.AvailableModel{
		{Provider: "fixture", ID: "model-a", Name: "A"},
		{Provider: "fixture", ID: "model-b", Name: "B"},
	}}}
	model := newModelWithAPIForTest(t, api)
	model.state.Model, model.state.HasModel = modelA, true
	open := model.openModelSelector("")
	model.handleSelectorLoaded(selectorCommandAt(t, open, 1)().(selectorLoadedMsg))
	generation := model.selectorGeneration
	model.projectionGeneration, model.snapshotInFlight, model.snapshotNeeded = 7, 7, true
	state := model.state
	state.Model = modelB

	commands := model.handleSnapshot(snapshotLoadedMsg{
		sessionID: model.sessionID, sessionGeneration: model.sessionGeneration, request: 7,
		snapshot: application.SessionSnapshot{
			Revision: 20, SessionID: model.sessionID,
			Info: application.SessionInfo{ID: model.sessionID, CWD: model.state.CWD}, LiveState: &state,
		},
	})
	if model.selector == nil || !model.selector.loading || model.selectorGeneration <= generation {
		t.Fatalf("snapshot did not reload model selector: %#v", model.selector)
	}
	if len(commands) != 2 || commands[0] == nil || commands[1] == nil {
		t.Fatalf("snapshot reconciliation commands = %#v", commands)
	}
}

func TestCompletedRemoteToolOperationRefreshesOpenSelector(t *testing.T) {
	api := &selectorTestAPI{tools: []application.ToolInfo{{Name: "read", Active: true}}}
	model := newModelWithAPIForTest(t, api)
	open := model.openToolsSelector()
	model.handleSelectorLoaded(selectorCommandAt(t, open, 1)().(selectorLoadedMsg))
	generation := model.selectorGeneration

	commands := model.applyApplicationEvent(application.Event{
		Sequence: model.revision + 1, SessionID: model.sessionID,
		Value: application.OperationEvent{Command: application.CommandSetTools, Status: application.OperationCompleted},
	})
	if model.selector == nil || !model.selector.loading || model.selectorGeneration <= generation {
		t.Fatalf("tool selector was not reloaded: %#v", model.selector)
	}
	if len(commands) != 2 || commands[0] == nil || commands[1] == nil {
		t.Fatalf("remote tool refresh commands = %#v", commands)
	}
}

func TestSelectorRejectsStaleLoadAfterAnotherSelectorOpens(t *testing.T) {
	api := &selectorTestAPI{models: selectorModelsFixture()}
	model := newModelWithAPIForTest(t, api)
	modelOpen := model.openModelSelector("")
	stale := selectorCommandAt(t, modelOpen, 1)().(selectorLoadedMsg)
	_ = model.openSessionSelector("")
	if command := model.handleSelectorLoaded(stale); command != nil {
		t.Fatal("stale model load produced a command")
	}
	if model.selector == nil || model.selector.kind != selectorSessions || len(model.selector.items) != 0 {
		t.Fatalf("stale load changed active selector: %#v", model.selector)
	}
}

func TestSuccessfulSessionOpenClosesSelectorStartedWhileRequestWasInFlight(t *testing.T) {
	model := newModelForTest(t)
	model.openRequest = 3
	_ = model.openToolsSelector()
	selectorGeneration := model.selectorGeneration
	state := application.State{SessionID: "session-2", CWD: "/other"}
	commands := model.handleSessionOpened(sessionOpenedMsg{
		sourceGeneration: model.sessionGeneration, request: 3, state: state,
		snapshot: application.SessionSnapshot{
			Revision: 20, SessionID: "session-2", Info: application.SessionInfo{ID: "session-2", CWD: "/other"},
			LiveState: &state,
		},
	})
	if model.selector != nil || model.selectorCancel != nil {
		t.Fatalf("session switch retained selector: %#v", model.selector)
	}
	if model.selectorGeneration <= selectorGeneration || model.sessionID != "session-2" {
		t.Fatalf("switch generations selector=%d session=%q", model.selectorGeneration, model.sessionID)
	}
	if len(commands) < 2 {
		t.Fatalf("session switch commands = %#v", commands)
	}
}

func TestSessionSelectorLoadsAndOpensSelectedSession(t *testing.T) {
	api := &selectorTestAPI{sessions: []application.SessionInfo{
		{ID: "session-1", CWD: "/workspace", Name: "Current", Modified: time.Now()},
		{ID: "session-2", CWD: "/other", FirstMessage: "Investigate selectors", MessageCount: 4, Modified: time.Now().Add(-time.Hour)},
	}}
	model := newModelWithAPIForTest(t, api)
	open := model.openSessionSelector("")
	loaded := selectorCommandAt(t, open, 1)().(selectorLoadedMsg)
	model.handleSelectorLoaded(loaded)
	model.selector.Move(1)
	selected, _ := model.selector.Selected()
	if selected.Key != "session-2" {
		t.Fatalf("selected session = %#v", selected)
	}
	command := model.applySelectorSelection()
	if command == nil || model.openRequest != 1 {
		t.Fatalf("session selection did not start open request: %v / %d", command != nil, model.openRequest)
	}
}

func TestThinkingSelectorUsesCurrentModelSupportedLevels(t *testing.T) {
	modelSpec, err := provider.NewModel(provider.ModelSpec{
		Provider: "fixture", API: "openai-completions", ID: "reasoning", Name: "Reasoning",
		Reasoning: true, ContextWindow: 100_000, MaxTokens: 8_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	api := &selectorTestAPI{}
	model := newModelWithAPIForTest(t, api)
	model.state.Model, model.state.HasModel = modelSpec, true
	model.state.ThinkingLevel = provider.ThinkingMedium
	_ = model.openThinkingSelector()
	if model.selector == nil || model.selector.loading || len(model.selector.items) != 5 {
		t.Fatalf("thinking selector = %#v", model.selector)
	}
	selected, ok := model.selector.Selected()
	if !ok || selected.Key != string(provider.ThinkingMedium) {
		t.Fatalf("current thinking selection = %#v", selected)
	}
	model.selector.Move(1)
	dispatch := model.applySelectorSelection()
	finished, ok := selectorCommandAt(t, dispatch, 1)().(commandFinishedMsg)
	if !ok || finished.err != nil {
		t.Fatalf("thinking result = %#v", finished)
	}
	set, ok := api.commands[len(api.commands)-1].(application.SetThinkingLevelCommand)
	if !ok || set.Level != provider.ThinkingHigh {
		t.Fatalf("thinking command = %#v", api.commands[len(api.commands)-1])
	}
}

func TestToolsSelectorTogglesAndAppliesActiveSet(t *testing.T) {
	api := &selectorTestAPI{tools: []application.ToolInfo{
		{Name: "read", Active: true}, {Name: "edit", Active: false}, {Name: "bash", Active: true},
	}}
	model := newModelWithAPIForTest(t, api)
	open := model.openToolsSelector()
	loaded := selectorCommandAt(t, open, 1)().(selectorLoadedMsg)
	model.handleSelectorLoaded(loaded)
	model.selector.Move(1)
	model.handleSelectorKey(tea.KeyPressMsg(tea.Key{Code: ' ', Text: " "}))
	dispatch := model.applySelectorSelection()
	_ = selectorCommandAt(t, dispatch, 1)()
	set, ok := api.commands[len(api.commands)-1].(application.SetToolsCommand)
	if !ok || strings.Join(set.ToolNames, ",") != "read,edit,bash" {
		t.Fatalf("tools command = %#v", api.commands[len(api.commands)-1])
	}
}

func TestSelectorErrorAndLayoutRemainInteractive(t *testing.T) {
	api := &selectorTestAPI{modelErr: errors.New("catalog unavailable")}
	model := newModelWithAPIForTest(t, api)
	model.width, model.height = 60, 12
	open := model.openModelSelector("")
	loaded := selectorCommandAt(t, open, 1)().(selectorLoadedMsg)
	model.handleSelectorLoaded(loaded)
	if model.selector == nil || model.selector.err != "catalog unavailable" {
		t.Fatalf("selector error = %#v", model.selector)
	}
	view := model.View()
	if rows := lipgloss.Height(view.Content); rows != model.height {
		t.Fatalf("selector view rows = %d, want %d:\n%s", rows, model.height, view.Content)
	}
	if !strings.Contains(StripTerminalSequences(view.Content), "catalog unavailable") {
		t.Fatalf("selector error is not visible:\n%s", view.Content)
	}
	model.handleSelectorKey(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	if model.selector != nil {
		t.Fatal("escape did not close selector")
	}
}

func TestSelectorLayoutAcrossSmallTerminalSizes(t *testing.T) {
	model := newModelForTest(t)
	_ = model.openModelSelector("a-very-long-model-query-that-must-scroll")
	for _, size := range []struct{ width, height int }{
		{20, 5}, {21, 6}, {32, 8}, {60, 12}, {100, 24},
	} {
		_, _ = model.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
		view := model.View()
		if rows := lipgloss.Height(view.Content); rows != size.height {
			t.Fatalf("%dx%d selector rows = %d:\n%s", size.width, size.height, rows, view.Content)
		}
		for _, line := range strings.Split(view.Content, "\n") {
			if columns := lipgloss.Width(line); columns > size.width {
				t.Fatalf("%dx%d selector has %d-column row: %q", size.width, size.height, columns, line)
			}
		}
		if cursor := view.Cursor; cursor != nil &&
			(cursor.Position.X < 0 || cursor.Position.X >= size.width || cursor.Position.Y < 0 || cursor.Position.Y >= size.height) {
			t.Fatalf("%dx%d selector cursor is out of bounds: %#v", size.width, size.height, cursor)
		}
	}
}

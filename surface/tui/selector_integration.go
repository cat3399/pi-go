package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/provider"
)

func (m *Model) openModelSelector(query string) tea.Cmd {
	ctx, focus := m.activateSelector(selectorModels, "Select model", query, true, false)
	return tea.Batch(focus, m.selectorLoadCommand(ctx))
}

func (m *Model) openSessionSelector(query string) tea.Cmd {
	ctx, focus := m.activateSelector(selectorSessions, "Resume session", query, true, false)
	return tea.Batch(focus, m.selectorLoadCommand(ctx))
}

func (m *Model) openThinkingSelector() tea.Cmd {
	_, focus := m.activateSelector(selectorThinking, "Select thinking level", "", false, false)
	m.selector.SetItems(m.thinkingSelectorItems(), "")
	return focus
}

func (m *Model) openToolsSelector() tea.Cmd {
	ctx, focus := m.activateSelector(selectorTools, "Configure tools", "", true, true)
	return tea.Batch(focus, m.selectorLoadCommand(ctx))
}

func (m *Model) openSettingsSelector() tea.Cmd {
	_, focus := m.activateSelector(selectorSettings, "Settings", "", false, false)
	m.selector.SetItems(m.settingsSelectorItems(), "Changes are saved immediately")
	return focus
}

func (m *Model) openImportConfirm(path string) tea.Cmd {
	m.pendingImportPath = strings.TrimSpace(path)
	_, focus := m.activateSelector(selectorImportConfirm, "Import session?", "", false, false)
	m.selector.SetItems([]selectorItem{
		{Key: "cancel", Title: "Cancel", Description: "Keep the current session unchanged"},
		{Key: "import", Title: "Import and replace", Description: m.pendingImportPath},
	}, "The current TUI will switch to the imported session")
	return focus
}

func (m *Model) openSessionRename(sessionID string) tea.Cmd {
	m.pendingSessionID = sessionID
	_, focus := m.activateSelector(selectorSessionRename, "Rename session", "", true, false)
	m.selector.input.Prompt = "Name: "
	m.selector.input.Placeholder = "session name"
	m.selector.SetItems([]selectorItem{{
		Key: "rename", Title: "Apply new name", Description: "Enter a non-empty session name",
	}}, "")
	return focus
}

func (m *Model) openSessionDelete(sessionID, title string) tea.Cmd {
	m.pendingSessionID = sessionID
	_, focus := m.activateSelector(selectorSessionDelete, "Delete session?", "", false, false)
	m.selector.SetItems([]selectorItem{
		{Key: "cancel", Title: "Cancel", Description: "Keep the session"},
		{Key: "delete", Title: "Delete permanently", Description: title},
	}, "This cannot be undone")
	return focus
}

func (m *Model) openTrustConfirm() tea.Cmd {
	_, focus := m.activateSelector(selectorTrustConfirm, "Trust project resources?", "", false, false)
	m.selector.SetItems([]selectorItem{
		{Key: "cancel", Title: "Cancel", Description: "Keep project-local resources disabled"},
		{Key: "trust", Title: "Trust this project", Description: m.state.CWD},
	}, "Trusted project resources may execute code")
	return focus
}

func (m *Model) openLoginSelector(query string) tea.Cmd {
	m.cancelOAuthLogin()
	m.pendingAuthID, m.pendingAuthType = "", ""
	ctx, focus := m.activateSelector(selectorLoginProvider, "Log in to provider", query, true, false)
	return tea.Batch(focus, m.selectorLoadCommand(ctx))
}

func (m *Model) openLogoutSelector(query string) tea.Cmd {
	m.cancelOAuthLogin()
	m.pendingAuthID, m.pendingAuthType = "", ""
	ctx, focus := m.activateSelector(selectorLogoutProvider, "Log out of provider", query, true, false)
	return tea.Batch(focus, m.selectorLoadCommand(ctx))
}

func (m *Model) openLoginMethod(info application.ProviderAuthInfo) tea.Cmd {
	m.pendingAuthID = info.ID
	_, focus := m.activateSelector(selectorLoginMethod, "Choose login method", "", false, false)
	items := make([]selectorItem, 0, 2)
	if info.SupportsOAuth {
		title := "OAuth"
		if strings.TrimSpace(info.OAuthName) != "" {
			title = info.OAuthName
		}
		items = append(items, selectorItem{
			Key: "oauth", Title: title, Description: "Sign in through your browser",
		})
	}
	if info.SupportsAPIKey {
		items = append(items, selectorItem{
			Key: "api_key", Title: "API key", Description: "Store a provider API key in auth.json",
		})
	}
	m.selector.SetItems(items, info.Name+" · "+info.ID)
	return focus
}

func (m *Model) openAPIKeyInput(info application.ProviderAuthInfo) tea.Cmd {
	m.pendingAuthID = info.ID
	_, focus := m.activateSelector(selectorLoginAPIKey, "Enter API key for "+info.ID, "", true, false)
	m.selector.input.Prompt = "API key: "
	m.selector.input.Placeholder = "paste key"
	m.selector.input.EchoMode = textinput.EchoPassword
	m.selector.SetItems([]selectorItem{{
		Key: "save", Title: "Save API key", Description: "The value is stored in the agent auth file",
	}}, "Input is masked and is never added to the conversation")
	return focus
}

func (m *Model) openOAuthCallback(providerID string, login *application.ProviderOAuthLogin) tea.Cmd {
	m.pendingAuthID = providerID
	_, focus := m.activateSelector(selectorLoginOAuth, "Complete OAuth login for "+providerID, "", true, false)
	m.selector.input.Prompt = "Callback: "
	m.selector.input.Placeholder = "paste redirect URL if needed"
	m.selector.SetItems([]selectorItem{{
		Key: "submit", Title: "Submit callback", Description: "Browser login may complete automatically",
	}}, login.Instructions)
	m.appendLocalNotice("OAuth sign-in · "+providerID, login.Instructions+"\n\n"+login.URL)
	m.setStatus("Waiting for OAuth login…", statusInfo)
	return focus
}

func (m *Model) openLogoutConfirm(info application.ProviderAuthInfo) tea.Cmd {
	m.pendingAuthID, m.pendingAuthType = info.ID, info.CredentialType
	_, focus := m.activateSelector(selectorLogoutConfirm, "Remove stored credential?", "", false, false)
	m.selector.SetItems([]selectorItem{
		{Key: "cancel", Title: "Cancel", Description: "Keep the stored credential"},
		{Key: "logout", Title: "Log out", Description: info.Name + " · " + info.CredentialType},
	}, "The provider credential will be removed from auth.json")
	return focus
}

func (m *Model) cancelOAuthLogin() {
	m.authGeneration++
	if m.oauthLogin != nil {
		m.oauthLogin.Close()
		m.oauthLogin = nil
	}
}

func (m *Model) startOAuthLogin(providerID string) tea.Cmd {
	m.cancelOAuthLogin()
	generation := m.authGeneration
	focus := m.closeSelector()
	m.setStatus("Starting OAuth login for "+providerID+"…", statusInfo)
	return tea.Batch(focus, startProviderOAuthCmd(m.ctx, m.api, providerID, generation))
}

func (m *Model) openTreeSelector() tea.Cmd {
	if m.busy() {
		m.setStatus("Abort the active operation before navigating the tree", statusWarning)
		return nil
	}
	ctx, focus := m.activateSelector(selectorTree, "Navigate session tree", "", true, false)
	return tea.Batch(focus, m.selectorLoadCommand(ctx))
}

func (m *Model) openForkSelector() tea.Cmd {
	if m.busy() {
		m.setStatus("Abort the active operation before forking", statusWarning)
		return nil
	}
	ctx, focus := m.activateSelector(selectorFork, "Fork from user message", "", true, false)
	return tea.Batch(focus, m.selectorLoadCommand(ctx))
}

func (m *Model) activateSelector(
	kind selectorKind,
	title, query string,
	searchable, multi bool,
) (context.Context, tea.Cmd) {
	if m.selectorCancel != nil {
		m.selectorCancel()
	}
	m.selectorGeneration++
	parent := m.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	m.selectorCancel = cancel
	m.selector = newSelectorModel(m.theme, kind, title, query, searchable, multi)
	m.selector.SetKeybindings(m.keybindings)
	m.composer.Blur()
	m.slashPalette.Hide(m.composer.Value())
	m.setStatus("", statusInfo)
	return ctx, m.selector.Focus()
}

func (m *Model) closeSelector() tea.Cmd {
	if m.selectorCancel != nil {
		m.selectorCancel()
		m.selectorCancel = nil
	}
	if m.selector != nil {
		if m.selector.kind == selectorTreeSummary || m.selector.kind == selectorTreeSummaryCustom {
			m.treeTargetID = ""
		}
		if m.selector.kind == selectorImportConfirm {
			m.pendingImportPath = ""
		}
		if m.selector.kind == selectorSessionRename || m.selector.kind == selectorSessionDelete {
			m.pendingSessionID = ""
		}
		switch m.selector.kind {
		case selectorLoginProvider, selectorLoginMethod, selectorLoginAPIKey, selectorLoginOAuth,
			selectorLogoutProvider, selectorLogoutConfirm:
			m.pendingAuthID, m.pendingAuthType = "", ""
		}
		m.selector.Blur()
		m.selector = nil
	}
	m.selectorGeneration++
	m.updateSlashPalette()
	return m.composer.Focus()
}

func selectorAffectedByCommand(kind selectorKind, command application.CommandType) bool {
	switch command {
	case application.CommandReload:
		return true
	case application.CommandSetModel:
		return kind == selectorModels || kind == selectorThinking
	case application.CommandSetThinkingLevel:
		return kind == selectorThinking
	case application.CommandSetTools:
		return kind == selectorTools
	default:
		return false
	}
}

func (m *Model) closeSelectorForCommand(command application.CommandType) tea.Cmd {
	if m.selector == nil || !selectorAffectedByCommand(m.selector.kind, command) {
		return nil
	}
	return m.closeSelector()
}

func (m *Model) refreshSelectorForOperation(command application.CommandType) tea.Cmd {
	if m.selector == nil {
		return nil
	}
	switch command {
	case application.CommandReload:
		return m.refreshSelector()
	case application.CommandSetTools:
		if m.selector.kind == selectorTools {
			return m.refreshSelector()
		}
	}
	return nil
}

func (m *Model) refreshSelectorForStateChange(previous, current application.State) tea.Cmd {
	if m.selector == nil {
		return nil
	}
	modelChanged := previous.HasModel != current.HasModel
	if previous.HasModel && current.HasModel && !previous.Model.Equal(current.Model) {
		modelChanged = true
	}
	thinkingChanged := previous.ThinkingLevel != current.ThinkingLevel
	settingsChanged := previous.SteeringMode != current.SteeringMode ||
		previous.FollowUpMode != current.FollowUpMode ||
		previous.AutoCompactionEnabled != current.AutoCompactionEnabled ||
		previous.AutoRetryEnabled != current.AutoRetryEnabled
	switch m.selector.kind {
	case selectorModels:
		if modelChanged {
			return m.refreshSelector()
		}
	case selectorThinking:
		if modelChanged || thinkingChanged {
			return m.refreshSelector()
		}
	case selectorSettings:
		if modelChanged || thinkingChanged || settingsChanged {
			m.selector.SetItems(m.settingsSelectorItems(), "Changes are saved immediately")
		}
	}
	return nil
}

func (m *Model) refreshSelector() tea.Cmd {
	if m.selector == nil {
		return nil
	}
	if m.selectorCancel != nil {
		m.selectorCancel()
	}
	m.selectorGeneration++
	parent := m.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	m.selectorCancel = cancel
	if m.selector.kind == selectorThinking {
		m.selector.SetItems(m.thinkingSelectorItems(), "")
		return nil
	}
	if m.selector.kind == selectorSettings {
		m.selector.SetItems(m.settingsSelectorItems(), "Changes are saved immediately")
		return nil
	}
	m.selector.SetLoading()
	return m.selectorLoadCommand(ctx)
}

func (m *Model) selectorLoadCommand(ctx context.Context) tea.Cmd {
	if m.selector == nil {
		return nil
	}
	switch m.selector.kind {
	case selectorModels:
		cwd := strings.TrimSpace(m.state.CWD)
		if cwd == "" {
			cwd = m.api.DefaultCWD()
		}
		return loadModelSelectorCmd(
			ctx, m.api, cwd, m.sessionID, m.sessionGeneration, m.selectorGeneration, m.selector.kind,
		)
	case selectorSessions:
		return loadSessionSelectorCmd(
			ctx, m.api, m.sessionID, m.sessionGeneration, m.selectorGeneration,
		)
	case selectorTools:
		return loadToolSelectorCmd(
			ctx, m.api, m.sessionID, m.sessionGeneration, m.selectorGeneration,
		)
	case selectorTree, selectorFork:
		return loadTreeSelectorCmd(
			ctx, m.api, m.sessionID, m.sessionGeneration, m.selectorGeneration, m.selector.kind,
		)
	case selectorLoginProvider, selectorLogoutProvider:
		cwd := strings.TrimSpace(m.state.CWD)
		if cwd == "" {
			cwd = m.api.DefaultCWD()
		}
		return loadProviderSelectorCmd(
			ctx, m.api, cwd, m.sessionID, m.sessionGeneration, m.selectorGeneration, m.selector.kind,
		)
	default:
		return nil
	}
}

func (m *Model) handleSelectorLoaded(message selectorLoadedMsg) tea.Cmd {
	if m.selector == nil || message.kind != m.selector.kind ||
		message.selectorGeneration != m.selectorGeneration ||
		message.sessionID != m.sessionID || message.sessionGeneration != m.sessionGeneration {
		return nil
	}
	autoSelectQuery := m.selector.TakeAutoSelectQuery()
	if message.err != nil {
		m.selector.SetError(message.err)
		return nil
	}
	switch message.kind {
	case selectorModels:
		m.selector.SetItems(m.modelSelectorItems(message.models), modelSelectorNotice(message.models))
		if exact, ok := exactModelSelectorKey(message.models.Models, autoSelectQuery); ok && m.selector.SelectKey(exact) {
			return m.applySelectorSelection()
		}
	case selectorSessions:
		m.selector.SetItems(m.sessionSelectorItems(message.sessions), m.sessionSelectorNotice())
	case selectorTools:
		m.selector.SetItems(toolSelectorItems(message.tools), "")
	case selectorTree:
		m.selector.SetItems(treeSelectorItems(message.snapshot), "")
	case selectorFork:
		m.selector.SetItems(forkSelectorItems(message.snapshot), "")
	case selectorLoginProvider:
		m.authProviders = providerAuthMap(message.providers)
		m.selector.SetItems(loginProviderItems(message.providers), "Choose OAuth or API key after selecting a provider")
		if exact, ok := exactProviderSelectorKey(message.providers, autoSelectQuery, false); ok && m.selector.SelectKey(exact) {
			return m.applySelectorSelection()
		}
	case selectorLogoutProvider:
		m.authProviders = providerAuthMap(message.providers)
		m.selector.SetItems(logoutProviderItems(message.providers), "Only credentials stored in auth.json can be removed")
		if exact, ok := exactProviderSelectorKey(message.providers, autoSelectQuery, true); ok && m.selector.SelectKey(exact) {
			return m.applySelectorSelection()
		}
	}
	return nil
}

func (m *Model) handleSelectorKey(message tea.KeyPressMsg) tea.Cmd {
	if m.selector == nil {
		return nil
	}
	matches := func(action string) bool { return m.keybindings.MatchesPress(action, message) }
	switch {
	case matches(keySelectCancel):
		if m.selector.kind == selectorLoginOAuth {
			m.cancelOAuthLogin()
			focus := m.closeSelector()
			m.setStatus("OAuth login cancelled", statusWarning)
			return focus
		}
		return m.closeSelector()
	case matches(keySelectUp):
		m.selector.Move(-1)
		return nil
	case matches(keySelectDown):
		m.selector.Move(1)
		return nil
	case matches(keySelectPageUp):
		m.selector.MovePage(-1, 8)
		return nil
	case matches(keySelectPageDown):
		m.selector.MovePage(1, 8)
		return nil
	case matches(keySessionRename) && m.selector.kind == selectorSessions:
		if selected, ok := m.selector.Selected(); ok {
			return m.openSessionRename(selected.Key)
		}
		return nil
	case message.Keystroke() == "ctrl+r" && m.selector.kind != selectorSessions:
		return m.refreshSelector()
	case matches(keySessionToggleNamed) && m.selector.kind == selectorSessions:
		m.sessionNamedOnly = !m.sessionNamedOnly
		return m.refreshSelector()
	case matches(keySessionToggleSort) && m.selector.kind == selectorSessions:
		m.sessionSortMode = nextSessionSortMode(m.sessionSortMode)
		return m.refreshSelector()
	case matches(keySessionTogglePath) && m.selector.kind == selectorSessions:
		m.sessionShowPath = !m.sessionShowPath
		return m.refreshSelector()
	case matches(keySessionDelete) && m.selector.kind == selectorSessions:
		if selected, ok := m.selector.Selected(); ok {
			if selected.Current {
				m.setStatus("Switch away before deleting the active session", statusWarning)
				return nil
			}
			return m.openSessionDelete(selected.Key, selected.Title)
		}
		return nil
	case message.String() == " " || message.Keystroke() == "space":
		if m.selector.multi {
			m.selector.ToggleSelected()
			return nil
		}
	case matches(keySelectConfirm):
		return m.applySelectorSelection()
	}
	return m.selector.Update(message)
}

func (m *Model) applySelectorSelection() tea.Cmd {
	if m.selector == nil || m.selector.loading || m.selector.err != "" {
		return nil
	}
	kind := m.selector.kind
	if kind == selectorTools {
		toolNames := m.selector.CheckedKeys()
		focus := m.closeSelector()
		m.setStatus("Updating tools…", statusInfo)
		return tea.Batch(focus, m.dispatchCommand(application.SetToolsCommand{ToolNames: toolNames}, "", nil))
	}
	selected, ok := m.selector.Selected()
	if !ok {
		return nil
	}
	if kind == selectorTree || kind == selectorFork || kind == selectorTreeSummary || kind == selectorTreeSummaryCustom {
		return m.applyTreeSelectorSelection(kind, selected)
	}
	if kind == selectorSettings {
		return m.applySettingsSelection(selected)
	}
	if kind == selectorImportConfirm {
		path := m.pendingImportPath
		focus := m.closeSelector()
		if selected.Key != "import" {
			m.setStatus("Import cancelled", statusWarning)
			return focus
		}
		if !filepath.IsAbs(path) {
			cwd := m.state.CWD
			if strings.TrimSpace(cwd) == "" {
				cwd = m.api.DefaultCWD()
			}
			path = filepath.Join(cwd, path)
		}
		m.openRequest++
		m.setStatus("Importing session…", statusInfo)
		return tea.Batch(focus, importSessionCmd(
			m.ctx, m.api, m.sessionID, path, m.sessionGeneration, m.openRequest,
		))
	}
	if kind == selectorSessionRename {
		targetID := m.pendingSessionID
		name := strings.TrimSpace(m.selector.Query())
		if name == "" {
			m.setStatus("Session name cannot be empty", statusWarning)
			return nil
		}
		focus := m.closeSelector()
		m.setStatus("Renaming session…", statusInfo)
		return tea.Batch(focus, renameSessionCmd(m.ctx, m.api, targetID, name, m.sessionGeneration))
	}
	if kind == selectorSessionDelete {
		targetID := m.pendingSessionID
		focus := m.closeSelector()
		if selected.Key != "delete" {
			m.setStatus("Delete cancelled", statusWarning)
			return focus
		}
		m.setStatus("Deleting session…", statusWarning)
		return tea.Batch(focus, deleteSessionCmd(m.ctx, m.api, targetID, m.sessionGeneration))
	}
	if kind == selectorLoginAPIKey {
		providerID := m.pendingAuthID
		apiKey := strings.TrimSpace(m.selector.Query())
		if providerID == "" || apiKey == "" {
			m.setStatus("API key is required", statusWarning)
			return nil
		}
		m.cancelOAuthLogin()
		generation := m.authGeneration
		focus := m.closeSelector()
		m.setStatus("Saving API key for "+providerID+"…", statusInfo)
		return tea.Batch(focus, setProviderAPIKeyCmd(m.ctx, m.api, providerID, apiKey, generation))
	}
	if kind == selectorLoginOAuth {
		callback := strings.TrimSpace(m.selector.Query())
		if callback == "" {
			m.setStatus("Waiting for browser login; paste a callback URL only if needed", statusInfo)
			return nil
		}
		if m.oauthLogin == nil {
			m.setStatus("OAuth login is no longer pending", statusError)
			return m.closeSelector()
		}
		if err := m.oauthLogin.Submit(callback); err != nil {
			m.setStatus("OAuth callback failed: "+err.Error(), statusError)
			return nil
		}
		focus := m.closeSelector()
		m.setStatus("OAuth callback submitted; waiting for completion…", statusInfo)
		return focus
	}
	if kind == selectorLogoutConfirm {
		providerID, credentialType := m.pendingAuthID, m.pendingAuthType
		focus := m.closeSelector()
		if selected.Key != "logout" {
			m.setStatus("Logout cancelled", statusWarning)
			return focus
		}
		m.cancelOAuthLogin()
		generation := m.authGeneration
		m.setStatus("Removing credential for "+providerID+"…", statusWarning)
		return tea.Batch(focus, deleteProviderCredentialCmd(
			m.ctx, m.api, providerID, credentialType, generation,
		))
	}
	if kind == selectorTrustConfirm {
		focus := m.closeSelector()
		if selected.Key != "trust" {
			m.setStatus("Project trust cancelled", statusWarning)
			return focus
		}
		m.setStatus("Trusting project…", statusWarning)
		return tea.Batch(focus, trustProjectCmd(m.ctx, m.api, m.state.CWD))
	}
	pendingAuthID := m.pendingAuthID
	focus := m.closeSelector()
	switch kind {
	case selectorLoginProvider:
		info, exists := m.authProviders[selected.Key]
		if !exists {
			m.setStatus("Selected provider is no longer available", statusError)
			return focus
		}
		switch {
		case info.SupportsOAuth && info.SupportsAPIKey:
			return m.openLoginMethod(info)
		case info.SupportsOAuth:
			return m.startOAuthLogin(info.ID)
		case info.SupportsAPIKey:
			return m.openAPIKeyInput(info)
		default:
			m.setStatus("Selected provider has no supported login method", statusError)
			return focus
		}
	case selectorLoginMethod:
		info, exists := m.authProviders[pendingAuthID]
		if !exists {
			m.setStatus("Selected provider is no longer available", statusError)
			return focus
		}
		if selected.Key == "oauth" {
			return m.startOAuthLogin(info.ID)
		}
		if selected.Key == "api_key" {
			return m.openAPIKeyInput(info)
		}
		m.setStatus("Unknown login method", statusError)
		return focus
	case selectorLogoutProvider:
		info, exists := m.authProviders[selected.Key]
		if !exists || info.CredentialType == "" {
			m.setStatus("Selected stored credential is no longer available", statusError)
			return focus
		}
		return m.openLogoutConfirm(info)
	case selectorModels:
		providerID, modelID, found := strings.Cut(selected.Key, "/")
		if !found || providerID == "" || modelID == "" {
			m.setStatus("Selected model has an invalid reference", statusError)
			return focus
		}
		m.setStatus("Switching model…", statusInfo)
		return tea.Batch(focus, m.dispatchCommand(application.SetModelCommand{
			Provider: providerID, ModelID: modelID,
		}, "", nil))
	case selectorSessions:
		if selected.Key == m.sessionID {
			m.setStatus("Already in this session", statusWarning)
			return focus
		}
		m.setStatus("Opening session "+shortID(selected.Key)+"…", statusInfo)
		return tea.Batch(focus, m.openSession(selected.Key))
	case selectorThinking:
		level := provider.ThinkingLevel(selected.Key)
		if !level.Valid() {
			m.setStatus("Selected thinking level is invalid", statusError)
			return focus
		}
		m.setStatus("Updating thinking level…", statusInfo)
		return tea.Batch(focus, m.dispatchCommand(application.SetThinkingLevelCommand{Level: level}, "", nil))
	default:
		return focus
	}
}

func providerAuthMap(providers []application.ProviderAuthInfo) map[string]application.ProviderAuthInfo {
	result := make(map[string]application.ProviderAuthInfo, len(providers))
	for _, info := range providers {
		if strings.TrimSpace(info.ID) != "" {
			result[info.ID] = info
		}
	}
	return result
}

func loginProviderItems(providers []application.ProviderAuthInfo) []selectorItem {
	items := make([]selectorItem, 0, len(providers))
	for _, info := range providers {
		if !info.SupportsAPIKey && !info.SupportsOAuth {
			continue
		}
		methods := make([]string, 0, 2)
		if info.SupportsOAuth {
			methods = append(methods, "OAuth")
		}
		if info.SupportsAPIKey {
			methods = append(methods, "API key")
		}
		badge := strings.Join(methods, " / ")
		description := fmt.Sprintf("%d models", info.ModelCount)
		if info.Configured {
			description += " • currently authenticated via " + info.Source
		}
		items = append(items, selectorItem{
			Key: info.ID, Title: info.Name, Badge: badge, Description: description,
			Keywords: info.ID + " " + info.Name + " " + info.OAuthName, Current: info.Configured,
		})
	}
	sort.SliceStable(items, func(left, right int) bool {
		if items[left].Current != items[right].Current {
			return !items[left].Current
		}
		return items[left].Title < items[right].Title
	})
	return items
}

func logoutProviderItems(providers []application.ProviderAuthInfo) []selectorItem {
	items := make([]selectorItem, 0, len(providers))
	for _, info := range providers {
		if strings.TrimSpace(info.CredentialType) == "" {
			continue
		}
		items = append(items, selectorItem{
			Key: info.ID, Title: info.Name, Badge: info.CredentialType,
			Description: "authenticated via " + info.Source,
			Keywords:    info.ID + " " + info.Name + " " + info.CredentialType,
		})
	}
	sort.SliceStable(items, func(left, right int) bool { return items[left].Title < items[right].Title })
	return items
}

func exactProviderSelectorKey(
	providers []application.ProviderAuthInfo,
	query string,
	requireStoredCredential bool,
) (string, bool) {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return "", false
	}
	match := ""
	for _, info := range providers {
		if requireStoredCredential && info.CredentialType == "" {
			continue
		}
		if !requireStoredCredential && !info.SupportsAPIKey && !info.SupportsOAuth {
			continue
		}
		if strings.ToLower(info.ID) != query && strings.ToLower(info.Name) != query {
			continue
		}
		if match != "" {
			return "", false
		}
		match = info.ID
	}
	return match, match != ""
}

func (m *Model) modelSelectorItems(snapshot application.ModelsSnapshot) []selectorItem {
	items := make([]selectorItem, 0, len(snapshot.Models))
	for _, candidate := range snapshot.Models {
		key := candidate.Provider + "/" + candidate.ID
		current := m.state.HasModel && m.state.Model.Provider() == candidate.Provider && m.state.Model.ID() == candidate.ID
		items = append(items, selectorItem{
			Key: key, Title: candidate.ID, Badge: candidate.Provider,
			Description: candidate.Name, Keywords: key + " " + candidate.Name, Current: current,
		})
	}
	sort.SliceStable(items, func(left, right int) bool {
		if items[left].Current != items[right].Current {
			return items[left].Current
		}
		if items[left].Badge != items[right].Badge {
			return items[left].Badge < items[right].Badge
		}
		return items[left].Title < items[right].Title
	})
	return items
}

func modelSelectorNotice(snapshot application.ModelsSnapshot) string {
	parts := make([]string, 0, len(snapshot.ModelScopeWarnings)+1)
	parts = append(parts, snapshot.ModelScopeWarnings...)
	if snapshot.Diagnostic != "" {
		parts = append(parts, snapshot.Diagnostic)
	}
	return strings.Join(compactNonEmptyStrings(parts), " • ")
}

func (m *Model) sessionSelectorItems(sessions []application.SessionInfo) []selectorItem {
	items := make([]selectorItem, 0, len(sessions))
	for _, session := range sessions {
		if m.sessionNamedOnly && strings.TrimSpace(session.Name) == "" {
			continue
		}
		title := strings.TrimSpace(session.Name)
		if title == "" {
			title = strings.TrimSpace(session.FirstMessage)
		}
		if title == "" {
			title = "(empty session)"
		}
		project := filepath.Base(session.CWD)
		if project == "." || project == string(filepath.Separator) || project == "" {
			project = session.CWD
		}
		if m.sessionShowPath {
			project = session.CWD
		}
		description := fmt.Sprintf("%s • %d messages", shortID(session.ID), session.MessageCount)
		if !session.Modified.IsZero() {
			description += " • " + session.Modified.Local().Format("2006-01-02 15:04")
		}
		items = append(items, selectorItem{
			Key: session.ID, Title: title, Badge: project, Description: description,
			Keywords: session.ID + " " + session.CWD + " " + session.Name + " " + session.FirstMessage,
			Current:  session.ID == m.sessionID, sortTime: session.Modified.UnixNano(), sortCount: session.MessageCount,
		})
	}
	sort.SliceStable(items, func(left, right int) bool {
		if items[left].Current != items[right].Current {
			return items[left].Current
		}
		switch m.sessionSortMode {
		case "name":
			if items[left].Title != items[right].Title {
				return strings.ToLower(items[left].Title) < strings.ToLower(items[right].Title)
			}
		case "messages":
			if items[left].sortCount != items[right].sortCount {
				return items[left].sortCount > items[right].sortCount
			}
		case "project":
			if items[left].Badge != items[right].Badge {
				return strings.ToLower(items[left].Badge) < strings.ToLower(items[right].Badge)
			}
		default:
			if items[left].sortTime != items[right].sortTime {
				return items[left].sortTime > items[right].sortTime
			}
		}
		return items[left].Key < items[right].Key
	})
	return items
}

func nextSessionSortMode(current string) string {
	switch current {
	case "name":
		return "messages"
	case "messages":
		return "project"
	case "project":
		return "recent"
	default:
		return "name"
	}
}

func (m *Model) sessionSelectorNotice() string {
	sortMode := m.sessionSortMode
	if sortMode == "" {
		sortMode = "recent"
	}
	parts := []string{"sort: " + sortMode}
	if m.sessionNamedOnly {
		parts = append(parts, "named only")
	}
	if m.sessionShowPath {
		parts = append(parts, "full paths")
	}
	return strings.Join(parts, " • ")
}

func (m *Model) thinkingSelectorItems() []selectorItem {
	levels := []provider.ThinkingLevel{
		provider.ThinkingOff, provider.ThinkingMinimal, provider.ThinkingLow,
		provider.ThinkingMedium, provider.ThinkingHigh,
	}
	if m.state.HasModel {
		levels = m.state.Model.SupportedThinkingLevels()
	}
	items := make([]selectorItem, 0, len(levels))
	for _, level := range levels {
		if !level.Valid() {
			continue
		}
		items = append(items, selectorItem{
			Key: string(level), Title: string(level), Description: thinkingLevelDescription(level),
			Current: level == m.state.ThinkingLevel,
		})
	}
	return items
}

func exactModelSelectorKey(models []application.AvailableModel, query string) (string, bool) {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return "", false
	}
	canonical := ""
	for _, candidate := range models {
		key := candidate.Provider + "/" + candidate.ID
		if strings.ToLower(key) != query {
			continue
		}
		if canonical != "" {
			return "", false
		}
		canonical = key
	}
	if canonical != "" {
		return canonical, true
	}
	bare := ""
	for _, candidate := range models {
		if strings.ToLower(candidate.ID) != query {
			continue
		}
		if bare != "" {
			return "", false
		}
		bare = candidate.Provider + "/" + candidate.ID
	}
	return bare, bare != ""
}

func thinkingLevelDescription(level provider.ThinkingLevel) string {
	switch level {
	case provider.ThinkingOff:
		return "Disable model reasoning"
	case provider.ThinkingMinimal:
		return "Fastest reasoning with the smallest budget"
	case provider.ThinkingLow:
		return "Light reasoning"
	case provider.ThinkingMedium:
		return "Balanced reasoning"
	case provider.ThinkingHigh:
		return "Deep reasoning"
	case provider.ThinkingXHigh:
		return "Extended deep reasoning"
	case provider.ThinkingMax:
		return "Maximum supported reasoning effort"
	default:
		return ""
	}
}

func toolSelectorItems(tools []application.ToolInfo) []selectorItem {
	items := make([]selectorItem, 0, len(tools))
	for _, tool := range tools {
		items = append(items, selectorItem{
			Key: tool.Name, Title: tool.Name, Description: tool.Description,
			Keywords: tool.Name + " " + tool.Description, Checked: tool.Active,
		})
	}
	return items
}

func (m *Model) renderSelectorView(width, height int) tea.View {
	selectorBudget := max(3, min(16, height-2))
	selector := m.selector.View(width, selectorBudget)
	selectorHeight := lipgloss.Height(selector)
	transcriptHeight := max(1, height-selectorHeight-1)
	transcript := m.renderTranscript(width, transcriptHeight)
	if m.helpVisible {
		transcript = m.renderHelp(width, transcriptHeight)
	}
	view := tea.NewView(m.prepareScreen(strings.Join([]string{transcript, selector, m.renderStateLine(width)}, "\n")))
	view.AltScreen = m.mode == ScreenFull
	view.MouseMode = tea.MouseModeNone
	if m.mode == ScreenFull {
		view.MouseMode = tea.MouseModeCellMotion
	}
	view.ReportFocus = true
	view.WindowTitle = "pi-go"
	view.KeyboardEnhancements.ReportAlternateKeys = true
	if cursor := m.selector.Cursor(); cursor != nil {
		cursor.Position.Y += transcriptHeight
		view.Cursor = cursor
	}
	return view
}

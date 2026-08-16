package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

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
	switch m.selector.kind {
	case selectorModels:
		if modelChanged {
			return m.refreshSelector()
		}
	case selectorThinking:
		if modelChanged || thinkingChanged {
			return m.refreshSelector()
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
		m.selector.SetItems(m.sessionSelectorItems(message.sessions), "")
	case selectorTools:
		m.selector.SetItems(toolSelectorItems(message.tools), "")
	}
	return nil
}

func (m *Model) handleSelectorKey(message tea.KeyPressMsg) tea.Cmd {
	if m.selector == nil {
		return nil
	}
	switch message.String() {
	case "esc", "ctrl+c":
		return m.closeSelector()
	case "up":
		m.selector.Move(-1)
		return nil
	case "down":
		m.selector.Move(1)
		return nil
	case "pgup":
		m.selector.MovePage(-1, 8)
		return nil
	case "pgdown":
		m.selector.MovePage(1, 8)
		return nil
	case "ctrl+r":
		return m.refreshSelector()
	case " ", "space":
		if m.selector.multi {
			m.selector.ToggleSelected()
			return nil
		}
	case "enter":
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
	focus := m.closeSelector()
	switch kind {
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
		description := fmt.Sprintf("%s • %d messages", shortID(session.ID), session.MessageCount)
		if !session.Modified.IsZero() {
			description += " • " + session.Modified.Local().Format("2006-01-02 15:04")
		}
		items = append(items, selectorItem{
			Key: session.ID, Title: title, Badge: project, Description: description,
			Keywords: session.ID + " " + session.CWD + " " + session.Name + " " + session.FirstMessage,
			Current:  session.ID == m.sessionID,
		})
	}
	return items
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
	transcript := m.transcript.View(width, transcriptHeight, m.renderer)
	if m.helpVisible {
		transcript = m.renderHelp(width, transcriptHeight)
	}
	view := tea.NewView(strings.Join([]string{transcript, selector, m.renderStateLine(width)}, "\n"))
	view.AltScreen = m.mode == ScreenFull
	view.MouseMode = tea.MouseModeNone
	view.ReportFocus = true
	view.WindowTitle = "pi-go"
	view.KeyboardEnhancements.ReportAlternateKeys = true
	if cursor := m.selector.Cursor(); cursor != nil {
		cursor.Position.Y += transcriptHeight
		view.Cursor = cursor
	}
	return view
}

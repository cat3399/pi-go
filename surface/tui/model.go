package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
)

type statusLevel uint8

const (
	statusInfo statusLevel = iota + 1
	statusSuccess
	statusWarning
	statusError
)

type modelStatus struct {
	text  string
	level statusLevel
}

type Model struct {
	ctx     context.Context
	api     application.API
	version string
	mode    ScreenMode
	theme   Theme

	sessionID string
	state     application.State
	revision  uint64

	sessionGeneration      uint64
	subscriptionGeneration uint64
	projectionGeneration   uint64
	snapshotInFlight       uint64
	snapshotNeeded         bool
	commandRequest         uint64
	commandApplied         uint64
	restoreQueueRequest    uint64
	commandsRequest        uint64
	openRequest            uint64

	subscription *application.EventSubscription
	closed       bool

	transcript         transcriptModel
	renderer           *contentRenderer
	composer           composerModel
	slashPalette       slashPaletteModel
	setClipboard       func(string) tea.Cmd
	selector           *selectorModel
	selectorCancel     context.CancelFunc
	selectorGeneration uint64

	width  int
	height int

	status              modelStatus
	statusGeneration    uint64
	statusExpiryPending bool
	helpVisible         bool
	localID             uint64

	liveItems       map[string]contentItem
	liveAssistantID string
}

func newModel(ctx context.Context, options Options, snapshot application.SessionSnapshot) (*Model, error) {
	if snapshot.SessionID == "" || snapshot.SessionID != options.SessionID {
		return nil, fmt.Errorf("TUI snapshot session %q does not match %q", snapshot.SessionID, options.SessionID)
	}
	theme := options.Theme
	if theme.ID == "" {
		theme = DefaultTheme()
	}
	state := application.State{SessionID: snapshot.SessionID, CWD: snapshot.Info.CWD}
	if snapshot.LiveState != nil {
		state = *snapshot.LiveState
	}
	model := &Model{
		ctx: ctx, api: options.Application, version: options.Version, mode: options.ScreenMode,
		theme: theme, sessionID: snapshot.SessionID, state: state, revision: snapshot.Revision,
		sessionGeneration: 1,
		transcript:        newTranscriptModel(), renderer: newContentRenderer(theme),
		composer: newComposerModel(theme), setClipboard: tea.SetClipboard, width: 80, height: 24,
		liveItems: make(map[string]contentItem),
	}
	model.transcript.SetItems(contentItemsFromSnapshot(snapshot))
	model.composer.SetWidth(model.width)
	model.slashPalette.SetCommands(mergeSlashCommands(nil))
	model.syncComposerState()
	return model, nil
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.composer.Init(), m.startSubscription(), m.requestCommands())
}

func (m *Model) Close() {
	if m == nil || m.closed {
		return
	}
	m.closed = true
	if m.selectorCancel != nil {
		m.selectorCancel()
		m.selectorCancel = nil
	}
	m.closeSubscription()
}

func (m *Model) Update(message tea.Msg) (updated tea.Model, command tea.Cmd) {
	if m == nil {
		return m, tea.Quit
	}
	defer func() {
		if expiry := m.takeStatusExpiry(); expiry != nil {
			command = tea.Batch(command, expiry)
		}
	}()
	var commands []tea.Cmd
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = max(1, message.Width)
		m.height = max(1, message.Height)
		m.composer.SetWidth(m.width)
		m.composer.SetMaxHeight(max(1, m.height-4))
		return m, nil
	case tea.KeyPressMsg:
		if m.selector != nil {
			return m, m.handleSelectorKey(message)
		}
		if handled, command := m.handleKey(message); handled {
			return m, command
		}
	case tea.MouseWheelMsg:
		switch message.Mouse().Button {
		case tea.MouseWheelUp:
			m.transcript.ScrollUp(3)
		case tea.MouseWheelDown:
			m.transcript.ScrollDown(3)
		}
		return m, nil
	case subscriptionReadyMsg:
		commands = append(commands, m.handleSubscription(message)...)
		return m, tea.Batch(commands...)
	case applicationEventMsg:
		if message.generation != m.subscriptionGeneration {
			return m, nil
		}
		if !message.ok {
			m.closeSubscription()
			return m, reconnectEventsCmd(m.subscriptionGeneration)
		}
		commands = append(commands, m.applyApplicationEvent(message.event)...)
		if m.subscription != nil {
			commands = append(commands, waitApplicationEventCmd(m.subscription, m.subscriptionGeneration))
		}
		return m, tea.Batch(commands...)
	case reconnectEventsMsg:
		if message.generation != m.subscriptionGeneration {
			return m, nil
		}
		return m, m.startSubscription()
	case retrySnapshotMsg:
		if message.sessionGeneration != m.sessionGeneration ||
			!m.snapshotNeeded || m.snapshotInFlight != 0 {
			return m, nil
		}
		return m, m.requestSnapshot()
	case statusExpiryMsg:
		if message.generation == m.statusGeneration {
			m.setStatus("", statusInfo)
		}
		return m, nil
	case stateLoadedMsg:
		if message.sessionID != m.sessionID ||
			message.sessionGeneration != m.sessionGeneration ||
			message.request != m.projectionGeneration {
			return m, nil
		}
		if message.err != nil {
			m.setStatus("State refresh failed: "+message.err.Error(), statusError)
		} else if message.active {
			return m, m.replaceState(message.state)
		}
		return m, nil
	case snapshotLoadedMsg:
		commands = append(commands, m.handleSnapshot(message)...)
		return m, tea.Batch(commands...)
	case commandsLoadedMsg:
		if message.sessionID == m.sessionID &&
			message.sessionGeneration == m.sessionGeneration &&
			message.request == m.commandsRequest && message.err == nil {
			m.slashPalette.SetCommands(mergeSlashCommands(message.commands))
			m.updateSlashPalette()
		}
		return m, nil
	case selectorLoadedMsg:
		return m, m.handleSelectorLoaded(message)
	case commandFinishedMsg:
		commands = append(commands, m.handleCommandFinished(message)...)
		return m, tea.Batch(commands...)
	case sessionOpenedMsg:
		commands = append(commands, m.handleSessionOpened(message)...)
		return m, tea.Batch(commands...)
	}

	if m.selector != nil {
		return m, m.selector.Update(message)
	}
	if command := m.composer.Update(message); command != nil {
		commands = append(commands, command)
	}
	m.updateSlashPalette()
	return m, tea.Batch(commands...)
}

func (m *Model) handleKey(message tea.KeyPressMsg) (bool, tea.Cmd) {
	keyName := message.String()
	if m.slashPalette.Visible() {
		switch keyName {
		case "up":
			m.slashPalette.Move(-1)
			return true, nil
		case "down":
			m.slashPalette.Move(1)
			return true, nil
		case "tab", "enter":
			if value, ok := m.slashPalette.Accept(); ok {
				m.composer.SetDraft(value, nil)
				return true, nil
			}
		case "esc":
			m.slashPalette.Dismiss()
			return true, nil
		}
	}
	switch keyName {
	case "ctrl+l":
		return true, m.openModelSelector("")
	case "up", "down":
		if m.composer.NavigateHistory(keyName) {
			m.updateSlashPalette()
			return true, nil
		}
	case "ctrl+o":
		m.renderer.SetToolsExpanded(!m.renderer.toolsExpanded)
		return true, nil
	case "alt+up":
		return true, m.restoreQueuedMessages()
	case "pgup", "ctrl+up":
		m.transcript.ScrollUp(max(1, m.transcript.lastHeight-2))
		return true, nil
	case "pgdown", "ctrl+down":
		m.transcript.ScrollDown(max(1, m.transcript.lastHeight-2))
		return true, nil
	case "ctrl+end":
		m.transcript.ScrollToBottom()
		return true, nil
	case "enter":
		return true, m.submit(false)
	case "alt+enter":
		return true, m.submit(true)
	case "esc":
		if m.helpVisible {
			m.helpVisible = false
			return true, nil
		}
		if m.busy() {
			return true, m.abort()
		}
		if !m.composer.Empty() {
			m.composer.Reset()
			m.updateSlashPalette()
			return true, nil
		}
	case "ctrl+c":
		if m.busy() {
			return true, m.abort()
		}
		if !m.composer.Empty() {
			m.composer.Reset()
			m.updateSlashPalette()
			return true, nil
		}
		return true, tea.Quit
	case "ctrl+d":
		if m.composer.Empty() {
			return true, tea.Quit
		}
	}
	return false, nil
}

func (m *Model) submit(followUp bool) tea.Cmd {
	defer m.updateSlashPalette()
	draft := strings.TrimSpace(m.composer.Value())
	draftImages := m.composer.Images()
	action, err := planRichInput(draft, draftImages, m.state, followUp)
	if err != nil {
		m.setStatus(err.Error(), statusWarning)
		return nil
	}
	switch action.kind {
	case inputActionQuit:
		m.composer.Reset()
		return tea.Quit
	case inputActionHelp:
		m.composer.Reset()
		m.helpVisible = !m.helpVisible
		return nil
	case inputActionNewSession:
		m.composer.Reset()
		m.setStatus("Creating a new session…", statusInfo)
		return m.createSession()
	case inputActionOpenSession:
		m.composer.Reset()
		m.setStatus("Opening session "+action.sessionID+"…", statusInfo)
		return m.openSession(action.sessionID)
	case inputActionModelSelector:
		m.composer.Reset()
		return m.openModelSelector(action.query)
	case inputActionSessionSelector:
		m.composer.Reset()
		return m.openSessionSelector("")
	case inputActionThinkingSelector:
		m.composer.Reset()
		return m.openThinkingSelector()
	case inputActionToolsSelector:
		m.composer.Reset()
		return m.openToolsSelector()
	case inputActionDispatch:
		switch action.command.(type) {
		case application.PromptCommand, application.BashCommand:
			if len(draftImages) == 0 {
				m.composer.AddToHistory(draft)
			}
		}
		m.composer.Reset()
		m.setStatus(commandPendingText(action.command), statusInfo)
		return m.dispatchCommand(action.command, draft, draftImages)
	default:
		m.setStatus("Unsupported input action", statusError)
		return nil
	}
}

func (m *Model) abort() tea.Cmd {
	var command application.Command = application.AbortCommand{}
	if m.state.IsBashRunning {
		command = application.AbortBashCommand{}
	} else if m.state.IsCompacting && !m.state.IsStreaming {
		command = application.AbortCompactionCommand{}
	}
	m.setStatus("Aborting…", statusWarning)
	return m.dispatchCommand(command, "", nil)
}

func (m *Model) restoreQueuedMessages() tea.Cmd {
	if m.restoreQueueRequest != 0 {
		m.setStatus("Queue restore is already in progress", statusWarning)
		return nil
	}
	if m.state.PendingMessageCount == 0 {
		m.setStatus("No queued messages to restore", statusWarning)
		return nil
	}
	command := m.dispatchCommand(
		application.ClearQueueCommand{}, m.composer.Value(), m.composer.Images(),
	)
	m.restoreQueueRequest = m.commandRequest
	m.setStatus("Restoring queued messages…", statusInfo)
	return command
}

func (m *Model) startSubscription() tea.Cmd {
	if m.subscription != nil {
		m.subscription.Close()
		m.subscription = nil
	}
	m.subscriptionGeneration++
	return subscribeEventsCmd(m.api, m.revision, m.subscriptionGeneration)
}

func (m *Model) closeSubscription() {
	if m == nil {
		return
	}
	m.subscriptionGeneration++
	if m.subscription != nil {
		m.subscription.Close()
		m.subscription = nil
	}
}

func (m *Model) requestState() tea.Cmd {
	m.projectionGeneration++
	return loadStateCmd(m.api, m.sessionID, m.sessionGeneration, m.projectionGeneration)
}

func (m *Model) requestSnapshot() tea.Cmd {
	m.projectionGeneration++
	m.snapshotInFlight = m.projectionGeneration
	m.snapshotNeeded = true
	return loadSnapshotCmd(m.api, m.sessionID, m.sessionGeneration, m.projectionGeneration)
}

func (m *Model) requestCommands() tea.Cmd {
	// Dynamic commands are session/resource scoped. Drop the previous set
	// before loading so a removed command cannot be accepted in the gap.
	m.slashPalette.SetCommands(mergeSlashCommands(nil))
	m.updateSlashPalette()
	m.commandsRequest++
	return loadCommandsCmd(m.ctx, m.api, m.sessionID, m.sessionGeneration, m.commandsRequest)
}

func (m *Model) dispatchCommand(command application.Command, draft string, draftImages []llm.ImageBlock) tea.Cmd {
	m.commandRequest++
	return dispatchCommandCmd(
		m.ctx, m.api, m.sessionID, m.sessionGeneration, m.commandRequest,
		command, draft, draftImages,
	)
}

func (m *Model) createSession() tea.Cmd {
	m.openRequest++
	return createSessionCmd(m.ctx, m.api, m.state, m.sessionGeneration, m.openRequest)
}

func (m *Model) openSession(sessionID string) tea.Cmd {
	m.openRequest++
	return openSessionCmd(m.ctx, m.api, sessionID, m.sessionGeneration, m.openRequest)
}

func (m *Model) busy() bool {
	return m.state.IsPromptRunning || m.state.IsStreaming || m.state.IsBashRunning || m.state.IsCompacting
}

func (m *Model) syncComposerState() {
	m.composer.SetBusy(m.busy())
	m.updateSlashPalette()
}

func (m *Model) replaceState(state application.State) tea.Cmd {
	previous := m.state
	m.state = state
	m.syncComposerState()
	return m.refreshSelectorForStateChange(previous, m.state)
}

func (m *Model) updateSlashPalette() {
	if m.composer.HasImages() {
		m.slashPalette.Hide(m.composer.Value())
		return
	}
	m.slashPalette.Update(m.composer.Value())
}

func (m *Model) setStatus(text string, level statusLevel) {
	m.statusGeneration++
	m.statusExpiryPending = false
	m.status = modelStatus{text: strings.TrimSpace(text), level: level}
	if m.status.text != "" && !m.busy() && level != statusError {
		m.statusExpiryPending = true
	}
}

func (m *Model) takeStatusExpiry() tea.Cmd {
	if m == nil || !m.statusExpiryPending {
		return nil
	}
	m.statusExpiryPending = false
	return expireStatusCmd(m.statusGeneration)
}

func (m *Model) handleSubscription(message subscriptionReadyMsg) []tea.Cmd {
	if message.generation != m.subscriptionGeneration {
		if message.subscription != nil {
			message.subscription.Close()
		}
		return nil
	}
	if message.err != nil {
		if errors.Is(message.err, application.ErrEventCursorUnavailable) {
			m.setStatus("Event history moved; refreshing session snapshot…", statusWarning)
			return []tea.Cmd{m.requestSnapshot()}
		}
		m.setStatus("Event subscription failed: "+message.err.Error(), statusError)
		return []tea.Cmd{reconnectEventsCmd(m.subscriptionGeneration)}
	}
	if message.subscription == nil {
		m.setStatus("Event subscription returned no stream", statusError)
		return []tea.Cmd{reconnectEventsCmd(m.subscriptionGeneration)}
	}
	if m.subscription != nil {
		m.subscription.Close()
	}
	m.subscription = message.subscription
	commands := make([]tea.Cmd, 0)
	for _, event := range message.subscription.Replay {
		commands = append(commands, m.applyApplicationEvent(event)...)
	}
	if message.subscription.Revision > m.revision {
		m.revision = message.subscription.Revision
	}
	commands = append(commands, waitApplicationEventCmd(message.subscription, message.generation))
	return commands
}

func (m *Model) handleSnapshot(message snapshotLoadedMsg) []tea.Cmd {
	if message.sessionID != m.sessionID ||
		message.sessionGeneration != m.sessionGeneration {
		return nil
	}
	if message.request != m.projectionGeneration {
		// A state-only refresh may supersede a snapshot while the snapshot is
		// still needed to replace ephemeral transcript items with durable ones.
		// Retry only for the latest in-flight snapshot so multiple stale results
		// cannot fan out into concurrent refreshes.
		if m.snapshotNeeded && message.request == m.snapshotInFlight {
			return []tea.Cmd{m.requestSnapshot()}
		}
		return nil
	}
	if message.err != nil {
		m.snapshotInFlight = 0
		m.setStatus("Snapshot refresh failed: "+message.err.Error(), statusError)
		return []tea.Cmd{
			reconnectEventsCmd(m.subscriptionGeneration),
			retrySnapshotCmd(m.sessionGeneration),
		}
	}
	m.transcript.SetItems(contentItemsFromSnapshot(message.snapshot))
	m.liveItems = make(map[string]contentItem)
	m.liveAssistantID = ""
	m.snapshotNeeded = false
	m.snapshotInFlight = 0
	var selectorRefresh tea.Cmd
	if message.snapshot.LiveState != nil {
		selectorRefresh = m.replaceState(*message.snapshot.LiveState)
	} else {
		m.syncComposerState()
	}
	m.revision = message.snapshot.Revision
	commands := []tea.Cmd{m.startSubscription()}
	if selectorRefresh != nil {
		commands = append(commands, selectorRefresh)
	}
	return commands
}

func (m *Model) handleSessionOpened(message sessionOpenedMsg) []tea.Cmd {
	if message.sourceGeneration != m.sessionGeneration || message.request != m.openRequest {
		return nil
	}
	if message.err != nil {
		m.setStatus("Open session failed: "+message.err.Error(), statusError)
		return nil
	}
	if message.state.SessionID == "" || message.snapshot.SessionID != message.state.SessionID {
		m.setStatus("Opened session returned an inconsistent snapshot", statusError)
		return nil
	}
	commands := make([]tea.Cmd, 0, 3)
	if m.selector != nil {
		commands = append(commands, m.closeSelector())
	}
	m.closeSubscription()
	m.sessionGeneration++
	m.projectionGeneration++
	m.snapshotNeeded = false
	m.snapshotInFlight = 0
	m.sessionID = message.state.SessionID
	m.state = message.state
	m.revision = message.snapshot.Revision
	m.transcript.SetItems(contentItemsFromSnapshot(message.snapshot))
	m.transcript.ScrollToBottom()
	m.liveItems = make(map[string]contentItem)
	m.liveAssistantID = ""
	m.syncComposerState()
	m.setStatus("Opened session "+shortID(m.sessionID), statusSuccess)
	commands = append(commands, m.startSubscription(), m.requestCommands())
	return commands
}

func (m *Model) View() tea.View {
	width, height := max(1, m.width), max(1, m.height)
	if width < 20 || height < 5 || (m.selector == nil && m.composer.HasImages() && height < 6) {
		content := Truncate("pi-go: terminal too small", width, "…", false)
		if height > 1 {
			content += strings.Repeat("\n", height-1)
		}
		view := tea.NewView(content)
		view.AltScreen = m.mode == ScreenFull
		view.WindowTitle = "pi-go"
		return view
	}
	if m.selector != nil {
		return m.renderSelectorView(width, height)
	}
	composer := m.composer.View()
	composerHeight := lipgloss.Height(composer)
	dockBudget := max(0, height-composerHeight-2)
	paletteLines := m.renderSlashPalette(width, min(5, dockBudget))
	dockBudget -= len(paletteLines)
	maxQueueLines := max(0, min(3, dockBudget))
	queueLines := m.renderQueueDock(width, maxQueueLines)
	transcriptHeight := max(1, height-len(queueLines)-len(paletteLines)-composerHeight-1)

	transcript := m.transcript.View(width, transcriptHeight, m.renderer)
	if m.helpVisible {
		transcript = m.renderHelp(width, transcriptHeight)
	}
	parts := []string{transcript}
	parts = append(parts, queueLines...)
	parts = append(parts, paletteLines...)
	parts = append(parts, composer, m.renderStateLine(width))
	view := tea.NewView(strings.Join(parts, "\n"))
	view.AltScreen = m.mode == ScreenFull
	// Leave the mouse to the terminal so users can select arbitrary visible
	// output and copy it with their terminal's normal shortcut. Bubble Tea's
	// cell-motion mode captures drag events even though this surface only used
	// mouse input for scrolling.
	view.MouseMode = tea.MouseModeNone
	view.ReportFocus = true
	view.WindowTitle = "pi-go"
	view.KeyboardEnhancements.ReportAlternateKeys = true

	if !m.helpVisible {
		cursor := m.composer.Cursor()
		if cursor != nil {
			cursor.Position.Y += transcriptHeight + len(queueLines) + len(paletteLines)
			view.Cursor = cursor
		}
	}
	return view
}

func (m *Model) renderHelp(width, height int) string {
	lines := []string{
		m.theme.titleStyle(contentRoleSystem, false).Render("pi-go TUI"),
		"",
		"Enter              send / steer while streaming",
		"Alt+Enter          queue a follow-up",
		"Shift+Enter        insert a newline",
		"Esc                abort current operation",
		"PgUp / PgDn        scroll conversation",
		"Ctrl+End           follow live output",
		"Ctrl+O             collapse / expand tool output",
		"Ctrl+L             open model selector",
		"Up / Down          browse prompt history at editor edges",
		"Alt+Up             restore queued messages",
		"Ctrl+D             quit when editor is empty",
		"",
		"/help /hotkeys     show this page",
		"/new               create a session",
		"/resume [id]       select or open a session",
		"/model [p/id]      select or switch model",
		"/thinking [level]  select reasoning level",
		"/compact [text]    compact context",
		"/reload            reload resources",
		"/copy              copy the last assistant reply",
		"/tools             configure active tools",
		"!cmd / !!cmd       bash in/out of model context",
	}
	result := make([]string, 0, height)
	for _, line := range lines {
		result = append(result, Wrap(line, width)...)
	}
	if len(result) > height {
		result = result[:height]
	}
	if len(result) < height {
		result = append(result, make([]string, height-len(result))...)
	}
	return strings.Join(result, "\n")
}

func shortID(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func commandPendingText(command application.Command) string {
	if command == nil {
		return "Working…"
	}
	switch command.Type() {
	case application.CommandPrompt:
		return "Sending…"
	case application.CommandBash:
		return "Running bash…"
	case application.CommandCompact:
		return "Compacting…"
	case application.CommandReload:
		return "Reloading…"
	case application.CommandGetLastAssistantText:
		return "Copying last assistant reply…"
	default:
		return strings.ReplaceAll(string(command.Type()), "_", " ") + "…"
	}
}

var _ tea.Model = (*Model)(nil)

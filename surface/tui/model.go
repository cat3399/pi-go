package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/cat3399/pi-go/internal/application"
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
	openRequest            uint64

	subscription *application.EventSubscription
	closed       bool

	transcript transcriptModel
	renderer   *contentRenderer
	composer   composerModel

	width  int
	height int

	status      modelStatus
	helpVisible bool
	localID     uint64

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
		composer: newComposerModel(theme), width: 80, height: 24,
		liveItems: make(map[string]contentItem),
	}
	model.transcript.SetItems(contentItemsFromSnapshot(snapshot))
	model.composer.SetWidth(model.width)
	model.syncComposerState()
	return model, nil
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.composer.Init(), m.startSubscription())
}

func (m *Model) Close() {
	if m == nil || m.closed {
		return
	}
	m.closed = true
	m.closeSubscription()
}

func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if m == nil {
		return m, tea.Quit
	}
	var commands []tea.Cmd
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = max(1, message.Width)
		m.height = max(1, message.Height)
		m.composer.SetWidth(m.width)
		m.composer.SetMaxHeight(max(1, m.height-6))
		return m, nil
	case tea.KeyPressMsg:
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
	case stateLoadedMsg:
		if message.sessionID != m.sessionID ||
			message.sessionGeneration != m.sessionGeneration ||
			message.request != m.projectionGeneration {
			return m, nil
		}
		if message.err != nil {
			m.setStatus("State refresh failed: "+message.err.Error(), statusError)
		} else if message.active {
			m.state = message.state
			m.syncComposerState()
		}
		return m, nil
	case snapshotLoadedMsg:
		commands = append(commands, m.handleSnapshot(message)...)
		return m, tea.Batch(commands...)
	case commandFinishedMsg:
		commands = append(commands, m.handleCommandFinished(message)...)
		return m, tea.Batch(commands...)
	case sessionOpenedMsg:
		commands = append(commands, m.handleSessionOpened(message)...)
		return m, tea.Batch(commands...)
	}

	if command := m.composer.Update(message); command != nil {
		commands = append(commands, command)
	}
	return m, tea.Batch(commands...)
}

func (m *Model) handleKey(message tea.KeyPressMsg) (bool, tea.Cmd) {
	keyName := message.String()
	switch keyName {
	case "ctrl+g":
		m.helpVisible = !m.helpVisible
		return true, nil
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
		if strings.TrimSpace(m.composer.Value()) != "" {
			m.composer.Reset()
			return true, nil
		}
	case "ctrl+c":
		if m.busy() {
			return true, m.abort()
		}
		if strings.TrimSpace(m.composer.Value()) != "" {
			m.composer.Reset()
			return true, nil
		}
		return true, tea.Quit
	case "ctrl+d":
		if strings.TrimSpace(m.composer.Value()) == "" {
			return true, tea.Quit
		}
	}
	return false, nil
}

func (m *Model) submit(followUp bool) tea.Cmd {
	draft := strings.TrimSpace(m.composer.Value())
	action, err := planInput(draft, m.state, followUp)
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
	case inputActionDispatch:
		m.composer.Reset()
		m.setStatus(commandPendingText(action.command), statusInfo)
		return m.dispatchCommand(action.command, draft)
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
	return m.dispatchCommand(command, "")
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

func (m *Model) dispatchCommand(command application.Command, draft string) tea.Cmd {
	m.commandRequest++
	return dispatchCommandCmd(
		m.ctx, m.api, m.sessionID, m.sessionGeneration, m.commandRequest, command, draft,
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
}

func (m *Model) setStatus(text string, level statusLevel) {
	m.status = modelStatus{text: strings.TrimSpace(text), level: level}
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
	if message.snapshot.LiveState != nil {
		m.state = *message.snapshot.LiveState
	}
	m.revision = message.snapshot.Revision
	m.syncComposerState()
	return []tea.Cmd{m.startSubscription()}
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
	return []tea.Cmd{m.startSubscription()}
}

func (m *Model) View() tea.View {
	width, height := max(1, m.width), max(1, m.height)
	if width < 20 || height < 6 {
		content := Truncate("pi-go: terminal too small", width, "…", false)
		if height > 1 {
			content += strings.Repeat("\n", height-1)
		}
		view := tea.NewView(content)
		view.AltScreen = m.mode == ScreenFull
		view.WindowTitle = "pi-go"
		return view
	}
	headerLines := m.renderHeader(width)
	footerLines := 1
	statusLines := 1
	if height < 8 {
		headerLines = headerLines[:1]
		footerLines = 0
	}
	composer := m.composer.View()
	composerHeight := lipgloss.Height(composer)
	transcriptHeight := max(1, height-len(headerLines)-statusLines-footerLines-composerHeight)

	transcript := m.transcript.View(width, transcriptHeight, m.renderer)
	if m.helpVisible {
		transcript = m.renderHelp(width, transcriptHeight)
	}
	parts := append([]string(nil), headerLines...)
	parts = append(parts, transcript, m.renderStatus(width), composer)
	if footerLines != 0 {
		parts = append(parts, m.renderFooter(width))
	}
	view := tea.NewView(strings.Join(parts, "\n"))
	view.AltScreen = m.mode == ScreenFull
	view.MouseMode = tea.MouseModeCellMotion
	view.ReportFocus = true
	view.WindowTitle = "pi-go"
	view.KeyboardEnhancements.ReportAlternateKeys = true

	if !m.helpVisible {
		cursor := m.composer.Cursor()
		if cursor != nil {
			cursor.Position.Y += len(headerLines) + transcriptHeight + statusLines
			view.Cursor = cursor
		}
	}
	return view
}

func (m *Model) renderHeader(width int) []string {
	name := "pi-go"
	if m.version != "" {
		name += " " + m.version
	}
	sessionName := shortID(m.sessionID)
	if m.state.SessionName != nil && strings.TrimSpace(*m.state.SessionName) != "" {
		sessionName = strings.TrimSpace(*m.state.SessionName)
	}
	model := "no model"
	if m.state.HasModel {
		model = m.state.Model.Provider() + "/" + m.state.Model.ID()
	}
	left := lipgloss.NewStyle().Bold(true).Foreground(m.theme.color(m.theme.Primary)).Render("π " + name)
	line := left + m.theme.mutedStyle().Render("  "+sessionName+"  "+model+"  thinking:"+string(m.state.ThinkingLevel))
	cwd := m.state.CWD
	if cwd == "" {
		cwd = m.api.DefaultCWD()
	}
	project := filepath.Base(cwd)
	if project == "." || project == string(filepath.Separator) {
		project = cwd
	}
	second := m.theme.subtleStyle().Render(project + "  " + cwd)
	return []string{Truncate(line, width, "…", true), Truncate(second, width, "…", true)}
}

func (m *Model) renderStatus(width int) string {
	text := m.status.text
	level := m.status.level
	if text == "" {
		phase := m.state.Phase.String()
		text = phase
		if m.state.PendingMessageCount > 0 {
			text += fmt.Sprintf(" · queued %d", m.state.PendingMessageCount)
		}
		if m.state.ContextUsage != nil && m.state.ContextUsage.Percent != nil {
			text += fmt.Sprintf(" · context %.1f%%", *m.state.ContextUsage.Percent)
		}
		if !m.transcript.Following() {
			text += " · scrolled"
		}
		level = statusInfo
	}
	color := m.theme.Muted
	switch level {
	case statusSuccess:
		color = m.theme.Success
	case statusWarning:
		color = m.theme.Warning
	case statusError:
		color = m.theme.Danger
	}
	return Truncate(lipgloss.NewStyle().Foreground(m.theme.color(color)).Render(" "+text), width, "…", true)
}

func (m *Model) renderFooter(width int) string {
	text := " Enter send  Shift+Enter newline  Alt+Enter follow-up  Esc abort  PgUp/PgDn scroll  Ctrl+G help  Ctrl+D quit "
	return Truncate(m.theme.subtleStyle().Render(text), width, "", true)
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
		"Ctrl+D             quit when editor is empty",
		"",
		"/new               create a session",
		"/resume <id>       open a session",
		"/model p/id        switch model",
		"/thinking <level>  switch reasoning level",
		"/compact [text]    compact context",
		"/reload            reload resources",
		"/tools             show tool state",
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
	default:
		return strings.ReplaceAll(string(command.Type()), "_", " ") + "…"
	}
}

var _ tea.Model = (*Model)(nil)

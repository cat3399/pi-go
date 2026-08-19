package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/cat3399/pi-go/internal/agent"
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
	ctx           context.Context
	api           application.API
	version       string
	mode          ScreenMode
	theme         Theme
	themeSetting  string
	themeAuto     bool
	environment   []string
	keybindings   appKeybindings
	initialPrompt string

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
	branchSummaryRequest   uint64
	commandsRequest        uint64
	openRequest            uint64

	subscription *application.EventSubscription
	closed       bool

	transcript               transcriptModel
	renderer                 *contentRenderer
	composer                 composerModel
	slashPalette             slashPaletteModel
	fileCompletion           fileCompletionModel
	fileCompletionGeneration uint64
	setClipboard             func(string) tea.Cmd
	openURL                  func(string) error
	mouseSelection           *transcriptSelection
	mouseAutoGeneration      uint64
	mouseAutoDirection       int
	mouseAutoPointer         tea.Mouse
	lastScreen               []string
	selector                 *selectorModel
	selectorCancel           context.CancelFunc
	selectorGeneration       uint64
	treeTargetID             string
	snapshotLeafID           *string
	readClipboardImage       func(context.Context) (llm.ImageBlock, error)
	clipboardInFlight        bool
	pendingImportPath        string
	pendingSessionID         string
	sessionNamedOnly         bool
	sessionSortMode          string
	sessionShowPath          bool
	authProviders            map[string]application.ProviderAuthInfo
	pendingAuthID            string
	pendingAuthType          string
	authGeneration           uint64
	oauthLogin               *application.ProviderOAuthLogin

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
	themeSetting, err := ParseThemeSetting(options.ThemeSetting)
	if err != nil || strings.TrimSpace(options.ThemeSetting) == "" {
		if theme.IsLight {
			themeSetting = ThemeLight
		} else {
			themeSetting = ThemeDark
		}
	}
	state := application.State{SessionID: snapshot.SessionID, CWD: snapshot.Info.CWD}
	if snapshot.LiveState != nil {
		state = *snapshot.LiveState
	}
	keybindings, _ := loadAppKeybindings(options.Application.AgentDir())
	model := &Model{
		ctx: ctx, api: options.Application, version: options.Version, mode: options.ScreenMode,
		theme: theme, themeSetting: themeSetting, themeAuto: themeSetting == ThemeAuto,
		sessionID: snapshot.SessionID, state: state, revision: snapshot.Revision,
		environment: append([]string(nil), options.Environment...), keybindings: keybindings,
		initialPrompt:     strings.TrimSpace(options.InitialPrompt),
		sessionGeneration: 1,
		transcript:        newTranscriptModel(), renderer: newContentRenderer(theme),
		composer: newComposerModel(theme), setClipboard: tea.SetClipboard, width: 80, height: 24,
		liveItems: make(map[string]contentItem), snapshotLeafID: cloneString(snapshot.LeafID),
	}
	model.openURL = options.OpenURL
	if model.openURL == nil {
		model.openURL = openURLWithSystem
	}
	model.readClipboardImage = options.ReadClipboardImage
	if model.readClipboardImage == nil {
		model.readClipboardImage = readSystemClipboardImage
	}
	model.renderer.SetImageProtocol(detectTerminalImageProtocol(options.Environment))
	model.composer.SetKeybindings(keybindings)
	model.transcript.SetItems(contentItemsFromSnapshot(snapshot))
	model.composer.SetWidth(model.width)
	model.slashPalette.SetCommands(mergeSlashCommands(nil))
	model.syncComposerState()
	return model, nil
}

func (m *Model) Init() tea.Cmd {
	commands := []tea.Cmd{m.composer.Init(), m.startSubscription(), m.requestCommands()}
	if m.themeAuto {
		commands = append(commands, tea.RequestBackgroundColor)
	}
	if m.initialPrompt != "" {
		m.composer.SetDraft(m.initialPrompt, nil)
		m.initialPrompt = ""
		commands = append(commands, m.submit(false))
	}
	return tea.Batch(commands...)
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
	if m.oauthLogin != nil {
		m.oauthLogin.Close()
		m.oauthLogin = nil
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
	case tea.BackgroundColorMsg:
		if m.themeAuto && message.Color != nil {
			if message.IsDark() {
				m.applyTheme(DefaultTheme())
			} else {
				m.applyTheme(LightTheme())
			}
		}
		return m, nil
	case tea.BlurMsg:
		if m.mouseSelection != nil && m.mouseSelection.active {
			m.mouseSelection = nil
		}
		m.stopSelectionAutoScroll()
		return m, nil
	case themeChangedMsg:
		return m, m.applyThemeChanged(message)
	case tea.KeyPressMsg:
		if m.selector != nil {
			return m, m.handleSelectorKey(message)
		}
		if handled, command := m.handleKey(message); handled {
			return m, command
		}
	case tea.MouseClickMsg:
		return m, m.handleMouseClick(message.Mouse())
	case tea.MouseMotionMsg:
		return m, m.handleMouseMotion(message.Mouse())
	case tea.MouseReleaseMsg:
		return m, m.handleMouseRelease(message.Mouse())
	case tea.MouseWheelMsg:
		return m, m.handleMouseWheel(message.Mouse())
	case selectionAutoScrollMsg:
		return m, m.handleSelectionAutoScroll(message)
	case urlOpenedMsg:
		if message.err != nil {
			m.setStatus("Open link failed: "+message.err.Error(), statusError)
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
	case clipboardImageMsg:
		m.clipboardInFlight = false
		if message.err != nil {
			m.setStatus(message.err.Error(), statusWarning)
			return m, nil
		}
		images := append(m.composer.Images(), message.image)
		m.composer.SetDraft(m.composer.Value(), images)
		m.updateSlashPalette()
		m.setStatus(fmt.Sprintf("Attached image · %s", formatBytes(len(message.image.Data()))), statusSuccess)
		return m, nil
	case fileCompletionsLoadedMsg:
		m.handleFileCompletionLoaded(message)
		return m, nil
	case externalEditorFinishedMsg:
		if message.err != nil {
			m.setStatus("External editor failed; draft preserved at "+message.path+": "+message.err.Error(), statusError)
			return m, nil
		}
		data, err := os.ReadFile(message.path)
		if err != nil {
			m.setStatus("Read external editor draft failed; file preserved at "+message.path+": "+err.Error(), statusError)
			return m, nil
		}
		value := strings.TrimSuffix(string(data), "\n")
		m.composer.SetDraft(value, m.composer.Images())
		m.updateSlashPalette()
		m.setStatus("Draft updated from external editor", statusSuccess)
		return m, m.refreshFileCompletion()
	case commandFinishedMsg:
		commands = append(commands, m.handleCommandFinished(message)...)
		return m, tea.Batch(commands...)
	case sessionOpenedMsg:
		commands = append(commands, m.handleSessionOpened(message)...)
		return m, tea.Batch(commands...)
	case sessionExportedMsg:
		if message.err != nil {
			m.setStatus("Export failed: "+message.err.Error(), statusError)
		} else {
			m.setStatus("Session exported to "+message.path, statusSuccess)
		}
		return m, nil
	case projectTrustMsg:
		if message.err != nil {
			m.setStatus("Project trust failed: "+message.err.Error(), statusError)
			return m, nil
		}
		if message.applied {
			m.setStatus("Project trusted; reopening session…", statusSuccess)
			return m, m.openSession(m.sessionID)
		}
		switch {
		case !message.status.RequiresTrust:
			m.setStatus("This project has no resources that require trust", statusInfo)
		case message.status.Trusted:
			m.setStatus("Project is already trusted", statusSuccess)
		default:
			return m, m.openTrustConfirm()
		}
		return m, nil
	case providerAuthMutationMsg:
		if message.generation != m.authGeneration {
			return m, nil
		}
		if message.err != nil {
			m.setStatus(message.operation+" failed: "+message.err.Error(), statusError)
			return m, nil
		}
		m.setStatus(message.operation+" completed for "+message.providerID, statusSuccess)
		return m, nil
	case providerOAuthStartedMsg:
		if message.generation != m.authGeneration {
			if message.login != nil {
				message.login.Close()
			}
			return m, nil
		}
		if message.err != nil || message.login == nil {
			if message.err == nil {
				message.err = errors.New("OAuth login did not start")
			}
			m.setStatus("Login failed: "+message.err.Error(), statusError)
			return m, nil
		}
		m.oauthLogin = message.login
		focus := m.openOAuthCallback(message.providerID, message.login)
		return m, tea.Batch(focus, waitProviderOAuthCmd(
			m.ctx, message.login, message.providerID, message.generation,
		))
	case providerOAuthFinishedMsg:
		if message.generation != m.authGeneration || message.login != m.oauthLogin {
			return m, nil
		}
		m.oauthLogin = nil
		var focus tea.Cmd
		if m.selector != nil && m.selector.kind == selectorLoginOAuth {
			focus = m.closeSelector()
		}
		if message.err != nil {
			m.setStatus("OAuth login failed: "+message.err.Error(), statusError)
		} else {
			m.setStatus("Login completed for "+message.providerID, statusSuccess)
		}
		return m, focus
	case sessionMutationMsg:
		if message.sessionGeneration != m.sessionGeneration {
			return m, nil
		}
		if message.err != nil {
			m.setStatus(message.operation+" failed: "+message.err.Error(), statusError)
			return m, nil
		}
		m.setStatus(message.operation+" completed", statusSuccess)
		if m.selector != nil && m.selector.kind == selectorSessions {
			return m, m.refreshSelector()
		}
		return m, nil
	}

	if m.selector != nil {
		return m, m.selector.Update(message)
	}
	if command := m.composer.Update(message); command != nil {
		commands = append(commands, command)
	}
	m.updateSlashPalette()
	if command := m.refreshFileCompletion(); command != nil {
		commands = append(commands, command)
	}
	return m, tea.Batch(commands...)
}

func (m *Model) handleKey(message tea.KeyPressMsg) (bool, tea.Cmd) {
	matches := func(action string) bool { return m.keybindings.MatchesPress(action, message) }
	if m.fileCompletion.Active() {
		switch {
		case matches(keySelectUp):
			m.fileCompletion.Move(-1)
			return true, nil
		case matches(keySelectDown):
			m.fileCompletion.Move(1)
			return true, nil
		case matches(keyInputTab), matches(keySelectConfirm):
			if _, ok := m.fileCompletion.Selected(); ok {
				return true, m.acceptFileCompletion()
			}
		case matches(keySelectCancel):
			m.fileCompletion.Dismiss()
			return true, nil
		}
	}
	if m.slashPalette.Visible() {
		switch {
		case matches(keySelectUp):
			m.slashPalette.Move(-1)
			return true, nil
		case matches(keySelectDown):
			m.slashPalette.Move(1)
			return true, nil
		case matches(keyInputTab), matches(keySelectConfirm):
			if value, ok := m.slashPalette.Accept(); ok {
				m.composer.SetDraft(value, nil)
				return true, nil
			}
		case matches(keySelectCancel):
			m.slashPalette.Dismiss()
			return true, nil
		}
	}
	switch {
	case matches(keySuspend):
		if runtime.GOOS == "windows" {
			m.setStatus("Process suspension is not supported on Windows", statusWarning)
			return true, nil
		}
		return true, tea.Suspend
	case matches(keySessionNew):
		m.setStatus("Creating a new session…", statusInfo)
		return true, m.createSession()
	case matches(keySessionResume):
		return true, m.openSessionSelector("")
	case matches(keySessionTree):
		return true, m.openTreeSelector()
	case matches(keySessionFork):
		return true, m.openForkSelector()
	case matches(keyModelSelect):
		return true, m.openModelSelector("")
	case matches(keyModelForward):
		m.setStatus("Cycling model…", statusInfo)
		return true, m.dispatchCommand(application.CycleModelCommand{Direction: agent.CycleForward}, "", nil)
	case matches(keyModelBackward):
		m.setStatus("Cycling model backward…", statusInfo)
		return true, m.dispatchCommand(application.CycleModelCommand{Direction: agent.CycleBackward}, "", nil)
	case matches(keyThinkingCycle):
		m.setStatus("Cycling thinking level…", statusInfo)
		return true, m.dispatchCommand(application.CycleThinkingLevelCommand{}, "", nil)
	case matches(keyThinkingToggle):
		m.renderer.SetThinkingVisible(!m.renderer.thinkingVisible)
		if m.renderer.thinkingVisible {
			m.setStatus("Thinking content shown", statusSuccess)
		} else {
			m.setStatus("Thinking content hidden", statusSuccess)
		}
		return true, nil
	case matches(keyMessageCopy):
		m.setStatus("Copying last assistant reply…", statusInfo)
		return true, m.dispatchCommand(application.GetLastAssistantTextCommand{}, "", nil)
	case matches(keyPasteImage):
		if m.clipboardInFlight {
			m.setStatus("Reading clipboard image…", statusInfo)
			return true, nil
		}
		m.clipboardInFlight = true
		m.setStatus("Reading clipboard image…", statusInfo)
		return true, readClipboardImageCmd(m.ctx, m.readClipboardImage)
	case matches(keyExternalEditor):
		command, path, err := prepareExternalEditor(
			m.environment, m.state.CWD, m.composer.Value(),
		)
		if err != nil {
			m.setStatus("External editor failed: "+err.Error(), statusError)
			return true, nil
		}
		m.fileCompletion.Hide()
		m.slashPalette.Hide(m.composer.Value())
		m.setStatus("Opening external editor…", statusInfo)
		return true, tea.ExecProcess(command, func(err error) tea.Msg {
			return externalEditorFinishedMsg{path: path, err: err}
		})
	case matches(keyEditorCursorUp):
		if m.composer.NavigateHistory("up") {
			m.updateSlashPalette()
			return true, nil
		}
	case matches(keyEditorCursorDown):
		if m.composer.NavigateHistory("down") {
			m.updateSlashPalette()
			return true, nil
		}
	case matches(keyToolsExpand):
		m.renderer.SetToolsExpanded(!m.renderer.toolsExpanded)
		return true, nil
	case matches(keyMessageDequeue):
		return true, m.restoreQueuedMessages(false)
	case matches(keyViewportPageUp):
		m.transcript.ScrollUp(max(1, m.transcript.lastHeight-2))
		return true, nil
	case matches(keyViewportPageDown):
		m.transcript.ScrollDown(max(1, m.transcript.lastHeight-2))
		return true, nil
	case matches(keyViewportTop):
		m.transcript.ScrollToTop()
		return true, nil
	case matches(keyViewportBottom):
		m.transcript.ScrollToBottom()
		return true, nil
	case matches(keyViewportPrevious):
		m.transcript.ScrollToPreviousPrompt()
		return true, nil
	case matches(keyViewportNext):
		m.transcript.ScrollToNextPrompt()
		return true, nil
	case matches(keyInputSubmit):
		return true, m.submit(false)
	case matches(keyMessageFollowUp):
		return true, m.submit(true)
	case matches(keyInterrupt):
		if m.helpVisible {
			m.helpVisible = false
			return true, nil
		}
		if m.state.RetryWaiting {
			m.setStatus("Cancelling retry…", statusWarning)
			return true, m.dispatchCommand(application.AbortRetryCommand{}, "", nil)
		}
		if m.busy() {
			if m.state.IsPromptRunning || m.state.IsStreaming {
				return true, m.restoreQueuedMessages(true)
			}
			return true, m.abort()
		}
		if !m.composer.Empty() {
			m.composer.Reset()
			m.updateSlashPalette()
			return true, nil
		}
	case matches(keyClear):
		if m.busy() {
			return true, m.abort()
		}
		if !m.composer.Empty() {
			m.composer.Reset()
			m.updateSlashPalette()
			return true, nil
		}
		return true, tea.Quit
	case matches(keyExit):
		if m.composer.Empty() {
			return true, tea.Quit
		}
	}
	if m.composer.HandleEditingKey(message, m.keybindings) {
		m.updateSlashPalette()
		return true, nil
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
	case inputActionSettingsSelector:
		m.composer.Reset()
		return m.openSettingsSelector()
	case inputActionExport:
		m.composer.Reset()
		m.setStatus("Exporting session…", statusInfo)
		return exportSessionCmd(m.ctx, m.api, m.sessionID, m.state.CWD, action.path)
	case inputActionImport:
		m.composer.Reset()
		return m.openImportConfirm(action.path)
	case inputActionTrust:
		m.composer.Reset()
		m.setStatus("Checking project trust…", statusInfo)
		return projectTrustStatusCmd(m.ctx, m.api, m.state.CWD)
	case inputActionLogin:
		m.composer.Reset()
		return m.openLoginSelector(action.query)
	case inputActionLogout:
		m.composer.Reset()
		return m.openLogoutSelector(action.query)
	case inputActionTreeSelector:
		m.composer.Reset()
		return m.openTreeSelector()
	case inputActionForkSelector:
		m.composer.Reset()
		return m.openForkSelector()
	case inputActionClone:
		m.composer.Reset()
		return m.cloneCurrentSession()
	case inputActionDispatch:
		if _, abort := action.command.(application.AbortCommand); abort {
			m.composer.Reset()
			return m.abort()
		}
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
	if m.branchSummaryRequest != 0 {
		command = application.AbortBranchSummaryCommand{}
	} else if m.state.IsBashRunning {
		command = application.AbortBashCommand{}
	} else if m.state.IsCompacting && !m.state.IsStreaming {
		command = application.AbortCompactionCommand{}
	}
	m.setStatus("Aborting…", statusWarning)
	return m.dispatchCommand(command, "", nil)
}

func (m *Model) restoreQueuedMessages(abort bool) tea.Cmd {
	if m.restoreQueueRequest != 0 {
		m.setStatus("Queue restore is already in progress", statusWarning)
		return nil
	}
	if !abort && m.state.PendingMessageCount == 0 {
		m.setStatus("No queued messages to restore", statusWarning)
		return nil
	}
	m.commandRequest++
	command := restoreQueuedMessagesCmd(
		m.ctx, m.api, m.sessionID, m.sessionGeneration, m.commandRequest,
		m.composer.Value(), m.composer.Images(), abort,
	)
	m.restoreQueueRequest = m.commandRequest
	if abort {
		m.setStatus("Restoring queued messages and aborting…", statusWarning)
	} else {
		m.setStatus("Restoring queued messages…", statusInfo)
	}
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
	return m.state.IsPromptRunning || m.state.IsStreaming || m.state.IsBashRunning ||
		m.state.IsCompacting || m.branchSummaryRequest != 0
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
	fileTarget, hasFileTarget := currentFileCompletionTarget(m.composer.Value(), m.composer.CursorOffset())
	if !hasFileTarget || !m.fileCompletion.target.equal(fileTarget) {
		m.fileCompletion.Hide()
	}
	if hasFileTarget {
		m.slashPalette.Hide(m.composer.Value())
		return
	}
	if m.composer.HasImages() {
		m.slashPalette.Hide(m.composer.Value())
		return
	}
	m.slashPalette.SetArgumentCompletions(m.slashArgumentCompletions())
	m.slashPalette.Update(m.composer.Value())
}

func (m *Model) slashArgumentCompletions() map[string][]slashCommand {
	arguments := make(map[string][]slashCommand)
	thinking := m.thinkingSelectorItems()
	arguments["thinking"] = make([]slashCommand, 0, len(thinking))
	for _, item := range thinking {
		arguments["thinking"] = append(arguments["thinking"], slashCommand{
			name: item.Key, description: item.Description, source: "argument",
		})
	}
	for _, info := range m.authProviders {
		if info.SupportsAPIKey || info.SupportsOAuth {
			arguments["login"] = append(arguments["login"], slashCommand{
				name: info.ID, description: info.Name, source: "argument",
			})
		}
		if info.CredentialType != "" {
			arguments["logout"] = append(arguments["logout"], slashCommand{
				name: info.ID, description: info.Name + " · " + info.CredentialType, source: "argument",
			})
		}
	}
	return arguments
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
	m.snapshotLeafID = cloneString(message.snapshot.LeafID)
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
	if message.cancelled {
		m.setStatus("Import cancelled", statusWarning)
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
	m.snapshotLeafID = cloneString(message.snapshot.LeafID)
	m.transcript.ScrollToBottom()
	m.liveItems = make(map[string]contentItem)
	m.liveAssistantID = ""
	m.branchSummaryRequest = 0
	m.syncComposerState()
	if message.notice != "" {
		m.setStatus(message.notice, statusSuccess)
	} else {
		m.setStatus("Opened session "+shortID(m.sessionID), statusSuccess)
	}
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
		view := tea.NewView(m.prepareScreen(content))
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
	paletteLines := m.renderFileCompletion(width, min(5, dockBudget))
	if len(paletteLines) == 0 {
		paletteLines = m.renderSlashPalette(width, min(5, dockBudget))
	}
	dockBudget -= len(paletteLines)
	maxQueueLines := max(0, min(3, dockBudget))
	queueLines := m.renderQueueDock(width, maxQueueLines)
	transcriptHeight := max(1, height-len(queueLines)-len(paletteLines)-composerHeight-1)

	transcript := m.renderTranscript(width, transcriptHeight)
	if m.helpVisible {
		transcript = m.renderHelp(width, transcriptHeight)
	}
	parts := []string{transcript}
	parts = append(parts, queueLines...)
	parts = append(parts, paletteLines...)
	parts = append(parts, composer, m.renderStateLine(width))
	view := tea.NewView(m.prepareScreen(strings.Join(parts, "\n")))
	view.AltScreen = m.mode == ScreenFull
	view.MouseMode = tea.MouseModeNone
	if m.mode == ScreenFull {
		view.MouseMode = tea.MouseModeCellMotion
	}
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
	hint := func(action, description string) string {
		return fmt.Sprintf("%-24s %s", m.keybindings.Hint(action), description)
	}
	lines := []string{
		m.theme.titleStyle(contentRoleSystem, false).Render("pi-go TUI"),
		"",
		hint(keyInputSubmit, "send / steer while streaming"),
		hint(keyMessageFollowUp, "queue a follow-up"),
		hint(keyInputNewLine, "insert a newline"),
		hint(keyInterrupt, "abort current operation"),
		hint(keyViewportPageUp, "scroll conversation upward"),
		hint(keyViewportPageDown, "scroll conversation downward"),
		hint(keyViewportTop, "jump to transcript start"),
		hint(keyViewportBottom, "follow live output"),
		hint(keyViewportPrevious, "jump to previous user prompt"),
		hint(keyViewportNext, "jump to next user prompt"),
		hint(keyToolsExpand, "collapse / expand tool output"),
		hint(keyModelSelect, "open model selector"),
		hint(keyModelForward, "cycle model forward"),
		hint(keyModelBackward, "cycle model backward"),
		hint(keyThinkingCycle, "cycle thinking level"),
		hint(keyThinkingToggle, "hide / show thinking content"),
		hint(keyMessageCopy, "copy last assistant reply"),
		hint(keyPasteImage, "attach image from clipboard"),
		hint(keyExternalEditor, "edit draft in external editor"),
		hint(keyEditorUndo, "undo the last editor change"),
		hint(keyEditorCursorUp, "browse prompt history at the top edge"),
		hint(keyEditorCursorDown, "leave prompt history at the bottom edge"),
		hint(keyMessageDequeue, "restore queued messages"),
		hint(keyExit, "quit when editor is empty"),
		"",
		"Edit keybindings.json in the agent directory, then run /reload.",
		"",
		"/help /hotkeys     show this page",
		"/new               create a session",
		"/resume [id]       select or open a session",
		"/tree              navigate the session tree",
		"/fork              fork from a user message",
		"/clone             clone the current branch",
		"/model [p/id]      select or switch model",
		"/thinking [level]  select reasoning level",
		"/settings          configure runtime and display settings",
		"/export [path]     export HTML, or JSONL when path ends in .jsonl",
		"/import <path>     replace the current session from JSONL",
		"/trust             trust project-local resources",
		"/login [provider]  authenticate with OAuth or an API key",
		"/logout [provider] remove a stored provider credential",
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

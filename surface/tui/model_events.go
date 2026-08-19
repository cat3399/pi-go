package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
)

func (m *Model) applyApplicationEvent(event application.Event) []tea.Cmd {
	if event.Sequence <= m.revision {
		return nil
	}
	m.revision = event.Sequence
	if event.SessionID != m.sessionID {
		return nil
	}
	switch value := event.Value.(type) {
	case application.AgentSessionEvent:
		return m.applyAgentEvent(value.Event)
	case application.OperationEvent:
		commands := make([]tea.Cmd, 0, 2)
		if value.Status == application.OperationFailed {
			m.setStatus(value.Error, statusError)
		} else {
			m.setStatus("", statusInfo)
			if command := m.refreshSelectorForOperation(value.Command); command != nil {
				commands = append(commands, command)
			}
		}
		commands = append(commands, m.requestState())
		return commands
	case application.SessionCatalogEvent:
		if value.Change == application.SessionUpdated {
			return []tea.Cmd{m.requestState()}
		}
	}
	return nil
}

func (m *Model) applyAgentEvent(event agent.SessionEvent) []tea.Cmd { //nolint:gocyclo
	if event == nil {
		return nil
	}
	refreshState := false
	refreshSnapshot := false
	var selectorRefresh tea.Cmd
	switch value := event.(type) {
	case agent.AgentStartEvent:
		m.state.IsPromptRunning = true
		m.state.IsStreaming = true
		m.state.Phase = agent.PhaseProvider
		m.setStatus("Agent started", statusInfo)
	case agent.MessageStartEvent:
		m.upsertAssistant(value.RunID, value.Turn, value.Message, true)
		m.state.IsStreaming = true
		m.state.Phase = agent.PhaseProvider
	case agent.MessageUpdateEvent:
		m.upsertAssistant(value.RunID, value.Turn, value.Message, true)
	case agent.MessageEndEvent:
		m.upsertAssistant(value.RunID, value.Turn, value.Message, false)
	case agent.ToolExecutionStartEvent:
		m.upsertToolStart(value.RunID, value.Turn, value.ToolCallID, value.ToolName, value.Arguments)
		m.state.Phase = agent.PhaseTool
		m.setStatus("Running "+value.ToolName+"…", statusInfo)
	case agent.ToolExecutionUpdateEvent:
		m.upsertToolUpdate(value.RunID, value.Turn, value.ToolCallID, value.ToolName, value.Arguments, value.PartialResult)
	case agent.ToolExecutionEndEvent:
		m.upsertToolEnd(value.RunID, value.Turn, value.ToolCallID, value.ToolName, value.Arguments, value.Result, value.IsError, value.Err)
		refreshState = true
	case agent.EntryAppendedEvent:
		m.applyEntry(value.Entry)
		refreshState = true
	case agent.SessionQueueUpdateEvent:
		m.state.QueuedMessages = agent.QueueState{
			Steering: append([]string(nil), value.Steering...), FollowUp: append([]string(nil), value.FollowUp...),
			SteeringMessages: append([]llm.ConversationMessage(nil), value.SteeringMessages...),
			FollowUpMessages: append([]llm.ConversationMessage(nil), value.FollowUpMessages...),
		}
		m.state.PendingMessageCount = len(value.SteeringMessages) + len(value.FollowUpMessages)
		m.setStatus(fmt.Sprintf("Queued messages: %d", m.state.PendingMessageCount), statusInfo)
	case agent.ThinkingLevelChangedEvent:
		previous := m.state
		m.state.ThinkingLevel = value.Level
		selectorRefresh = m.refreshSelectorForStateChange(previous, m.state)
		m.setStatus("Thinking level: "+string(value.Level), statusSuccess)
	case agent.CompactionStartEvent:
		m.state.IsCompacting = true
		m.state.Phase = agent.PhaseCompacting
		m.setStatus("Compacting context ("+value.Reason.String()+")…", statusInfo)
	case agent.CompactionEndEvent:
		m.state.IsCompacting = false
		if value.Err != nil {
			m.setStatus("Compaction failed: "+value.ErrorMessage, statusError)
		} else if value.Aborted {
			m.setStatus("Compaction aborted", statusWarning)
		} else {
			m.setStatus("Compaction completed", statusSuccess)
		}
		refreshSnapshot, refreshState = true, true
	case agent.AutoRetryStartEvent:
		m.state.RetryAttempt = value.Attempt
		m.state.RetryWaiting = true
		m.state.Phase = agent.PhaseRetryWait
		m.setStatus(fmt.Sprintf("Retry %d/%d in %s", value.Attempt, value.MaxAttempts, value.Delay), statusWarning)
	case agent.AutoRetryEndEvent:
		m.state.RetryWaiting = false
		if value.Success {
			m.setStatus("Retry recovered", statusSuccess)
		} else if value.FinalError != "" {
			m.setStatus("Retry failed: "+value.FinalError, statusError)
		}
		refreshState = true
	case agent.SessionSummarizationRetryScheduledEvent:
		m.setStatus(fmt.Sprintf("Summary retry %d/%d in %s", value.Attempt, value.MaxAttempts, value.Delay), statusWarning)
	case agent.SessionSummarizationRetryFinishedEvent:
		if value.Succeeded {
			m.setStatus("Summary retry recovered", statusSuccess)
		} else if value.FinalError != "" {
			m.setStatus("Summary retry failed: "+value.FinalError, statusError)
		}
	case agent.BashExecutionUpdateEvent:
		m.upsertBashUpdate(value)
		m.state.IsBashRunning = true
	case agent.SessionInfoChangeEvent:
		m.state.SessionName = cloneString(value.Name)
		refreshState = true
	case agent.SessionAgentEndEvent:
		m.state.IsStreaming = value.WillRetry
		m.state.IsPromptRunning = value.WillRetry
		refreshState = true
	case agent.AgentSettledEvent:
		m.state.IsStreaming = false
		m.state.IsPromptRunning = false
		m.state.Phase = agent.PhaseIdle
		m.setStatus("", statusInfo)
		refreshSnapshot, refreshState = true, true
	}
	m.syncComposerState()
	commands := make([]tea.Cmd, 0, 3)
	if selectorRefresh != nil {
		commands = append(commands, selectorRefresh)
	}
	if refreshSnapshot {
		commands = append(commands, m.requestSnapshot())
	} else if refreshState {
		commands = append(commands, m.requestState())
	}
	return commands
}

func (m *Model) upsertAssistant(runID uint64, turn uint32, message agentmsg.Message, live bool) {
	role := "message"
	if message != nil && message.Role() != "" {
		role = string(message.Role())
	}
	id := fmt.Sprintf("live:%s:%d:%d", role, runID, turn)
	revision := uint64(1)
	if existing, ok := m.liveItems[id]; ok {
		revision = existing.Revision + 1
	}
	item, ok := contentItemFromAgentMessage(id, revision, message, live)
	if !ok {
		return
	}
	item.Live = live
	if callID, result, toolResult := toolResultBlocks(item); toolResult {
		if merged, found := m.transcript.MergeToolResult(callID, result); found {
			if _, tracked := m.liveItems[merged.ID]; tracked {
				m.liveItems[merged.ID] = merged
			}
			return
		}
	}
	m.liveItems[id] = item
	if item.Role == contentRoleAssistant {
		m.liveAssistantID = id
	}
	m.transcript.Upsert(item)
}

func (m *Model) upsertToolStart(runID uint64, turn uint32, callID, name string, arguments []byte) {
	m.upsertToolExecution(callID, name, arguments, nil, true, false)
	_ = runID
	_ = turn
}

func (m *Model) upsertToolUpdate(runID uint64, turn uint32, callID, name string, arguments []byte, update agent.ToolUpdate) {
	result := toolResultContentBlocks(name, callID, update.Text, update.Content, encodeToolDetails(update.Details), false, true)
	m.upsertToolExecution(callID, name, arguments, result, true, false)
	_ = runID
	_ = turn
}

func (m *Model) upsertToolEnd(runID uint64, turn uint32, callID, name string, arguments []byte, output agent.ToolOutput, failed bool, err error) {
	failed = failed || err != nil
	result := toolResultContentBlocks(name, callID, output.Text, output.Content, encodeToolDetails(output.Details), failed, false)
	if err != nil {
		result = append(result, contentBlock{
			Kind: contentBlockNotice, Text: err.Error(), ToolCallID: callID, ToolName: name, IsError: true,
		})
	}
	m.upsertToolExecution(callID, name, arguments, result, false, failed)
	_ = runID
	_ = turn
}

func (m *Model) upsertToolExecution(
	callID, name string,
	arguments []byte,
	result []contentBlock,
	live, failed bool,
) {
	formattedArguments := prettyJSON(arguments)
	if item, found := m.transcript.UpdateToolExecution(callID, name, formattedArguments, result, live, failed); found {
		if _, tracked := m.liveItems[item.ID]; tracked {
			m.liveItems[item.ID] = item
		}
		if item.ID != liveToolID(callID) {
			m.removeLive(liveToolID(callID))
		}
		return
	}
	id := liveToolID(callID)
	item := contentItem{
		ID: id, Revision: 1, Role: contentRoleTool, Title: name, Live: live, Failed: failed,
		Blocks: []contentBlock{{
			Kind: contentBlockToolCall, ToolCallID: callID, ToolName: name,
			Text: formattedArguments, Live: live, IsError: failed,
		}},
	}
	item.Blocks = append(item.Blocks, result...)
	if existing, ok := m.liveItems[id]; ok {
		item.Revision = existing.Revision + 1
	}
	m.liveItems[id] = item
	m.transcript.Upsert(item)
}

func (m *Model) upsertBashUpdate(update agent.BashExecutionUpdateEvent) {
	key := "default"
	if update.ID != nil && *update.ID != "" {
		key = *update.ID
	}
	id := "live:bash:" + key
	callID := "bash:" + key
	item, ok := m.liveItems[id]
	if !ok {
		item = contentItem{
			ID: id, Role: contentRoleTool, Title: "Bash", Live: true,
			Blocks: []contentBlock{
				{Kind: contentBlockToolCall, ToolCallID: callID, ToolName: "bash", Live: true},
				{Kind: contentBlockToolResult, ToolCallID: callID, ToolName: "bash", Live: true},
			},
		}
	}
	item.Revision++
	item.Blocks[1].Text += update.Delta
	m.liveItems[id] = item
	m.transcript.Upsert(item)
}

func (m *Model) applyEntry(entry session.Entry) {
	if message, ok := entry.AgentMessage(); ok {
		m.removeLiveForMessage(message)
	}
	item, ok := contentItemFromEntry(entry)
	if !ok {
		return
	}
	if callID, result, toolResult := toolResultBlocks(item); toolResult {
		if merged, found := m.transcript.MergeToolResult(callID, result); found {
			if _, tracked := m.liveItems[merged.ID]; tracked {
				m.liveItems[merged.ID] = merged
			}
			return
		}
	}
	m.transcript.Upsert(item)
	m.adoptLiveToolExecutions(item)
}

func (m *Model) adoptLiveToolExecutions(item contentItem) {
	for _, call := range item.Blocks {
		if call.Kind != contentBlockToolCall || call.ToolCallID == "" {
			continue
		}
		liveID := liveToolID(call.ToolCallID)
		live, found := m.liveItems[liveID]
		if !found || len(live.Blocks) == 0 {
			continue
		}
		result := append([]contentBlock(nil), live.Blocks[1:]...)
		m.transcript.UpdateToolExecution(
			call.ToolCallID, call.ToolName, call.Text, result, live.Live, live.Failed,
		)
		m.removeLive(liveID)
	}
}

func (m *Model) removeLiveForMessage(message agentmsg.Message) {
	switch value := message.(type) {
	case agentmsg.LLM:
		conversation := value.Conversation()
		switch conversation.Role() {
		case llm.RoleAssistant:
			m.removeLive(m.liveAssistantID)
			m.liveAssistantID = ""
		case llm.RoleToolResult:
			switch result := conversation.(type) {
			case llm.ToolResultMessage:
				m.removeLive(liveToolID(result.ToolCallID()))
			case llm.ToolResultContentMessage:
				m.removeLive(liveToolID(result.ToolCallID()))
			}
		}
	case agentmsg.BashExecution:
		for id := range m.liveItems {
			if strings.HasPrefix(id, "live:bash:") {
				m.removeLive(id)
			}
		}
	}
}

func (m *Model) removeLive(id string) {
	if id == "" {
		return
	}
	delete(m.liveItems, id)
	m.transcript.Remove(id)
}

func liveToolID(callID string) string { return "live:tool:" + callID }

func toolResultContentBlocks(
	name, callID, text string,
	rich []llm.ToolResultContentBlock,
	details json.RawMessage,
	failed, live bool,
) []contentBlock {
	result := make([]contentBlock, 0, len(rich)+1)
	if len(rich) == 0 {
		result = append(result, contentBlock{
			Kind: contentBlockToolResult, Text: text, ToolName: name, ToolCallID: callID,
			ToolDetails: details, IsError: failed, Live: live,
		})
		return result
	}
	for _, block := range rich {
		switch block := block.(type) {
		case llm.TextBlock:
			result = append(result, contentBlock{
				Kind: contentBlockToolResult, Text: block.Text(), ToolName: name, ToolCallID: callID,
				ToolDetails: details, IsError: failed, Live: live,
			})
		case llm.ImageBlock:
			image := imageContentBlock(block)
			image.ToolCallID, image.ToolName, image.ToolDetails = callID, name, details
			image.IsError, image.Live = failed, live
			result = append(result, image)
		}
	}
	return result
}

func encodeToolDetails(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil || !json.Valid(encoded) {
		return nil
	}
	return encoded
}

func (m *Model) handleCommandFinished(message commandFinishedMsg) []tea.Cmd { //nolint:gocyclo
	restoreResult := m.restoreQueueRequest != 0 && message.request == m.restoreQueueRequest
	branchSummaryResult := m.branchSummaryRequest != 0 && message.request == m.branchSummaryRequest
	if message.sessionID != m.sessionID ||
		message.sessionGeneration != m.sessionGeneration ||
		(message.request <= m.commandApplied && !restoreResult && !branchSummaryResult) {
		return nil
	}
	if message.request > m.commandApplied {
		m.commandApplied = message.request
	}
	if message.err != nil {
		if branchSummaryResult {
			m.branchSummaryRequest = 0
			m.syncComposerState()
		}
		if message.request == m.restoreQueueRequest {
			m.restoreQueueRequest = 0
			if result, ok := message.result.(application.ClearQueueResult); ok {
				restoredCount := m.restoreQueueDraft(result.Queue)
				m.setStatus(fmt.Sprintf("Restored %d queued messages; abort failed: %v", restoredCount, message.err), statusError)
				return []tea.Cmd{m.requestState()}
			}
		}
		if _, modelSelection := message.command.(application.SetModelCommand); modelSelection && m.selector == nil {
			name, query, slash := splitSlashCommand(strings.TrimSpace(message.draft))
			if slash && name == "model" && query != "" {
				return []tea.Cmd{m.openModelSelector(query)}
			}
		}
		m.composer.RestoreDraftIfEmpty(message.draft, message.draftImages)
		m.updateSlashPalette()
		m.setStatus(message.err.Error(), statusError)
		return []tea.Cmd{m.requestState()}
	}
	refreshState := false
	refreshSnapshot := false
	refreshCommands := false
	commands := make([]tea.Cmd, 0, 4)
	switch result := message.result.(type) {
	case application.PromptStartedResult:
		m.state.IsPromptRunning = true
		m.setStatus(fmt.Sprintf("Prompt accepted · operation %d", result.OperationID), statusSuccess)
	case application.AbortResult, application.AbortBashResult, application.AbortCompactionResult:
		m.setStatus("Abort requested", statusWarning)
		refreshState = true
	case application.AbortBranchSummaryResult:
		m.setStatus("Branch summary cancellation requested", statusWarning)
		refreshState = true
	case application.GetStateResult:
		m.state = result.State
	case application.ClearQueueResult:
		m.state.QueuedMessages = agent.QueueState{}
		m.state.PendingMessageCount = 0
		if message.request == m.restoreQueueRequest {
			m.restoreQueueRequest = 0
			restoredCount := m.restoreQueueDraft(result.Queue)
			if message.restoreAndAbort {
				m.setStatus(fmt.Sprintf("Abort requested · restored %d queued messages", restoredCount), statusWarning)
				refreshState = true
			} else {
				m.setStatus(fmt.Sprintf("Restored %d queued messages", restoredCount), statusSuccess)
			}
		} else {
			m.setStatus("Queue cleared", statusSuccess)
		}
	case application.ReloadResult:
		if command := m.closeSelectorForCommand(application.CommandReload); command != nil {
			commands = append(commands, command)
		}
		if err := m.reloadKeybindings(); err != nil {
			m.setStatus("Resources reloaded; keybindings reload failed: "+err.Error(), statusError)
		} else {
			m.setStatus("Resources and keybindings reloaded", statusSuccess)
		}
		refreshSnapshot, refreshState, refreshCommands = true, true, true
	case application.SetModelResult:
		if command := m.closeSelectorForCommand(application.CommandSetModel); command != nil {
			commands = append(commands, command)
		}
		m.state.Model, m.state.HasModel = result.Model, true
		m.setStatus("Model: "+result.Model.Provider()+"/"+result.Model.ID(), statusSuccess)
	case application.CycleModelResult:
		if result.Result == nil {
			m.setStatus("No model available to cycle", statusWarning)
			break
		}
		m.state.Model, m.state.HasModel = result.Result.Model, true
		m.state.ThinkingLevel = result.Result.ThinkingLevel
		m.setStatus("Model: "+result.Result.Model.Provider()+"/"+result.Result.Model.ID(), statusSuccess)
		m.refreshOpenSettings()
	case application.GetAvailableModelsResult:
		lines := make([]string, 0, len(result.Models))
		for _, model := range result.Models {
			lines = append(lines, model.Provider()+"/"+model.ID())
		}
		m.appendLocalNotice("Available models", strings.Join(lines, "\n"))
		m.setStatus(fmt.Sprintf("%d models available", len(result.Models)), statusSuccess)
	case application.SetThinkingLevelResult:
		if command := m.closeSelectorForCommand(application.CommandSetThinkingLevel); command != nil {
			commands = append(commands, command)
		}
		if command, ok := message.command.(application.SetThinkingLevelCommand); ok {
			m.state.ThinkingLevel = command.Level
		}
		m.setStatus("Thinking level updated", statusSuccess)
		m.refreshOpenSettings()
	case application.CycleThinkingLevelResult:
		if result.Level == nil {
			m.setStatus("The active model does not support thinking", statusWarning)
			break
		}
		m.state.ThinkingLevel = *result.Level
		m.setStatus("Thinking level: "+string(*result.Level), statusSuccess)
		m.refreshOpenSettings()
	case application.GetAvailableThinkingLevelsResult:
		levels := make([]string, len(result.Levels))
		for index, level := range result.Levels {
			levels[index] = string(level)
		}
		m.appendLocalNotice("Available thinking levels", strings.Join(levels, "\n"))
		m.setStatus(fmt.Sprintf("%d thinking levels available", len(result.Levels)), statusSuccess)
	case application.SetSteeringModeResult:
		if command, ok := message.command.(application.SetSteeringModeCommand); ok {
			m.state.SteeringMode = command.Mode
			m.setStatus(settingUpdatedStatus("Steering mode", command.Mode.String()), statusSuccess)
		}
		m.refreshOpenSettings()
	case application.SetFollowUpModeResult:
		if command, ok := message.command.(application.SetFollowUpModeCommand); ok {
			m.state.FollowUpMode = command.Mode
			m.setStatus(settingUpdatedStatus("Follow-up mode", command.Mode.String()), statusSuccess)
		}
		m.refreshOpenSettings()
	case application.CompactResult:
		m.setStatus("Compaction completed", statusSuccess)
		refreshSnapshot, refreshState = true, true
	case application.SetSessionNameResult:
		if command, ok := message.command.(application.SetSessionNameCommand); ok {
			m.state.SessionName = cloneString(&command.Name)
		}
		m.setStatus("Session renamed", statusSuccess)
	case application.GetSessionStatsResult:
		m.appendLocalNotice("Session stats", formatSessionStats(result))
		m.setStatus("Session stats loaded", statusSuccess)
	case application.GetLastAssistantTextResult:
		if result.Text == nil || strings.TrimSpace(*result.Text) == "" {
			m.setStatus("No assistant reply to copy", statusWarning)
		} else if m.setClipboard == nil {
			m.setStatus("Clipboard is unavailable", statusError)
		} else {
			commands = append(commands, m.setClipboard(*result.Text))
			m.setStatus("Copied last assistant reply", statusSuccess)
		}
	case application.SetAutoCompactionResult:
		if command, ok := message.command.(application.SetAutoCompactionCommand); ok {
			m.state.AutoCompactionEnabled = command.Enabled
			m.setStatus(settingUpdatedStatus("Automatic compaction", enabledLabel(command.Enabled)), statusSuccess)
		}
		m.refreshOpenSettings()
	case application.SetAutoRetryResult:
		if command, ok := message.command.(application.SetAutoRetryCommand); ok {
			m.state.AutoRetryEnabled = command.Enabled
			m.setStatus(settingUpdatedStatus("Automatic retry", enabledLabel(command.Enabled)), statusSuccess)
		}
		m.refreshOpenSettings()
	case application.AbortRetryResult:
		m.state.RetryWaiting = false
		m.setStatus("Retry cancellation requested", statusWarning)
		refreshState = true
	case application.GetToolsResult:
		lines := make([]string, 0, len(result.Tools))
		for _, tool := range result.Tools {
			mark := "○"
			if tool.Active {
				mark = "●"
			}
			lines = append(lines, mark+" "+tool.Name+" — "+tool.Description)
		}
		m.appendLocalNotice("Tools", strings.Join(lines, "\n"))
		m.setStatus("Tool state loaded", statusSuccess)
	case application.SetToolsResult:
		if command := m.closeSelectorForCommand(application.CommandSetTools); command != nil {
			commands = append(commands, command)
		}
		m.setStatus("Tools updated", statusSuccess)
		refreshState = true
	case application.BashResult:
		m.state.IsBashRunning = false
		m.setStatus("Bash completed", statusSuccess)
		refreshSnapshot, refreshState = true, true
	case application.ForkResult:
		if result.Cancelled {
			m.setStatus("Fork cancelled", statusWarning)
			break
		}
		if result.SelectedText != nil {
			m.composer.SetDraft(*result.SelectedText, nil)
			m.updateSlashPalette()
		}
		if result.SessionID == nil || strings.TrimSpace(*result.SessionID) == "" {
			m.setStatus("Fork completed without a session id", statusError)
			break
		}
		m.setStatus("Opening forked session…", statusInfo)
		commands = append(commands, m.openSession(*result.SessionID))
	case application.NavigateTreeResult:
		if branchSummaryResult {
			m.branchSummaryRequest = 0
		}
		if result.EditorText != nil {
			m.composer.SetDraft(*result.EditorText, nil)
			m.updateSlashPalette()
		}
		switch {
		case result.Aborted:
			m.setStatus("Branch summary cancelled", statusWarning)
		case result.Cancelled:
			m.setStatus("Tree navigation cancelled", statusWarning)
		default:
			m.setStatus("Session tree position updated", statusSuccess)
		}
		refreshSnapshot, refreshState = true, true
	default:
		m.setStatus(strings.ReplaceAll(string(message.command.Type()), "_", " ")+" completed", statusSuccess)
		refreshState = true
	}
	m.syncComposerState()
	if refreshSnapshot {
		commands = append(commands, m.requestSnapshot())
	} else if refreshState {
		commands = append(commands, m.requestState())
	}
	if refreshCommands {
		commands = append(commands, m.requestCommands())
	}
	return commands
}

func formatSessionStats(result application.GetSessionStatsResult) string {
	stats := result.Stats
	lines := []string{
		"session id: " + stats.SessionID,
		fmt.Sprintf(
			"messages: %d total · %d user · %d assistant · %d tool results",
			stats.TotalMessages, stats.UserMessages, stats.AssistantMessages, stats.ToolResults,
		),
		fmt.Sprintf("tool calls: %d", stats.ToolCalls),
		fmt.Sprintf(
			"tokens: %d total · %d input · %d output · %d cache read · %d cache write",
			stats.Tokens.Total, stats.Tokens.Input, stats.Tokens.Output, stats.Tokens.CacheRead, stats.Tokens.CacheWrite,
		),
		fmt.Sprintf("cost: $%.6f", stats.Cost),
	}
	if result.SessionName != nil && strings.TrimSpace(*result.SessionName) != "" {
		lines = append(lines, "name: "+strings.TrimSpace(*result.SessionName))
	}
	if stats.SessionFile != nil && strings.TrimSpace(*stats.SessionFile) != "" {
		lines = append(lines, "file: "+strings.TrimSpace(*stats.SessionFile))
	}
	if stats.ContextUsage != nil {
		context := "context: unknown"
		if stats.ContextUsage.Tokens != nil {
			context = fmt.Sprintf("context: %d / %d tokens", *stats.ContextUsage.Tokens, stats.ContextUsage.ContextWindow)
		}
		if stats.ContextUsage.Percent != nil {
			context += fmt.Sprintf(" · %.1f%%", *stats.ContextUsage.Percent)
		}
		lines = append(lines, context)
	}
	return strings.Join(lines, "\n")
}

func (m *Model) restoreQueueDraft(queue agent.QueueState) int {
	m.state.QueuedMessages = agent.QueueState{}
	m.state.PendingMessageCount = 0
	restored, restoredImages, restoredCount := queueDraft(queue)
	current := strings.TrimSpace(m.composer.Value())
	if current != "" {
		restored = append(restored, current)
	}
	restoredImages = append(restoredImages, m.composer.Images()...)
	m.composer.SetDraft(strings.Join(restored, "\n\n"), restoredImages)
	m.updateSlashPalette()
	return restoredCount
}

func (m *Model) appendLocalNotice(title, text string) {
	m.localID++
	item := contentItem{
		ID: fmt.Sprintf("local:%d", m.localID), Revision: 1, Role: contentRoleSystem, Title: title,
		Blocks: []contentBlock{{Kind: contentBlockText, Text: text}},
	}
	m.transcript.Upsert(item)
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func compactNonEmptyStrings(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func queueDraft(queue agent.QueueState) ([]string, []llm.ImageBlock, int) {
	steeringText, steeringImages, steeringCount := queueGroupDraft(queue.SteeringMessages, queue.Steering)
	followUpText, followUpImages, followUpCount := queueGroupDraft(queue.FollowUpMessages, queue.FollowUp)
	return append(steeringText, followUpText...),
		append(steeringImages, followUpImages...), steeringCount + followUpCount
}

func queueGroupDraft(messages []llm.ConversationMessage, fallback []string) ([]string, []llm.ImageBlock, int) {
	count := max(len(messages), len(fallback))
	texts := make([]string, 0, count)
	var images []llm.ImageBlock
	for index := range count {
		var text string
		var messageImages []llm.ImageBlock
		if index < len(messages) {
			text, messageImages = conversationMessageDraft(messages[index])
		}
		if strings.TrimSpace(text) == "" && len(messageImages) == 0 && index < len(fallback) {
			text = fallback[index]
		}
		if text = strings.TrimSpace(text); text != "" {
			texts = append(texts, text)
		}
		images = append(images, messageImages...)
	}
	return texts, images, count
}

func conversationMessageDraft(message llm.ConversationMessage) (string, []llm.ImageBlock) {
	var texts []string
	var images []llm.ImageBlock
	switch message := message.(type) {
	case llm.UserTextMessage:
		for _, block := range message.Content() {
			texts = append(texts, block.Text())
		}
	case llm.UserContentMessage:
		for _, block := range message.Content() {
			switch block := block.(type) {
			case llm.TextBlock:
				texts = append(texts, block.Text())
			case llm.ImageBlock:
				images = append(images, block)
			}
		}
	}
	return strings.Join(compactNonEmptyStrings(texts), "\n"), images
}

package tui

import (
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
		if value.Status == application.OperationFailed {
			m.setStatus(value.Error, statusError)
		} else {
			m.setStatus("", statusInfo)
		}
		return []tea.Cmd{m.requestState()}
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
		m.state.ThinkingLevel = value.Level
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
	commands := make([]tea.Cmd, 0, 2)
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
	m.liveItems[id] = item
	if item.Role == contentRoleAssistant {
		m.liveAssistantID = id
	}
	m.transcript.Upsert(item)
}

func (m *Model) upsertToolStart(runID uint64, turn uint32, callID, name string, arguments []byte) {
	id := liveToolID(callID)
	item := contentItem{
		ID: id, Revision: 1, Role: contentRoleTool, Title: name, Live: true,
		Blocks: []contentBlock{{Kind: contentBlockToolCall, ToolCallID: callID, ToolName: name, Text: prettyJSON(arguments), Live: true}},
	}
	if existing, ok := m.liveItems[id]; ok {
		item.Revision = existing.Revision + 1
	}
	m.liveItems[id] = item
	m.transcript.Upsert(item)
	_ = runID
	_ = turn
}

func (m *Model) upsertToolUpdate(runID uint64, turn uint32, callID, name string, arguments []byte, update agent.ToolUpdate) {
	m.upsertToolStart(runID, turn, callID, name, arguments)
	id := liveToolID(callID)
	item := m.liveItems[id]
	item.Revision++
	item.Blocks = append(item.Blocks[:1], toolResultContentBlocks(name, callID, update.Text, update.Content, false, true)...)
	m.liveItems[id] = item
	m.transcript.Upsert(item)
}

func (m *Model) upsertToolEnd(runID uint64, turn uint32, callID, name string, arguments []byte, output agent.ToolOutput, failed bool, err error) {
	m.upsertToolStart(runID, turn, callID, name, arguments)
	id := liveToolID(callID)
	item := m.liveItems[id]
	item.Revision++
	item.Live = false
	item.Failed = failed || err != nil
	item.Blocks = append(item.Blocks[:1], toolResultContentBlocks(name, callID, output.Text, output.Content, item.Failed, false)...)
	if err != nil {
		item.Blocks = append(item.Blocks, contentBlock{Kind: contentBlockNotice, Text: err.Error(), IsError: true})
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
	item, ok := m.liveItems[id]
	if !ok {
		item = contentItem{ID: id, Role: contentRoleTool, Title: "Bash", Live: true}
	}
	item.Revision++
	if len(item.Blocks) == 0 {
		item.Blocks = []contentBlock{{Kind: contentBlockCode, Text: update.Delta, Live: true}}
	} else {
		item.Blocks[0].Text += update.Delta
	}
	m.liveItems[id] = item
	m.transcript.Upsert(item)
}

func (m *Model) applyEntry(entry session.Entry) {
	if message, ok := entry.AgentMessage(); ok {
		m.removeLiveForMessage(message)
	}
	if item, ok := contentItemFromEntry(entry); ok {
		m.transcript.Upsert(item)
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

func toolResultContentBlocks(name, callID, text string, rich []llm.ToolResultContentBlock, failed, live bool) []contentBlock {
	result := make([]contentBlock, 0, len(rich)+1)
	if len(rich) == 0 {
		result = append(result, contentBlock{
			Kind: contentBlockToolResult, Text: text, ToolName: name, ToolCallID: callID, IsError: failed, Live: live,
		})
		return result
	}
	for _, block := range rich {
		switch block := block.(type) {
		case llm.TextBlock:
			result = append(result, contentBlock{
				Kind: contentBlockToolResult, Text: block.Text(), ToolName: name, ToolCallID: callID,
				IsError: failed, Live: live,
			})
		case llm.ImageBlock:
			image := imageContentBlock(block)
			image.IsError, image.Live = failed, live
			result = append(result, image)
		}
	}
	return result
}

func (m *Model) handleCommandFinished(message commandFinishedMsg) []tea.Cmd { //nolint:gocyclo
	if message.sessionID != m.sessionID ||
		message.sessionGeneration != m.sessionGeneration ||
		message.request <= m.commandApplied {
		return nil
	}
	m.commandApplied = message.request
	if message.err != nil {
		m.composer.RestoreIfEmpty(message.draft)
		m.setStatus(message.err.Error(), statusError)
		return []tea.Cmd{m.requestState()}
	}
	refreshState := false
	refreshSnapshot := false
	switch result := message.result.(type) {
	case application.PromptStartedResult:
		m.state.IsPromptRunning = true
		m.setStatus(fmt.Sprintf("Prompt accepted · operation %d", result.OperationID), statusSuccess)
	case application.AbortResult, application.AbortBashResult, application.AbortCompactionResult:
		m.setStatus("Abort requested", statusWarning)
		refreshState = true
	case application.GetStateResult:
		m.state = result.State
	case application.ClearQueueResult:
		m.state.QueuedMessages = result.Queue
		m.state.PendingMessageCount = len(result.Queue.SteeringMessages) + len(result.Queue.FollowUpMessages)
		m.setStatus("Queue cleared", statusSuccess)
	case application.ReloadResult:
		m.setStatus("Resources reloaded", statusSuccess)
		refreshSnapshot, refreshState = true, true
	case application.SetModelResult:
		m.state.Model, m.state.HasModel = result.Model, true
		m.setStatus("Model: "+result.Model.Provider()+"/"+result.Model.ID(), statusSuccess)
	case application.SetThinkingLevelResult:
		if command, ok := message.command.(application.SetThinkingLevelCommand); ok {
			m.state.ThinkingLevel = command.Level
		}
		m.setStatus("Thinking level updated", statusSuccess)
	case application.CompactResult:
		m.setStatus("Compaction completed", statusSuccess)
		refreshSnapshot, refreshState = true, true
	case application.SetSessionNameResult:
		if command, ok := message.command.(application.SetSessionNameCommand); ok {
			m.state.SessionName = cloneString(&command.Name)
		}
		m.setStatus("Session renamed", statusSuccess)
	case application.GetSessionStatsResult:
		m.appendLocalNotice("Session stats", fmt.Sprintf(
			"messages: %d\ntool calls: %d\ntokens: %d\ncost: $%.6f",
			result.Stats.TotalMessages, result.Stats.ToolCalls, result.Stats.Tokens.Total, result.Stats.Cost,
		))
		m.setStatus("Session stats loaded", statusSuccess)
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
	case application.BashResult:
		m.state.IsBashRunning = false
		m.setStatus("Bash completed", statusSuccess)
		refreshSnapshot, refreshState = true, true
	case application.ForkResult, application.NavigateTreeResult:
		refreshSnapshot, refreshState = true, true
	default:
		m.setStatus(strings.ReplaceAll(string(message.command.Type()), "_", " ")+" completed", statusSuccess)
		refreshState = true
	}
	m.syncComposerState()
	commands := make([]tea.Cmd, 0, 2)
	if refreshSnapshot {
		commands = append(commands, m.requestSnapshot())
	} else if refreshState {
		commands = append(commands, m.requestState())
	}
	return commands
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

// Package protocolv1 owns the versioned JSON projection used by transport
// adapters above the application API. It never owns Agent or Session state.
package protocolv1

import (
	"encoding/json"
	"fmt"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/resource"
	"github.com/cat3399/pi-go/internal/session"
)

type noData struct{}

var omittedData = noData{}

// EncodeResult converts a application command result to the canonical public JSON
// shape. present is false for commands whose successful response has no data.
func EncodeResult(result application.CommandResult) (data any, present bool, err error) {
	data, err = encodeResult(result)
	if err != nil {
		return nil, false, err
	}
	if _, omitted := data.(noData); omitted {
		return nil, false, nil
	}
	return data, true, nil
}

// EncodeState converts an authoritative application snapshot to its public JSON
// shape. The snapshot is sampled by application; this function does not replay events.
func EncodeState(value application.State) map[string]any { return stateWire(value) }

// EncodeEvent converts one ordered application event to its public JSON shape.
func EncodeEvent(value application.Event) (any, error) { return encodeApplicationEvent(value) }

func successResponse(id *string, command string, data any) map[string]any {
	response := map[string]any{"type": "response", "command": command, "success": true}
	if id != nil {
		response["id"] = *id
	}
	if _, omitted := data.(noData); !omitted {
		response["data"] = data
	}
	return response
}

func errorResponse(id *string, command string, err error) map[string]any {
	response := map[string]any{"type": "response", "command": command, "success": false, "error": err.Error()}
	if id != nil {
		response["id"] = *id
	}
	return response
}

func encodeResult(result application.CommandResult) (any, error) {
	switch value := result.(type) {
	case application.PromptStartedResult:
		return map[string]any{"operationId": value.OperationID}, nil
	case application.AbortResult, application.SteerResult, application.FollowUpResult,
		application.SetThinkingLevelResult, application.AbortCompactionResult, application.AbortBranchSummaryResult,
		application.SetSessionNameResult, application.SetSteeringModeResult, application.SetFollowUpModeResult,
		application.SetAutoCompactionResult, application.SetAutoRetryResult, application.SetToolsResult,
		application.AbortRetryResult, application.AbortBashResult:
		return omittedData, nil
	case application.GetStateResult:
		return stateWire(value.State), nil
	case application.ClearQueueResult:
		return queueWire(value.Queue), nil
	case application.ReloadResult:
		return map[string]any{"success": true}, nil
	case application.SetModelResult:
		return modelWire(value.Model), nil
	case application.CycleModelResult:
		if value.Result == nil {
			return nil, nil
		}
		return map[string]any{
			"model": modelWire(value.Result.Model), "thinkingLevel": value.Result.ThinkingLevel,
			"isScoped": value.Result.IsScoped,
		}, nil
	case application.GetAvailableModelsResult:
		models := make([]map[string]any, len(value.Models))
		for index, model := range value.Models {
			models[index] = modelWire(model)
		}
		return map[string]any{"models": models}, nil
	case application.ForkResult:
		data := map[string]any{"cancelled": value.Cancelled}
		if value.SelectedText != nil {
			data["text"] = *value.SelectedText
		}
		if value.SessionID != nil {
			data["newSessionId"] = *value.SessionID
		}
		return data, nil
	case application.NavigateTreeResult:
		data := map[string]any{"cancelled": value.Cancelled, "aborted": value.Aborted}
		if value.EditorText != nil {
			data["editorText"] = *value.EditorText
		}
		if value.SummaryEntry != nil {
			data["summaryEntry"] = json.RawMessage(value.SummaryEntry.RawJSON())
		}
		return data, nil
	case application.CycleThinkingLevelResult:
		if value.Level == nil {
			return nil, nil
		}
		return map[string]any{"level": *value.Level}, nil
	case application.GetAvailableThinkingLevelsResult:
		return map[string]any{"levels": value.Levels}, nil
	case application.CompactResult:
		return compactResultWire(value.Result)
	case application.GetSessionStatsResult:
		return statsWire(value), nil
	case application.GetLastAssistantTextResult:
		return map[string]any{"text": value.Text}, nil
	case application.GetToolsResult:
		tools := make([]map[string]any, len(value.Tools))
		for index, tool := range value.Tools {
			tools[index] = map[string]any{
				"name": tool.Name, "description": tool.Description, "parameters": tool.Parameters,
				"promptGuidelines": tool.PromptGuidelines, "sourceInfo": agentSourceWire(tool.SourceInfo),
				"active": tool.Active,
			}
		}
		return tools, nil
	case application.BashResult:
		return bashResultWire(value.Result), nil
	case application.GetCommandsResult:
		commands := make([]map[string]any, len(value.Commands))
		for index, command := range value.Commands {
			item := map[string]any{
				"name": command.Name, "source": command.Source,
				"sourceInfo": sourceWire(command.SourceInfo),
			}
			if command.Description != "" {
				item["description"] = command.Description
			}
			if command.ArgumentHint != "" {
				item["argumentHint"] = command.ArgumentHint
			}
			commands[index] = item
		}
		return map[string]any{"commands": commands}, nil
	default:
		return nil, fmt.Errorf("unsupported command result %T", result)
	}
}

func stateWire(value application.State) map[string]any {
	state := map[string]any{
		"sessionId": value.SessionID, "cwd": value.CWD,
		"thinkingLevel": value.ThinkingLevel, "systemPrompt": value.SystemPrompt,
		"phase": value.Phase.String(), "isStreaming": value.IsStreaming,
		"isPromptRunning": value.IsPromptRunning, "isBashRunning": value.IsBashRunning,
		"isCompacting": value.IsCompacting, "retryAttempt": value.RetryAttempt,
		"retryWaiting": value.RetryWaiting, "steeringMode": value.SteeringMode.String(),
		"followUpMode": value.FollowUpMode.String(), "autoCompactionEnabled": value.AutoCompactionEnabled,
		"autoRetryEnabled": value.AutoRetryEnabled, "messageCount": value.MessageCount,
		"pendingMessageCount": value.PendingMessageCount, "queuedMessages": queueWire(value.QueuedMessages),
	}
	if value.SessionFile != nil {
		state["sessionFile"] = *value.SessionFile
	}
	if value.SessionName != nil {
		state["sessionName"] = *value.SessionName
	}
	if value.HasModel {
		state["model"] = modelWire(value.Model)
	}
	if value.ContextUsage == nil {
		state["contextUsage"] = nil
	} else {
		state["contextUsage"] = contextUsageWire(*value.ContextUsage)
	}
	return state
}

func queueWire(value agent.QueueState) map[string]any {
	return map[string]any{
		"steering": nonNilStrings(value.Steering),
		"followUp": nonNilStrings(value.FollowUp),
	}
}

func nonNilStrings(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	return append([]string(nil), values...)
}

func modelWire(value provider.Model) map[string]any {
	model := map[string]any{
		"id": value.ID(), "name": value.Name(), "api": value.API(), "provider": value.Provider(),
		"baseUrl": value.BaseURL(), "reasoning": value.Reasoning(), "input": value.Input(),
		"cost": value.Cost(), "contextWindow": value.ContextWindow(), "maxTokens": value.MaxTokens(),
	}
	if levels := value.ThinkingLevelMap(); len(levels) != 0 {
		model["thinkingLevelMap"] = levels
	}
	if headers := value.Headers(); len(headers) != 0 {
		model["headers"] = headers
	}
	if compat := modelCompatWire(value); compat != nil {
		model["compat"] = compat
	}
	return model
}

func modelCompatWire(model provider.Model) map[string]any {
	compat := model.Compat()
	if raw, ok := compat.Additional[model.API()]; ok {
		var value map[string]any
		if json.Unmarshal(raw, &value) == nil {
			return value
		}
	}
	var value any
	switch model.API() {
	case provider.OpenAIResponsesAPI, provider.OpenAICodexResponsesAPI:
		value = compat.OpenAIResponses
	case provider.OpenAICompletionsAPI:
		value = compat.OpenAICompletions
	case provider.AnthropicMessagesAPI:
		value = compat.AnthropicMessages
	default:
		value = compat.Bedrock
	}
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	// Provider compat structs intentionally have Go-native field names. Convert
	// them through the catalog's public lower-camel wire vocabulary here.
	var native map[string]any
	if json.Unmarshal(raw, &native) != nil {
		return nil
	}
	result := make(map[string]any, len(native))
	for key, item := range native {
		if item == nil {
			continue
		}
		result[lowerCamel(key)] = item
	}
	return result
}

func lowerCamel(value string) string {
	if value == "" || value[0] < 'A' || value[0] > 'Z' {
		return value
	}
	return string(value[0]-'A'+'a') + value[1:]
}

func contextUsageWire(value agent.ContextUsage) map[string]any {
	return map[string]any{"tokens": value.Tokens, "contextWindow": value.ContextWindow, "percent": value.Percent}
}

func statsWire(value application.GetSessionStatsResult) map[string]any {
	stats := value.Stats
	result := map[string]any{
		"sessionId": stats.SessionID, "userMessages": stats.UserMessages,
		"assistantMessages": stats.AssistantMessages, "toolCalls": stats.ToolCalls,
		"toolResults": stats.ToolResults, "totalMessages": stats.TotalMessages,
		"tokens": map[string]any{
			"input": stats.Tokens.Input, "output": stats.Tokens.Output,
			"cacheRead": stats.Tokens.CacheRead, "cacheWrite": stats.Tokens.CacheWrite,
			"total": stats.Tokens.Total,
		},
		"cost": stats.Cost,
	}
	if stats.SessionFile != nil {
		result["sessionFile"] = *stats.SessionFile
	}
	if value.SessionName != nil {
		result["sessionName"] = *value.SessionName
	}
	if stats.ContextUsage != nil {
		result["contextUsage"] = contextUsageWire(*stats.ContextUsage)
	}
	return result
}

func compactResultWire(value session.CompactResult) (map[string]any, error) {
	firstKeptEntryID := value.Output.FirstKeptEntryID
	if firstKeptEntryID == "" {
		firstKeptEntryID = value.Input.FirstKeptEntryID
	}
	tokensBefore := value.Output.TokensBefore
	if tokensBefore == 0 {
		tokensBefore = value.Input.TokensBefore
	}
	result := map[string]any{
		"summary": value.Output.Text, "firstKeptEntryId": firstKeptEntryID,
		"tokensBefore": tokensBefore,
	}
	estimated := value.EstimatedTokensAfter
	if value.Output.EstimatedTokensAfter != nil {
		estimated = *value.Output.EstimatedTokensAfter
	}
	if estimated != 0 {
		result["estimatedTokensAfter"] = estimated
	}
	if value.Output.Usage != nil {
		usage, err := marshalCompactionUsage(*value.Output.Usage)
		if err != nil {
			return nil, err
		}
		result["usage"] = usage
	}
	if len(value.Output.Details) != 0 {
		result["details"] = json.RawMessage(value.Output.Details)
	}
	return result, nil
}

func marshalCompactionUsage(value session.CompactionUsage) (json.RawMessage, error) {
	usage, err := session.MarshalUsage(value.Usage)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(usage, &object); err != nil {
		return nil, err
	}
	object["cost"] = value.Cost
	return json.Marshal(object)
}

func bashResultWire(value agent.BashResult) map[string]any {
	result := map[string]any{
		"output": value.Output, "cancelled": value.Cancelled, "truncated": value.Truncated,
	}
	if value.ExitCode != nil {
		result["exitCode"] = *value.ExitCode
	}
	if value.FullOutputPath != "" {
		result["fullOutputPath"] = value.FullOutputPath
	}
	return result
}

func sourceWire(value resource.Source) map[string]any {
	result := map[string]any{"path": value.Path, "source": value.Source, "scope": value.Scope, "origin": value.Origin}
	if value.BaseDir != "" {
		result["baseDir"] = value.BaseDir
	}
	return result
}

func agentSourceWire(value agent.SystemPromptSourceInfo) map[string]any {
	result := map[string]any{"path": value.Path, "source": value.Source, "scope": value.Scope, "origin": value.Origin}
	if value.BaseDir != nil {
		result["baseDir"] = *value.BaseDir
	}
	return result
}

func encodeApplicationEvent(value application.Event) (any, error) {
	switch event := value.Value.(type) {
	case application.AgentSessionEvent:
		return sessionEventWire(event.Event)
	case application.OperationEvent:
		result := map[string]any{
			"type": "operation", "operationId": event.OperationID,
			"command": event.Command, "status": event.Status,
		}
		if event.Error != "" {
			result["errorMessage"] = event.Error
		}
		return result, nil
	case application.SessionCatalogEvent:
		return map[string]any{"type": "session_catalog", "change": event.Change}, nil
	default:
		return nil, fmt.Errorf("unsupported application event %T", value.Value)
	}
}

func sessionEventWire(event agent.SessionEvent) (map[string]any, error) {
	switch value := event.(type) {
	case agent.AgentStartEvent:
		return map[string]any{"type": "agent_start"}, nil
	case agent.SessionAgentEndEvent:
		messages, err := messageListWire(value.Messages)
		return map[string]any{"type": "agent_end", "messages": messages, "willRetry": value.WillRetry}, err
	case agent.TurnStartEvent:
		return map[string]any{"type": "turn_start"}, nil
	case agent.TurnEndEvent:
		message, err := messageWire(value.Message)
		if err != nil {
			return nil, err
		}
		results, err := messageListWire(value.ToolResults)
		return map[string]any{"type": "turn_end", "message": message, "toolResults": results}, err
	case agent.MessageStartEvent:
		message, err := messageWire(value.Message)
		return map[string]any{"type": "message_start", "message": message}, err
	case agent.MessageUpdateEvent:
		message, err := messageWire(value.Message)
		if err != nil {
			return nil, err
		}
		assistantEvent, err := assistantMessageEventWire(value.AssistantMessageEvent)
		return map[string]any{"type": "message_update", "message": message, "assistantMessageEvent": assistantEvent}, err
	case agent.MessageEndEvent:
		message, err := messageWire(value.Message)
		return map[string]any{"type": "message_end", "message": message}, err
	case agent.ToolExecutionStartEvent:
		return map[string]any{
			"type": "tool_execution_start", "toolCallId": value.ToolCallID,
			"toolName": value.ToolName, "args": json.RawMessage(value.Arguments),
		}, nil
	case agent.ToolExecutionUpdateEvent:
		partial, err := toolResultWire(value.PartialResult.Text, value.PartialResult.Content, value.PartialResult.Details, value.PartialResult.Usage, value.PartialResult.AddedToolNames, value.PartialResult.Terminate)
		return map[string]any{
			"type": "tool_execution_update", "toolCallId": value.ToolCallID,
			"toolName": value.ToolName, "args": json.RawMessage(value.Arguments), "partialResult": partial,
		}, err
	case agent.ToolExecutionEndEvent:
		result, err := toolResultWire(value.Result.Text, value.Result.Content, value.Result.Details, value.Result.Usage, value.Result.AddedToolNames, value.Result.Terminate)
		return map[string]any{
			"type": "tool_execution_end", "toolCallId": value.ToolCallID,
			"toolName": value.ToolName, "result": result, "isError": value.IsError,
		}, err
	case agent.AgentSettledEvent:
		return map[string]any{"type": "agent_settled"}, nil
	case agent.SessionQueueUpdateEvent:
		return map[string]any{"type": "queue_update", "steering": nonNilStrings(value.Steering), "followUp": nonNilStrings(value.FollowUp)}, nil
	case agent.ThinkingLevelChangedEvent:
		return map[string]any{"type": "thinking_level_changed", "level": value.Level}, nil
	case agent.CompactionStartEvent:
		return map[string]any{"type": "compaction_start", "reason": value.Reason}, nil
	case agent.CompactionEndEvent:
		result := map[string]any{
			"type": "compaction_end", "reason": value.Reason,
			"aborted": value.Aborted, "willRetry": value.WillRetry,
		}
		if value.Result != nil {
			encoded, err := compactResultWire(*value.Result)
			if err != nil {
				return nil, err
			}
			result["result"] = encoded
		}
		if value.ErrorMessage != "" {
			result["errorMessage"] = value.ErrorMessage
		}
		return result, nil
	case agent.AutoRetryStartEvent:
		return map[string]any{
			"type": "auto_retry_start", "attempt": value.Attempt,
			"maxAttempts": value.MaxAttempts, "delayMs": value.Delay.Milliseconds(),
			"errorMessage": value.ErrorMessage,
		}, nil
	case agent.AutoRetryEndEvent:
		result := map[string]any{"type": "auto_retry_end", "success": value.Success, "attempt": value.Attempt}
		if value.FinalError != "" {
			result["finalError"] = value.FinalError
		}
		return result, nil
	case agent.SessionSummarizationRetryScheduledEvent:
		return map[string]any{
			"type": "summarization_retry_scheduled", "attempt": value.Attempt,
			"maxAttempts": value.MaxAttempts, "delayMs": value.Delay.Milliseconds(),
			"errorMessage": value.ErrorMessage,
		}, nil
	case agent.SessionSummarizationRetryAttemptEvent:
		result := map[string]any{"type": "summarization_retry_attempt_start", "source": value.Source}
		if value.Source == "compaction" {
			result["reason"] = value.Reason
		}
		return result, nil
	case agent.SessionSummarizationRetryFinishedEvent:
		return map[string]any{"type": "summarization_retry_finished"}, nil
	case agent.EntryAppendedEvent:
		return map[string]any{"type": "entry_appended", "entry": json.RawMessage(value.Entry.RawJSON())}, nil
	case agent.SessionInfoChangeEvent:
		result := map[string]any{"type": "session_info_changed"}
		if value.Name != nil {
			result["name"] = *value.Name
		}
		return result, nil
	case agent.BashExecutionUpdateEvent:
		result := map[string]any{"type": "bash_execution_update", "delta": value.Delta}
		if value.ID != nil {
			result["id"] = *value.ID
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported AgentSession event %T", event)
	}
}

func messageWire(value agentmsg.Message) (json.RawMessage, error) {
	if value == nil {
		return json.RawMessage("null"), nil
	}
	return session.MarshalAgentMessage(value)
}

func messageListWire(values []agentmsg.Message) ([]json.RawMessage, error) {
	messages := make([]json.RawMessage, len(values))
	for index, value := range values {
		message, err := messageWire(value)
		if err != nil {
			return nil, err
		}
		messages[index] = message
	}
	return messages, nil
}

func assistantMessageEventWire(value agent.AssistantMessageEvent) (map[string]any, error) {
	partial, err := session.MarshalAgentMessage(value.Partial())
	if err != nil {
		return nil, err
	}
	result := map[string]any{"partial": partial}
	switch event := value.Event().(type) {
	case llm.TextStartEvent:
		result["type"], result["contentIndex"] = "text_start", event.ContentIndex()
	case llm.TextDeltaEvent:
		result["type"], result["contentIndex"], result["delta"] = "text_delta", event.ContentIndex(), event.Delta()
	case llm.TextEndEvent:
		result["type"], result["contentIndex"], result["content"] = "text_end", event.ContentIndex(), event.Content()
	case llm.ThinkingStartEvent:
		result["type"], result["contentIndex"] = "thinking_start", event.ContentIndex()
	case llm.ThinkingDeltaEvent:
		result["type"], result["contentIndex"], result["delta"] = "thinking_delta", event.ContentIndex(), event.Delta()
	case llm.ThinkingEndEvent:
		result["type"], result["contentIndex"], result["content"] = "thinking_end", event.ContentIndex(), event.Content().Thinking()
	case llm.ToolCallStartEvent:
		result["type"], result["contentIndex"] = "toolcall_start", event.ContentIndex()
	case llm.ToolCallDeltaEvent:
		result["type"], result["contentIndex"], result["delta"] = "toolcall_delta", event.ContentIndex(), string(event.Delta())
	case llm.ToolCallEndEvent:
		result["type"], result["contentIndex"] = "toolcall_end", event.ContentIndex()
		call := event.ToolCall()
		toolCall := map[string]any{
			"type": "toolCall", "id": call.ID(), "name": call.Name(),
			"arguments": json.RawMessage(call.ArgumentsJSON()),
		}
		if signature, ok := call.ThoughtSignature(); ok {
			toolCall["thoughtSignature"] = signature
		}
		result["toolCall"] = toolCall
	default:
		return nil, fmt.Errorf("unsupported assistant message event %T", value.Event())
	}
	return result, nil
}

func toolResultWire(text string, content []llm.ToolResultContentBlock, details any, usage *llm.Usage, addedToolNames []string, terminate bool) (map[string]any, error) {
	if len(content) == 0 {
		block, err := llm.NewTextBlock(text)
		if err != nil {
			return nil, err
		}
		content = []llm.ToolResultContentBlock{block}
	}
	encodedContent, err := session.MarshalToolResultContent(content)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"content": encodedContent, "details": details}
	if usage != nil {
		encodedUsage, err := session.MarshalUsage(*usage)
		if err != nil {
			return nil, err
		}
		result["usage"] = encodedUsage
	}
	if addedToolNames != nil {
		result["addedToolNames"] = addedToolNames
	}
	if terminate {
		result["terminate"] = true
	}
	return result, nil
}

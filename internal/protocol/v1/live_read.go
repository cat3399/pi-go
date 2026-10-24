package protocolv1

import (
	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/application"
)

// EncodeLiveSnapshot omits settled history and global event revisions. Each
// owner snapshot remains explicit; its content token covers independently
// sampled activity/controls too, not only Agent.State's revision.
func EncodeLiveSnapshot(value application.LiveSnapshot, active bool) (map[string]any, error) {
	result := map[string]any{"sessionId": value.SessionID, "active": active}
	if !active {
		return result, nil
	}
	read, history, activity := value.State.Agent, value.State.History, value.State.Activity
	state := application.State{
		SessionID: value.SessionID, CWD: history.Info.Cwd, Model: read.Model, HasModel: read.HasModel,
		ThinkingLevel: read.ThinkingLevel, SystemPrompt: read.SystemPrompt, Phase: activity.Phase,
		IsStreaming: activity.IsStreaming, IsPromptRunning: value.PromptRunning, IsBashRunning: value.State.BashRunning,
		IsCompacting: activity.IsCompacting, RetryAttempt: activity.RetryAttempt, RetryWaiting: activity.RetryWaiting,
		SteeringMode: read.SteeringMode, FollowUpMode: read.FollowUpMode,
		AutoCompactionEnabled: value.State.Controls.AutoCompactionEnabled, AutoRetryEnabled: value.State.Controls.AutoRetryEnabled,
		MessageCount: read.MessageCount, PendingMessageCount: len(read.Steering) + len(read.FollowUp), QueuedMessages: read.Queue,
	}
	result["state"], result["version"] = stateWire(state), AgentReadVersion(read.Version)
	result["activityVersion"], result["bashVersion"] = resourceRevision(value.State.ActivityVersion), resourceRevision(value.State.BashVersion)
	result["history"] = map[string]any{"generation": history.Version.Generation, "log": history.Version.Log, "view": history.Version.View, "leafId": history.LeafID}
	result["runId"], result["turn"], result["errorMessage"] = read.RunID, read.Turn, read.ErrorMessage
	project := func(message agent.ReadMessage) (map[string]any, error) {
		encoded, err := messageWire(message.Message)
		return map[string]any{"id": message.ID, "message": encoded}, err
	}
	result["streaming"] = nil
	if read.Streaming != nil {
		message, err := project(*read.Streaming)
		if err != nil {
			return nil, err
		}
		result["streaming"] = message
	}
	pending := make([]map[string]any, 0, len(read.Pending))
	for _, message := range read.Pending {
		item, err := project(message)
		if err != nil {
			return nil, err
		}
		pending = append(pending, item)
	}
	result["pending"] = pending
	tools := make([]map[string]any, 0, len(read.ToolsRunning))
	for _, progress := range read.ToolsRunning {
		item := map[string]any{"toolCallId": progress.ToolCallID, "toolName": progress.ToolName, "args": progress.Arguments}
		if update := progress.Update; update != nil {
			partial, err := toolResultWire(update.Text, update.Content, update.Details, update.Usage, update.AddedToolNames, update.Terminate)
			if err != nil {
				return nil, err
			}
			item["partialResult"] = partial
		}
		tools = append(tools, item)
	}
	result["toolsRunning"] = tools
	return result, nil
}

func resourceRevision(value agent.ResourceRevision) map[string]any {
	return map[string]any{"generation": value.Generation, "revision": value.Revision}
}

func activityWire(value agent.SessionActivity) map[string]any {
	return map[string]any{"phase": value.Phase.String(), "isStreaming": value.IsStreaming, "isCompacting": value.IsCompacting, "retryAttempt": value.RetryAttempt, "retryWaiting": value.RetryWaiting}
}

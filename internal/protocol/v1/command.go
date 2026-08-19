// Package protocolv1 owns the versioned JSON projection used by transport
// adapters above the application API. It never owns Agent or Session state.
package protocolv1

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

type request struct {
	ID                  *string        `json:"id"`
	Type                string         `json:"type"`
	Message             *string        `json:"message"`
	Images              []imageRequest `json:"images"`
	StreamingBehavior   string         `json:"streamingBehavior"`
	Provider            *string        `json:"provider"`
	ModelID             *string        `json:"modelId"`
	Direction           string         `json:"direction"`
	Mode                *string        `json:"mode"`
	EntryID             *string        `json:"entryId"`
	Position            string         `json:"position"`
	TargetID            *string        `json:"targetId"`
	Summarize           bool           `json:"summarize"`
	CustomInstructions  *string        `json:"customInstructions"`
	ReplaceInstructions *bool          `json:"replaceInstructions"`
	Label               *string        `json:"label"`
	Level               *string        `json:"level"`
	Name                *string        `json:"name"`
	Enabled             *bool          `json:"enabled"`
	ToolNames           *[]string      `json:"toolNames"`
	Command             *string        `json:"command"`
	ExcludeFromContext  bool           `json:"excludeFromContext"`
}

type imageRequest struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

type decodedCommand struct {
	id      *string
	typ     string
	command application.Command
}

// DecodeCommand validates one JSON command and returns the corresponding
// transport-neutral application command. HTTP and JSONL transports deliberately
// share this boundary while identifying their prompt source explicitly.
func DecodeCommand(data []byte, promptSource agent.InputSource) (application.Command, error) {
	if !promptSource.Valid() {
		return nil, fmt.Errorf("invalid prompt source %q", promptSource)
	}
	decoded, err := decodeCommand(data, promptSource)
	if err != nil {
		return nil, err
	}
	return decoded.command, nil
}

func decodeCommand(line []byte, promptSource agent.InputSource) (decodedCommand, error) {
	var input request
	if err := json.Unmarshal(line, &input); err != nil {
		return decodedCommand{typ: "parse"}, fmt.Errorf("Failed to parse command: %w", err)
	}
	if strings.TrimSpace(input.Type) == "" {
		return decodedCommand{id: input.ID, typ: ""}, fmt.Errorf("command type is required")
	}
	decoded := decodedCommand{id: input.ID, typ: input.Type}
	requiredString := func(field *string, name string) (string, error) {
		if field == nil || strings.TrimSpace(*field) == "" {
			return "", fmt.Errorf("%s is required", name)
		}
		return *field, nil
	}

	switch input.Type {
	case string(application.CommandPrompt):
		images, err := decodeImages(input.Images)
		if err != nil {
			return decoded, err
		}
		message, err := requiredString(input.Message, "message")
		if err != nil {
			return decoded, err
		}
		behavior := agent.StreamingBehavior(input.StreamingBehavior)
		if !behavior.Valid() {
			return decoded, fmt.Errorf("invalid streamingBehavior %q", input.StreamingBehavior)
		}
		decoded.command = application.PromptCommand{
			Message: message, Images: images, StreamingBehavior: behavior, Source: promptSource,
		}
	case string(application.CommandSteer):
		images, err := decodeImages(input.Images)
		if err != nil {
			return decoded, err
		}
		message, err := requiredString(input.Message, "message")
		if err != nil {
			return decoded, err
		}
		decoded.command = application.SteerCommand{Message: message, Images: images}
	case string(application.CommandFollowUp):
		images, err := decodeImages(input.Images)
		if err != nil {
			return decoded, err
		}
		message, err := requiredString(input.Message, "message")
		if err != nil {
			return decoded, err
		}
		decoded.command = application.FollowUpCommand{Message: message, Images: images}
	case string(application.CommandAbort):
		decoded.command = application.AbortCommand{}
	case string(application.CommandGetState):
		decoded.command = application.GetStateCommand{}
	case string(application.CommandClearQueue):
		decoded.command = application.ClearQueueCommand{}
	case string(application.CommandReload):
		decoded.command = application.ReloadCommand{}
	case string(application.CommandSetModel):
		providerID, err := requiredString(input.Provider, "provider")
		if err != nil {
			return decoded, err
		}
		modelID, err := requiredString(input.ModelID, "modelId")
		if err != nil {
			return decoded, err
		}
		decoded.command = application.SetModelCommand{Provider: providerID, ModelID: modelID}
	case string(application.CommandCycleModel):
		direction := agent.ModelCycleDirection(input.Direction)
		if direction != "" && direction != agent.CycleForward && direction != agent.CycleBackward {
			return decoded, fmt.Errorf("invalid direction %q", input.Direction)
		}
		decoded.command = application.CycleModelCommand{Direction: direction}
	case string(application.CommandGetAvailableModels):
		decoded.command = application.GetAvailableModelsCommand{}
	case string(application.CommandFork):
		entryID, err := requiredString(input.EntryID, "entryId")
		if err != nil {
			return decoded, err
		}
		position := agent.ForkPosition(input.Position)
		if position != "" && position != agent.ForkBefore && position != agent.ForkAt {
			return decoded, fmt.Errorf("invalid fork position %q", position)
		}
		decoded.command = application.ForkCommand{EntryID: entryID, Position: position}
	case string(application.CommandNavigateTree):
		targetID, err := requiredString(input.TargetID, "targetId")
		if err != nil {
			return decoded, err
		}
		decoded.command = application.NavigateTreeCommand{TargetID: targetID, Options: agent.NavigateTreeOptions{
			Summarize: input.Summarize, CustomInstructions: input.CustomInstructions,
			ReplaceInstructions: input.ReplaceInstructions, Label: input.Label,
		}}
	case string(application.CommandSetThinkingLevel):
		level, err := requiredString(input.Level, "level")
		if err != nil {
			return decoded, err
		}
		thinking := provider.ThinkingLevel(level)
		if !thinking.Valid() {
			return decoded, fmt.Errorf("invalid thinking level %q", level)
		}
		decoded.command = application.SetThinkingLevelCommand{Level: thinking}
	case string(application.CommandCycleThinkingLevel):
		decoded.command = application.CycleThinkingLevelCommand{}
	case string(application.CommandGetThinkingLevels):
		decoded.command = application.GetAvailableThinkingLevelsCommand{}
	case string(application.CommandSetSteeringMode):
		mode, err := requiredString(input.Mode, "mode")
		if err != nil {
			return decoded, err
		}
		queueMode, ok := parseQueueMode(mode)
		if !ok {
			return decoded, fmt.Errorf("invalid mode %q", mode)
		}
		decoded.command = application.SetSteeringModeCommand{Mode: queueMode}
	case string(application.CommandSetFollowUpMode):
		mode, err := requiredString(input.Mode, "mode")
		if err != nil {
			return decoded, err
		}
		queueMode, ok := parseQueueMode(mode)
		if !ok {
			return decoded, fmt.Errorf("invalid mode %q", mode)
		}
		decoded.command = application.SetFollowUpModeCommand{Mode: queueMode}
	case string(application.CommandCompact):
		instructions := ""
		if input.CustomInstructions != nil {
			instructions = *input.CustomInstructions
		}
		decoded.command = application.CompactCommand{CustomInstructions: instructions}
	case string(application.CommandAbortCompaction):
		decoded.command = application.AbortCompactionCommand{}
	case string(application.CommandAbortBranchSummary):
		decoded.command = application.AbortBranchSummaryCommand{}
	case string(application.CommandSetSessionName):
		name, err := requiredString(input.Name, "name")
		if err != nil {
			return decoded, err
		}
		decoded.command = application.SetSessionNameCommand{Name: name}
	case string(application.CommandGetSessionStats):
		decoded.command = application.GetSessionStatsCommand{}
	case string(application.CommandGetLastAssistantText):
		decoded.command = application.GetLastAssistantTextCommand{}
	case string(application.CommandSetAutoCompaction):
		if input.Enabled == nil {
			return decoded, fmt.Errorf("enabled is required")
		}
		decoded.command = application.SetAutoCompactionCommand{Enabled: *input.Enabled}
	case string(application.CommandSetAutoRetry):
		if input.Enabled == nil {
			return decoded, fmt.Errorf("enabled is required")
		}
		decoded.command = application.SetAutoRetryCommand{Enabled: *input.Enabled}
	case string(application.CommandAbortRetry):
		decoded.command = application.AbortRetryCommand{}
	case string(application.CommandGetTools):
		decoded.command = application.GetToolsCommand{}
	case string(application.CommandSetTools):
		if input.ToolNames == nil {
			return decoded, fmt.Errorf("toolNames is required")
		}
		decoded.command = application.SetToolsCommand{ToolNames: append([]string(nil), (*input.ToolNames)...)}
	case string(application.CommandBash):
		command, err := requiredString(input.Command, "command")
		if err != nil {
			return decoded, err
		}
		decoded.command = application.BashCommand{Command: command, ExcludeFromContext: input.ExcludeFromContext}
	case string(application.CommandAbortBash):
		decoded.command = application.AbortBashCommand{}
	case string(application.CommandGetCommands):
		decoded.command = application.GetCommandsCommand{}
	default:
		return decoded, fmt.Errorf("Unsupported command: %s", input.Type)
	}
	return decoded, nil
}

func parseQueueMode(value string) (agent.QueueMode, bool) {
	switch value {
	case "all":
		return agent.QueueAll, true
	case "one-at-a-time":
		return agent.QueueOneAtATime, true
	default:
		return 0, false
	}
}

func decodeImages(values []imageRequest) ([]llm.ImageBlock, error) {
	if len(values) == 0 {
		return nil, nil
	}
	images := make([]llm.ImageBlock, 0, len(values))
	for index, value := range values {
		if value.Type != "" && value.Type != "image" {
			return nil, fmt.Errorf("images[%d] has invalid type %q", index, value.Type)
		}
		data, err := base64.StdEncoding.DecodeString(value.Data)
		if err != nil {
			return nil, fmt.Errorf("images[%d] has invalid base64 data", index)
		}
		image, err := llm.NewImageDataBlock(value.MimeType, data)
		if err != nil {
			return nil, fmt.Errorf("images[%d]: %w", index, err)
		}
		images = append(images, image)
	}
	return images, nil
}

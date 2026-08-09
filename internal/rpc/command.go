// Package rpc implements pi's headless JSONL protocol above host.Host. It is
// deliberately a wire adapter: all product state and behavior remain owned by
// Runtime -> AgentSession -> Agent.
package rpc

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/host"
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
	command host.Command
}

func decodeCommand(line []byte) (decodedCommand, error) {
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
	case string(host.CommandPrompt):
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
		decoded.command = host.PromptCommand{Message: message, Images: images, StreamingBehavior: behavior}
	case string(host.CommandSteer):
		images, err := decodeImages(input.Images)
		if err != nil {
			return decoded, err
		}
		message, err := requiredString(input.Message, "message")
		if err != nil {
			return decoded, err
		}
		decoded.command = host.SteerCommand{Message: message, Images: images}
	case string(host.CommandFollowUp):
		images, err := decodeImages(input.Images)
		if err != nil {
			return decoded, err
		}
		message, err := requiredString(input.Message, "message")
		if err != nil {
			return decoded, err
		}
		decoded.command = host.FollowUpCommand{Message: message, Images: images}
	case string(host.CommandAbort):
		decoded.command = host.AbortCommand{}
	case string(host.CommandGetState):
		decoded.command = host.GetStateCommand{}
	case string(host.CommandClearQueue):
		decoded.command = host.ClearQueueCommand{}
	case string(host.CommandReload):
		decoded.command = host.ReloadCommand{}
	case string(host.CommandSetModel):
		providerID, err := requiredString(input.Provider, "provider")
		if err != nil {
			return decoded, err
		}
		modelID, err := requiredString(input.ModelID, "modelId")
		if err != nil {
			return decoded, err
		}
		decoded.command = host.SetModelCommand{Provider: providerID, ModelID: modelID}
	case string(host.CommandFork):
		entryID, err := requiredString(input.EntryID, "entryId")
		if err != nil {
			return decoded, err
		}
		position := agent.ForkPosition(input.Position)
		if position != "" && position != agent.ForkBefore && position != agent.ForkAt {
			return decoded, fmt.Errorf("invalid fork position %q", position)
		}
		decoded.command = host.ForkCommand{EntryID: entryID, Position: position}
	case string(host.CommandNavigateTree):
		targetID, err := requiredString(input.TargetID, "targetId")
		if err != nil {
			return decoded, err
		}
		decoded.command = host.NavigateTreeCommand{TargetID: targetID, Options: agent.NavigateTreeOptions{
			Summarize: input.Summarize, CustomInstructions: input.CustomInstructions,
			ReplaceInstructions: input.ReplaceInstructions, Label: input.Label,
		}}
	case string(host.CommandSetThinkingLevel):
		level, err := requiredString(input.Level, "level")
		if err != nil {
			return decoded, err
		}
		thinking := provider.ThinkingLevel(level)
		if !thinking.Valid() {
			return decoded, fmt.Errorf("invalid thinking level %q", level)
		}
		decoded.command = host.SetThinkingLevelCommand{Level: thinking}
	case string(host.CommandCompact):
		instructions := ""
		if input.CustomInstructions != nil {
			instructions = *input.CustomInstructions
		}
		decoded.command = host.CompactCommand{CustomInstructions: instructions}
	case string(host.CommandAbortCompaction):
		decoded.command = host.AbortCompactionCommand{}
	case string(host.CommandSetSessionName):
		name, err := requiredString(input.Name, "name")
		if err != nil {
			return decoded, err
		}
		decoded.command = host.SetSessionNameCommand{Name: name}
	case string(host.CommandGetSessionStats):
		decoded.command = host.GetSessionStatsCommand{}
	case string(host.CommandGetLastAssistantText):
		decoded.command = host.GetLastAssistantTextCommand{}
	case string(host.CommandSetAutoCompaction):
		if input.Enabled == nil {
			return decoded, fmt.Errorf("enabled is required")
		}
		decoded.command = host.SetAutoCompactionCommand{Enabled: *input.Enabled}
	case string(host.CommandSetAutoRetry):
		if input.Enabled == nil {
			return decoded, fmt.Errorf("enabled is required")
		}
		decoded.command = host.SetAutoRetryCommand{Enabled: *input.Enabled}
	case string(host.CommandGetTools):
		decoded.command = host.GetToolsCommand{}
	case string(host.CommandSetTools):
		if input.ToolNames == nil {
			return decoded, fmt.Errorf("toolNames is required")
		}
		decoded.command = host.SetToolsCommand{ToolNames: append([]string(nil), (*input.ToolNames)...)}
	case string(host.CommandBash):
		command, err := requiredString(input.Command, "command")
		if err != nil {
			return decoded, err
		}
		decoded.command = host.BashCommand{Command: command, ExcludeFromContext: input.ExcludeFromContext}
	case string(host.CommandAbortBash):
		decoded.command = host.AbortBashCommand{}
	case string(host.CommandGetCommands):
		decoded.command = host.GetCommandsCommand{}
	default:
		return decoded, fmt.Errorf("Unsupported command: %s", input.Type)
	}
	return decoded, nil
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

package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/resource"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
)

func promptContent(text string, images []llm.ImageBlock) ([]llm.UserContentBlock, error) {
	block, err := llm.NewTextBlock(text)
	if err != nil {
		return nil, err
	}
	content := make([]llm.UserContentBlock, 0, 1+len(images))
	content = append(content, block)
	for _, image := range images {
		content = append(content, image)
	}
	return content, nil
}

func (h *Host) dispatchSteer(command SteerCommand) (CommandResult, error) {
	session, _, err := h.currentSession()
	if err != nil {
		return nil, err
	}
	if len(command.Images) == 0 {
		err = session.Steer(command.Message)
	} else {
		var content []llm.UserContentBlock
		content, err = promptContent(command.Message, command.Images)
		if err == nil {
			err = session.SteerContent(content)
		}
	}
	if err != nil {
		return nil, err
	}
	return SteerResult{}, nil
}

func (h *Host) dispatchFollowUp(command FollowUpCommand) (CommandResult, error) {
	session, _, err := h.currentSession()
	if err != nil {
		return nil, err
	}
	if len(command.Images) == 0 {
		err = session.FollowUp(command.Message)
	} else {
		var content []llm.UserContentBlock
		content, err = promptContent(command.Message, command.Images)
		if err == nil {
			err = session.FollowUpContent(content)
		}
	}
	if err != nil {
		return nil, err
	}
	return FollowUpResult{}, nil
}

func (h *Host) dispatchSetModel(ctx context.Context, command SetModelCommand) (CommandResult, error) {
	session, _, err := h.currentSession()
	if err != nil {
		return nil, err
	}
	models, err := session.AvailableModels(ctx)
	if err != nil {
		return nil, err
	}
	for _, candidate := range models {
		if candidate.Provider() != command.Provider || candidate.ID() != command.ModelID {
			continue
		}
		if err := session.SetModelContext(ctx, candidate); err != nil {
			return nil, err
		}
		return SetModelResult{Model: candidate}, nil
	}
	return nil, fmt.Errorf("Model not found: %s/%s", command.Provider, command.ModelID)
}

func (h *Host) dispatchFork(ctx context.Context, command ForkCommand) (CommandResult, error) {
	if _, _, err := h.currentSession(); err != nil {
		return nil, err
	}
	result, err := h.runtime.Fork(ctx, command.EntryID, agentruntime.ForkOptions{Position: command.Position})
	if err != nil {
		return nil, err
	}
	hostResult := ForkResult{Cancelled: result.Cancelled, SelectedText: cloneOptionalString(result.SelectedText)}
	if !result.Cancelled {
		if current, _, currentErr := h.currentSession(); currentErr == nil {
			sessionID := current.SessionManager().SessionID()
			hostResult.SessionID = &sessionID
		} else {
			return nil, currentErr
		}
	}
	return hostResult, nil
}

func (h *Host) dispatchNavigateTree(ctx context.Context, command NavigateTreeCommand) (CommandResult, error) {
	session, _, err := h.currentSession()
	if err != nil {
		return nil, err
	}
	result, err := session.NavigateTree(ctx, command.TargetID, command.Options)
	if err != nil {
		return nil, err
	}
	return NavigateTreeResult{
		EditorText: cloneOptionalString(result.EditorText), Cancelled: result.Cancelled,
		Aborted: result.Aborted, SummaryEntry: cloneEntryPointer(result.SummaryEntry),
	}, nil
}

func (h *Host) dispatchSetThinkingLevel(command SetThinkingLevelCommand) (CommandResult, error) {
	session, _, err := h.currentSession()
	if err != nil {
		return nil, err
	}
	if err := session.SetThinkingLevel(command.Level); err != nil {
		return nil, err
	}
	return SetThinkingLevelResult{}, nil
}

func (h *Host) dispatchCompact(ctx context.Context, command CompactCommand) (CommandResult, error) {
	session, _, err := h.currentSession()
	if err != nil {
		return nil, err
	}
	result, err := session.Compact(ctx, command.CustomInstructions)
	if err != nil {
		return nil, err
	}
	return CompactResult{Result: result}, nil
}

func (h *Host) dispatchAbortCompaction() (CommandResult, error) {
	session, _, err := h.currentSession()
	if err != nil {
		return nil, err
	}
	session.AbortCompaction()
	return AbortCompactionResult{}, nil
}

func (h *Host) dispatchSetSessionName(ctx context.Context, command SetSessionNameCommand) (CommandResult, error) {
	name := strings.TrimSpace(command.Name)
	if name == "" {
		return nil, fmt.Errorf("Session name cannot be empty")
	}
	session, _, err := h.currentSession()
	if err != nil {
		return nil, err
	}
	if err := session.SetSessionName(ctx, name); err != nil {
		return nil, err
	}
	return SetSessionNameResult{}, nil
}

func (h *Host) dispatchGetSessionStats() (CommandResult, error) {
	session, _, err := h.currentSession()
	if err != nil {
		return nil, err
	}
	stats, err := session.GetSessionStats()
	if err != nil {
		return nil, err
	}
	result := GetSessionStatsResult{Stats: stats}
	if name, ok := session.SessionName(); ok {
		result.SessionName = stringPointer(name)
	}
	return result, nil
}

func (h *Host) dispatchGetLastAssistantText() (CommandResult, error) {
	session, _, err := h.currentSession()
	if err != nil {
		return nil, err
	}
	text, ok := session.GetLastAssistantText()
	if !ok {
		return GetLastAssistantTextResult{}, nil
	}
	return GetLastAssistantTextResult{Text: stringPointer(text)}, nil
}

func (h *Host) dispatchSetAutoCompaction(command SetAutoCompactionCommand) (CommandResult, error) {
	session, _, err := h.currentSession()
	if err != nil {
		return nil, err
	}
	if err := session.SetAutoCompactionEnabled(command.Enabled); err != nil {
		return nil, err
	}
	return SetAutoCompactionResult{}, nil
}

func (h *Host) dispatchSetAutoRetry(command SetAutoRetryCommand) (CommandResult, error) {
	session, _, err := h.currentSession()
	if err != nil {
		return nil, err
	}
	if err := session.SetAutoRetryEnabled(command.Enabled); err != nil {
		return nil, err
	}
	return SetAutoRetryResult{}, nil
}

func (h *Host) dispatchGetTools() (CommandResult, error) {
	session, _, err := h.currentSession()
	if err != nil {
		return nil, err
	}
	active := make(map[string]struct{}, len(session.ActiveToolNames()))
	for _, name := range session.ActiveToolNames() {
		active[name] = struct{}{}
	}
	definitions := session.AllTools()
	tools := make([]ToolInfo, len(definitions))
	for index, definition := range definitions {
		_, enabled := active[definition.Name()]
		tools[index] = ToolInfo{Name: definition.Name(), Description: definition.Description(), Active: enabled}
	}
	return GetToolsResult{Tools: tools}, nil
}

func (h *Host) dispatchSetTools(command SetToolsCommand) (CommandResult, error) {
	session, _, err := h.currentSession()
	if err != nil {
		return nil, err
	}
	if err := session.SetActiveToolsByName(command.ToolNames); err != nil {
		return nil, err
	}
	return SetToolsResult{}, nil
}

func (h *Host) dispatchBash(ctx context.Context, command BashCommand) (CommandResult, error) {
	session, _, err := h.currentSession()
	if err != nil {
		return nil, err
	}
	result, err := session.ExecuteBash(ctx, command.Command, nil, agent.ExecuteBashOptions{
		ExcludeFromContext: command.ExcludeFromContext,
		ID:                 cloneOptionalString(command.ExecutionID),
	})
	if err != nil {
		return nil, err
	}
	return BashResult{Result: result}, nil
}

func (h *Host) dispatchAbortBash() (CommandResult, error) {
	session, _, err := h.currentSession()
	if err != nil {
		return nil, err
	}
	session.AbortBash()
	return AbortBashResult{}, nil
}

func (h *Host) dispatchGetCommands() (CommandResult, error) {
	for attempt := 0; attempt < 2; attempt++ {
		session, generation, err := h.currentSession()
		if err != nil {
			return nil, err
		}
		services := h.runtime.Services()
		if services == nil || services.ResourceService == nil {
			if h.sameBinding(session, generation) && h.runtime.Session() == session {
				return GetCommandsResult{}, nil
			}
			continue
		}
		snapshot, err := services.ResourceService.Snapshot()
		if err != nil {
			if h.sameBinding(session, generation) && h.runtime.Session() == session {
				return nil, err
			}
			continue
		}
		commands := resourceCommands(snapshot)
		if h.sameBinding(session, generation) && h.runtime.Session() == session {
			return GetCommandsResult{Commands: commands}, nil
		}
	}
	return nil, ErrSessionChanged
}

func resourceCommands(snapshot resource.Snapshot) []SlashCommandInfo {
	commands := make([]SlashCommandInfo, 0, len(snapshot.Templates)+len(snapshot.Skills))
	for _, template := range snapshot.Templates {
		commands = append(commands, SlashCommandInfo{
			Name: template.Name, Description: template.Description, Source: CommandSourcePrompt, SourceInfo: template.Source,
		})
	}
	for _, skill := range snapshot.Skills {
		commands = append(commands, SlashCommandInfo{
			Name: "skill:" + skill.Name, Description: skill.Description, Source: CommandSourceSkill, SourceInfo: skill.Source,
		})
	}
	return commands
}

func cloneOptionalString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneEntryPointer(value *session.Entry) *session.Entry {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

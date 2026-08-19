package application

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

func (s *ApplicationSession) dispatchSteer(command SteerCommand) (CommandResult, error) {
	session, _, err := s.currentSession()
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

func (s *ApplicationSession) dispatchFollowUp(command FollowUpCommand) (CommandResult, error) {
	session, _, err := s.currentSession()
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

func (s *ApplicationSession) dispatchSetModel(ctx context.Context, command SetModelCommand) (CommandResult, error) {
	session, _, err := s.currentSession()
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

func (s *ApplicationSession) dispatchCycleModel(ctx context.Context, command CycleModelCommand) (CommandResult, error) {
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	result, err := session.CycleModel(ctx, command.Direction)
	if err != nil {
		return nil, err
	}
	return CycleModelResult{Result: result}, nil
}

func (s *ApplicationSession) dispatchGetAvailableModels(ctx context.Context) (CommandResult, error) {
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	models, err := session.AvailableModels(ctx)
	if err != nil {
		return nil, err
	}
	return GetAvailableModelsResult{Models: models}, nil
}

func (s *ApplicationSession) dispatchFork(ctx context.Context, command ForkCommand) (CommandResult, error) {
	if _, _, err := s.currentSession(); err != nil {
		return nil, err
	}
	result, err := s.runtime.Fork(ctx, command.EntryID, agentruntime.ForkOptions{Position: command.Position})
	if err != nil {
		return nil, err
	}
	applicationResult := ForkResult{Cancelled: result.Cancelled, SelectedText: cloneOptionalString(result.SelectedText)}
	if !result.Cancelled {
		if current, _, currentErr := s.currentSession(); currentErr == nil {
			sessionID := current.SessionManager().SessionID()
			applicationResult.SessionID = &sessionID
		} else {
			return nil, currentErr
		}
	}
	return applicationResult, nil
}

func (s *ApplicationSession) dispatchNavigateTree(ctx context.Context, command NavigateTreeCommand) (CommandResult, error) {
	session, _, err := s.currentSession()
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

func (s *ApplicationSession) dispatchSetThinkingLevel(command SetThinkingLevelCommand) (CommandResult, error) {
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	if err := session.SetThinkingLevel(command.Level); err != nil {
		return nil, err
	}
	return SetThinkingLevelResult{}, nil
}

func (s *ApplicationSession) dispatchCycleThinkingLevel() (CommandResult, error) {
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	level, err := session.CycleThinkingLevel()
	if err != nil {
		return nil, err
	}
	return CycleThinkingLevelResult{Level: level}, nil
}

func (s *ApplicationSession) dispatchGetAvailableThinkingLevels() (CommandResult, error) {
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	return GetAvailableThinkingLevelsResult{Levels: session.AvailableThinkingLevels()}, nil
}

func (s *ApplicationSession) dispatchSetSteeringMode(command SetSteeringModeCommand) (CommandResult, error) {
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	if err := session.SetSteeringMode(command.Mode); err != nil {
		return nil, err
	}
	return SetSteeringModeResult{}, nil
}

func (s *ApplicationSession) dispatchSetFollowUpMode(command SetFollowUpModeCommand) (CommandResult, error) {
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	if err := session.SetFollowUpMode(command.Mode); err != nil {
		return nil, err
	}
	return SetFollowUpModeResult{}, nil
}

func (s *ApplicationSession) dispatchCompact(ctx context.Context, command CompactCommand) (CommandResult, error) {
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	result, err := session.Compact(ctx, command.CustomInstructions)
	if err != nil {
		return nil, err
	}
	return CompactResult{Result: result}, nil
}

func (s *ApplicationSession) dispatchAbortCompaction() (CommandResult, error) {
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	session.AbortCompaction()
	return AbortCompactionResult{}, nil
}

func (s *ApplicationSession) dispatchAbortBranchSummary() (CommandResult, error) {
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	session.AbortBranchSummary()
	return AbortBranchSummaryResult{}, nil
}

func (s *ApplicationSession) dispatchSetSessionName(ctx context.Context, command SetSessionNameCommand) (CommandResult, error) {
	name := strings.TrimSpace(command.Name)
	if name == "" {
		return nil, fmt.Errorf("Session name cannot be empty")
	}
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	if err := session.SetSessionName(ctx, name); err != nil {
		return nil, err
	}
	return SetSessionNameResult{}, nil
}

func (s *ApplicationSession) dispatchGetSessionStats() (CommandResult, error) {
	session, _, err := s.currentSession()
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

func (s *ApplicationSession) dispatchGetLastAssistantText() (CommandResult, error) {
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	text, ok := session.GetLastAssistantText()
	if !ok {
		return GetLastAssistantTextResult{}, nil
	}
	return GetLastAssistantTextResult{Text: stringPointer(text)}, nil
}

func (s *ApplicationSession) dispatchSetAutoCompaction(command SetAutoCompactionCommand) (CommandResult, error) {
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	if err := session.SetAutoCompactionEnabled(command.Enabled); err != nil {
		return nil, err
	}
	return SetAutoCompactionResult{}, nil
}

func (s *ApplicationSession) dispatchSetAutoRetry(command SetAutoRetryCommand) (CommandResult, error) {
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	if err := session.SetAutoRetryEnabled(command.Enabled); err != nil {
		return nil, err
	}
	return SetAutoRetryResult{}, nil
}

func (s *ApplicationSession) dispatchAbortRetry() (CommandResult, error) {
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	session.AbortRetry()
	return AbortRetryResult{}, nil
}

func (s *ApplicationSession) dispatchGetTools() (CommandResult, error) {
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	active := make(map[string]struct{}, len(session.ActiveToolNames()))
	for _, name := range session.ActiveToolNames() {
		active[name] = struct{}{}
	}
	definitions := session.AllToolInfo()
	tools := make([]ToolInfo, len(definitions))
	for index, info := range definitions {
		definition := info.Definition
		_, enabled := active[definition.Name()]
		tools[index] = ToolInfo{
			Name: definition.Name(), Description: definition.Description(),
			Parameters: definition.ParametersJSON(), PromptGuidelines: append([]string(nil), info.PromptGuidelines...),
			SourceInfo: info.SourceInfo, Active: enabled,
		}
	}
	return GetToolsResult{Tools: tools}, nil
}

func (s *ApplicationSession) dispatchSetTools(command SetToolsCommand) (CommandResult, error) {
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	if err := session.SetActiveToolsByName(command.ToolNames); err != nil {
		return nil, err
	}
	return SetToolsResult{}, nil
}

func (s *ApplicationSession) dispatchBash(ctx context.Context, command BashCommand) (CommandResult, error) {
	session, _, err := s.currentSession()
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

func (s *ApplicationSession) dispatchAbortBash() (CommandResult, error) {
	session, _, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	session.AbortBash()
	return AbortBashResult{}, nil
}

func (s *ApplicationSession) dispatchGetCommands() (CommandResult, error) {
	for attempt := 0; attempt < 2; attempt++ {
		session, generation, err := s.currentSession()
		if err != nil {
			return nil, err
		}
		services := s.runtime.Services()
		if services == nil || services.ResourceService == nil {
			if s.sameBinding(session, generation) && s.runtime.Session() == session {
				return GetCommandsResult{}, nil
			}
			continue
		}
		snapshot, err := services.ResourceService.Snapshot()
		if err != nil {
			if s.sameBinding(session, generation) && s.runtime.Session() == session {
				return nil, err
			}
			continue
		}
		commands := resourceCommands(snapshot)
		if s.sameBinding(session, generation) && s.runtime.Session() == session {
			return GetCommandsResult{Commands: commands}, nil
		}
	}
	return nil, ErrSessionChanged
}

func resourceCommands(snapshot resource.Snapshot) []SlashCommandInfo {
	commands := make([]SlashCommandInfo, 0, len(snapshot.Templates)+len(snapshot.Skills))
	for _, template := range snapshot.Templates {
		commands = append(commands, SlashCommandInfo{
			Name: template.Name, Description: template.Description, ArgumentHint: template.ArgumentHint,
			Source: CommandSourcePrompt, SourceInfo: template.Source,
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

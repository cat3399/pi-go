package agentruntime

import (
	"context"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/resource"
)

type sessionResources struct {
	service      *resource.Service
	resolvePaths func() (skillPaths, promptPaths []string)
}

func newSessionResources(service *resource.Service, resolvePaths func() (skillPaths, promptPaths []string)) agent.SessionResources {
	if service == nil {
		return nil
	}
	return sessionResources{service: service, resolvePaths: resolvePaths}
}

func (r sessionResources) BuildSystemPrompt(activeToolNames []string) (string, agent.BuildSystemPromptOptions, error) {
	prompt, options, err := r.service.BuildSystemPromptForTools(activeToolNames)
	if err != nil {
		return "", agent.BuildSystemPromptOptions{}, err
	}
	return prompt, agentSystemPromptOptions(options), nil
}

func (r sessionResources) ExpandPromptInput(text string) (string, error) {
	return r.service.ExpandInput(text)
}

func (r sessionResources) Reload(ctx context.Context) error {
	if r.resolvePaths != nil {
		skills, prompts := r.resolvePaths()
		return r.service.ReloadAdditionalPaths(ctx, skills, prompts)
	}
	return r.service.Reload(ctx)
}

func agentSystemPromptOptions(options resource.BuildSystemPromptOptions) agent.BuildSystemPromptOptions {
	converted := agent.BuildSystemPromptOptions{
		CustomPrompt:     cloneRuntimeString(options.CustomPrompt),
		SelectedTools:    cloneRuntimeStrings(options.SelectedTools),
		ToolSnippets:     make(map[string]string, len(options.ToolSnippets)),
		PromptGuidelines: append([]string(nil), options.PromptGuidelines...),
		CWD:              options.CWD,
	}
	if options.AppendSystemPrompt != "" {
		appendPrompt := options.AppendSystemPrompt
		converted.AppendSystemPrompt = &appendPrompt
	}
	for name, snippet := range options.ToolSnippets {
		converted.ToolSnippets[name] = snippet
	}
	converted.ContextFiles = make([]agent.SystemPromptContextFile, len(options.ContextFiles))
	for index, contextFile := range options.ContextFiles {
		converted.ContextFiles[index] = agent.SystemPromptContextFile{Path: contextFile.Path, Content: contextFile.Content}
	}
	converted.Skills = make([]agent.SystemPromptSkill, len(options.Skills))
	for index, skill := range options.Skills {
		converted.Skills[index] = agent.SystemPromptSkill{
			Name: skill.Name, Description: skill.Description, FilePath: skill.Path, BaseDir: skill.BaseDir,
			DisableModelInvocation: skill.DisableModelInvocation,
			SourceInfo: agent.SystemPromptSourceInfo{
				Path: skill.Path, Source: skill.Source.Source,
				Scope: agent.SystemPromptSourceScope(skill.Source.Scope), Origin: agent.SystemPromptSourceOrigin(skill.Source.Origin),
				BaseDir: runtimeStringPointer(skill.Source.BaseDir),
			},
		}
	}
	return converted
}

func cloneRuntimeString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func runtimeStringPointer(value string) *string {
	if value == "" {
		return nil
	}
	cloned := value
	return &cloned
}

func cloneRuntimeStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}

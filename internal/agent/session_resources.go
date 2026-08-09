package agent

import (
	"context"
	"fmt"

	"github.com/cat3399/pi-go/internal/provider"
)

// SessionResources is the product resource boundary owned by AgentSession.
// Implementations retain the last healthy discovery snapshot and must not
// perform filesystem discovery from BuildSystemPrompt or ExpandPromptInput.
// Reload is the only operation that publishes a new snapshot.
type SessionResources interface {
	BuildSystemPrompt(activeToolNames []string) (string, BuildSystemPromptOptions, error)
	ExpandPromptInput(text string) (string, error)
	Reload(context.Context) error
}

func buildToolCatalog(active, all []provider.ToolDefinition, requested []string) (
	[]provider.ToolDefinition,
	map[string]provider.ToolDefinition,
	[]string,
	error,
) {
	if all == nil {
		all = active
	}
	registry := make(map[string]provider.ToolDefinition, len(all))
	order := make([]string, 0, len(all))
	for _, definition := range all {
		name := definition.Name()
		if name == "" {
			return nil, nil, nil, fmt.Errorf("%w: tool registry contains an unnamed definition", ErrInvalidConfig)
		}
		if _, exists := registry[name]; exists {
			return nil, nil, nil, fmt.Errorf("%w: duplicate tool %q", ErrInvalidConfig, name)
		}
		registry[name] = definition
		order = append(order, name)
	}
	if requested == nil {
		requested = make([]string, len(active))
		for index, definition := range active {
			requested[index] = definition.Name()
		}
	}
	selected := selectToolDefinitions(registry, requested)
	return selected, registry, order, nil
}

func selectToolDefinitions(registry map[string]provider.ToolDefinition, names []string) []provider.ToolDefinition {
	selected := make([]provider.ToolDefinition, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		definition, exists := registry[name]
		if !exists {
			continue
		}
		seen[name] = struct{}{}
		selected = append(selected, definition)
	}
	return selected
}

func toolDefinitionNames(definitions []provider.ToolDefinition) []string {
	names := make([]string, len(definitions))
	for index, definition := range definitions {
		names[index] = definition.Name()
	}
	return names
}

// Package app owns the headless product workflow: CLI admission, dependency
// assembly, signal lifetime, output, exit status, and durable session teardown.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/resource"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
	"github.com/cat3399/pi-go/internal/tool"
)

var (
	ErrInvalidArguments           = errors.New("invalid application arguments")
	ErrInvalidDependencies        = errors.New("invalid application dependencies")
	ErrInvalidProductionConfig    = errors.New("invalid production configuration")
	ErrUnsupportedProductionValue = errors.New("unsupported production configuration value")
)

const (
	ExitSuccess = 0
	ExitFailure = 1
)

// SessionPathFactory supplies the default durable path when --session is not
// present. It is an application dependency because the future settings module,
// not the agent or session domain, will own the product's default location.
type SessionPathFactory func(workingDir string) (string, error)

// Dependencies is the production assembly boundary consumed by Run. Tests may
// inject a deterministic provider, clock, ID source, and Bash runner; none are
// selectable through ordinary CLI arguments.
type Dependencies struct {
	Provider           provider.Provider
	Model              provider.Model
	Stream             provider.StreamOptions
	Hooks              agent.Hooks
	SystemPrompt       string
	WorkingDir         string
	AgentDir           string
	DocsDir            string
	DefaultSessionPath SessionPathFactory

	SessionID         string
	SessionNow        session.Clock
	SessionCreateTime time.Time
	NewSessionEntryID session.IDGenerator
	AgentNow          func() time.Time
	SettlementTimeout time.Duration

	BashRunner            tool.Runner
	BashEnvironment       []string
	BashShellPath         string
	BashArtifactDirectory string
	BashMaxOutputLines    int
	BashMaxOutputBytes    int
	// ExpandPrompt is an already-admitted, pure input transform for injected
	// front ends. It runs after session/runtime construction and before prompt
	// admission or any provider call.
	ExpandPrompt func(string) string
}

type runtimeDependencies struct {
	workingDir         string
	agentDir           string
	defaultSessionPath SessionPathFactory
	sessionID          string
	sessionNow         session.Clock
	sessionCreateTime  time.Time
	newSessionEntryID  session.IDGenerator
	expandPrompt       func(string) string
	factory            agentruntime.Factory
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copy := make(map[string]string, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}

func validateDependencies(deps Dependencies) (runtimeDependencies, error) {
	if isNilInterface(deps.Provider) {
		return runtimeDependencies{}, fmt.Errorf("%w: provider is required", ErrInvalidDependencies)
	}
	if _, err := provider.NewRequest(deps.Model, deps.SystemPrompt, nil); err != nil {
		return runtimeDependencies{}, fmt.Errorf("%w: %w", ErrInvalidDependencies, err)
	}
	if deps.SessionID != "" {
		if err := session.ValidateSessionID(deps.SessionID); err != nil {
			return runtimeDependencies{}, fmt.Errorf("%w: %w", ErrInvalidDependencies, err)
		}
	}
	if deps.SettlementTimeout < 0 {
		return runtimeDependencies{}, fmt.Errorf("%w: settlement timeout cannot be negative", ErrInvalidDependencies)
	}
	if isNilInterface(deps.BashRunner) && deps.BashRunner != nil {
		return runtimeDependencies{}, fmt.Errorf("%w: bash runner is a typed nil", ErrInvalidDependencies)
	}

	workingDir, err := resolveWorkingDirectory(deps.WorkingDir)
	if err != nil {
		return runtimeDependencies{}, fmt.Errorf("%w: %w", ErrInvalidDependencies, err)
	}

	environment := deps.BashEnvironment
	if environment == nil {
		environment = os.Environ()
	}

	bashOptions := tool.BashOptions{
		WorkingDir:        workingDir,
		Environment:       append([]string(nil), environment...),
		Runner:            deps.BashRunner,
		ShellPath:         deps.BashShellPath,
		ArtifactDirectory: deps.BashArtifactDirectory,
		MaxOutputLines:    deps.BashMaxOutputLines,
		MaxOutputBytes:    deps.BashMaxOutputBytes,
	}
	agentDir := deps.AgentDir
	if agentDir == "" {
		agentDir = workingDir
	}
	resolvedAgentDir, err := filepath.Abs(agentDir)
	if err != nil {
		return runtimeDependencies{}, fmt.Errorf("%w: resolve agent directory: %w", ErrInvalidDependencies, err)
	}
	catalogModel := catalogModelFromProvider(deps.Model)
	factory := func(ctx context.Context, options agentruntime.CreateOptions) (agentruntime.CreateResult, error) {
		toolOptions := bashOptions
		toolOptions.WorkingDir = options.SessionManager.Cwd()
		executor, definitions, _, standaloneBash, err := buildProductionToolRuntime(productionToolRuntimeOptions{
			Bash: toolOptions, Filesystem: tool.FilesystemOptions{WorkingDir: toolOptions.WorkingDir},
		})
		if err != nil {
			return agentruntime.CreateResult{}, fmt.Errorf("initialize session tool runtime: %w", err)
		}
		supportsRoute := func(candidate model.Model) bool {
			ref, refErr := candidate.Ref()
			if refErr != nil {
				return false
			}
			if routes, ok := deps.Provider.(provider.RouteValidator); ok {
				return routes.SupportsModel(ref)
			}
			return ref.Equal(deps.Model)
		}
		services := &agentruntime.Services{
			CWD: options.SessionManager.Cwd(), AgentDir: resolvedAgentDir,
			Provider: deps.Provider, Tool: executor, Tools: append([]provider.ToolDefinition(nil), definitions...), StandaloneBash: standaloneBash,
		}
		stream := provider.CloneStreamOptions(deps.Stream)
		stream.SessionID = options.SessionManager.SessionID()
		return agentruntime.CreateAgentSession(ctx, agentruntime.SessionFactoryOptions{
			Services: services, Provider: deps.Provider, SessionManager: options.SessionManager,
			AllModels: []model.Model{catalogModel}, Availability: model.Availability{
				HasConfiguredAuth: func(string) bool { return true }, SupportsRoute: supportsRoute,
			},
			ExplicitModel: &catalogModel,
			BaseConfig: agent.SessionConfig{
				SystemPrompt: deps.SystemPrompt, Tool: executor, Tools: definitions,
				Stream: stream, Hooks: deps.Hooks, Now: deps.AgentNow, SettlementTimeout: deps.SettlementTimeout,
			},
			SessionStartEvent: options.SessionStartEvent, DocsDir: deps.DocsDir,
		})
	}

	return runtimeDependencies{
		workingDir:         filepath.Clean(workingDir),
		agentDir:           filepath.Clean(resolvedAgentDir),
		defaultSessionPath: deps.DefaultSessionPath,
		sessionID:          deps.SessionID,
		sessionNow:         deps.SessionNow,
		sessionCreateTime:  deps.SessionCreateTime,
		newSessionEntryID:  deps.NewSessionEntryID,
		expandPrompt:       deps.ExpandPrompt,
		factory:            factory,
	}, nil
}

func catalogModelFromProvider(value provider.Model) model.Model {
	return model.Model{
		Provider: value.Provider(), API: value.API(), ID: value.ID(), Name: value.Name(), BaseURL: value.BaseURL(),
		Headers: value.Headers(), Reasoning: value.Reasoning(), ThinkingLevelMap: value.ThinkingLevelMap(),
		Input: value.Input(), Cost: value.Cost(), ContextWindow: value.ContextWindow(), MaxTokens: value.MaxTokens(), Compat: value.Compat(),
	}
}

func resolveWorkingDirectory(path string) (string, error) {
	if path == "" {
		path = "."
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	resolved = filepath.Clean(resolved)
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect working directory: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("working directory is not a directory")
	}
	return resolved, nil
}

type productionToolRuntimeOptions struct {
	Bash       tool.BashOptions
	Filesystem tool.FilesystemOptions
}

func buildProductionToolRuntime(options productionToolRuntimeOptions) (agent.ToolExecutor, []provider.ToolDefinition, []resource.Tool, agent.StandaloneBashExecutor, error) {
	bash, err := tool.NewBash(options.Bash)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	standaloneBash, err := agent.NewBashExecutor(bash)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	filesystem, err := tool.NewFilesystemSuite(options.Filesystem)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	registry, err := tool.NewBuiltInRegistry(bash, filesystem)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	executor, err := agent.NewRegistryExecutor(registry)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	specifications := registry.Specifications()
	definitions := make([]provider.ToolDefinition, len(specifications))
	resourceTools := make([]resource.Tool, len(specifications))
	for index, specification := range specifications {
		definition, err := provider.NewToolDefinition(
			specification.Name(), specification.Description(), specification.Strict(), specification.ParametersJSON(),
		)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		definitions[index] = definition
		resourceTools[index] = resource.Tool{
			Name: specification.Name(), Snippet: specification.PromptSnippet(), PromptGuidelines: specification.PromptGuidelines(),
		}
	}
	return executor, definitions, resourceTools, standaloneBash, nil
}

func defaultActiveToolNames() []string {
	return []string{tool.ReadToolName, tool.BashToolName, tool.EditToolName, tool.WriteToolName}
}

func selectProductionToolDefinitions(all []provider.ToolDefinition, names []string) []provider.ToolDefinition {
	registry := make(map[string]provider.ToolDefinition, len(all))
	for _, definition := range all {
		registry[definition.Name()] = definition
	}
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

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

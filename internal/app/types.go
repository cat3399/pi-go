// Package app owns the headless product workflow: CLI admission, dependency
// assembly, signal lifetime, output, exit status, and durable session teardown.
package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/provider"
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
	Model              provider.ModelRef
	Stream             provider.StreamOptions
	Hooks              agent.Hooks
	SystemPrompt       string
	WorkingDir         string
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
	// ExpandPrompt is an already-admitted, pure input transform. It exists for
	// trusted local prompt templates and runs before any session/network work.
	ExpandPrompt func(string) string
}

type runtimeDependencies struct {
	provider           provider.Provider
	model              provider.ModelRef
	stream             provider.StreamOptions
	hooks              agent.Hooks
	systemPrompt       string
	workingDir         string
	defaultSessionPath SessionPathFactory
	sessionID          string
	sessionNow         session.Clock
	sessionCreateTime  time.Time
	newSessionEntryID  session.IDGenerator
	agentNow           func() time.Time
	settlementTimeout  time.Duration
	executor           agent.ToolExecutor
	toolDefinitions    []provider.ToolDefinition
	bashOptions        tool.BashOptions
	expandPrompt       func(string) string
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
	executor, definitions, err := buildProductionToolRuntime(bashOptions)
	if err != nil {
		return runtimeDependencies{}, fmt.Errorf("%w: %w", ErrInvalidDependencies, err)
	}

	return runtimeDependencies{
		provider:           deps.Provider,
		model:              deps.Model,
		stream:             provider.CloneStreamOptions(deps.Stream),
		hooks:              deps.Hooks,
		systemPrompt:       deps.SystemPrompt,
		workingDir:         filepath.Clean(workingDir),
		defaultSessionPath: deps.DefaultSessionPath,
		sessionID:          deps.SessionID,
		sessionNow:         deps.SessionNow,
		sessionCreateTime:  deps.SessionCreateTime,
		newSessionEntryID:  deps.NewSessionEntryID,
		agentNow:           deps.AgentNow,
		settlementTimeout:  deps.SettlementTimeout,
		executor:           executor,
		toolDefinitions:    definitions,
		bashOptions:        bashOptions,
		expandPrompt:       deps.ExpandPrompt,
	}, nil
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

func (r runtimeDependencies) executorFor(workingDir string) (agent.ToolExecutor, []provider.ToolDefinition, error) {
	if filepath.Clean(workingDir) == r.workingDir {
		return r.executor, append([]provider.ToolDefinition(nil), r.toolDefinitions...), nil
	}
	options := r.bashOptions
	options.WorkingDir = workingDir
	return buildProductionToolRuntime(options)
}

func buildProductionToolRuntime(options tool.BashOptions) (agent.ToolExecutor, []provider.ToolDefinition, error) {
	bash, err := tool.NewBash(options)
	if err != nil {
		return nil, nil, err
	}
	filesystem, err := tool.NewFilesystemSuite(tool.FilesystemOptions{WorkingDir: options.WorkingDir})
	if err != nil {
		return nil, nil, err
	}
	registry, err := tool.NewBuiltInRegistry(bash, filesystem)
	if err != nil {
		return nil, nil, err
	}
	executor, err := agent.NewRegistryExecutor(registry)
	if err != nil {
		return nil, nil, err
	}
	specifications := registry.Specifications()
	definitions := make([]provider.ToolDefinition, len(specifications))
	for index, specification := range specifications {
		definition, err := provider.NewToolDefinition(
			specification.Name(), specification.Description(), specification.Strict(), specification.ParametersJSON(),
		)
		if err != nil {
			return nil, nil, err
		}
		definitions[index] = definition
	}
	return executor, definitions, nil
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

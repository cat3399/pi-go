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
}

type runtimeDependencies struct {
	provider           provider.Provider
	model              provider.ModelRef
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
	bashOptions        tool.BashOptions
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
	bash, err := tool.NewBash(bashOptions)
	if err != nil {
		return runtimeDependencies{}, fmt.Errorf("%w: %w", ErrInvalidDependencies, err)
	}
	executor, err := agent.NewBashExecutor(bash)
	if err != nil {
		return runtimeDependencies{}, fmt.Errorf("%w: %w", ErrInvalidDependencies, err)
	}

	return runtimeDependencies{
		provider:           deps.Provider,
		model:              deps.Model,
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
		bashOptions:        bashOptions,
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

func (r runtimeDependencies) executorFor(workingDir string) (agent.ToolExecutor, error) {
	if filepath.Clean(workingDir) == r.workingDir {
		return r.executor, nil
	}
	options := r.bashOptions
	options.WorkingDir = workingDir
	bash, err := tool.NewBash(options)
	if err != nil {
		return nil, err
	}
	return agent.NewBashExecutor(bash)
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

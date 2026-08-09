package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	agentruntime "github.com/cat3399/pi-go/internal/runtime"
	"github.com/cat3399/pi-go/internal/session"
)

// Run executes the headless workflow with an already assembled dependency set.
// Product-facing provider/model/auth flags are admitted only by RunProduction;
// this injected boundary remains available for deterministic workflow tests.
func Run(
	ctx context.Context,
	deps Dependencies,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) (exitCode int) {
	return runApplication(ctx, args, stdout, stderr, func(
		_ context.Context,
		parsed options,
	) (runtimeDependencies, error) {
		if parsed.hasProductionSelection() {
			return runtimeDependencies{}, fmt.Errorf(
				"%w: provider, model, and API-key flags require the production assembler",
				ErrInvalidArguments,
			)
		}
		return validateDependencies(deps)
	})
}

type dependencyBuilder func(context.Context, options) (runtimeDependencies, error)

func runApplication(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	build dependencyBuilder,
) (exitCode int) {
	if ctx == nil {
		ctx = context.Background()
	}
	if isNilInterface(stdout) || isNilInterface(stderr) {
		if !isNilInterface(stderr) {
			writeDiagnostic(stderr, errors.New("stdout and stderr writers are required"))
			flushOne(stderr)
		}
		return ExitFailure
	}

	parsed, err := parseArgs(args)
	if err != nil {
		writeDiagnostic(stderr, err)
		flushOne(stderr)
		return ExitFailure
	}
	runtime, err := build(ctx, parsed)
	if err != nil {
		writeDiagnostic(stderr, err)
		flushOne(stderr)
		return ExitFailure
	}
	runContext, signals := startSignalController(ctx)
	defer func() {
		if signalCode, caught := signals.stopAndExitCode(); caught {
			exitCode = signalCode
		}
	}()
	defer func() {
		if err := flushOne(stdout); err != nil {
			writeDiagnostic(stderr, fmt.Errorf("flush stdout: %w", err))
			if exitCode == ExitSuccess {
				exitCode = ExitFailure
			}
		}
		if err := flushOne(stderr); err != nil && exitCode == ExitSuccess {
			exitCode = ExitFailure
		}
	}()
	if cause := context.Cause(runContext); cause != nil {
		if signalCode, caught := signals.exitCode(); caught {
			return signalCode
		}
		writeDiagnostic(stderr, fmt.Errorf("application context cancelled: %w", cause))
		return ExitFailure
	}

	sessionPath, err := resolveSessionPath(parsed.sessionPath, runtime)
	if err != nil {
		writeDiagnostic(stderr, err)
		return ExitFailure
	}
	sessionManager, err := openOrCreateSessionManager(runContext, sessionPath, runtime)
	if err != nil {
		if signalCode, caught := signals.exitCode(); caught {
			return signalCode
		}
		writeDiagnostic(stderr, err)
		return ExitFailure
	}
	managerOwnedByCoordinator := false
	defer func() {
		if !managerOwnedByCoordinator {
			if err := sessionManager.Close(); err != nil {
				writeDiagnostic(stderr, fmt.Errorf("close session: %w", err))
				if exitCode == ExitSuccess {
					exitCode = ExitFailure
				}
			}
		}
	}()
	if err := agentruntime.AssertSessionCwdExists(sessionManager, runtime.workingDir); err != nil {
		writeDiagnostic(stderr, err)
		return ExitFailure
	}

	productRuntime, err := agentruntime.Create(runContext, runtime.factory, agentruntime.InitialOptions{
		CWD: sessionManager.Cwd(), AgentDir: runtime.agentDir, SessionManager: sessionManager,
	})
	if err != nil {
		writeDiagnostic(stderr, fmt.Errorf("initialize agent runtime: %w", err))
		return ExitFailure
	}
	managerOwnedByCoordinator = true
	defer func() {
		if err := productRuntime.Dispose(context.Background()); err != nil && exitCode == ExitSuccess {
			writeDiagnostic(stderr, fmt.Errorf("close agent runtime: %w", err))
			exitCode = ExitFailure
		}
	}()
	coordinator := productRuntime.Session()
	if reportRuntimeDiagnostics(stderr, productRuntime.Diagnostics()) {
		return ExitFailure
	}
	if !coordinator.HasModel() {
		if guidance := productRuntime.ModelFallbackMessage(); guidance != nil {
			writeTextLine(stderr, *guidance)
		} else {
			writeDiagnostic(stderr, agent.ErrNoModelSelected)
		}
		return ExitFailure
	}
	prompt := parsed.prompt
	if services := productRuntime.Services(); (services == nil || services.ResourceService == nil) && runtime.expandPrompt != nil {
		prompt = runtime.expandPrompt(prompt)
	}

	result, runErr := coordinator.Run(runContext, prompt)
	if signalCode, caught := signals.exitCode(); caught {
		if runErr != nil && !errors.Is(runErr, agent.ErrInvalidRun) {
			writeDiagnostic(stderr, runErr)
		}
		return signalCode
	}
	if runErr != nil {
		if errors.Is(runErr, agent.ErrModelAccess) {
			var accessErr *agent.ModelAccessError
			if errors.As(runErr, &accessErr) {
				writeTextLine(stderr, safeErrorText(accessErr))
			} else {
				writeTextLine(stderr, safeErrorText(runErr))
			}
		} else {
			writeDiagnostic(stderr, runErr)
		}
		return ExitFailure
	}
	if result.Handled() {
		return ExitSuccess
	}
	terminal, ok := result.Terminal()
	if !ok {
		writeDiagnostic(stderr, errors.New("agent completed without a terminal result"))
		return ExitFailure
	}
	return renderTerminal(terminal, stdout, stderr)
}

func resolveSessionPath(explicit string, runtime runtimeDependencies) (string, error) {
	path := explicit
	if path == "" {
		if runtime.defaultSessionPath == nil {
			return "", fmt.Errorf("%w: default session path is not configured", ErrInvalidDependencies)
		}
		var err error
		path, err = runtime.defaultSessionPath(runtime.workingDir)
		if err != nil {
			return "", fmt.Errorf("resolve default session path: %w", err)
		}
	}
	if path == "" || !utf8.ValidString(path) {
		return "", errors.New("session path must be non-empty valid UTF-8")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(runtime.workingDir, path)
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve session path: %w", err)
	}
	return filepath.Clean(resolved), nil
}

func openOrCreateSessionManager(
	ctx context.Context,
	path string,
	runtime runtimeDependencies,
) (*session.SessionManager, error) {
	if cause := context.Cause(ctx); cause != nil {
		return nil, fmt.Errorf("open session %s: %w", path, cause)
	}
	info, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("open session %s: path is not a regular file", path)
		}
		manager, err := session.OpenSessionManagerWithOptions(path, filepath.Dir(path), "", session.ManagerOptions{Now: runtime.sessionNow, NewEntryID: runtime.newSessionEntryID})
		if err != nil {
			return nil, fmt.Errorf("open session %s: %w", path, err)
		}
		return manager, nil
	case errors.Is(statErr, os.ErrNotExist):
		parent := filepath.Dir(path)
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return nil, fmt.Errorf("prepare session directory %s: %w", parent, err)
		}
		if cause := context.Cause(ctx); cause != nil {
			return nil, fmt.Errorf("create session %s: %w", path, cause)
		}
		createClock := runtime.sessionNow
		if !runtime.sessionCreateTime.IsZero() {
			createClock = clockBeginningWith(runtime.sessionCreateTime, runtime.sessionNow)
		}
		manager, err := session.OpenSessionManagerWithOptions(path, parent, runtime.workingDir, session.ManagerOptions{
			NewSession: session.NewSessionOptions{ID: runtime.sessionID},
			Now:        createClock,
			NewEntryID: runtime.newSessionEntryID,
		})
		if err != nil {
			return nil, fmt.Errorf("create session %s: %w", path, err)
		}
		return manager, nil
	default:
		return nil, fmt.Errorf("inspect session %s: %w", path, statErr)
	}
}

func renderTerminal(terminal llm.AssistantTerminal, stdout, stderr io.Writer) int {
	switch terminal := terminal.(type) {
	case llm.AssistantTextMessage, llm.AssistantRichMessage:
		for _, block := range terminal.Blocks() {
			text, ok := block.(llm.TextBlock)
			if !ok {
				continue
			}
			if _, err := fmt.Fprintln(stdout, text.Text()); err != nil {
				writeDiagnostic(stderr, fmt.Errorf("write stdout: %w", err))
				return ExitFailure
			}
		}
		return ExitSuccess
	case llm.AssistantFailureMessage:
		message := terminal.ErrorMessage()
		if strings.TrimSpace(message) == "" {
			message = "request " + terminal.FinishReason().String()
		}
		writeTextLine(stderr, message)
		return ExitFailure
	default:
		writeDiagnostic(stderr, fmt.Errorf("unsupported final assistant result %T", terminal))
		return ExitFailure
	}
}

func (c *signalController) exitCode() (int, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.caught == nil {
		return 0, false
	}
	return c.caught.exitCode, true
}

func writeDiagnostic(writer io.Writer, err error) {
	writeTextLine(writer, "pi-go: "+safeErrorText(err))
}

func writeTextLine(writer io.Writer, text string) {
	text = strings.ToValidUTF8(text, "�")
	_, _ = fmt.Fprintln(writer, text)
}

// reportRuntimeDiagnostics mirrors main.ts reportDiagnostics. Error
// diagnostics are blocking; informational and warning diagnostics are emitted
// without changing prompt admission.
func reportRuntimeDiagnostics(writer io.Writer, diagnostics []agentruntime.Diagnostic) (hasError bool) {
	for _, diagnostic := range diagnostics {
		switch diagnostic.Kind {
		case agentruntime.DiagnosticWarning:
			writeTextLine(writer, "Warning: "+diagnostic.Message)
		case agentruntime.DiagnosticError:
			writeTextLine(writer, "Error: "+diagnostic.Message)
			hasError = true
		default:
			writeTextLine(writer, diagnostic.Message)
		}
	}
	return hasError
}

func safeErrorText(err error) (text string) {
	if err == nil {
		return "unknown error"
	}
	defer func() {
		if recover() != nil {
			text = fmt.Sprintf("error value of type %T", err)
		}
	}()
	text = strings.ToValidUTF8(err.Error(), "�")
	if strings.TrimSpace(text) == "" {
		return fmt.Sprintf("error value of type %T", err)
	}
	return text
}

type flusher interface {
	Flush() error
}

func flushOne(writer io.Writer) error {
	if value, ok := writer.(flusher); ok {
		return value.Flush()
	}
	return nil
}

// Package agentruntime owns the transport-neutral AgentSession replacement
// lifecycle. It mirrors coding-agent's agent-session-runtime.ts boundary: the
// host factory assembles cwd-bound services and the runtime coordinates session
// managers, lifecycle hooks, replacement ordering, and resource ownership.
package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/auth"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/model"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/resource"
	"github.com/cat3399/pi-go/internal/session"
)

type DiagnosticKind string

const (
	DiagnosticInfo    DiagnosticKind = "info"
	DiagnosticWarning DiagnosticKind = "warning"
	DiagnosticError   DiagnosticKind = "error"
)

type Diagnostic struct {
	Kind    DiagnosticKind
	Message string
}

// Services is the cwd-bound service set replaced together with AgentSession.
// The concrete fields correspond to the Go product services that currently
// exist; hosts may leave a service nil when their assembly does not use it.
type Services struct {
	CWD             string
	AgentDir        string
	ModelRuntime    *model.Runtime
	ResourceService *resource.Service
	AuthRuntime     *auth.Runtime
	Provider        provider.Provider
	Tool            agent.ToolExecutor
	Tools           []provider.ToolDefinition
}

type CreateOptions struct {
	CWD               string
	AgentDir          string
	SessionManager    *session.SessionManager
	SessionStartEvent *agent.SessionStartHookEvent
}

type CreateResult struct {
	Session              *agent.AgentSession
	Services             *Services
	Diagnostics          []Diagnostic
	ModelFallbackMessage *string
}

type Factory func(context.Context, CreateOptions) (CreateResult, error)

type InitialOptions struct {
	CWD               string
	AgentDir          string
	SessionManager    *session.SessionManager
	SessionStartEvent *agent.SessionStartHookEvent
}

type ReplacementResult struct {
	Cancelled bool
}

type WithSession func(context.Context, *agent.AgentSession) error

type SwitchOptions struct {
	CWDOverride string
	WithSession WithSession
}

type NewOptions struct {
	ParentSession string
	Setup         func(context.Context, *session.SessionManager) error
	WithSession   WithSession
}

type ForkOptions struct {
	Position    agent.ForkPosition
	WithSession WithSession
}

type ForkResult struct {
	Cancelled    bool
	SelectedText *string
}

type SessionCwdIssue struct {
	SessionFile string
	SessionCWD  string
	FallbackCWD string
}

type MissingSessionCwdError struct{ Issue SessionCwdIssue }

func (e *MissingSessionCwdError) Error() string {
	sessionFile := ""
	if e.Issue.SessionFile != "" {
		sessionFile = "\nSession file: " + e.Issue.SessionFile
	}
	return fmt.Sprintf("Stored session working directory does not exist: %s%s\nCurrent working directory: %s", e.Issue.SessionCWD, sessionFile, e.Issue.FallbackCWD)
}

type SessionImportFileNotFoundError struct{ FilePath string }

func (e *SessionImportFileNotFoundError) Error() string { return "File not found: " + e.FilePath }

type Runtime struct {
	opMu sync.Mutex
	mu   sync.RWMutex

	session              *agent.AgentSession
	services             *Services
	factory              Factory
	diagnostics          []Diagnostic
	modelFallbackMessage *string
	rebindSession        func(context.Context, *agent.AgentSession) error
	beforeInvalidate     func()
	disposed             bool
}

func Create(ctx context.Context, factory Factory, options InitialOptions) (*Runtime, error) {
	if factory == nil {
		return nil, errors.New("agent runtime factory is required")
	}
	if options.SessionManager == nil {
		return nil, errors.New("session manager is required")
	}
	if err := AssertSessionCwdExists(options.SessionManager, options.CWD); err != nil {
		return nil, err
	}
	result, err := factory(normalizeContext(ctx), CreateOptions{
		CWD: options.CWD, AgentDir: options.AgentDir, SessionManager: options.SessionManager,
		SessionStartEvent: cloneStartEvent(options.SessionStartEvent),
	})
	if err != nil {
		cleanupInitialCreateResult(result)
		return nil, err
	}
	if err := validateCreateResult(result, options.SessionManager); err != nil {
		cleanupInitialCreateResult(result)
		return nil, err
	}
	return &Runtime{
		session: result.Session, services: result.Services, factory: factory,
		diagnostics: cloneDiagnostics(result.Diagnostics), modelFallbackMessage: cloneString(result.ModelFallbackMessage),
	}, nil
}

func (r *Runtime) Session() *agent.AgentSession {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.session
}

func (r *Runtime) Services() *Services {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.services
}

func (r *Runtime) CWD() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.services == nil {
		return ""
	}
	return r.services.CWD
}

func (r *Runtime) Diagnostics() []Diagnostic {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneDiagnostics(r.diagnostics)
}

func (r *Runtime) ModelFallbackMessage() *string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneString(r.modelFallbackMessage)
}

func (r *Runtime) SetRebindSession(callback func(context.Context, *agent.AgentSession) error) {
	r.mu.Lock()
	r.rebindSession = callback
	r.mu.Unlock()
}

func (r *Runtime) SetBeforeSessionInvalidate(callback func()) {
	r.mu.Lock()
	r.beforeInvalidate = callback
	r.mu.Unlock()
}

// Reload refreshes the current AgentSession in place. The runtime operation
// gate serializes it with new/resume/fork/import/dispose, while rebind runs at
// AgentSession's original before-session-start boundary.
func (r *Runtime) Reload(ctx context.Context) error {
	if r == nil {
		return errors.New("agent runtime is disposed")
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()
	ctx = normalizeContext(ctx)
	current, _, err := r.current()
	if err != nil {
		return err
	}
	r.mu.RLock()
	rebind := r.rebindSession
	r.mu.RUnlock()
	return current.Reload(ctx, agent.ReloadOptions{BeforeSessionStart: func(reloadCtx context.Context) error {
		if rebind == nil {
			return nil
		}
		return rebind(reloadCtx, current)
	}})
}

func (r *Runtime) SwitchSession(ctx context.Context, sessionPath string, options SwitchOptions) (ReplacementResult, error) {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	ctx = normalizeContext(ctx)
	current, services, err := r.current()
	if err != nil {
		return ReplacementResult{}, err
	}
	before, err := current.PrepareSessionSwitch(ctx, agent.SessionBeforeSwitchEvent{Reason: agent.SessionSwitchResume, TargetSessionFile: stringPointer(sessionPath)})
	if err != nil {
		return ReplacementResult{}, err
	}
	if before.Cancel.Cancelled() {
		return ReplacementResult{Cancelled: true}, nil
	}
	previous := managerFile(current.SessionManager())
	nextManager, err := session.OpenSessionManager(sessionPath, "", options.CWDOverride)
	if err != nil {
		return ReplacementResult{}, err
	}
	owned := true
	defer func() {
		if owned {
			_ = nextManager.Close()
		}
	}()
	if err := AssertSessionCwdExists(nextManager, services.CWD); err != nil {
		return ReplacementResult{}, err
	}
	if err := r.teardownCurrent(ctx, agent.ShutdownResume, managerFile(nextManager)); err != nil {
		return ReplacementResult{}, err
	}
	result, err := r.factory(ctx, CreateOptions{
		CWD: nextManager.Cwd(), AgentDir: services.AgentDir, SessionManager: nextManager,
		SessionStartEvent: startEvent(agent.SessionResume, previous),
	})
	if err != nil {
		cleanupInvalidCreateResult(result, nextManager)
		return ReplacementResult{}, err
	}
	if err := validateCreateResult(result, nextManager); err != nil {
		cleanupInvalidCreateResult(result, nextManager)
		return ReplacementResult{}, err
	}
	owned = false
	r.apply(result)
	if err := r.finishReplacement(ctx, options.WithSession); err != nil {
		return ReplacementResult{}, err
	}
	return ReplacementResult{}, nil
}

func (r *Runtime) NewSession(ctx context.Context, options NewOptions) (ReplacementResult, error) {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	ctx = normalizeContext(ctx)
	current, services, err := r.current()
	if err != nil {
		return ReplacementResult{}, err
	}
	before, err := current.PrepareSessionSwitch(ctx, agent.SessionBeforeSwitchEvent{Reason: agent.SessionSwitchNew})
	if err != nil {
		return ReplacementResult{}, err
	}
	if before.Cancel.Cancelled() {
		return ReplacementResult{Cancelled: true}, nil
	}
	previous := managerFile(current.SessionManager())
	manager := current.SessionManager()
	var nextManager *session.SessionManager
	if manager.IsPersisted() {
		nextManager, err = session.CreateSessionManager(services.CWD, manager.SessionDir(), session.NewSessionOptions{})
	} else {
		nextManager, err = session.InMemorySessionManager(services.CWD, session.NewSessionOptions{})
	}
	if err != nil {
		return ReplacementResult{}, err
	}
	owned := true
	defer func() {
		if owned {
			_ = nextManager.Close()
		}
	}()
	if options.ParentSession != "" {
		if _, _, err := nextManager.NewSession(session.NewSessionOptions{ParentSession: options.ParentSession}); err != nil {
			return ReplacementResult{}, err
		}
	}
	if err := r.teardownCurrent(ctx, agent.ShutdownNew, managerFile(nextManager)); err != nil {
		return ReplacementResult{}, err
	}
	result, err := r.factory(ctx, CreateOptions{
		CWD: services.CWD, AgentDir: services.AgentDir, SessionManager: nextManager,
		SessionStartEvent: startEvent(agent.SessionNew, previous),
	})
	if err != nil {
		cleanupInvalidCreateResult(result, nextManager)
		return ReplacementResult{}, err
	}
	if err := validateCreateResult(result, nextManager); err != nil {
		cleanupInvalidCreateResult(result, nextManager)
		return ReplacementResult{}, err
	}
	owned = false
	r.apply(result)
	if options.Setup != nil {
		if err := options.Setup(ctx, result.Session.SessionManager()); err != nil {
			return ReplacementResult{}, err
		}
		if err := result.Session.ReloadMessagesFromSession(); err != nil {
			return ReplacementResult{}, err
		}
	}
	if err := r.finishReplacement(ctx, options.WithSession); err != nil {
		return ReplacementResult{}, err
	}
	return ReplacementResult{}, nil
}

func (r *Runtime) Fork(ctx context.Context, entryID string, options ForkOptions) (ForkResult, error) {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	ctx = normalizeContext(ctx)
	current, services, err := r.current()
	if err != nil {
		return ForkResult{}, err
	}
	position := options.Position
	if position == "" {
		position = agent.ForkBefore
	}
	if position != agent.ForkBefore && position != agent.ForkAt {
		return ForkResult{}, fmt.Errorf("invalid fork position %q", position)
	}
	before, err := current.PrepareSessionFork(ctx, agent.SessionBeforeForkEvent{EntryID: entryID, Position: position})
	if err != nil {
		return ForkResult{}, err
	}
	if before.Cancel.Cancelled() {
		return ForkResult{Cancelled: true}, nil
	}
	manager := current.SessionManager()
	selected, ok := manager.Entry(entryID)
	if !ok {
		return ForkResult{}, errors.New("Invalid entry ID for forking")
	}
	targetLeafID := ""
	var selectedText *string
	if position == agent.ForkAt {
		targetLeafID = selected.ID()
	} else {
		message, ok := selected.Message()
		if !ok || message.Role() != llm.RoleUser {
			return ForkResult{}, errors.New("Invalid entry ID for forking")
		}
		text := extractUserMessageText(message)
		selectedText = &text
		if parent, ok := selected.ParentID(); ok {
			targetLeafID = parent
		}
	}
	previous := managerFile(manager)
	var nextManager *session.SessionManager
	if targetLeafID == "" {
		parent := ""
		if previous != nil {
			parent = *previous
		}
		if manager.IsPersisted() {
			nextManager, err = session.CreateSessionManager(services.CWD, manager.SessionDir(), session.NewSessionOptions{})
		} else {
			nextManager, err = session.InMemorySessionManager(services.CWD, session.NewSessionOptions{})
		}
		if err == nil {
			_, _, err = nextManager.NewSession(session.NewSessionOptions{ParentSession: parent})
			if err != nil {
				_ = nextManager.Close()
			}
		}
	} else {
		if manager.IsPersisted() {
			currentFile, ok := manager.SessionFile()
			if !ok {
				return ForkResult{}, errors.New("Persisted session is missing a session file")
			}
			if _, statErr := os.Stat(currentFile); statErr != nil {
				if errors.Is(statErr, os.ErrNotExist) {
					return ForkResult{}, errors.New("This session has not been saved yet. Wait for the first assistant response before cloning or forking it.")
				}
				return ForkResult{}, statErr
			}
		}
		nextManager, err = manager.CloneBranchedSession(ctx, targetLeafID)
	}
	if err != nil {
		return ForkResult{}, err
	}
	owned := true
	defer func() {
		if owned {
			_ = nextManager.Close()
		}
	}()
	if err := r.teardownCurrent(ctx, agent.ShutdownFork, managerFile(nextManager)); err != nil {
		return ForkResult{}, err
	}
	result, err := r.factory(ctx, CreateOptions{
		CWD: nextManager.Cwd(), AgentDir: services.AgentDir, SessionManager: nextManager,
		SessionStartEvent: startEvent(agent.SessionFork, previous),
	})
	if err != nil {
		cleanupInvalidCreateResult(result, nextManager)
		return ForkResult{}, err
	}
	if err := validateCreateResult(result, nextManager); err != nil {
		cleanupInvalidCreateResult(result, nextManager)
		return ForkResult{}, err
	}
	owned = false
	r.apply(result)
	if err := r.finishReplacement(ctx, options.WithSession); err != nil {
		return ForkResult{}, err
	}
	return ForkResult{SelectedText: selectedText}, nil
}

func (r *Runtime) ImportFromJSONL(ctx context.Context, inputPath, cwdOverride string) (ReplacementResult, error) {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	ctx = normalizeContext(ctx)
	current, services, err := r.current()
	if err != nil {
		return ReplacementResult{}, err
	}
	resolvedPath, err := resolveInputPath(inputPath)
	if err != nil {
		return ReplacementResult{}, err
	}
	info, err := os.Stat(resolvedPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ReplacementResult{}, &SessionImportFileNotFoundError{FilePath: resolvedPath}
		}
		return ReplacementResult{}, err
	}
	if !info.Mode().IsRegular() {
		return ReplacementResult{}, fmt.Errorf("import path is not a regular file: %s", resolvedPath)
	}
	sessionDir := current.SessionManager().SessionDir()
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return ReplacementResult{}, err
	}
	destinationPath := filepath.Join(sessionDir, filepath.Base(resolvedPath))
	before, err := current.PrepareSessionSwitch(ctx, agent.SessionBeforeSwitchEvent{Reason: agent.SessionSwitchResume, TargetSessionFile: stringPointer(destinationPath)})
	if err != nil {
		return ReplacementResult{}, err
	}
	if before.Cancel.Cancelled() {
		return ReplacementResult{Cancelled: true}, nil
	}
	previous := managerFile(current.SessionManager())
	if filepath.Clean(destinationPath) != filepath.Clean(resolvedPath) {
		if err := copyFile(resolvedPath, destinationPath); err != nil {
			return ReplacementResult{}, err
		}
	}
	nextManager, err := session.OpenSessionManager(destinationPath, sessionDir, cwdOverride)
	if err != nil {
		return ReplacementResult{}, err
	}
	owned := true
	defer func() {
		if owned {
			_ = nextManager.Close()
		}
	}()
	if err := AssertSessionCwdExists(nextManager, services.CWD); err != nil {
		return ReplacementResult{}, err
	}
	if err := r.teardownCurrent(ctx, agent.ShutdownResume, managerFile(nextManager)); err != nil {
		return ReplacementResult{}, err
	}
	result, err := r.factory(ctx, CreateOptions{
		CWD: nextManager.Cwd(), AgentDir: services.AgentDir, SessionManager: nextManager,
		SessionStartEvent: startEvent(agent.SessionResume, previous),
	})
	if err != nil {
		cleanupInvalidCreateResult(result, nextManager)
		return ReplacementResult{}, err
	}
	if err := validateCreateResult(result, nextManager); err != nil {
		cleanupInvalidCreateResult(result, nextManager)
		return ReplacementResult{}, err
	}
	owned = false
	r.apply(result)
	if err := r.finishReplacement(ctx, nil); err != nil {
		return ReplacementResult{}, err
	}
	return ReplacementResult{}, nil
}

func (r *Runtime) Dispose(ctx context.Context) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	current, _, err := r.current()
	if err != nil {
		if r.isDisposed() {
			return nil
		}
		return err
	}
	callback := r.beforeInvalidateCallback()
	if err := current.Shutdown(normalizeContext(ctx), agent.SessionShutdownOptions{
		Event: agent.SessionShutdownHookEvent{Reason: agent.ShutdownQuit}, BeforeInvalidate: callback,
	}); err != nil {
		return err
	}
	r.mu.Lock()
	r.disposed = true
	r.mu.Unlock()
	return nil
}

func (r *Runtime) teardownCurrent(ctx context.Context, reason agent.SessionShutdownReason, target *string) error {
	current := r.Session()
	return current.Shutdown(ctx, agent.SessionShutdownOptions{
		Event:            agent.SessionShutdownHookEvent{Reason: reason, TargetSessionFile: cloneString(target)},
		BeforeInvalidate: r.beforeInvalidateCallback(),
	})
}

func (r *Runtime) apply(result CreateResult) {
	r.mu.Lock()
	r.session = result.Session
	r.services = result.Services
	r.diagnostics = cloneDiagnostics(result.Diagnostics)
	r.modelFallbackMessage = cloneString(result.ModelFallbackMessage)
	r.mu.Unlock()
}

func (r *Runtime) finishReplacement(ctx context.Context, withSession WithSession) error {
	r.mu.RLock()
	current, rebind := r.session, r.rebindSession
	r.mu.RUnlock()
	if rebind != nil {
		if err := rebind(ctx, current); err != nil {
			return err
		}
	}
	if withSession != nil {
		return withSession(ctx, current)
	}
	return nil
}

func (r *Runtime) current() (*agent.AgentSession, *Services, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.disposed || r.session == nil || r.services == nil {
		return nil, nil, errors.New("agent runtime is disposed")
	}
	return r.session, r.services, nil
}

func (r *Runtime) isDisposed() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.disposed
}

func (r *Runtime) beforeInvalidateCallback() func() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.beforeInvalidate
}

func AssertSessionCwdExists(manager *session.SessionManager, fallbackCWD string) error {
	if manager == nil {
		return errors.New("session manager is required")
	}
	sessionFile, ok := manager.SessionFile()
	if !ok {
		return nil
	}
	sessionCWD := manager.Cwd()
	if sessionCWD == "" {
		return nil
	}
	if _, err := os.Stat(sessionCWD); err == nil {
		return nil
	}
	return &MissingSessionCwdError{Issue: SessionCwdIssue{
		SessionFile: sessionFile, SessionCWD: sessionCWD, FallbackCWD: fallbackCWD,
	}}
}

func validateCreateResult(result CreateResult, manager *session.SessionManager) error {
	if result.Session == nil {
		return errors.New("agent runtime factory returned a nil session")
	}
	if result.Services == nil {
		return errors.New("agent runtime factory returned nil services")
	}
	if result.Session.SessionManager() != manager {
		return errors.New("agent runtime factory returned a session for a different session manager")
	}
	return nil
}

func cleanupInvalidCreateResult(result CreateResult, manager *session.SessionManager) {
	if result.Session != nil {
		_ = result.Session.Close(context.Background())
	}
	_ = manager.Close()
}

func cleanupInitialCreateResult(result CreateResult) {
	if result.Session != nil {
		_ = result.Session.Close(context.Background())
		if manager := result.Session.SessionManager(); manager != nil {
			_ = manager.Close()
		}
	}
}

func extractUserMessageText(message llm.ConversationMessage) string {
	var builder strings.Builder
	switch value := message.(type) {
	case llm.UserTextMessage:
		for _, block := range value.Content() {
			builder.WriteString(block.Text())
		}
	case llm.UserContentMessage:
		for _, block := range value.Content() {
			if text, ok := block.(llm.TextBlock); ok {
				builder.WriteString(text.Text())
			}
		}
	}
	return builder.String()
}

func resolveInputPath(input string) (string, error) {
	if !utf8.ValidString(input) || input == "" {
		return "", errors.New("invalid import path")
	}
	if input == "~" || strings.HasPrefix(input, "~"+string(filepath.Separator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if input == "~" {
			input = home
		} else {
			input = filepath.Join(home, input[2:])
		}
	}
	resolved, err := filepath.Abs(input)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func copyFile(source, destination string) (resultErr error) {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, input.Close()) }()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, output.Close()) }()
	_, err = io.Copy(output, input)
	return err
}

func managerFile(manager *session.SessionManager) *string {
	if manager == nil {
		return nil
	}
	value, ok := manager.SessionFile()
	if !ok {
		return nil
	}
	return &value
}

func startEvent(reason agent.SessionStartReason, previous *string) *agent.SessionStartHookEvent {
	return &agent.SessionStartHookEvent{Reason: reason, PreviousSessionFile: cloneString(previous)}
}

func cloneStartEvent(event *agent.SessionStartHookEvent) *agent.SessionStartHookEvent {
	if event == nil {
		return nil
	}
	copy := *event
	copy.PreviousSessionFile = cloneString(event.PreviousSessionFile)
	return &copy
}

func stringPointer(value string) *string { return &value }

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneDiagnostics(values []Diagnostic) []Diagnostic {
	return append([]Diagnostic(nil), values...)
}

func normalizeContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

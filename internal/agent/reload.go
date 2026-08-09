package agent

import (
	"context"
	"fmt"

	"github.com/cat3399/pi-go/internal/provider"
)

// ReloadOptions carries the host rebind boundary that original AgentSession
// invokes after rebuilding its runtime and before session_start(reload).
type ReloadOptions struct {
	BeforeSessionStart func(context.Context) error
}

func callSessionShutdownHook(hook SessionShutdownHook, ctx context.Context, event SessionShutdownHookEvent) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("session_shutdown hook panicked: %s", safeValueText(recovered))
		}
	}()
	return hook(ctx, event)
}

func callSessionStartHook(hook SessionStartHook, ctx context.Context, event SessionStartHookEvent) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("session_start hook panicked: %s", safeValueText(recovered))
		}
	}()
	return hook(ctx, event)
}

func (s *AgentSession) syncReloadedRuntimeSettings(settings RuntimeControlSettings) error {
	if !validQueueMode(settings.SteeringMode) || !validQueueMode(settings.FollowUpMode) {
		return fmt.Errorf("%w: reload resolved an invalid queue mode", ErrInvalidConfig)
	}
	if _, err := provider.NewRetryController(settings.Retry); err != nil {
		return fmt.Errorf("%w: reload retry policy: %w", ErrInvalidConfig, err)
	}
	// The two settings are one SettingsManager snapshot. Publish them under one
	// Agent lock so an active run cannot drain one queue under mixed generations.
	if err := s.loop.setQueueModes(settings.SteeringMode, settings.FollowUpMode); err != nil {
		return err
	}
	s.mu.Lock()
	s.compactionEnabled = settings.AutoCompactionEnabled
	s.retryEnabled = settings.AutoRetryEnabled
	s.retryPolicy = settings.Retry
	if settings.CompactionReserveSet {
		s.contextReserve = settings.CompactionReserveTokens
	}
	if settings.CompactionKeepRecentSet {
		s.keepRecentTokens = settings.CompactionKeepRecentTokens
		s.keepRecentSet = true
	}
	if settings.BranchSummaryReserveSet {
		s.branchSummaryReserve = settings.BranchSummaryReserveTokens
	}
	s.mu.Unlock()
	return nil
}

func (s *AgentSession) beginReload() (func(), error) {
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return nil, fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	if s.reloading {
		s.lifecycleMu.Unlock()
		return nil, fmt.Errorf("%w: session reload is already active", ErrBusy)
	}
	done := make(chan struct{})
	s.reloading = true
	s.reloadDone = done
	s.lifecycleMu.Unlock()
	return func() {
		s.lifecycleMu.Lock()
		if s.reloadDone == done {
			s.reloading = false
			s.reloadDone = nil
			close(done)
		}
		s.lifecycleMu.Unlock()
	}, nil
}

// Reload follows coding-agent's product ordering without replacing Agent or
// conversation state:
//
//  1. session_shutdown(reload)
//  2. external settings/catalog reload
//  3. queue/runtime settings synchronization
//  4. resource reload
//  5. system prompt + current active-tool publication
//  6. host rebind and session_start(reload)
//
// Hook failures are isolated through ExtensionError like ExtensionRunner.emit;
// service and rebind failures remain real reload errors. Each underlying
// service retains its own last-healthy transactional snapshot.
func (s *AgentSession) Reload(ctx context.Context, options ReloadOptions) error {
	if s == nil || s.loop == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.rejectIfClosed(); err != nil {
		return err
	}
	finish, err := s.beginReload()
	if err != nil {
		return err
	}
	defer finish()
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if hook := s.hooks.SessionShutdown; hook != nil {
		if err := callSessionShutdownHook(hook, ctx, SessionShutdownHookEvent{Reason: ShutdownReload}); err != nil {
			s.reportExtensionError(ctx, "session_shutdown", 0, err)
		}
	}

	s.mu.RLock()
	reloadRuntime := s.reloadRuntime
	resources := s.resources
	s.mu.RUnlock()
	if reloadRuntime != nil {
		if err := reloadRuntime(ctx); err != nil {
			return fmt.Errorf("reload runtime settings: %w", err)
		}
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	s.controlMu.Lock()
	err = s.syncReloadedRuntimeSettings(s.resolvedRuntimeSettings())
	s.controlMu.Unlock()
	if err != nil {
		return err
	}
	if resources != nil {
		if err := resources.Reload(ctx); err != nil {
			return fmt.Errorf("reload session resources: %w", err)
		}
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	// Public active-tool changes share controlMu. Sampling and publication in
	// one short critical section preserves the latest concurrent user choice.
	s.controlMu.Lock()
	err = s.setActiveToolsByName(s.ActiveToolNames())
	s.controlMu.Unlock()
	if err != nil {
		return fmt.Errorf("reload active tools and system prompt: %w", err)
	}
	if options.BeforeSessionStart != nil {
		if err := options.BeforeSessionStart(ctx); err != nil {
			return err
		}
	}
	if hook := s.hooks.SessionStart; hook != nil {
		if err := callSessionStartHook(hook, ctx, SessionStartHookEvent{Reason: SessionReload}); err != nil {
			s.reportExtensionError(ctx, "session_start", 0, err)
		}
	}
	return nil
}

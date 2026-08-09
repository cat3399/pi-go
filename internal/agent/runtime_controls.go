package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/cat3399/pi-go/internal/provider"
)

func validQueueMode(mode QueueMode) bool {
	return mode == QueueAll || mode == QueueOneAtATime
}

func (s *AgentSession) resolvedRuntimeSettings() RuntimeControlSettings {
	settings := RuntimeControlSettings{
		SteeringMode: QueueOneAtATime, FollowUpMode: QueueOneAtATime,
		AutoCompactionEnabled: true, AutoRetryEnabled: true,
	}
	if s == nil {
		return settings
	}
	if s.loop != nil {
		settings.SteeringMode = s.loop.SteeringMode()
		settings.FollowUpMode = s.loop.FollowUpMode()
	}
	s.mu.RLock()
	settings.AutoCompactionEnabled = s.compactionEnabled
	settings.AutoRetryEnabled = s.retryEnabled
	settings.Retry = s.retryPolicy
	settings.CompactionReserveTokens = s.contextReserve
	settings.CompactionReserveSet = true
	settings.CompactionKeepRecentTokens = s.keepRecentTokens
	settings.CompactionKeepRecentSet = s.keepRecentSet
	settings.BranchSummaryReserveTokens = s.branchSummaryReserve
	settings.BranchSummaryReserveSet = true
	resolve := s.resolveRuntimeSettings
	s.mu.RUnlock()
	if resolve != nil {
		settings = resolve()
	}
	if !validQueueMode(settings.SteeringMode) {
		settings.SteeringMode = QueueOneAtATime
	}
	if !validQueueMode(settings.FollowUpMode) {
		settings.FollowUpMode = QueueOneAtATime
	}
	return settings
}

// SetSteeringMode persists the global preference before publishing the
// requested value to Agent. A definite write failure leaves runtime state
// unchanged; commit-unknown publishes the forward snapshot and
// returns the uncertainty to the caller.
func (s *AgentSession) SetSteeringMode(mode QueueMode) error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if !validQueueMode(mode) {
		return fmt.Errorf("%w: invalid steering mode", ErrInvalidConfig)
	}
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	write, writeErr := s.writeSettings(SettingsUpdate{SteeringMode: &mode})
	if writeErr != nil && !write.CommitUnknown {
		return writeErr
	}
	if err := s.loop.SetSteeringMode(mode); err != nil {
		return errors.Join(writeErr, err)
	}
	return writeErr
}

// SetFollowUpMode follows the same durability policy as SetSteeringMode.
func (s *AgentSession) SetFollowUpMode(mode QueueMode) error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if !validQueueMode(mode) {
		return fmt.Errorf("%w: invalid follow-up mode", ErrInvalidConfig)
	}
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	write, writeErr := s.writeSettings(SettingsUpdate{FollowUpMode: &mode})
	if writeErr != nil && !write.CommitUnknown {
		return writeErr
	}
	if err := s.loop.SetFollowUpMode(mode); err != nil {
		return errors.Join(writeErr, err)
	}
	return writeErr
}

func (s *AgentSession) AutoCompactionEnabled() bool {
	if s == nil {
		return true
	}
	if s.resolveRuntimeSettings != nil {
		return s.resolvedRuntimeSettings().AutoCompactionEnabled
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.compactionEnabled
}

func (s *AgentSession) SetAutoCompactionEnabled(enabled bool) error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	write, writeErr := s.writeSettings(SettingsUpdate{AutoCompactionEnabled: &enabled})
	if writeErr != nil && !write.CommitUnknown {
		return writeErr
	}
	effective := enabled
	if s.resolveRuntimeSettings != nil {
		effective = s.resolvedRuntimeSettings().AutoCompactionEnabled
	}
	s.mu.Lock()
	s.compactionEnabled = effective
	s.mu.Unlock()
	return writeErr
}

func (s *AgentSession) AutoRetryEnabled() bool {
	if s == nil {
		return true
	}
	if s.resolveRuntimeSettings != nil {
		return s.resolvedRuntimeSettings().AutoRetryEnabled
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.retryEnabled
}

func (s *AgentSession) SetAutoRetryEnabled(enabled bool) error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	write, writeErr := s.writeSettings(SettingsUpdate{AutoRetryEnabled: &enabled})
	if writeErr != nil && !write.CommitUnknown {
		return writeErr
	}
	effective := enabled
	if s.resolveRuntimeSettings != nil {
		effective = s.resolvedRuntimeSettings().AutoRetryEnabled
	}
	s.mu.Lock()
	s.retryEnabled = effective
	s.mu.Unlock()
	return writeErr
}

func (s *AgentSession) currentRetrySettings() (bool, provider.RetryController) {
	settings := s.resolvedRuntimeSettings()
	controller, err := provider.NewRetryController(settings.Retry)
	if err != nil {
		return false, provider.RetryController{}
	}
	return settings.AutoRetryEnabled, controller
}

// AbortRetry cancels only the active retry delay. The owning session run
// remains alive and settles normally after emitting the retry cancellation.
func (s *AgentSession) AbortRetry() {
	if s == nil {
		return
	}
	s.lifecycleMu.Lock()
	var cancel context.CancelCauseFunc
	if s.run != nil {
		cancel = s.run.retryCancel
	}
	s.lifecycleMu.Unlock()
	if cancel != nil {
		cancel(errRetryCancelled)
	}
}

func (s *AgentSession) IsRetrying() bool {
	if s == nil {
		return false
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.run != nil && s.run.retryCancel != nil
}

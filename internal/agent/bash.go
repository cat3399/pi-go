package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/agentmsg"
)

// BashResult is coding-agent's user-initiated shell result. ExitCode is nil
// when the process was cancelled or its platform status is unavailable.
type BashResult struct {
	Output         string
	ExitCode       *int
	Cancelled      bool
	Truncated      bool
	FullOutputPath string
}

func cloneBashResult(result BashResult) BashResult {
	if result.ExitCode != nil {
		code := *result.ExitCode
		result.ExitCode = &code
	}
	return result
}

// StandaloneBashExecutor is distinct from ToolExecutor because non-zero exit
// codes are ordinary results and output deltas are part of AgentSession's
// product event stream rather than tool_execution_update.
type StandaloneBashExecutor interface {
	ExecuteBash(context.Context, string, func(string)) (BashResult, error)
}

type ExecuteBashOptions struct {
	ExcludeFromContext bool
	ID                 *string
	// Executor is the Go equivalent of user_bash's custom BashOperations. Nil
	// uses the session's configured local executor.
	Executor StandaloneBashExecutor
}

type RecordBashOptions struct{ ExcludeFromContext bool }

type bashExecutionRegistration struct {
	id     uint64
	ctx    context.Context
	cancel context.CancelCauseFunc
}

func (s *AgentSession) registerBashExecution(ctx context.Context) (bashExecutionRegistration, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closed || s.closing {
		return bashExecutionRegistration{}, fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	s.bashMu.Lock()
	s.bashNextID++
	id := s.bashNextID
	if s.bashRuns == nil {
		s.bashRuns = make(map[uint64]context.CancelCauseFunc)
	}
	if len(s.bashRuns) == 0 {
		s.bashIdle = make(chan struct{})
	}
	s.bashRuns[id] = cancel
	s.bashMu.Unlock()
	return bashExecutionRegistration{id: id, ctx: runCtx, cancel: cancel}, nil
}

func (s *AgentSession) unregisterBashExecution(registration bashExecutionRegistration) {
	registration.cancel(context.Canceled)
	s.bashMu.Lock()
	delete(s.bashRuns, registration.id)
	if len(s.bashRuns) == 0 && s.bashIdle != nil {
		close(s.bashIdle)
		s.bashIdle = nil
	}
	s.bashMu.Unlock()
}

func (s *AgentSession) bashPrefix() (string, error) {
	s.mu.RLock()
	prefix, resolve := s.bashCommandPrefix, s.resolveBashPrefix
	s.mu.RUnlock()
	if resolve != nil {
		prefix = resolve()
	}
	if !utf8.ValidString(prefix) || strings.IndexByte(prefix, 0) >= 0 {
		return "", fmt.Errorf("%w: resolved bash command prefix is invalid", ErrInvalidConfig)
	}
	return prefix, nil
}

func executeStandaloneBashSafely(
	executor StandaloneBashExecutor,
	ctx context.Context,
	command string,
	onChunk func(string),
) (result BashResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &toolPanicError{value: "standalone bash executor panicked: " + safeValueText(recovered), stack: debug.Stack()}
		}
	}()
	return executor.ExecuteBash(ctx, command, onChunk)
}

// ExecuteBash runs one !/!! command, streams sanitized output, and records the
// settled BashExecution message. Multiple executions may overlap and AbortBash
// cancels all of them, matching coding-agent's AbortController set.
func (s *AgentSession) ExecuteBash(
	ctx context.Context,
	command string,
	onChunk func(string),
	options ExecuteBashOptions,
) (BashResult, error) {
	if s == nil || s.loop == nil {
		return BashResult{}, fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	executor := options.Executor
	if isNilInterface(executor) {
		s.mu.RLock()
		executor = s.standaloneBash
		resolve := s.resolveStandaloneBash
		s.mu.RUnlock()
		if resolve != nil {
			var err error
			executor, err = resolve(ctx)
			if err != nil {
				return BashResult{}, fmt.Errorf("resolve standalone bash: %w", err)
			}
		}
	}
	if isNilInterface(executor) {
		return BashResult{}, ErrBashUnavailable
	}
	prefix, err := s.bashPrefix()
	if err != nil {
		return BashResult{}, err
	}
	registration, err := s.registerBashExecution(ctx)
	if err != nil {
		return BashResult{}, err
	}
	defer s.unregisterBashExecution(registration)

	resolvedCommand := command
	if prefix != "" {
		resolvedCommand = prefix + "\n" + command
	}
	id := cloneStringPointer(options.ID)
	result, err := executeStandaloneBashSafely(executor, registration.ctx, resolvedCommand, func(delta string) {
		if onChunk != nil {
			onChunk(delta)
		}
		s.emitBashExecutionUpdate(registration.ctx, id, delta)
	})
	result = cloneBashResult(result)
	if context.Cause(registration.ctx) != nil {
		// BashOperations may either return normally or fail when its signal is
		// aborted. coding-agent normalizes both paths to one partial cancelled
		// result and never retains an exit code from the interrupted process.
		result.Cancelled = true
		result.ExitCode = nil
		err = nil
	}
	if err != nil {
		return result, err
	}
	if err := s.recordBashResult(ctx, command, result, RecordBashOptions{ExcludeFromContext: options.ExcludeFromContext}, true); err != nil {
		return result, err
	}
	return result, nil
}

// RecordBashResult is the extension-handled counterpart to ExecuteBash. It
// records a supplied complete result without emitting streaming updates.
func (s *AgentSession) RecordBashResult(ctx context.Context, command string, result BashResult, options RecordBashOptions) error {
	return s.recordBashResult(ctx, command, result, options, false)
}

func (s *AgentSession) recordBashResult(
	ctx context.Context,
	command string,
	result BashResult,
	options RecordBashOptions,
	allowClosing bool,
) error {
	if s == nil || s.loop == nil || s.sessionManager == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timestamp, err := s.loop.now()
	if err != nil {
		return err
	}
	message, err := agentmsg.NewBashExecution(agentmsg.BashExecution{
		Command: command, Output: result.Output, ExitCode: result.ExitCode,
		Cancelled: result.Cancelled, Truncated: result.Truncated, FullOutputPath: result.FullOutputPath,
		ExcludeFromContext: options.ExcludeFromContext, At: timestamp,
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRun, err)
	}
	return s.recordBashMessage(ctx, message, allowClosing)
}

func (s *AgentSession) recordBashMessage(ctx context.Context, message agentmsg.BashExecution, allowClosing bool) error {
	for {
		s.bashRecordMu.Lock()
		s.lifecycleMu.Lock()
		if s.closed || s.closing && !allowClosing {
			s.lifecycleMu.Unlock()
			s.bashRecordMu.Unlock()
			return fmt.Errorf("%w: session is closed", ErrInvalidRun)
		}
		if s.standaloneMutation {
			done := s.standaloneDone
			s.lifecycleMu.Unlock()
			s.bashRecordMu.Unlock()
			if done == nil {
				return fmt.Errorf("%w: standalone mutation has no completion signal", ErrInvariant)
			}
			wait, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.settlementTimeout)
			select {
			case <-done:
				cancel()
				continue
			case <-wait.Done():
				cause := context.Cause(wait)
				cancel()
				return cause
			}
		}
		if s.run != nil && s.run.agentRunActive {
			s.pendingBash = append(s.pendingBash, message)
			s.lifecycleMu.Unlock()
			s.bashRecordMu.Unlock()
			return nil
		}
		done := make(chan struct{})
		s.standaloneMutation = true
		s.standaloneDone = done
		if s.idleWait == nil {
			s.idleWait = make(chan struct{})
		}
		s.lifecycleMu.Unlock()
		reservation := &standaloneReservation{session: s, done: done}
		if err := s.flushPendingBashMessagesLocked(ctx); err != nil {
			reservation.finish(false)
			s.bashRecordMu.Unlock()
			return err
		}
		err := s.appendSettledBashMessage(ctx, message)
		reservation.finish(false)
		s.bashRecordMu.Unlock()
		return err
	}
}

func (s *AgentSession) appendSettledBashMessage(ctx context.Context, message agentmsg.BashExecution) error {
	settlement, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.settlementTimeout)
	defer cancel()
	if _, err := s.sessionManager.AppendMessage(settlement, message); err != nil {
		return fmt.Errorf("%w: bash execution: %w", ErrTranscriptCommit, err)
	}
	if err := s.loop.appendSettledMessage(message); err != nil {
		_ = s.reloadAgentMessagesFromSession()
		return fmt.Errorf("%w: publish bash execution: %w", ErrInvariant, err)
	}
	return nil
}

func (s *AgentSession) flushPendingBashMessages(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.bashRecordMu.Lock()
	defer s.bashRecordMu.Unlock()
	return s.flushPendingBashMessagesLocked(ctx)
}

func (s *AgentSession) flushPendingBashMessagesLocked(ctx context.Context) error {
	for len(s.pendingBash) != 0 {
		message := s.pendingBash[0]
		if err := s.appendSettledBashMessage(ctx, message); err != nil {
			return err
		}
		s.pendingBash = s.pendingBash[1:]
	}
	return nil
}

func (s *AgentSession) HasPendingBashMessages() bool {
	if s == nil {
		return false
	}
	s.bashRecordMu.Lock()
	defer s.bashRecordMu.Unlock()
	return len(s.pendingBash) != 0
}

func (s *AgentSession) IsBashRunning() bool {
	if s == nil {
		return false
	}
	s.bashMu.Lock()
	defer s.bashMu.Unlock()
	return len(s.bashRuns) != 0
}

// AbortBash cancels every active user shell execution without aborting the
// Agent run. Completion still records each partial cancelled result.
func (s *AgentSession) AbortBash() {
	_ = s.abortBashExecutions()
}

func (s *AgentSession) abortBashExecutions() <-chan struct{} {
	if s == nil {
		return nil
	}
	s.bashMu.Lock()
	idle := s.bashIdle
	cancels := make([]context.CancelCauseFunc, 0, len(s.bashRuns))
	for _, cancel := range s.bashRuns {
		cancels = append(cancels, cancel)
	}
	s.bashMu.Unlock()
	for _, cancel := range cancels {
		cancel(ErrAgentAborted)
	}
	return idle
}

func (s *AgentSession) emitBashExecutionUpdate(ctx context.Context, id *string, delta string) {
	s.mu.RLock()
	observers := make([]SessionObserver, 0, len(s.observers))
	for _, entry := range s.observers {
		if entry.observer != nil {
			observers = append(observers, entry.observer)
		}
	}
	s.mu.RUnlock()
	s.emitToObservers(ctx, observers, BashExecutionUpdateEvent{ID: cloneStringPointer(id), Delta: delta})
}

func (s *AgentSession) runLowAgent(run *sessionRun, operation func() (Result, error)) (result Result, err error) {
	if run == nil || operation == nil {
		return Result{}, fmt.Errorf("%w: invalid low agent run", ErrInvalidRun)
	}
	s.bashRecordMu.Lock()
	s.lifecycleMu.Lock()
	if s.run != run {
		s.lifecycleMu.Unlock()
		s.bashRecordMu.Unlock()
		return Result{}, fmt.Errorf("%w: session run is no longer active", ErrInvalidRun)
	}
	// AgentSession.isStreaming stays true for the complete high-level agent
	// prompt, including retry waits, automatic compaction, and queued
	// continuations. It becomes false only after pending BashExecution messages
	// are drained in settleSessionRun.
	run.agentRunActive = true
	run.phase = PhaseProvider
	s.lifecycleMu.Unlock()
	s.bashRecordMu.Unlock()
	return operation()
}

func joinBashSettlementError(runErr, settleErr error) error {
	if settleErr == nil {
		return runErr
	}
	if runErr == nil {
		return settleErr
	}
	return errors.Join(runErr, settleErr)
}

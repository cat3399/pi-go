package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

type queueReservation struct {
	steering  bool
	remaining int
}

// Steer accepts a user message while a run is active or idle. It is injected
// after the current tool batch and before the next provider request. A queue
// is deliberately not a second run: it cannot bypass the active-run barrier.
func (a *Agent) Steer(prompt string) error { return a.enqueue(prompt, true) }

// FollowUp accepts a message that is consumed only after the agent would
// otherwise stop. It is useful for a producer which receives input while the
// current assistant turn is still settling.
func (a *Agent) FollowUp(prompt string) error { return a.enqueue(prompt, false) }

func (a *Agent) enqueue(prompt string, steering bool) error {
	if a == nil {
		return fmt.Errorf("%w: nil agent", ErrInvalidRun)
	}
	timestamp, err := a.now()
	if err != nil {
		return err
	}
	message, err := llm.NewUserTextMessage(prompt, timestamp)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidQueueMessage, err)
	}
	return a.enqueueMessage(message, steering)
}

func (a *Agent) SteerContent(content []llm.UserContentBlock) error {
	return a.enqueueContent(content, true)
}
func (a *Agent) FollowUpContent(content []llm.UserContentBlock) error {
	return a.enqueueContent(content, false)
}
func (a *Agent) enqueueContent(content []llm.UserContentBlock, steering bool) error {
	if a == nil {
		return fmt.Errorf("%w: nil agent", ErrInvalidRun)
	}
	timestamp, err := a.now()
	if err != nil {
		return err
	}
	message, err := llm.NewUserContentMessage(content, timestamp)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidQueueMessage, err)
	}
	return a.enqueueMessage(message, steering)
}
func (a *Agent) enqueueMessage(message llm.ConversationMessage, steering bool) error {
	if message.Role() != llm.RoleUser {
		return fmt.Errorf("%w: queued message must be user content", ErrInvalidQueueMessage)
	}
	a.mu.Lock()
	if steering {
		a.steeringQueue = append(a.steeringQueue, message)
	} else {
		a.followUpQueue = append(a.followUpQueue, message)
	}
	a.mu.Unlock()
	return nil
}

// Queues returns immutable snapshots in FIFO order. It is intentionally a
// diagnostic/admission surface, not a mutable transcript view.
func (a *Agent) Queues() (steering, followUp []llm.UserTextMessage) {
	richSteering, richFollow := a.RichQueues()
	for _, message := range richSteering {
		if text, ok := message.(llm.UserTextMessage); ok {
			steering = append(steering, text)
		}
	}
	for _, message := range richFollow {
		if text, ok := message.(llm.UserTextMessage); ok {
			followUp = append(followUp, text)
		}
	}
	return
}
func (a *Agent) RichQueues() (steering, followUp []llm.ConversationMessage) {
	if a == nil {
		return nil, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.ConversationMessage(nil), a.steeringQueue...), append([]llm.ConversationMessage(nil), a.followUpQueue...)
}

func (a *Agent) ClearSteeringQueue() {
	if a == nil {
		return
	}
	a.mu.Lock()
	clearUnreservedQueue(&a.steeringQueue, a.steeringReserved)
	a.mu.Unlock()
}
func (a *Agent) ClearFollowUpQueue() {
	if a == nil {
		return
	}
	a.mu.Lock()
	clearUnreservedQueue(&a.followUpQueue, a.followUpReserved)
	a.mu.Unlock()
}
func (a *Agent) ClearAllQueues() {
	if a == nil {
		return
	}
	a.mu.Lock()
	clearUnreservedQueue(&a.steeringQueue, a.steeringReserved)
	clearUnreservedQueue(&a.followUpQueue, a.followUpReserved)
	a.mu.Unlock()
}

func clearUnreservedQueue(queue *[]llm.ConversationMessage, reserved int) {
	for index := reserved; index < len(*queue); index++ {
		(*queue)[index] = nil
	}
	*queue = (*queue)[:reserved]
	if reserved == 0 {
		*queue = nil
	}
}

// Continue starts from a durable user/tool-result tail. If the transcript ends
// in an assistant result, only queued input can make continuation legal; that
// exactly mirrors the queue drain point rather than silently fabricating a
// provider request from an assistant tail.
func (a *Agent) Continue(ctx context.Context) (Result, error) {
	if a == nil {
		return Result{}, fmt.Errorf("%w: nil agent", ErrInvalidRun)
	}
	if ctx == nil || context.Cause(ctx) != nil {
		return Result{}, fmt.Errorf("%w: invalid continuation context", ErrInvalidRun)
	}
	active, initial, err := a.admitContinuation(ctx)
	if err != nil {
		return Result{}, err
	}
	return a.runV2(active, initial)
}

func (a *Agent) admitContinuation(ctx context.Context) (*activeRun, []llm.ConversationMessage, error) {
	if a == nil || ctx == nil || context.Cause(ctx) != nil {
		return nil, nil, fmt.Errorf("%w: invalid continuation", ErrInvalidRun)
	}
	a.mu.Lock()
	if a.active != nil || a.starting {
		a.mu.Unlock()
		return nil, nil, ErrBusy
	}
	if a.nextID == ^uint64(0) {
		a.mu.Unlock()
		return nil, nil, ErrRunIDExhausted
	}
	reservedID := a.nextID
	// Reserve the same single-run slot used by Run before consulting the
	// external transcript port. Context may block or re-enter Agent methods, so
	// it must never execute under mu. The reservation prevents Run or another
	// Continue from taking the slot while this snapshot is in flight.
	a.starting = true
	a.mu.Unlock()
	reserved := true
	defer func() {
		if !reserved {
			return
		}
		a.mu.Lock()
		if a.starting && a.active == nil {
			a.starting = false
		}
		a.mu.Unlock()
	}()

	messages := a.config.transcript.Context().Messages()

	a.mu.Lock()
	active, initial, err := func() (*activeRun, []llm.ConversationMessage, error) {
		defer a.mu.Unlock()
		// Every path after reserving must return the slot if admission does not
		// install an active run. No queue is touched until validation succeeds.
		if !a.starting || a.active != nil || a.nextID != reservedID {
			return nil, nil, fmt.Errorf("%w: continuation reservation was lost", ErrInvariant)
		}
		if cause := context.Cause(ctx); cause != nil {
			return nil, nil, fmt.Errorf("%w: continuation context cancelled during admission: %w", ErrInvalidRun, cause)
		}
		if a.nextID == ^uint64(0) {
			return nil, nil, ErrRunIDExhausted
		}
		if len(messages) == 0 {
			return nil, nil, fmt.Errorf("%w: empty transcript", ErrCannotContinue)
		}
		var initial []llm.ConversationMessage
		var reservation *queueReservation
		if messages[len(messages)-1].Role() == llm.RoleAssistant {
			var err error
			initial, reservation, err = a.reserveQueueLocked(true)
			if err != nil {
				return nil, nil, err
			}
			if reservation == nil {
				initial, reservation, err = a.reserveQueueLocked(false)
				if err != nil {
					return nil, nil, err
				}
			}
			if reservation == nil {
				return nil, nil, fmt.Errorf("%w: assistant tail", ErrCannotContinue)
			}
		}
		a.nextID++
		runCtx, cancel := context.WithCancelCause(ctx)
		active := &activeRun{
			id:               a.nextID,
			ctx:              runCtx,
			cancel:           cancel,
			done:             make(chan struct{}),
			phase:            PhaseProvider,
			turn:             1,
			queueReservation: reservation,
		}
		a.active = active
		a.starting = false
		reserved = false
		return active, initial, nil
	}()
	return active, initial, err
}

func (a *Agent) reserveQueue(active *activeRun, steering bool) ([]llm.ConversationMessage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active != active || active.queueReservation != nil {
		return nil, fmt.Errorf("%w: queue reservation belongs to an inactive or already-reserved run", ErrInvariant)
	}
	messages, reservation, err := a.reserveQueueLocked(steering)
	if err != nil {
		return nil, err
	}
	active.queueReservation = reservation
	return messages, nil
}

func (a *Agent) reserveQueueLocked(steering bool) ([]llm.ConversationMessage, *queueReservation, error) {
	queue := &a.followUpQueue
	reserved := &a.followUpReserved
	mode := a.config.followUpMode
	if steering {
		queue, reserved, mode = &a.steeringQueue, &a.steeringReserved, a.config.steeringMode
	}
	if *reserved != 0 || *reserved > len(*queue) {
		return nil, nil, fmt.Errorf("%w: invalid queue reservation state", ErrInvariant)
	}
	if len(*queue) == 0 {
		return nil, nil, nil
	}
	items := queuePrefix(*queue, mode)
	*reserved = len(items)
	return items, &queueReservation{steering: steering, remaining: len(items)}, nil
}

func queuePrefix(queue []llm.ConversationMessage, mode QueueMode) []llm.ConversationMessage {
	if len(queue) == 0 {
		return nil
	}
	n := 1
	if mode == QueueAll {
		n = len(queue)
	}
	return append([]llm.ConversationMessage(nil), queue[:n]...)
}

func discardQueuePrefix(queue *[]llm.ConversationMessage, count int) {
	copy(*queue, (*queue)[count:])
	for index := len(*queue) - count; index < len(*queue); index++ {
		(*queue)[index] = nil
	}
	*queue = (*queue)[:len(*queue)-count]
}

func (a *Agent) runV2(active *activeRun, initial []llm.ConversationMessage) (result Result, runErr error) {
	return a.runV2WithAgentMessages(active, initial, nil)
}

func (a *Agent) runV2WithAgentMessages(active *activeRun, initial []llm.ConversationMessage, extra []agentmsg.Message) (result Result, runErr error) {
	result.runID = active.id
	// Continue from an assistant tail may already have reserved one steering
	// message as the run's prompt. Upstream skips only that one initial steering
	// poll; ordinary prompts, tool-result continuations, and follow-up-backed
	// continuations all admit steering before their first provider request.
	skipInitialSteeringPoll := a.hasInitialSteeringReservation(active)
	defer a.finishRun(active)
	defer func() {
		a.enterSettling(active)
		result.providerTurns, result.toolExecutions = a.runCounts(active)
		a.notify(active.ctx, AgentEndEvent{
			RunID: active.id, Turn: a.runTurn(active), Messages: a.runMessages(active),
			Terminal: result.terminal, Err: runErr,
		})
	}()
	a.notify(active.ctx, AgentStartEvent{RunID: active.id})
	turn := uint32(1)
	turnStarted := len(initial) > 0 || len(extra) > 0
	if turnStarted {
		a.notify(active.ctx, TurnStartEvent{RunID: active.id, Turn: turn})
		if err := a.commitQueued(active, turn, initial); err != nil {
			return result, err
		}
		for _, message := range extra {
			if err := a.commitAgentMessage(active, turn, message); err != nil {
				return result, err
			}
		}
	}
	if !skipInitialSteeringPoll {
		queued, err := a.reserveQueue(active, true)
		if err != nil {
			return result, err
		}
		if len(queued) > 0 {
			if !turnStarted {
				a.notify(active.ctx, TurnStartEvent{RunID: active.id, Turn: turn})
			}
			if err := a.commitQueued(active, turn, queued); err != nil {
				return result, err
			}
			turnStarted = true
		}
	}
	for {
		if !turnStarted {
			a.notify(active.ctx, TurnStartEvent{RunID: active.id, Turn: turn})
			turnStarted = true
		}
		terminal, err := a.providerTurnV2(active, turn)
		if err != nil {
			return result, err
		}
		terminal, err = a.commitAssistantV2(active, turn, terminal)
		if err != nil {
			return result, err
		}
		toolUse, usesTools := terminal.(llm.AssistantToolUseMessage)
		if !usesTools {
			if err := a.notifyTurnEnd(active, turn, terminal); err != nil {
				return result, err
			}
			if terminal.FinishReason() == llm.FinishError || terminal.FinishReason() == llm.FinishAborted {
				return a.acceptFinalV2(active, result, terminal)
			}
			queued, err := a.reserveQueue(active, true)
			if err != nil {
				return result, err
			}
			if len(queued) > 0 {
				turn++
				if err := a.startQueuedTurnV2(active, turn, queued); err != nil {
					return result, err
				}
				turnStarted = true
				continue
			}
			queued, err = a.reserveQueue(active, false)
			if err != nil {
				return result, err
			}
			if len(queued) > 0 {
				turn++
				if err := a.startQueuedTurnV2(active, turn, queued); err != nil {
					return result, err
				}
				turnStarted = true
				continue
			}
			return a.acceptFinalV2(active, result, terminal)
		}

		batch, err := a.executeBatchV2(active, turn, toolUse)
		if err != nil {
			return result, err
		}
		for _, outcome := range batch.outcomes {
			if err := a.commitToolResultV2(active, turn, outcome); err != nil {
				return result, err
			}
		}
		if err := a.completeToolBatchV2(active, turn); err != nil {
			return result, err
		}
		if err := a.notifyTurnEnd(active, turn, toolUse); err != nil {
			return result, err
		}
		if batch.cancelled || a.runCause(active) != nil {
			failure, err := a.failureTerminal(nil, llm.FinishAborted, runToolCancelText, a.contextCause(active), llm.Usage{})
			if err != nil {
				return result, err
			}
			turn++
			if err := a.setRunPhaseV2(active, turn, PhaseSettling); err != nil {
				return result, err
			}
			a.notify(active.ctx, TurnStartEvent{RunID: active.id, Turn: turn})
			committedFailure, err := a.commitAssistantV2(active, turn, failure)
			if err != nil {
				return result, err
			}
			if err := a.notifyTurnEnd(active, turn, committedFailure); err != nil {
				return result, err
			}
			return a.acceptFinalV2(active, result, committedFailure)
		}
		queued, err := a.reserveQueue(active, true)
		if err != nil {
			return result, err
		}
		if len(queued) == 0 && batch.terminate {
			queued, err = a.reserveQueue(active, false)
			if err != nil {
				return result, err
			}
		}
		if len(queued) > 0 {
			turn++
			if err := a.startQueuedTurnV2(active, turn, queued); err != nil {
				return result, err
			}
			turnStarted = true
			continue
		}
		if batch.terminate {
			return a.acceptFinalV2(active, result, toolUse)
		}
		turn++
		turnStarted = false
		if err := a.setRunPhaseV2(active, turn, PhaseProvider); err != nil {
			return result, err
		}
	}
}

func (a *Agent) hasInitialSteeringReservation(active *activeRun) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active == active && active.queueReservation != nil && active.queueReservation.steering
}

func (a *Agent) startQueuedTurnV2(active *activeRun, turn uint32, messages []llm.ConversationMessage) error {
	if err := a.setRunPhaseV2(active, turn, PhaseProvider); err != nil {
		return err
	}
	a.notify(active.ctx, TurnStartEvent{RunID: active.id, Turn: turn})
	return a.commitQueued(active, turn, messages)
}

func (a *Agent) setRunPhaseV2(active *activeRun, turn uint32, phase Phase) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active != active || active.terminalAccepted {
		return fmt.Errorf("%w: inactive turn transition", ErrInvariant)
	}
	active.turn = turn
	active.phase = phase
	active.pendingToolCalls = nil
	return nil
}

func (a *Agent) completeToolBatchV2(active *activeRun, turn uint32) error {
	return a.setRunPhaseV2(active, turn, PhaseSettling)
}

func (a *Agent) commitQueued(active *activeRun, turn uint32, messages []llm.ConversationMessage) error {
	a.mu.Lock()
	queueBacked := active.queueReservation != nil
	if queueBacked && (a.active != active || active.queueReservation.remaining != len(messages)) {
		a.mu.Unlock()
		return fmt.Errorf("%w: queued commit does not match its reservation", ErrInvariant)
	}
	a.mu.Unlock()
	for _, message := range messages {
		if queueBacked {
			if _, err := a.commitAfterAppend(active, turn, message, nil, func() error {
				if err := a.ackQueueReservation(active); err != nil {
					return err
				}
				// Match pi's queue contract: consumers see the updated queue before
				// this queued user message emits message_start/message_end.
				a.notify(active.ctx, QueueUpdateEvent{RunID: active.id, Turn: turn})
				return nil
			}); err != nil {
				return err
			}
			continue
		}
		if _, err := a.commit(active, turn, message); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) ackQueueReservation(active *activeRun) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active != active || active.queueReservation == nil || active.queueReservation.remaining <= 0 {
		return fmt.Errorf("%w: missing queue acknowledgement reservation", ErrInvariant)
	}
	reservation := active.queueReservation
	queue, reserved := a.queueStateLocked(reservation.steering)
	if *reserved != reservation.remaining || len(*queue) < *reserved {
		return fmt.Errorf("%w: queue acknowledgement state mismatch", ErrInvariant)
	}
	discardQueuePrefix(queue, 1)
	*reserved--
	reservation.remaining--
	if reservation.remaining == 0 {
		active.queueReservation = nil
	}
	return nil
}

func (a *Agent) releaseQueueReservationLocked(active *activeRun) {
	if active == nil || active.queueReservation == nil {
		return
	}
	reservation := active.queueReservation
	_, reserved := a.queueStateLocked(reservation.steering)
	if reservation.remaining <= *reserved {
		*reserved -= reservation.remaining
	} else {
		// Preserve every queued message if an invariant failure reaches teardown.
		*reserved = 0
	}
	active.queueReservation = nil
}

func (a *Agent) queueStateLocked(steering bool) (*[]llm.ConversationMessage, *int) {
	if steering {
		return &a.steeringQueue, &a.steeringReserved
	}
	return &a.followUpQueue, &a.followUpReserved
}

func (a *Agent) commitAssistantV2(active *activeRun, turn uint32, terminal llm.AssistantTerminal) (llm.AssistantTerminal, error) {
	committed, err := a.commit(active, turn, terminal)
	if err != nil {
		return nil, err
	}
	final, ok := committed.(llm.AssistantTerminal)
	if !ok {
		return nil, fmt.Errorf("%w: message_end assistant replacement is not terminal", ErrInvariant)
	}
	return final, nil
}

func (a *Agent) acceptFinalV2(active *activeRun, result Result, terminal llm.AssistantTerminal) (Result, error) {
	a.mu.Lock()
	if a.active != active || active.terminalAccepted {
		a.mu.Unlock()
		return result, fmt.Errorf("%w: duplicate terminal", ErrInvariant)
	}
	active.terminalAccepted = true
	active.phase = PhaseSettling
	a.mu.Unlock()
	result.terminal = terminal
	return result, nil
}

func (a *Agent) providerTurnV2(active *activeRun, turn uint32) (llm.AssistantTerminal, error) {
	var providerAttempt uint32
	var chainAttempt uint32
	overflowRetried := false
	retryInFlight := false
	for {
		providerAttempt++
		chainAttempt++
		retryOpen := retryInFlight
		retryInFlight = false
		finishRetry := func(kind provider.FailureKind, status int, succeeded bool, reason provider.RetryFinishReason) {
			if !retryOpen {
				return
			}
			a.notify(active.ctx, ProviderRetryFinishedEvent{
				RunID: active.id, Turn: turn, Attempt: providerAttempt,
				FailureKind: kind, HTTPStatus: status, Succeeded: succeeded,
				FinishReason: reason,
			})
			retryOpen = false
		}
		if cause := a.runCause(active); cause != nil {
			finishRetry(provider.FailureCancelled, 0, false, provider.RetryFinishCancelled)
			return a.failureTerminal(nil, llm.FinishAborted, "Run cancelled before provider execution", cause, llm.Usage{})
		}
		if retryOpen {
			// Attempt means request reconstruction has begun. Cancellation
			// observed before this point closes the scheduled scope without an
			// attempt; transform/build failures after it still get a finish.
			a.notify(active.ctx, ProviderRetryAttemptEvent{RunID: active.id, Turn: turn, Attempt: providerAttempt})
		}
		// A snapshot is the atomic turn configuration boundary: request,
		// threshold policy, terminal provenance and the next tool batch all see
		// this same value. Retries deliberately obtain a fresh snapshot.
		turnSnapshot, err := a.prepareTurnSnapshot(active)
		if err != nil {
			return nil, err
		}
		if providerAttempt == 1 {
			if err := a.compactBeforeProvider(active, turn, turnSnapshot); err != nil {
				if cause := a.runCause(active); cause != nil {
					return a.failureTerminal(nil, llm.FinishAborted, "Run cancelled during context compaction", cause, llm.Usage{})
				}
				return nil, err
			}
		}
		request, err := a.providerRequest(active, turnSnapshot)
		if err != nil {
			if cause := a.runCause(active); cause != nil {
				finishRetry(provider.FailureCancelled, 0, false, provider.RetryFinishCancelled)
				return a.failureTerminal(nil, llm.FinishAborted, "Run cancelled before provider execution", cause, llm.Usage{})
			}
			finishRetry(provider.FailureInvalidRequest, 0, false, provider.RetryFinishFailed)
			return nil, err
		}
		a.mu.Lock()
		if a.active != active || active.terminalAccepted {
			a.mu.Unlock()
			finishRetry(provider.FailureInvalidResponse, 0, false, provider.RetryFinishFailed)
			return nil, fmt.Errorf("%w: inactive provider turn", ErrInvariant)
		}
		active.phase, active.turn = PhaseProvider, turn
		active.providerTurns++
		a.mu.Unlock()
		terminal, streamErr := a.collectProvider(active, turn, request)
		if streamErr == nil && (a.runCause(active) != nil && terminal.FinishReason() != llm.FinishAborted) {
			terminal, streamErr = nil, a.contextCause(active)
		}
		providerFailure := providerFailureFromTerminal(terminal)
		retryable := a.retryableProviderOutcome(active, terminal, streamErr)
		kind, status, succeeded := retryOutcome(terminal, streamErr)
		finishReason := provider.RetryFinishFailed
		switch {
		case succeeded:
			finishReason = provider.RetryFinishSucceeded
		case a.runCause(active) != nil || kind == provider.FailureCancelled:
			kind = provider.FailureCancelled
			finishReason = provider.RetryFinishCancelled
		case retryable && chainAttempt >= a.config.retry.MaxAttempts():
			finishReason = provider.RetryFinishExhausted
		}
		finishRetry(kind, status, succeeded, finishReason)
		if !overflowRetried && providerFailure != nil && providerFailure.Kind() == provider.FailureContextOverflow && a.config.compactor != nil && a.config.summarizer != nil {
			overflowRetried = true
			if err := a.compactProviderContext(active, turn, CompactionContextOverflow); err != nil {
				if cause := a.runCause(active); cause != nil {
					return a.failureTerminal(nil, llm.FinishAborted, "Run cancelled during context overflow compaction", cause, llm.Usage{})
				}
				return nil, fmt.Errorf("context overflow compaction: %w", err)
			}
			chainAttempt = 0
			status, _ := providerFailure.HTTPStatus()
			a.notify(active.ctx, ProviderRetryScheduledEvent{
				RunID: active.id, Turn: turn, Attempt: providerAttempt + 1,
				FailureKind: provider.FailureContextOverflow, HTTPStatus: status,
			})
			retryInFlight = true
			continue
		}
		if !retryable || chainAttempt >= a.config.retry.MaxAttempts() {
			if streamErr != nil {
				reason, text, cause := llm.FinishError, "Provider stream failed", streamErr
				if runCause := a.runCause(active); runCause != nil {
					reason, text, cause = llm.FinishAborted, "Run cancelled during provider execution", errors.Join(runCause, streamErr)
				}
				return a.failureTerminal(nil, reason, text, cause, llm.Usage{})
			}
			return terminal, nil
		}
		delay := a.config.retry.Delay(chainAttempt+1, providerFailure)
		a.mu.Lock()
		if a.active != active {
			a.mu.Unlock()
			return nil, fmt.Errorf("%w: inactive retry", ErrInvariant)
		}
		active.phase = PhaseRetryWait
		a.mu.Unlock()
		a.notify(active.ctx, ProviderRetryScheduledEvent{
			RunID: active.id, Turn: turn, Attempt: providerAttempt + 1,
			Delay: delay, FailureKind: kind, HTTPStatus: status,
		})
		// Failed attempts remain runtime-only. runV2 commits only the terminal
		// returned from this function, so a resend is rebuilt from unchanged
		// durable conversation context.
		if err := a.config.retry.Wait(active.ctx, delay); err != nil {
			a.notify(active.ctx, ProviderRetryFinishedEvent{
				RunID: active.id, Turn: turn, Attempt: providerAttempt + 1,
				FailureKind: provider.FailureCancelled, FinishReason: provider.RetryFinishCancelled,
			})
			return a.failureTerminal(nil, llm.FinishAborted, "Run cancelled while waiting to retry provider", err, llm.Usage{})
		}
		retryInFlight = true
	}
}

func (a *Agent) providerRequest(active *activeRun, turnSnapshot TurnSnapshot) (provider.Request, error) {
	var snapshot session.Context
	if builder, ok := a.config.transcript.(ContextBuilder); ok {
		snapshot = builder.BuildContext()
	} else {
		snapshot = a.config.transcript.Context()
	}
	messages := snapshot.Messages()
	if a.config.transformAgentContext != nil {
		replacement, err := a.transformAgentContextV2(active.ctx, snapshot.AgentMessages())
		if err != nil {
			return provider.Request{}, err
		}
		if replacement != nil {
			messages, err = agentmsg.ConvertToLLM(*replacement)
			if err != nil {
				return provider.Request{}, fmt.Errorf("%w: project agent context: %w", ErrContextTransform, err)
			}
		}
	}
	if a.config.transformContext != nil {
		transformed, err := a.transformV2(active.ctx, messages)
		if err != nil {
			return provider.Request{}, err
		}
		messages = transformed
	}
	request, err := provider.NewRequestWithOptions(turnSnapshot.Model, turnSnapshot.SystemPrompt, messages, provider.RequestOptions{
		Tools:                  turnSnapshot.Tools,
		AllowParallelToolCalls: effectiveToolExecutionMode(a.config.toolExecution, turnSnapshot.Tool, toolDefinitionNames(turnSnapshot.Tools)) == ToolExecutionParallel,
		ThinkingLevel:          turnSnapshot.ThinkingLevel,
		Stream:                 turnSnapshot.Stream,
	})
	if err != nil {
		return provider.Request{}, fmt.Errorf("%w: build provider request: %w", ErrInvariant, err)
	}
	return request, nil
}

func (a *Agent) transformAgentContextV2(ctx context.Context, messages []agentmsg.Message) (transformed *[]agentmsg.Message, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			transformed = nil
			err = fmt.Errorf("%w: panic: %s\n%s", ErrContextTransform, safeValueText(recovered), debug.Stack())
		}
	}()
	transformed, err = a.config.transformAgentContext(ctx, agentmsg.Clone(messages))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrContextTransform, err)
	}
	if transformed == nil {
		return nil, nil
	}
	clone := agentmsg.Clone(*transformed)
	return &clone, nil
}

func toolDefinitionNames(definitions []provider.ToolDefinition) []string {
	names := make([]string, len(definitions))
	for index, definition := range definitions {
		names[index] = definition.Name()
	}
	return names
}

func (a *Agent) prepareTurnSnapshot(active *activeRun) (TurnSnapshot, error) {
	turn := a.runTurn(active)
	snapshot := TurnSnapshot{
		Model: a.config.model, SystemPrompt: a.config.systemPrompt,
		Tool: a.config.tool, Tools: append([]provider.ToolDefinition(nil), a.config.tools...),
		BeforeToolCall: a.config.beforeToolCall, AfterToolCall: a.config.afterToolCall,
	}
	if a.config.prepareTurn != nil {
		var err error
		snapshot, err = a.config.prepareTurn(active.ctx, TurnContext{RunID: active.id, Turn: turn})
		if err != nil {
			return TurnSnapshot{}, fmt.Errorf("%w: prepare turn: %w", ErrInvariant, err)
		}
		snapshot.Tools = append([]provider.ToolDefinition(nil), snapshot.Tools...)
	}
	if _, err := provider.NewRequestWithOptions(snapshot.Model, snapshot.SystemPrompt, nil, provider.RequestOptions{Tools: snapshot.Tools, ThinkingLevel: snapshot.ThinkingLevel}); err != nil {
		return TurnSnapshot{}, fmt.Errorf("%w: invalid turn snapshot: %w", ErrInvariant, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active != active || active.terminalAccepted {
		return TurnSnapshot{}, fmt.Errorf("%w: inactive turn snapshot", ErrInvariant)
	}
	copy := snapshot
	active.snapshot = &copy
	return snapshot, nil
}

func (a *Agent) activeSnapshot(active *activeRun) (TurnSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active != active || active.snapshot == nil {
		return TurnSnapshot{}, fmt.Errorf("%w: missing active turn snapshot", ErrInvariant)
	}
	copy := *active.snapshot
	copy.Tools = append([]provider.ToolDefinition(nil), copy.Tools...)
	return copy, nil
}

func (a *Agent) compactBeforeProvider(active *activeRun, turn uint32, snapshot TurnSnapshot) error {
	window, reserve := a.config.contextWindow, a.config.contextReserve
	// A model catalog window is metadata, not consent to mutate transcript.
	// Automatic compaction is enabled only by explicit session policy.
	if window == 0 {
		return nil
	}
	if snapshot.Model.ContextWindow() != 0 {
		window = snapshot.Model.ContextWindow()
	}
	if snapshot.Model.MaxTokens() != 0 {
		reserve = snapshot.Model.MaxTokens()
	}
	if a.config.compactor == nil || a.config.summarizer == nil {
		return fmt.Errorf("%w: model context window requires configured compaction", ErrCompactionUnavailable)
	}
	contextSnapshot := a.contextSnapshot()
	compact, err := session.ShouldCompact(contextSnapshot.Messages(), window, reserve)
	if err != nil {
		return fmt.Errorf("context threshold: %w", err)
	}
	if !compact {
		return nil
	}
	return a.compactProviderContext(active, turn, CompactionThreshold)
}

func (a *Agent) compactProviderContext(active *activeRun, turn uint32, reason CompactionReason) error {
	a.mu.Lock()
	if a.active != active || active.terminalAccepted {
		a.mu.Unlock()
		return fmt.Errorf("%w: inactive compaction", ErrInvariant)
	}
	active.phase = PhaseCompacting
	a.mu.Unlock()
	willRetry := reason == CompactionContextOverflow
	a.notify(active.ctx, CompactionStartEvent{RunID: active.id, Turn: turn, Reason: reason, WillRetry: willRetry})
	result, err := a.config.compactor.Compact(active.ctx, session.CompactRequest{
		KeepRecentTokens: a.config.keepRecentTokens,
		Summarizer:       a.observedSummarizer(active, turn, reason),
	})
	if err != nil {
		eventErr := safeCompactionEventError(err)
		a.notify(active.ctx, CompactionEndEvent{
			RunID: active.id, Turn: turn, Reason: reason,
			Aborted: context.Cause(active.ctx) != nil, WillRetry: willRetry,
			ErrorMessage: eventErr.Error(), Err: eventErr,
		})
		return fmt.Errorf("automatic context compaction: %w", err)
	}
	a.notify(active.ctx, CompactionEndEvent{RunID: active.id, Turn: turn, Reason: reason, Result: &result, WillRetry: willRetry})
	return nil
}

type summarizerWithRetryObserver interface {
	SummarizeWithRetryObserver(context.Context, session.SummaryInput, provider.RetryObserver) (session.SummaryOutput, error)
}

type observedSummarizer struct {
	agent  *Agent
	active *activeRun
	turn   uint32
	reason CompactionReason
	base   session.Summarizer
}

func (a *Agent) observedSummarizer(active *activeRun, turn uint32, reason CompactionReason) session.Summarizer {
	return observedSummarizer{agent: a, active: active, turn: turn, reason: reason, base: a.config.summarizer}
}

func (s observedSummarizer) Summarize(ctx context.Context, input session.SummaryInput) (session.SummaryOutput, error) {
	observable, ok := s.base.(summarizerWithRetryObserver)
	if !ok {
		return s.base.Summarize(ctx, input)
	}
	return observable.SummarizeWithRetryObserver(ctx, input, func(_ context.Context, retry provider.RetryEvent) {
		switch retry.Kind {
		case provider.RetryScheduled:
			s.agent.notify(s.active.ctx, SummarizationRetryScheduledEvent{
				RunID: s.active.id, Turn: s.turn, Reason: s.reason, Attempt: retry.Attempt,
				Delay: retry.Delay, FailureKind: retry.FailureKind, HTTPStatus: retry.HTTPStatus,
			})
		case provider.RetryAttempt:
			s.agent.notify(s.active.ctx, SummarizationRetryAttemptEvent{
				RunID: s.active.id, Turn: s.turn, Reason: s.reason, Attempt: retry.Attempt,
			})
		case provider.RetryFinished:
			s.agent.notify(s.active.ctx, SummarizationRetryFinishedEvent{
				RunID: s.active.id, Turn: s.turn, Reason: s.reason, Attempt: retry.Attempt,
				FailureKind: retry.FailureKind, HTTPStatus: retry.HTTPStatus,
				Succeeded: retry.Succeeded, FinishReason: retry.FinishReason,
			})
		}
	})
}

func safeCompactionEventError(err error) error {
	for _, sentinel := range []error{
		session.ErrAppendCanceled, session.ErrCompactionConflict, session.ErrCommitUnknown,
		session.ErrSummaryFailed, session.ErrAlreadyCompacted, session.ErrNothingToCompact,
		session.ErrTokenEstimateOverflow, session.ErrPoisoned, session.ErrClosed,
		session.ErrStorage, session.ErrInvalidEntry, session.ErrInvalidSession,
		session.ErrIDGeneration, session.ErrEntryIDExhausted, session.ErrWriterActive,
	} {
		if errors.Is(err, sentinel) {
			return sentinel
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return session.ErrAppendCanceled
	}
	return session.ErrSummaryFailed
}

func (a *Agent) contextSnapshot() session.Context {
	if builder, ok := a.config.transcript.(ContextBuilder); ok {
		return builder.BuildContext()
	}
	return a.config.transcript.Context()
}

func (a *Agent) retryableProviderOutcome(active *activeRun, terminal llm.AssistantTerminal, streamErr error) bool {
	if a.runCause(active) != nil {
		return false
	}
	if streamErr != nil {
		return provider.IsTransientStreamError(streamErr)
	}
	return provider.IsTransientFailure(providerFailureFromTerminal(terminal))
}

func providerFailureFromTerminal(terminal llm.AssistantTerminal) *provider.ProviderFailure {
	failure, ok := terminal.(llm.AssistantFailureMessage)
	if !ok {
		return nil
	}
	var providerFailure *provider.ProviderFailure
	if !errors.As(failure.Failure().Cause(), &providerFailure) {
		return nil
	}
	return providerFailure
}

func retryOutcome(terminal llm.AssistantTerminal, streamErr error) (provider.FailureKind, int, bool) {
	if streamErr != nil {
		if provider.IsTransientStreamError(streamErr) {
			return provider.FailureTransport, 0, false
		}
		return provider.FailureInvalidResponse, 0, false
	}
	if failure := providerFailureFromTerminal(terminal); failure != nil {
		status, _ := failure.HTTPStatus()
		return failure.Kind(), status, false
	}
	if terminal == nil || terminal.FinishReason() == llm.FinishError || terminal.FinishReason() == llm.FinishAborted {
		return provider.FailureInvalidResponse, 0, false
	}
	return 0, 0, true
}

func (a *Agent) transformV2(ctx context.Context, messages []llm.ConversationMessage) (transformed []llm.ConversationMessage, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			transformed = nil
			err = fmt.Errorf("%w: panic: %s\n%s", ErrContextTransform, safeValueText(recovered), debug.Stack())
		}
	}()
	transformed, err = a.config.transformContext(ctx, append([]llm.ConversationMessage(nil), messages...))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrContextTransform, err)
	}
	return append([]llm.ConversationMessage(nil), transformed...), nil
}

type batchOutcome struct {
	call      llm.ToolCallBlock
	output    ToolOutput
	err       error
	cancelled bool
}
type toolBatch struct {
	outcomes  []batchOutcome
	terminate bool
	cancelled bool
}

func (a *Agent) executeBatchV2(active *activeRun, turn uint32, assistant llm.AssistantToolUseMessage) (toolBatch, error) {
	snapshot, err := a.activeSnapshot(active)
	if err != nil {
		return toolBatch{}, err
	}
	calls := toolCalls(assistant)
	if len(calls) == 0 {
		return toolBatch{}, fmt.Errorf("%w: empty tool batch", ErrInvariant)
	}
	names := make([]string, len(calls))
	for index, call := range calls {
		names[index] = call.Name()
	}
	sequential := effectiveToolExecutionMode(a.config.toolExecution, snapshot.Tool, names) == ToolExecutionSequential
	a.mu.Lock()
	if a.active != active {
		a.mu.Unlock()
		return toolBatch{}, fmt.Errorf("%w: inactive tool batch", ErrInvariant)
	}
	active.phase = PhaseTool
	active.pendingToolCalls = nil
	a.mu.Unlock()
	results := make([]batchOutcome, len(calls))
	if sequential {
		for i, call := range calls {
			outcome := a.executeOneV2(active, turn, snapshot, assistant, call)
			results[i] = outcome
			a.emitToolSettled(active, turn, outcome)
			if outcome.cancelled {
				// Keep the durable batch closed without starting later calls. Each
				// unstarted source call receives an explicit associated cancellation
				// result; no zero call identity can reach session validation.
				for remaining := i + 1; remaining < len(calls); remaining++ {
					results[remaining] = batchOutcome{
						call:      calls[remaining],
						output:    ToolOutput{Text: toolCancellationText},
						err:       errors.Join(ErrAgentAborted, a.contextCause(active)),
						cancelled: true,
					}
				}
				break
			}
		}
	} else {
		var workers sync.WaitGroup
		completed := make(chan batchOutcome, len(calls))
		type preparedCall struct {
			index int
			call  llm.ToolCallBlock
		}
		prepared := make([]preparedCall, 0, len(calls))
		for i, call := range calls {
			if err := a.beginToolCallV2(active, turn, call); err != nil {
				return toolBatch{}, err
			}
			if immediate, ok := a.preflightToolCallV2(active, snapshot, assistant, call); ok {
				results[i] = immediate
				a.emitToolSettled(active, turn, immediate)
				continue
			} else if immediate.call.ID() != "" {
				call = immediate.call
			}
			prepared = append(prepared, preparedCall{index: i, call: call})
		}
		for _, item := range prepared {
			workers.Add(1)
			go func(index int, c llm.ToolCallBlock) {
				defer workers.Done()
				outcome := a.executeOneNoStartV2(active, turn, snapshot, assistant, c, true)
				outcome.call = c
				results[index] = outcome
				completed <- outcome
			}(item.index, item.call)
		}
		go func() { workers.Wait(); close(completed) }()
		for outcome := range completed {
			a.emitToolSettled(active, turn, outcome)
		}
	}
	batch := toolBatch{outcomes: results}
	allTerminate := len(results) > 0
	for _, outcome := range results {
		batch.cancelled = batch.cancelled || outcome.cancelled
		allTerminate = allTerminate && outcome.output.Terminate
	}
	batch.terminate = allTerminate
	return batch, nil
}

func (a *Agent) emitToolStart(active *activeRun, turn uint32, call llm.ToolCallBlock) {
	a.notify(active.ctx, ToolExecutionStartEvent{RunID: active.id, Turn: turn, ToolCallID: call.ID(), ToolName: call.Name(), Arguments: call.ArgumentsJSON()})
}
func (a *Agent) emitToolSettled(active *activeRun, turn uint32, outcome batchOutcome) {
	a.removePendingToolCallV2(active, outcome.call.ID())
	a.notify(active.ctx, ToolExecutionEndEvent{
		RunID: active.id, Turn: turn, ToolCallID: outcome.call.ID(), ToolName: outcome.call.Name(),
		Arguments: outcome.call.ArgumentsJSON(), Result: cloneToolOutput(outcome.output),
		IsError: outcome.err != nil, Err: outcome.err,
	})
}
func (a *Agent) executeOneV2(active *activeRun, turn uint32, snapshot TurnSnapshot, assistant llm.AssistantToolUseMessage, call llm.ToolCallBlock) batchOutcome {
	if err := a.beginToolCallV2(active, turn, call); err != nil {
		return batchOutcome{call: call, err: err}
	}
	return a.executeOneNoStartV2(active, turn, snapshot, assistant, call, false)
}

// preflightToolCallV2 is deliberately called by the parallel path in source
// order before any worker launches. It owns lookup and before-hook decisions;
// prepared executions then overlap without racing hook ordering.
func (a *Agent) preflightToolCallV2(active *activeRun, snapshot TurnSnapshot, assistant llm.AssistantToolUseMessage, call llm.ToolCallBlock) (batchOutcome, bool) {
	if a.runCause(active) != nil {
		return batchOutcome{call: call, output: ToolOutput{Text: toolCancellationText}, err: ErrAgentAborted, cancelled: true}, true
	}
	supported, lookupErr := supportsToolCall(snapshot.Tool, call.Name())
	if lookupErr != nil {
		return batchOutcome{call: call, output: ToolOutput{Text: lookupErr.Error()}, err: lookupErr}, true
	}
	if !supported {
		return batchOutcome{call: call, output: ToolOutput{Text: fmt.Sprintf("Tool %s not found", call.Name())}, err: fmt.Errorf("%w: %s", ErrToolNotFound, call.Name())}, true
	}
	if snapshot.BeforeToolCall == nil {
		return batchOutcome{}, false
	}
	before, err := callBeforeToolHook(snapshot.BeforeToolCall, active.ctx, BeforeToolCallContext{Assistant: assistant, ToolCall: call, Arguments: call.ArgumentsJSON(), Context: a.contextSnapshot().Messages()})
	if err != nil || before.Block {
		reason := before.Reason
		if reason == "" && err != nil {
			reason = err.Error()
		}
		if reason == "" {
			reason = "Tool execution was blocked"
		}
		return batchOutcome{call: call, output: ToolOutput{Text: reason}, err: errors.New(reason)}, true
	}
	if before.Arguments != nil {
		updated, updateErr := llm.NewToolCallBlock(call.ID(), call.Name(), *before.Arguments)
		if updateErr != nil {
			return batchOutcome{call: call, output: ToolOutput{Text: updateErr.Error()}, err: updateErr}, true
		}
		call = updated
	}
	return batchOutcome{call: call}, false
}

func (a *Agent) beginToolCallV2(active *activeRun, turn uint32, call llm.ToolCallBlock) error {
	a.mu.Lock()
	if a.active != active || active.terminalAccepted {
		a.mu.Unlock()
		return fmt.Errorf("%w: inactive tool state transition", ErrInvariant)
	}
	active.pendingToolCalls = append(active.pendingToolCalls, call.ID())
	a.mu.Unlock()
	a.emitToolStart(active, turn, call)
	return nil
}

func (a *Agent) removePendingToolCallV2(active *activeRun, id string) {
	a.mu.Lock()
	if a.active == active {
		for index, pending := range active.pendingToolCalls {
			if pending != id {
				continue
			}
			copy(active.pendingToolCalls[index:], active.pendingToolCalls[index+1:])
			active.pendingToolCalls[len(active.pendingToolCalls)-1] = ""
			active.pendingToolCalls = active.pendingToolCalls[:len(active.pendingToolCalls)-1]
			break
		}
	}
	a.mu.Unlock()
}

func (a *Agent) executeOneNoStartV2(active *activeRun, turn uint32, snapshot TurnSnapshot, assistant llm.AssistantToolUseMessage, call llm.ToolCallBlock, preflighted bool) batchOutcome {
	outcome := batchOutcome{call: call}
	if a.runCause(active) != nil {
		outcome.output, outcome.err, outcome.cancelled = ToolOutput{Text: toolCancellationText}, ErrAgentAborted, true
		return outcome
	}
	contextMessages := a.contextSnapshot().Messages()
	if !preflighted {
		supported, lookupErr := supportsToolCall(snapshot.Tool, call.Name())
		if lookupErr != nil {
			outcome.output, outcome.err = ToolOutput{Text: lookupErr.Error()}, lookupErr
			return outcome
		}
		if !supported {
			outcome.output, outcome.err = ToolOutput{Text: fmt.Sprintf("Tool %s not found", call.Name())}, fmt.Errorf("%w: %s", ErrToolNotFound, call.Name())
			return outcome
		}
		if snapshot.BeforeToolCall != nil {
			before, beforeErr := callBeforeToolHook(snapshot.BeforeToolCall, active.ctx, BeforeToolCallContext{Assistant: assistant, ToolCall: call, Arguments: call.ArgumentsJSON(), Context: contextMessages})
			if beforeErr != nil || before.Block {
				reason := before.Reason
				if reason == "" && beforeErr != nil {
					reason = beforeErr.Error()
				}
				if reason == "" {
					reason = "Tool execution was blocked"
				}
				outcome.output, outcome.err = ToolOutput{Text: reason}, errors.New(reason)
				return outcome
			}
			if before.Arguments != nil {
				updated, updateErr := llm.NewToolCallBlock(call.ID(), call.Name(), *before.Arguments)
				if updateErr != nil {
					outcome.output, outcome.err = ToolOutput{Text: updateErr.Error()}, updateErr
					return outcome
				}
				call = updated
				outcome.call = updated
			}
		}
	}
	a.mu.Lock()
	if a.active != active || active.terminalAccepted {
		a.mu.Unlock()
		outcome.err = fmt.Errorf("%w: inactive tool", ErrInvariant)
		return outcome
	}
	active.toolExecutions++
	a.mu.Unlock()
	var updates sync.Mutex
	accepting := true
	report := func(update ToolUpdate) {
		updates.Lock()
		defer updates.Unlock()
		if !accepting || !validToolUpdate(update) {
			return
		}
		update = cloneToolUpdate(update)
		a.notify(active.ctx, ToolExecutionUpdateEvent{
			RunID: active.id, Turn: turn, ToolCallID: call.ID(), ToolName: call.Name(),
			Arguments: call.ArgumentsJSON(), PartialResult: update,
		})
	}
	outcome.output, outcome.err = executeNamedToolSafely(snapshot.Tool, active.ctx, call.Name(), call.ArgumentsJSON(), report)
	updates.Lock()
	accepting = false
	updates.Unlock()
	outcome.output, outcome.err = normalizeToolOutcome(outcome.output, outcome.err)
	outcome.output = ownToolOutput(outcome.output)
	if snapshot.AfterToolCall != nil {
		after, afterErr := callAfterToolHook(snapshot.AfterToolCall, active.ctx, AfterToolCallContext{Assistant: assistant, ToolCall: call, Arguments: call.ArgumentsJSON(), Context: contextMessages, Result: cloneToolOutput(outcome.output), IsError: outcome.err != nil})
		if afterErr != nil {
			outcome.output, outcome.err = ToolOutput{Text: afterErr.Error()}, afterErr
		} else {
			if after.Content != nil {
				outcome.output.Content = append([]llm.ToolResultContentBlock(nil), (*after.Content)...)
				outcome.output.Text = ""
			}
			if after.Details != nil {
				if details, ok := cloneToolDetails(*after.Details); ok {
					outcome.output.Details = details
				} else {
					// Preserve the invalid value for commitToolResultV2's existing
					// fatal validation path without exposing it to observers.
					outcome.output.Details = *after.Details
				}
			}
			if after.Usage != nil {
				usage := *after.Usage
				outcome.output.Usage = &usage
			}
			if after.AddedToolNames != nil {
				outcome.output.AddedToolNames = append([]string(nil), (*after.AddedToolNames)...)
			}
			if after.Terminate != nil {
				outcome.output.Terminate = *after.Terminate
			}
			if after.IsError != nil {
				if *after.IsError && outcome.err == nil {
					outcome.err = errors.New("tool result marked as error")
				}
				if !*after.IsError {
					outcome.err = nil
				}
			}
		}
	}
	a.mu.Lock()
	outcome.cancelled = a.active == active && context.Cause(active.ctx) != nil
	a.mu.Unlock()
	if outcome.cancelled {
		outcome.output, outcome.err = ToolOutput{Text: toolCancellationText}, errors.Join(ErrAgentAborted, a.contextCause(active))
	}
	return outcome
}

func callBeforeToolHook(hook BeforeToolCallHook, ctx context.Context, input BeforeToolCallContext) (result BeforeToolCallResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("before tool hook panicked: %v", recovered)
		}
	}()
	return hook(ctx, input)
}
func callAfterToolHook(hook AfterToolCallHook, ctx context.Context, input AfterToolCallContext) (result AfterToolCallResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("after tool hook panicked: %v", recovered)
		}
	}()
	return hook(ctx, input)
}

func validToolUpdate(update ToolUpdate) bool {
	if !utf8.ValidString(update.Text) {
		return false
	}
	for _, block := range update.Content {
		switch block.(type) {
		case llm.TextBlock, llm.ImageBlock:
		default:
			return false
		}
	}
	if _, ok := cloneToolDetails(update.Details); !ok {
		return false
	}
	seen := map[string]struct{}{}
	for _, name := range update.AddedToolNames {
		if !utf8.ValidString(name) || name == "" {
			return false
		}
		if _, ok := seen[name]; ok {
			return false
		}
		seen[name] = struct{}{}
	}
	return true
}

// supportsToolCall turns registry/name lookup failures into associated tool
// results. A registry is extension code, so even a panicking Supports method
// must not tear down the batch or leave a started call without settlement.
func supportsToolCall(executor ToolExecutor, requestedName string) (supported bool, err error) {
	if executor == nil {
		return false, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("tool lookup panicked: %s", safeValueText(recovered))
		}
	}()
	if named, ok := executor.(NamedToolExecutor); ok {
		return named.Supports(requestedName), nil
	}
	configuredName, err := configuredToolName(executor)
	if err != nil {
		return false, err
	}
	return configuredName == requestedName, nil
}

func (a *Agent) commitToolResultV2(active *activeRun, turn uint32, outcome batchOutcome) error {
	timestamp, err := a.now()
	if err != nil {
		return err
	}
	var message llm.ConversationMessage
	var details json.RawMessage
	if outcome.output.Details != nil {
		encoded, marshalErr := json.Marshal(outcome.output.Details)
		if marshalErr != nil {
			return fmt.Errorf("%w: tool details: %w", ErrInvariant, marshalErr)
		}
		details = encoded
	}
	metadata := llm.ToolResultMetadata{Details: details, Usage: outcome.output.Usage, AddedToolNames: outcome.output.AddedToolNames}
	if len(outcome.output.Content) != 0 {
		message, err = llm.NewToolResultContentMessageWithMetadata(outcome.call.ID(), outcome.call.Name(), outcome.output.Content, outcome.err != nil, timestamp, metadata)
	} else {
		block, blockErr := llm.NewTextBlock(outcome.output.Text)
		if blockErr != nil {
			return fmt.Errorf("%w: tool text: %w", ErrInvariant, blockErr)
		}
		message, err = llm.NewToolResultMessageWithMetadata(outcome.call.ID(), outcome.call.Name(), []llm.TextBlock{block}, outcome.err != nil, timestamp, metadata)
	}
	if err != nil {
		return fmt.Errorf("%w: tool result: %w", ErrInvariant, err)
	}
	validateAssociation := func(candidate llm.ConversationMessage) error {
		switch result := candidate.(type) {
		case llm.ToolResultMessage:
			if err := llm.ValidateToolResultAssociation(outcome.call, result); err != nil {
				return fmt.Errorf("%w: %w", ErrInvariant, err)
			}
		case llm.ToolResultContentMessage:
			if err := llm.ValidateToolResultContentAssociation(outcome.call, result); err != nil {
				return fmt.Errorf("%w: %w", ErrInvariant, err)
			}
		default:
			return fmt.Errorf("%w: unexpected tool result %T", ErrInvariant, candidate)
		}
		return nil
	}
	if _, err := a.commitAfterAppend(active, turn, message, validateAssociation, nil); err != nil {
		return err
	}
	return nil
}

package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
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
	if a == nil {
		return nil, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.UserTextMessage(nil), a.steeringQueue...), append([]llm.UserTextMessage(nil), a.followUpQueue...)
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

func clearUnreservedQueue(queue *[]llm.UserTextMessage, reserved int) {
	for index := reserved; index < len(*queue); index++ {
		(*queue)[index] = llm.UserTextMessage{}
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

func (a *Agent) admitContinuation(ctx context.Context) (*activeRun, []llm.UserTextMessage, error) {
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
	active, initial, err := func() (*activeRun, []llm.UserTextMessage, error) {
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
		var initial []llm.UserTextMessage
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

func (a *Agent) reserveQueue(active *activeRun, steering bool) ([]llm.UserTextMessage, error) {
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

func (a *Agent) reserveQueueLocked(steering bool) ([]llm.UserTextMessage, *queueReservation, error) {
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

func queuePrefix(queue []llm.UserTextMessage, mode QueueMode) []llm.UserTextMessage {
	if len(queue) == 0 {
		return nil
	}
	n := 1
	if mode == QueueAll {
		n = len(queue)
	}
	return append([]llm.UserTextMessage(nil), queue[:n]...)
}

func discardQueuePrefix(queue *[]llm.UserTextMessage, count int) {
	copy(*queue, (*queue)[count:])
	for index := len(*queue) - count; index < len(*queue); index++ {
		(*queue)[index] = llm.UserTextMessage{}
	}
	*queue = (*queue)[:len(*queue)-count]
}

func (a *Agent) runV2(active *activeRun, initial []llm.UserTextMessage) (result Result, runErr error) {
	result.runID = active.id
	defer a.finishRun(active)
	defer func() {
		a.enterSettling(active)
		result.providerTurns, result.toolExecutions = a.runCounts(active)
		a.notify(active.ctx, Event{Kind: EventRunSettled, RunID: active.id, Turn: a.runTurn(active), Terminal: result.terminal, RunError: runErr})
	}()
	a.notify(active.ctx, Event{Kind: EventRunStarted, RunID: active.id})
	turn := uint32(1)
	if len(initial) > 0 {
		a.notify(active.ctx, Event{Kind: EventTurnStarted, RunID: active.id, Turn: turn})
		if err := a.commitQueued(active, turn, initial); err != nil {
			return result, err
		}
	}
	for {
		if len(initial) == 0 {
			a.notify(active.ctx, Event{Kind: EventTurnStarted, RunID: active.id, Turn: turn})
		}
		terminal, err := a.providerTurnV2(active, turn)
		if err != nil {
			return result, err
		}
		toolUse, usesTools := terminal.(llm.AssistantToolUseMessage)
		if !usesTools {
			if err := a.commitAssistantV2(active, turn, terminal); err != nil {
				return result, err
			}
			a.notify(active.ctx, Event{Kind: EventTurnSettled, RunID: active.id, Turn: turn, Terminal: terminal})
			if terminal.FinishReason() == llm.FinishError || terminal.FinishReason() == llm.FinishAborted {
				return a.acceptFinalV2(active, result, terminal)
			}
			queued, err := a.reserveQueue(active, true)
			if err != nil {
				return result, err
			}
			if len(queued) > 0 {
				turn++
				initial = queued
				if err := a.startQueuedTurnV2(active, turn, queued); err != nil {
					return result, err
				}
				continue
			}
			queued, err = a.reserveQueue(active, false)
			if err != nil {
				return result, err
			}
			if len(queued) > 0 {
				turn++
				initial = queued
				if err := a.startQueuedTurnV2(active, turn, queued); err != nil {
					return result, err
				}
				continue
			}
			return a.acceptFinalV2(active, result, terminal)
		}

		if err := a.commit(active, turn, toolUse); err != nil {
			return result, err
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
		a.notify(active.ctx, Event{Kind: EventTurnSettled, RunID: active.id, Turn: turn, Terminal: toolUse})
		if batch.cancelled || a.runCause(active) != nil {
			failure, err := a.failureTerminal(nil, llm.FinishAborted, runToolCancelText, a.contextCause(active), llm.Usage{})
			if err != nil {
				return result, err
			}
			turn++
			if err := a.setRunPhaseV2(active, turn, PhaseSettling); err != nil {
				return result, err
			}
			a.notify(active.ctx, Event{Kind: EventTurnStarted, RunID: active.id, Turn: turn})
			if err := a.commitAssistantV2(active, turn, failure); err != nil {
				return result, err
			}
			a.notify(active.ctx, Event{Kind: EventTurnSettled, RunID: active.id, Turn: turn, Terminal: failure})
			return a.acceptFinalV2(active, result, failure)
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
			initial = queued
			if err := a.startQueuedTurnV2(active, turn, queued); err != nil {
				return result, err
			}
			continue
		}
		if batch.terminate {
			return a.acceptFinalV2(active, result, toolUse)
		}
		turn++
		initial = nil
		if err := a.setRunPhaseV2(active, turn, PhaseProvider); err != nil {
			return result, err
		}
	}
}

func (a *Agent) startQueuedTurnV2(active *activeRun, turn uint32, messages []llm.UserTextMessage) error {
	if err := a.setRunPhaseV2(active, turn, PhaseProvider); err != nil {
		return err
	}
	a.notify(active.ctx, Event{Kind: EventTurnStarted, RunID: active.id, Turn: turn})
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

func (a *Agent) commitQueued(active *activeRun, turn uint32, messages []llm.UserTextMessage) error {
	a.mu.Lock()
	queueBacked := active.queueReservation != nil
	if queueBacked && (a.active != active || active.queueReservation.remaining != len(messages)) {
		a.mu.Unlock()
		return fmt.Errorf("%w: queued commit does not match its reservation", ErrInvariant)
	}
	a.mu.Unlock()
	for _, message := range messages {
		if queueBacked {
			if err := a.commitAfterAppend(active, turn, message, func() error {
				return a.ackQueueReservation(active)
			}); err != nil {
				return err
			}
			continue
		}
		if err := a.commit(active, turn, message); err != nil {
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

func (a *Agent) queueStateLocked(steering bool) (*[]llm.UserTextMessage, *int) {
	if steering {
		return &a.steeringQueue, &a.steeringReserved
	}
	return &a.followUpQueue, &a.followUpReserved
}

func (a *Agent) commitAssistantV2(active *activeRun, turn uint32, terminal llm.AssistantTerminal) error {
	if err := a.commit(active, turn, terminal); err != nil {
		return err
	}
	return nil
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
	messages := a.config.transcript.Context().Messages()
	if a.config.transformContext != nil {
		if cause := a.runCause(active); cause != nil {
			return a.failureTerminal(nil, llm.FinishAborted, "Run cancelled before provider execution", cause, llm.Usage{})
		}
		transformed, err := a.transformV2(active.ctx, messages)
		if err != nil {
			if cause := a.runCause(active); cause != nil {
				return a.failureTerminal(nil, llm.FinishAborted, "Run cancelled before provider execution", cause, llm.Usage{})
			}
			return nil, err
		}
		if context.Cause(active.ctx) != nil {
			return a.failureTerminal(nil, llm.FinishAborted, "Run cancelled before provider execution", a.contextCause(active), llm.Usage{})
		}
		messages = append([]llm.ConversationMessage(nil), transformed...)
	}
	request, err := provider.NewRequest(a.config.model, a.config.systemPrompt, messages)
	if err != nil {
		return nil, fmt.Errorf("%w: build provider request: %w", ErrInvariant, err)
	}
	a.mu.Lock()
	if a.active != active || active.terminalAccepted {
		a.mu.Unlock()
		return nil, fmt.Errorf("%w: inactive provider turn", ErrInvariant)
	}
	active.phase, active.turn = PhaseProvider, turn
	active.providerTurns++
	a.mu.Unlock()
	terminal, streamErr := a.collectProvider(active, turn, request)
	if streamErr != nil {
		reason, text, cause := llm.FinishError, "Provider stream failed", streamErr
		if runCause := a.runCause(active); runCause != nil {
			reason, text, cause = llm.FinishAborted, "Run cancelled during provider execution", errors.Join(runCause, streamErr)
		}
		return a.failureTerminal(nil, reason, text, cause, llm.Usage{})
	}
	if cause := a.runCause(active); cause != nil && terminal.FinishReason() != llm.FinishAborted {
		return a.failureTerminal(nil, llm.FinishAborted, "Run cancelled during provider execution", cause, llm.Usage{})
	}
	return terminal, nil
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
	calls := toolCalls(assistant)
	if len(calls) == 0 {
		return toolBatch{}, fmt.Errorf("%w: empty tool batch", ErrInvariant)
	}
	sequential := a.config.toolExecution == ToolExecutionSequential
	if modes, ok := a.config.tool.(ToolExecutionOverride); ok {
		for _, call := range calls {
			if mode, set := modes.ToolExecutionMode(call.Name()); set && mode == ToolExecutionSequential {
				sequential = true
			}
		}
	}
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
			outcome := a.executeOneV2(active, turn, call)
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
		for i, call := range calls {
			if err := a.beginToolCallV2(active, turn, call); err != nil {
				return toolBatch{}, err
			}
			workers.Add(1)
			go func(index int, c llm.ToolCallBlock) {
				defer workers.Done()
				outcome := a.executeOneNoStartV2(active, turn, c)
				outcome.call = c
				results[index] = outcome
				completed <- outcome
			}(i, call)
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
	a.notify(active.ctx, Event{Kind: EventToolStarted, RunID: active.id, Turn: turn, ToolCallID: call.ID(), ToolName: call.Name()})
}
func (a *Agent) emitToolSettled(active *activeRun, turn uint32, outcome batchOutcome) {
	a.removePendingToolCallV2(active, outcome.call.ID())
	a.notify(active.ctx, Event{Kind: EventToolSettled, RunID: active.id, Turn: turn, ToolCallID: outcome.call.ID(), ToolName: outcome.call.Name(), ToolOutput: outcome.output, ToolError: outcome.err})
}
func (a *Agent) executeOneV2(active *activeRun, turn uint32, call llm.ToolCallBlock) batchOutcome {
	if err := a.beginToolCallV2(active, turn, call); err != nil {
		return batchOutcome{call: call, err: err}
	}
	return a.executeOneNoStartV2(active, turn, call)
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

func (a *Agent) executeOneNoStartV2(active *activeRun, turn uint32, call llm.ToolCallBlock) batchOutcome {
	outcome := batchOutcome{call: call}
	if a.runCause(active) != nil {
		outcome.output, outcome.err, outcome.cancelled = ToolOutput{Text: toolCancellationText}, ErrAgentAborted, true
		return outcome
	}
	if a.config.tool == nil || !toolSupports(a.config.tool, a.config.toolName, call.Name()) {
		outcome.output, outcome.err = ToolOutput{Text: fmt.Sprintf("Tool %s not found", call.Name())}, fmt.Errorf("%w: %s", ErrToolNotFound, call.Name())
		return outcome
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
		a.notify(active.ctx, Event{Kind: EventToolProgress, RunID: active.id, Turn: turn, ToolCallID: call.ID(), ToolName: call.Name(), ToolUpdate: update})
	}
	outcome.output, outcome.err = executeNamedToolSafely(a.config.tool, active.ctx, call.Name(), call.ArgumentsJSON(), report)
	updates.Lock()
	accepting = false
	updates.Unlock()
	outcome.output, outcome.err = normalizeToolOutcome(outcome.output, outcome.err)
	a.mu.Lock()
	outcome.cancelled = a.active == active && context.Cause(active.ctx) != nil
	a.mu.Unlock()
	if outcome.cancelled {
		outcome.output, outcome.err = ToolOutput{Text: toolCancellationText}, errors.Join(ErrAgentAborted, a.contextCause(active))
	}
	return outcome
}

func validToolUpdate(update ToolUpdate) bool { return utf8.ValidString(update.Text) }

func toolSupports(executor ToolExecutor, configuredName, requestedName string) bool {
	if named, ok := executor.(NamedToolExecutor); ok {
		return named.Supports(requestedName)
	}
	return configuredName == requestedName
}

func (a *Agent) commitToolResultV2(active *activeRun, turn uint32, outcome batchOutcome) error {
	block, err := llm.NewTextBlock(outcome.output.Text)
	if err != nil {
		return fmt.Errorf("%w: tool text: %w", ErrInvariant, err)
	}
	timestamp, err := a.now()
	if err != nil {
		return err
	}
	message, err := llm.NewToolResultMessage(outcome.call.ID(), outcome.call.Name(), []llm.TextBlock{block}, outcome.err != nil, timestamp)
	if err != nil {
		return fmt.Errorf("%w: tool result: %w", ErrInvariant, err)
	}
	if err := llm.ValidateToolResultAssociation(outcome.call, message); err != nil {
		return fmt.Errorf("%w: %w", ErrInvariant, err)
	}
	if err := a.commit(active, turn, message); err != nil {
		return err
	}
	return nil
}

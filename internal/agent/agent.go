package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"runtime/debug"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

const (
	toolCancellationText = "Tool execution cancelled"
	runToolCancelText    = "Run cancelled during tool execution"
)

type activeRun struct {
	id               uint64
	ctx              context.Context
	cancel           context.CancelCauseFunc
	done             chan struct{}
	phase            Phase
	turn             uint32
	pendingToolCall  string
	pendingToolName  string
	providerTurns    uint32
	toolExecutions   uint32
	terminalAccepted bool
}

type observerEntry struct {
	id       uint64
	observer Observer
}

// Agent is the single owner of volatile run state. Provider, tool, transcript,
// and observers are always called without holding mu.
type Agent struct {
	mu sync.Mutex
	// clockMu serializes an injected clock independently from coordinator state.
	clockMu sync.Mutex

	config runtimeConfig
	active *activeRun
	// starting reserves the single-run slot while prompt preflight constructs
	// immutable values. It prevents a busy loser from invoking the shared clock.
	starting bool
	nextID   uint64

	observers      []observerEntry
	nextObserverID uint64
}

func New(config Config) (*Agent, error) {
	runtime, err := validateConfig(config)
	if err != nil {
		return nil, err
	}
	return &Agent{config: runtime}, nil
}

// Subscribe adds an observer in deterministic subscription order. Unsubscribe
// is idempotent; it affects subsequent notifications, not a callback snapshot
// already in progress.
func (a *Agent) Subscribe(observer Observer) func() {
	if a == nil || observer == nil {
		return func() {}
	}
	a.mu.Lock()
	a.nextObserverID++
	id := a.nextObserverID
	a.observers = append(a.observers, observerEntry{id: id, observer: observer})
	a.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			for index := range a.observers {
				if a.observers[index].id == id {
					copy(a.observers[index:], a.observers[index+1:])
					a.observers[len(a.observers)-1] = observerEntry{}
					a.observers = a.observers[:len(a.observers)-1]
					break
				}
			}
			a.mu.Unlock()
		})
	}
}

func (a *Agent) State() State {
	if a == nil {
		return State{phase: PhaseIdle}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active == nil {
		return State{phase: PhaseIdle}
	}
	return State{
		phase:           a.active.phase,
		runID:           a.active.id,
		turn:            a.active.turn,
		pendingToolCall: a.active.pendingToolCall,
	}
}

// Abort idempotently cancels the active generation and waits for its complete
// settlement. A caller deadline only stops this wait; it does not make the run
// idle or permit a second run. Because observers are part of settlement, an
// observer must not synchronously call Abort for the run invoking it.
func (a *Agent) Abort(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	active := a.active
	if active == nil {
		a.mu.Unlock()
		return nil
	}
	active.cancel(ErrAgentAborted)
	done := active.done
	a.mu.Unlock()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// WaitForIdle waits for the generation active at call time. It never cancels
// the run. If the Agent is already idle it returns immediately. An active-run
// observer must not synchronously wait for its own settlement.
func (a *Agent) WaitForIdle(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	active := a.active
	if active == nil {
		a.mu.Unlock()
		return nil
	}
	done := active.done
	a.mu.Unlock()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// Run accepts one text prompt while idle and synchronously settles its entire
// provider/tool/transcript/observer lifecycle before returning.
func (a *Agent) Run(ctx context.Context, prompt string) (result Result, runErr error) {
	active, user, err := a.beginRun(ctx, prompt)
	if err != nil {
		return Result{}, err
	}
	result.runID = active.id

	defer a.finishRun(active)
	defer func() {
		a.enterSettling(active)
		result.providerTurns, result.toolExecutions = a.runCounts(active)
		a.notify(active.ctx, Event{
			Kind:     EventRunSettled,
			RunID:    active.id,
			Turn:     a.runTurn(active),
			Terminal: result.terminal,
			RunError: runErr,
		})
	}()

	a.notify(active.ctx, Event{Kind: EventRunStarted, RunID: active.id})
	a.notify(active.ctx, Event{Kind: EventTurnStarted, RunID: active.id, Turn: 1})
	if err := a.commit(active, 1, user); err != nil {
		return result, err
	}

	terminal, err := a.providerTurn(active, 1)
	if err != nil {
		return result, err
	}
	if toolUse, ok := terminal.(llm.AssistantToolUseMessage); ok {
		return a.runToolTurn(active, toolUse, result)
	}
	return a.commitTerminal(active, 1, terminal, result)
}

func (a *Agent) beginRun(
	ctx context.Context,
	prompt string,
) (*activeRun, llm.UserTextMessage, error) {
	if a == nil {
		return nil, llm.UserTextMessage{}, fmt.Errorf("%w: nil agent", ErrInvalidRun)
	}
	if ctx == nil {
		return nil, llm.UserTextMessage{}, fmt.Errorf("%w: context is nil", ErrInvalidRun)
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, llm.UserTextMessage{}, fmt.Errorf("%w: context already cancelled: %w", ErrInvalidRun, cause)
	}

	a.mu.Lock()
	if a.active != nil || a.starting {
		a.mu.Unlock()
		return nil, llm.UserTextMessage{}, ErrBusy
	}
	if a.nextID == math.MaxUint64 {
		a.mu.Unlock()
		return nil, llm.UserTextMessage{}, ErrRunIDExhausted
	}
	a.starting = true
	a.mu.Unlock()

	reserved := true
	defer func() {
		if reserved {
			a.mu.Lock()
			a.starting = false
			a.mu.Unlock()
		}
	}()

	timestamp, err := a.now()
	if err != nil {
		return nil, llm.UserTextMessage{}, fmt.Errorf("%w: prompt timestamp: %w", ErrInvalidRun, err)
	}
	user, err := llm.NewUserTextMessage(prompt, timestamp)
	if err != nil {
		return nil, llm.UserTextMessage{}, fmt.Errorf("%w: prompt: %w", ErrInvalidRun, err)
	}

	a.mu.Lock()
	if !a.starting || a.active != nil {
		a.mu.Unlock()
		return nil, llm.UserTextMessage{}, fmt.Errorf("%w: run reservation was lost", ErrInvariant)
	}
	a.nextID++
	runContext, cancel := context.WithCancelCause(ctx)
	active := &activeRun{
		id:     a.nextID,
		ctx:    runContext,
		cancel: cancel,
		done:   make(chan struct{}),
		phase:  PhaseProvider,
		turn:   1,
	}
	a.starting = false
	a.active = active
	reserved = false
	a.mu.Unlock()
	return active, user, nil
}

func (a *Agent) finishRun(active *activeRun) {
	a.mu.Lock()
	if a.active == active {
		a.active = nil
		active.cancel(context.Canceled)
		close(active.done)
	}
	a.mu.Unlock()
}

func (a *Agent) enterSettling(active *activeRun) {
	a.mu.Lock()
	if a.active == active {
		active.phase = PhaseSettling
		active.pendingToolCall = ""
		active.pendingToolName = ""
	}
	a.mu.Unlock()
}

func (a *Agent) runCounts(active *activeRun) (uint32, uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return active.providerTurns, active.toolExecutions
}

func (a *Agent) runTurn(active *activeRun) uint32 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return active.turn
}

func (a *Agent) runCause(active *activeRun) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active != active || active.terminalAccepted {
		return nil
	}
	return context.Cause(active.ctx)
}

func (a *Agent) acceptTerminal(
	active *activeRun,
	terminal llm.AssistantTerminal,
) (llm.AssistantTerminal, error) {
	a.mu.Lock()
	if a.active != active {
		a.mu.Unlock()
		return nil, fmt.Errorf("%w: accepted terminal belongs to an inactive run", ErrInvariant)
	}
	if active.terminalAccepted {
		a.mu.Unlock()
		return nil, fmt.Errorf("%w: run already accepted a terminal", ErrInvariant)
	}
	cause := context.Cause(active.ctx)
	active.terminalAccepted = true
	active.phase = PhaseSettling
	a.mu.Unlock()

	// Abort and terminal acceptance share the mutex above. If cancellation won,
	// it replaces a non-aborted provider outcome exactly once. A terminal that
	// already represents provider or tool cancellation remains authoritative.
	if cause != nil && terminal.FinishReason() != llm.FinishAborted {
		cancelled, err := a.failureTerminal(
			nil,
			llm.FinishAborted,
			"Run cancelled during provider execution",
			cause,
			llm.Usage{},
		)
		if err != nil {
			return nil, err
		}
		return cancelled, nil
	}
	return terminal, nil
}

func (a *Agent) notify(ctx context.Context, event Event) {
	a.mu.Lock()
	observers := make([]Observer, 0, len(a.observers))
	for _, entry := range a.observers {
		if entry.observer != nil {
			observers = append(observers, entry.observer)
		}
	}
	a.mu.Unlock()
	for _, observer := range observers {
		observer(ctx, event)
	}
}

func (a *Agent) commit(active *activeRun, turn uint32, message llm.ConversationMessage) error {
	settlementBase := context.WithoutCancel(active.ctx)
	settlement, cancel := context.WithTimeout(settlementBase, a.config.settlementTimeout)
	options := session.AppendOptions{}
	if message.Role() == llm.RoleAssistant {
		options.Assistant = session.AssistantProvenance{
			API:      a.config.model.API(),
			Provider: a.config.model.Provider(),
			Model:    a.config.model.ID(),
			Cost:     session.ZeroUsageCost(),
		}
	}
	_, err := a.config.transcript.Append(settlement, message, options)
	cancel()
	if err != nil {
		return fmt.Errorf("%w: %s message: %w", ErrTranscriptCommit, message.Role(), err)
	}
	a.notify(active.ctx, Event{
		Kind:    EventMessageCommitted,
		RunID:   active.id,
		Turn:    turn,
		Message: message,
	})
	return nil
}

func (a *Agent) commitTerminal(
	active *activeRun,
	turn uint32,
	terminal llm.AssistantTerminal,
	result Result,
) (Result, error) {
	terminal, err := a.acceptTerminal(active, terminal)
	if err != nil {
		return result, err
	}
	if err := a.commit(active, turn, terminal); err != nil {
		return result, err
	}
	a.notify(active.ctx, Event{
		Kind:     EventTurnSettled,
		RunID:    active.id,
		Turn:     turn,
		Terminal: terminal,
	})
	result.terminal = terminal
	return result, nil
}

func (a *Agent) providerTurn(active *activeRun, turn uint32) (llm.AssistantTerminal, error) {
	messages := a.config.transcript.Context().Messages()
	request, err := provider.NewRequestWithTools(a.config.model, a.config.systemPrompt, messages, a.config.tools)
	if err != nil {
		return nil, fmt.Errorf("%w: build provider request: %w", ErrInvariant, err)
	}

	a.mu.Lock()
	if a.active != active || active.terminalAccepted {
		a.mu.Unlock()
		return nil, fmt.Errorf("%w: provider turn belongs to an inactive or terminal run", ErrInvariant)
	}
	active.phase = PhaseProvider
	active.turn = turn
	active.providerTurns++
	a.mu.Unlock()

	terminal, streamErr := a.collectProvider(active, turn, request)
	if streamErr != nil {
		reason := llm.FinishError
		message := "Provider stream failed"
		cause := streamErr
		if runCause := a.runCause(active); runCause != nil {
			reason = llm.FinishAborted
			message = "Run cancelled during provider execution"
			cause = errors.Join(runCause, streamErr)
		}
		terminal, err = a.failureTerminal(nil, reason, message, cause, llm.Usage{})
		if err != nil {
			return nil, err
		}
	}

	if cause := a.runCause(active); cause != nil && terminal.FinishReason() != llm.FinishAborted {
		terminal, err = a.failureTerminal(
			nil,
			llm.FinishAborted,
			"Run cancelled during provider execution",
			cause,
			llm.Usage{},
		)
		if err != nil {
			return nil, err
		}
	}

	toolUse, isToolUse := terminal.(llm.AssistantToolUseMessage)
	if !isToolUse {
		return terminal, nil
	}
	calls := toolCalls(toolUse)
	if turn == 1 && len(calls) == 1 {
		a.mu.Lock()
		cause := context.Cause(active.ctx)
		if cause == nil && a.active == active && !active.terminalAccepted {
			active.phase = PhaseTool
			a.mu.Unlock()
			return terminal, nil
		}
		a.mu.Unlock()
		if cause == nil {
			return nil, fmt.Errorf("%w: tool-use outcome belongs to an inactive run", ErrInvariant)
		}
		terminal, err = a.failureTerminal(
			nil,
			llm.FinishAborted,
			"Run cancelled during provider execution",
			cause,
			llm.Usage{},
		)
		if err != nil {
			return nil, err
		}
		return terminal, nil
	}
	text := assistantTextBlocks(toolUse)
	terminal, err = a.failureTerminal(
		text,
		llm.FinishError,
		"Agent v0.1 supports one tool call in the first provider turn",
		ErrUnsupportedToolTurn,
		toolUse.Usage(),
	)
	if err != nil {
		return nil, err
	}
	return terminal, nil
}

func (a *Agent) collectProvider(
	active *activeRun,
	turn uint32,
	request provider.Request,
) (terminal llm.AssistantTerminal, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			terminal = nil
			err = fmt.Errorf("%w: panic: %s\n%s", ErrProviderStream, safeValueText(recovered), debug.Stack())
		}
	}()

	stream := a.config.provider.Stream(active.ctx, request)
	if isNilInterface(stream) {
		return nil, fmt.Errorf("%w: provider returned a nil stream", ErrProviderStream)
	}
	closed := false
	defer func() {
		if !closed {
			_ = stream.Close()
		}
	}()

	collector := &llm.StreamCollector{}
	for {
		event, nextErr := stream.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return nil, fmt.Errorf("%w: next event: %w", ErrProviderStream, nextErr)
		}
		if acceptErr := collector.Accept(event); acceptErr != nil {
			return nil, fmt.Errorf("%w: collect event: %w", ErrProviderStream, acceptErr)
		}
		switch event.(type) {
		case llm.DoneEvent, llm.ErrorEvent:
			// The durable MessageCommitted event is the terminal notification.
			// Progress observers only see partial snapshots.
			continue
		}
		snapshot, snapshotErr := collector.Snapshot()
		if snapshotErr != nil {
			return nil, fmt.Errorf("%w: snapshot: %w", ErrProviderStream, snapshotErr)
		}
		a.notify(active.ctx, Event{
			Kind:             EventProviderProgress,
			RunID:            active.id,
			Turn:             turn,
			ProviderSnapshot: snapshot,
		})
	}

	// Claim the one allowed close attempt before invoking foreign code. If
	// Close panics, the recovery above must not retry a potentially non-idempotent
	// operation while unwinding.
	closed = true
	closeErr := stream.Close()
	if closeErr != nil {
		return nil, fmt.Errorf("%w: close provider stream: %w", ErrProviderStream, closeErr)
	}
	if err := collector.Close(); err != nil {
		return nil, fmt.Errorf("%w: close collector: %w", ErrProviderStream, err)
	}
	terminal, err = collector.Result()
	if err != nil {
		return nil, fmt.Errorf("%w: collect result: %w", ErrProviderStream, err)
	}
	return terminal, nil
}

func (a *Agent) runToolTurn(
	active *activeRun,
	toolUse llm.AssistantToolUseMessage,
	result Result,
) (Result, error) {
	calls := toolCalls(toolUse)
	if len(calls) != 1 {
		return result, fmt.Errorf("%w: normalized tool turn has %d calls", ErrInvariant, len(calls))
	}
	call := calls[0]
	if err := a.commit(active, 1, toolUse); err != nil {
		return result, err
	}

	a.mu.Lock()
	if a.active != active || active.terminalAccepted {
		a.mu.Unlock()
		return result, fmt.Errorf("%w: tool turn belongs to an inactive or terminal run", ErrInvariant)
	}
	active.phase = PhaseTool
	active.pendingToolCall = call.ID()
	active.pendingToolName = call.Name()
	a.mu.Unlock()
	a.notify(active.ctx, Event{
		Kind:       EventToolStarted,
		RunID:      active.id,
		Turn:       1,
		ToolCallID: call.ID(),
		ToolName:   call.Name(),
	})

	output, toolErr, cancelled := a.executeToolCall(active, call)
	if cancelled {
		output = ToolOutput{Text: toolCancellationText}
		toolErr = errors.Join(ErrAgentAborted, a.contextCause(active))
	}
	a.notify(active.ctx, Event{
		Kind:       EventToolSettled,
		RunID:      active.id,
		Turn:       1,
		ToolCallID: call.ID(),
		ToolName:   call.Name(),
		ToolOutput: output,
		ToolError:  toolErr,
	})

	block, err := llm.NewTextBlock(output.Text)
	if err != nil {
		return result, fmt.Errorf("%w: tool result text: %w", ErrInvariant, err)
	}
	timestamp, err := a.now()
	if err != nil {
		return result, err
	}
	toolResult, err := llm.NewToolResultMessage(
		call.ID(),
		call.Name(),
		[]llm.TextBlock{block},
		toolErr != nil,
		timestamp,
	)
	if err != nil {
		return result, fmt.Errorf("%w: construct tool result: %w", ErrInvariant, err)
	}
	if err := llm.ValidateToolResultAssociation(call, toolResult); err != nil {
		return result, fmt.Errorf("%w: %w", ErrInvariant, err)
	}
	if err := a.commit(active, 1, toolResult); err != nil {
		return result, err
	}

	a.mu.Lock()
	if a.active != active {
		a.mu.Unlock()
		return result, fmt.Errorf("%w: tool result committed to an inactive run", ErrInvariant)
	}
	active.pendingToolCall = ""
	active.pendingToolName = ""
	a.mu.Unlock()
	a.notify(active.ctx, Event{
		Kind:     EventTurnSettled,
		RunID:    active.id,
		Turn:     1,
		Terminal: toolUse,
	})

	if cancelled || a.runCause(active) != nil {
		return a.commitToolCancellationTerminal(active, result)
	}

	a.mu.Lock()
	if a.active != active || active.terminalAccepted {
		a.mu.Unlock()
		return result, fmt.Errorf("%w: cannot start second provider turn", ErrInvariant)
	}
	if cause := context.Cause(active.ctx); cause != nil {
		a.mu.Unlock()
		return a.commitToolCancellationTerminal(active, result)
	}
	active.turn = 2
	active.phase = PhaseProvider
	a.mu.Unlock()
	a.notify(active.ctx, Event{Kind: EventTurnStarted, RunID: active.id, Turn: 2})

	terminal, err := a.providerTurn(active, 2)
	if err != nil {
		return result, err
	}
	return a.commitTerminal(active, 2, terminal, result)
}

func (a *Agent) executeToolCall(
	active *activeRun,
	call llm.ToolCallBlock,
) (ToolOutput, error, bool) {
	if a.runCause(active) != nil {
		return ToolOutput{Text: toolCancellationText}, ErrAgentAborted, true
	}
	if a.config.tool == nil || !toolSupports(a.config.tool, a.config.toolName, call.Name()) {
		return ToolOutput{Text: fmt.Sprintf("Tool %s not found", call.Name())},
			fmt.Errorf("%w: %s", ErrToolNotFound, call.Name()), false
	}

	a.mu.Lock()
	if a.active != active || active.terminalAccepted {
		a.mu.Unlock()
		return ToolOutput{}, fmt.Errorf("%w: tool execution belongs to an inactive run", ErrInvariant), false
	}
	active.toolExecutions++
	a.mu.Unlock()

	var updateMu sync.Mutex
	acceptingUpdates := true
	report := func(update ToolUpdate) {
		updateMu.Lock()
		defer updateMu.Unlock()
		if !acceptingUpdates || !utf8.ValidString(update.Text) {
			return
		}
		a.notify(active.ctx, Event{
			Kind:       EventToolProgress,
			RunID:      active.id,
			Turn:       1,
			ToolCallID: call.ID(),
			ToolName:   call.Name(),
			ToolUpdate: update,
		})
	}
	output, toolErr := executeNamedToolSafely(a.config.tool, active.ctx, call.Name(), call.ArgumentsJSON(), report)
	updateMu.Lock()
	acceptingUpdates = false
	updateMu.Unlock()
	output, toolErr = normalizeToolOutcome(output, toolErr)

	// This is the normal-outcome/cancel linearization point. Abort uses the
	// same mutex; parent cancellation is classified by what the coordinator
	// observes here.
	a.mu.Lock()
	cancelled := a.active == active && context.Cause(active.ctx) != nil
	a.mu.Unlock()
	return output, toolErr, cancelled
}

func toolSupports(executor ToolExecutor, configuredName, requestedName string) bool {
	if named, ok := executor.(NamedToolExecutor); ok {
		return named.Supports(requestedName)
	}
	return configuredName == requestedName
}

func (a *Agent) commitToolCancellationTerminal(
	active *activeRun,
	result Result,
) (Result, error) {
	a.mu.Lock()
	if a.active != active || active.terminalAccepted {
		a.mu.Unlock()
		return result, fmt.Errorf("%w: cannot settle tool cancellation", ErrInvariant)
	}
	active.turn = 2
	a.mu.Unlock()
	a.notify(active.ctx, Event{Kind: EventTurnStarted, RunID: active.id, Turn: 2})

	terminal, err := a.failureTerminal(
		nil,
		llm.FinishAborted,
		runToolCancelText,
		a.contextCause(active),
		llm.Usage{},
	)
	if err != nil {
		return result, err
	}
	return a.commitTerminal(active, 2, terminal, result)
}

func (a *Agent) contextCause(active *activeRun) error {
	cause := context.Cause(active.ctx)
	if cause == nil {
		return ErrAgentAborted
	}
	return cause
}

func (a *Agent) failureTerminal(
	content []llm.TextBlock,
	reason llm.FinishReason,
	message string,
	cause error,
	usage llm.Usage,
) (llm.AssistantFailureMessage, error) {
	failure, err := llm.NewFailure(message, cause)
	if err != nil {
		return llm.AssistantFailureMessage{}, fmt.Errorf("%w: failure value: %w", ErrInvariant, err)
	}
	timestamp, err := a.now()
	if err != nil {
		return llm.AssistantFailureMessage{}, err
	}
	terminal, err := llm.NewAssistantFailureMessageWithFailure(content, reason, failure, usage, timestamp)
	if err != nil {
		return llm.AssistantFailureMessage{}, fmt.Errorf("%w: failure terminal: %w", ErrInvariant, err)
	}
	return terminal, nil
}

func (a *Agent) now() (value time.Time, err error) {
	a.clockMu.Lock()
	defer a.clockMu.Unlock()
	defer func() {
		if recovered := recover(); recovered != nil {
			value = time.Time{}
			err = fmt.Errorf("%w: clock panicked: %s", ErrInvariant, safeValueText(recovered))
		}
	}()
	value = a.config.now().UTC().Truncate(time.Millisecond)
	if value.IsZero() || !time.UnixMilli(value.UnixMilli()).Equal(value) {
		return time.Time{}, fmt.Errorf("%w: clock returned an unsupported timestamp", ErrInvariant)
	}
	return value, nil
}

func toolCalls(message llm.AssistantToolUseMessage) []llm.ToolCallBlock {
	calls := make([]llm.ToolCallBlock, 0, 1)
	for _, block := range message.Blocks() {
		if call, ok := block.(llm.ToolCallBlock); ok {
			calls = append(calls, call)
		}
	}
	return calls
}

func assistantTextBlocks(message llm.AssistantToolUseMessage) []llm.TextBlock {
	text := make([]llm.TextBlock, 0, len(message.Blocks()))
	for _, block := range message.Blocks() {
		if value, ok := block.(llm.TextBlock); ok {
			text = append(text, value)
		}
	}
	return text
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

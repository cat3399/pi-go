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

// Agent is the single owner of volatile run state. Provider, tool, transcript
// append, and observers are always called without holding mu. Continue reads a
// transcript snapshot while holding mu so tail admission and queue reservation
// share one linearization point.
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

	steeringQueue []llm.UserTextMessage
	followUpQueue []llm.UserTextMessage
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
	return a.runV2(active, []llm.UserTextMessage{user})
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

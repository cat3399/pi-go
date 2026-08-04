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

	"github.com/cat3399/pi-go/internal/agentmsg"
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
	pendingToolCalls []string
	providerTurns    uint32
	toolExecutions   uint32
	terminalAccepted bool
	queueReservation *queueReservation
	// snapshot is replaced only at the provider-turn boundary, before foreign
	// provider code starts. It is then the immutable configuration that owns
	// the resulting assistant/tool batch and its durable provenance.
	snapshot *TurnSnapshot
}

type observerEntry struct {
	id       uint64
	observer Observer
}

// Agent is the existing stateful execution coordinator. Provider, tool, transcript
// append, and observers are always called without holding mu. Continue reserves
// its single-run slot under mu, reads its transcript snapshot without mu, then
// validates the snapshot and consumes queues under mu.
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

	steeringQueue []llm.ConversationMessage
	followUpQueue []llm.ConversationMessage
	// Reserved entries are always a prefix. Clear operations preserve this
	// prefix; durable per-message acknowledgements remove it one entry at a time.
	steeringReserved int
	followUpReserved int
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
		phase:            a.active.phase,
		runID:            a.active.id,
		turn:             a.active.turn,
		pendingToolCalls: append([]string(nil), a.active.pendingToolCalls...),
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

// Compact performs an explicit manual compaction while the coordinator owns
// the active slot. It is intentionally separate from Run: it does not append a
// synthetic user message, and therefore cannot make a queued prompt appear to
// have been processed. The real provider call happens inside Session.Compact
// with no Agent or Session mutex held.
func (a *Agent) Compact(ctx context.Context, instructions string) (result session.CompactResult, compactErr error) {
	if a == nil {
		return session.CompactResult{}, fmt.Errorf("%w: nil agent", ErrInvalidRun)
	}
	if ctx == nil {
		return session.CompactResult{}, fmt.Errorf("%w: context is nil", ErrInvalidRun)
	}
	if a.config.compactor == nil || a.config.summarizer == nil {
		return session.CompactResult{}, ErrCompactionUnavailable
	}
	if cause := context.Cause(ctx); cause != nil {
		return session.CompactResult{}, fmt.Errorf("%w: context already cancelled: %w", ErrInvalidRun, cause)
	}
	a.mu.Lock()
	if a.active != nil || a.starting {
		a.mu.Unlock()
		return session.CompactResult{}, ErrBusy
	}
	if a.nextID == math.MaxUint64 {
		a.mu.Unlock()
		return session.CompactResult{}, ErrRunIDExhausted
	}
	a.nextID++
	runCtx, cancel := context.WithCancelCause(ctx)
	active := &activeRun{id: a.nextID, ctx: runCtx, cancel: cancel, done: make(chan struct{}), phase: PhaseCompacting, turn: 1}
	a.active = active
	a.mu.Unlock()
	defer a.finishRun(active)
	defer func() {
		a.enterSettling(active)
		var eventErr error
		if compactErr != nil {
			eventErr = safeCompactionEventError(compactErr)
		}
		a.notify(active.ctx, Event{Kind: EventRunSettled, RunID: active.id, Turn: 1, RunError: eventErr})
	}()
	a.notify(active.ctx, Event{Kind: EventRunStarted, RunID: active.id})
	a.notify(active.ctx, Event{
		Kind: EventCompactionStarted, RunID: active.id, Turn: 1,
		CompactionReason: CompactionManual, CompactionWillRetry: false,
	})
	result, compactErr = a.config.compactor.Compact(active.ctx, session.CompactRequest{
		KeepRecentTokens: a.config.keepRecentTokens,
		Instructions:     instructions,
		Summarizer:       a.observedSummarizer(active, 1, CompactionManual),
	})
	if compactErr != nil {
		eventErr := safeCompactionEventError(compactErr)
		a.notify(active.ctx, Event{
			Kind: EventCompactionSettled, RunID: active.id, Turn: 1, RunError: eventErr,
			CompactionReason: CompactionManual, CompactionWillRetry: false,
		})
		return session.CompactResult{}, compactErr
	}
	a.notify(active.ctx, Event{
		Kind: EventCompactionSettled, RunID: active.id, Turn: 1, Compaction: &result,
		CompactionReason: CompactionManual, CompactionWillRetry: false,
	})
	return result, nil
}

// Run accepts one text prompt while idle and synchronously settles its entire
// provider/tool/transcript/observer lifecycle before returning.
func (a *Agent) Run(ctx context.Context, prompt string) (result Result, runErr error) {
	active, user, err := a.beginRun(ctx, prompt)
	if err != nil {
		return Result{}, err
	}
	return a.runV2(active, []llm.ConversationMessage{user})
}

// RunAgentMessages starts one turn from the complete provider-neutral message
// batch. It is the core equivalent of pi's AgentMessage | AgentMessage[] prompt
// input and preserves custom/rich message order through commit and hooks.
func (a *Agent) RunAgentMessages(ctx context.Context, messages []agentmsg.Message) (Result, error) {
	initial := agentmsg.Clone(messages)
	active, err := a.beginAgentMessageRun(ctx, initial)
	if err != nil {
		return Result{}, err
	}
	return a.runV2WithAgentMessages(active, nil, initial)
}

func (a *Agent) beginAgentMessageRun(ctx context.Context, messages []agentmsg.Message) (*activeRun, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: nil agent", ErrInvalidRun)
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", ErrInvalidRun)
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, fmt.Errorf("%w: context already cancelled: %w", ErrInvalidRun, cause)
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("%w: empty agent message prompt", ErrInvalidRun)
	}
	for _, message := range messages {
		if message == nil {
			return nil, fmt.Errorf("%w: nil agent message prompt", ErrInvalidRun)
		}
		if _, partial := message.(agentmsg.AssistantPartial); partial {
			return nil, fmt.Errorf("%w: partial assistant prompt", ErrInvalidRun)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active != nil || a.starting {
		return nil, ErrBusy
	}
	if a.nextID == math.MaxUint64 {
		return nil, ErrRunIDExhausted
	}
	a.nextID++
	runContext, cancel := context.WithCancelCause(ctx)
	active := &activeRun{id: a.nextID, ctx: runContext, cancel: cancel, done: make(chan struct{}), phase: PhaseProvider, turn: 1}
	a.active = active
	return active, nil
}

func (a *Agent) runWithAgentMessages(ctx context.Context, prompt string, extra []agentmsg.Message) (Result, error) {
	active, user, err := a.beginRun(ctx, prompt)
	if err != nil {
		return Result{}, err
	}
	return a.runV2WithAgentMessages(active, []llm.ConversationMessage{user}, agentmsg.Clone(extra))
}

// RunContent accepts one rich user message while idle.  It deliberately uses
// the same admission and run seam as Run: the message is committed by runV2
// and therefore has the same event, provenance, and cancellation behaviour
// as a text prompt.
func (a *Agent) RunContent(ctx context.Context, content []llm.UserContentBlock) (Result, error) {
	active, user, err := a.beginRunContent(ctx, content)
	if err != nil {
		return Result{}, err
	}
	return a.runV2(active, []llm.ConversationMessage{user})
}

func (a *Agent) runContentWithAgentMessages(ctx context.Context, content []llm.UserContentBlock, extra []agentmsg.Message) (Result, error) {
	active, user, err := a.beginRunContent(ctx, content)
	if err != nil {
		return Result{}, err
	}
	return a.runV2WithAgentMessages(active, []llm.ConversationMessage{user}, agentmsg.Clone(extra))
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

func (a *Agent) beginRunContent(ctx context.Context, content []llm.UserContentBlock) (*activeRun, llm.UserContentMessage, error) {
	if a == nil {
		return nil, llm.UserContentMessage{}, fmt.Errorf("%w: nil agent", ErrInvalidRun)
	}
	if ctx == nil {
		return nil, llm.UserContentMessage{}, fmt.Errorf("%w: context is nil", ErrInvalidRun)
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, llm.UserContentMessage{}, fmt.Errorf("%w: context already cancelled: %w", ErrInvalidRun, cause)
	}

	a.mu.Lock()
	if a.active != nil || a.starting {
		a.mu.Unlock()
		return nil, llm.UserContentMessage{}, ErrBusy
	}
	if a.nextID == math.MaxUint64 {
		a.mu.Unlock()
		return nil, llm.UserContentMessage{}, ErrRunIDExhausted
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
		return nil, llm.UserContentMessage{}, fmt.Errorf("%w: prompt timestamp: %w", ErrInvalidRun, err)
	}
	user, err := llm.NewUserContentMessage(content, timestamp)
	if err != nil {
		return nil, llm.UserContentMessage{}, fmt.Errorf("%w: prompt content: %w", ErrInvalidRun, err)
	}

	a.mu.Lock()
	if !a.starting || a.active != nil {
		a.mu.Unlock()
		return nil, llm.UserContentMessage{}, fmt.Errorf("%w: run reservation was lost", ErrInvariant)
	}
	a.nextID++
	runContext, cancel := context.WithCancelCause(ctx)
	active := &activeRun{id: a.nextID, ctx: runContext, cancel: cancel, done: make(chan struct{}), phase: PhaseProvider, turn: 1}
	a.starting = false
	a.active = active
	reserved = false
	a.mu.Unlock()
	return active, user, nil
}

func (a *Agent) finishRun(active *activeRun) {
	a.mu.Lock()
	if a.active == active {
		a.releaseQueueReservationLocked(active)
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
		active.pendingToolCalls = nil
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

func (a *Agent) commit(active *activeRun, turn uint32, message llm.ConversationMessage) (llm.ConversationMessage, error) {
	return a.commitAfterAppend(active, turn, message, nil, nil)
}

func (a *Agent) commitAfterAppend(
	active *activeRun,
	turn uint32,
	message llm.ConversationMessage,
	beforeAppend func(llm.ConversationMessage) error,
	afterAppend func() error,
) (llm.ConversationMessage, error) {
	wrapped, err := agentmsg.NewLLM(message)
	if err != nil {
		return nil, err
	}
	final, err := a.applyMessageEnd(active.ctx, wrapped)
	if err != nil {
		return nil, err
	}
	converted, err := agentmsg.ConvertToLLM([]agentmsg.Message{final})
	if err != nil || len(converted) != 1 {
		return nil, fmt.Errorf("%w: invalid message_end replacement", ErrInvariant)
	}
	message = converted[0]
	if beforeAppend != nil {
		if err := beforeAppend(message); err != nil {
			return nil, err
		}
	}
	options := session.AppendOptions{}
	var eventModel provider.ModelRef
	if message.Role() == llm.RoleAssistant {
		snapshot, snapshotErr := a.activeSnapshot(active)
		if snapshotErr != nil {
			// A cancellation can be persisted before any provider attempt (for
			// example while durable queued input is settling). No turn snapshot
			// exists in that case; use the immutable loop defaults solely for
			// failure provenance rather than fabricating a request snapshot.
			if !errors.Is(snapshotErr, ErrInvariant) {
				return nil, snapshotErr
			}
			snapshot = TurnSnapshot{Model: a.config.model}
		}
		options.Assistant = session.AssistantProvenance{
			API:      snapshot.Model.API(),
			Provider: snapshot.Model.Provider(),
			Model:    snapshot.Model.ID(),
			Cost:     assistantSessionCost(message),
		}
		eventModel = snapshot.Model
	}
	settlementBase := context.WithoutCancel(active.ctx)
	settlement, cancel := context.WithTimeout(settlementBase, a.config.settlementTimeout)
	_, err = a.config.transcript.Append(settlement, message, options)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("%w: %s message: %w", ErrTranscriptCommit, message.Role(), err)
	}
	if afterAppend != nil {
		if err := afterAppend(); err != nil {
			return nil, err
		}
	}
	a.notify(active.ctx, Event{
		Kind:         EventMessageCommitted,
		RunID:        active.id,
		Turn:         turn,
		Message:      message,
		AgentMessage: agentmsg.CloneOne(final),
		Model:        eventModel,
	})
	return message, nil
}

func (a *Agent) applyMessageEnd(ctx context.Context, message agentmsg.Message) (agentmsg.Message, error) {
	if message == nil {
		return nil, fmt.Errorf("%w: nil message", ErrInvariant)
	}
	if a.config.messageEnd == nil {
		return agentmsg.CloneOne(message), nil
	}
	replacement, err := a.config.messageEnd(ctx, agentmsg.CloneOne(message))
	if err != nil {
		return nil, err
	}
	if replacement == nil {
		return agentmsg.CloneOne(message), nil
	}
	if replacement.Role() != message.Role() {
		return nil, fmt.Errorf("%w: message_end replacement changed role", ErrInvariant)
	}
	return agentmsg.CloneOne(replacement), nil
}

func (a *Agent) commitAgentMessage(active *activeRun, turn uint32, message agentmsg.Message) error {
	final, err := a.applyMessageEnd(active.ctx, message)
	if err != nil {
		return err
	}
	if standard, ok := final.(agentmsg.LLM); ok {
		return a.commitConversationAfterMessageEnd(active, turn, standard.Conversation(), final)
	}
	transcript, ok := a.config.transcript.(AgentMessageTranscript)
	if !ok {
		return fmt.Errorf("%w: transcript does not support AgentMessage persistence", ErrTranscriptCommit)
	}
	settlementBase := context.WithoutCancel(active.ctx)
	settlement, cancel := context.WithTimeout(settlementBase, a.config.settlementTimeout)
	_, appendErr := transcript.AppendAgentMessage(settlement, final, session.AppendOptions{})
	cancel()
	if appendErr != nil {
		return fmt.Errorf("%w: %s message: %w", ErrTranscriptCommit, final.Role(), appendErr)
	}
	converted, convertErr := agentmsg.ConvertToLLM([]agentmsg.Message{final})
	if convertErr != nil {
		return fmt.Errorf("%w: project committed AgentMessage: %w", ErrInvariant, convertErr)
	}
	var projected llm.ConversationMessage
	if len(converted) == 1 {
		projected = converted[0]
	} else if len(converted) > 1 {
		return fmt.Errorf("%w: one AgentMessage projected to multiple LLM messages", ErrInvariant)
	}
	a.notify(active.ctx, Event{Kind: EventMessageCommitted, RunID: active.id, Turn: turn, Message: projected, AgentMessage: agentmsg.CloneOne(final)})
	return nil
}

func (a *Agent) commitConversationAfterMessageEnd(active *activeRun, turn uint32, message llm.ConversationMessage, final agentmsg.Message) error {
	options := session.AppendOptions{}
	var eventModel provider.ModelRef
	if message.Role() == llm.RoleAssistant {
		snapshot, snapshotErr := a.activeSnapshot(active)
		if snapshotErr != nil {
			if !errors.Is(snapshotErr, ErrInvariant) {
				return snapshotErr
			}
			snapshot = TurnSnapshot{Model: a.config.model}
		}
		options.Assistant = session.AssistantProvenance{API: snapshot.Model.API(), Provider: snapshot.Model.Provider(), Model: snapshot.Model.ID(), Cost: assistantSessionCost(message)}
		eventModel = snapshot.Model
	}
	settlementBase := context.WithoutCancel(active.ctx)
	settlement, cancel := context.WithTimeout(settlementBase, a.config.settlementTimeout)
	_, err := a.config.transcript.Append(settlement, message, options)
	cancel()
	if err != nil {
		return fmt.Errorf("%w: %s message: %w", ErrTranscriptCommit, message.Role(), err)
	}
	a.notify(active.ctx, Event{Kind: EventMessageCommitted, RunID: active.id, Turn: turn, Message: message, AgentMessage: agentmsg.CloneOne(final), Model: eventModel})
	return nil
}

func assistantSessionCost(message llm.ConversationMessage) session.UsageCost {
	terminal, ok := message.(llm.AssistantTerminal)
	if !ok {
		return session.ZeroUsageCost()
	}
	cost, known := terminal.Usage().Cost()
	if !known {
		return session.ZeroUsageCost()
	}
	return session.UsageCostFromLLM(cost)
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
	// Reuse the timestamp of the context message that caused this provider
	// turn. This keeps partial events deterministic and avoids an extra call to
	// the Agent clock (busy/admission semantics guarantee one clock read per
	// newly created durable message).
	var partialTimestamp time.Time
	if messages := a.config.transcript.Context().Messages(); len(messages) != 0 {
		partialTimestamp = messages[len(messages)-1].Timestamp()
	}
	partialModel := request.Model()
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
		partial, partialErr := agentmsg.NewAssistantPartial(agentmsg.AssistantPartialSpec{
			Snapshot: snapshot,
			Event:    event,
			API:      partialModel.API(),
			Provider: partialModel.Provider(),
			Model:    partialModel.ID(),
			At:       partialTimestamp,
		})
		if partialErr != nil {
			return nil, fmt.Errorf("%w: partial message: %w", ErrProviderStream, partialErr)
		}
		a.notify(active.ctx, Event{
			Kind:             EventProviderProgress,
			RunID:            active.id,
			Turn:             turn,
			ProviderSnapshot: snapshot,
			ProviderEvent:    event,
			AgentMessage:     partial,
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

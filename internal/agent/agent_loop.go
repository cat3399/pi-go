package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

// AgentLoopContext is the complete in-memory state visible to one low-level
// run. AgentLoop owns a private copy and never persists it.
type AgentLoopContext struct {
	SystemPrompt string
	Messages     []agentmsg.Message
	Tools        []AgentLoopTool
}

// AgentLoopTool is the executable tool boundary owned by the low-level loop.
// Definition supplies the provider schema; Execute receives prepared,
// validated arguments rather than raw JSON.
type AgentLoopTool interface {
	Definition() provider.ToolDefinition
	Execute(context.Context, string, any, func(ToolUpdate)) (ToolOutput, error)
}

type AgentLoopToolArgumentPreparer interface {
	PrepareArguments(any) (any, error)
}

type AgentLoopToolExecutionOverride interface {
	ExecutionMode() ToolExecutionMode
}

type AgentLoopBeforeToolCallContext struct {
	Assistant llm.AssistantToolUseMessage
	ToolCall  llm.ToolCallBlock
	Arguments any
	Context   AgentLoopContext
}

type AgentLoopBeforeToolCallResult struct {
	Block     bool
	Reason    string
	Arguments *any
}

type AgentLoopAfterToolCallContext struct {
	Assistant llm.AssistantToolUseMessage
	ToolCall  llm.ToolCallBlock
	Arguments any
	Context   AgentLoopContext
	Result    ToolOutput
	IsError   bool
}

type AgentLoopBeforeToolCallHook func(context.Context, AgentLoopBeforeToolCallContext) (AgentLoopBeforeToolCallResult, error)

type AgentLoopAfterToolCallResult struct {
	Content   *[]llm.ToolResultContentBlock
	Details   *any
	IsError   *bool
	Usage     *llm.Usage
	Terminate *bool
}

type AgentLoopAfterToolCallHook func(context.Context, AgentLoopAfterToolCallContext) (AgentLoopAfterToolCallResult, error)

// AgentLoopTurnContext is supplied after turn_end. Every field is an immutable
// snapshot; callbacks cannot mutate the loop by retaining or editing slices.
type AgentLoopTurnContext struct {
	Message     llm.AssistantTerminal
	ToolResults []agentmsg.Message
	Context     AgentLoopContext
	NewMessages []agentmsg.Message
}

// AgentLoopTurnUpdate replaces selected runtime values before another provider
// request. A nil field preserves the current value.
type AgentLoopTurnUpdate struct {
	Context       *AgentLoopContext
	Model         *provider.Model
	ThinkingLevel *provider.ThinkingLevel
	Stream        *provider.StreamOptions
}

type AgentLoopEventSink func(context.Context, AgentEvent) error
type AgentLoopMessageSource func(context.Context) ([]agentmsg.Message, error)
type agentLoopProviderTurnContext struct {
	Turn    uint32
	Context AgentLoopContext
}
type agentLoopPrepareProviderTurn func(context.Context, agentLoopProviderTurnContext) (*AgentLoopTurnUpdate, error)
type AgentLoopPrepareNextTurn func(context.Context, AgentLoopTurnContext) (*AgentLoopTurnUpdate, error)
type AgentLoopShouldStopAfterTurn func(context.Context, AgentLoopTurnContext) (bool, error)
type AgentLoopConvertToLLM func(context.Context, []agentmsg.Message) ([]llm.ConversationMessage, error)
type AgentLoopTransformContext func(context.Context, []agentmsg.Message) ([]agentmsg.Message, error)
type AgentLoopProcessMessage func(context.Context, agentmsg.Message) (agentmsg.Message, error)
type AgentLoopAPIKey func(context.Context, string) (string, error)

// AgentLoopConfig contains only provider/tool/callback policy for one run. It
// deliberately has no Transcript, Session, settings, app, or storage port.
type AgentLoopConfig struct {
	RunID               uint64
	Provider            provider.Provider
	Model               provider.Model
	ThinkingLevel       provider.ThinkingLevel
	Stream              provider.StreamOptions
	ToolExecution       ToolExecutionMode
	BeforeToolCall      AgentLoopBeforeToolCallHook
	AfterToolCall       AgentLoopAfterToolCallHook
	ConvertToLLM        AgentLoopConvertToLLM
	TransformContext    AgentLoopTransformContext
	ProcessMessage      AgentLoopProcessMessage
	GetAPIKey           AgentLoopAPIKey
	GetSteeringMessages AgentLoopMessageSource
	GetFollowUpMessages AgentLoopMessageSource
	// prepareProviderTurn refreshes request-scoped runtime values after this
	// turn's turn_start and queued user delivery, immediately before invoking
	// the provider. It is distinct from the upstream after-turn
	// PrepareNextTurn contract below.
	prepareProviderTurn agentLoopPrepareProviderTurn
	PrepareNextTurn     AgentLoopPrepareNextTurn
	ShouldStopAfterTurn AgentLoopShouldStopAfterTurn
	Emit                AgentLoopEventSink
	Now                 func() time.Time
}

type AgentLoopResult struct {
	Messages       []agentmsg.Message
	Context        AgentLoopContext
	Terminal       llm.AssistantTerminal
	ProviderTurns  uint32
	ToolExecutions uint32
}

// AgentLoop executes one prompt/continuation run entirely in memory.
type AgentLoop struct {
	config AgentLoopConfig
	nowMu  sync.Mutex
}

type agentLoopEventDispatcherKey struct{}

type agentLoopEventJob struct {
	ctx   context.Context
	event AgentEvent
	done  chan error
}

type agentLoopEventDispatcher struct {
	sink    AgentLoopEventSink
	mu      sync.Mutex
	queue   []agentLoopEventJob
	running bool
}

func newAgentLoopEventDispatcher(sink AgentLoopEventSink) *agentLoopEventDispatcher {
	return &agentLoopEventDispatcher{sink: sink}
}

func (d *agentLoopEventDispatcher) enqueue(ctx context.Context, event AgentEvent) <-chan error {
	done := make(chan error, 1)
	d.mu.Lock()
	d.queue = append(d.queue, agentLoopEventJob{ctx: ctx, event: event, done: done})
	if !d.running {
		d.running = true
		go d.drain()
	}
	d.mu.Unlock()
	return done
}

func (d *agentLoopEventDispatcher) drain() {
	for {
		d.mu.Lock()
		if len(d.queue) == 0 {
			d.running = false
			d.mu.Unlock()
			return
		}
		job := d.queue[0]
		d.queue[0] = agentLoopEventJob{}
		d.queue = d.queue[1:]
		d.mu.Unlock()

		job.done <- callAgentLoopEventSink(d.sink, job.ctx, job.event)
		close(job.done)
	}
}

func callAgentLoopEventSink(sink AgentLoopEventSink, ctx context.Context, event AgentEvent) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("agent loop event sink panicked: %s", safeValueText(recovered))
		}
	}()
	return sink(ctx, event)
}

type agentLoopInvocation struct {
	model         provider.Model
	thinkingLevel provider.ThinkingLevel
	stream        provider.StreamOptions
}

func NewAgentLoop(config AgentLoopConfig) (*AgentLoop, error) {
	if isNilInterface(config.Provider) {
		return nil, fmt.Errorf("%w: provider is required", ErrInvalidConfig)
	}
	if config.ToolExecution == 0 {
		config.ToolExecution = ToolExecutionParallel
	}
	if config.ToolExecution != ToolExecutionParallel && config.ToolExecution != ToolExecutionSequential {
		return nil, fmt.Errorf("%w: invalid tool execution mode", ErrInvalidConfig)
	}
	if config.ConvertToLLM == nil {
		config.ConvertToLLM = func(_ context.Context, messages []agentmsg.Message) ([]llm.ConversationMessage, error) {
			return agentmsg.ConvertToLLM(messages)
		}
	}
	if config.Emit == nil {
		config.Emit = func(context.Context, AgentEvent) error { return nil }
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if _, err := provider.NewRequestWithOptions(config.Model, "", nil, provider.RequestOptions{ThinkingLevel: config.ThinkingLevel}); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	config.Stream = provider.CloneStreamOptions(config.Stream)
	return &AgentLoop{config: config}, nil
}

func (l *AgentLoop) Run(ctx context.Context, prompts []agentmsg.Message, initial AgentLoopContext) (AgentLoopResult, error) {
	if l == nil || ctx == nil {
		return AgentLoopResult{}, fmt.Errorf("%w: invalid AgentLoop run", ErrInvalidRun)
	}
	for _, message := range prompts {
		if isNilInterface(message) {
			return AgentLoopResult{}, fmt.Errorf("%w: nil prompt message", ErrInvalidRun)
		}
		if isAssistantPartialMessage(message) {
			return AgentLoopResult{}, fmt.Errorf("%w: partial assistant prompt", ErrInvalidRun)
		}
	}
	current := cloneAgentLoopContext(initial)
	ctx = context.WithValue(ctx, agentLoopEventDispatcherKey{}, newAgentLoopEventDispatcher(l.config.Emit))
	invocation := agentLoopInvocation{model: l.config.Model, thinkingLevel: l.config.ThinkingLevel, stream: provider.CloneStreamOptions(l.config.Stream)}
	return l.run(ctx, &invocation, current, agentmsg.Clone(prompts), true)
}

func (l *AgentLoop) Continue(ctx context.Context, initial AgentLoopContext) (AgentLoopResult, error) {
	if l == nil || ctx == nil || len(initial.Messages) == 0 {
		return AgentLoopResult{}, fmt.Errorf("%w: no messages in context", ErrCannotContinue)
	}
	if initial.Messages[len(initial.Messages)-1].Role() == agentmsg.RoleAssistant {
		return AgentLoopResult{}, fmt.Errorf("%w: assistant tail", ErrCannotContinue)
	}
	ctx = context.WithValue(ctx, agentLoopEventDispatcherKey{}, newAgentLoopEventDispatcher(l.config.Emit))
	invocation := agentLoopInvocation{model: l.config.Model, thinkingLevel: l.config.ThinkingLevel, stream: provider.CloneStreamOptions(l.config.Stream)}
	return l.run(ctx, &invocation, cloneAgentLoopContext(initial), nil, false)
}

func (l *AgentLoop) run(ctx context.Context, invocation *agentLoopInvocation, current AgentLoopContext, newMessages []agentmsg.Message, emitPrompts bool) (AgentLoopResult, error) {
	result := AgentLoopResult{}
	turn := uint32(1)
	if err := l.emit(ctx, AgentStartEvent{RunID: l.config.RunID}); err != nil {
		return result, err
	}
	if err := l.emit(ctx, TurnStartEvent{RunID: l.config.RunID, Turn: turn}); err != nil {
		return result, err
	}
	if emitPrompts {
		for index, message := range newMessages {
			processed, err := l.emitMessage(ctx, turn, message)
			if err != nil {
				return result, err
			}
			current.Messages = append(current.Messages, agentmsg.CloneOne(processed))
			newMessages[index] = agentmsg.CloneOne(processed)
		}
	}
	pending, err := l.messagesFrom(ctx, l.config.GetSteeringMessages)
	if err != nil {
		return result, err
	}
	firstTurn := true
	for {
		hasMoreToolCalls := true
		for hasMoreToolCalls || len(pending) != 0 {
			if !firstTurn {
				turn++
				if err := l.emit(ctx, TurnStartEvent{RunID: l.config.RunID, Turn: turn}); err != nil {
					return result, err
				}
			} else {
				firstTurn = false
			}
			for _, message := range pending {
				processed, err := l.emitMessage(ctx, turn, message)
				if err != nil {
					return result, err
				}
				current.Messages = append(current.Messages, agentmsg.CloneOne(processed))
				newMessages = append(newMessages, agentmsg.CloneOne(processed))
			}
			pending = nil
			if l.config.prepareProviderTurn != nil {
				// Turn-scoped configuration is resolved at the same boundary as the
				// provider request it controls. In particular, a slow credential
				// refresh cannot fail between turns before turn_start or strand
				// already-drained queued user messages outside a complete turn.
				// Deliberately continue through this boundary after cancellation.
				// Pi performs one final stream call with the aborted signal after an
				// interrupted tool batch; that call produces the provider's canonical
				// aborted assistant message and completes the turn transcript.
				prepareContext := agentLoopProviderTurnContext{Turn: turn, Context: cloneAgentLoopContext(current)}
				update, prepareErr := l.config.prepareProviderTurn(ctx, prepareContext)
				if prepareErr != nil {
					return result, prepareErr
				}
				if update != nil {
					if update.Context != nil {
						current = cloneAgentLoopContext(*update.Context)
					}
					if update.Model != nil {
						invocation.model = *update.Model
					}
					if update.ThinkingLevel != nil {
						invocation.thinkingLevel = *update.ThinkingLevel
					}
					if update.Stream != nil {
						invocation.stream = provider.CloneStreamOptions(*update.Stream)
					}
				}
			}
			terminal, err := l.streamAssistant(ctx, invocation, turn, &current)
			if err != nil {
				return result, err
			}
			result.ProviderTurns++
			result.Terminal = terminal
			wrapped, err := agentmsg.NewLLM(terminal)
			if err != nil {
				return result, err
			}
			newMessages = append(newMessages, wrapped)
			if terminal.FinishReason() == llm.FinishError || terminal.FinishReason() == llm.FinishAborted {
				if err := l.emit(ctx, TurnEndEvent{RunID: l.config.RunID, Turn: turn, Message: wrapped}); err != nil {
					return result, err
				}
				return l.finish(ctx, turn, current, newMessages, terminal, result)
			}

			var toolResults []agentmsg.Message
			hasMoreToolCalls = false
			if toolMessage, ok := terminal.(llm.AssistantToolUseMessage); ok {
				batch, batchErr := l.executeToolBatch(ctx, turn, current, toolMessage)
				if batchErr != nil {
					return result, batchErr
				}
				result.ToolExecutions += batch.executions
				toolResults = batch.messages
				hasMoreToolCalls = !batch.terminate
				for _, message := range toolResults {
					current.Messages = append(current.Messages, agentmsg.CloneOne(message))
					newMessages = append(newMessages, agentmsg.CloneOne(message))
				}
			}
			if err := l.emit(ctx, TurnEndEvent{RunID: l.config.RunID, Turn: turn, Message: wrapped, ToolResults: agentmsg.Clone(toolResults)}); err != nil {
				return result, err
			}

			turnContext := cloneAgentLoopTurnContext(terminal, toolResults, current, newMessages)
			if l.config.PrepareNextTurn != nil {
				update, prepareErr := l.config.PrepareNextTurn(ctx, turnContext)
				if prepareErr != nil {
					return result, prepareErr
				}
				if update != nil {
					if update.Context != nil {
						current = cloneAgentLoopContext(*update.Context)
					}
					if update.Model != nil {
						invocation.model = *update.Model
					}
					if update.ThinkingLevel != nil {
						invocation.thinkingLevel = *update.ThinkingLevel
					}
					if update.Stream != nil {
						invocation.stream = provider.CloneStreamOptions(*update.Stream)
					}
				}
			}
			if l.config.ShouldStopAfterTurn != nil {
				stopContext := cloneAgentLoopTurnContext(terminal, toolResults, current, newMessages)
				stop, stopErr := l.config.ShouldStopAfterTurn(ctx, stopContext)
				if stopErr != nil {
					return result, stopErr
				}
				if stop {
					return l.finish(ctx, turn, current, newMessages, terminal, result)
				}
			}
			pending, err = l.messagesFrom(ctx, l.config.GetSteeringMessages)
			if err != nil {
				return result, err
			}
		}
		pending, err = l.messagesFrom(ctx, l.config.GetFollowUpMessages)
		if err != nil {
			return result, err
		}
		if len(pending) == 0 {
			return l.finish(ctx, turn, current, newMessages, result.Terminal, result)
		}
	}
}

func (l *AgentLoop) finish(ctx context.Context, turn uint32, current AgentLoopContext, messages []agentmsg.Message, terminal llm.AssistantTerminal, result AgentLoopResult) (AgentLoopResult, error) {
	result.Messages = agentmsg.Clone(messages)
	result.Context = cloneAgentLoopContext(current)
	result.Terminal = terminal
	if err := l.emit(ctx, AgentEndEvent{RunID: l.config.RunID, Turn: turn, Messages: result.Messages, Terminal: terminal}); err != nil {
		return AgentLoopResult{}, err
	}
	return result, nil
}

func (l *AgentLoop) streamAssistant(ctx context.Context, invocation *agentLoopInvocation, turn uint32, current *AgentLoopContext) (llm.AssistantTerminal, error) {
	messages := agentmsg.Clone(current.Messages)
	var err error
	if l.config.TransformContext != nil {
		messages, err = l.config.TransformContext(ctx, messages)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrContextTransform, err)
		}
		messages = agentmsg.Clone(messages)
	}
	converted, err := l.config.ConvertToLLM(ctx, messages)
	if err != nil {
		return nil, fmt.Errorf("%w: convert to LLM: %w", ErrContextTransform, err)
	}
	streamOptions := provider.CloneStreamOptions(invocation.stream)
	if l.config.GetAPIKey != nil {
		apiKey, keyErr := l.config.GetAPIKey(ctx, invocation.model.Provider())
		if keyErr != nil {
			return nil, keyErr
		}
		if apiKey != "" {
			streamOptions.APIKey = apiKey
		}
	}
	definitions := agentLoopToolDefinitions(current.Tools)
	request, err := provider.NewRequestWithOptions(invocation.model, current.SystemPrompt, converted, provider.RequestOptions{
		Tools: definitions, AllowParallelToolCalls: effectiveAgentLoopToolExecutionMode(l.config.ToolExecution, current.Tools) == ToolExecutionParallel,
		ThinkingLevel: invocation.thinkingLevel, Stream: streamOptions,
	})
	if err != nil {
		// A sequential cancellation deliberately leaves later calls unstarted and
		// therefore without synthetic results. The strict Go request constructor
		// cannot represent that incomplete tool batch, so settle the cancellation
		// locally; pre-cancelled valid contexts still reach Provider.Stream below.
		if cause := context.Cause(ctx); cause != nil {
			return l.emitProviderFailure(ctx, turn, current, invocation.model, "Operation aborted", errors.Join(err, cause), nil, false)
		}
		return nil, err
	}
	stream := l.config.Provider.Stream(ctx, request)
	if isNilInterface(stream) {
		return l.emitProviderFailure(ctx, turn, current, request.Model(), "Provider returned a nil stream", ErrProviderStream, nil, false)
	}
	closed := false
	defer func() {
		if !closed {
			_ = stream.Close()
		}
	}()
	collector := &llm.StreamCollector{}
	started := false
	for {
		event, nextErr := stream.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			blocks, failureErr := agentLoopCollectorFailure(collector, nextErr)
			return l.emitProviderFailure(ctx, turn, current, request.Model(), "Provider stream failed", failureErr, blocks, started)
		}
		if !started {
			switch event.(type) {
			case llm.StartEvent, llm.DoneEvent, llm.ErrorEvent:
			default:
				continue
			}
		}
		if err := collector.Accept(event); err != nil {
			blocks, failureErr := agentLoopCollectorFailure(collector, err)
			return l.emitProviderFailure(ctx, turn, current, request.Model(), "Provider stream failed", failureErr, blocks, started)
		}
		if _, terminal := event.(llm.DoneEvent); terminal {
			continue
		}
		if _, terminal := event.(llm.ErrorEvent); terminal {
			continue
		}
		snapshot, err := collector.Snapshot()
		if err != nil {
			return nil, err
		}
		partial, err := agentmsg.NewAssistantPartial(agentmsg.AssistantPartialSpec{Snapshot: snapshot, Event: event})
		if err != nil {
			return nil, err
		}
		if _, start := event.(llm.StartEvent); start {
			started = true
			if err := l.emit(ctx, MessageStartEvent{RunID: l.config.RunID, Turn: turn, Message: partial}); err != nil {
				return nil, err
			}
		} else if err := l.emit(ctx, MessageUpdateEvent{RunID: l.config.RunID, Turn: turn, Message: partial, AssistantMessageEvent: newAssistantMessageEvent(event, partial)}); err != nil {
			return nil, err
		}
	}
	closed = true
	if err := stream.Close(); err != nil {
		blocks, failureErr := agentLoopCollectorFailure(collector, err)
		return l.emitProviderFailure(ctx, turn, current, request.Model(), "Provider stream failed", failureErr, blocks, started)
	}
	if err := collector.Close(); err != nil {
		blocks, failureErr := agentLoopCollectorFailure(collector, err)
		return l.emitProviderFailure(ctx, turn, current, request.Model(), "Provider stream failed", failureErr, blocks, started)
	}
	terminal, err := collector.Result()
	if err != nil {
		blocks, failureErr := agentLoopCollectorFailure(collector, err)
		return l.emitProviderFailure(ctx, turn, current, request.Model(), "Provider stream failed", failureErr, blocks, started)
	}
	provenance := terminal.AssistantProvenance()
	if !provenance.Matches(request.Model().Provider(), request.Model().API(), request.Model().ID()) {
		blocks, failureErr := agentLoopCollectorFailure(collector, fmt.Errorf("%w: terminal provenance does not match request model", ErrProviderStream))
		return l.emitProviderFailure(ctx, turn, current, request.Model(), "Provider stream failed", failureErr, blocks, started)
	}
	wrapped, err := agentmsg.NewLLM(terminal)
	if err != nil {
		return nil, err
	}
	if !started {
		if err := l.emit(ctx, MessageStartEvent{RunID: l.config.RunID, Turn: turn, Message: wrapped}); err != nil {
			return nil, err
		}
	}
	processed, err := l.processMessage(ctx, wrapped)
	if err != nil {
		return nil, err
	}
	standard, ok := processed.(agentmsg.LLM)
	if !ok {
		return nil, fmt.Errorf("%w: processed assistant terminal is %T", ErrInvariant, processed)
	}
	processedTerminal, ok := standard.Conversation().(llm.AssistantTerminal)
	if !ok {
		return nil, fmt.Errorf("%w: processed assistant message is not terminal", ErrInvariant)
	}
	current.Messages = append(current.Messages, processed)
	if err := l.emit(ctx, MessageEndEvent{RunID: l.config.RunID, Turn: turn, Message: processed, Model: request.Model()}); err != nil {
		return nil, err
	}
	return processedTerminal, nil
}

func (l *AgentLoop) emitProviderFailure(ctx context.Context, turn uint32, current *AgentLoopContext, model provider.Model, text string, cause error, blocks []llm.AssistantBlock, started bool) (llm.AssistantTerminal, error) {
	reason := llm.FinishError
	if context.Cause(ctx) != nil {
		reason = llm.FinishAborted
		cause = errors.Join(cause, context.Cause(ctx))
	}
	failure, err := llm.NewFailure(text, cause)
	if err != nil {
		return nil, err
	}
	terminal, err := llm.NewAssistantFailureMessageWithBlocksAndMetadata(blocks, reason, failure, llm.Usage{}, l.now(), llm.AssistantProvenance{Provider: model.Provider(), API: model.API(), Model: model.ID()}, nil, nil)
	if err != nil {
		return nil, err
	}
	wrapped, err := agentmsg.NewLLM(terminal)
	if err != nil {
		return nil, err
	}
	if !started {
		if err := l.emit(ctx, MessageStartEvent{RunID: l.config.RunID, Turn: turn, Message: wrapped}); err != nil {
			return nil, err
		}
	}
	processed, err := l.processMessage(ctx, wrapped)
	if err != nil {
		return nil, err
	}
	standard, ok := processed.(agentmsg.LLM)
	if !ok {
		return nil, fmt.Errorf("%w: processed assistant terminal is %T", ErrInvariant, processed)
	}
	processedTerminal, ok := standard.Conversation().(llm.AssistantTerminal)
	if !ok {
		return nil, fmt.Errorf("%w: processed assistant message is not terminal", ErrInvariant)
	}
	current.Messages = append(current.Messages, processed)
	if err := l.emit(ctx, MessageEndEvent{RunID: l.config.RunID, Turn: turn, Message: processed, Model: model}); err != nil {
		return nil, err
	}
	return processedTerminal, nil
}

func agentLoopCollectorFailure(collector *llm.StreamCollector, cause error) ([]llm.AssistantBlock, error) {
	blocks, err := collector.FailureBlocks()
	if err != nil {
		return nil, errors.Join(cause, err)
	}
	return blocks, cause
}

type loopToolBatch struct {
	messages   []agentmsg.Message
	terminate  bool
	executions uint32
}

type loopToolOutcome struct {
	call     llm.ToolCallBlock
	output   ToolOutput
	err      error
	executed bool
}

type loopPreparedToolCall struct {
	call      llm.ToolCallBlock
	tool      AgentLoopTool
	arguments any
}

func (l *AgentLoop) executeToolBatch(ctx context.Context, turn uint32, current AgentLoopContext, assistant llm.AssistantToolUseMessage) (loopToolBatch, error) {
	calls := toolCalls(assistant)
	if assistant.FinishReason() == llm.FinishLength {
		batch := loopToolBatch{}
		for _, call := range calls {
			if err := l.emitToolStart(ctx, turn, call); err != nil {
				return loopToolBatch{}, err
			}
			output, callErr := truncatedToolCallOutcome(call)
			outcome := loopToolOutcome{call: call, output: output, err: callErr}
			if err := l.emitToolEnd(ctx, turn, outcome); err != nil {
				return loopToolBatch{}, err
			}
			message, err := l.toolResultMessage(outcome)
			if err != nil {
				return loopToolBatch{}, err
			}
			processed, err := l.emitMessage(ctx, turn, message)
			if err != nil {
				return loopToolBatch{}, err
			}
			batch.messages = append(batch.messages, processed)
		}
		return batch, nil
	}
	sequential := effectiveAgentLoopToolExecutionModeForCalls(l.config.ToolExecution, current.Tools, calls) == ToolExecutionSequential
	if sequential {
		outcomes := make([]loopToolOutcome, 0, len(calls))
		batch := loopToolBatch{}
		for _, call := range calls {
			if err := l.emitToolStart(ctx, turn, call); err != nil {
				return loopToolBatch{}, err
			}
			prepared, immediate := l.preflightLoopTool(ctx, current, assistant, call)
			var outcome loopToolOutcome
			if immediate != nil {
				outcome = *immediate
			} else {
				var executeErr error
				outcome, executeErr = l.executeLoopTool(ctx, turn, current, assistant, prepared)
				if executeErr != nil {
					return loopToolBatch{}, executeErr
				}
			}
			if outcome.executed {
				batch.executions++
			}
			if err := l.emitToolEnd(ctx, turn, outcome); err != nil {
				return loopToolBatch{}, err
			}
			outcomes = append(outcomes, outcome)
			message, err := l.toolResultMessage(outcome)
			if err != nil {
				return loopToolBatch{}, err
			}
			processed, err := l.emitMessage(ctx, turn, message)
			if err != nil {
				return loopToolBatch{}, err
			}
			batch.messages = append(batch.messages, processed)
			if context.Cause(ctx) != nil {
				break
			}
		}
		batch.terminate = allLoopToolOutcomesTerminate(outcomes)
		return batch, nil
	}

	outcomes := make([]loopToolOutcome, len(calls))
	scanned := 0
	prepared := make([]struct {
		index    int
		prepared loopPreparedToolCall
	}, 0, len(calls))
	for index, call := range calls {
		if err := l.emitToolStart(ctx, turn, call); err != nil {
			return loopToolBatch{}, err
		}
		preparedCall, immediate := l.preflightLoopTool(ctx, current, assistant, call)
		scanned = index + 1
		if immediate != nil {
			outcomes[index] = *immediate
			if err := l.emitToolEnd(ctx, turn, *immediate); err != nil {
				return loopToolBatch{}, err
			}
			if context.Cause(ctx) != nil {
				break
			}
			continue
		}
		prepared = append(prepared, struct {
			index    int
			prepared loopPreparedToolCall
		}{index, preparedCall})
		if context.Cause(ctx) != nil {
			break
		}
	}
	outcomes = outcomes[:scanned]
	type completed struct {
		index   int
		outcome loopToolOutcome
		err     error
	}
	done := make(chan completed, len(prepared))
	for _, item := range prepared {
		go func(index int, prepared loopPreparedToolCall) {
			outcome, err := l.executeLoopTool(ctx, turn, current, assistant, prepared)
			done <- completed{index: index, outcome: outcome, err: err}
		}(item.index, item.prepared)
	}
	var batchErr error
	for range prepared {
		completed := <-done
		if completed.err != nil {
			if batchErr == nil {
				batchErr = completed.err
			}
			continue
		}
		outcomes[completed.index] = completed.outcome
		if batchErr == nil {
			if err := l.emitToolEnd(ctx, turn, completed.outcome); err != nil {
				batchErr = err
			}
		}
	}
	if batchErr != nil {
		return loopToolBatch{}, batchErr
	}
	var executions uint32
	for _, outcome := range outcomes {
		if outcome.executed {
			executions++
		}
	}
	return l.toolResultMessages(ctx, turn, outcomes, executions)
}

func (l *AgentLoop) preflightLoopTool(ctx context.Context, current AgentLoopContext, assistant llm.AssistantToolUseMessage, call llm.ToolCallBlock) (loopPreparedToolCall, *loopToolOutcome) {
	tool := findAgentLoopTool(current.Tools, call.Name())
	if tool == nil {
		toolErr := fmt.Errorf("%w: %s", ErrToolNotFound, call.Name())
		outcome := syntheticLoopToolError(call, fmt.Sprintf("Tool %s not found", call.Name()), toolErr)
		return loopPreparedToolCall{}, &outcome
	}
	arguments, err := decodeAgentLoopToolArguments(call.ArgumentsJSON())
	if err == nil {
		if preparer, ok := tool.(AgentLoopToolArgumentPreparer); ok {
			arguments, err = prepareAgentLoopToolArgumentsSafely(preparer, arguments)
		}
	}
	if err == nil {
		arguments, err = validateAndCoerceAgentLoopArguments(tool.Definition(), arguments)
	}
	if err != nil {
		outcome := syntheticLoopToolError(call, err.Error(), err)
		return loopPreparedToolCall{}, &outcome
	}
	if l.config.BeforeToolCall != nil {
		before, hookErr := callAgentLoopBeforeToolHook(l.config.BeforeToolCall, ctx, AgentLoopBeforeToolCallContext{Assistant: assistant, ToolCall: call, Arguments: arguments, Context: cloneAgentLoopContext(current)})
		if hookErr != nil {
			outcome := syntheticLoopToolError(call, hookErr.Error(), hookErr)
			return loopPreparedToolCall{}, &outcome
		}
		if context.Cause(ctx) != nil {
			outcome := syntheticLoopToolError(call, "Operation aborted", ErrAgentAborted)
			return loopPreparedToolCall{}, &outcome
		}
		if before.Block {
			reason := before.Reason
			if reason == "" {
				reason = "Tool execution was blocked"
			}
			outcome := syntheticLoopToolError(call, reason, errors.New(reason))
			return loopPreparedToolCall{}, &outcome
		}
		if before.Arguments != nil {
			arguments = *before.Arguments
		}
	}
	if context.Cause(ctx) != nil {
		outcome := syntheticLoopToolError(call, "Operation aborted", ErrAgentAborted)
		return loopPreparedToolCall{}, &outcome
	}
	return loopPreparedToolCall{call: call, tool: tool, arguments: arguments}, nil
}

func (l *AgentLoop) executeLoopTool(ctx context.Context, turn uint32, current AgentLoopContext, assistant llm.AssistantToolUseMessage, prepared loopPreparedToolCall) (loopToolOutcome, error) {
	call := prepared.call
	var updateMu sync.Mutex
	accepting := true
	var updateSettlements []<-chan error
	report := func(update ToolUpdate) {
		updateMu.Lock()
		defer updateMu.Unlock()
		if !accepting || !validToolUpdate(update) {
			return
		}
		settlement := l.emitAsync(ctx, ToolExecutionUpdateEvent{RunID: l.config.RunID, Turn: turn, ToolCallID: call.ID(), ToolName: call.Name(), Arguments: call.ArgumentsJSON(), PartialResult: cloneToolUpdate(update)})
		updateSettlements = append(updateSettlements, settlement)
	}
	output, executionErr := executeAgentLoopToolSafely(prepared.tool, ctx, call.ID(), prepared.arguments, report)
	updateMu.Lock()
	accepting = false
	settlements := append([]<-chan error(nil), updateSettlements...)
	updateMu.Unlock()
	var settledUpdateErr error
	for _, settlement := range settlements {
		if err := <-settlement; err != nil && settledUpdateErr == nil {
			settledUpdateErr = err
		}
	}
	if settledUpdateErr != nil {
		return loopToolOutcome{}, settledUpdateErr
	}
	if executionErr != nil {
		output = syntheticLoopToolError(call, safeErrorText(executionErr), executionErr).output
	} else {
		output, executionErr = normalizeToolOutcome(output, nil)
		output = ownToolOutput(output)
	}
	if l.config.AfterToolCall != nil {
		after, afterErr := callAgentLoopAfterToolHook(l.config.AfterToolCall, ctx, AgentLoopAfterToolCallContext{Assistant: assistant, ToolCall: call, Arguments: prepared.arguments, Context: cloneAgentLoopContext(current), Result: cloneToolOutput(output), IsError: executionErr != nil})
		if afterErr != nil {
			failure := syntheticLoopToolError(call, afterErr.Error(), afterErr)
			output, executionErr = failure.output, failure.err
		} else {
			if after.Content != nil && *after.Content != nil {
				output.Content = make([]llm.ToolResultContentBlock, len(*after.Content))
				copy(output.Content, *after.Content)
				output.Text = ""
			}
			if after.Details != nil && !isNilInterface(*after.Details) {
				if details, ok := cloneToolDetails(*after.Details); ok {
					output.Details = details
				} else {
					output.Details = *after.Details
				}
			}
			if after.Usage != nil {
				usage := *after.Usage
				output.Usage = &usage
			}
			if after.Terminate != nil {
				output.Terminate = *after.Terminate
			}
			if after.IsError != nil {
				if *after.IsError && executionErr == nil {
					executionErr = errors.New("tool result marked as error")
				}
				if !*after.IsError {
					executionErr = nil
				}
			}
		}
	}
	return loopToolOutcome{call: call, output: output, err: executionErr, executed: true}, nil
}

func (l *AgentLoop) toolResultMessages(ctx context.Context, turn uint32, outcomes []loopToolOutcome, executions uint32) (loopToolBatch, error) {
	batch := loopToolBatch{executions: executions, terminate: allLoopToolOutcomesTerminate(outcomes)}
	for _, outcome := range outcomes {
		message, err := l.toolResultMessage(outcome)
		if err != nil {
			return loopToolBatch{}, err
		}
		processed, err := l.emitMessage(ctx, turn, message)
		if err != nil {
			return loopToolBatch{}, err
		}
		batch.messages = append(batch.messages, processed)
	}
	return batch, nil
}

func (l *AgentLoop) toolResultMessage(outcome loopToolOutcome) (agentmsg.Message, error) {
	var details json.RawMessage
	if outcome.output.Details != nil {
		encoded, err := json.Marshal(outcome.output.Details)
		if err != nil {
			return nil, err
		}
		details = encoded
	}
	metadata := llm.ToolResultMetadata{Details: details, Usage: outcome.output.Usage, AddedToolNames: outcome.output.AddedToolNames}
	var conversation llm.ConversationMessage
	var err error
	if outcome.output.Content != nil {
		conversation, err = llm.NewToolResultContentMessageWithMetadata(outcome.call.ID(), outcome.call.Name(), outcome.output.Content, outcome.err != nil, l.now(), metadata)
	} else {
		text, textErr := llm.NewTextBlock(outcome.output.Text)
		if textErr != nil {
			return nil, textErr
		}
		conversation, err = llm.NewToolResultMessageWithMetadata(outcome.call.ID(), outcome.call.Name(), []llm.TextBlock{text}, outcome.err != nil, l.now(), metadata)
	}
	if err != nil {
		return nil, err
	}
	wrapped, err := agentmsg.NewLLM(conversation)
	if err != nil {
		return nil, err
	}
	return wrapped, nil
}

func truncatedToolCallOutcome(call llm.ToolCallBlock) (ToolOutput, error) {
	failure := syntheticLoopToolError(call, fmt.Sprintf("Tool call %q was not executed: the response hit the output token limit, so its arguments may be truncated. Re-issue the tool call with complete arguments.", call.Name()), ErrTruncatedToolCall)
	return failure.output, failure.err
}

func syntheticLoopToolError(call llm.ToolCallBlock, text string, err error) loopToolOutcome {
	return loopToolOutcome{call: call, output: ToolOutput{Text: text, Details: map[string]any{}}, err: err}
}

func allLoopToolOutcomesTerminate(outcomes []loopToolOutcome) bool {
	if len(outcomes) == 0 {
		return false
	}
	for _, outcome := range outcomes {
		if !outcome.output.Terminate {
			return false
		}
	}
	return true
}

func (l *AgentLoop) emitToolStart(ctx context.Context, turn uint32, call llm.ToolCallBlock) error {
	return l.emit(ctx, ToolExecutionStartEvent{RunID: l.config.RunID, Turn: turn, ToolCallID: call.ID(), ToolName: call.Name(), Arguments: call.ArgumentsJSON()})
}
func (l *AgentLoop) emitToolEnd(ctx context.Context, turn uint32, outcome loopToolOutcome) error {
	return l.emit(ctx, ToolExecutionEndEvent{RunID: l.config.RunID, Turn: turn, ToolCallID: outcome.call.ID(), ToolName: outcome.call.Name(), Arguments: outcome.call.ArgumentsJSON(), Result: cloneToolOutput(outcome.output), IsError: outcome.err != nil, Err: outcome.err})
}
func (l *AgentLoop) emitMessage(ctx context.Context, turn uint32, message agentmsg.Message) (agentmsg.Message, error) {
	// The public start boundary observes the message as it entered the loop.
	// coding-agent's message_end hook may replace the same-role finalized
	// message afterwards; that replacement belongs to message_end, retained
	// context, and the provider request that follows, not to message_start.
	if err := l.emit(ctx, MessageStartEvent{RunID: l.config.RunID, Turn: turn, Message: agentmsg.CloneOne(message)}); err != nil {
		return nil, err
	}
	processed, err := l.processMessage(ctx, message)
	if err != nil {
		return nil, err
	}
	if err := l.emit(ctx, MessageEndEvent{RunID: l.config.RunID, Turn: turn, Message: agentmsg.CloneOne(processed)}); err != nil {
		return nil, err
	}
	return processed, nil
}

func (l *AgentLoop) processMessage(ctx context.Context, message agentmsg.Message) (agentmsg.Message, error) {
	processed := agentmsg.CloneOne(message)
	if l.config.ProcessMessage != nil {
		var err error
		processed, err = l.config.ProcessMessage(ctx, processed)
		if err != nil {
			return nil, err
		}
	}
	if isNilInterface(processed) || processed.Role() != message.Role() {
		return nil, fmt.Errorf("%w: processed message changed role", ErrInvariant)
	}
	if isAssistantPartialMessage(processed) {
		return nil, fmt.Errorf("%w: processed message is partial", ErrInvariant)
	}
	return agentmsg.CloneOne(processed), nil
}

func (l *AgentLoop) emit(ctx context.Context, event AgentEvent) error {
	return <-l.emitAsync(ctx, event)
}
func (l *AgentLoop) emitAsync(ctx context.Context, event AgentEvent) <-chan error {
	dispatcher, ok := ctx.Value(agentLoopEventDispatcherKey{}).(*agentLoopEventDispatcher)
	if !ok {
		done := make(chan error, 1)
		done <- l.config.Emit(ctx, cloneAgentEvent(event))
		close(done)
		return done
	}
	return dispatcher.enqueue(ctx, cloneAgentEvent(event))
}
func (l *AgentLoop) messagesFrom(ctx context.Context, source AgentLoopMessageSource) ([]agentmsg.Message, error) {
	if source == nil {
		return nil, nil
	}
	messages, err := source(ctx)
	if err != nil {
		return nil, err
	}
	for _, message := range messages {
		if message == nil {
			return nil, fmt.Errorf("%w: nil queued message", ErrInvalidQueueMessage)
		}
	}
	return agentmsg.Clone(messages), nil
}
func (l *AgentLoop) now() time.Time {
	l.nowMu.Lock()
	defer l.nowMu.Unlock()
	return l.config.Now().UTC().Truncate(time.Millisecond)
}

func cloneAgentLoopContext(value AgentLoopContext) AgentLoopContext {
	value.Messages = agentmsg.Clone(value.Messages)
	value.Tools = append([]AgentLoopTool(nil), value.Tools...)
	return value
}
func cloneAgentLoopTurnContext(message llm.AssistantTerminal, results []agentmsg.Message, current AgentLoopContext, newMessages []agentmsg.Message) AgentLoopTurnContext {
	return AgentLoopTurnContext{Message: message, ToolResults: agentmsg.Clone(results), Context: cloneAgentLoopContext(current), NewMessages: agentmsg.Clone(newMessages)}
}
func cloneAgentLoopTurnUpdate(value *AgentLoopTurnUpdate) *AgentLoopTurnUpdate {
	if value == nil {
		return nil
	}
	cloned := &AgentLoopTurnUpdate{}
	if value.Context != nil {
		context := cloneAgentLoopContext(*value.Context)
		cloned.Context = &context
	}
	if value.Model != nil {
		model := *value.Model
		cloned.Model = &model
	}
	if value.ThinkingLevel != nil {
		thinking := *value.ThinkingLevel
		cloned.ThinkingLevel = &thinking
	}
	if value.Stream != nil {
		stream := provider.CloneStreamOptions(*value.Stream)
		cloned.Stream = &stream
	}
	return cloned
}

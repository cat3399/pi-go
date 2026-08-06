package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sync"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

type activeRun struct {
	id               uint64
	ctx              context.Context
	cancel           context.CancelCauseFunc
	done             chan struct{}
	turn             uint32
	phase            Phase
	pendingToolCalls []string
}

type agentRunMode uint8

const (
	agentRunPrompt agentRunMode = iota + 1
	agentRunContinuation
)

type agentListener func(context.Context, AgentEvent) error

type observerEntry struct {
	id       uint64
	listener agentListener
}

type controlObserverEntry struct {
	id       uint64
	observer ControlObserver
}

func validateAgentMessageBatch(messages []agentmsg.Message, label string) error {
	for _, message := range messages {
		if isNilInterface(message) {
			return fmt.Errorf("%w: nil %s message", ErrInvalidRun, label)
		}
		if isAssistantPartialMessage(message) {
			return fmt.Errorf("%w: partial %s message", ErrInvalidRun, label)
		}
	}
	return nil
}

// Agent is the long-lived, in-memory stateful wrapper around AgentLoop. It
// owns mutable AgentState, queues, listeners and one active run. Persistence,
// retry, compaction and session policy belong to AgentSession or another host.
type Agent struct {
	mu      sync.Mutex
	clockMu sync.Mutex

	config runtimeConfig
	active *activeRun
	nextID uint64

	model         provider.Model
	hasModel      bool
	thinkingLevel provider.ThinkingLevel
	systemPrompt  string
	tool          ToolExecutor
	tools         []provider.ToolDefinition
	messages      []agentmsg.Message
	isStreaming   bool
	streaming     agentmsg.Message
	errorMessage  string

	observers        []observerEntry
	controlObservers []controlObserverEntry
	nextObserverID   uint64

	steeringQueue []agentmsg.Message
	followUpQueue []agentmsg.Message
	steeringMode  QueueMode
	followUpMode  QueueMode
}

func New(config Config) (*Agent, error) {
	runtime, err := validateConfig(config)
	if err != nil {
		return nil, err
	}
	if err := validateAgentMessageBatch(config.InitialMessages, "initial"); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	// Match createMutableAgentState: isolate the array while retaining each
	// immutable AgentMessage element as supplied.
	messages := append([]agentmsg.Message(nil), config.InitialMessages...)
	if len(runtime.tools) == 0 && !isNilInterface(runtime.tool) {
		definition, err := provider.NewToolDefinition(runtime.toolName, runtime.toolName, false, []byte(`{"type":"object"}`))
		if err != nil {
			return nil, fmt.Errorf("%w: default tool definition: %w", ErrInvalidConfig, err)
		}
		runtime.tools = []provider.ToolDefinition{definition}
	}
	return &Agent{
		config: runtime, model: runtime.model, hasModel: runtime.hasModel, thinkingLevel: runtime.thinkingLevel,
		systemPrompt: runtime.systemPrompt, tool: runtime.tool,
		tools:    append([]provider.ToolDefinition(nil), runtime.tools...),
		messages: messages, steeringMode: runtime.steeringMode, followUpMode: runtime.followUpMode,
	}, nil
}

// Subscribe accepts both the legacy fire-and-forget Observer and an
// error-returning listener. Error-returning listeners model an awaited JS
// listener: the first error stops the remaining listeners for that event.
func (a *Agent) Subscribe(observer any) func() {
	if a == nil || observer == nil {
		return func() {}
	}
	var listener agentListener
	switch value := observer.(type) {
	case Observer:
		listener = func(ctx context.Context, event AgentEvent) error { value(ctx, event); return nil }
	case func(context.Context, AgentEvent):
		listener = func(ctx context.Context, event AgentEvent) error { value(ctx, event); return nil }
	case func(context.Context, AgentEvent) error:
		listener = value
	default:
		return func() {}
	}
	a.mu.Lock()
	a.nextObserverID++
	id := a.nextObserverID
	a.observers = append(a.observers, observerEntry{id: id, listener: listener})
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

// SubscribeControl remains as a compatibility boundary for AgentSession
// control events. Stateful Agent itself emits only the canonical AgentEvent
// lifecycle; retry and compaction live above it.
func (a *Agent) SubscribeControl(observer ControlObserver) func() {
	if a == nil || observer == nil {
		return func() {}
	}
	a.mu.Lock()
	a.nextObserverID++
	id := a.nextObserverID
	a.controlObservers = append(a.controlObservers, controlObserverEntry{id: id, observer: observer})
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			for index := range a.controlObservers {
				if a.controlObservers[index].id == id {
					copy(a.controlObservers[index:], a.controlObservers[index+1:])
					a.controlObservers[len(a.controlObservers)-1] = controlObserverEntry{}
					a.controlObservers = a.controlObservers[:len(a.controlObservers)-1]
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
	state := State{
		phase: a.phaseLocked(), model: a.model, hasModel: a.hasModel, thinkingLevel: a.thinkingLevel,
		systemPrompt: a.systemPrompt, tools: append([]provider.ToolDefinition(nil), a.tools...),
		messages: append([]agentmsg.Message(nil), a.messages...), isStreaming: a.isStreaming,
		streamingMessage: agentmsg.CloneOne(a.streaming), errorMessage: a.errorMessage,
	}
	if a.active != nil {
		state.runID = a.active.id
		state.turn = a.active.turn
		state.pendingToolCalls = append([]string(nil), a.active.pendingToolCalls...)
	}
	return state
}

func (a *Agent) phaseLocked() Phase {
	if a.active == nil {
		return PhaseIdle
	}
	return a.active.phase
}

func (a *Agent) SetSystemPrompt(prompt string) error {
	if a == nil {
		return fmt.Errorf("%w: nil agent", ErrInvalidRun)
	}
	a.mu.Lock()
	a.systemPrompt = prompt
	a.mu.Unlock()
	return nil
}

func (a *Agent) SetModel(model provider.Model) error {
	if a == nil {
		return fmt.Errorf("%w: nil agent", ErrInvalidRun)
	}
	if _, err := provider.NewRequest(model, "", nil); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	a.mu.Lock()
	a.model = model
	a.hasModel = true
	a.mu.Unlock()
	return nil
}

func (a *Agent) SetThinkingLevel(level provider.ThinkingLevel) error {
	if a == nil || !level.Valid() {
		return fmt.Errorf("%w: invalid thinking level %q", ErrInvalidConfig, level)
	}
	a.mu.Lock()
	if a.hasModel {
		a.thinkingLevel = level
	} else {
		a.thinkingLevel = provider.ThinkingOff
	}
	a.mu.Unlock()
	return nil
}

func (a *Agent) SetTools(executor ToolExecutor, tools []provider.ToolDefinition) error {
	if a == nil {
		return fmt.Errorf("%w: nil agent", ErrInvalidRun)
	}
	if len(tools) != 0 && isNilInterface(executor) {
		return fmt.Errorf("%w: advertised tools require an executor", ErrInvalidConfig)
	}
	a.mu.Lock()
	a.tool = executor
	a.tools = append([]provider.ToolDefinition(nil), tools...)
	a.mu.Unlock()
	return nil
}

func (a *Agent) SetMessages(messages []agentmsg.Message) error {
	if a == nil {
		return fmt.Errorf("%w: nil agent", ErrInvalidRun)
	}
	for _, message := range messages {
		if isNilInterface(message) {
			return fmt.Errorf("%w: nil state message", ErrInvalidRun)
		}
		if isAssistantPartialMessage(message) {
			return fmt.Errorf("%w: partial state message", ErrInvalidRun)
		}
	}
	a.mu.Lock()
	a.messages = append([]agentmsg.Message(nil), messages...)
	a.mu.Unlock()
	return nil
}

// Reset mirrors the original mutable wrapper: it neither aborts nor rejects
// an active run and leaves model/system/thinking/tools/options unchanged.
func (a *Agent) Reset() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.messages = nil
	a.isStreaming = false
	a.streaming = nil
	a.errorMessage = ""
	a.steeringQueue = nil
	a.followUpQueue = nil
	if a.active != nil {
		a.active.pendingToolCalls = nil
	}
	a.mu.Unlock()
}

func (a *Agent) SetSteeringMode(mode QueueMode) error { return a.setQueueMode(mode, true) }
func (a *Agent) SetFollowUpMode(mode QueueMode) error { return a.setQueueMode(mode, false) }
func (a *Agent) SteeringMode() QueueMode {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.steeringMode
}
func (a *Agent) FollowUpMode() QueueMode {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.followUpMode
}
func (a *Agent) setQueueMode(mode QueueMode, steering bool) error {
	if a == nil || (mode != QueueOneAtATime && mode != QueueAll) {
		return fmt.Errorf("%w: invalid queue mode", ErrInvalidConfig)
	}
	a.mu.Lock()
	if steering {
		a.steeringMode = mode
	} else {
		a.followUpMode = mode
	}
	a.mu.Unlock()
	return nil
}

func (a *Agent) Abort(ctx context.Context) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	active := a.active
	if active != nil {
		active.cancel(ErrAgentAborted)
	}
	a.mu.Unlock()
	return nil
}

// Signal returns the active run's cancellation context. The returned context
// remains observable after Abort even though Abort itself is non-blocking.
func (a *Agent) Signal() (context.Context, bool) {
	if a == nil {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active == nil {
		return nil, false
	}
	return a.active.ctx, true
}

func (a *Agent) WaitForIdle(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	active := a.active
	a.mu.Unlock()
	if active == nil {
		return nil
	}
	select {
	case <-active.done:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (a *Agent) Run(ctx context.Context, prompt string) (Result, error) {
	active, err := a.beginRun(ctx)
	if err != nil {
		return Result{}, err
	}
	return a.runTextPrompt(active, prompt)
}

func (a *Agent) runTextPrompt(active *activeRun, prompt string) (result Result, err error) {
	timestamp, err := a.now()
	if err != nil {
		a.finishRun(active)
		return Result{}, fmt.Errorf("%w: %w", ErrInvalidRun, err)
	}
	text, err := llm.NewTextBlock(prompt)
	if err != nil {
		a.finishRun(active)
		return Result{}, fmt.Errorf("%w: prompt: %w", ErrInvalidRun, err)
	}
	message, err := llm.NewUserContentMessage([]llm.UserContentBlock{text}, timestamp)
	if err != nil {
		a.finishRun(active)
		return Result{}, fmt.Errorf("%w: prompt: %w", ErrInvalidRun, err)
	}
	wrapped, err := agentmsg.NewLLM(message)
	if err != nil {
		a.finishRun(active)
		return Result{}, err
	}
	return a.runLifecycle(active, []agentmsg.Message{wrapped}, false, agentRunPrompt)
}

func (a *Agent) RunContent(ctx context.Context, content []llm.UserContentBlock) (Result, error) {
	active, err := a.beginRun(ctx)
	if err != nil {
		return Result{}, err
	}
	timestamp, err := a.now()
	if err != nil {
		a.finishRun(active)
		return Result{}, fmt.Errorf("%w: %w", ErrInvalidRun, err)
	}
	message, err := llm.NewUserContentMessage(content, timestamp)
	if err != nil {
		a.finishRun(active)
		return Result{}, fmt.Errorf("%w: prompt content: %w", ErrInvalidRun, err)
	}
	wrapped, err := agentmsg.NewLLM(message)
	if err != nil {
		a.finishRun(active)
		return Result{}, err
	}
	return a.runLifecycle(active, []agentmsg.Message{wrapped}, false, agentRunPrompt)
}

func (a *Agent) RunAgentMessages(ctx context.Context, messages []agentmsg.Message) (Result, error) {
	active, err := a.beginRun(ctx)
	if err != nil {
		return Result{}, err
	}
	if err := validateAgentMessageBatch(messages, "prompt"); err != nil {
		a.finishRun(active)
		return Result{}, err
	}
	initial := agentmsg.Clone(messages)
	return a.runLifecycle(active, initial, false, agentRunPrompt)
}

func (a *Agent) Continue(ctx context.Context) (Result, error) {
	if a == nil {
		return Result{}, fmt.Errorf("%w: nil agent", ErrInvalidRun)
	}
	if ctx == nil {
		return Result{}, fmt.Errorf("%w: invalid continuation context", ErrInvalidRun)
	}
	a.mu.Lock()
	if !a.hasModel {
		a.mu.Unlock()
		return Result{}, ErrNoModelSelected
	}
	if a.active != nil {
		a.mu.Unlock()
		return Result{}, ErrBusy
	}
	if len(a.messages) == 0 {
		a.mu.Unlock()
		return Result{}, fmt.Errorf("%w: empty transcript", ErrCannotContinue)
	}
	lastRole := a.messages[len(a.messages)-1].Role()
	var prompts []agentmsg.Message
	skipInitialSteering := false
	mode := agentRunContinuation
	if lastRole == agentmsg.RoleAssistant {
		mode = agentRunPrompt
		prompts = a.drainQueueLocked(true)
		if len(prompts) != 0 {
			skipInitialSteering = true
		} else {
			prompts = a.drainQueueLocked(false)
		}
		if len(prompts) == 0 {
			a.mu.Unlock()
			return Result{}, fmt.Errorf("%w: assistant tail", ErrCannotContinue)
		}
	}
	active, err := a.beginRunLocked(ctx)
	a.mu.Unlock()
	if err != nil {
		return Result{}, err
	}
	return a.runLifecycle(active, prompts, skipInitialSteering, mode)
}

func (a *Agent) beginRun(ctx context.Context) (*activeRun, error) {
	if a == nil || ctx == nil {
		return nil, fmt.Errorf("%w: invalid run", ErrInvalidRun)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.beginRunLocked(ctx)
}

func (a *Agent) beginRunLocked(ctx context.Context) (*activeRun, error) {
	if !a.hasModel {
		return nil, ErrNoModelSelected
	}
	if a.active != nil {
		return nil, ErrBusy
	}
	if a.nextID == math.MaxUint64 {
		return nil, ErrRunIDExhausted
	}
	a.nextID++
	runCtx, cancel := context.WithCancelCause(ctx)
	active := &activeRun{id: a.nextID, ctx: runCtx, cancel: cancel, done: make(chan struct{}), turn: 1, phase: PhaseProvider}
	a.active = active
	a.isStreaming = true
	a.streaming = nil
	a.errorMessage = ""
	return active, nil
}

func (a *Agent) runLifecycle(active *activeRun, prompts []agentmsg.Message, skipInitialSteering bool, mode agentRunMode) (result Result, runErr error) {
	defer a.finishRun(active)
	result.runID = active.id
	low, err := a.executeLowLoop(active, prompts, skipInitialSteering, mode)
	result = resultFromLoop(active.id, low)
	if err == nil {
		return result, nil
	}
	failure, failureErr := a.handleRunFailure(active, err)
	if failureErr != nil {
		return result, failureErr
	}
	result.terminal = failure
	return result, nil
}

func (a *Agent) executeLowLoop(active *activeRun, prompts []agentmsg.Message, skipInitialSteering bool, mode agentRunMode) (result AgentLoopResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("agent loop panicked: %s", safeValueText(recovered))
		}
	}()
	loop, contextSnapshot, err := a.newLoop(active, skipInitialSteering)
	if err != nil {
		return AgentLoopResult{}, err
	}
	if mode == agentRunContinuation {
		return loop.Continue(active.ctx, contextSnapshot)
	}
	return loop.Run(active.ctx, append([]agentmsg.Message(nil), prompts...), contextSnapshot)
}

func resultFromLoop(runID uint64, low AgentLoopResult) Result {
	return Result{runID: runID, terminal: low.Terminal, providerTurns: low.ProviderTurns, toolExecutions: low.ToolExecutions}
}

func (a *Agent) newLoop(active *activeRun, skipInitialSteering bool) (*AgentLoop, AgentLoopContext, error) {
	a.mu.Lock()
	model := a.model
	thinking := a.thinkingLevel
	systemPrompt := a.systemPrompt
	executor := a.tool
	definitions := append([]provider.ToolDefinition(nil), a.tools...)
	messages := append([]agentmsg.Message(nil), a.messages...)
	stream := provider.CloneStreamOptions(a.config.stream)
	a.mu.Unlock()

	if a.config.prepareTurn != nil {
		snapshot, err := a.config.prepareTurn(active.ctx, TurnContext{RunID: active.id, Turn: 1})
		if err != nil {
			return nil, AgentLoopContext{}, err
		}
		model, thinking, systemPrompt, executor = snapshot.Model, snapshot.ThinkingLevel, snapshot.SystemPrompt, snapshot.Tool
		definitions = append([]provider.ToolDefinition(nil), snapshot.Tools...)
		stream = provider.CloneStreamOptions(snapshot.Stream)
	}
	tools, err := adaptAgentLoopTools(definitions, executor)
	if err != nil {
		return nil, AgentLoopContext{}, err
	}
	contextSnapshot := AgentLoopContext{SystemPrompt: systemPrompt, Messages: messages, Tools: tools}
	turn := uint32(1)
	config := AgentLoopConfig{
		RunID: active.id, Provider: a.config.provider, Model: model, ThinkingLevel: thinking,
		Stream:        stream,
		ToolExecution: a.config.toolExecution, BeforeToolCall: bridgeAgentLoopBeforeHook(a.config.beforeToolCall),
		AfterToolCall: bridgeAgentLoopAfterHook(a.config.afterToolCall), Now: a.config.now,
		ConvertToLLM: a.agentLoopConvertToLLM(), TransformContext: a.agentLoopTransformContext(), GetAPIKey: a.config.getAPIKey,
		Emit: func(ctx context.Context, event AgentEvent) error { return a.processEvent(active, ctx, event) },
	}
	if a.config.messageEnd != nil {
		config.ProcessMessage = func(ctx context.Context, message agentmsg.Message) (agentmsg.Message, error) {
			replacement, err := a.config.messageEnd(ctx, agentmsg.CloneOne(message))
			if err != nil {
				return nil, err
			}
			if replacement == nil {
				return message, nil
			}
			return agentmsg.CloneOne(replacement), nil
		}
	}
	if a.config.prepareTurn != nil || a.config.prepareNextTurn != nil {
		config.PrepareNextTurn = func(ctx context.Context, input AgentLoopTurnContext) (*AgentLoopTurnUpdate, error) {
			turn++
			update := AgentLoopTurnUpdate{}
			changed := false
			a.mu.Lock()
			hasQueued := len(a.steeringQueue) != 0 || len(a.followUpQueue) != 0
			a.mu.Unlock()
			_, toolTurn := input.Message.(llm.AssistantToolUseMessage)
			if a.config.prepareTurn != nil && (toolTurn || hasQueued) {
				snapshot, err := a.config.prepareTurn(ctx, TurnContext{RunID: active.id, Turn: turn})
				if err != nil {
					return nil, err
				}
				adapted, err := adaptAgentLoopTools(snapshot.Tools, snapshot.Tool)
				if err != nil {
					return nil, err
				}
				next := cloneAgentLoopContext(input.Context)
				next.SystemPrompt = snapshot.SystemPrompt
				next.Tools = adapted
				stream := provider.CloneStreamOptions(snapshot.Stream)
				update.Context, update.Model, update.ThinkingLevel, update.Stream = &next, &snapshot.Model, &snapshot.ThinkingLevel, &stream
				changed = true
			}
			if a.config.prepareNextTurn != nil {
				fullInput := cloneAgentLoopTurnContext(input.Message, input.ToolResults, input.Context, input.NewMessages)
				full, err := a.config.prepareNextTurn(ctx, fullInput)
				if err != nil {
					return nil, err
				}
				if full != nil {
					if full.Context != nil {
						next := cloneAgentLoopContext(*full.Context)
						update.Context = &next
						changed = true
					}
					if full.Model != nil {
						model := *full.Model
						update.Model = &model
						changed = true
					}
					if full.ThinkingLevel != nil {
						thinking := *full.ThinkingLevel
						update.ThinkingLevel = &thinking
						changed = true
					}
					if full.Stream != nil {
						stream := provider.CloneStreamOptions(*full.Stream)
						update.Stream = &stream
						changed = true
					}
				}
			}
			if !changed {
				return nil, nil
			}
			return &update, nil
		}
	}
	firstSteeringPoll := true
	config.GetSteeringMessages = func(ctx context.Context) ([]agentmsg.Message, error) {
		if context.Cause(ctx) != nil {
			return nil, nil
		}
		a.mu.Lock()
		if skipInitialSteering && firstSteeringPoll {
			firstSteeringPoll = false
			a.mu.Unlock()
			return nil, nil
		}
		firstSteeringPoll = false
		drained := a.drainQueueLocked(true)
		a.mu.Unlock()
		if len(drained) != 0 {
			a.notifyControl(active.ctx, QueueUpdateEvent{RunID: active.id, Turn: active.turn})
		}
		return drained, nil
	}
	config.GetFollowUpMessages = func(ctx context.Context) ([]agentmsg.Message, error) {
		if context.Cause(ctx) != nil {
			return nil, nil
		}
		a.mu.Lock()
		drained := a.drainQueueLocked(false)
		a.mu.Unlock()
		if len(drained) != 0 {
			a.notifyControl(active.ctx, QueueUpdateEvent{RunID: active.id, Turn: active.turn})
		}
		return drained, nil
	}
	loop, err := NewAgentLoop(config)
	return loop, contextSnapshot, err
}

func adaptAgentLoopTools(definitions []provider.ToolDefinition, executor ToolExecutor) ([]AgentLoopTool, error) {
	if len(definitions) == 0 {
		return nil, nil
	}
	if isNilInterface(executor) {
		return nil, fmt.Errorf("%w: advertised tools require an executor", ErrInvalidConfig)
	}
	tools := make([]AgentLoopTool, 0, len(definitions))
	for _, definition := range definitions {
		tool, err := NewAgentLoopToolAdapter(definition, executor, nil)
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func (a *Agent) agentLoopConvertToLLM() AgentLoopConvertToLLM {
	return func(ctx context.Context, messages []agentmsg.Message) ([]llm.ConversationMessage, error) {
		if a.config.transformAgentContext != nil {
			transformed, err := a.config.transformAgentContext(ctx, append([]agentmsg.Message(nil), messages...))
			if err != nil {
				return nil, err
			}
			if transformed != nil {
				messages = append([]agentmsg.Message(nil), (*transformed)...)
			}
		}
		if a.config.convertToLLM != nil {
			return a.config.convertToLLM(ctx, agentmsg.Clone(messages))
		}
		return agentmsg.ConvertToLLM(messages)
	}
}

func (a *Agent) agentLoopTransformContext() AgentLoopTransformContext {
	if a.config.transformContext == nil {
		return nil
	}
	return func(ctx context.Context, messages []agentmsg.Message) ([]agentmsg.Message, error) {
		converted, err := agentmsg.ConvertToLLM(messages)
		if err != nil {
			return nil, err
		}
		transformed, err := a.config.transformContext(ctx, converted)
		if err != nil {
			return nil, err
		}
		result := make([]agentmsg.Message, 0, len(transformed))
		for _, message := range transformed {
			wrapped, err := agentmsg.NewLLM(message)
			if err != nil {
				return nil, err
			}
			result = append(result, wrapped)
		}
		return result, nil
	}
}

func bridgeAgentLoopBeforeHook(hook BeforeToolCallHook) AgentLoopBeforeToolCallHook {
	if hook == nil {
		return nil
	}
	return func(ctx context.Context, input AgentLoopBeforeToolCallContext) (AgentLoopBeforeToolCallResult, error) {
		arguments, err := jsonMarshal(input.Arguments)
		if err != nil {
			return AgentLoopBeforeToolCallResult{}, err
		}
		conversation, err := agentmsg.ConvertToLLM(input.Context.Messages)
		if err != nil {
			return AgentLoopBeforeToolCallResult{}, err
		}
		result, err := hook(ctx, BeforeToolCallContext{Assistant: input.Assistant, ToolCall: input.ToolCall, Arguments: arguments, Context: conversation})
		converted := AgentLoopBeforeToolCallResult{Block: result.Block, Reason: result.Reason}
		if result.Arguments != nil {
			var value any
			if decodeErr := json.Unmarshal(*result.Arguments, &value); decodeErr != nil {
				return AgentLoopBeforeToolCallResult{}, decodeErr
			}
			converted.Arguments = &value
		}
		return converted, err
	}
}

func bridgeAgentLoopAfterHook(hook AfterToolCallHook) AgentLoopAfterToolCallHook {
	if hook == nil {
		return nil
	}
	return func(ctx context.Context, input AgentLoopAfterToolCallContext) (AgentLoopAfterToolCallResult, error) {
		arguments, err := jsonMarshal(input.Arguments)
		if err != nil {
			return AgentLoopAfterToolCallResult{}, err
		}
		conversation, err := agentmsg.ConvertToLLM(input.Context.Messages)
		if err != nil {
			return AgentLoopAfterToolCallResult{}, err
		}
		result, err := hook(ctx, AfterToolCallContext{Assistant: input.Assistant, ToolCall: input.ToolCall, Arguments: arguments, Context: conversation, Result: input.Result, IsError: input.IsError})
		if err != nil {
			return AgentLoopAfterToolCallResult{}, err
		}
		return AgentLoopAfterToolCallResult{Content: result.Content, Details: result.Details, IsError: result.IsError, Usage: result.Usage, Terminate: result.Terminate}, nil
	}
}

func jsonMarshal(value any) ([]byte, error) {
	return json.Marshal(value)
}

func (a *Agent) processEvent(active *activeRun, ctx context.Context, event AgentEvent) error {
	if event == nil {
		return nil
	}
	a.mu.Lock()
	if a.active != active {
		a.mu.Unlock()
		return fmt.Errorf("%w: event for inactive run", ErrInvariant)
	}
	a.reduceEventLocked(event)
	listeners := make([]agentListener, 0, len(a.observers))
	for _, entry := range a.observers {
		if entry.listener != nil {
			listeners = append(listeners, entry.listener)
		}
	}
	a.mu.Unlock()
	for _, listener := range listeners {
		if err := callAgentListener(listener, ctx, cloneAgentEvent(event)); err != nil {
			return err
		}
	}
	return nil
}

func callAgentListener(listener agentListener, ctx context.Context, event AgentEvent) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("agent listener panicked: %s", safeValueText(recovered))
		}
	}()
	return listener(ctx, event)
}

func (a *Agent) notifyControl(ctx context.Context, event AgentControlEvent) {
	a.mu.Lock()
	observers := make([]ControlObserver, 0, len(a.controlObservers))
	for _, entry := range a.controlObservers {
		if entry.observer != nil {
			observers = append(observers, entry.observer)
		}
	}
	a.mu.Unlock()
	for _, observer := range observers {
		observer(ctx, cloneAgentControlEvent(event))
	}
}

func (a *Agent) reduceEventLocked(event AgentEvent) {
	active := a.active
	switch value := event.(type) {
	case TurnStartEvent:
		active.turn, active.phase = value.Turn, PhaseProvider
	case MessageStartEvent:
		a.streaming = agentmsg.CloneOne(value.Message)
	case MessageUpdateEvent:
		a.streaming = agentmsg.CloneOne(value.Message)
	case MessageEndEvent:
		a.streaming = nil
		a.messages = append(a.messages, agentmsg.CloneOne(value.Message))
	case ToolExecutionStartEvent:
		active.phase = PhaseTool
		active.pendingToolCalls = append(active.pendingToolCalls, value.ToolCallID)
	case ToolExecutionEndEvent:
		for index, id := range active.pendingToolCalls {
			if id == value.ToolCallID {
				active.pendingToolCalls = append(active.pendingToolCalls[:index], active.pendingToolCalls[index+1:]...)
				break
			}
		}
	case TurnEndEvent:
		active.phase = PhaseProvider
		if wrapped, ok := value.Message.(agentmsg.LLM); ok {
			if failure, ok := wrapped.Conversation().(llm.AssistantFailureMessage); ok {
				a.errorMessage = failure.ErrorMessage()
			}
		}
	case AgentEndEvent:
		a.streaming = nil
		active.phase = PhaseSettling
	}
}

func (a *Agent) handleRunFailure(active *activeRun, cause error) (llm.AssistantTerminal, error) {
	a.mu.Lock()
	model := a.model
	turn := active.turn
	aborted := context.Cause(active.ctx) != nil
	a.mu.Unlock()
	reason := llm.FinishError
	if aborted {
		reason = llm.FinishAborted
	}
	failure, err := llm.NewFailure(safeErrorText(cause), cause)
	if err != nil {
		return nil, err
	}
	text, err := llm.NewTextBlock("")
	if err != nil {
		return nil, err
	}
	failureTerminal, err := llm.NewAssistantFailureMessageWithBlocksAndMetadata(
		[]llm.AssistantBlock{text}, reason, failure, llm.Usage{}, a.mustNow(),
		llm.AssistantProvenance{Provider: model.Provider(), API: model.API(), Model: model.ID()}, nil, nil,
	)
	if err != nil {
		return nil, err
	}
	var terminal llm.AssistantTerminal = failureTerminal
	wrappedLLM, err := agentmsg.NewLLM(terminal)
	if err != nil {
		return nil, err
	}
	var wrapped agentmsg.Message = wrappedLLM
	if a.config.messageEnd != nil {
		replacement, hookErr := a.config.messageEnd(active.ctx, agentmsg.CloneOne(wrapped))
		if hookErr != nil {
			return nil, hookErr
		}
		if replacement != nil {
			standard, ok := replacement.(agentmsg.LLM)
			if !ok {
				return nil, fmt.Errorf("%w: processed synthetic failure is %T", ErrInvariant, replacement)
			}
			replacedTerminal, ok := standard.Conversation().(llm.AssistantTerminal)
			if !ok {
				return nil, fmt.Errorf("%w: processed synthetic failure is not terminal", ErrInvariant)
			}
			terminal = replacedTerminal
			wrapped = agentmsg.CloneOne(replacement)
		}
	}
	for _, event := range []AgentEvent{
		MessageStartEvent{RunID: active.id, Turn: turn, Message: wrapped},
		MessageEndEvent{RunID: active.id, Turn: turn, Message: wrapped, Model: model},
		TurnEndEvent{RunID: active.id, Turn: turn, Message: wrapped},
		AgentEndEvent{RunID: active.id, Turn: turn, Messages: []agentmsg.Message{wrapped}, Terminal: terminal},
	} {
		if err := a.processEvent(active, active.ctx, event); err != nil {
			return nil, err
		}
	}
	return terminal, nil
}

func (a *Agent) finishRun(active *activeRun) {
	a.mu.Lock()
	if a.active == active {
		a.isStreaming = false
		a.streaming = nil
		active.pendingToolCalls = nil
		a.active = nil
		close(active.done)
	}
	a.mu.Unlock()
}

func (a *Agent) Steer(prompt string) error    { return a.enqueueText(prompt, true) }
func (a *Agent) FollowUp(prompt string) error { return a.enqueueText(prompt, false) }
func (a *Agent) SteerContent(content []llm.UserContentBlock) error {
	return a.enqueueContent(content, true)
}
func (a *Agent) FollowUpContent(content []llm.UserContentBlock) error {
	return a.enqueueContent(content, false)
}
func (a *Agent) SteerAgentMessage(message agentmsg.Message) error {
	return a.enqueueAgentMessage(message, true)
}
func (a *Agent) FollowUpAgentMessage(message agentmsg.Message) error {
	return a.enqueueAgentMessage(message, false)
}

func (a *Agent) enqueueText(prompt string, steering bool) error {
	timestamp, err := a.now()
	if err != nil {
		return err
	}
	text, err := llm.NewTextBlock(prompt)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidQueueMessage, err)
	}
	message, err := llm.NewUserContentMessage([]llm.UserContentBlock{text}, timestamp)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidQueueMessage, err)
	}
	wrapped, err := agentmsg.NewLLM(message)
	if err != nil {
		return err
	}
	return a.enqueueAgentMessage(wrapped, steering)
}

func (a *Agent) enqueueContent(content []llm.UserContentBlock, steering bool) error {
	timestamp, err := a.now()
	if err != nil {
		return err
	}
	message, err := llm.NewUserContentMessage(content, timestamp)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidQueueMessage, err)
	}
	wrapped, err := agentmsg.NewLLM(message)
	if err != nil {
		return err
	}
	return a.enqueueAgentMessage(wrapped, steering)
}

func (a *Agent) enqueueAgentMessage(message agentmsg.Message, steering bool) error {
	if a == nil || isNilInterface(message) {
		return fmt.Errorf("%w: invalid queued message", ErrInvalidQueueMessage)
	}
	if isAssistantPartialMessage(message) {
		return fmt.Errorf("%w: partial queued message", ErrInvalidQueueMessage)
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

func (a *Agent) enqueueMessage(message llm.ConversationMessage, steering bool) error {
	wrapped, err := agentmsg.NewLLM(message)
	if err != nil {
		return err
	}
	return a.enqueueAgentMessage(wrapped, steering)
}

func (a *Agent) drainQueueLocked(steering bool) []agentmsg.Message {
	queue, mode := &a.followUpQueue, a.followUpMode
	if steering {
		queue, mode = &a.steeringQueue, a.steeringMode
	}
	if len(*queue) == 0 {
		return nil
	}
	count := 1
	if mode == QueueAll {
		count = len(*queue)
	}
	result := append([]agentmsg.Message(nil), (*queue)[:count]...)
	copy(*queue, (*queue)[count:])
	*queue = (*queue)[:len(*queue)-count]
	return result
}

func (a *Agent) RichQueues() (steering, followUp []llm.ConversationMessage) {
	if a == nil {
		return nil, nil
	}
	a.mu.Lock()
	steeringMessages := append([]agentmsg.Message(nil), a.steeringQueue...)
	followMessages := append([]agentmsg.Message(nil), a.followUpQueue...)
	a.mu.Unlock()
	steering, _ = agentmsg.ConvertToLLM(steeringMessages)
	followUp, _ = agentmsg.ConvertToLLM(followMessages)
	return
}

func (a *Agent) Queues() (steering, followUp []llm.UserTextMessage) {
	richSteering, richFollow := a.RichQueues()
	for _, message := range richSteering {
		if text, ok := queuedUserTextSnapshot(message); ok {
			steering = append(steering, text)
		}
	}
	for _, message := range richFollow {
		if text, ok := queuedUserTextSnapshot(message); ok {
			followUp = append(followUp, text)
		}
	}
	return
}

func queuedUserTextSnapshot(message llm.ConversationMessage) (llm.UserTextMessage, bool) {
	if text, ok := message.(llm.UserTextMessage); ok {
		return text, true
	}
	content, ok := message.(llm.UserContentMessage)
	if !ok || len(content.Content()) != 1 {
		return llm.UserTextMessage{}, false
	}
	block, ok := content.Content()[0].(llm.TextBlock)
	if !ok {
		return llm.UserTextMessage{}, false
	}
	text, err := llm.NewUserTextMessage(block.Text(), content.Timestamp())
	return text, err == nil
}

func (a *Agent) HasQueuedMessages() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.steeringQueue) != 0 || len(a.followUpQueue) != 0
}

func (a *Agent) ClearSteeringQueue() { a.clearQueue(true) }
func (a *Agent) ClearFollowUpQueue() { a.clearQueue(false) }
func (a *Agent) ClearAllQueues() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.steeringQueue, a.followUpQueue = nil, nil
	a.mu.Unlock()
}
func (a *Agent) clearQueue(steering bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	if steering {
		a.steeringQueue = nil
	} else {
		a.followUpQueue = nil
	}
	a.mu.Unlock()
}

func (a *Agent) now() (value time.Time, err error) {
	if a == nil {
		return time.Time{}, fmt.Errorf("%w: nil agent", ErrInvalidRun)
	}
	a.clockMu.Lock()
	defer a.clockMu.Unlock()
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: clock panicked: %s", ErrInvariant, safeValueText(recovered))
		}
	}()
	value = a.config.now().UTC().Truncate(time.Millisecond)
	if value.IsZero() {
		return time.Time{}, fmt.Errorf("%w: invalid clock value", ErrInvariant)
	}
	return value, nil
}

func (a *Agent) mustNow() time.Time {
	value, err := a.now()
	if err != nil {
		return time.Now().UTC().Truncate(time.Millisecond)
	}
	return value
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

func isAssistantPartialMessage(message agentmsg.Message) bool {
	switch message.(type) {
	case agentmsg.AssistantPartial, *agentmsg.AssistantPartial:
		return true
	default:
		return false
	}
}

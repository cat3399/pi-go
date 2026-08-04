package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

// SessionConfig supplies the long-lived product state around the existing
// stateful coordinator.
// Provider and Transcript are lifecycle dependencies; all prompt-visible
// settings below are owned by AgentSession and snapshotted per provider turn.
type SessionConfig struct {
	Provider       provider.Provider
	Transcript     Transcript
	Model          provider.ModelRef
	ThinkingLevel  provider.ThinkingLevel
	SystemPrompt   string
	Tool           ToolExecutor
	Tools          []provider.ToolDefinition
	BeforeToolCall BeforeToolCallHook
	AfterToolCall  AfterToolCallHook
	Stream         provider.StreamOptions
	// ResolveStreamOptions resolves credentials and request headers for the
	// model selected by a concrete turn. It is invoked outside session locks.
	ResolveStreamOptions func(context.Context, provider.ModelRef) (provider.StreamOptions, error)
	Hooks                Hooks

	ToolExecution     ToolExecutionMode
	TransformContext  ContextTransform
	SteeringMode      QueueMode
	FollowUpMode      QueueMode
	ContextWindow     uint64
	ContextReserve    uint64
	KeepRecentTokens  uint64
	Summarizer        session.Summarizer
	Retry             RetryPolicy
	Now               func() time.Time
	SettlementTimeout time.Duration
}

// SessionState is a copy-only view of the mutable product configuration.
// Conversation remains durable in Transcript, which is intentionally the
// only source of truth for a resumed session.
type SessionState struct {
	Model         provider.ModelRef
	ThinkingLevel provider.ThinkingLevel
	SystemPrompt  string
	Tools         []provider.ToolDefinition
	Active        State
}

// SessionEvent is the product event surface. It preserves the coordinator
// event ordering while attaching the current session configuration snapshot.
type SessionEvent struct {
	// Type mirrors pi AgentSession control-plane event names. Event is retained
	// for the lower-level coordinator payload where applicable.
	Type     string
	Event    Event
	State    SessionState
	Steering []llm.ConversationMessage
	FollowUp []llm.ConversationMessage
	// WillRetry is meaningful on agent_end. It tells consumers whether this
	// completed low-level run is followed by a session retry continuation.
	WillRetry bool
	// Retry fields are populated on auto_retry_start and auto_retry_end. They
	// describe the whole session retry series rather than a low-level attempt.
	RetryAttempt      uint32
	RetryMaxAttempts  uint32
	RetryDelay        time.Duration
	RetrySucceeded    bool
	RetryErrorMessage string
	FinalError        string
	// SummarizationSource scopes a retry to the compaction workflow. It is
	// deliberately explicit so clients never infer it from a concurrent
	// top-level agent retry.
	SummarizationSource    string
	RetryFailureKind       provider.FailureKind
	RetryHTTPStatus        int
	RetryFinishReason      provider.RetryFinishReason
	CompactionReason       CompactionReason
	CompactionResult       *session.CompactResult
	CompactionAborted      bool
	CompactionWillRetry    bool
	CompactionErrorMessage string
	Message                llm.ConversationMessage
	AgentMessage           agentmsg.Message
	Terminal               llm.AssistantTerminal
	ToolResults            []llm.ConversationMessage
	Messages               []llm.ConversationMessage
	AgentMessages          []agentmsg.Message
}
type SessionObserver func(context.Context, SessionEvent)

// AgentSession owns the persistent agent product state. It never invokes
// providers, tools, transcript writes, or observers while holding mu. The
// loop calls prepareTurn immediately before each provider request, so a
// model/tool/prompt change made while tools are running applies to the next
// request in that same run.
type AgentSession struct {
	mu             sync.RWMutex
	loop           *Agent
	transcript     Transcript
	model          provider.ModelRef
	thinkingLevel  provider.ThinkingLevel
	systemPrompt   string
	tool           ToolExecutor
	tools          []provider.ToolDefinition
	beforeToolCall BeforeToolCallHook
	afterToolCall  AfterToolCallHook
	stream         provider.StreamOptions
	resolveStream  func(context.Context, provider.ModelRef) (provider.StreamOptions, error)
	hooks          Hooks
	// lifecycleMu owns admission, close state, and the complete top-level
	// lifecycle.  A low Agent run is only one phase of sessionRun: retry waits
	// and post-run continuations remain active too.
	lifecycleMu       sync.Mutex
	run               *sessionRun
	closing           bool
	closed            bool
	retry             provider.RetryController
	contextWindow     uint64
	contextReserve    uint64
	keepRecentTokens  uint64
	summarizer        session.Summarizer
	compactor         sessionCompactor
	observers         []sessionObserverEntry
	nextObserver      uint64
	loopUnsubscribe   func()
	runtimeTranscript *sessionTranscript
}

type sessionRun struct {
	ctx                   context.Context
	cancel                context.CancelCauseFunc
	done                  chan struct{}
	phase                 Phase
	retryAttempt          uint32
	retrySeries           bool
	retryDelay            time.Duration
	retryError            string
	overflowCompacted     bool
	assistantStarted      bool
	assistantHookStarted  bool
	committed             []llm.ConversationMessage
	committedAgent        []agentmsg.Message
	toolResults           []llm.ConversationMessage
	terminalModel         provider.ModelRef
	started               bool
	extensionSystemPrompt *string
}
type sessionTranscript struct {
	mu            sync.RWMutex
	durable       Transcript
	messages      []llm.ConversationMessage
	agentMessages []agentmsg.Message
	assistant     session.AssistantProvenance
	hasAssistant  bool
}

func newSessionTranscript(durable Transcript) *sessionTranscript {
	context := durable.Context()
	assistant, hasAssistant := context.AssistantProvenance()
	return &sessionTranscript{durable: durable, messages: context.Messages(), agentMessages: context.AgentMessages(), assistant: assistant, hasAssistant: hasAssistant}
}
func (t *sessionTranscript) Context() session.Context {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return session.NewAgentContext(t.agentMessages)
}
func (t *sessionTranscript) BuildContext() session.Context { return t.Context() }
func (t *sessionTranscript) Append(ctx context.Context, message llm.ConversationMessage, options session.AppendOptions) (session.Entry, error) {
	entry, err := t.durable.Append(ctx, message, options)
	if err != nil {
		return session.Entry{}, err
	}
	t.mu.Lock()
	t.messages = append(t.messages, message)
	if wrapped, wrapErr := agentmsg.NewLLM(message); wrapErr == nil {
		t.agentMessages = append(t.agentMessages, wrapped)
	}
	context := t.durable.Context()
	t.assistant, t.hasAssistant = context.AssistantProvenance()
	t.mu.Unlock()
	return entry, nil
}
func (t *sessionTranscript) AppendAgentMessage(ctx context.Context, message agentmsg.Message, options session.AppendOptions) (session.Entry, error) {
	durable, ok := t.durable.(interface {
		AppendAgentMessage(context.Context, agentmsg.Message, session.AppendOptions) (session.Entry, error)
	})
	if !ok {
		return session.Entry{}, fmt.Errorf("%w: transcript does not support agent messages", ErrInvalidConfig)
	}
	entry, err := durable.AppendAgentMessage(ctx, message, options)
	if err != nil {
		return session.Entry{}, err
	}
	converted, err := agentmsg.ConvertToLLM([]agentmsg.Message{message})
	if err != nil {
		return session.Entry{}, err
	}
	t.mu.Lock()
	t.agentMessages = append(t.agentMessages, agentmsg.CloneOne(message))
	t.messages = append(t.messages, converted...)
	current := t.durable.Context()
	t.assistant, t.hasAssistant = current.AssistantProvenance()
	t.mu.Unlock()
	return entry, nil
}
func (t *sessionTranscript) removeLastFailure() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.messages) != 0 {
		if _, ok := t.messages[len(t.messages)-1].(llm.AssistantFailureMessage); ok {
			t.messages = t.messages[:len(t.messages)-1]
			if len(t.agentMessages) > 0 {
				t.agentMessages = t.agentMessages[:len(t.agentMessages)-1]
			}
		}
	}
}

func (t *sessionTranscript) Compact(ctx context.Context, request session.CompactRequest) (session.CompactResult, error) {
	compactor, ok := t.durable.(sessionCompactor)
	if !ok {
		return session.CompactResult{}, ErrCompactionUnavailable
	}
	result, err := compactor.Compact(ctx, request)
	if err != nil {
		return session.CompactResult{}, err
	}
	var refreshed session.Context
	if builder, ok := t.durable.(ContextBuilder); ok {
		refreshed = builder.BuildContext()
	} else {
		refreshed = t.durable.Context()
	}
	t.mu.Lock()
	t.messages = refreshed.Messages()
	t.agentMessages = refreshed.AgentMessages()
	t.assistant, t.hasAssistant = refreshed.AssistantProvenance()
	t.mu.Unlock()
	return result, nil
}
func (t *sessionTranscript) AssistantProvenance() (session.AssistantProvenance, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.assistant, t.hasAssistant
}

type sessionObserverEntry struct {
	id       uint64
	observer SessionObserver
}

func sameMessages(left, right []llm.ConversationMessage) bool {
	return reflect.DeepEqual(left, right)
}

// NewSession constructs the long-lived layer and its coordinator. It
// deliberately does not recreate the loop on configuration changes: doing so
// would lose cancellation, queue and event ordering guarantees mid-run.
func NewSession(config SessionConfig) (*AgentSession, error) {
	if isNilInterface(config.Provider) {
		return nil, fmt.Errorf("%w: provider is required", ErrInvalidConfig)
	}
	if isNilInterface(config.Transcript) {
		return nil, fmt.Errorf("%w: transcript is required", ErrInvalidConfig)
	}
	if config.ContextReserve > config.ContextWindow && config.ContextWindow != 0 {
		return nil, fmt.Errorf("%w: context reserve exceeds window", ErrInvalidConfig)
	}
	compactor, hasCompactor := config.Transcript.(sessionCompactor)
	if config.ContextWindow != 0 && (!hasCompactor || config.Summarizer == nil) {
		return nil, fmt.Errorf("%w: automatic compaction requires Session and summarizer", ErrInvalidConfig)
	}
	if len(config.Tools) != 0 && isNilInterface(config.Tool) {
		return nil, fmt.Errorf("%w: advertised tools require a non-nil executor", ErrInvalidConfig)
	}
	if config.ThinkingLevel == "" {
		config.ThinkingLevel = provider.ThinkingOff
	}
	if !config.ThinkingLevel.Valid() {
		return nil, fmt.Errorf("%w: invalid thinking level %q", ErrInvalidConfig, config.ThinkingLevel)
	}
	config.ThinkingLevel = config.Model.ClampThinkingLevel(config.ThinkingLevel)
	retry, err := provider.NewRetryController(config.Retry)
	if err != nil {
		return nil, fmt.Errorf("%w: retry policy: %w", ErrInvalidConfig, err)
	}
	s := &AgentSession{
		transcript: config.Transcript, model: config.Model, thinkingLevel: config.ThinkingLevel,
		systemPrompt: config.SystemPrompt, tool: config.Tool, tools: append([]provider.ToolDefinition(nil), config.Tools...), beforeToolCall: composeBeforeToolHooks(config.BeforeToolCall, config.Hooks.ToolCall), afterToolCall: composeAfterToolHooks(config.AfterToolCall, config.Hooks.ToolResult),
		stream:        provider.CloneStreamOptions(config.Stream),
		resolveStream: config.ResolveStreamOptions, hooks: config.Hooks,
		retry:         retry,
		contextWindow: config.ContextWindow, contextReserve: config.ContextReserve, keepRecentTokens: config.KeepRecentTokens,
		summarizer: config.Summarizer, compactor: compactor,
	}
	s.runtimeTranscript = newSessionTranscript(config.Transcript)
	loop, err := New(Config{
		Provider: config.Provider, Transcript: s.runtimeTranscript, Model: config.Model,
		SystemPrompt: config.SystemPrompt, Tool: config.Tool, Tools: config.Tools, BeforeToolCall: s.beforeToolCall, AfterToolCall: s.afterToolCall,
		ToolExecution: config.ToolExecution, TransformContext: config.TransformContext, TransformAgentContext: contextHookTransform(config.Hooks.Context),
		MessageEnd:   s.messageEndTransform,
		SteeringMode: config.SteeringMode, FollowUpMode: config.FollowUpMode,
		ContextWindow: 0, ContextReserve: 0,
		// Session owns retries and (eventually) automatic compaction.  Keep the
		// low coordinator to a single provider attempt and disable its automatic
		// compaction path so lifecycle ownership cannot split between layers.
		KeepRecentTokens: 0, Summarizer: nil,
		Retry: RetryPolicy{MaxAttempts: 1}, Now: config.Now, SettlementTimeout: config.SettlementTimeout,
		PrepareTurn: s.prepareTurn,
	})
	if err != nil {
		return nil, err
	}
	s.loop = loop
	s.loopUnsubscribe = loop.Subscribe(s.handleLoopEvent)
	if s.hooks.SessionStart != nil {
		if hookErr := s.hooks.SessionStart(context.Background(), SessionStartHookEvent{Reason: SessionStartup}); hookErr != nil {
			s.loopUnsubscribe()
			return nil, hookErr
		}
	}
	return s, nil
}

func (s *AgentSession) handleLoopEvent(ctx context.Context, event Event) {
	if s == nil {
		return
	}
	s.mu.RLock()
	state := SessionState{Model: s.model, ThinkingLevel: s.thinkingLevel, SystemPrompt: s.systemPrompt, Tools: append([]provider.ToolDefinition(nil), s.tools...)}
	observers := make([]SessionObserver, 0, len(s.observers))
	for _, entry := range s.observers {
		if entry.observer != nil {
			observers = append(observers, entry.observer)
		}
	}
	s.mu.RUnlock()
	state.Active = s.activeState()
	types := []string{}
	var message llm.ConversationMessage
	var agentMessage agentmsg.Message
	var terminal llm.AssistantTerminal
	var toolResults, messages []llm.ConversationMessage
	var agentMessages []agentmsg.Message
	switch event.Kind {
	case EventRunStarted:
		s.lifecycleMu.Lock()
		if s.run == nil {
			s.lifecycleMu.Unlock()
			// Manual compact remains a standalone low-level lifecycle.
			types = []string{"agent_start"}
			break
		}
		s.run.started = true
		s.run.assistantStarted = false
		s.run.assistantHookStarted = false
		s.run.committed = nil
		s.run.committedAgent = nil
		s.run.toolResults = nil
		s.lifecycleMu.Unlock()
		// Every low-level continuation is a new upstream agent loop and therefore
		// emits its own agent_start. AgentSession adds agent_settled only once after
		// the complete retry/compaction/queue series becomes idle.
		types = []string{"agent_start"}
	case EventTurnStarted:
		s.resetSessionTurn(event)
		types = []string{"turn_start"}
	case EventProviderProgress:
		if _, ok := event.ProviderEvent.(llm.StartEvent); ok {
			if s.beginAssistantMessage() {
				types = append(types, "message_start")
			}
		} else {
			if s.beginAssistantMessage() {
				types = append(types, "message_start")
			}
			types = append(types, "message_update")
		}
	case EventMessageCommitted:
		message = event.Message
		agentMessage = agentmsg.CloneOne(event.AgentMessage)
		if agentMessage == nil && event.Message != nil {
			agentMessage, _ = agentmsg.NewLLM(event.Message)
		}
		if agentMessage != nil && agentMessage.Role() != agentmsg.RoleAssistant {
			types = append(types, "message_start")
		} else if agentMessage != nil && s.beginAssistantMessage() {
			types = append(types, "message_start")
		}
		s.recordCommitted(event.Message, agentMessage, event.Model)
		types = append(types, "message_end")
	case EventToolStarted:
		types = []string{"tool_execution_start"}
	case EventToolProgress:
		types = []string{"tool_execution_update"}
	case EventToolSettled:
		types = []string{"tool_execution_end"}
	case EventTurnSettled:
		terminal = event.Terminal
		toolResults = s.sessionTurnResults()
		types = []string{"turn_end"}
	case EventRunSettled:
		types = []string{"agent_end"}
		terminal = event.Terminal
		messages = s.sessionCommittedMessages()
		agentMessages = s.sessionCommittedAgentMessages()
	case EventQueueUpdated:
		types = []string{"queue_update"}
	}
	steering, follow := s.RichQueues()
	willRetry := event.Kind == EventRunSettled && s.willRetry(event.Terminal)
	for _, kind := range types {
		s.dispatchExtensionHook(ctx, kind, message, agentMessage, terminal, event)
		s.emitToObservers(ctx, observers, SessionEvent{Type: kind, Event: event, State: state, Steering: steering, FollowUp: follow, WillRetry: willRetry, Message: message, AgentMessage: agentmsg.CloneOne(agentMessage), Terminal: terminal, ToolResults: toolResults, Messages: messages, AgentMessages: agentMessages})
	}
	// Upstream ends a successful retry immediately after the successful
	// assistant message has been committed, before that low run emits
	// agent_end. A final failed low run ends only after its agent_end.
	if event.Kind == EventMessageCommitted && retrySucceededMessage(event.Message) {
		s.endRetrySeries(ctx, true, "")
	}
	if event.Kind == EventRunSettled && !willRetry {
		s.endRetrySeries(ctx, false, retryFinalError(event))
	}
}

// dispatchExtensionHook is observational for post-commit lifecycle events.
// Mutation/cancellation hooks run at their safe pre-boundaries (context and
// compaction); an error here cannot retroactively alter durable history.
func (s *AgentSession) dispatchExtensionHook(ctx context.Context, kind string, message llm.ConversationMessage, agentMessage agentmsg.Message, terminal llm.AssistantTerminal, event Event) {
	if s == nil {
		return
	}
	if s.hooks.Agent != nil {
		var lifecycle AgentLifecycleType
		switch kind {
		case "agent_start":
			lifecycle = AgentStartHookEvent
		case "agent_end":
			lifecycle = AgentEndHookEvent
		case "agent_settled":
			lifecycle = AgentSettledHookEvent
		}
		if lifecycle != "" {
			_ = s.hooks.Agent(ctx, AgentLifecycleEvent{Type: lifecycle, Messages: s.sessionCommittedAgentMessages(), Terminal: terminal})
		}
	}
	if s.hooks.Message != nil && agentMessage != nil && event.Kind != EventMessageCommitted {
		var messageType MessageHookType
		switch kind {
		case "message_start":
			messageType = MessageStartHookEvent
		case "message_update":
			messageType = MessageUpdateHookEvent
		}
		if messageType != "" {
			_, _ = s.hooks.Message(ctx, MessageHookEvent{Type: messageType, Message: agentmsg.CloneOne(agentMessage)})
		}
	}
}

func retrySucceededMessage(message llm.ConversationMessage) bool {
	terminal, ok := message.(llm.AssistantTerminal)
	if !ok {
		return false
	}
	return terminal.FinishReason() != llm.FinishError && terminal.FinishReason() != llm.FinishAborted
}

func retryFinalError(event Event) string {
	if event.RunError != nil {
		return event.RunError.Error()
	}
	if failure, ok := event.Terminal.(llm.AssistantFailureMessage); ok {
		return failure.ErrorMessage()
	}
	return ""
}

func (s *AgentSession) resetSessionTurn(_ Event) {
	s.lifecycleMu.Lock()
	if s.run != nil {
		s.run.assistantStarted = false
		s.run.assistantHookStarted = false
		s.run.toolResults = nil
	}
	s.lifecycleMu.Unlock()
}

func (s *AgentSession) beginAssistantMessage() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.run == nil || s.run.assistantStarted {
		return false
	}
	s.run.assistantStarted = true
	return true
}

func (s *AgentSession) beginAssistantHookMessage() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.run == nil || s.run.assistantHookStarted {
		return false
	}
	s.run.assistantHookStarted = true
	return true
}

func (s *AgentSession) recordCommitted(message llm.ConversationMessage, agentMessage agentmsg.Message, model provider.ModelRef) {
	if message == nil && agentMessage == nil {
		return
	}
	s.lifecycleMu.Lock()
	if s.run != nil {
		if agentMessage != nil {
			s.run.committedAgent = append(s.run.committedAgent, agentmsg.CloneOne(agentMessage))
		}
		if message != nil {
			s.run.committed = append(s.run.committed, message)
		}
		if message != nil && message.Role() == llm.RoleToolResult {
			s.run.toolResults = append(s.run.toolResults, message)
		}
		if message != nil && message.Role() == llm.RoleAssistant && model.ID() != "" {
			s.run.terminalModel = model
		}
	}
	s.lifecycleMu.Unlock()
}

func (s *AgentSession) sessionCommittedAgentMessages() []agentmsg.Message {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.run == nil {
		return nil
	}
	return agentmsg.Clone(s.run.committedAgent)
}

func (s *AgentSession) sessionTurnResults() []llm.ConversationMessage {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.run == nil {
		return nil
	}
	return append([]llm.ConversationMessage(nil), s.run.toolResults...)
}

func (s *AgentSession) sessionCommittedMessages() []llm.ConversationMessage {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.run == nil {
		return nil
	}
	return append([]llm.ConversationMessage(nil), s.run.committed...)
}

func (s *AgentSession) prepareTurn(ctx context.Context, _ TurnContext) (TurnSnapshot, error) {
	if s == nil {
		return TurnSnapshot{}, errors.New("nil agent session")
	}
	if err := s.rejectIfClosed(); err != nil {
		return TurnSnapshot{}, err
	}
	s.mu.RLock()
	snapshot := TurnSnapshot{
		Model: s.model, ThinkingLevel: s.thinkingLevel, SystemPrompt: s.systemPrompt,
		Tool: s.tool, Tools: append([]provider.ToolDefinition(nil), s.tools...), BeforeToolCall: s.beforeToolCall, AfterToolCall: s.afterToolCall, Stream: provider.CloneStreamOptions(s.stream),
	}
	resolver := s.resolveStream
	s.mu.RUnlock()
	s.lifecycleMu.Lock()
	if s.run != nil && s.run.extensionSystemPrompt != nil {
		snapshot.SystemPrompt = *s.run.extensionSystemPrompt
	}
	s.lifecycleMu.Unlock()
	if resolver != nil {
		resolved, err := resolver(ctx, snapshot.Model)
		if err != nil {
			return TurnSnapshot{}, fmt.Errorf("%w: resolve stream options: %w", ErrInvalidConfig, err)
		}
		// Resolver owns auth/header selection; preserve explicit per-session
		// operational options only when it leaves them unspecified.
		if resolved.MaxTokens == 0 {
			resolved.MaxTokens = snapshot.Stream.MaxTokens
		}
		if resolved.SessionID == "" {
			resolved.SessionID = snapshot.Stream.SessionID
		}
		snapshot.Stream = provider.CloneStreamOptions(resolved)
	}
	return snapshot, nil
}

func (s *AgentSession) State() SessionState {
	if s == nil {
		return SessionState{Active: State{phase: PhaseIdle}}
	}
	s.mu.RLock()
	state := SessionState{Model: s.model, ThinkingLevel: s.thinkingLevel, SystemPrompt: s.systemPrompt, Tools: append([]provider.ToolDefinition(nil), s.tools...)}
	s.mu.RUnlock()
	state.Active = s.activeState()
	return state
}

func (s *AgentSession) activeState() State {
	if s == nil {
		return State{phase: PhaseIdle}
	}
	s.lifecycleMu.Lock()
	run := s.run
	if run == nil {
		s.lifecycleMu.Unlock()
		return State{phase: PhaseIdle}
	}
	phase := run.phase
	s.lifecycleMu.Unlock()
	if s.loop != nil {
		state := s.loop.State()
		if state.Phase() != PhaseIdle {
			return state
		}
	}
	return State{phase: phase}
}

func (s *AgentSession) rejectIfClosed() error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	s.lifecycleMu.Lock()
	closed := s.closed || s.closing
	s.lifecycleMu.Unlock()
	if closed {
		return fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	return nil
}

type retryControl struct {
	attempt, max uint32
	delay        time.Duration
	succeeded    bool
	errorMessage string
	finalError   string
}

func (s *AgentSession) emitControl(ctx context.Context, kind string, retry ...retryControl) {
	if s == nil {
		return
	}
	s.mu.RLock()
	state := SessionState{Model: s.model, ThinkingLevel: s.thinkingLevel, SystemPrompt: s.systemPrompt, Tools: append([]provider.ToolDefinition(nil), s.tools...)}
	observers := make([]SessionObserver, 0, len(s.observers))
	for _, entry := range s.observers {
		if entry.observer != nil {
			observers = append(observers, entry.observer)
		}
	}
	s.mu.RUnlock()
	state.Active = s.activeState()
	if kind == "agent_settled" {
		state.Active = State{phase: PhaseIdle}
		if s.hooks.Agent != nil {
			_ = s.hooks.Agent(ctx, AgentLifecycleEvent{Type: AgentSettledHookEvent, Messages: s.sessionCommittedAgentMessages()})
		}
	}
	steering, follow := s.RichQueues()
	var retryEvent retryControl
	if len(retry) != 0 {
		retryEvent = retry[0]
	}
	s.emitToObservers(ctx, observers, SessionEvent{
		Type: kind, State: state, Steering: steering, FollowUp: follow,
		RetryAttempt: retryEvent.attempt, RetryMaxAttempts: retryEvent.max, RetryDelay: retryEvent.delay,
		RetrySucceeded: retryEvent.succeeded, RetryErrorMessage: retryEvent.errorMessage, FinalError: retryEvent.finalError,
	})
}

func (s *AgentSession) Model() provider.ModelRef              { return s.State().Model }
func (s *AgentSession) ThinkingLevel() provider.ThinkingLevel { return s.State().ThinkingLevel }
func (s *AgentSession) SystemPrompt() string                  { return s.State().SystemPrompt }
func (s *AgentSession) Tools() []provider.ToolDefinition      { return s.State().Tools }
func (s *AgentSession) Transcript() Transcript {
	if s == nil {
		return nil
	}
	return s.transcript
}
func (s *AgentSession) SetModel(model provider.ModelRef) error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if _, err := provider.NewRequest(model, "", nil); err != nil {
		return fmt.Errorf("%w: model: %w", ErrInvalidConfig, err)
	}
	if routes, ok := s.loop.config.provider.(provider.RouteValidator); ok && !routes.SupportsModel(model) {
		return fmt.Errorf("%w: no provider adapter for %s/%s", ErrInvalidConfig, model.Provider(), model.API())
	}
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	s.mu.Lock()
	s.model = model
	s.thinkingLevel = model.ClampThinkingLevel(s.thinkingLevel)
	s.mu.Unlock()
	s.lifecycleMu.Unlock()
	s.emitControl(context.Background(), "model_changed")
	return nil
}

func (s *AgentSession) SetThinkingLevel(level provider.ThinkingLevel) error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if !level.Valid() {
		return fmt.Errorf("%w: invalid thinking level %q", ErrInvalidConfig, level)
	}
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	s.mu.Lock()
	s.thinkingLevel = s.model.ClampThinkingLevel(level)
	s.mu.Unlock()
	s.lifecycleMu.Unlock()
	s.emitControl(context.Background(), "thinking_level_changed")
	return nil
}

func (s *AgentSession) SetSystemPrompt(prompt string) error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if !utf8.ValidString(prompt) {
		return fmt.Errorf("%w: system prompt is not valid UTF-8", ErrInvalidConfig)
	}
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	s.mu.Lock()
	s.systemPrompt = prompt
	s.mu.Unlock()
	s.lifecycleMu.Unlock()
	return nil
}

func (s *AgentSession) SetTools(executor ToolExecutor, tools []provider.ToolDefinition) error {
	if s == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	state := s.State()
	if _, err := provider.NewRequestWithOptions(state.Model, state.SystemPrompt, nil, provider.RequestOptions{Tools: tools, ThinkingLevel: state.ThinkingLevel}); err != nil {
		return fmt.Errorf("%w: tools: %w", ErrInvalidConfig, err)
	}
	if len(tools) != 0 && isNilInterface(executor) {
		return fmt.Errorf("%w: advertised tools require a non-nil executor", ErrInvalidConfig)
	}
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	s.mu.Lock()
	s.tool = executor
	s.tools = append([]provider.ToolDefinition(nil), tools...)
	s.mu.Unlock()
	s.lifecycleMu.Unlock()
	return nil
}

func (s *AgentSession) Run(ctx context.Context, prompt string) (Result, error) {
	if err := s.rejectIfClosed(); err != nil {
		return Result{}, err
	}
	if s.loop == nil {
		return Result{}, fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	return s.runSession(ctx, true, prompt, func(run context.Context, extra []agentmsg.Message) (Result, error) {
		return s.loop.runWithAgentMessages(run, prompt, extra)
	})
}
func (s *AgentSession) Prompt(ctx context.Context, prompt string) (Result, error) {
	return s.Run(ctx, prompt)
}

// RunContent is the rich-input counterpart to Run. Agent owns the initial
// append, so rich prompts pass through exactly the same beginRun/runV2/commit
// lifecycle as text prompts.
func (s *AgentSession) RunContent(ctx context.Context, content []llm.UserContentBlock) (Result, error) {
	if err := s.rejectIfClosed(); err != nil {
		return Result{}, err
	}
	if s.loop == nil {
		return Result{}, fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	return s.runSession(ctx, true, "", func(run context.Context, extra []agentmsg.Message) (Result, error) {
		return s.loop.runContentWithAgentMessages(run, content, extra)
	})
}
func (s *AgentSession) PromptContent(ctx context.Context, content []llm.UserContentBlock) (Result, error) {
	return s.RunContent(ctx, content)
}

func (s *AgentSession) runSession(ctx context.Context, prePromptCheck bool, prompt string, begin func(context.Context, []agentmsg.Message) (Result, error)) (result Result, runErr error) {
	run, err := s.admitSessionRun(ctx)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		s.setSessionPhase(run, PhaseSettling)
		if s.sessionRunStarted(run) {
			s.emitControl(run.ctx, "agent_settled")
		}
		s.finishSessionRun(run)
	}()
	var extra []agentmsg.Message
	if hook := s.hooks.BeforeAgentStart; prePromptCheck && hook != nil {
		state := s.State()
		out, hookErr := hook(run.ctx, BeforeAgentStartEvent{Prompt: prompt, SystemPrompt: state.SystemPrompt, Messages: s.runtimeTranscript.Context().AgentMessages()})
		if hookErr != nil {
			return Result{}, hookErr
		}
		if err := out.Cancel.Validate(); err != nil {
			return Result{}, err
		}
		if out.Cancel.Cancelled() {
			if out.Cancel.Reason == "" {
				return Result{}, ErrAgentAborted
			}
			return Result{}, fmt.Errorf("%w: %s", ErrAgentAborted, out.Cancel.Reason)
		}
		if out.SystemPrompt != nil {
			value := *out.SystemPrompt
			s.lifecycleMu.Lock()
			run.extensionSystemPrompt = &value
			s.lifecycleMu.Unlock()
		}
		extra = agentmsg.Clone(out.ExtraMessages)
	}

	if prePromptCheck {
		s.checkPrePromptCompaction(run)
		s.setSessionPhase(run, PhaseProvider)
	}
	result, runErr = begin(run.ctx, extra)
	for runErr == nil {
		if cause := context.Cause(run.ctx); cause != nil {
			s.endRetrySeries(run.ctx, false, cause.Error())
			// Cancellation after admission is represented by a settled terminal
			// result, never a Go-level lifecycle error.
			return result, nil
		}
		if s.retryableResult(result) && run.retryAttempt+1 < s.retry.MaxAttempts() {
			run.retryAttempt++
			nextAttempt := run.retryAttempt + 1
			failure := providerFailureFromTerminalForSession(result)
			delay := s.retry.Delay(nextAttempt, failure)
			errorMessage := retryErrorMessage(result)
			s.beginRetrySeries(run, delay, errorMessage)
			s.setSessionPhase(run, PhaseRetryWait)
			s.emitControl(run.ctx, "auto_retry_start", retryControl{
				attempt: run.retryAttempt, max: s.maxRetries(), delay: delay, errorMessage: errorMessage,
			})
			// The failed assistant turn is durable history but must not be sent
			// back to the provider when resending this attempt.
			s.runtimeTranscript.removeLastFailure()
			if waitErr := s.retry.Wait(run.ctx, delay); waitErr != nil {
				s.endRetrySeries(run.ctx, false, waitErr.Error())
				return result, nil
			}
			s.setSessionPhase(run, PhaseProvider)
			result, runErr = s.loop.Continue(run.ctx)
			continue
		}
		if s.checkPostRunCompaction(run, result) {
			s.setSessionPhase(run, PhaseProvider)
			result, runErr = s.loop.Continue(run.ctx)
			continue
		}
		s.resetRetryState(run)
		// agent_end observers run synchronously before the low run returns. A
		// newly queued message is therefore visible here and gets its own low
		// continuation while the top-level admission remains held.
		steering, follow := s.RichQueues()
		if len(steering) == 0 && len(follow) == 0 {
			return result, runErr
		}
		s.setSessionPhase(run, PhaseProvider)
		result, runErr = s.loop.Continue(run.ctx)
	}
	return result, runErr
}

func retryErrorMessage(result Result) string {
	terminal, ok := result.Terminal()
	if !ok {
		return ""
	}
	if failure, ok := terminal.(llm.AssistantFailureMessage); ok {
		return failure.ErrorMessage()
	}
	return ""
}

func (s *AgentSession) beginRetrySeries(run *sessionRun, delay time.Duration, errorMessage string) {
	s.lifecycleMu.Lock()
	if s.run == run {
		run.retrySeries = true
		run.retryDelay = delay
		run.retryError = errorMessage
	}
	s.lifecycleMu.Unlock()
}

// endRetrySeries emits at most once. Successful dispatch calls this from the
// assistant commit event; failed dispatch calls it after the final agent_end.
func (s *AgentSession) endRetrySeries(ctx context.Context, succeeded bool, finalError string) {
	s.lifecycleMu.Lock()
	run := s.run
	if run == nil || !run.retrySeries {
		s.lifecycleMu.Unlock()
		return
	}
	payload := retryControl{
		attempt: run.retryAttempt, max: s.maxRetries(), delay: run.retryDelay,
		succeeded: succeeded, errorMessage: run.retryError, finalError: finalError,
	}
	run.retrySeries = false
	s.lifecycleMu.Unlock()
	s.emitControl(ctx, "auto_retry_end", payload)
}

func (s *AgentSession) resetRetryState(run *sessionRun) {
	s.lifecycleMu.Lock()
	if s.run == run {
		run.retryAttempt = 0
		run.retryDelay = 0
		run.retryError = ""
	}
	s.lifecycleMu.Unlock()
}

// maxRetries uses the product-facing retry budget: the initial request is not
// a retry. Provider RetryController counts the initial request in MaxAttempts.
func (s *AgentSession) maxRetries() uint32 {
	if s == nil {
		return 0
	}
	return retryBudget(s.retry.MaxAttempts())
}

func retryBudget(maxAttempts uint32) uint32 {
	if maxAttempts == 0 {
		return 0
	}
	return maxAttempts - 1
}

func (s *AgentSession) checkPrePromptCompaction(run *sessionRun) {
	messages := s.runtimeTranscript.Context().Messages()
	if len(messages) == 0 {
		return
	}
	terminal, ok := messages[len(messages)-1].(llm.AssistantTerminal)
	if !ok {
		return
	}
	provenance, hasProvenance := s.runtimeTranscript.AssistantProvenance()
	currentModel := s.State().Model
	if !hasProvenance || provenance.Provider != currentModel.Provider() || provenance.Model != currentModel.ID() {
		return
	}
	// Pre-prompt policy deliberately never continues: the pending user prompt
	// is about to create the next low run itself.
	_ = s.checkCompaction(run, terminal, false)
}

// checkPostRunCompaction returns true only when overflow recovery compacted
// successfully and the caller must Continue from the refreshed runtime
// transcript. Threshold/successful-over-window compaction never fabricates a
// provider continuation.
func (s *AgentSession) checkPostRunCompaction(run *sessionRun, result Result) bool {
	terminal, ok := result.Terminal()
	if !ok {
		return false
	}
	return s.checkCompaction(run, terminal, true)
}

func (s *AgentSession) checkCompaction(run *sessionRun, terminal llm.AssistantTerminal, skipAborted bool) bool {
	if s == nil || s.contextWindow == 0 || s.compactor == nil || s.summarizer == nil || terminal == nil {
		return false
	}
	if skipAborted && terminal.FinishReason() == llm.FinishAborted {
		return false
	}
	terminalModel := s.terminalRunModel(run)
	currentModel := s.State().Model
	if terminalModel.ID() != "" && !sameModelRef(terminalModel, currentModel) {
		return false
	}
	if terminalModel.ID() == "" {
		terminalModel = currentModel
	}
	window, reserve := s.compactionLimitsFor(terminalModel)
	if window == 0 {
		return false
	}
	if failure := providerFailureFromTerminalForSession(Result{terminal: terminal}); failure != nil && failure.Kind() == provider.FailureContextOverflow {
		s.lifecycleMu.Lock()
		already := run.overflowCompacted
		if !already && s.run == run {
			run.overflowCompacted = true
		}
		s.lifecycleMu.Unlock()
		if already {
			return false
		}
		// Preserve the failed assistant durably, but never include it in the
		// retry context. Compact refreshes the runtime projection on success.
		s.runtimeTranscript.removeLastFailure()
		if s.runCompaction(run, CompactionContextOverflow, true, "") {
			return true
		}
		return false
	}
	if terminal.FinishReason() != llm.FinishError && terminal.Usage().TotalTokens() > window {
		_ = s.runCompaction(run, CompactionContextOverflow, false, "")
		return false
	}

	compact, err := session.ShouldCompact(s.runtimeTranscript.Context().Messages(), window, reserve)
	if err != nil || !compact {
		return false
	}
	_ = s.runCompaction(run, CompactionThreshold, false, "")
	return false
}

func (s *AgentSession) terminalRunModel(run *sessionRun) provider.ModelRef {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.run != run {
		return provider.ModelRef{}
	}
	return run.terminalModel
}

func sameModelRef(left, right provider.ModelRef) bool {
	return left.Provider() == right.Provider() && left.ID() == right.ID()
}

func (s *AgentSession) compactionLimitsFor(model provider.ModelRef) (uint64, uint64) {
	s.mu.RLock()
	window, reserve := s.contextWindow, s.contextReserve
	s.mu.RUnlock()
	if model.ContextWindow() != 0 {
		window = model.ContextWindow()
	}
	if model.MaxTokens() != 0 {
		reserve = model.MaxTokens()
	}
	return window, reserve
}

// runCompaction returns true only on a committed compaction. Automatic
// failures are surfaced as compaction_end and leave the surrounding request's
// settled result intact.
func (s *AgentSession) runCompaction(run *sessionRun, reason CompactionReason, willRetry bool, instructions string) bool {
	if s == nil || s.runtimeTranscript == nil || s.summarizer == nil {
		return false
	}
	var proceed bool
	var hookErr error
	instructions, proceed, hookErr = s.beforeCompaction(run.ctx, reason, willRetry, instructions)
	if !proceed {
		if hookErr != nil {
			s.emitCompaction(run.ctx, "compaction_end", reason, nil, false, false, hookErr.Error())
		}
		return false
	}
	s.setSessionPhase(run, PhaseCompacting)
	s.emitCompaction(run.ctx, "compaction_start", reason, nil, false, willRetry, "")
	result, err := s.runtimeTranscript.Compact(run.ctx, session.CompactRequest{
		KeepRecentTokens: s.keepRecentTokens, Instructions: instructions,
		Summarizer: sessionObservedSummarizer{session: s, run: run, reason: reason, base: s.summarizer},
	})
	if err != nil {
		aborted := context.Cause(run.ctx) != nil || errors.Is(err, session.ErrAppendCanceled)
		s.emitCompaction(run.ctx, "compaction_end", reason, nil, aborted, false, err.Error())
		return false
	}
	if willRetry {
		// A retained-tail policy may keep the durable overflow failure beside the
		// new checkpoint. It remains in history, but never belongs to resend
		// context.
		s.runtimeTranscript.removeLastFailure()
	}
	s.emitCompaction(run.ctx, "compaction_end", reason, &result, false, willRetry, "")
	s.afterCompaction(run.ctx, reason, willRetry)
	return true
}

func (s *AgentSession) beforeCompaction(ctx context.Context, reason CompactionReason, willRetry bool, instructions string) (string, bool, error) {
	hook := s.hooks.SessionCompact
	if hook == nil {
		return instructions, true, nil
	}
	branch := []session.Entry(nil)
	if durable, ok := s.transcript.(*session.Session); ok {
		branch = durable.BranchPath()
	}
	result, err := hook(ctx, SessionCompactHookEvent{Before: true, Reason: reason, WillRetry: willRetry, Branch: branch, Instructions: instructions})
	if err != nil {
		return instructions, false, err
	}
	if result.Cancel.Cancelled() {
		return instructions, false, nil
	}
	if result.Instructions != nil {
		return *result.Instructions, true, nil
	}
	return instructions, true, nil
}
func (s *AgentSession) afterCompaction(ctx context.Context, reason CompactionReason, willRetry bool) {
	if hook := s.hooks.SessionCompact; hook != nil {
		branch := []session.Entry(nil)
		if durable, ok := s.transcript.(*session.Session); ok {
			branch = durable.BranchPath()
		}
		_, _ = hook(ctx, SessionCompactHookEvent{Before: false, Reason: reason, WillRetry: willRetry, Branch: branch})
	}
}

type sessionObservedSummarizer struct {
	session *AgentSession
	run     *sessionRun
	reason  CompactionReason
	base    session.Summarizer
}

func (s sessionObservedSummarizer) Summarize(ctx context.Context, input session.SummaryInput) (session.SummaryOutput, error) {
	observable, ok := s.base.(summarizerWithRetryObserver)
	if !ok {
		return s.base.Summarize(ctx, input)
	}
	return observable.SummarizeWithRetryObserver(ctx, input, func(_ context.Context, retry provider.RetryEvent) {
		s.session.emitSummarizationRetry(s.run.ctx, s.reason, retry)
	})
}

func (s *AgentSession) emitSummarizationRetry(ctx context.Context, reason CompactionReason, retry provider.RetryEvent) {
	kind := "summarization_retry_scheduled"
	switch retry.Kind {
	case provider.RetryAttempt:
		kind = "summarization_retry_attempt_start"
	case provider.RetryFinished:
		kind = "summarization_retry_finished"
	}
	s.emitCompactionRetry(ctx, kind, reason, retry)
}

func (s *AgentSession) emitCompaction(ctx context.Context, kind string, reason CompactionReason, result *session.CompactResult, aborted, willRetry bool, errorMessage string) {
	if s == nil {
		return
	}
	s.mu.RLock()
	state := SessionState{Model: s.model, ThinkingLevel: s.thinkingLevel, SystemPrompt: s.systemPrompt, Tools: append([]provider.ToolDefinition(nil), s.tools...)}
	observers := make([]SessionObserver, 0, len(s.observers))
	for _, entry := range s.observers {
		if entry.observer != nil {
			observers = append(observers, entry.observer)
		}
	}
	s.mu.RUnlock()
	state.Active = s.activeState()
	steering, follow := s.RichQueues()
	s.emitToObservers(ctx, observers, SessionEvent{
		Type: kind, State: state, Steering: steering, FollowUp: follow,
		CompactionReason: reason, CompactionResult: result, CompactionAborted: aborted,
		CompactionWillRetry: willRetry, CompactionErrorMessage: errorMessage,
	})
}

func (s *AgentSession) emitCompactionRetry(ctx context.Context, kind string, reason CompactionReason, retry provider.RetryEvent) {
	if s == nil {
		return
	}
	s.mu.RLock()
	state := SessionState{Model: s.model, ThinkingLevel: s.thinkingLevel, SystemPrompt: s.systemPrompt, Tools: append([]provider.ToolDefinition(nil), s.tools...)}
	observers := make([]SessionObserver, 0, len(s.observers))
	for _, entry := range s.observers {
		if entry.observer != nil {
			observers = append(observers, entry.observer)
		}
	}
	s.mu.RUnlock()
	state.Active = s.activeState()
	steering, follow := s.RichQueues()
	maxRetries := s.maxRetries()
	if retry.MaxAttempts != 0 {
		maxRetries = retryBudget(retry.MaxAttempts)
	}
	s.emitToObservers(ctx, observers, SessionEvent{
		Type: kind, State: state, Steering: steering, FollowUp: follow,
		SummarizationSource: "compaction", CompactionReason: reason,
		RetryAttempt: retryBudget(retry.Attempt), RetryMaxAttempts: maxRetries, RetryDelay: retry.Delay,
		RetryErrorMessage: retry.ErrorMessage, RetryFailureKind: retry.FailureKind,
		RetryHTTPStatus: retry.HTTPStatus, RetrySucceeded: retry.Succeeded,
		RetryFinishReason: retry.FinishReason, FinalError: retry.FinalError,
	})
}

// emitToObservers gives every observer a fresh event. Session observers are
// user callbacks, so a mutation by one must never affect another observer or
// the coordinator's retained state.
func (s *AgentSession) emitToObservers(ctx context.Context, observers []SessionObserver, event SessionEvent) {
	for _, observer := range observers {
		observer(ctx, cloneSessionEvent(event))
	}
}

func cloneSessionEvent(event SessionEvent) SessionEvent {
	event.State.Tools = append([]provider.ToolDefinition(nil), event.State.Tools...)
	event.Steering = append([]llm.ConversationMessage(nil), event.Steering...)
	event.FollowUp = append([]llm.ConversationMessage(nil), event.FollowUp...)
	event.ToolResults = append([]llm.ConversationMessage(nil), event.ToolResults...)
	event.Messages = append([]llm.ConversationMessage(nil), event.Messages...)
	event.AgentMessage = agentmsg.CloneOne(event.AgentMessage)
	event.AgentMessages = agentmsg.Clone(event.AgentMessages)
	event.Event.AgentMessage = agentmsg.CloneOne(event.Event.AgentMessage)
	event.Event.ToolArguments = bytes.Clone(event.Event.ToolArguments)
	event.Event.ToolOutput.Content = append([]llm.ToolResultContentBlock(nil), event.Event.ToolOutput.Content...)
	event.Event.ToolOutput.AddedToolNames = append([]string(nil), event.Event.ToolOutput.AddedToolNames...)
	if event.Event.ToolOutput.Usage != nil {
		usage := *event.Event.ToolOutput.Usage
		event.Event.ToolOutput.Usage = &usage
	}
	event.Event.ToolUpdate.Content = append([]llm.ToolResultContentBlock(nil), event.Event.ToolUpdate.Content...)
	event.Event.ToolUpdate.AddedToolNames = append([]string(nil), event.Event.ToolUpdate.AddedToolNames...)
	if event.Event.ToolUpdate.Usage != nil {
		usage := *event.Event.ToolUpdate.Usage
		event.Event.ToolUpdate.Usage = &usage
	}
	if event.CompactionResult != nil {
		result := *event.CompactionResult
		result.Input.Messages = append([]llm.ConversationMessage(nil), result.Input.Messages...)
		result.Input.RetainedTail = append([]llm.ConversationMessage(nil), result.Input.RetainedTail...)
		event.CompactionResult = &result
	}
	return event
}

func (s *AgentSession) sessionRunStarted(run *sessionRun) bool {
	s.lifecycleMu.Lock()
	started := s.run == run && run.started
	s.lifecycleMu.Unlock()
	return started
}

func (s *AgentSession) admitSessionRun(ctx context.Context) (*sessionRun, error) {
	if s == nil || ctx == nil || context.Cause(ctx) != nil {
		return nil, fmt.Errorf("%w: invalid session context", ErrInvalidRun)
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closed || s.closing {
		return nil, fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	if s.run != nil {
		return nil, ErrBusy
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	run := &sessionRun{ctx: runCtx, cancel: cancel, done: make(chan struct{}), phase: PhaseProvider}
	s.run = run
	return run, nil
}

func (s *AgentSession) setSessionPhase(run *sessionRun, phase Phase) {
	s.lifecycleMu.Lock()
	if s.run == run {
		run.phase = phase
	}
	s.lifecycleMu.Unlock()
}

func (s *AgentSession) finishSessionRun(run *sessionRun) {
	s.lifecycleMu.Lock()
	if s.run == run {
		s.run = nil
		run.cancel(context.Canceled)
		close(run.done)
	}
	s.lifecycleMu.Unlock()
}

func providerFailureFromTerminalForSession(result Result) *provider.ProviderFailure {
	terminal, ok := result.Terminal()
	if !ok || terminal.FinishReason() != llm.FinishError {
		return nil
	}
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
func (s *AgentSession) retryableResult(result Result) bool {
	return provider.IsTransientFailure(providerFailureFromTerminalForSession(result))
}
func (s *AgentSession) willRetry(terminal llm.AssistantTerminal) bool {
	if terminal == nil || terminal.FinishReason() != llm.FinishError {
		return false
	}
	result := Result{terminal: terminal}
	s.lifecycleMu.Lock()
	run := s.run
	will := run != nil && run.retryAttempt+1 < s.retry.MaxAttempts() && s.retryableResult(result)
	s.lifecycleMu.Unlock()
	return will
}
func (s *AgentSession) Continue(ctx context.Context) (Result, error) {
	if err := s.rejectIfClosed(); err != nil {
		return Result{}, err
	}
	if s.loop == nil {
		return Result{}, fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	return s.runSession(ctx, false, "", func(run context.Context, _ []agentmsg.Message) (Result, error) {
		return s.loop.Continue(run)
	})
}
func (s *AgentSession) Steer(prompt string) error {
	return s.enqueueText(prompt, true)
}
func (s *AgentSession) SteerContent(content []llm.UserContentBlock) error {
	return s.enqueueContent(content, true)
}
func (s *AgentSession) FollowUp(prompt string) error {
	return s.enqueueText(prompt, false)
}
func (s *AgentSession) FollowUpContent(content []llm.UserContentBlock) error {
	return s.enqueueContent(content, false)
}

func (s *AgentSession) enqueueText(prompt string, steering bool) error {
	if s == nil || s.loop == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	timestamp, err := s.loop.now() // injected clock stays outside lifecycleMu
	if err != nil {
		return err
	}
	message, err := llm.NewUserTextMessage(prompt, timestamp)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidQueueMessage, err)
	}
	return s.enqueueMessage(message, steering)
}

func (s *AgentSession) enqueueContent(content []llm.UserContentBlock, steering bool) error {
	if s == nil || s.loop == nil {
		return fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	timestamp, err := s.loop.now() // injected clock stays outside lifecycleMu
	if err != nil {
		return err
	}
	message, err := llm.NewUserContentMessage(content, timestamp)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidQueueMessage, err)
	}
	return s.enqueueMessage(message, steering)
}

func (s *AgentSession) enqueueMessage(message llm.ConversationMessage, steering bool) error {
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return fmt.Errorf("%w: session is closed", ErrInvalidRun)
	}
	// enqueueMessage only takes Agent.mu and invokes no clock/provider/tool or
	// observer callback, so it is safe to make close/admission atomic here.
	err := s.loop.enqueueMessage(message, steering)
	s.lifecycleMu.Unlock()
	if err == nil {
		s.emitControl(context.Background(), "queue_update")
	}
	return err
}
func (s *AgentSession) Queues() (steering, followUp []llm.UserTextMessage) {
	if s == nil || s.loop == nil {
		return nil, nil
	}
	return s.loop.Queues()
}
func (s *AgentSession) RichQueues() (steering, followUp []llm.ConversationMessage) {
	if s == nil || s.loop == nil {
		return nil, nil
	}
	return s.loop.RichQueues()
}
func (s *AgentSession) ClearSteeringQueue() {
	s.clearQueues(func() { s.loop.ClearSteeringQueue() })
}
func (s *AgentSession) ClearFollowUpQueue() {
	s.clearQueues(func() { s.loop.ClearFollowUpQueue() })
}
func (s *AgentSession) ClearAllQueues() {
	s.clearQueues(func() { s.loop.ClearAllQueues() })
}

func (s *AgentSession) clearQueues(clear func()) {
	if s == nil || s.loop == nil {
		return
	}
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return
	}
	beforeSteering, beforeFollowUp := s.RichQueues()
	clear()
	afterSteering, afterFollowUp := s.RichQueues()
	s.lifecycleMu.Unlock()
	if !sameMessages(beforeSteering, afterSteering) || !sameMessages(beforeFollowUp, afterFollowUp) {
		s.emitControl(context.Background(), "queue_update")
	}
}
func (s *AgentSession) Abort(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.lifecycleMu.Lock()
	run := s.run
	if run != nil {
		run.cancel(ErrAgentAborted)
	}
	s.lifecycleMu.Unlock()
	if run == nil {
		return nil
	}
	select {
	case <-run.done:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}
func (s *AgentSession) WaitForIdle(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.lifecycleMu.Lock()
	run := s.run
	s.lifecycleMu.Unlock()
	if run == nil {
		return nil
	}
	select {
	case <-run.done:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}
func (s *AgentSession) Subscribe(observer SessionObserver) func() {
	if s == nil || observer == nil {
		return func() {}
	}
	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.lifecycleMu.Unlock()
		return func() {}
	}
	s.mu.Lock()
	s.nextObserver++
	id := s.nextObserver
	s.observers = append(s.observers, sessionObserverEntry{id: id, observer: observer})
	s.mu.Unlock()
	s.lifecycleMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			for index := range s.observers {
				if s.observers[index].id == id {
					copy(s.observers[index:], s.observers[index+1:])
					s.observers[len(s.observers)-1] = sessionObserverEntry{}
					s.observers = s.observers[:len(s.observers)-1]
					break
				}
			}
			s.mu.Unlock()
		})
	}
}

// Close first settles/cancels work, then detaches event delivery. It is safe
// to call repeatedly and must run before an owning transcript is closed.
func (s *AgentSession) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.lifecycleMu.Lock()
	if s.closed {
		s.lifecycleMu.Unlock()
		return nil
	}
	s.closing = true
	s.lifecycleMu.Unlock()
	err := s.Abort(ctx)
	if err == nil {
		err = s.WaitForIdle(ctx)
	}
	if err != nil {
		return err
	}
	if s.hooks.SessionShutdown != nil {
		if hookErr := s.hooks.SessionShutdown(ctx, SessionShutdownHookEvent{Reason: ShutdownQuit}); hookErr != nil {
			s.lifecycleMu.Lock()
			s.closing = false
			s.lifecycleMu.Unlock()
			return hookErr
		}
	}
	s.lifecycleMu.Lock()
	s.closed = true
	s.closing = false
	s.lifecycleMu.Unlock()
	s.mu.Lock()
	unsubscribe := s.loopUnsubscribe
	s.loopUnsubscribe = nil
	s.observers = nil
	s.mu.Unlock()
	if unsubscribe != nil {
		unsubscribe()
	}
	return err
}

func cloneHeaderMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copy := make(map[string]string, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}
func (s *AgentSession) Compact(ctx context.Context, instructions string) (session.CompactResult, error) {
	if s == nil {
		return session.CompactResult{}, fmt.Errorf("%w: nil agent session", ErrInvalidRun)
	}
	if err := s.rejectIfClosed(); err != nil {
		return session.CompactResult{}, err
	}
	if s.runtimeTranscript == nil || s.summarizer == nil || s.compactor == nil {
		return session.CompactResult{}, ErrCompactionUnavailable
	}
	run, err := s.admitSessionRun(ctx)
	if err != nil {
		return session.CompactResult{}, err
	}
	defer s.finishSessionRun(run)
	var proceed bool
	var hookErr error
	instructions, proceed, hookErr = s.beforeCompaction(run.ctx, CompactionManual, false, instructions)
	if hookErr != nil {
		return session.CompactResult{}, hookErr
	}
	if !proceed {
		return session.CompactResult{}, ErrAgentAborted
	}
	s.setSessionPhase(run, PhaseCompacting)
	s.emitCompaction(run.ctx, "compaction_start", CompactionManual, nil, false, false, "")
	result, err := s.runtimeTranscript.Compact(run.ctx, session.CompactRequest{
		KeepRecentTokens: s.keepRecentTokens, Instructions: instructions,
		Summarizer: sessionObservedSummarizer{session: s, run: run, reason: CompactionManual, base: s.summarizer},
	})
	if err != nil {
		aborted := context.Cause(run.ctx) != nil || errors.Is(err, session.ErrAppendCanceled)
		s.emitCompaction(run.ctx, "compaction_end", CompactionManual, nil, aborted, false, err.Error())
		return session.CompactResult{}, err
	}
	s.emitCompaction(run.ctx, "compaction_end", CompactionManual, &result, false, false, "")
	s.afterCompaction(run.ctx, CompactionManual, false)
	return result, nil
}

// SelectLeaf exposes the current Session tree navigation boundary without
// teaching Agent about JSONL. Extensions may cancel before selection and are
// notified after the new branch becomes active.
func (s *AgentSession) SelectLeaf(ctx context.Context, id string) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil tree context", ErrInvalidRun)
	}
	if err := s.rejectIfClosed(); err != nil {
		return err
	}
	durable, ok := s.transcript.(*session.Session)
	if !ok {
		return fmt.Errorf("%w: transcript has no session tree", ErrInvalidConfig)
	}
	run, err := s.admitSessionRun(ctx)
	if err != nil {
		return err
	}
	defer s.finishSessionRun(run)
	old, _ := durable.LeafID()
	branch := durable.BranchPath()
	if hook := s.hooks.SessionTree; hook != nil {
		result, err := hook(run.ctx, SessionTreeHookEvent{Before: true, OldLeafID: old, NewLeafID: id, Branch: branch})
		if err != nil {
			return err
		}
		if result.Cancel.Cancelled() {
			return ErrAgentAborted
		}
	}
	if err := durable.SelectLeaf(id); err != nil {
		return err
	}
	refreshed := durable.BuildContext()
	s.runtimeTranscript.mu.Lock()
	s.runtimeTranscript.messages = refreshed.Messages()
	s.runtimeTranscript.agentMessages = refreshed.AgentMessages()
	s.runtimeTranscript.assistant, s.runtimeTranscript.hasAssistant = refreshed.AssistantProvenance()
	s.runtimeTranscript.mu.Unlock()
	if hook := s.hooks.SessionTree; hook != nil {
		_, _ = hook(run.ctx, SessionTreeHookEvent{Before: false, OldLeafID: old, NewLeafID: id, Branch: durable.BranchPath()})
	}
	return nil
}

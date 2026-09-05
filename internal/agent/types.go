// Package agent coordinates one provider/tool/session run without owning any
// of those modules' implementation details or durable formats.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

const defaultSettlementTimeout = 30 * time.Second

var (
	ErrInvalidConfig            = errors.New("invalid agent configuration")
	ErrInvalidRun               = errors.New("invalid agent run")
	ErrBusy                     = errors.New("agent is already running")
	ErrRunIDExhausted           = errors.New("agent run id exhausted")
	ErrTranscriptCommit         = errors.New("agent transcript commit failed")
	ErrInvariant                = errors.New("agent invariant failure")
	ErrProviderStream           = errors.New("provider stream failed")
	ErrToolNotFound             = errors.New("tool not found")
	ErrTruncatedToolCall        = errors.New("tool call arguments may be truncated")
	ErrToolUnsettled            = errors.New("tool returned an unsettled outcome")
	ErrAgentAborted             = errors.New("agent run aborted")
	ErrContextTransform         = errors.New("agent context transform failed")
	ErrInvalidQueueMessage      = errors.New("invalid queued message")
	ErrCannotContinue           = errors.New("agent cannot continue from current transcript")
	ErrNoModelSelected          = errors.New("No model selected.")
	ErrModelAccess              = errors.New("model access unavailable")
	ErrCompactionUnavailable    = errors.New("agent compaction is not configured")
	ErrBranchSummaryUnavailable = errors.New("agent branch summarization is not configured")
	ErrBashUnavailable          = errors.New("agent standalone bash is not configured")
	ErrInvalidExtensionResult   = errors.New("invalid extension hook result")
	ErrRetryPolicy              = provider.ErrInvalidRetryPolicy
	// ErrUnsupportedToolTurn is retained for source compatibility with the
	// v0.1 internal implementation; v0.2 no longer returns it for batches.
	ErrUnsupportedToolTurn = errors.New("unsupported tool turn")
)

// ModelAccessError preserves product-facing authentication guidance while
// allowing hosts to distinguish admission failures from internal run errors.
type ModelAccessError struct{ Message string }

func (e *ModelAccessError) Error() string {
	if e == nil || e.Message == "" {
		return ErrModelAccess.Error()
	}
	return e.Message
}

func (*ModelAccessError) Is(target error) bool { return target == ErrModelAccess }

// Transcript is the narrow persistence seam used by Agent-core integrations.
// Product AgentSession does not accept this seam; it requires and owns a real
// session.SessionManager.
type Transcript interface {
	Context() session.Context
	Append(context.Context, llm.ConversationMessage, session.AppendOptions) (session.Entry, error)
}

// AgentMessageTranscript is the durable extension of Transcript used for
// non-LLM AgentMessage union members. Agent only requires it when a caller
// actually injects such a message.
type AgentMessageTranscript interface {
	AppendAgentMessage(context.Context, agentmsg.Message, session.AppendOptions) (session.Entry, error)
}

// RetryPolicy is shared with provider.ContextSummarizer so both request paths
// use identical attempt, Retry-After, jitter, cap, and cancellation semantics.
type RetryPolicy = provider.RetryPolicy

// ToolOutput is the provider-visible final text returned by one tool. A
// non-nil Execute error makes the associated ToolResult an error result.
type ToolOutput struct {
	Text string
	// Content is the provider-visible rich result.  Text remains the legacy
	// convenience surface; when Content is present it is authoritative and is
	// never flattened by the loop. Details is encoded as immutable opaque JSON
	// on the durable tool-result message, but remains excluded from provider
	// request payloads.
	Content []llm.ToolResultContentBlock
	Details any
	// Usage and AddedToolNames are retained exactly as pi's AgentToolResult.
	// They are not part of the main model token accounting; deferred-tool-aware
	// adapters consume AddedToolNames at the ToolResult boundary.
	Usage          *llm.Usage
	AddedToolNames []string
	// Terminate asks the coordinator to stop after this batch. A batch stops
	// early only when every finalized call asks to terminate; this prevents a
	// concurrent success from silently hiding another call's continuation.
	Terminate bool
}

type BeforeToolCallContext struct {
	Assistant llm.AssistantToolUseMessage
	ToolCall  llm.ToolCallBlock
	Arguments []byte
	Context   []llm.ConversationMessage
}
type BeforeToolCallResult struct {
	Block     bool
	Reason    string
	Arguments *json.RawMessage
}
type AfterToolCallContext struct {
	Assistant llm.AssistantToolUseMessage
	ToolCall  llm.ToolCallBlock
	Arguments []byte
	Context   []llm.ConversationMessage
	Result    ToolOutput
	IsError   bool
}
type AfterToolCallResult struct {
	Content   *[]llm.ToolResultContentBlock
	Details   *any
	IsError   *bool
	Usage     *llm.Usage
	Terminate *bool
}
type BeforeToolCallHook func(context.Context, BeforeToolCallContext) (BeforeToolCallResult, error)
type AfterToolCallHook func(context.Context, AfterToolCallContext) (AfterToolCallResult, error)

// TurnSnapshot is the immutable product context used for one provider
// turn. Agent obtains a new snapshot before every request, including the
// request after a tool batch. AgentSession can refresh configuration and
// compact messages here without replacing the surrounding Agent run.
type TurnSnapshot struct {
	Model         provider.Model
	ThinkingLevel provider.ThinkingLevel
	SystemPrompt  string
	Tool          ToolExecutor
	Tools         []provider.ToolDefinition
	Stream        provider.StreamOptions
	// Messages replaces the request context after preparation, for example
	// after session compaction. Nil preserves the loop's current messages.
	Messages []agentmsg.Message
}

// PrepareTurn is called without the Agent mutex held. Implementations
// must return a self-contained immutable value and must not retain a mutable
// slice supplied by the session. The supplied TurnContext is observational;
// changing it cannot mutate loop state.
type PrepareTurn func(context.Context, TurnContext) (TurnSnapshot, error)

type TurnContext struct {
	RunID    uint64
	Turn     uint32
	Messages []agentmsg.Message
}

// ToolUpdate is an ephemeral partial AgentToolResult. It is never persisted or
// fed to the provider. Updates arriving after Execute settles are discarded.
// Keeping the full result shape lets extension-neutral observers render rich
// output without forcing a future hook loader to invent a second protocol.
type ToolUpdate struct {
	Text           string
	Content        []llm.ToolResultContentBlock
	Details        any
	Usage          *llm.Usage
	AddedToolNames []string
	Terminate      bool
}

// ToolExecutor is the product tool execution port. toolCallID is the exact ID
// emitted by the provider and must reach the tool unchanged, matching pi's
// AgentTool.execute contract.
// report may be called synchronously or concurrently while Execute is active.
type ToolExecutor interface {
	Name() string
	Execute(context.Context, string, []byte, func(ToolUpdate)) (ToolOutput, error)
}

// NamedToolExecutor dispatches an admitted tool call by name. The agent checks
// Supports before starting it, so unknown registry names retain the normal
// error-ToolResult behavior instead of becoming coordinator failures.
type NamedToolExecutor interface {
	ToolExecutor
	Supports(string) bool
	ExecuteNamed(context.Context, string, string, []byte, func(ToolUpdate)) (ToolOutput, error)
}

// NamedToolArgumentPreparer is the optional execution-port extension used by
// tools whose provider arguments need a compatibility transform before JSON
// Schema validation. Preparation is selected by the advertised tool name so a
// registry remains the single owner of both the schema and its transform.
type NamedToolArgumentPreparer interface {
	PrepareArguments(string, any) (any, error)
}

// ToolExecutionOverride is optionally implemented by a registry adapter. A
// sequential tool makes its entire assistant batch sequential: source-order
// dependencies must never race merely because neighbouring tools are safe.
type ToolExecutionOverride interface {
	ToolExecutionMode(name string) (ToolExecutionMode, bool)
}

// Config is immutable after New. Tool may be nil so a model request for an
// unavailable tool can still become a normal error ToolResult.
type Config struct {
	Provider        provider.Provider
	InitialMessages []agentmsg.Message
	Model           provider.Model
	ThinkingLevel   provider.ThinkingLevel
	SystemPrompt    string
	Stream          provider.StreamOptions
	Tool            ToolExecutor
	// ToolExecution controls a batch unless any selected named tool requests
	// sequential execution. The zero value is parallel, matching upstream.
	ToolExecution ToolExecutionMode
	// TransformContext is an immutable request seam. It receives a copied
	// context snapshot immediately before every provider call and must return
	// a replacement snapshot; it never mutates Agent's retained messages.
	TransformContext      ContextTransform
	TransformAgentContext AgentContextTransform
	ConvertToLLM          AgentLoopConvertToLLM
	GetAPIKey             AgentLoopAPIKey
	SteeringMode          QueueMode
	FollowUpMode          QueueMode
	// Tools is the immutable model-visible schema snapshot for this run. It is
	// separate from Tool execution so deterministic providers can remain
	// tool-free while production binds both views through one registry.
	Tools          []provider.ToolDefinition
	BeforeToolCall BeforeToolCallHook
	AfterToolCall  AfterToolCallHook
	MessageEnd     MessageEndHook
	// PrepareTurn refreshes the snapshot for every provider request.
	// AgentSession resolves dynamic configuration and compacts messages here.
	PrepareTurn PrepareTurn
	// PrepareNextTurn is the stateful Agent's full upstream after-turn boundary.
	// It runs after turn_end even when no provider request follows, and its
	// context update is visible to ShouldStopAfterTurn. When combined with
	// PrepareTurn, its selected configuration overrides the request snapshot.
	// Its message replacement precedes queue delivery and request preparation.
	PrepareNextTurn     AgentLoopPrepareNextTurn
	ShouldStopAfterTurn AgentLoopShouldStopAfterTurn
	Now                 func() time.Time
}

type runtimeConfig struct {
	provider              provider.Provider
	stream                provider.StreamOptions
	beforeToolCall        BeforeToolCallHook
	afterToolCall         AfterToolCallHook
	messageEnd            MessageEndHook
	prepareTurn           PrepareTurn
	prepareNextTurn       AgentLoopPrepareNextTurn
	shouldStopAfterTurn   AgentLoopShouldStopAfterTurn
	now                   func() time.Time
	toolExecution         ToolExecutionMode
	transformContext      ContextTransform
	transformAgentContext AgentContextTransform
	convertToLLM          AgentLoopConvertToLLM
	getAPIKey             AgentLoopAPIKey
	steeringMode          QueueMode
	followUpMode          QueueMode
}

// validatedConfig exists only during construction. Keeping bootstrap state
// separate from runtimeConfig prevents Agent.config from retaining stale
// model/prompt/tool copies after Agent becomes their single mutable owner.
type validatedConfig struct {
	policy        runtimeConfig
	model         provider.Model
	hasModel      bool
	thinkingLevel provider.ThinkingLevel
	systemPrompt  string
	tool          ToolExecutor
	toolName      string
	tools         []provider.ToolDefinition
}

// ToolExecutionMode controls one assistant message's complete tool batch.
type ToolExecutionMode uint8

const (
	ToolExecutionParallel ToolExecutionMode = iota + 1
	ToolExecutionSequential
)

func (m ToolExecutionMode) String() string {
	switch m {
	case ToolExecutionParallel:
		return "parallel"
	case ToolExecutionSequential:
		return "sequential"
	default:
		return "unknown"
	}
}

// QueueMode determines how a queue drain point consumes accepted messages.
type QueueMode uint8

const (
	QueueOneAtATime QueueMode = iota + 1
	QueueAll
)

func (m QueueMode) String() string {
	switch m {
	case QueueOneAtATime:
		return "one-at-a-time"
	case QueueAll:
		return "all"
	default:
		return "unknown"
	}
}

// CompactionReason is the typed trigger carried by both start and settlement
// events. It is policy metadata only; Session remains the durable owner.
type CompactionReason uint8

const (
	CompactionManual CompactionReason = iota + 1
	CompactionThreshold
	CompactionContextOverflow
	CompactionBranchSummary
)

func (r CompactionReason) String() string {
	switch r {
	case CompactionManual:
		return "manual"
	case CompactionThreshold:
		return "threshold"
	case CompactionContextOverflow:
		return "overflow"
	case CompactionBranchSummary:
		return "branchSummary"
	default:
		return "unknown"
	}
}

// ContextTransform is called synchronously by the coordinator before each
// provider request. Both input and output are copied at the boundary.
type ContextTransform func(context.Context, []llm.ConversationMessage) ([]llm.ConversationMessage, error)
type AgentContextTransform func(context.Context, []agentmsg.Message) (*[]agentmsg.Message, error)
type MessageEndHook func(context.Context, agentmsg.Message) (agentmsg.Message, error)

func modelPresent(value provider.Model) bool {
	return value.Provider() != "" || value.API() != "" || value.ID() != ""
}

func validateConfig(config Config) (validatedConfig, error) {
	if isNilInterface(config.Provider) {
		return validatedConfig{}, fmt.Errorf("%w: provider is required", ErrInvalidConfig)
	}
	hasModel := modelPresent(config.Model)
	if hasModel {
		if _, err := provider.NewRequestWithOptions(config.Model, config.SystemPrompt, nil, provider.RequestOptions{
			Tools:                  config.Tools,
			AllowParallelToolCalls: false,
		}); err != nil {
			return validatedConfig{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
	}

	configuredTool := config.Tool
	toolName := ""
	if isNilInterface(configuredTool) {
		configuredTool = nil
	} else {
		var err error
		toolName, err = configuredToolName(configuredTool)
		if err != nil {
			return validatedConfig{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
		if !utf8.ValidString(toolName) || strings.TrimSpace(toolName) == "" {
			return validatedConfig{}, fmt.Errorf("%w: tool name must be non-empty valid UTF-8", ErrInvalidConfig)
		}
	}

	toolExecution := config.ToolExecution
	if toolExecution == 0 {
		toolExecution = ToolExecutionParallel
	}
	if toolExecution != ToolExecutionParallel && toolExecution != ToolExecutionSequential {
		return validatedConfig{}, fmt.Errorf("%w: invalid tool execution mode", ErrInvalidConfig)
	}
	steeringMode := config.SteeringMode
	if steeringMode == 0 {
		steeringMode = QueueOneAtATime
	}
	followUpMode := config.FollowUpMode
	if followUpMode == 0 {
		followUpMode = QueueOneAtATime
	}
	if (steeringMode != QueueOneAtATime && steeringMode != QueueAll) ||
		(followUpMode != QueueOneAtATime && followUpMode != QueueAll) {
		return validatedConfig{}, fmt.Errorf("%w: invalid queue mode", ErrInvalidConfig)
	}
	thinkingLevel := config.ThinkingLevel
	if thinkingLevel == "" {
		thinkingLevel = provider.ThinkingOff
	}
	if !thinkingLevel.Valid() {
		return validatedConfig{}, fmt.Errorf("%w: invalid thinking level %q", ErrInvalidConfig, thinkingLevel)
	}
	if !hasModel {
		thinkingLevel = provider.ThinkingOff
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}

	return validatedConfig{
		policy: runtimeConfig{
			provider: config.Provider, stream: provider.CloneStreamOptions(config.Stream),
			beforeToolCall: config.BeforeToolCall, afterToolCall: config.AfterToolCall,
			messageEnd: config.MessageEnd, prepareTurn: config.PrepareTurn, prepareNextTurn: config.PrepareNextTurn,
			shouldStopAfterTurn: config.ShouldStopAfterTurn,
			now:                 now, toolExecution: toolExecution,
			transformContext: config.TransformContext, transformAgentContext: config.TransformAgentContext,
			convertToLLM: config.ConvertToLLM, getAPIKey: config.GetAPIKey,
			steeringMode: steeringMode, followUpMode: followUpMode,
		},
		model: config.Model, hasModel: hasModel, thinkingLevel: thinkingLevel,
		systemPrompt: config.SystemPrompt, tool: configuredTool, toolName: toolName,
		tools: append([]provider.ToolDefinition(nil), config.Tools...),
	}, nil
}

func configuredToolName(tool ToolExecutor) (name string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			name = ""
			err = fmt.Errorf("tool name panicked: %s", safeValueText(recovered))
		}
	}()
	return tool.Name(), nil
}

// Phase is the externally inspectable coarse phase. Detailed transition state
// remains coordinator-owned.
type Phase uint8

const (
	PhaseIdle Phase = iota + 1
	PhaseProvider
	PhaseCompacting
	PhaseRetryWait
	PhaseTool
	PhaseSettling
)

func (p Phase) String() string {
	switch p {
	case PhaseIdle:
		return "idle"
	case PhaseProvider:
		return "provider"
	case PhaseCompacting:
		return "compacting"
	case PhaseRetryWait:
		return "retry_wait"
	case PhaseTool:
		return "tool"
	case PhaseSettling:
		return "settling"
	default:
		return "unknown"
	}
}

// State is an immutable snapshot of the active coordinator state.
type State struct {
	phase            Phase
	runID            uint64
	turn             uint32
	pendingToolCalls []string
	model            provider.Model
	hasModel         bool
	thinkingLevel    provider.ThinkingLevel
	systemPrompt     string
	tools            []provider.ToolDefinition
	messages         []agentmsg.Message
	isStreaming      bool
	streamingMessage agentmsg.Message
	errorMessage     string
}

func (s State) Phase() Phase { return s.phase }
func (s State) RunID() (uint64, bool) {
	return s.runID, s.phase != PhaseIdle
}
func (s State) Turn() uint32                          { return s.turn }
func (s State) Model() provider.Model                 { return s.model }
func (s State) HasModel() bool                        { return s.hasModel }
func (s State) SelectedModel() (provider.Model, bool) { return s.model, s.hasModel }
func (s State) ThinkingLevel() provider.ThinkingLevel { return s.thinkingLevel }
func (s State) SystemPrompt() string                  { return s.systemPrompt }
func (s State) Tools() []provider.ToolDefinition {
	return append([]provider.ToolDefinition(nil), s.tools...)
}
func (s State) Messages() []agentmsg.Message { return append([]agentmsg.Message(nil), s.messages...) }
func (s State) IsStreaming() bool            { return s.isStreaming }
func (s State) StreamingMessage() (agentmsg.Message, bool) {
	return agentmsg.CloneOne(s.streamingMessage), s.streamingMessage != nil
}
func (s State) ErrorMessage() (string, bool) { return s.errorMessage, s.errorMessage != "" }

// PendingToolCalls returns an immutable snapshot of all calls active in the
// current batch. Parallel batches expose every call that has started and not
// settled; sequential batches expose the one call currently being executed.
func (s State) PendingToolCalls() []string {
	return append([]string(nil), s.pendingToolCalls...)
}

// PendingToolCall is retained for callers of the v0.1 single-tool surface. It
// reports a call only while exactly one call is active; use PendingToolCalls
// for a complete multi-tool snapshot.
func (s State) PendingToolCall() (string, bool) {
	if len(s.pendingToolCalls) != 1 {
		return "", false
	}
	return s.pendingToolCalls[0], true
}

// AgentEventType is the Go discriminator for pi's AgentEvent union. Concrete
// event structs carry only fields valid for that member; there is no generic
// bag whose zero values need interpretation.
type AgentEventType string

const (
	AgentStartEventType          AgentEventType = "agent_start"
	AgentEndEventType            AgentEventType = "agent_end"
	TurnStartEventType           AgentEventType = "turn_start"
	TurnEndEventType             AgentEventType = "turn_end"
	MessageStartEventType        AgentEventType = "message_start"
	MessageUpdateEventType       AgentEventType = "message_update"
	MessageEndEventType          AgentEventType = "message_end"
	ToolExecutionStartEventType  AgentEventType = "tool_execution_start"
	ToolExecutionUpdateEventType AgentEventType = "tool_execution_update"
	ToolExecutionEndEventType    AgentEventType = "tool_execution_end"

	// Control events describe coordinator mechanics that are not members of
	// pi's AgentEvent union. They have a separate SubscribeControl boundary so
	// Agent.Subscribe remains source- and data-shape compatible with pi.
	QueueUpdateEventType                 AgentEventType = "queue_update"
	CompactionStartEventType             AgentEventType = "compaction_start"
	CompactionEndEventType               AgentEventType = "compaction_end"
	ProviderRetryScheduledEventType      AgentEventType = "provider_retry_scheduled"
	ProviderRetryAttemptEventType        AgentEventType = "provider_retry_attempt"
	ProviderRetryFinishedEventType       AgentEventType = "provider_retry_finished"
	SummarizationRetryScheduledEventType AgentEventType = "summarization_retry_scheduled"
	SummarizationRetryAttemptEventType   AgentEventType = "summarization_retry_attempt_start"
	SummarizationRetryFinishedEventType  AgentEventType = "summarization_retry_finished"
)

// AgentEvent is sealed to the event vocabulary emitted by Agent.Subscribe.
type AgentEvent interface {
	Type() AgentEventType
	agentEvent()
}

// AgentControlEvent is the typed diagnostic/control plane for mechanics that
// pi's low-level AgentEvent intentionally does not expose.
type AgentControlEvent interface {
	Type() AgentEventType
	agentControlEvent()
}

type agentRuntimeEvent interface {
	Type() AgentEventType
	agentRuntimeEvent()
}

// AssistantMessageEvent is the canonical message_update payload: the raw
// provider-neutral delta plus the complete assistant partial after that delta.
type AssistantMessageEvent struct {
	event   llm.StreamEvent
	partial agentmsg.AssistantPartial
}

func newAssistantMessageEvent(event llm.StreamEvent, partial agentmsg.AssistantPartial) AssistantMessageEvent {
	return AssistantMessageEvent{event: event, partial: partial}
}

func (e AssistantMessageEvent) Event() llm.StreamEvent             { return e.event }
func (e AssistantMessageEvent) Partial() agentmsg.AssistantPartial { return e.partial }

type AgentStartEvent struct{ RunID uint64 }
type AgentEndEvent struct {
	RunID    uint64
	Turn     uint32
	Messages []agentmsg.Message
	Terminal llm.AssistantTerminal
	Err      error
}
type TurnStartEvent struct {
	RunID uint64
	Turn  uint32
}
type TurnEndEvent struct {
	RunID       uint64
	Turn        uint32
	Message     agentmsg.Message
	ToolResults []agentmsg.Message
}
type MessageStartEvent struct {
	RunID   uint64
	Turn    uint32
	Message agentmsg.Message
}
type MessageUpdateEvent struct {
	RunID                 uint64
	Turn                  uint32
	Message               agentmsg.Message
	AssistantMessageEvent AssistantMessageEvent
}
type MessageEndEvent struct {
	RunID   uint64
	Turn    uint32
	Message agentmsg.Message
	Model   provider.Model
}
type ToolExecutionStartEvent struct {
	RunID      uint64
	Turn       uint32
	ToolCallID string
	ToolName   string
	Arguments  json.RawMessage
}
type ToolExecutionUpdateEvent struct {
	RunID         uint64
	Turn          uint32
	ToolCallID    string
	ToolName      string
	Arguments     json.RawMessage
	PartialResult ToolUpdate
}
type ToolExecutionEndEvent struct {
	RunID      uint64
	Turn       uint32
	ToolCallID string
	ToolName   string
	Arguments  json.RawMessage
	Result     ToolOutput
	IsError    bool
	Err        error
}
type QueueUpdateEvent struct {
	RunID            uint64
	Turn             uint32
	SteeringMessages []agentmsg.Message
	FollowUpMessages []agentmsg.Message
}
type CompactionStartEvent struct {
	RunID     uint64
	Turn      uint32
	Reason    CompactionReason
	WillRetry bool
}
type CompactionEndEvent struct {
	RunID     uint64
	Turn      uint32
	Reason    CompactionReason
	Result    *session.CompactResult
	Aborted   bool
	WillRetry bool
	// ErrorMessage is the transport-neutral AgentSessionEvent payload. Err
	// retains the wrapped Go cause for internal observers that need errors.Is.
	ErrorMessage string
	Err          error
}
type ProviderRetryScheduledEvent struct {
	RunID       uint64
	Turn        uint32
	Attempt     uint32
	Delay       time.Duration
	FailureKind provider.FailureKind
	HTTPStatus  int
}
type ProviderRetryAttemptEvent struct {
	RunID   uint64
	Turn    uint32
	Attempt uint32
}
type ProviderRetryFinishedEvent struct {
	RunID        uint64
	Turn         uint32
	Attempt      uint32
	FailureKind  provider.FailureKind
	HTTPStatus   int
	Succeeded    bool
	FinishReason provider.RetryFinishReason
}
type SummarizationRetryScheduledEvent struct {
	RunID       uint64
	Turn        uint32
	Reason      CompactionReason
	Attempt     uint32
	Delay       time.Duration
	FailureKind provider.FailureKind
	HTTPStatus  int
}
type SummarizationRetryAttemptEvent struct {
	RunID   uint64
	Turn    uint32
	Reason  CompactionReason
	Attempt uint32
}
type SummarizationRetryFinishedEvent struct {
	RunID        uint64
	Turn         uint32
	Reason       CompactionReason
	Attempt      uint32
	FailureKind  provider.FailureKind
	HTTPStatus   int
	Succeeded    bool
	FinishReason provider.RetryFinishReason
}

func (AgentStartEvent) Type() AgentEventType             { return AgentStartEventType }
func (AgentEndEvent) Type() AgentEventType               { return AgentEndEventType }
func (TurnStartEvent) Type() AgentEventType              { return TurnStartEventType }
func (TurnEndEvent) Type() AgentEventType                { return TurnEndEventType }
func (MessageStartEvent) Type() AgentEventType           { return MessageStartEventType }
func (MessageUpdateEvent) Type() AgentEventType          { return MessageUpdateEventType }
func (MessageEndEvent) Type() AgentEventType             { return MessageEndEventType }
func (ToolExecutionStartEvent) Type() AgentEventType     { return ToolExecutionStartEventType }
func (ToolExecutionUpdateEvent) Type() AgentEventType    { return ToolExecutionUpdateEventType }
func (ToolExecutionEndEvent) Type() AgentEventType       { return ToolExecutionEndEventType }
func (QueueUpdateEvent) Type() AgentEventType            { return QueueUpdateEventType }
func (CompactionStartEvent) Type() AgentEventType        { return CompactionStartEventType }
func (CompactionEndEvent) Type() AgentEventType          { return CompactionEndEventType }
func (ProviderRetryScheduledEvent) Type() AgentEventType { return ProviderRetryScheduledEventType }
func (ProviderRetryAttemptEvent) Type() AgentEventType   { return ProviderRetryAttemptEventType }
func (ProviderRetryFinishedEvent) Type() AgentEventType  { return ProviderRetryFinishedEventType }
func (SummarizationRetryScheduledEvent) Type() AgentEventType {
	return SummarizationRetryScheduledEventType
}
func (SummarizationRetryAttemptEvent) Type() AgentEventType {
	return SummarizationRetryAttemptEventType
}
func (SummarizationRetryFinishedEvent) Type() AgentEventType {
	return SummarizationRetryFinishedEventType
}

func (AgentStartEvent) agentEvent()          {}
func (AgentEndEvent) agentEvent()            {}
func (TurnStartEvent) agentEvent()           {}
func (TurnEndEvent) agentEvent()             {}
func (MessageStartEvent) agentEvent()        {}
func (MessageUpdateEvent) agentEvent()       {}
func (MessageEndEvent) agentEvent()          {}
func (ToolExecutionStartEvent) agentEvent()  {}
func (ToolExecutionUpdateEvent) agentEvent() {}
func (ToolExecutionEndEvent) agentEvent()    {}

func (QueueUpdateEvent) agentControlEvent()                 {}
func (CompactionStartEvent) agentControlEvent()             {}
func (CompactionEndEvent) agentControlEvent()               {}
func (ProviderRetryScheduledEvent) agentControlEvent()      {}
func (ProviderRetryAttemptEvent) agentControlEvent()        {}
func (ProviderRetryFinishedEvent) agentControlEvent()       {}
func (SummarizationRetryScheduledEvent) agentControlEvent() {}
func (SummarizationRetryAttemptEvent) agentControlEvent()   {}
func (SummarizationRetryFinishedEvent) agentControlEvent()  {}

func (AgentStartEvent) agentRuntimeEvent()                  {}
func (AgentEndEvent) agentRuntimeEvent()                    {}
func (TurnStartEvent) agentRuntimeEvent()                   {}
func (TurnEndEvent) agentRuntimeEvent()                     {}
func (MessageStartEvent) agentRuntimeEvent()                {}
func (MessageUpdateEvent) agentRuntimeEvent()               {}
func (MessageEndEvent) agentRuntimeEvent()                  {}
func (ToolExecutionStartEvent) agentRuntimeEvent()          {}
func (ToolExecutionUpdateEvent) agentRuntimeEvent()         {}
func (ToolExecutionEndEvent) agentRuntimeEvent()            {}
func (QueueUpdateEvent) agentRuntimeEvent()                 {}
func (CompactionStartEvent) agentRuntimeEvent()             {}
func (CompactionEndEvent) agentRuntimeEvent()               {}
func (ProviderRetryScheduledEvent) agentRuntimeEvent()      {}
func (ProviderRetryAttemptEvent) agentRuntimeEvent()        {}
func (ProviderRetryFinishedEvent) agentRuntimeEvent()       {}
func (SummarizationRetryScheduledEvent) agentRuntimeEvent() {}
func (SummarizationRetryAttemptEvent) agentRuntimeEvent()   {}
func (SummarizationRetryFinishedEvent) agentRuntimeEvent()  {}

func cloneAgentEvent(event AgentEvent) AgentEvent {
	switch value := event.(type) {
	case AgentStartEvent:
		return value
	case AgentEndEvent:
		value.Messages = agentmsg.Clone(value.Messages)
		return value
	case TurnStartEvent:
		return value
	case TurnEndEvent:
		value.Message = agentmsg.CloneOne(value.Message)
		value.ToolResults = agentmsg.Clone(value.ToolResults)
		return value
	case MessageStartEvent:
		value.Message = agentmsg.CloneOne(value.Message)
		return value
	case MessageUpdateEvent:
		value.Message = agentmsg.CloneOne(value.Message)
		return value
	case MessageEndEvent:
		value.Message = agentmsg.CloneOne(value.Message)
		return value
	case ToolExecutionStartEvent:
		value.Arguments = bytes.Clone(value.Arguments)
		return value
	case ToolExecutionUpdateEvent:
		value.Arguments = bytes.Clone(value.Arguments)
		value.PartialResult = cloneToolUpdate(value.PartialResult)
		return value
	case ToolExecutionEndEvent:
		value.Arguments = bytes.Clone(value.Arguments)
		value.Result = cloneToolOutput(value.Result)
		return value
	default:
		return nil
	}
}

func cloneAgentControlEvent(event AgentControlEvent) AgentControlEvent {
	switch value := event.(type) {
	case QueueUpdateEvent:
		value.SteeringMessages = agentmsg.Clone(value.SteeringMessages)
		value.FollowUpMessages = agentmsg.Clone(value.FollowUpMessages)
		return value
	case CompactionStartEvent,
		ProviderRetryScheduledEvent, ProviderRetryAttemptEvent, ProviderRetryFinishedEvent,
		SummarizationRetryScheduledEvent, SummarizationRetryAttemptEvent, SummarizationRetryFinishedEvent:
		return value
	case CompactionEndEvent:
		if value.Result != nil {
			result := session.CloneCompactResult(*value.Result)
			value.Result = &result
		}
		return value
	default:
		return nil
	}
}

func cloneAgentRuntimeEvent(event agentRuntimeEvent) agentRuntimeEvent {
	if value, ok := event.(AgentEvent); ok {
		return cloneAgentEvent(value).(agentRuntimeEvent)
	}
	if value, ok := event.(AgentControlEvent); ok {
		return cloneAgentControlEvent(value).(agentRuntimeEvent)
	}
	return nil
}

func agentEventTurn(event agentRuntimeEvent) uint32 {
	switch value := event.(type) {
	case AgentEndEvent:
		return value.Turn
	case TurnStartEvent:
		return value.Turn
	case TurnEndEvent:
		return value.Turn
	case MessageStartEvent:
		return value.Turn
	case MessageUpdateEvent:
		return value.Turn
	case MessageEndEvent:
		return value.Turn
	case ToolExecutionStartEvent:
		return value.Turn
	case ToolExecutionUpdateEvent:
		return value.Turn
	case ToolExecutionEndEvent:
		return value.Turn
	case QueueUpdateEvent:
		return value.Turn
	case CompactionStartEvent:
		return value.Turn
	case CompactionEndEvent:
		return value.Turn
	case ProviderRetryScheduledEvent:
		return value.Turn
	case ProviderRetryAttemptEvent:
		return value.Turn
	case ProviderRetryFinishedEvent:
		return value.Turn
	case SummarizationRetryScheduledEvent:
		return value.Turn
	case SummarizationRetryAttemptEvent:
		return value.Turn
	case SummarizationRetryFinishedEvent:
		return value.Turn
	default:
		return 0
	}
}

func agentEventRunID(event agentRuntimeEvent) uint64 {
	switch value := event.(type) {
	case AgentStartEvent:
		return value.RunID
	case AgentEndEvent:
		return value.RunID
	case TurnStartEvent:
		return value.RunID
	case TurnEndEvent:
		return value.RunID
	case MessageStartEvent:
		return value.RunID
	case MessageUpdateEvent:
		return value.RunID
	case MessageEndEvent:
		return value.RunID
	case ToolExecutionStartEvent:
		return value.RunID
	case ToolExecutionUpdateEvent:
		return value.RunID
	case ToolExecutionEndEvent:
		return value.RunID
	case QueueUpdateEvent:
		return value.RunID
	case CompactionStartEvent:
		return value.RunID
	case CompactionEndEvent:
		return value.RunID
	case ProviderRetryScheduledEvent:
		return value.RunID
	case ProviderRetryAttemptEvent:
		return value.RunID
	case ProviderRetryFinishedEvent:
		return value.RunID
	case SummarizationRetryScheduledEvent:
		return value.RunID
	case SummarizationRetryAttemptEvent:
		return value.RunID
	case SummarizationRetryFinishedEvent:
		return value.RunID
	default:
		return 0
	}
}

// Observer is invoked synchronously in subscription order. Each observer gets
// an independent snapshot of mutable slices/maps. Agent holds no internal
// mutex while callbacks run.
type Observer func(context.Context, AgentEvent)

// ControlObserver observes coordinator mechanics separately from pi's public
// AgentEvent lifecycle.
type ControlObserver func(context.Context, AgentControlEvent)

// Result describes a settled accepted run. A provider error or abort is a
// terminal result, not a returned Go error. Returned errors are preflight or
// fatal coordinator/storage failures.
type Result struct {
	runID          uint64
	terminal       llm.AssistantTerminal
	providerTurns  uint32
	toolExecutions uint32
	handled        bool
}

func (r Result) RunID() uint64 { return r.runID }
func (r Result) Terminal() (llm.AssistantTerminal, bool) {
	return r.terminal, r.terminal != nil
}
func (r Result) ProviderTurns() uint32  { return r.providerTurns }
func (r Result) ToolExecutions() uint32 { return r.toolExecutions }

// Handled reports that prompt preflight was fully consumed by an extension
// command or input hook, or was successfully queued onto an existing run.
// Such a result intentionally has no synthetic assistant terminal.
func (r Result) Handled() bool { return r.handled }
func (r Result) Succeeded() bool {
	if r.handled {
		return true
	}
	if r.terminal == nil {
		return false
	}
	return r.terminal.FinishReason() == llm.FinishStop || r.terminal.FinishReason() == llm.FinishLength
}

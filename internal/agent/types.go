// Package agent coordinates one provider/tool/session run without owning any
// of those modules' implementation details or durable formats.
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

const defaultSettlementTimeout = 30 * time.Second

var (
	ErrInvalidConfig         = errors.New("invalid agent configuration")
	ErrInvalidRun            = errors.New("invalid agent run")
	ErrBusy                  = errors.New("agent is already running")
	ErrRunIDExhausted        = errors.New("agent run id exhausted")
	ErrTranscriptCommit      = errors.New("agent transcript commit failed")
	ErrInvariant             = errors.New("agent invariant failure")
	ErrProviderStream        = errors.New("provider stream failed")
	ErrToolNotFound          = errors.New("tool not found")
	ErrToolUnsettled         = errors.New("tool returned an unsettled outcome")
	ErrAgentAborted          = errors.New("agent run aborted")
	ErrContextTransform      = errors.New("agent context transform failed")
	ErrInvalidQueueMessage   = errors.New("invalid queued message")
	ErrCannotContinue        = errors.New("agent cannot continue from current transcript")
	ErrCompactionUnavailable = errors.New("agent compaction is not configured")
	ErrRetryPolicy           = provider.ErrInvalidRetryPolicy
	// ErrUnsupportedToolTurn is retained for source compatibility with the
	// v0.1 internal implementation; v0.2 no longer returns it for batches.
	ErrUnsupportedToolTurn = errors.New("unsupported tool turn")
)

// Transcript is the complete session capability consumed by the coordinator.
// The concrete session remains responsible for ordering and durability.
type Transcript interface {
	Context() session.Context
	Append(context.Context, llm.ConversationMessage, session.AppendOptions) (session.Entry, error)
}

// ContextBuilder is implemented by Session. Keeping it optional preserves the
// narrow transcript port used by deterministic test doubles, while production
// turns use Session.BuildContext's immutable selected-leaf snapshot.
type ContextBuilder interface{ BuildContext() session.Context }

type sessionCompactor interface {
	Compact(context.Context, session.CompactRequest) (session.CompactResult, error)
}

// RetryPolicy is shared with provider.ContextSummarizer so both request paths
// use identical attempt, Retry-After, jitter, cap, and cancellation semantics.
type RetryPolicy = provider.RetryPolicy

// ToolOutput is the provider-visible final text returned by one tool. A
// non-nil Execute error makes the associated ToolResult an error result.
type ToolOutput struct {
	Text string
	// Terminate asks the coordinator to stop after this batch. A batch stops
	// early only when every finalized call asks to terminate; this prevents a
	// concurrent success from silently hiding another call's continuation.
	Terminate bool
}

// ToolUpdate is an ephemeral progress snapshot. It is never persisted or fed
// to the provider. Updates arriving after Execute settles are discarded.
type ToolUpdate struct {
	Text string
}

// ToolExecutor is the compatibility execution port used by the first
// milestone. NamedToolExecutor below extends it for a registry while retaining
// existing single-tool implementations and tests.
// report may be called synchronously or concurrently while Execute is active.
type ToolExecutor interface {
	Name() string
	Execute(context.Context, []byte, func(ToolUpdate)) (ToolOutput, error)
}

// NamedToolExecutor dispatches an admitted tool call by name. The agent checks
// Supports before starting it, so unknown registry names retain the normal
// error-ToolResult behavior instead of becoming coordinator failures.
type NamedToolExecutor interface {
	ToolExecutor
	Supports(string) bool
	ExecuteNamed(context.Context, string, []byte, func(ToolUpdate)) (ToolOutput, error)
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
	Provider     provider.Provider
	Transcript   Transcript
	Model        provider.ModelRef
	SystemPrompt string
	Tool         ToolExecutor
	// ToolExecution controls a batch unless any selected named tool requests
	// sequential execution. The zero value is parallel, matching upstream.
	ToolExecution ToolExecutionMode
	// TransformContext is an immutable request seam. It receives a copied
	// transcript projection immediately before every provider call and must
	// return a replacement snapshot; it never mutates durable transcript data.
	TransformContext ContextTransform
	SteeringMode     QueueMode
	FollowUpMode     QueueMode
	// Tools is the immutable model-visible schema snapshot for this run. It is
	// separate from Tool execution so deterministic providers can remain
	// tool-free while production binds both views through one registry.
	Tools []provider.ToolDefinition
	// ContextWindow and ContextReserve enable pre-prompt automatic compaction.
	// A configured threshold requires a real Session compactor and summarizer.
	ContextWindow     uint64
	ContextReserve    uint64
	KeepRecentTokens  uint64
	Summarizer        session.Summarizer
	Retry             RetryPolicy
	Now               func() time.Time
	SettlementTimeout time.Duration
}

type runtimeConfig struct {
	provider          provider.Provider
	transcript        Transcript
	model             provider.ModelRef
	systemPrompt      string
	tool              ToolExecutor
	toolName          string
	tools             []provider.ToolDefinition
	now               func() time.Time
	settlementTimeout time.Duration
	toolExecution     ToolExecutionMode
	transformContext  ContextTransform
	steeringMode      QueueMode
	followUpMode      QueueMode
	contextWindow     uint64
	contextReserve    uint64
	keepRecentTokens  uint64
	summarizer        session.Summarizer
	compactor         sessionCompactor
	retry             provider.RetryController
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
)

func (r CompactionReason) String() string {
	switch r {
	case CompactionManual:
		return "manual"
	case CompactionThreshold:
		return "threshold"
	case CompactionContextOverflow:
		return "overflow"
	default:
		return "unknown"
	}
}

// ContextTransform is called synchronously by the coordinator before each
// provider request. Both input and output are copied at the boundary.
type ContextTransform func(context.Context, []llm.ConversationMessage) ([]llm.ConversationMessage, error)

func validateConfig(config Config) (runtimeConfig, error) {
	if isNilInterface(config.Provider) {
		return runtimeConfig{}, fmt.Errorf("%w: provider is required", ErrInvalidConfig)
	}
	if isNilInterface(config.Transcript) {
		return runtimeConfig{}, fmt.Errorf("%w: transcript is required", ErrInvalidConfig)
	}
	if _, err := provider.NewRequestWithOptions(config.Model, config.SystemPrompt, nil, provider.RequestOptions{
		Tools:                  config.Tools,
		AllowParallelToolCalls: false,
	}); err != nil {
		return runtimeConfig{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	configuredTool := config.Tool
	toolName := ""
	if isNilInterface(configuredTool) {
		configuredTool = nil
	} else {
		var err error
		toolName, err = configuredToolName(configuredTool)
		if err != nil {
			return runtimeConfig{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
		if !utf8.ValidString(toolName) || strings.TrimSpace(toolName) == "" {
			return runtimeConfig{}, fmt.Errorf("%w: tool name must be non-empty valid UTF-8", ErrInvalidConfig)
		}
	}

	settlementTimeout := config.SettlementTimeout
	if settlementTimeout < 0 {
		return runtimeConfig{}, fmt.Errorf("%w: settlement timeout cannot be negative", ErrInvalidConfig)
	}
	if settlementTimeout == 0 {
		settlementTimeout = defaultSettlementTimeout
	}
	toolExecution := config.ToolExecution
	if toolExecution == 0 {
		toolExecution = ToolExecutionParallel
	}
	if toolExecution != ToolExecutionParallel && toolExecution != ToolExecutionSequential {
		return runtimeConfig{}, fmt.Errorf("%w: invalid tool execution mode", ErrInvalidConfig)
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
		return runtimeConfig{}, fmt.Errorf("%w: invalid queue mode", ErrInvalidConfig)
	}
	if config.ContextReserve > config.ContextWindow && config.ContextWindow != 0 {
		return runtimeConfig{}, fmt.Errorf("%w: context reserve exceeds window", ErrInvalidConfig)
	}
	if config.ContextWindow != 0 && config.Summarizer == nil {
		return runtimeConfig{}, fmt.Errorf("%w: automatic compaction requires a summarizer", ErrInvalidConfig)
	}
	var compactor sessionCompactor
	if candidate, ok := config.Transcript.(sessionCompactor); ok {
		compactor = candidate
	}
	if config.ContextWindow != 0 && compactor == nil {
		return runtimeConfig{}, fmt.Errorf("%w: automatic compaction requires Session", ErrInvalidConfig)
	}
	retry, err := provider.NewRetryController(config.Retry)
	if err != nil {
		return runtimeConfig{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}

	return runtimeConfig{
		provider:          config.Provider,
		transcript:        config.Transcript,
		model:             config.Model,
		systemPrompt:      config.SystemPrompt,
		tool:              configuredTool,
		toolName:          toolName,
		tools:             append([]provider.ToolDefinition(nil), config.Tools...),
		now:               now,
		settlementTimeout: settlementTimeout,
		toolExecution:     toolExecution,
		transformContext:  config.TransformContext,
		steeringMode:      steeringMode,
		followUpMode:      followUpMode,
		contextWindow:     config.ContextWindow,
		contextReserve:    config.ContextReserve,
		keepRecentTokens:  config.KeepRecentTokens,
		summarizer:        config.Summarizer,
		compactor:         compactor,
		retry:             retry,
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

// effectiveToolExecutionMode is the shared request/execution admission rule.
// A request may advertise parallel calls only when every advertised tool can
// remain in the parallel lane. At execution time the same rule is applied to
// the calls actually selected by the provider. Unknown overrides inherit the
// global mode; malformed override values fail closed to sequential.
func effectiveToolExecutionMode(global ToolExecutionMode, executor ToolExecutor, names []string) ToolExecutionMode {
	if global != ToolExecutionParallel {
		return ToolExecutionSequential
	}
	overrides, ok := executor.(ToolExecutionOverride)
	if !ok {
		return ToolExecutionParallel
	}
	for _, name := range names {
		mode, set := overrides.ToolExecutionMode(name)
		if set && mode != ToolExecutionParallel {
			return ToolExecutionSequential
		}
	}
	return ToolExecutionParallel
}

func (c runtimeConfig) allowParallelToolCalls() bool {
	names := make([]string, len(c.tools))
	for index, definition := range c.tools {
		names[index] = definition.Name()
	}
	return effectiveToolExecutionMode(c.toolExecution, c.tool, names) == ToolExecutionParallel
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
}

func (s State) Phase() Phase { return s.phase }
func (s State) RunID() (uint64, bool) {
	return s.runID, s.phase != PhaseIdle
}
func (s State) Turn() uint32 { return s.turn }

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

// EventKind is the compact lifecycle vocabulary needed by current application
// callers. MessageCommitted is emitted only after the durable append succeeds.
type EventKind uint8

const (
	EventRunStarted EventKind = iota + 1
	EventTurnStarted
	EventProviderProgress
	EventMessageCommitted
	EventToolStarted
	EventToolProgress
	EventToolSettled
	EventTurnSettled
	EventRunSettled
	EventCompactionStarted
	EventCompactionSettled
	EventRetryScheduled
	EventRetryAttempt
	EventRetryFinished
	EventSummarizationRetryScheduled
	EventSummarizationRetryAttempt
	EventSummarizationRetryFinished
)

func (k EventKind) String() string {
	switch k {
	case EventRunStarted:
		return "run_started"
	case EventTurnStarted:
		return "turn_started"
	case EventProviderProgress:
		return "provider_progress"
	case EventMessageCommitted:
		return "message_committed"
	case EventToolStarted:
		return "tool_started"
	case EventToolProgress:
		return "tool_progress"
	case EventToolSettled:
		return "tool_settled"
	case EventTurnSettled:
		return "turn_settled"
	case EventRunSettled:
		return "run_settled"
	case EventCompactionStarted:
		return "compaction_started"
	case EventCompactionSettled:
		return "compaction_settled"
	case EventRetryScheduled:
		return "retry_scheduled"
	case EventRetryAttempt:
		return "retry_attempt"
	case EventRetryFinished:
		return "retry_finished"
	case EventSummarizationRetryScheduled:
		return "summarization_retry_scheduled"
	case EventSummarizationRetryAttempt:
		return "summarization_retry_attempt"
	case EventSummarizationRetryFinished:
		return "summarization_retry_finished"
	default:
		return "unknown"
	}
}

// Event is passed by value to observers. The llm values it carries are
// immutable snapshots; zero-valued fields do not apply to that event kind.
type Event struct {
	Kind             EventKind
	RunID            uint64
	Turn             uint32
	Message          llm.ConversationMessage
	ProviderSnapshot llm.StreamSnapshot
	ToolCallID       string
	ToolName         string
	ToolUpdate       ToolUpdate
	ToolOutput       ToolOutput
	ToolError        error
	Terminal         llm.AssistantTerminal
	RunError         error
	// RetryAttempt identifies the request slot. EventRetryAttempt means request
	// reconstruction begins; EventSummarizationRetryAttempt means provider
	// redispatch begins. Cancellation observed earlier closes a scheduled slot
	// without an attempt event.
	RetryAttempt      uint32
	RetryDelay        time.Duration
	RetryFailureKind  provider.FailureKind
	RetryHTTPStatus   int
	RetrySucceeded    bool
	RetryFinishReason provider.RetryFinishReason
	// CompactionReason scopes compaction and summarization-retry events.
	// CompactionWillRetry is true only for overflow recovery intent.
	CompactionReason    CompactionReason
	CompactionWillRetry bool
	Compaction          *session.CompactResult
}

// Observer is invoked synchronously in subscription order. The Agent holds no
// internal mutex while it runs, and run settlement waits for it to return.
// It must not synchronously call Abort or WaitForIdle on that same active run.
type Observer func(context.Context, Event)

// Result describes a settled accepted run. A provider error or abort is a
// terminal result, not a returned Go error. Returned errors are preflight or
// fatal coordinator/storage failures.
type Result struct {
	runID          uint64
	terminal       llm.AssistantTerminal
	providerTurns  uint32
	toolExecutions uint32
}

func (r Result) RunID() uint64 { return r.runID }
func (r Result) Terminal() (llm.AssistantTerminal, bool) {
	return r.terminal, r.terminal != nil
}
func (r Result) ProviderTurns() uint32  { return r.providerTurns }
func (r Result) ToolExecutions() uint32 { return r.toolExecutions }
func (r Result) Succeeded() bool {
	if r.terminal == nil {
		return false
	}
	return r.terminal.FinishReason() == llm.FinishStop || r.terminal.FinishReason() == llm.FinishLength
}

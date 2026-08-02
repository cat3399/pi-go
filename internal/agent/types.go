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
	ErrInvalidConfig       = errors.New("invalid agent configuration")
	ErrInvalidRun          = errors.New("invalid agent run")
	ErrBusy                = errors.New("agent is already running")
	ErrRunIDExhausted      = errors.New("agent run id exhausted")
	ErrTranscriptCommit    = errors.New("agent transcript commit failed")
	ErrInvariant           = errors.New("agent invariant failure")
	ErrProviderStream      = errors.New("provider stream failed")
	ErrToolNotFound        = errors.New("tool not found")
	ErrToolUnsettled       = errors.New("tool returned an unsettled outcome")
	ErrAgentAborted        = errors.New("agent run aborted")
	ErrContextTransform    = errors.New("agent context transform failed")
	ErrInvalidQueueMessage = errors.New("invalid queued message")
	ErrCannotContinue      = errors.New("agent cannot continue from current transcript")
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
	TransformContext  ContextTransform
	SteeringMode      QueueMode
	FollowUpMode      QueueMode
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
	now               func() time.Time
	settlementTimeout time.Duration
	toolExecution     ToolExecutionMode
	transformContext  ContextTransform
	steeringMode      QueueMode
	followUpMode      QueueMode
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
	if _, err := provider.NewRequest(config.Model, config.SystemPrompt, nil); err != nil {
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
		now:               now,
		settlementTimeout: settlementTimeout,
		toolExecution:     toolExecution,
		transformContext:  config.TransformContext,
		steeringMode:      steeringMode,
		followUpMode:      followUpMode,
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
	PhaseTool
	PhaseSettling
)

func (p Phase) String() string {
	switch p {
	case PhaseIdle:
		return "idle"
	case PhaseProvider:
		return "provider"
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
	phase           Phase
	runID           uint64
	turn            uint32
	pendingToolCall string
}

func (s State) Phase() Phase { return s.phase }
func (s State) RunID() (uint64, bool) {
	return s.runID, s.phase != PhaseIdle
}
func (s State) Turn() uint32 { return s.turn }
func (s State) PendingToolCall() (string, bool) {
	return s.pendingToolCall, s.pendingToolCall != ""
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

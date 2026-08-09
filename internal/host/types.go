// Package host exposes the long-lived, transport-neutral command, state, and
// event boundary above agentruntime.Runtime. Protocol adapters may encode these
// values, but must not own a second copy of AgentSession product state.
package host

import (
	"context"
	"errors"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

var (
	ErrClosed             = errors.New("agent host is closed")
	ErrSessionUnavailable = errors.New("agent host session is unavailable")
	ErrSessionChanged     = errors.New("agent host session changed while taking a snapshot")
	ErrInvalidCommand     = errors.New("invalid agent host command")
)

type CommandType string

const (
	CommandPrompt     CommandType = "prompt"
	CommandAbort      CommandType = "abort"
	CommandGetState   CommandType = "get_state"
	CommandClearQueue CommandType = "clear_queue"
	CommandReload     CommandType = "reload"
)

type Command interface {
	Type() CommandType
	hostCommand()
}

type PromptCommand struct {
	Message           string
	Images            []llm.ImageBlock
	StreamingBehavior agent.StreamingBehavior
}
type AbortCommand struct{}
type GetStateCommand struct{}
type ClearQueueCommand struct{}
type ReloadCommand struct{}

func (PromptCommand) Type() CommandType     { return CommandPrompt }
func (AbortCommand) Type() CommandType      { return CommandAbort }
func (GetStateCommand) Type() CommandType   { return CommandGetState }
func (ClearQueueCommand) Type() CommandType { return CommandClearQueue }
func (ReloadCommand) Type() CommandType     { return CommandReload }

func (PromptCommand) hostCommand()     {}
func (AbortCommand) hostCommand()      {}
func (GetStateCommand) hostCommand()   {}
func (ClearQueueCommand) hostCommand() {}
func (ReloadCommand) hostCommand()     {}

type CommandResult interface {
	CommandType() CommandType
	hostCommandResult()
}

// PromptAcceptedResult is returned at pi's preflightResult(true) boundary. The
// Agent operation continues asynchronously and remains observable through the
// ordered Host event stream and State.IsPromptRunning/IsStreaming.
type PromptAcceptedResult struct{ OperationID uint64 }
type AbortResult struct{}
type GetStateResult struct{ State State }
type ClearQueueResult struct{ Queue agent.QueueState }
type ReloadResult struct{}

func (PromptAcceptedResult) CommandType() CommandType { return CommandPrompt }
func (AbortResult) CommandType() CommandType          { return CommandAbort }
func (GetStateResult) CommandType() CommandType       { return CommandGetState }
func (ClearQueueResult) CommandType() CommandType     { return CommandClearQueue }
func (ReloadResult) CommandType() CommandType         { return CommandReload }

func (PromptAcceptedResult) hostCommandResult() {}
func (AbortResult) hostCommandResult()          {}
func (GetStateResult) hostCommandResult()       {}
func (ClearQueueResult) hostCommandResult()     {}
func (ReloadResult) hostCommandResult()         {}

// State is assembled from the current AgentSession owners on demand. It never
// advances by replaying events, so reconnecting transports observe the same
// authoritative product state as in-process callers.
type State struct {
	SessionID   string
	SessionFile *string
	SessionName *string
	CWD         string

	Model         provider.Model
	HasModel      bool
	ThinkingLevel provider.ThinkingLevel
	SystemPrompt  string
	Phase         agent.Phase

	IsStreaming     bool
	IsPromptRunning bool
	IsBashRunning   bool
	IsCompacting    bool
	RetryAttempt    uint32
	RetryWaiting    bool

	SteeringMode          agent.QueueMode
	FollowUpMode          agent.QueueMode
	AutoCompactionEnabled bool
	AutoRetryEnabled      bool

	MessageCount        int
	PendingMessageCount int
	QueuedMessages      agent.QueueState
	ContextUsage        *agent.ContextUsage
}

type EventType string

const (
	EventAgentSession EventType = "agent_session"
	EventPromptError  EventType = "prompt_error"
	EventPromptDone   EventType = "prompt_done"
)

type EventValue interface {
	Type() EventType
	hostEvent()
}

// AgentSessionEvent retains the complete concrete AgentSessionEvent union.
// Wire adapters are responsible only for encoding the contained event.
type AgentSessionEvent struct{ Event agent.SessionEvent }
type PromptErrorEvent struct {
	OperationID uint64
	Message     string
}
type PromptDoneEvent struct{ OperationID uint64 }

func (AgentSessionEvent) Type() EventType { return EventAgentSession }
func (PromptErrorEvent) Type() EventType  { return EventPromptError }
func (PromptDoneEvent) Type() EventType   { return EventPromptDone }

func (AgentSessionEvent) hostEvent() {}
func (PromptErrorEvent) hostEvent()  {}
func (PromptDoneEvent) hostEvent()   {}

// Event is one item in the single Host-owned total order. Sequence is
// monotonically increasing for the lifetime of a Host, including across
// AgentSession replacement and reload rebinds.
type Event struct {
	Sequence  uint64
	SessionID string
	Value     EventValue
}

type Observer func(context.Context, Event)

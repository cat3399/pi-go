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
	"github.com/cat3399/pi-go/internal/resource"
	"github.com/cat3399/pi-go/internal/session"
)

var (
	ErrClosed             = errors.New("agent host is closed")
	ErrSessionUnavailable = errors.New("agent host session is unavailable")
	ErrSessionChanged     = errors.New("agent host session changed while taking a snapshot")
	ErrInvalidCommand     = errors.New("invalid agent host command")
)

type CommandType string

const (
	CommandPrompt               CommandType = "prompt"
	CommandAbort                CommandType = "abort"
	CommandGetState             CommandType = "get_state"
	CommandClearQueue           CommandType = "clear_queue"
	CommandReload               CommandType = "reload"
	CommandSteer                CommandType = "steer"
	CommandFollowUp             CommandType = "follow_up"
	CommandSetModel             CommandType = "set_model"
	CommandFork                 CommandType = "fork"
	CommandNavigateTree         CommandType = "navigate_tree"
	CommandSetThinkingLevel     CommandType = "set_thinking_level"
	CommandCompact              CommandType = "compact"
	CommandAbortCompaction      CommandType = "abort_compaction"
	CommandSetSessionName       CommandType = "set_session_name"
	CommandGetSessionStats      CommandType = "get_session_stats"
	CommandGetLastAssistantText CommandType = "get_last_assistant_text"
	CommandSetAutoCompaction    CommandType = "set_auto_compaction"
	CommandSetAutoRetry         CommandType = "set_auto_retry"
	CommandGetTools             CommandType = "get_tools"
	CommandSetTools             CommandType = "set_tools"
	CommandBash                 CommandType = "bash"
	CommandAbortBash            CommandType = "abort_bash"
	CommandGetCommands          CommandType = "get_commands"
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
type SteerCommand struct {
	Message string
	Images  []llm.ImageBlock
}
type FollowUpCommand struct {
	Message string
	Images  []llm.ImageBlock
}
type SetModelCommand struct{ Provider, ModelID string }
type ForkCommand struct {
	EntryID  string
	Position agent.ForkPosition
}
type NavigateTreeCommand struct {
	TargetID string
	Options  agent.NavigateTreeOptions
}
type SetThinkingLevelCommand struct{ Level provider.ThinkingLevel }
type CompactCommand struct{ CustomInstructions string }
type AbortCompactionCommand struct{}
type SetSessionNameCommand struct{ Name string }
type GetSessionStatsCommand struct{}
type GetLastAssistantTextCommand struct{}
type SetAutoCompactionCommand struct{ Enabled bool }
type SetAutoRetryCommand struct{ Enabled bool }
type GetToolsCommand struct{}
type SetToolsCommand struct{ ToolNames []string }
type BashCommand struct {
	Command            string
	ExcludeFromContext bool
	ExecutionID        *string
}
type AbortBashCommand struct{}
type GetCommandsCommand struct{}

func (PromptCommand) Type() CommandType               { return CommandPrompt }
func (AbortCommand) Type() CommandType                { return CommandAbort }
func (GetStateCommand) Type() CommandType             { return CommandGetState }
func (ClearQueueCommand) Type() CommandType           { return CommandClearQueue }
func (ReloadCommand) Type() CommandType               { return CommandReload }
func (SteerCommand) Type() CommandType                { return CommandSteer }
func (FollowUpCommand) Type() CommandType             { return CommandFollowUp }
func (SetModelCommand) Type() CommandType             { return CommandSetModel }
func (ForkCommand) Type() CommandType                 { return CommandFork }
func (NavigateTreeCommand) Type() CommandType         { return CommandNavigateTree }
func (SetThinkingLevelCommand) Type() CommandType     { return CommandSetThinkingLevel }
func (CompactCommand) Type() CommandType              { return CommandCompact }
func (AbortCompactionCommand) Type() CommandType      { return CommandAbortCompaction }
func (SetSessionNameCommand) Type() CommandType       { return CommandSetSessionName }
func (GetSessionStatsCommand) Type() CommandType      { return CommandGetSessionStats }
func (GetLastAssistantTextCommand) Type() CommandType { return CommandGetLastAssistantText }
func (SetAutoCompactionCommand) Type() CommandType    { return CommandSetAutoCompaction }
func (SetAutoRetryCommand) Type() CommandType         { return CommandSetAutoRetry }
func (GetToolsCommand) Type() CommandType             { return CommandGetTools }
func (SetToolsCommand) Type() CommandType             { return CommandSetTools }
func (BashCommand) Type() CommandType                 { return CommandBash }
func (AbortBashCommand) Type() CommandType            { return CommandAbortBash }
func (GetCommandsCommand) Type() CommandType          { return CommandGetCommands }

func (PromptCommand) hostCommand()               {}
func (AbortCommand) hostCommand()                {}
func (GetStateCommand) hostCommand()             {}
func (ClearQueueCommand) hostCommand()           {}
func (ReloadCommand) hostCommand()               {}
func (SteerCommand) hostCommand()                {}
func (FollowUpCommand) hostCommand()             {}
func (SetModelCommand) hostCommand()             {}
func (ForkCommand) hostCommand()                 {}
func (NavigateTreeCommand) hostCommand()         {}
func (SetThinkingLevelCommand) hostCommand()     {}
func (CompactCommand) hostCommand()              {}
func (AbortCompactionCommand) hostCommand()      {}
func (SetSessionNameCommand) hostCommand()       {}
func (GetSessionStatsCommand) hostCommand()      {}
func (GetLastAssistantTextCommand) hostCommand() {}
func (SetAutoCompactionCommand) hostCommand()    {}
func (SetAutoRetryCommand) hostCommand()         {}
func (GetToolsCommand) hostCommand()             {}
func (SetToolsCommand) hostCommand()             {}
func (BashCommand) hostCommand()                 {}
func (AbortBashCommand) hostCommand()            {}
func (GetCommandsCommand) hostCommand()          {}

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
type SteerResult struct{}
type FollowUpResult struct{}
type SetModelResult struct{ Model provider.Model }
type ForkResult struct {
	Cancelled    bool
	SelectedText *string
	SessionID    *string
}
type NavigateTreeResult struct {
	EditorText   *string
	Cancelled    bool
	Aborted      bool
	SummaryEntry *session.Entry
}
type SetThinkingLevelResult struct{}
type CompactResult struct{ Result session.CompactResult }
type AbortCompactionResult struct{}
type SetSessionNameResult struct{}
type GetSessionStatsResult struct {
	Stats       agent.SessionStats
	SessionName *string
}
type GetLastAssistantTextResult struct{ Text *string }
type SetAutoCompactionResult struct{}
type SetAutoRetryResult struct{}
type ToolInfo struct {
	Name        string
	Description string
	Active      bool
}
type GetToolsResult struct{ Tools []ToolInfo }
type SetToolsResult struct{}
type BashResult struct{ Result agent.BashResult }
type AbortBashResult struct{}
type CommandSource string

const (
	CommandSourceExtension CommandSource = "extension"
	CommandSourcePrompt    CommandSource = "prompt"
	CommandSourceSkill     CommandSource = "skill"
)

type SlashCommandInfo struct {
	Name        string
	Description string
	Source      CommandSource
	SourceInfo  resource.Source
}
type GetCommandsResult struct{ Commands []SlashCommandInfo }

func (PromptAcceptedResult) CommandType() CommandType       { return CommandPrompt }
func (AbortResult) CommandType() CommandType                { return CommandAbort }
func (GetStateResult) CommandType() CommandType             { return CommandGetState }
func (ClearQueueResult) CommandType() CommandType           { return CommandClearQueue }
func (ReloadResult) CommandType() CommandType               { return CommandReload }
func (SteerResult) CommandType() CommandType                { return CommandSteer }
func (FollowUpResult) CommandType() CommandType             { return CommandFollowUp }
func (SetModelResult) CommandType() CommandType             { return CommandSetModel }
func (ForkResult) CommandType() CommandType                 { return CommandFork }
func (NavigateTreeResult) CommandType() CommandType         { return CommandNavigateTree }
func (SetThinkingLevelResult) CommandType() CommandType     { return CommandSetThinkingLevel }
func (CompactResult) CommandType() CommandType              { return CommandCompact }
func (AbortCompactionResult) CommandType() CommandType      { return CommandAbortCompaction }
func (SetSessionNameResult) CommandType() CommandType       { return CommandSetSessionName }
func (GetSessionStatsResult) CommandType() CommandType      { return CommandGetSessionStats }
func (GetLastAssistantTextResult) CommandType() CommandType { return CommandGetLastAssistantText }
func (SetAutoCompactionResult) CommandType() CommandType    { return CommandSetAutoCompaction }
func (SetAutoRetryResult) CommandType() CommandType         { return CommandSetAutoRetry }
func (GetToolsResult) CommandType() CommandType             { return CommandGetTools }
func (SetToolsResult) CommandType() CommandType             { return CommandSetTools }
func (BashResult) CommandType() CommandType                 { return CommandBash }
func (AbortBashResult) CommandType() CommandType            { return CommandAbortBash }
func (GetCommandsResult) CommandType() CommandType          { return CommandGetCommands }

func (PromptAcceptedResult) hostCommandResult()       {}
func (AbortResult) hostCommandResult()                {}
func (GetStateResult) hostCommandResult()             {}
func (ClearQueueResult) hostCommandResult()           {}
func (ReloadResult) hostCommandResult()               {}
func (SteerResult) hostCommandResult()                {}
func (FollowUpResult) hostCommandResult()             {}
func (SetModelResult) hostCommandResult()             {}
func (ForkResult) hostCommandResult()                 {}
func (NavigateTreeResult) hostCommandResult()         {}
func (SetThinkingLevelResult) hostCommandResult()     {}
func (CompactResult) hostCommandResult()              {}
func (AbortCompactionResult) hostCommandResult()      {}
func (SetSessionNameResult) hostCommandResult()       {}
func (GetSessionStatsResult) hostCommandResult()      {}
func (GetLastAssistantTextResult) hostCommandResult() {}
func (SetAutoCompactionResult) hostCommandResult()    {}
func (SetAutoRetryResult) hostCommandResult()         {}
func (GetToolsResult) hostCommandResult()             {}
func (SetToolsResult) hostCommandResult()             {}
func (BashResult) hostCommandResult()                 {}
func (AbortBashResult) hostCommandResult()            {}
func (GetCommandsResult) hostCommandResult()          {}

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

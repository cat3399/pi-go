// Package application exposes the process-local command, query, snapshot, and
// event API above agentruntime.Runtime. Transport adapters encode this API but
// never own Agent or durable Session state.
package application

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
	ErrClosed             = errors.New("application session is closed")
	ErrSessionUnavailable = errors.New("application session is unavailable")
	ErrSessionChanged     = errors.New("application session changed while taking a snapshot")
	ErrInvalidCommand     = errors.New("invalid application command")
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
	applicationCommand()
}

type PromptCommand struct {
	Message           string
	Images            []llm.ImageBlock
	StreamingBehavior agent.StreamingBehavior
	Source            agent.InputSource
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

func (PromptCommand) applicationCommand()               {}
func (AbortCommand) applicationCommand()                {}
func (GetStateCommand) applicationCommand()             {}
func (ClearQueueCommand) applicationCommand()           {}
func (ReloadCommand) applicationCommand()               {}
func (SteerCommand) applicationCommand()                {}
func (FollowUpCommand) applicationCommand()             {}
func (SetModelCommand) applicationCommand()             {}
func (ForkCommand) applicationCommand()                 {}
func (NavigateTreeCommand) applicationCommand()         {}
func (SetThinkingLevelCommand) applicationCommand()     {}
func (CompactCommand) applicationCommand()              {}
func (AbortCompactionCommand) applicationCommand()      {}
func (SetSessionNameCommand) applicationCommand()       {}
func (GetSessionStatsCommand) applicationCommand()      {}
func (GetLastAssistantTextCommand) applicationCommand() {}
func (SetAutoCompactionCommand) applicationCommand()    {}
func (SetAutoRetryCommand) applicationCommand()         {}
func (GetToolsCommand) applicationCommand()             {}
func (SetToolsCommand) applicationCommand()             {}
func (BashCommand) applicationCommand()                 {}
func (AbortBashCommand) applicationCommand()            {}
func (GetCommandsCommand) applicationCommand()          {}

type CommandResult interface {
	CommandType() CommandType
	applicationCommandResult()
}

// PromptStartedResult is returned at pi's preflightResult(true) boundary. The
// Agent operation continues asynchronously and remains observable through the
// ordered ApplicationSession event stream and State.IsPromptRunning/IsStreaming.
type PromptStartedResult struct{ OperationID uint64 }
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

func (PromptStartedResult) CommandType() CommandType        { return CommandPrompt }
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

func (PromptStartedResult) applicationCommandResult()        {}
func (AbortResult) applicationCommandResult()                {}
func (GetStateResult) applicationCommandResult()             {}
func (ClearQueueResult) applicationCommandResult()           {}
func (ReloadResult) applicationCommandResult()               {}
func (SteerResult) applicationCommandResult()                {}
func (FollowUpResult) applicationCommandResult()             {}
func (SetModelResult) applicationCommandResult()             {}
func (ForkResult) applicationCommandResult()                 {}
func (NavigateTreeResult) applicationCommandResult()         {}
func (SetThinkingLevelResult) applicationCommandResult()     {}
func (CompactResult) applicationCommandResult()              {}
func (AbortCompactionResult) applicationCommandResult()      {}
func (SetSessionNameResult) applicationCommandResult()       {}
func (GetSessionStatsResult) applicationCommandResult()      {}
func (GetLastAssistantTextResult) applicationCommandResult() {}
func (SetAutoCompactionResult) applicationCommandResult()    {}
func (SetAutoRetryResult) applicationCommandResult()         {}
func (GetToolsResult) applicationCommandResult()             {}
func (SetToolsResult) applicationCommandResult()             {}
func (BashResult) applicationCommandResult()                 {}
func (AbortBashResult) applicationCommandResult()            {}
func (GetCommandsResult) applicationCommandResult()          {}

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
	EventAgentSession   EventType = "agent_session"
	EventOperation      EventType = "operation"
	EventSessionCatalog EventType = "session_catalog"
)

type OperationStatus string

const (
	OperationCompleted OperationStatus = "completed"
	OperationFailed    OperationStatus = "failed"
)

type EventValue interface {
	Type() EventType
	applicationEvent()
}

// AgentSessionEvent retains the complete concrete AgentSessionEvent union.
// Wire adapters are responsible only for encoding the contained event.
type AgentSessionEvent struct{ Event agent.SessionEvent }
type SessionCatalogChange string

const (
	SessionCreated SessionCatalogChange = "created"
	SessionUpdated SessionCatalogChange = "updated"
	SessionDeleted SessionCatalogChange = "deleted"
)

type SessionCatalogEvent struct{ Change SessionCatalogChange }
type OperationEvent struct {
	OperationID uint64
	Command     CommandType
	Status      OperationStatus
	Error       string
}

func (AgentSessionEvent) Type() EventType   { return EventAgentSession }
func (SessionCatalogEvent) Type() EventType { return EventSessionCatalog }
func (OperationEvent) Type() EventType      { return EventOperation }

func (AgentSessionEvent) applicationEvent()   {}
func (SessionCatalogEvent) applicationEvent() {}
func (OperationEvent) applicationEvent()      {}

// Event is an immutable application event envelope. ApplicationSession assigns
// a session-local Sequence across replacement and reload; Service republishes
// the same payload with a process-wide Sequence for surface subscriptions.
type Event struct {
	Sequence  uint64
	SessionID string
	Value     EventValue
}

type SessionObserver func(context.Context, Event)

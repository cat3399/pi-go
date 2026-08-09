package agent

// This file is the extension-neutral core contract. It intentionally models
// lifecycle semantics, values and cancellation without a JS loader, UI, or
// plugin discovery mechanism. A host may adapt this typed surface to any
// extension runtime.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

type HookCancel struct {
	Cancel *bool
	Reason string
}

func (r HookCancel) Validate() error {
	return nil
}
func (r HookCancel) Cancelled() bool { return r.Cancel != nil && *r.Cancel }

type ContextHookEvent struct{ Messages []agentmsg.Message }
type ContextHookResult struct{ Messages *[]agentmsg.Message }
type ContextHook func(context.Context, ContextHookEvent) (ContextHookResult, error)

// BuildSystemPromptOptions mirrors coding-agent's structured system-prompt
// inputs. It remains a value-only contract: loading resources and assembling
// the final prompt belong to the product runtime, not the extension surface.
// Nil slices/maps preserve the original optional-field distinction.
type BuildSystemPromptOptions struct {
	CustomPrompt       *string
	SelectedTools      []string
	ToolSnippets       map[string]string
	PromptGuidelines   []string
	AppendSystemPrompt *string
	CWD                string
	ContextFiles       []SystemPromptContextFile
	Skills             []SystemPromptSkill
}

type SystemPromptContextFile struct {
	Path    string
	Content string
}

type SystemPromptSourceScope string

const (
	SystemPromptSourceUser      SystemPromptSourceScope = "user"
	SystemPromptSourceProject   SystemPromptSourceScope = "project"
	SystemPromptSourceTemporary SystemPromptSourceScope = "temporary"
)

type SystemPromptSourceOrigin string

const (
	SystemPromptSourcePackage  SystemPromptSourceOrigin = "package"
	SystemPromptSourceTopLevel SystemPromptSourceOrigin = "top-level"
)

type SystemPromptSourceInfo struct {
	Path    string
	Source  string
	Scope   SystemPromptSourceScope
	Origin  SystemPromptSourceOrigin
	BaseDir *string
}

type SystemPromptSkill struct {
	Name                   string
	Description            string
	FilePath               string
	BaseDir                string
	SourceInfo             SystemPromptSourceInfo
	DisableModelInvocation bool
}

type BeforeAgentStartEvent struct {
	Prompt              string
	Images              []llm.ImageBlock
	PromptMessages      []agentmsg.Message
	SystemPrompt        string
	SystemPromptOptions BuildSystemPromptOptions
	Messages            []agentmsg.Message
}
type BeforeAgentStartResult struct {
	ExtraMessages []agentmsg.Message
	SystemPrompt  *string
	Cancel        HookCancel
}
type BeforeAgentStartHook func(context.Context, BeforeAgentStartEvent) (BeforeAgentStartResult, error)

// InputSource identifies the surface that submitted a prompt. Values match
// coding-agent's input extension event exactly.
type InputSource string

const (
	InputInteractive InputSource = "interactive"
	InputRPC         InputSource = "rpc"
	InputExtension   InputSource = "extension"
)

func (s InputSource) Valid() bool {
	return s == InputInteractive || s == InputRPC || s == InputExtension
}

// StreamingBehavior selects the queue used when prompt preflight observes an
// active AgentSession run. The empty value means no delivery mode was supplied.
type StreamingBehavior string

const (
	StreamingSteer    StreamingBehavior = "steer"
	StreamingFollowUp StreamingBehavior = "followUp"
)

func (b StreamingBehavior) Valid() bool {
	return b == "" || b == StreamingSteer || b == StreamingFollowUp
}

type InputEvent struct {
	Text              string
	Images            []llm.ImageBlock
	Source            InputSource
	StreamingBehavior StreamingBehavior
}

type InputAction string

const (
	InputContinue  InputAction = "continue"
	InputTransform InputAction = "transform"
	InputHandled   InputAction = "handled"
)

// InputResult is applied in registration order. A nil Images slice on a
// transform retains the images from the preceding handler; a non-nil empty
// slice explicitly removes them.
type InputResult struct {
	Action InputAction
	Text   string
	Images []llm.ImageBlock
}

type InputHook func(context.Context, InputEvent) (InputResult, error)

// ExtensionCommand is the resolved, transport-neutral command registration
// consumed by AgentSession prompt preflight. Duplicate Name values receive the
// same :1, :2 invocation suffixes as coding-agent's ExtensionRunner.
type ExtensionCommand struct {
	Name        string
	Description string
	Handler     ExtensionCommandHandler
}

type ExtensionCommandHandler func(context.Context, string, *AgentSession) error

// Provider hooks live in provider.StreamOptions at the exact payload/header/
// response boundary. These aliases make that relationship explicit to Agent
// callers while preserving the provider package as the transport owner.
type BeforeProviderRequestHook = provider.PayloadHook
type BeforeProviderHeadersHook = provider.HeaderHook
type AfterProviderResponseHook = provider.ResponseHook

type AgentLifecycleEvent struct {
	Type     AgentLifecycleType
	Messages []agentmsg.Message
	Terminal llm.AssistantTerminal
}
type AgentLifecycleType string

const (
	AgentStartHookEvent   AgentLifecycleType = "agent_start"
	AgentEndHookEvent     AgentLifecycleType = "agent_end"
	AgentSettledHookEvent AgentLifecycleType = "agent_settled"
)

type AgentLifecycleHook func(context.Context, AgentLifecycleEvent) error

type TurnLifecycleType string

const (
	TurnStartHookEvent TurnLifecycleType = "turn_start"
	TurnEndHookEvent   TurnLifecycleType = "turn_end"
)

// TurnLifecycleEvent mirrors the generic Agent turn boundary. TurnIndex is
// zero-based like the original package; Message and ToolResults are present
// only for turn_end.
type TurnLifecycleEvent struct {
	Type        TurnLifecycleType
	TurnIndex   uint32
	Timestamp   time.Time
	Message     agentmsg.Message
	ToolResults []agentmsg.Message
}

type TurnLifecycleHook func(context.Context, TurnLifecycleEvent) error

type MessageHookEvent struct {
	Type          MessageHookType
	Message       agentmsg.Message
	ProviderEvent llm.StreamEvent
}
type MessageHookType string

const (
	MessageStartHookEvent  MessageHookType = "message_start"
	MessageUpdateHookEvent MessageHookType = "message_update"
	MessageEndHookEvent    MessageHookType = "message_end"
)

type MessageHookResult struct {
	Message agentmsg.Message
	Cancel  HookCancel
}
type MessageHook func(context.Context, MessageHookEvent) (MessageHookResult, error)

// ExtensionErrorEvent is the transport-neutral projection of pi's extension
// runner error report. ExtensionPath belongs to a future loader; HandlerIndex
// still identifies the failing callback deterministically within this host's
// ordered hook chain.
type ExtensionErrorEvent struct {
	Event        string
	HandlerIndex int
	Cause        error
}
type ExtensionErrorHook func(context.Context, ExtensionErrorEvent)

type ToolExecutionLifecycleType string

const (
	ToolExecutionStartHookEvent  ToolExecutionLifecycleType = "tool_execution_start"
	ToolExecutionUpdateHookEvent ToolExecutionLifecycleType = "tool_execution_update"
	ToolExecutionEndHookEvent    ToolExecutionLifecycleType = "tool_execution_end"
)

// ToolExecutionLifecycleEvent is observational. ToolCall and ToolResult are
// the separate pre/post mutation hooks; this event reports the execution
// lifecycle using immutable argument/result snapshots.
type ToolExecutionLifecycleEvent struct {
	Type       ToolExecutionLifecycleType
	ToolCallID string
	ToolName   string
	Arguments  []byte
	Update     *ToolUpdate
	Result     *ToolOutput
	IsError    bool
}

type ToolExecutionLifecycleHook func(context.Context, ToolExecutionLifecycleEvent) error

type ModelSelectSource string

const (
	ModelSelectSet     ModelSelectSource = "set"
	ModelSelectCycle   ModelSelectSource = "cycle"
	ModelSelectRestore ModelSelectSource = "restore"
)

type ModelSelectEvent struct {
	Model         provider.Model
	PreviousModel *provider.Model
	Source        ModelSelectSource
}
type ModelSelectHook func(context.Context, ModelSelectEvent) error

type ThinkingLevelSelectEvent struct {
	Level         provider.ThinkingLevel
	PreviousLevel provider.ThinkingLevel
}
type ThinkingLevelSelectHook func(context.Context, ThinkingLevelSelectEvent) error

func (s *AgentSession) messageEndTransform(ctx context.Context, message agentmsg.Message) (agentmsg.Message, error) {
	if s == nil {
		return nil, nil
	}
	// message_start is dispatched from the real low-level start event. This
	// transform runs only after Agent state contains the finalized message and
	// is therefore the faithful message_end replacement boundary.
	current := agentmsg.CloneOne(message)
	modified := false
	for index, hook := range s.messageHooks() {
		result, err := callMessageHook(hook, ctx, MessageHookEvent{Type: MessageEndHookEvent, Message: agentmsg.CloneOne(current)})
		if err != nil {
			s.reportExtensionError(ctx, string(MessageEndHookEvent), index, err)
			continue
		}
		if result.Cancel.Cancelled() {
			s.reportExtensionError(ctx, string(MessageEndHookEvent), index, fmt.Errorf("%w: message_end does not support cancellation", ErrInvalidExtensionResult))
			continue
		}
		if result.Message == nil {
			continue
		}
		if isNilInterface(result.Message) || result.Message.Role() != current.Role() || isAssistantPartialMessage(result.Message) {
			s.reportExtensionError(ctx, string(MessageEndHookEvent), index, fmt.Errorf("%w: message_end handlers must return a finalized message with the same role", ErrInvalidExtensionResult))
			continue
		}
		current = agentmsg.CloneOne(result.Message)
		modified = true
	}
	if !modified {
		return nil, nil
	}
	return current, nil
}

func (s *AgentSession) messageHooks() []MessageHook {
	if s == nil {
		return nil
	}
	capacity := len(s.hooks.MessageHandlers)
	if s.hooks.Message != nil {
		capacity++
	}
	result := make([]MessageHook, 0, capacity)
	if s.hooks.Message != nil {
		result = append(result, s.hooks.Message)
	}
	result = append(result, s.hooks.MessageHandlers...)
	return result
}

func callMessageHook(hook MessageHook, ctx context.Context, event MessageHookEvent) (result MessageHookResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("message hook panicked: %s", safeValueText(recovered))
		}
	}()
	return hook(ctx, event)
}

func (s *AgentSession) reportExtensionError(ctx context.Context, event string, handlerIndex int, cause error) {
	if s == nil || s.hooks.ExtensionError == nil || cause == nil {
		return
	}
	defer func() { _ = recover() }()
	s.hooks.ExtensionError(ctx, ExtensionErrorEvent{Event: event, HandlerIndex: handlerIndex, Cause: cause})
}

type SessionStartHookEvent struct {
	Reason              SessionStartReason
	PreviousSessionFile *string
}
type SessionStartReason string

const (
	SessionStartup SessionStartReason = "startup"
	SessionReload  SessionStartReason = "reload"
	SessionNew     SessionStartReason = "new"
	SessionResume  SessionStartReason = "resume"
	SessionFork    SessionStartReason = "fork"
)

type SessionStartHook func(context.Context, SessionStartHookEvent) error
type SessionShutdownHookEvent struct {
	Reason            SessionShutdownReason
	TargetSessionFile *string
}
type SessionShutdownReason string

const (
	ShutdownQuit   SessionShutdownReason = "quit"
	ShutdownReload SessionShutdownReason = "reload"
	ShutdownNew    SessionShutdownReason = "new"
	ShutdownResume SessionShutdownReason = "resume"
	ShutdownFork   SessionShutdownReason = "fork"
)

type SessionShutdownHook func(context.Context, SessionShutdownHookEvent) error

type SessionInfoChangedEvent struct{ Name *string }
type SessionInfoChangedHook func(context.Context, SessionInfoChangedEvent) error

type ExtensionCompactionResult struct {
	Summary              string
	FirstKeptEntryID     string
	TokensBefore         uint64
	EstimatedTokensAfter *uint64
	Usage                *session.CompactionUsage
	Details              json.RawMessage
}

type SessionBeforeCompactEvent struct {
	Preparation        session.SummaryInput
	BranchEntries      []session.Entry
	CustomInstructions *string
	Reason             CompactionReason
	WillRetry          bool
}
type SessionBeforeCompactResult struct {
	Cancel     HookCancel
	Compaction *ExtensionCompactionResult
}
type SessionBeforeCompactHook func(context.Context, SessionBeforeCompactEvent) (SessionBeforeCompactResult, error)

type SessionCompactEvent struct {
	CompactionEntry session.Entry
	Result          ExtensionCompactionResult
	FromExtension   bool
	Reason          CompactionReason
	WillRetry       bool
}
type SessionCompactHook func(context.Context, SessionCompactEvent) error

type TreePreparation struct {
	TargetID            string
	OldLeafID           *string
	CommonAncestorID    *string
	EntriesToSummarize  []session.Entry
	UserWantsSummary    bool
	CustomInstructions  *string
	ReplaceInstructions *bool
	Label               *string
}
type TreeSummary struct {
	Summary string
	Details json.RawMessage
	Usage   *session.CompactionUsage
}
type SessionBeforeTreeEvent struct{ Preparation TreePreparation }
type SessionBeforeTreeResult struct {
	Cancel              HookCancel
	Summary             *TreeSummary
	CustomInstructions  *string
	ReplaceInstructions *bool
	Label               *string
}
type SessionBeforeTreeHook func(context.Context, SessionBeforeTreeEvent) (SessionBeforeTreeResult, error)
type SessionTreeEvent struct {
	NewLeafID, OldLeafID *string
	SummaryEntry         *session.Entry
	FromExtension        *bool
}
type SessionTreeHook func(context.Context, SessionTreeEvent) error

// Switch and fork are owned by the transport-neutral agentruntime.Runtime.
// Their exact extension contracts live here so Runtime can emit them through
// AgentSession without reaching into its private hook state.
type SessionSwitchReason string

const (
	SessionSwitchNew    SessionSwitchReason = "new"
	SessionSwitchResume SessionSwitchReason = "resume"
)

type SessionBeforeSwitchEvent struct {
	Reason            SessionSwitchReason
	TargetSessionFile *string
}
type SessionBeforeSwitchResult struct{ Cancel HookCancel }
type SessionBeforeSwitchHook func(context.Context, SessionBeforeSwitchEvent) (SessionBeforeSwitchResult, error)
type ForkPosition string

const (
	ForkBefore ForkPosition = "before"
	ForkAt     ForkPosition = "at"
)

type SessionBeforeForkEvent struct {
	EntryID  string
	Position ForkPosition
}
type SessionBeforeForkResult struct {
	Cancel                  HookCancel
	SkipConversationRestore *bool
}
type SessionBeforeForkHook func(context.Context, SessionBeforeForkEvent) (SessionBeforeForkResult, error)

// Hooks is a typed, host-provided callback set. No generic maps or string
// dispatch are used. Nil fields deliberately mean the corresponding runtime
// surface is not enabled by this host.
type Hooks struct {
	Context          ContextHook
	BeforeAgentStart BeforeAgentStartHook
	// InputHandlers run before skill/template expansion. Their transformations
	// chain in registration order and InputHandled stops prompt processing.
	InputHandlers         []InputHook
	Commands              []ExtensionCommand
	BeforeProviderRequest BeforeProviderRequestHook
	BeforeProviderHeaders BeforeProviderHeadersHook
	AfterProviderResponse AfterProviderResponseHook
	Agent                 AgentLifecycleHook
	Turn                  TurnLifecycleHook
	Message               MessageHook
	// MessageHandlers follow Message in registration order. The slice models
	// the original runner's multiple callbacks while Message preserves the
	// existing single-host adapter surface.
	MessageHandlers     []MessageHook
	ExtensionError      ExtensionErrorHook
	ToolExecution       ToolExecutionLifecycleHook
	ModelSelect         ModelSelectHook
	ThinkingLevelSelect ThinkingLevelSelectHook
	ToolCall            BeforeToolCallHook
	ToolResult          AfterToolCallHook
	// ToolResultHandlers follow ToolResult in registration order. Each handler
	// is isolated like the upstream extension runner: failures are reported and
	// later handlers still receive the last valid result snapshot.
	ToolResultHandlers   []AfterToolCallHook
	SessionStart         SessionStartHook
	SessionInfoChanged   SessionInfoChangedHook
	SessionShutdown      SessionShutdownHook
	SessionBeforeCompact SessionBeforeCompactHook
	SessionCompact       SessionCompactHook
	SessionBeforeTree    SessionBeforeTreeHook
	SessionTree          SessionTreeHook
	SessionBeforeSwitch  SessionBeforeSwitchHook
	SessionBeforeFork    SessionBeforeForkHook
}

func cloneBuildSystemPromptOptions(value BuildSystemPromptOptions) BuildSystemPromptOptions {
	value.CustomPrompt = cloneStringPointer(value.CustomPrompt)
	if value.SelectedTools != nil {
		value.SelectedTools = append([]string{}, value.SelectedTools...)
	}
	value.ToolSnippets = cloneStringValues(value.ToolSnippets)
	if value.PromptGuidelines != nil {
		value.PromptGuidelines = append([]string{}, value.PromptGuidelines...)
	}
	value.AppendSystemPrompt = cloneStringPointer(value.AppendSystemPrompt)
	if value.ContextFiles != nil {
		value.ContextFiles = append([]SystemPromptContextFile{}, value.ContextFiles...)
	}
	if value.Skills != nil {
		value.Skills = append([]SystemPromptSkill{}, value.Skills...)
	}
	for index := range value.Skills {
		value.Skills[index].SourceInfo.BaseDir = cloneStringPointer(value.Skills[index].SourceInfo.BaseDir)
	}
	return value
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneStringValues(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func contextHookTransform(hook ContextHook) AgentContextTransform {
	if hook == nil {
		return nil
	}
	return func(ctx context.Context, messages []agentmsg.Message) (*[]agentmsg.Message, error) {
		result, err := hook(ctx, ContextHookEvent{Messages: agentmsg.Clone(messages)})
		if err != nil {
			return nil, err
		}
		if result.Messages == nil {
			return nil, nil
		}
		clone := agentmsg.Clone(*result.Messages)
		return &clone, nil
	}
}

func composeBeforeToolHooks(first, second BeforeToolCallHook) BeforeToolCallHook {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	return func(ctx context.Context, event BeforeToolCallContext) (BeforeToolCallResult, error) {
		a, err := first(ctx, event)
		if err != nil || a.Block {
			return a, err
		}
		if a.Arguments != nil {
			event.Arguments = append([]byte(nil), (*a.Arguments)...)
			if call, e := llm.NewToolCallBlock(event.ToolCall.ID(), event.ToolCall.Name(), event.Arguments); e == nil {
				event.ToolCall = call
			}
		}
		b, err := second(ctx, event)
		if err != nil {
			return b, err
		}
		if b.Arguments == nil {
			b.Arguments = a.Arguments
		}
		return b, nil
	}
}
func composeAfterToolHooks(first, second AfterToolCallHook) AfterToolCallHook {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	return func(ctx context.Context, event AfterToolCallContext) (AfterToolCallResult, error) {
		a, err := first(ctx, event)
		if err != nil {
			return a, err
		}
		event = applyAfterToolPatch(event, a)
		b, err := second(ctx, event)
		if err != nil {
			return b, err
		}
		if b.Content == nil {
			b.Content = a.Content
		}
		if b.Details == nil {
			b.Details = a.Details
		}
		if b.IsError == nil {
			b.IsError = a.IsError
		}
		if b.Usage == nil {
			b.Usage = a.Usage
		}
		if b.Terminate == nil {
			b.Terminate = a.Terminate
		}
		return b, nil
	}
}

func (s *AgentSession) toolResultTransform() AfterToolCallHook {
	hooks := make([]AfterToolCallHook, 0, 1+len(s.hooks.ToolResultHandlers))
	if s.hooks.ToolResult != nil {
		hooks = append(hooks, s.hooks.ToolResult)
	}
	hooks = append(hooks, s.hooks.ToolResultHandlers...)
	if len(hooks) == 0 {
		return nil
	}
	return func(ctx context.Context, event AfterToolCallContext) (AfterToolCallResult, error) {
		combined := AfterToolCallResult{}
		for index, hook := range hooks {
			patch, err := callAfterToolCallHook(hook, ctx, event)
			if err != nil {
				s.reportExtensionError(ctx, "tool_result", index, err)
				continue
			}
			event = applyAfterToolPatch(event, patch)
			combined = mergeAfterToolPatch(combined, patch)
		}
		return combined, nil
	}
}

func callAfterToolCallHook(hook AfterToolCallHook, ctx context.Context, event AfterToolCallContext) (result AfterToolCallResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("tool_result hook panicked: %s", safeValueText(recovered))
		}
	}()
	return hook(ctx, event)
}

func mergeAfterToolPatch(current, next AfterToolCallResult) AfterToolCallResult {
	if next.Content != nil {
		current.Content = next.Content
	}
	if next.Details != nil {
		current.Details = next.Details
	}
	if next.IsError != nil {
		current.IsError = next.IsError
	}
	if next.Usage != nil {
		current.Usage = next.Usage
	}
	if next.Terminate != nil {
		current.Terminate = next.Terminate
	}
	return current
}

func applyAfterToolPatch(event AfterToolCallContext, patch AfterToolCallResult) AfterToolCallContext {
	if patch.Content != nil {
		event.Result.Content = append([]llm.ToolResultContentBlock(nil), (*patch.Content)...)
		event.Result.Text = ""
	}
	if patch.Details != nil {
		event.Result.Details = *patch.Details
	}
	if patch.Usage != nil {
		usage := *patch.Usage
		event.Result.Usage = &usage
	}
	if patch.Terminate != nil {
		event.Result.Terminate = *patch.Terminate
	}
	if patch.IsError != nil {
		event.IsError = *patch.IsError
	}
	return event
}

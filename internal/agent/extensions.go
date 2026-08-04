package agent

// This file is the extension-neutral core contract. It intentionally models
// lifecycle semantics, values and cancellation without a JS loader, UI, or
// plugin discovery mechanism. A host may adapt this typed surface to any
// extension runtime.

import (
	"context"

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

type BeforeAgentStartEvent struct {
	Prompt, SystemPrompt string
	Messages             []agentmsg.Message
}
type BeforeAgentStartResult struct {
	ExtraMessages []agentmsg.Message
	SystemPrompt  *string
	Cancel        HookCancel
}
type BeforeAgentStartHook func(context.Context, BeforeAgentStartEvent) (BeforeAgentStartResult, error)

// Provider hooks live in provider.StreamOptions at the exact payload/header/
// response boundary. These aliases make that relationship explicit to Agent
// callers while preserving the provider package as the transport owner.
type BeforeProviderRequestHook = provider.PayloadHook
type BeforeProviderResponseHook = provider.ResponseHook

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

type MessageHookEvent struct {
	Type    MessageHookType
	Message agentmsg.Message
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

func (s *AgentSession) messageEndTransform(ctx context.Context, message agentmsg.Message) (agentmsg.Message, error) {
	if s == nil || s.hooks.Message == nil {
		return nil, nil
	}
	// Provider streaming already emitted assistant message_start. Initial user,
	// injected custom, and tool-result messages reach their first observable
	// boundary here, immediately before message_end and persistence.
	if message.Role() != agentmsg.RoleAssistant || s.beginAssistantHookMessage() {
		_, _ = s.hooks.Message(ctx, MessageHookEvent{Type: MessageStartHookEvent, Message: agentmsg.CloneOne(message)})
	}
	result, err := s.hooks.Message(ctx, MessageHookEvent{Type: MessageEndHookEvent, Message: agentmsg.CloneOne(message)})
	if err != nil {
		return nil, err
	}
	if result.Cancel.Cancelled() {
		return nil, ErrAgentAborted
	}
	return agentmsg.CloneOne(result.Message), nil
}

type SessionStartHookEvent struct {
	Reason       SessionStartReason
	PreviousPath string
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
	Reason     SessionShutdownReason
	TargetPath string
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

type SessionCompactHookEvent struct {
	Before       bool
	Reason       CompactionReason
	WillRetry    bool
	Branch       []session.Entry
	Instructions string
}
type SessionCompactHookResult struct {
	Instructions *string
	Cancel       HookCancel
}
type SessionCompactHook func(context.Context, SessionCompactHookEvent) (SessionCompactHookResult, error)
type SessionTreeHookEvent struct {
	Before               bool
	OldLeafID, NewLeafID string
	Branch               []session.Entry
}
type SessionTreeHookResult struct{ Cancel HookCancel }
type SessionTreeHook func(context.Context, SessionTreeHookEvent) (SessionTreeHookResult, error)

// Hooks is a typed, host-provided callback set. No generic maps or string
// dispatch are used. Nil fields deliberately mean the corresponding runtime
// surface is not enabled by this host.
type Hooks struct {
	Context          ContextHook
	BeforeAgentStart BeforeAgentStartHook
	Agent            AgentLifecycleHook
	Message          MessageHook
	ToolCall         BeforeToolCallHook
	ToolResult       AfterToolCallHook
	SessionStart     SessionStartHook
	SessionShutdown  SessionShutdownHook
	SessionCompact   SessionCompactHook
	SessionTree      SessionTreeHook
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
		if b.AddedToolNames == nil {
			b.AddedToolNames = a.AddedToolNames
		}
		if b.Terminate == nil {
			b.Terminate = a.Terminate
		}
		return b, nil
	}
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
	if patch.AddedToolNames != nil {
		event.Result.AddedToolNames = append([]string(nil), (*patch.AddedToolNames)...)
	}
	if patch.Terminate != nil {
		event.Result.Terminate = *patch.Terminate
	}
	if patch.IsError != nil {
		event.IsError = *patch.IsError
	}
	return event
}

package agent

// This file is the extension-neutral core contract. It intentionally models
// lifecycle semantics, values and cancellation without a JS loader, UI, or
// plugin discovery mechanism. A host may adapt this typed surface to any
// extension runtime.

import (
	"context"
	"fmt"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

type HookCancel struct {
	Cancel bool
	Reason string
}

func (r HookCancel) Validate() error {
	if r.Cancel && r.Reason == "" {
		return fmt.Errorf("cancelled hook result requires a reason")
	}
	return nil
}

type ContextHookEvent struct{ Messages []agentmsg.Message }
type ContextHookResult struct{ Messages []agentmsg.Message }
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

type ToolHookEvent struct {
	Type   ToolHookType
	Call   llm.ToolCallBlock
	Result *ToolOutput
}
type ToolHookType string

const (
	ToolStartHookEvent  ToolHookType = "tool_call"
	ToolResultHookEvent ToolHookType = "tool_result"
)

type ToolHookResult struct {
	Result *ToolOutput
	Cancel HookCancel
}
type ToolHook func(context.Context, ToolHookEvent) (ToolHookResult, error)

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
type SessionSwitchHookEvent struct {
	Before     bool
	Reason     SessionStartReason
	TargetPath string
}
type SessionSwitchHookResult struct{ Cancel HookCancel }
type SessionSwitchHook func(context.Context, SessionSwitchHookEvent) (SessionSwitchHookResult, error)

// Hooks is a typed, host-provided callback set. No generic maps or string
// dispatch are used. Nil fields deliberately mean the corresponding runtime
// surface is not enabled by this host.
type Hooks struct {
	Context          ContextHook
	BeforeAgentStart BeforeAgentStartHook
	Agent            AgentLifecycleHook
	Message          MessageHook
	Tool             ToolHook
	SessionStart     SessionStartHook
	SessionShutdown  SessionShutdownHook
	SessionCompact   SessionCompactHook
	SessionTree      SessionTreeHook
	SessionSwitch    SessionSwitchHook
}

func invokeContextHook(ctx context.Context, hook ContextHook, messages []llm.ConversationMessage) ([]llm.ConversationMessage, error) {
	if hook == nil {
		return append([]llm.ConversationMessage(nil), messages...), nil
	}
	wrapped := make([]agentmsg.Message, 0, len(messages))
	for _, message := range messages {
		value, err := agentmsg.NewLLM(message)
		if err != nil {
			return nil, err
		}
		wrapped = append(wrapped, value)
	}
	result, err := hook(ctx, ContextHookEvent{Messages: agentmsg.Clone(wrapped)})
	if err != nil {
		return nil, err
	}
	return agentmsg.ConvertToLLM(agentmsg.Clone(result.Messages))
}

func composeContextHooks(base ContextTransform, hook ContextHook) ContextTransform {
	if base == nil && hook == nil {
		return nil
	}
	return func(ctx context.Context, messages []llm.ConversationMessage) ([]llm.ConversationMessage, error) {
		var err error
		if base != nil {
			messages, err = base(ctx, append([]llm.ConversationMessage(nil), messages...))
			if err != nil {
				return nil, err
			}
		}
		return invokeContextHook(ctx, hook, messages)
	}
}

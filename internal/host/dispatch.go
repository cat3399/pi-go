package host

import (
	"context"
	"fmt"

	"github.com/cat3399/pi-go/internal/agent"
)

func (h *Host) Dispatch(ctx context.Context, command Command) (CommandResult, error) {
	if h == nil {
		return nil, ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if command == nil {
		return nil, fmt.Errorf("%w: nil command", ErrInvalidCommand)
	}
	switch command := command.(type) {
	case PromptCommand:
		return h.dispatchPrompt(ctx, command)
	case AbortCommand:
		session, _, err := h.currentSession()
		if err != nil {
			return nil, err
		}
		if err := session.Abort(ctx); err != nil {
			return nil, err
		}
		return AbortResult{}, nil
	case GetStateCommand:
		state, err := h.State()
		if err != nil {
			return nil, err
		}
		return GetStateResult{State: state}, nil
	case ClearQueueCommand:
		session, _, err := h.currentSession()
		if err != nil {
			return nil, err
		}
		return ClearQueueResult{Queue: session.ClearQueue()}, nil
	case ReloadCommand:
		if _, _, err := h.currentSession(); err != nil {
			return nil, err
		}
		if err := h.runtime.Reload(ctx); err != nil {
			return nil, err
		}
		return ReloadResult{}, nil
	case SteerCommand:
		return h.dispatchSteer(command)
	case FollowUpCommand:
		return h.dispatchFollowUp(command)
	case SetModelCommand:
		return h.dispatchSetModel(ctx, command)
	case ForkCommand:
		return h.dispatchFork(ctx, command)
	case NavigateTreeCommand:
		return h.dispatchNavigateTree(ctx, command)
	case SetThinkingLevelCommand:
		return h.dispatchSetThinkingLevel(command)
	case CompactCommand:
		return h.dispatchCompact(ctx, command)
	case AbortCompactionCommand:
		return h.dispatchAbortCompaction()
	case SetSessionNameCommand:
		return h.dispatchSetSessionName(ctx, command)
	case GetSessionStatsCommand:
		return h.dispatchGetSessionStats()
	case GetLastAssistantTextCommand:
		return h.dispatchGetLastAssistantText()
	case SetAutoCompactionCommand:
		return h.dispatchSetAutoCompaction(command)
	case SetAutoRetryCommand:
		return h.dispatchSetAutoRetry(command)
	case GetToolsCommand:
		return h.dispatchGetTools()
	case SetToolsCommand:
		return h.dispatchSetTools(command)
	case BashCommand:
		return h.dispatchBash(ctx, command)
	case AbortBashCommand:
		return h.dispatchAbortBash()
	case GetCommandsCommand:
		return h.dispatchGetCommands()
	default:
		return nil, fmt.Errorf("%w: %s", ErrInvalidCommand, command.Type())
	}
}

func (h *Host) beginPrompt() (uint64, *agent.AgentSession, error) {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	if h.closed || h.closing {
		return 0, nil, ErrClosed
	}
	h.bindMu.RLock()
	session := h.session
	h.bindMu.RUnlock()
	if session == nil {
		return 0, nil, ErrSessionUnavailable
	}
	h.nextOpID++
	operationID := h.nextOpID
	h.promptCount++
	h.operations.Add(1)
	return operationID, session, nil
}

func (h *Host) finishPromptState() {
	h.lifecycleMu.Lock()
	if h.promptCount > 0 {
		h.promptCount--
	}
	h.lifecycleMu.Unlock()
}

func (h *Host) dispatchPrompt(ctx context.Context, command PromptCommand) (CommandResult, error) {
	operationID, session, err := h.beginPrompt()
	if err != nil {
		return nil, err
	}
	preflight := make(chan bool, 1)
	outcome := make(chan error, 1)
	sessionID := session.SessionManager().SessionID()
	go func() {
		defer h.operations.Done()
		_, promptErr := session.PromptWithOptions(h.ctx, command.Message, agent.PromptOptions{
			Images:            command.Images,
			StreamingBehavior: command.StreamingBehavior,
			Source:            agent.InputRPC,
			PreflightResult: func(success bool) {
				preflight <- success
			},
		})
		h.finishPromptState()
		if promptErr != nil {
			h.enqueue(context.Background(), sessionID, PromptErrorEvent{OperationID: operationID, Message: promptErr.Error()})
		}
		// pi-web's prompt_done represents a top-level prompt command. Queued
		// steer/follow-up prompts deliberately do not terminate that active UI
		// stage, matching its existing wrapper contract.
		if command.StreamingBehavior == "" {
			h.enqueue(context.Background(), sessionID, PromptDoneEvent{OperationID: operationID})
		}
		outcome <- promptErr
	}()

	select {
	case accepted := <-preflight:
		if accepted {
			return PromptAcceptedResult{OperationID: operationID}, nil
		}
		select {
		case promptErr := <-outcome:
			if promptErr == nil {
				return nil, fmt.Errorf("%w: prompt preflight failed without an error", ErrInvalidCommand)
			}
			return nil, promptErr
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	case promptErr := <-outcome:
		if promptErr == nil {
			return nil, fmt.Errorf("%w: prompt completed without preflight acknowledgement", ErrInvalidCommand)
		}
		return nil, promptErr
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

func (h *Host) promptRunning() bool {
	h.lifecycleMu.Lock()
	running := h.promptCount != 0
	h.lifecycleMu.Unlock()
	return running
}

// State samples the current AgentSession directly and retries once if Runtime
// replaces/rebinds it during the read. It never derives product state by
// replaying the Host event stream.
func (h *Host) State() (State, error) {
	for attempt := 0; attempt < 2; attempt++ {
		session, generation, err := h.currentSession()
		if err != nil {
			return State{}, err
		}
		state, err := snapshotSession(session, h.promptRunning())
		if err != nil {
			if h.sameBinding(session, generation) {
				return State{}, err
			}
			continue
		}
		if h.sameBinding(session, generation) {
			return state, nil
		}
	}
	return State{}, ErrSessionChanged
}

func snapshotSession(session *agent.AgentSession, promptRunning bool) (State, error) {
	manager := session.SessionManager()
	configuration := session.State()
	activity := session.Activity()
	queue := session.PendingQueue()
	contextUsage, hasContextUsage, err := session.GetContextUsage()
	if err != nil {
		return State{}, err
	}
	state := State{
		SessionID: manager.SessionID(), CWD: manager.Cwd(),
		Model: configuration.Model, HasModel: configuration.HasModel,
		ThinkingLevel: configuration.ThinkingLevel, SystemPrompt: configuration.SystemPrompt,
		Phase: activity.Phase, IsStreaming: activity.IsStreaming,
		IsPromptRunning: promptRunning, IsBashRunning: session.IsBashRunning(),
		IsCompacting: activity.IsCompacting, RetryAttempt: activity.RetryAttempt, RetryWaiting: activity.RetryWaiting,
		SteeringMode: session.SteeringMode(), FollowUpMode: session.FollowUpMode(),
		AutoCompactionEnabled: session.AutoCompactionEnabled(), AutoRetryEnabled: session.AutoRetryEnabled(),
		MessageCount: len(configuration.Active.Messages()), PendingMessageCount: len(queue.SteeringMessages) + len(queue.FollowUpMessages),
		QueuedMessages: queue,
	}
	if path, ok := manager.SessionFile(); ok {
		state.SessionFile = stringPointer(path)
	}
	if name, ok := manager.SessionName(); ok {
		state.SessionName = stringPointer(name)
	}
	if hasContextUsage {
		state.ContextUsage = &contextUsage
	}
	return state, nil
}

func stringPointer(value string) *string {
	copy := value
	return &copy
}

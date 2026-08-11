package application

import (
	"context"
	"fmt"

	"github.com/cat3399/pi-go/internal/agent"
)

func (s *ApplicationSession) Dispatch(ctx context.Context, command Command) (CommandResult, error) {
	if s == nil {
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
		return s.dispatchPrompt(ctx, command)
	case AbortCommand:
		session, _, err := s.currentSession()
		if err != nil {
			return nil, err
		}
		if err := session.Abort(ctx); err != nil {
			return nil, err
		}
		return AbortResult{}, nil
	case GetStateCommand:
		state, err := s.State()
		if err != nil {
			return nil, err
		}
		return GetStateResult{State: state}, nil
	case ClearQueueCommand:
		session, _, err := s.currentSession()
		if err != nil {
			return nil, err
		}
		return ClearQueueResult{Queue: session.ClearQueue()}, nil
	case ReloadCommand:
		if _, _, err := s.currentSession(); err != nil {
			return nil, err
		}
		if err := s.runtime.Reload(ctx); err != nil {
			return nil, err
		}
		return ReloadResult{}, nil
	case SteerCommand:
		return s.dispatchSteer(command)
	case FollowUpCommand:
		return s.dispatchFollowUp(command)
	case SetModelCommand:
		return s.dispatchSetModel(ctx, command)
	case ForkCommand:
		return s.dispatchFork(ctx, command)
	case NavigateTreeCommand:
		return s.dispatchNavigateTree(ctx, command)
	case SetThinkingLevelCommand:
		return s.dispatchSetThinkingLevel(command)
	case CompactCommand:
		return s.dispatchCompact(ctx, command)
	case AbortCompactionCommand:
		return s.dispatchAbortCompaction()
	case SetSessionNameCommand:
		return s.dispatchSetSessionName(ctx, command)
	case GetSessionStatsCommand:
		return s.dispatchGetSessionStats()
	case GetLastAssistantTextCommand:
		return s.dispatchGetLastAssistantText()
	case SetAutoCompactionCommand:
		return s.dispatchSetAutoCompaction(command)
	case SetAutoRetryCommand:
		return s.dispatchSetAutoRetry(command)
	case GetToolsCommand:
		return s.dispatchGetTools()
	case SetToolsCommand:
		return s.dispatchSetTools(command)
	case BashCommand:
		return s.dispatchBash(ctx, command)
	case AbortBashCommand:
		return s.dispatchAbortBash()
	case GetCommandsCommand:
		return s.dispatchGetCommands()
	default:
		return nil, fmt.Errorf("%w: %s", ErrInvalidCommand, command.Type())
	}
}

func (s *ApplicationSession) beginPrompt() (uint64, *agent.AgentSession, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closed || s.closing {
		return 0, nil, ErrClosed
	}
	s.bindMu.RLock()
	session := s.session
	s.bindMu.RUnlock()
	if session == nil {
		return 0, nil, ErrSessionUnavailable
	}
	s.nextOpID++
	operationID := s.nextOpID
	s.promptCount++
	s.operations.Add(1)
	return operationID, session, nil
}

func (s *ApplicationSession) finishPromptState() {
	s.lifecycleMu.Lock()
	if s.promptCount > 0 {
		s.promptCount--
	}
	s.lifecycleMu.Unlock()
}

func (s *ApplicationSession) dispatchPrompt(ctx context.Context, command PromptCommand) (CommandResult, error) {
	source := command.Source
	if source == "" {
		source = agent.InputInteractive
	}
	if !source.Valid() {
		return nil, fmt.Errorf("%w: invalid prompt source %q", ErrInvalidCommand, source)
	}
	operationID, session, err := s.beginPrompt()
	if err != nil {
		return nil, err
	}
	preflight := make(chan bool, 1)
	outcome := make(chan error, 1)
	sessionID := session.SessionManager().SessionID()
	go func() {
		defer s.operations.Done()
		accepted := false
		_, promptErr := session.PromptWithOptions(s.ctx, command.Message, agent.PromptOptions{
			Images:            command.Images,
			StreamingBehavior: command.StreamingBehavior,
			Source:            source,
			PreflightResult: func(success bool) {
				accepted = success
				preflight <- success
			},
		})
		s.finishPromptState()
		if accepted {
			operation := OperationEvent{
				OperationID: operationID,
				Command:     CommandPrompt,
				Status:      OperationCompleted,
			}
			if promptErr != nil {
				operation.Status = OperationFailed
				operation.Error = promptErr.Error()
			}
			s.enqueue(context.Background(), sessionID, operation)
		}
		outcome <- promptErr
	}()

	select {
	case accepted := <-preflight:
		if accepted {
			return PromptStartedResult{OperationID: operationID}, nil
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

func (s *ApplicationSession) promptRunning() bool {
	s.lifecycleMu.Lock()
	running := s.promptCount != 0
	s.lifecycleMu.Unlock()
	return running
}

// State samples the current AgentSession directly and retries once if Runtime
// replaces/rebinds it during the read. It never derives product state by
// replaying the ApplicationSession event stream.
func (s *ApplicationSession) State() (State, error) {
	for attempt := 0; attempt < 2; attempt++ {
		session, generation, err := s.currentSession()
		if err != nil {
			return State{}, err
		}
		state, err := snapshotSession(session, s.promptRunning())
		if err != nil {
			if s.sameBinding(session, generation) {
				return State{}, err
			}
			continue
		}
		if s.sameBinding(session, generation) {
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

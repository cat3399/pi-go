package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/session"
)

type ContextChangeRequest struct {
	ID           string `json:"requestId,omitempty"`
	Mode         string `json:"mode"` // handoff or compact
	Handoff      string `json:"handoff,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

type ContextChangeResult struct {
	RequestID string `json:"requestId"`
	Mode      string `json:"mode"`
	State     string `json:"state"` // accepted, completed, failed, cancelled
	WindowID  string `json:"window,omitempty"`
	Error     string `json:"error,omitempty"`
}

type pendingContextChange struct {
	request ContextChangeRequest
	run     *sessionRun
	started bool
}

type ContextStatus struct {
	SessionID       string               `json:"sessionId"`
	WindowID        string               `json:"window"`
	EstimatedTokens uint64               `json:"estimatedTokens"`
	ContextWindow   uint64               `json:"contextWindow"`
	ReserveTokens   uint64               `json:"reserveTokens"`
	RequestBudget   uint64               `json:"requestBudget"`
	ReminderTokens  uint64               `json:"reminderTokens"`
	AutoCompaction  bool                 `json:"autoCompaction"`
	Change          *ContextChangeResult `json:"change,omitempty"`
}

// ConfigureContextAccess installs a host-owned access endpoint and its short
// model-facing instructions. The core only owns its lifetime and per-call tool
// environment; it does not know a transport, address format or CLI protocol.
func (s *AgentSession) ConfigureContextAccess(instructions string, environment map[string]string, closer io.Closer) error {
	s.lifecycleMu.Lock()
	defer s.unlockLifecycle()
	if s.closed || s.closing {
		return session.ErrClosed
	}
	if s.run != nil || s.standaloneMutation || s.contextAccessCloser != nil {
		return ErrBusy
	}
	s.contextAccessInstructions = instructions
	s.contextAccessEnvironment = make(map[string]string, len(environment))
	for key, value := range environment {
		s.contextAccessEnvironment[key] = value
	}
	s.contextAccessCloser = closer
	return nil
}

func (s *AgentSession) ContextStatus() (ContextStatus, error) {
	if err := s.rejectIfClosed(); err != nil {
		return ContextStatus{}, err
	}
	state := s.State()
	windowID, err := s.sessionManager.CurrentContextWindow()
	if err != nil {
		return ContextStatus{}, err
	}
	result := ContextStatus{SessionID: s.sessionManager.SessionID(), WindowID: windowID, AutoCompaction: s.AutoCompactionEnabled()}
	result.ContextWindow, result.ReserveTokens = s.compactionLimitsFor(state.Model)
	if result.ContextWindow > result.ReserveTokens {
		result.RequestBudget = result.ContextWindow - result.ReserveTokens
	}
	result.ReminderTokens = result.RequestBudget / 5 * 4
	estimate, err := session.EstimateAgentContextTokens(state.Active.Messages())
	if err != nil {
		return ContextStatus{}, err
	}
	// Provider usage already includes prompt/schema costs. Compare it with a
	// complete local estimate instead of counting that overhead twice. After
	// a handoff there is no previous-window usage in the active messages.
	local, err := session.EstimateAgentMessagesTokens(state.Active.Messages())
	if err != nil {
		return ContextStatus{}, err
	}
	instructions, _ := s.contextAccessSnapshot()
	overhead := len(state.SystemPrompt) + len(instructions)
	for _, definition := range state.Tools {
		overhead += len(definition.Name()) + len(definition.Description()) + len(definition.ParametersJSON())
	}
	result.EstimatedTokens = max(estimate.Tokens, local+uint64((overhead+3)/4))
	s.lifecycleMu.Lock()
	if s.contextRequest != nil {
		value := acceptedContextChange(s.contextRequest.request)
		result.Change = &value
	} else if s.lastContextChange != nil {
		value := *s.lastContextChange
		result.Change = &value
	}
	s.unlockLifecycle()
	return result, nil
}

func acceptedContextChange(request ContextChangeRequest) ContextChangeResult {
	return ContextChangeResult{RequestID: request.ID, Mode: request.Mode, State: "accepted"}
}

// RequestContextChange accepts an active-run request without waiting for the
// requesting tool to finish. Idle callers execute through the same session
// admission and persistence boundary as manual compaction.
func (s *AgentSession) RequestContextChange(ctx context.Context, request ContextChangeRequest) (ContextChangeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cause := context.Cause(ctx); cause != nil {
		return ContextChangeResult{}, cause
	}
	if request.ID == "" {
		var bytes [16]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			return ContextChangeResult{}, err
		}
		request.ID = hex.EncodeToString(bytes[:])
	}
	if request.Mode != "handoff" && request.Mode != "compact" {
		return ContextChangeResult{}, fmt.Errorf("invalid context change mode %q", request.Mode)
	}
	if request.Mode == "handoff" {
		if err := session.ValidateContextHandoff(request.ID, request.Handoff); err != nil {
			return ContextChangeResult{}, err
		}
	}
	if err := s.rejectIfClosed(); err != nil {
		return ContextChangeResult{}, err
	}
	if request.Mode == "handoff" {
		path, err := s.sessionManager.BranchPath("")
		if err != nil {
			return ContextChangeResult{}, err
		}
		for _, entry := range path {
			if previous, ok := entry.ContextHandoff(); ok && previous.RequestID == request.ID {
				if previous.Text != request.Handoff {
					return ContextChangeResult{}, fmt.Errorf("handoff request ID already has different content")
				}
				return ContextChangeResult{RequestID: request.ID, Mode: request.Mode, State: "completed", WindowID: entry.ID()}, nil
			}
		}
	}
	s.lifecycleMu.Lock()
	if pending := s.contextRequest; pending != nil {
		s.unlockLifecycle()
		if pending.request == request {
			return acceptedContextChange(request), nil
		}
		return ContextChangeResult{}, fmt.Errorf("%w: another context change is pending", ErrBusy)
	}
	if previous := s.lastContextChange; request.Mode == "compact" && previous != nil && previous.Mode == request.Mode && previous.RequestID == request.ID && previous.State == "completed" {
		result := *previous
		s.unlockLifecycle()
		return result, nil
	}
	if s.closed || s.closing {
		s.unlockLifecycle()
		return ContextChangeResult{}, session.ErrClosed
	}
	if run := s.run; run != nil {
		if !run.agentPipeline || !run.acceptingQueues || context.Cause(run.ctx) != nil {
			s.unlockLifecycle()
			return ContextChangeResult{}, ErrBusy
		}
		s.contextRequest = &pendingContextChange{request: request, run: run}
		s.unlockLifecycle()
		return acceptedContextChange(request), nil
	}
	if s.standaloneMutation {
		s.unlockLifecycle()
		return ContextChangeResult{}, ErrBusy
	}
	// Admit while still holding the same lock so a prompt cannot slip between
	// the idle check and the context mutation.
	run := s.admitSessionRunLocked(ctx, false)
	pending := &pendingContextChange{request: request, run: run, started: true}
	s.contextRequest = pending
	s.unlockLifecycle()
	defer s.finishSessionRun(run)
	return s.executeContextChange(ctx, pending)
}

func (s *AgentSession) prepareContextChange(ctx context.Context, turn AgentLoopTurnContext) (*AgentLoopTurnUpdate, error) {
	s.lifecycleMu.Lock()
	pending := s.contextRequest
	if pending == nil || pending.started || pending.run != s.run || context.Cause(ctx) != nil {
		s.unlockLifecycle()
		return nil, nil
	}
	pending.started = true
	s.unlockLifecycle()
	if _, err := s.executeContextChange(ctx, pending); err != nil {
		return nil, err
	}
	// Publish only messages at request preparation. A full after-turn context
	// override would also pin old configuration over a live reload.
	s.lifecycleMu.Lock()
	s.contextRebuildPending = true
	s.unlockLifecycle()
	return nil, nil
}

func (s *AgentSession) executeContextChange(ctx context.Context, pending *pendingContextChange) (result ContextChangeResult, resultErr error) {
	request := pending.request
	result = acceptedContextChange(request)
	defer func() {
		if resultErr != nil {
			result.State, result.Error = "failed", resultErr.Error()
			if context.Cause(ctx) != nil {
				result.State = "cancelled"
			}
		}
		s.lifecycleMu.Lock()
		if s.contextRequest == pending {
			s.contextRequest = nil
		}
		copy := result
		s.lastContextChange = &copy
		s.unlockLifecycle()
	}()
	if request.Mode == "compact" {
		compacted, err := s.manualCompaction(pending.run, request.Instructions)
		if err != nil {
			return result, err
		}
		result.State, result.WindowID = "completed", compacted.Entry.ID()
		return result, nil
	}
	status, err := s.ContextStatus()
	if err != nil {
		return result, err
	}
	entry, err := s.sessionManager.CommitContextHandoff(ctx, request.ID, request.Handoff, status.EstimatedTokens)
	if err != nil {
		return result, fmt.Errorf("%w: context handoff: %w", ErrTranscriptCommit, err)
	}
	if err := s.reloadAgentMessagesFromSession(); err != nil {
		return result, err
	}
	s.lifecycleMu.Lock()
	if s.run == pending.run {
		// Overflow recovery is bounded per context, not forever across every
		// handoff in the same long-running Agent operation.
		pending.run.overflowCompacted = false
		pending.run.thresholdCompactionAttempted = false
	}
	s.unlockLifecycle()
	result.State, result.WindowID = "completed", entry.ID()
	s.mu.RLock()
	var observers []SessionObserver
	for _, observer := range s.observers {
		if observer.observer != nil {
			observers = append(observers, observer.observer)
		}
	}
	s.mu.RUnlock()
	s.emitToObservers(ctx, observers, EntryAppendedEvent{Entry: entry})
	return result, nil
}

// Unconsumed requests belong to their originating run, never to a later
// unrelated user prompt after cancellation or a failed tool batch.
func (s *AgentSession) cancelContextChangeLocked(run *sessionRun) {
	if pending := s.contextRequest; pending != nil && pending.run == run {
		result := acceptedContextChange(pending.request)
		result.State, result.Error = "cancelled", "run ended before the context change boundary"
		s.lastContextChange, s.contextRequest = &result, nil
	}
}

func (s *AgentSession) contextAccessSnapshot() (string, map[string]string) {
	s.lifecycleMu.Lock()
	defer s.unlockLifecycle()
	environment := make(map[string]string, len(s.contextAccessEnvironment))
	for key, value := range s.contextAccessEnvironment {
		environment[key] = value
	}
	return s.contextAccessInstructions, environment
}

func (s *AgentSession) prepareContextReminder(snapshot *TurnSnapshot, messages []agentmsg.Message) error {
	instructions, _ := s.contextAccessSnapshot()
	if instructions == "" {
		return nil
	}
	snapshot.SystemPrompt += "\n\n" + instructions
	status, err := s.ContextStatus()
	if err != nil {
		return err
	}
	if !status.AutoCompaction || status.ReminderTokens == 0 || status.EstimatedTokens < status.ReminderTokens {
		return nil
	}
	key := fmt.Sprintf("%s/%s/%s/%d/%d", status.WindowID, snapshot.Model.Provider(), snapshot.Model.ID(), status.ContextWindow, status.ReserveTokens)
	s.lifecycleMu.Lock()
	if s.contextReminderKey == key {
		s.unlockLifecycle()
		return nil
	}
	s.contextReminderKey = key
	s.unlockLifecycle()
	text := fmt.Sprintf("Context usage is approximately %d tokens; request budget is %d with %d reserved. If the task is almost finished, finish normally. Otherwise you can submit a short handoff and start a new context window through the session context CLI described in the system instructions. Read its help for options. Automatic summary compaction remains the budget fallback. This is a system notice, not user input.", status.EstimatedTokens, status.RequestBudget, status.ReserveTokens)
	notice, err := agentmsg.NewCustom(agentmsg.CustomSpec{CustomType: "pi-go.context-notice", StringContent: &text, At: time.Now()})
	if err != nil {
		return err
	}
	if snapshot.Messages != nil {
		messages = snapshot.Messages
	}
	snapshot.Messages = append(agentmsg.Clone(messages), notice)
	return s.loop.SetMessages(snapshot.Messages)
}

func closeContextAccess(closer io.Closer) error {
	if closer == nil {
		return nil
	}
	return closer.Close()
}

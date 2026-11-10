package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/session"
)

// A manual request is an operation awaiting the same tool-submitted handoff
// used by an ordinary task. It never owns a separate model invocation.
type contextCompactionRequest struct {
	request ContextChangeRequest
	run     *sessionRun
	domain  *runCancellation
	done    chan struct{}
	result  session.CompactResult
	err     error
}

func newContextRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *AgentSession) requestContextCompaction(ctx context.Context, request ContextChangeRequest) (ContextChangeResult, *contextCompactionRequest, error) {
	if err := s.rejectIfClosed(); err != nil {
		return ContextChangeResult{}, nil, err
	}
	if cause := context.Cause(ctx); cause != nil {
		return ContextChangeResult{}, nil, cause
	}
	if err := s.requireModelAccess(ctx); err != nil {
		return ContextChangeResult{}, nil, err
	}
	if len(s.loop.State().Messages()) == 0 {
		return ContextChangeResult{}, nil, session.ErrNothingToCompact
	}
	access, _ := s.contextAccessSnapshot()
	if access.Handoff == "" {
		return ContextChangeResult{}, nil, fmt.Errorf("%w: context handoff access is not configured", ErrCompactionUnavailable)
	}
	if request.ID == "" {
		var err error
		request.ID, err = newContextRequestID()
		if err != nil {
			return ContextChangeResult{}, nil, err
		}
	}
	text := "A context handoff has been requested. Use your existing tools to submit a brief handoff through the CLI, with current progress, relevant files or references, and the next direction. Original history remains available, so keep the handoff concise. " + access.Handoff
	if request.Instructions != "" {
		text += "\nAdditional focus: " + request.Instructions
	}
	details, _ := json.Marshal(request.ID)
	message, err := agentmsg.NewCustom(agentmsg.CustomSpec{CustomType: "pi-go.context-request", StringContent: &text, Details: details, At: time.Now()})
	if err != nil {
		return ContextChangeResult{}, nil, err
	}

	s.lifecycleMu.Lock()
	if s.closed || s.closing {
		s.unlockLifecycle()
		return ContextChangeResult{}, nil, session.ErrClosed
	}
	if pending := s.contextCompaction; pending != nil {
		s.unlockLifecycle()
		if pending.request == request {
			return acceptedContextChange(request), pending, nil
		}
		return ContextChangeResult{}, nil, fmt.Errorf("%w: another context handoff is requested", ErrBusy)
	}
	if previous := s.lastContextChange; previous != nil && previous.Mode == "compact" && previous.RequestID == request.ID && previous.State == "completed" {
		value := *previous
		s.unlockLifecycle()
		return value, nil, nil
	}
	if s.standaloneMutation || s.contextRequest != nil {
		s.unlockLifecycle()
		return ContextChangeResult{}, nil, ErrBusy
	}
	run := s.run
	idle := run == nil
	if !idle && (!run.agentPipeline || !run.acceptingQueues || context.Cause(run.ctx) != nil) {
		s.unlockLifecycle()
		return ContextChangeResult{}, nil, ErrBusy
	}
	if idle {
		run = s.admitSessionRunLocked(ctx, true)
		run.stopAfterHandoff = true
	}
	pending := &contextCompactionRequest{request: request, run: run, done: make(chan struct{})}
	s.contextCompaction = pending
	if !idle {
		err = s.loop.enqueueAgentMessage(message, true)
		if err != nil {
			s.contextCompaction = nil
		}
	}
	s.unlockLifecycle()
	if err != nil {
		return ContextChangeResult{}, nil, err
	}
	if !idle {
		s.emitControl(ctx, "queue_update")
		return acceptedContextChange(request), pending, nil
	}
	_, runErr := s.runAdmittedSession(run, false, func() (sessionPromptInput, error) {
		return sessionPromptInput{Messages: []agentmsg.Message{message}}, nil
	}, nil, nil, func(ctx context.Context, input sessionPromptInput, _ []agentmsg.Message) (Result, error) {
		return s.loop.RunAgentMessages(ctx, input.Messages)
	})
	result := acceptedContextChange(request)
	result.State, result.WindowID = "completed", pending.result.Entry.ID()
	if pending.err != nil {
		result.State, result.Error = "failed", pending.err.Error()
		if errors.Is(pending.err, errCompactionCancelled) || errors.Is(pending.err, context.Canceled) || errors.Is(pending.err, ErrAgentAborted) {
			result.State = "cancelled"
		}
		return result, pending, pending.err
	}
	return result, pending, runErr
}

func (s *AgentSession) startContextCompaction(requestID string) error {
	s.lifecycleMu.Lock()
	pending := s.contextCompaction
	if pending == nil || pending.run != s.run || pending.request.ID != requestID {
		s.unlockLifecycle()
		return nil
	}
	if pending.domain != nil {
		cause := context.Cause(pending.domain.ctx)
		s.unlockLifecycle()
		return cause
	}
	ctx, cancel := context.WithCancelCause(pending.run.ctx)
	pending.domain = &runCancellation{ctx: ctx, cancel: cancel}
	pending.run.compaction = pending.domain
	pending.run.phase = PhaseCompacting
	s.unlockLifecycle()
	s.emitCompaction(ctx, "compaction_start", CompactionManual, nil, false, false, "")
	return context.Cause(ctx)
}

func (s *AgentSession) contextCompactionCause() error {
	s.lifecycleMu.Lock()
	defer s.unlockLifecycle()
	if pending := s.contextCompaction; pending != nil && pending.domain != nil {
		return context.Cause(pending.domain.ctx)
	}
	return nil
}

func (s *AgentSession) finishContextCompaction(pending *contextCompactionRequest, result session.CompactResult, err error) {
	s.lifecycleMu.Lock()
	if s.contextCompaction != pending {
		s.unlockLifecycle()
		return
	}
	domain := pending.domain
	if domain != nil && !result.Committed && context.Cause(domain.ctx) != nil {
		err = context.Cause(domain.ctx)
	}
	if errors.Is(err, errExtensionCompactionCancelled) {
		err = ErrAgentAborted
	}
	pending.result, pending.err = result, err
	status := acceptedContextChange(pending.request)
	status.State, status.WindowID = "completed", result.Entry.ID()
	aborted := errors.Is(err, errCompactionCancelled) || errors.Is(err, context.Canceled) || errors.Is(err, ErrAgentAborted) || errors.Is(err, errExtensionCompactionCancelled)
	if err != nil {
		status.State, status.Error = "failed", err.Error()
		if aborted {
			status.State = "cancelled"
		}
	}
	s.lastContextChange, s.contextCompaction = &status, nil
	if pending.run.compaction == domain {
		pending.run.compaction = nil
	}
	s.unlockLifecycle()
	matches := func(message agentmsg.Message) bool {
		custom, ok := message.(agentmsg.Custom)
		if !ok || custom.CustomType() != "pi-go.context-request" {
			return false
		}
		var id string
		return json.Unmarshal(custom.Details(), &id) == nil && id == pending.request.ID
	}
	if s.loop.discardQueuedAgentMessages(matches) {
		s.emitControl(context.Background(), "queue_update")
	}
	if !result.Committed {
		messages := s.loop.State().Messages()
		kept := make([]agentmsg.Message, 0, len(messages))
		for _, message := range messages {
			if !matches(message) {
				kept = append(kept, message)
			}
		}
		if len(kept) != len(messages) {
			_ = s.loop.SetMessages(kept)
		}
	}
	if domain != nil {
		var compacted *session.CompactResult
		message := ""
		if result.Committed {
			compacted = &result
		} else if !aborted {
			message = safeCompactionEventError(err)
		}
		s.emitCompaction(domain.ctx, "compaction_end", CompactionManual, compacted, aborted, false, message)
		domain.cancel(context.Canceled)
	}
	close(pending.done)
}

func (s *AgentSession) settleContextCompaction(run *sessionRun, runErr error) {
	s.lifecycleMu.Lock()
	pending := s.contextCompaction
	s.unlockLifecycle()
	if pending == nil || pending.run != run {
		return
	}
	if runErr == nil {
		runErr = fmt.Errorf("agent ended without submitting a context handoff through the CLI")
	}
	if cause := context.Cause(run.ctx); cause != nil {
		runErr = cause
	}
	s.finishContextCompaction(pending, session.CompactResult{}, runErr)
}

func (s *AgentSession) stopAfterContextHandoff(context.Context, AgentLoopTurnContext) (bool, error) {
	s.lifecycleMu.Lock()
	defer s.unlockLifecycle()
	return s.run != nil && s.run.stopAfterHandoff && s.run.handoffComplete, nil
}

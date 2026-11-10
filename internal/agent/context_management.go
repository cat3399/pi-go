package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
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

type providedHandoffWriter struct{ text string }

func (w providedHandoffWriter) Summarize(context.Context, session.SummaryInput) (session.SummaryOutput, error) {
	return session.SummaryOutput{Text: w.text}, nil
}

type ContextStatus struct {
	SessionID       string `json:"sessionId"`
	WindowID        string `json:"window"`
	EstimatedTokens uint64 `json:"estimatedTokens"`
	ContextWindow   uint64 `json:"contextWindow"`
	RemainingTokens uint64 `json:"remainingTokens"`
	// ReserveTokens and ReminderTokens are remaining-context thresholds.
	// RequestBudget is the derived usage budget retained for status consumers.
	ReserveTokens  uint64               `json:"reserveTokens"`
	RequestBudget  uint64               `json:"requestBudget"`
	ReminderTokens uint64               `json:"reminderTokens"`
	AutoCompaction bool                 `json:"autoCompaction"`
	Change         *ContextChangeResult `json:"change,omitempty"`
}

// ContextAccessInstructions are shown only at their relevant runtime boundary.
type ContextAccessInstructions struct {
	Handoff string
	History string
}

// ConfigureContextAccess installs a host-owned access endpoint. The core owns
// its lifetime and per-call tool environment, but not its transport or protocol.
func (s *AgentSession) ConfigureContextAccess(instructions ContextAccessInstructions, environment map[string]string, closer io.Closer) error {
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
	return s.contextStatusFor(s.State().Active)
}

func validateContextThresholds(reminder, handoff *uint64) error {
	if reminder != nil && handoff != nil && *reminder < *handoff {
		return fmt.Errorf("%w: compaction.reminderAtTokens must not be less than handoffAtTokens (remaining tokens)", ErrInvalidConfig)
	}
	return nil
}

func (s *AgentSession) contextStatusFor(state State) (ContextStatus, error) {
	windowID, err := s.sessionManager.CurrentContextWindow()
	if err != nil {
		return ContextStatus{}, err
	}
	result := ContextStatus{SessionID: s.sessionManager.SessionID(), WindowID: windowID, AutoCompaction: s.AutoCompactionEnabled()}
	window, reserve, reminder, handoff := s.compactionLimitsFor(state.Model())
	result.ContextWindow, result.ReserveTokens = window, reserve
	if handoff != nil {
		result.ReserveTokens = *handoff
	}
	result.RequestBudget = window - min(window, result.ReserveTokens)
	// Express the legacy early-reminder point in remaining tokens too. Only
	// this default conversion uses the historical usage-budget calculation.
	result.ReminderTokens = max(result.ReserveTokens, window-result.RequestBudget/5*4)
	if reminder != nil {
		result.ReminderTokens = *reminder
		if *reminder < result.ReserveTokens {
			return ContextStatus{}, fmt.Errorf("%w: compaction.reminderAtTokens is below the handoff reserve (%d remaining tokens)", ErrInvalidConfig, result.ReserveTokens)
		}
	}
	messages := state.Messages()
	estimate, err := session.EstimateAgentContextTokens(messages)
	if err != nil {
		return ContextStatus{}, err
	}
	// Provider usage already includes prompt/schema costs. Compare it with a
	// complete local estimate instead of counting that overhead twice. After
	// a handoff there is no previous-window usage in the active messages.
	local, err := session.EstimateAgentMessagesTokens(messages)
	if err != nil {
		return ContextStatus{}, err
	}
	// Old v3 compactions can retain assistant messages whose usage describes
	// the previous window. Only new-window usage can anchor this estimate.
	if boundary, ok := s.latestCompactionBoundary(); ok && estimate.LastUsageIndex >= 0 && !messages[estimate.LastUsageIndex].Timestamp().After(boundary) {
		estimate.Tokens = local
	}
	overhead := len(state.SystemPrompt())
	for _, definition := range state.Tools() {
		overhead += len(definition.Name()) + len(definition.Description()) + len(definition.ParametersJSON())
	}
	result.EstimatedTokens = max(estimate.Tokens, local+uint64((overhead+3)/4))
	result.RemainingTokens = window - min(window, result.EstimatedTokens)
	s.lifecycleMu.Lock()
	if s.contextCompaction != nil {
		value := acceptedContextChange(s.contextCompaction.request)
		result.Change = &value
	} else if s.contextRequest != nil {
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
	if request.Mode == "compact" {
		result, _, err := s.requestContextCompaction(ctx, request)
		return result, err
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
				if previous.RequestText() != request.Handoff {
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
	completesManual := false
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
		if !completesManual {
			s.lastContextChange = &copy
		}
		s.unlockLifecycle()
	}()
	s.lifecycleMu.Lock()
	manual := s.contextCompaction
	// An already-in-flight CLI submission cannot consume a later manual
	// request whose reminder is still queued for the model.
	if manual != nil && (manual.run != pending.run || manual.domain == nil) {
		manual = nil
	}
	var domain *runCancellation
	if manual != nil {
		domain = manual.domain
		completesManual = true
	}
	s.unlockLifecycle()
	ownedDomain := domain == nil
	if ownedDomain {
		var ok bool
		domain, ok = s.beginCompaction(pending.run)
		if !ok {
			return result, ErrBusy
		}
		defer s.endCompaction(pending.run, domain)
	}
	s.setSessionPhase(pending.run, PhaseCompacting)
	if ownedDomain {
		s.emitCompaction(domain.ctx, "compaction_start", CompactionManual, nil, false, false, "")
	}
	status, err := s.ContextStatus()
	var compacted session.CompactResult
	if err == nil {
		leaf, _ := s.sessionManager.LeafID()
		input := session.SummaryInput{TokensBefore: status.EstimatedTokens, SelectedLeafID: leaf,
			SystemPrompt: s.loop.State().SystemPrompt(), MessagesToSummarize: s.loop.State().Messages(), ContextWindow: status.ContextWindow}
		input.Messages, err = agentmsg.ConvertToLLM(input.MessagesToSummarize)
		input.FileOperations = session.MessageFileOperations(input.MessagesToSummarize)
		s.mu.RLock()
		keepRecent := s.keepRecentTokens
		s.mu.RUnlock()
		input.Settings = session.CompactionSettings{Enabled: status.AutoCompaction, ReserveTokens: status.ReserveTokens, KeepRecentTokens: keepRecent}
		if manual != nil {
			input.Instructions = manual.request.Instructions
		}
		writer := extensionCompactionSummarizer{session: s, reason: CompactionManual, instructions: input.Instructions, base: providedHandoffWriter{text: request.Handoff}}
		var output session.SummaryOutput
		if err == nil {
			output, err = writer.Summarize(domain.ctx, input)
		}
		if err == nil {
			compacted, err = s.commitHandoff(domain.ctx, request.ID, input, output, request.Handoff)
		}
	}
	if err != nil {
		if manual != nil {
			s.finishContextCompaction(manual, session.CompactResult{}, err)
		}
		aborted := context.Cause(domain.ctx) != nil || errors.Is(err, session.ErrAppendCanceled)
		message := ""
		if !aborted {
			message = safeCompactionEventError(err)
		}
		if ownedDomain {
			s.emitCompaction(domain.ctx, "compaction_end", CompactionManual, nil, aborted, false, message)
		}
		return result, fmt.Errorf("%w: context handoff: %w", ErrTranscriptCommit, err)
	}
	s.afterCompaction(domain.ctx, compacted, CompactionManual, false)
	if manual != nil {
		s.finishContextCompaction(manual, compacted, nil)
	} else {
		s.emitCompaction(domain.ctx, "compaction_end", CompactionManual, &compacted, false, false, "")
	}
	entry := compacted.Entry
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

func (s *AgentSession) contextAccessSnapshot() (ContextAccessInstructions, map[string]string) {
	s.lifecycleMu.Lock()
	defer s.unlockLifecycle()
	environment := make(map[string]string, len(s.contextAccessEnvironment))
	for key, value := range s.contextAccessEnvironment {
		environment[key] = value
	}
	return s.contextAccessInstructions, environment
}

// Capability instructions live at the point of use, never in SystemPrompt.
func (s *AgentSession) prepareContextReminder(snapshot *TurnSnapshot, messages []agentmsg.Message) error {
	instructions, _ := s.contextAccessSnapshot()
	if instructions.Handoff == "" && instructions.History == "" {
		return nil
	}
	status, err := s.ContextStatus()
	if err != nil {
		return err
	}
	if snapshot.Messages != nil {
		messages = snapshot.Messages
	}
	messages = agentmsg.Clone(messages)
	changed := false
	if instructions.History != "" && status.WindowID != session.InitialContextWindow {
		found := false
		for _, message := range messages {
			if custom, ok := message.(agentmsg.Custom); ok && custom.CustomType() == "pi-go.context-history" {
				found = true
			}
		}
		if !found {
			text := fmt.Sprintf("Context window %s. %s", status.WindowID, instructions.History)
			notice, err := agentmsg.NewCustom(agentmsg.CustomSpec{CustomType: "pi-go.context-history", StringContent: &text, At: time.Now()})
			if err != nil {
				return err
			}
			// Place retrieval instructions beside the checkpoint, before new input.
			position := min(1, len(messages))
			messages = append(messages[:position], append([]agentmsg.Message{notice}, messages[position:]...)...)
			changed = true
		}
	}
	if status.AutoCompaction && instructions.Handoff != "" && status.ContextWindow != 0 && status.RemainingTokens <= status.ReminderTokens {
		urgent := status.RemainingTokens <= status.ReserveTokens
		level := uint8(1)
		if urgent {
			level = 2
		}
		key := fmt.Sprintf("%s/%s/%s/%d/%d/%d", status.WindowID, snapshot.Model.Provider(), snapshot.Model.ID(), status.ContextWindow, status.ReserveTokens, status.ReminderTokens)
		s.lifecycleMu.Lock()
		fresh := s.contextReminderKey != key || level > s.contextReminderLevel
		if fresh {
			s.contextReminderKey, s.contextReminderLevel = key, level
		}
		s.unlockLifecycle()
		if fresh {
			text := fmt.Sprintf("This context window has approximately %d tokens remaining. If that is enough to finish the current task, finish and deliver the result. Otherwise, submit a brief handoff before the context fills up and continue in a new window. %s", status.RemainingTokens, instructions.Handoff)
			if urgent {
				text = fmt.Sprintf("Only approximately %d tokens remain in this context window, at or below the handoff threshold. Use your existing tools to submit a brief handoff now, then continue the task in a new window. %s", status.RemainingTokens, instructions.Handoff)
			}
			notice, err := agentmsg.NewCustom(agentmsg.CustomSpec{CustomType: "pi-go.context-notice", StringContent: &text, At: time.Now()})
			if err != nil {
				return err
			}
			messages, changed = append(messages, notice), true
		}
	}
	if !changed {
		return nil
	}
	snapshot.Messages = messages
	return s.loop.SetMessages(messages)
}

func (s *AgentSession) commitHandoff(ctx context.Context, requestID string, input session.SummaryInput, output session.SummaryOutput, submittedText string) (session.CompactResult, error) {
	if requestID == "" {
		var bytes [16]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			return session.CompactResult{}, err
		}
		requestID = hex.EncodeToString(bytes[:])
	}
	if strings.TrimSpace(output.Text) == "" {
		return session.CompactResult{}, fmt.Errorf("%w: empty handoff", session.ErrSummaryFailed)
	}
	path, err := s.sessionManager.BranchPath("")
	if err != nil {
		return session.CompactResult{}, err
	}
	var pending []string
	for i := len(path) - 1; i >= 0; i-- {
		entry := path[i]
		if message, ok := entry.AgentMessage(); ok {
			if standard, ok := message.(agentmsg.LLM); ok {
				if terminal, ok := standard.Conversation().(llm.AssistantTerminal); ok && terminal.FinishReason() != llm.FinishError && terminal.FinishReason() != llm.FinishAborted {
					break
				}
			}
		}
		if handoff, ok := entry.ContextHandoff(); ok {
			pending = append(pending, handoff.PendingEntryIDs...)
			break
		}
		role, _ := entry.MessageRole()
		if role == "user" || entry.Type() == "custom_message" {
			pending = append(pending, entry.ID())
		} else if message, ok := entry.AgentMessage(); ok {
			if bash, ok := message.(agentmsg.BashExecution); ok && !bash.ExcludeFromContext {
				pending = append(pending, entry.ID())
			}
		}
	}
	tokensBefore := input.TokensBefore
	if output.FromExtension {
		tokensBefore = output.TokensBefore
	}
	entry, err := s.sessionManager.CommitContextHandoffWithOptions(ctx, requestID, output.Text, tokensBefore, session.ContextHandoffOptions{
		PendingEntryIDs: pending, SubmittedText: submittedText, Usage: output.Usage, Details: output.Details, ExpectedLeafID: &input.SelectedLeafID,
		FromExtension: output.FromExtension, FirstKeptEntryID: output.FirstKeptEntryID, EstimatedTokensAfter: output.EstimatedTokensAfter,
	})
	if err != nil {
		return session.CompactResult{}, err
	}
	if err := s.reloadAgentMessagesFromSession(); err != nil {
		return session.CompactResult{}, err
	}
	s.lifecycleMu.Lock()
	if s.run != nil {
		s.run.handoffComplete = true
	}
	s.unlockLifecycle()
	estimate, err := session.EstimateAgentMessagesTokens(s.loop.State().Messages())
	if err != nil {
		return session.CompactResult{}, err
	}
	return session.CompactResult{Entry: entry, Input: input, Output: output, EstimatedTokensAfter: estimate, Committed: true}, nil
}

func closeContextAccess(closer io.Closer) error {
	if closer == nil {
		return nil
	}
	return closer.Close()
}

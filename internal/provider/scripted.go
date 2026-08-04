package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
)

const defaultChunkRunes = 4

var (
	ErrInvalidScriptConfig = errors.New("invalid scripted provider config")
	ErrInvalidScriptStep   = errors.New("invalid scripted response step")
	ErrQueueExhausted      = errors.New("scripted provider response queue exhausted")
	ErrRequestAborted      = errors.New("scripted provider request aborted")
)

type Clock func() time.Time

type ScriptedConfig struct {
	// ChunkRunes is the exact maximum number of Unicode code points per text or
	// raw JSON delta. Zero selects a deterministic default; negative is invalid.
	ChunkRunes int
	// Clock timestamps synthetic queue, factory, request, and cancellation errors.
	// Nil is deterministic and returns time.Time{}; wall clock is never implicit.
	Clock Clock
}

type ResponseFactory func(
	context.Context,
	Request,
	uint64,
) (llm.AssistantTerminal, error)

type scriptStepKind uint8

const (
	fixedStep scriptStepKind = iota + 1
	factoryStep
)

// ScriptStep is either one validated terminal response or a factory resolved
// when its stream is first consumed.
type ScriptStep struct {
	kind     scriptStepKind
	terminal llm.AssistantTerminal
	factory  ResponseFactory
}

func FixedResponseStep(terminal llm.AssistantTerminal) (ScriptStep, error) {
	step := ScriptStep{kind: fixedStep, terminal: terminal}
	if err := step.validate(); err != nil {
		return ScriptStep{}, err
	}
	return step, nil
}

func FactoryResponseStep(factory ResponseFactory) (ScriptStep, error) {
	step := ScriptStep{kind: factoryStep, factory: factory}
	if err := step.validate(); err != nil {
		return ScriptStep{}, err
	}
	return step, nil
}

func (s ScriptStep) validate() error {
	switch s.kind {
	case fixedStep:
		if s.factory != nil {
			return fmt.Errorf("%w: fixed step also contains a factory", ErrInvalidScriptStep)
		}
		if err := llm.ValidateAssistantTerminal(s.terminal); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidScriptStep, err)
		}
		return nil
	case factoryStep:
		if s.terminal != nil {
			return fmt.Errorf("%w: factory step also contains a terminal", ErrInvalidScriptStep)
		}
		if s.factory == nil {
			return fmt.Errorf("%w: nil response factory", ErrInvalidScriptStep)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown step kind", ErrInvalidScriptStep)
	}
}

type ScriptedProvider struct {
	mu         sync.Mutex
	queue      []ScriptStep
	requests   []Request
	callCount  uint64
	chunkRunes int
	clock      Clock
}

func NewScriptedProvider(config ScriptedConfig) (*ScriptedProvider, error) {
	if config.ChunkRunes < 0 {
		return nil, fmt.Errorf("%w: chunk runes cannot be negative", ErrInvalidScriptConfig)
	}
	chunkRunes := config.ChunkRunes
	if chunkRunes == 0 {
		chunkRunes = defaultChunkRunes
	}
	clock := config.Clock
	if clock == nil {
		clock = zeroClock
	}
	clock = synchronizedClock(clock)
	return &ScriptedProvider{chunkRunes: chunkRunes, clock: clock}, nil
}

func (p *ScriptedProvider) SetResponses(steps []ScriptStep) error {
	if err := validateSteps(steps); err != nil {
		return err
	}
	p.mu.Lock()
	p.queue = append([]ScriptStep(nil), steps...)
	p.mu.Unlock()
	return nil
}

func (p *ScriptedProvider) AppendResponses(steps []ScriptStep) error {
	if err := validateSteps(steps); err != nil {
		return err
	}
	p.mu.Lock()
	p.queue = append(p.queue, steps...)
	p.mu.Unlock()
	return nil
}

func (p *ScriptedProvider) PendingResponses() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue)
}

func (p *ScriptedProvider) CallCount() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.callCount
}

func (p *ScriptedProvider) Requests() []Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	requests := make([]Request, len(p.requests))
	for index, request := range p.requests {
		requests[index] = request.clone()
	}
	return requests
}

func (p *ScriptedProvider) Stream(ctx context.Context, request Request) EventStream {
	if p == nil {
		return newSyntheticErrorStream(
			context.Background(),
			FailureConfiguration,
			fmt.Errorf("%w: nil scripted provider", ErrInvalidScriptConfig),
			"",
			zeroClock,
		)
	}
	clock := p.clock
	if clock == nil {
		clock = zeroClock
	}
	chunkRunes := p.chunkRunes
	if chunkRunes <= 0 {
		chunkRunes = defaultChunkRunes
	}
	if ctx == nil {
		return newSyntheticErrorStream(
			context.Background(),
			FailureInvalidRequest,
			fmt.Errorf("%w: nil context", ErrInvalidRequest),
			"",
			clock,
		)
	}
	if err := request.validate(); err != nil {
		return newSyntheticErrorStream(ctx, FailureInvalidRequest, err, "", clock)
	}

	p.mu.Lock()
	p.callCount++
	callIndex := p.callCount
	p.requests = append(p.requests, request.clone())
	var step *ScriptStep
	if len(p.queue) != 0 {
		allocated := p.queue[0]
		p.queue[0] = ScriptStep{}
		p.queue = p.queue[1:]
		step = &allocated
	}
	p.mu.Unlock()

	stream := &scriptedStream{
		ctx:        ctx,
		request:    request.clone(),
		callIndex:  callIndex,
		step:       step,
		chunkRunes: chunkRunes,
		clock:      clock,
	}
	if step == nil {
		stream.syntheticKind = FailureQueueExhausted
		stream.syntheticError = ErrQueueExhausted
	}
	return stream
}

func validateSteps(steps []ScriptStep) error {
	for index, step := range steps {
		if err := step.validate(); err != nil {
			return fmt.Errorf("response step %d: %w", index, err)
		}
	}
	return nil
}

type scriptedStream struct {
	ctx            context.Context
	request        Request
	callIndex      uint64
	step           *ScriptStep
	chunkRunes     int
	clock          Clock
	syntheticKind  FailureKind
	syntheticError error
	syntheticText  string
	events         []llm.StreamEvent
	next           int
	initialized    bool
	terminal       bool
	closed         bool
}

func newSyntheticErrorStream(
	ctx context.Context,
	kind FailureKind,
	err error,
	message string,
	clock Clock,
) EventStream {
	if clock == nil {
		clock = zeroClock
	}
	return &scriptedStream{
		ctx:            ctx,
		chunkRunes:     defaultChunkRunes,
		clock:          clock,
		syntheticKind:  kind,
		syntheticError: err,
		syntheticText:  message,
	}
}

func (s *scriptedStream) Next() (llm.StreamEvent, error) {
	if s.closed || s.terminal {
		return nil, io.EOF
	}
	if !s.initialized {
		if err := s.initialize(); err != nil {
			s.closed = true
			return nil, closedStreamError(err)
		}
	}
	if s.ctx.Err() != nil && !s.hasPendingAbortTerminal() {
		event, err := s.cancellationEvent()
		if err != nil {
			s.closed = true
			return nil, closedStreamError(err)
		}
		s.events = []llm.StreamEvent{event}
		s.next = 0
	}
	if s.next >= len(s.events) {
		s.closed = true
		return nil, io.ErrUnexpectedEOF
	}

	event := s.events[s.next]
	s.next++
	switch event.(type) {
	case llm.DoneEvent, llm.ErrorEvent:
		s.terminal = true
	}
	return event, nil
}

func (s *scriptedStream) hasPendingAbortTerminal() bool {
	if s.next != 0 || len(s.events) != 1 {
		return false
	}
	event, ok := s.events[0].(llm.ErrorEvent)
	return ok && event.Reason() == llm.FinishAborted
}

func (s *scriptedStream) Close() error {
	s.closed = true
	s.events = nil
	return nil
}

func (s *scriptedStream) initialize() error {
	s.initialized = true
	if s.ctx.Err() != nil {
		event, err := s.cancellationEvent()
		if err != nil {
			return err
		}
		s.events = []llm.StreamEvent{event}
		return nil
	}
	if s.syntheticError != nil {
		event, err := s.errorEvent(
			llm.FinishError,
			s.syntheticKind,
			s.syntheticError,
			s.syntheticText,
		)
		if err != nil {
			return err
		}
		s.events = []llm.StreamEvent{event}
		return nil
	}

	terminal, failureKind, err := s.resolveStep()
	if s.ctx.Err() != nil {
		event, eventErr := s.cancellationEvent()
		if eventErr != nil {
			return eventErr
		}
		s.events = []llm.StreamEvent{event}
		return nil
	}
	if err != nil {
		event, eventErr := s.errorEvent(llm.FinishError, failureKind, err, "")
		if eventErr != nil {
			return eventErr
		}
		s.events = []llm.StreamEvent{event}
		return nil
	}
	events, err := buildEvents(terminal, s.chunkRunes)
	if err != nil {
		cause := fmt.Errorf("invalid scripted response: %w", err)
		event, eventErr := s.errorEvent(llm.FinishError, FailureInvalidResponse, cause, "")
		if eventErr != nil {
			return eventErr
		}
		s.events = []llm.StreamEvent{event}
		return nil
	}
	s.events = events
	return nil
}

func (s *scriptedStream) resolveStep() (llm.AssistantTerminal, FailureKind, error) {
	if s.step == nil {
		return nil, FailureQueueExhausted, ErrQueueExhausted
	}
	switch s.step.kind {
	case fixedStep:
		terminal, err := llm.WithAssistantProvenance(s.step.terminal, assistantProvenanceForModel(s.request.Model()))
		if err != nil {
			return nil, FailureInvalidResponse, err
		}
		return terminal, 0, nil
	case factoryStep:
		terminal, err := invokeResponseFactory(s.step.factory, s.ctx, s.request.clone(), s.callIndex)
		if err != nil {
			return nil, FailureFactory, err
		}
		if err := llm.ValidateAssistantTerminal(terminal); err != nil {
			return nil, FailureInvalidResponse, fmt.Errorf("factory returned invalid terminal: %w", err)
		}
		terminal, err = llm.WithAssistantProvenance(terminal, assistantProvenanceForModel(s.request.Model()))
		if err != nil {
			return nil, FailureInvalidResponse, err
		}
		return terminal, 0, nil
	default:
		return nil, FailureInvalidResponse, ErrInvalidScriptStep
	}
}

func invokeResponseFactory(
	factory ResponseFactory,
	ctx context.Context,
	request Request,
	callIndex uint64,
) (terminal llm.AssistantTerminal, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			terminal = nil
			err = newFactoryPanicError(recovered)
		}
	}()
	return factory(ctx, request, callIndex)
}

func (s *scriptedStream) cancellationEvent() (llm.ErrorEvent, error) {
	cause := error(ErrRequestAborted)
	if contextCause := context.Cause(s.ctx); contextCause != nil {
		cause = errors.Join(ErrRequestAborted, contextCause)
	}
	return s.errorEvent(
		llm.FinishAborted,
		FailureCancelled,
		cause,
		ErrRequestAborted.Error(),
	)
}

func (s *scriptedStream) errorEvent(
	reason llm.FinishReason,
	kind FailureKind,
	cause error,
	message string,
) (llm.ErrorEvent, error) {
	if message == "" {
		message = normalizeErrorMessage(cause)
	}
	providerFailure, err := NewProviderFailure(ProviderFailureSpec{
		Kind:    kind,
		Message: message,
		Cause:   cause,
	})
	if err != nil {
		return llm.ErrorEvent{}, fmt.Errorf("construct provider failure: %w", err)
	}
	failure, err := llm.NewFailure(providerFailure.Error(), providerFailure)
	if err != nil {
		return llm.ErrorEvent{}, fmt.Errorf("construct terminal failure: %w", err)
	}
	event, err := llm.NewErrorEventWithFailure(reason, failure, llm.Usage{}, s.clock(), assistantProvenanceForModel(s.request.Model()))
	if err != nil {
		return llm.ErrorEvent{}, fmt.Errorf("construct provider terminal error: %w", err)
	}
	return event, nil
}

func normalizeErrorMessage(err error) (message string) {
	message = "scripted provider failed"
	if err != nil {
		defer func() {
			if recover() != nil {
				message = "scripted provider failed"
			}
		}()
		candidate := err.Error()
		if utf8.ValidString(candidate) && strings.TrimSpace(candidate) != "" {
			message = candidate
		}
	}
	return message
}

func zeroClock() time.Time { return time.Time{} }

func synchronizedClock(clock Clock) Clock {
	var mu sync.Mutex
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock()
	}
}

func buildEvents(terminal llm.AssistantTerminal, chunkRunes int) ([]llm.StreamEvent, error) {
	if err := llm.ValidateAssistantTerminal(terminal); err != nil {
		return nil, err
	}
	start, err := llm.NewStartEvent(terminal.AssistantProvenance(), terminal.Timestamp())
	if err != nil {
		return nil, err
	}
	events := []llm.StreamEvent{start}
	for index, block := range terminal.Blocks() {
		switch block := block.(type) {
		case llm.TextBlock:
			start, err := llm.NewTextStartEvent(index)
			if err != nil {
				return nil, err
			}
			events = append(events, start)
			for _, chunk := range splitRunes(block.Text(), chunkRunes) {
				delta, err := llm.NewTextDeltaEvent(index, chunk)
				if err != nil {
					return nil, err
				}
				events = append(events, delta)
			}
			end, err := llm.NewTextEndEvent(index, block.Text())
			if err != nil {
				return nil, err
			}
			events = append(events, end)

		case llm.ToolCallBlock:
			start, err := llm.NewToolCallStartEvent(index, block.ID(), block.Name())
			if err != nil {
				return nil, err
			}
			events = append(events, start)
			for _, chunk := range splitRunes(string(block.ArgumentsJSON()), chunkRunes) {
				delta, err := llm.NewToolCallDeltaEvent(index, []byte(chunk))
				if err != nil {
					return nil, err
				}
				events = append(events, delta)
			}
			end, err := llm.NewToolCallEndEvent(index, block)
			if err != nil {
				return nil, err
			}
			events = append(events, end)
		case llm.ThinkingBlock:
			start, err := llm.NewThinkingStartEvent(index)
			if err != nil {
				return nil, err
			}
			events = append(events, start)
			for _, chunk := range splitRunes(block.Thinking(), chunkRunes) {
				delta, err := llm.NewThinkingDeltaEvent(index, chunk)
				if err != nil {
					return nil, err
				}
				events = append(events, delta)
			}
			end, err := llm.NewThinkingEndEvent(index, block)
			if err != nil {
				return nil, err
			}
			events = append(events, end)

		default:
			return nil, fmt.Errorf("unsupported assistant block %T", block)
		}
	}

	switch terminal := terminal.(type) {
	case llm.AssistantTextMessage, llm.AssistantToolUseMessage, llm.AssistantRichMessage:
		response, hasResponse := terminal.ResponseMetadata()
		var responsePointer *llm.AssistantResponseMetadata
		if hasResponse {
			responsePointer = &response
		}
		done, err := llm.NewDoneEventWithMetadata(
			terminal.FinishReason(),
			terminal.Usage(),
			terminal.Timestamp(),
			terminal.AssistantProvenance(),
			responsePointer,
			terminal.Diagnostics(),
		)
		if err != nil {
			return nil, err
		}
		events = append(events, done)
	case llm.AssistantFailureMessage:
		response, hasResponse := terminal.ResponseMetadata()
		var responsePointer *llm.AssistantResponseMetadata
		if hasResponse {
			responsePointer = &response
		}
		event, err := llm.NewErrorEventWithMetadata(
			terminal.FinishReason(),
			terminal.Failure(),
			terminal.Usage(),
			terminal.Timestamp(),
			terminal.AssistantProvenance(),
			responsePointer,
			terminal.Diagnostics(),
		)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	default:
		return nil, fmt.Errorf("unsupported assistant terminal %T", terminal)
	}
	return events, nil
}

func splitRunes(value string, maximum int) []string {
	if value == "" {
		return nil
	}
	runes := []rune(value)
	capacity := 1 + (len(runes)-1)/maximum
	chunks := make([]string, 0, capacity)
	for start := 0; start < len(runes); {
		width := min(maximum, len(runes)-start)
		end := start + width
		chunks = append(chunks, string(runes[start:end]))
		start = end
	}
	return chunks
}

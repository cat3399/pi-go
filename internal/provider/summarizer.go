package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
)

// ContextSummarizer is the production request seam used by Agent context
// resilience. It turns Session's already-serialized summary input into one
// ordinary provider request; it never owns a Session lock or durable state.
type ContextSummarizer struct {
	provider Provider
	model    Model
	now      func() time.Time
	retry    RetryController
}

func NewContextSummarizer(implementation Provider, model Model, now func() time.Time) (*ContextSummarizer, error) {
	return NewContextSummarizerWithRetry(implementation, model, now, RetryPolicy{
		MaxAttempts: 3, InitialDelay: 250 * time.Millisecond, MaxDelay: 2 * time.Second,
	})
}

// NewContextSummarizerWithRetry exposes deterministic timing seams for tests
// and product policy. Retry remains entirely inside this one summary request;
// Session sees either one accepted SummaryOutput or one error and therefore
// never appends a checkpoint for a failed attempt.
func NewContextSummarizerWithRetry(implementation Provider, model Model, now func() time.Time, policy RetryPolicy) (*ContextSummarizer, error) {
	if implementation == nil || isTypedNil(implementation) {
		return nil, errors.New("context summarizer requires a provider")
	}
	if _, err := NewRequest(model, "", nil); err != nil {
		return nil, fmt.Errorf("context summarizer model: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	retry, err := NewRetryController(policy)
	if err != nil {
		return nil, fmt.Errorf("context summarizer retry: %w", err)
	}
	return &ContextSummarizer{provider: implementation, model: model, now: now, retry: retry}, nil
}

func (s *ContextSummarizer) Summarize(ctx context.Context, input session.SummaryInput) (session.SummaryOutput, error) {
	return s.SummarizeWithRetryObserver(ctx, input, nil)
}

// SummarizeWithRetryObserver reports only normalized retry metadata. Each
// scheduled retry has exactly one finished event, including cancellation while
// waiting; an attempt event means provider redispatch is beginning.
func (s *ContextSummarizer) SummarizeWithRetryObserver(ctx context.Context, input session.SummaryInput, observer RetryObserver) (session.SummaryOutput, error) {
	if s == nil || s.provider == nil {
		return session.SummaryOutput{}, errors.New("context summarizer is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timestamp := s.now().UTC()
	user, err := llm.NewUserTextMessage(input.Prompt, timestamp)
	if err != nil {
		return session.SummaryOutput{}, fmt.Errorf("build summary prompt: %w", err)
	}
	request, err := NewRequest(s.model, input.SystemPrompt, []llm.ConversationMessage{user})
	if err != nil {
		return session.SummaryOutput{}, fmt.Errorf("build summary request: %w", err)
	}
	retryPending := false
	for attempt := uint32(1); ; attempt++ {
		if retryPending {
			if cause := context.Cause(ctx); cause != nil {
				notifyRetryObserver(ctx, observer, RetryEvent{
					Kind: RetryFinished, Attempt: attempt, MaxAttempts: s.retry.MaxAttempts(), FailureKind: FailureCancelled,
					FinishReason: RetryFinishCancelled, FinalError: "summary retry cancelled",
				})
				return session.SummaryOutput{}, fmt.Errorf("summary retry cancelled: %w", cause)
			}
			notifyRetryObserver(ctx, observer, RetryEvent{Kind: RetryAttempt, Attempt: attempt, MaxAttempts: s.retry.MaxAttempts()})
		}
		finishRetry := func(kind FailureKind, status int, succeeded bool, reason RetryFinishReason, finalError string) {
			if !retryPending {
				return
			}
			notifyRetryObserver(ctx, observer, RetryEvent{
				Kind: RetryFinished, Attempt: attempt, MaxAttempts: s.retry.MaxAttempts(), FailureKind: kind,
				HTTPStatus: status, FinishReason: reason, Succeeded: succeeded, FinalError: finalError,
			})
			retryPending = false
		}
		terminal, streamErr := s.collectAttempt(ctx, request)
		errorMessage := summaryRetryError(terminal, streamErr)
		var failure *ProviderFailure
		if terminalFailure, ok := terminal.(llm.AssistantFailureMessage); ok {
			_ = errors.As(terminalFailure.Failure().Cause(), &failure)
		}
		retryable := IsTransientStreamError(streamErr) || IsTransientFailure(failure)
		kind, status, _ := normalizedRetryOutcome(terminal, streamErr)
		if retryable && attempt < s.retry.MaxAttempts() {
			finishRetry(kind, status, false, RetryFinishFailed, errorMessage)
			delay := s.retry.Delay(attempt+1, failure)
			notifyRetryObserver(ctx, observer, RetryEvent{
				Kind: RetryScheduled, Attempt: attempt + 1, MaxAttempts: s.retry.MaxAttempts(), Delay: delay,
				FailureKind: kind, HTTPStatus: status, ErrorMessage: errorMessage,
			})
			if err := s.retry.Wait(ctx, delay); err != nil {
				notifyRetryObserver(ctx, observer, RetryEvent{
					Kind: RetryFinished, Attempt: attempt + 1, MaxAttempts: s.retry.MaxAttempts(), FailureKind: FailureCancelled,
					FinishReason: RetryFinishCancelled, FinalError: "summary retry cancelled",
				})
				return session.SummaryOutput{}, fmt.Errorf("summary retry cancelled: %w", err)
			}
			retryPending = true
			continue
		}
		failureReason := RetryFinishFailed
		if cause := context.Cause(ctx); cause != nil {
			finishRetry(FailureCancelled, 0, false, RetryFinishCancelled, "summary retry cancelled")
			return session.SummaryOutput{}, fmt.Errorf("summary retry cancelled: %w", cause)
		}
		if retryable && attempt >= s.retry.MaxAttempts() {
			failureReason = RetryFinishExhausted
		}
		if streamErr != nil {
			finishRetry(kind, status, false, failureReason, errorMessage)
			return session.SummaryOutput{}, fmt.Errorf("summary stream: %w", streamErr)
		}
		text, usage, err := summaryTerminal(terminal)
		if err != nil {
			if kind == 0 {
				kind = FailureInvalidResponse
			}
			finishRetry(kind, status, false, failureReason, errorMessage)
			return session.SummaryOutput{}, err
		}
		finishRetry(0, 0, true, RetryFinishSucceeded, "")
		return session.SummaryOutput{Text: text, Usage: &session.CompactionUsage{Usage: usage, Cost: session.ZeroUsageCost()}}, nil
	}
}

func summaryRetryError(terminal llm.AssistantTerminal, streamErr error) string {
	if streamErr != nil {
		return "summary stream failed"
	}
	if failure, ok := terminal.(llm.AssistantFailureMessage); ok {
		return failure.ErrorMessage()
	}
	return ""
}

func notifyRetryObserver(ctx context.Context, observer RetryObserver, event RetryEvent) {
	if observer != nil {
		observer(ctx, event)
	}
}

func normalizedRetryOutcome(terminal llm.AssistantTerminal, streamErr error) (FailureKind, int, bool) {
	if streamErr != nil {
		if IsTransientStreamError(streamErr) {
			return FailureTransport, 0, false
		}
		return FailureInvalidResponse, 0, false
	}
	if failure, ok := terminal.(llm.AssistantFailureMessage); ok {
		var providerFailure *ProviderFailure
		if errors.As(failure.Failure().Cause(), &providerFailure) {
			status, _ := providerFailure.HTTPStatus()
			return providerFailure.Kind(), status, false
		}
		return FailureInvalidResponse, 0, false
	}
	if terminal == nil || terminal.FinishReason() == llm.FinishError || terminal.FinishReason() == llm.FinishAborted {
		return FailureInvalidResponse, 0, false
	}
	return 0, 0, true
}

func (s *ContextSummarizer) collectAttempt(ctx context.Context, request Request) (llm.AssistantTerminal, error) {
	stream := s.provider.Stream(ctx, request)
	if stream == nil || isTypedNil(stream) {
		return nil, errors.New("context summarizer provider returned nil stream")
	}
	closed := false
	defer func() {
		if !closed {
			_ = stream.Close()
		}
	}()
	collector := &llm.StreamCollector{}
	for {
		event, nextErr := stream.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return nil, nextErr
		}
		if err := collector.Accept(event); err != nil {
			return nil, fmt.Errorf("summary stream event: %w", err)
		}
	}
	closed = true
	if err := stream.Close(); err != nil {
		return nil, fmt.Errorf("summary stream transport close: %w", err)
	}
	if err := collector.Close(); err != nil {
		return nil, fmt.Errorf("summary stream close: %w", err)
	}
	terminal, err := collector.Result()
	if err != nil {
		return nil, fmt.Errorf("summary result: %w", err)
	}
	return terminal, nil
}

func summaryTerminal(terminal llm.AssistantTerminal) (string, llm.Usage, error) {
	switch value := terminal.(type) {
	case llm.AssistantTextMessage:
		parts := value.Content()
		var text strings.Builder
		for _, part := range parts {
			text.WriteString(part.Text())
		}
		if strings.TrimSpace(text.String()) == "" {
			return "", llm.Usage{}, errors.New("summary response was empty")
		}
		return text.String(), value.Usage(), nil
	case llm.AssistantFailureMessage:
		return "", llm.Usage{}, fmt.Errorf("summary provider failed: %w", value.Failure().Cause())
	default:
		return "", llm.Usage{}, fmt.Errorf("summary provider returned unsupported terminal %T", terminal)
	}
}

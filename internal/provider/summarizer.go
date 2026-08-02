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
	model    ModelRef
	now      func() time.Time
	retry    RetryController
}

func NewContextSummarizer(implementation Provider, model ModelRef, now func() time.Time) (*ContextSummarizer, error) {
	return NewContextSummarizerWithRetry(implementation, model, now, RetryPolicy{
		MaxAttempts: 3, InitialDelay: 250 * time.Millisecond, MaxDelay: 2 * time.Second,
	})
}

// NewContextSummarizerWithRetry exposes deterministic timing seams for tests
// and product policy. Retry remains entirely inside this one summary request;
// Session sees either one accepted SummaryOutput or one error and therefore
// never appends a checkpoint for a failed attempt.
func NewContextSummarizerWithRetry(implementation Provider, model ModelRef, now func() time.Time, policy RetryPolicy) (*ContextSummarizer, error) {
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
	for attempt := uint32(1); ; attempt++ {
		terminal, streamErr := s.collectAttempt(ctx, request)
		var failure *ProviderFailure
		if terminalFailure, ok := terminal.(llm.AssistantFailureMessage); ok {
			_ = errors.As(terminalFailure.Failure().Cause(), &failure)
		}
		retryable := IsTransientStreamError(streamErr) || IsTransientFailure(failure)
		if !retryable || attempt >= s.retry.MaxAttempts() {
			if streamErr != nil {
				return session.SummaryOutput{}, fmt.Errorf("summary stream: %w", streamErr)
			}
			text, usage, err := summaryTerminal(terminal)
			if err != nil {
				return session.SummaryOutput{}, err
			}
			return session.SummaryOutput{Text: text, Usage: &session.CompactionUsage{Usage: usage, Cost: session.ZeroUsageCost()}}, nil
		}
		delay := s.retry.Delay(attempt+1, failure)
		if err := s.retry.Wait(ctx, delay); err != nil {
			return session.SummaryOutput{}, fmt.Errorf("summary retry cancelled: %w", err)
		}
	}
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

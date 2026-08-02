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
}

func NewContextSummarizer(implementation Provider, model ModelRef, now func() time.Time) (*ContextSummarizer, error) {
	if implementation == nil || isTypedNil(implementation) {
		return nil, errors.New("context summarizer requires a provider")
	}
	if _, err := NewRequest(model, "", nil); err != nil {
		return nil, fmt.Errorf("context summarizer model: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	return &ContextSummarizer{provider: implementation, model: model, now: now}, nil
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
	stream := s.provider.Stream(ctx, request)
	if stream == nil || isTypedNil(stream) {
		return session.SummaryOutput{}, errors.New("context summarizer provider returned nil stream")
	}
	defer stream.Close()
	collector := &llm.StreamCollector{}
	for {
		event, nextErr := stream.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return session.SummaryOutput{}, fmt.Errorf("summary stream: %w", nextErr)
		}
		if err := collector.Accept(event); err != nil {
			return session.SummaryOutput{}, fmt.Errorf("summary stream event: %w", err)
		}
	}
	if err := collector.Close(); err != nil {
		return session.SummaryOutput{}, fmt.Errorf("summary stream close: %w", err)
	}
	terminal, err := collector.Result()
	if err != nil {
		return session.SummaryOutput{}, fmt.Errorf("summary result: %w", err)
	}
	text, usage, err := summaryTerminal(terminal)
	if err != nil {
		return session.SummaryOutput{}, err
	}
	return session.SummaryOutput{Text: text, Usage: &session.CompactionUsage{Usage: usage, Cost: session.ZeroUsageCost()}}, nil
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

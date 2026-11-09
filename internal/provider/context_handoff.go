package provider

import (
	"context"
	"errors"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
)

// ContextHandoffWriter continues the selected model's actual message prefix.
// Only response collection/retry is shared with branch summarization; it never
// serializes a transcript into a summary prompt or changes cache attribution.
type ContextHandoffWriter struct {
	collector *ContextSummarizer
	tools     []ToolDefinition
}

func NewContextHandoffWriter(implementation Provider, model Model, now func() time.Time, options ContextSummarizerOptions, tools []ToolDefinition) (*ContextHandoffWriter, error) {
	collector, err := NewContextSummarizerWithOptions(implementation, model, now, options)
	if err != nil {
		return nil, err
	}
	return &ContextHandoffWriter{collector: collector, tools: append([]ToolDefinition(nil), tools...)}, nil
}

func (w *ContextHandoffWriter) Summarize(ctx context.Context, input session.SummaryInput) (session.SummaryOutput, error) {
	return w.SummarizeWithRetryObserver(ctx, input, nil)
}

func (w *ContextHandoffWriter) SummarizeWithRetryObserver(ctx context.Context, input session.SummaryInput, observer RetryObserver) (session.SummaryOutput, error) {
	if w == nil || w.collector == nil {
		return session.SummaryOutput{}, errors.New("context handoff writer is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	notice, err := llm.NewUserTextMessage(input.Prompt, w.collector.now())
	if err != nil {
		return session.SummaryOutput{}, err
	}
	messages := append(append([]llm.ConversationMessage(nil), input.Messages...), notice)
	options := w.collector.request
	options.Stream = CloneStreamOptions(options.Stream)
	maximum := min(uint64(2048), w.collector.model.MaxTokens())
	if options.Stream.MaxTokens != nil && *options.Stream.MaxTokens > 0 {
		maximum = min(maximum, *options.Stream.MaxTokens)
	}
	if window := w.collector.model.ContextWindow(); window > 0 {
		noticeTokens := uint64((len(input.Prompt) + 3) / 4)
		if input.TokensBefore >= window || noticeTokens >= window-input.TokensBefore {
			failure, _ := NewProviderFailure(ProviderFailureSpec{Kind: FailureContextOverflow, Message: "no room for a handoff", Cause: errors.New("context capacity exhausted")})
			return session.SummaryOutput{}, failure
		}
		maximum = min(maximum, window-input.TokensBefore-noticeTokens)
	}
	options.Stream.MaxTokens = &maximum
	options.Tools = append([]ToolDefinition(nil), w.tools...)
	options.ToolChoice = &ToolChoice{Mode: "none"}
	request, err := NewRequestWithOptions(w.collector.model, input.SystemPrompt, messages, options)
	if err != nil {
		return session.SummaryOutput{}, err
	}
	return w.collector.completeRequest(ctx, request, observer, false)
}

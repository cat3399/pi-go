package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
)

// ContextSummarizer is the production request seam used by Agent context
// resilience. It turns Session's already-serialized summary input into one
// ordinary provider request, or the original's ordered history/prefix pair when
// a cut splits a turn. It never owns a Session lock or durable state.
type ContextSummarizer struct {
	provider Provider
	model    Model
	now      func() time.Time
	retry    RetryController
	request  RequestOptions
}

// ContextSummarizerOptions are snapshotted for one compaction operation. Tools
// are intentionally not exposed here: a summary is an isolated provider call,
// never an Agent continuation.
type ContextSummarizerOptions struct {
	ThinkingLevel ThinkingLevel
	Stream        StreamOptions
	Retry         RetryPolicy
}

func NewContextSummarizer(implementation Provider, model Model, now func() time.Time) (*ContextSummarizer, error) {
	return NewContextSummarizerWithOptions(implementation, model, now, ContextSummarizerOptions{Retry: RetryPolicy{
		MaxAttempts: 3, InitialDelay: 250 * time.Millisecond, MaxDelay: 2 * time.Second,
	}})
}

// NewContextSummarizerWithRetry exposes deterministic timing seams for tests
// and product policy. Retry remains entirely inside this one summary request;
// Session sees either one accepted SummaryOutput or one error and therefore
// never appends a checkpoint for a failed attempt.
func NewContextSummarizerWithRetry(implementation Provider, model Model, now func() time.Time, policy RetryPolicy) (*ContextSummarizer, error) {
	return NewContextSummarizerWithOptions(implementation, model, now, ContextSummarizerOptions{Retry: policy})
}

// NewContextSummarizerWithOptions constructs the concrete request used by a
// single compaction operation. Callers should create a new value for each
// operation so model selection, thinking, credentials, headers, and session
// attribution cannot become stale.
func NewContextSummarizerWithOptions(implementation Provider, model Model, now func() time.Time, options ContextSummarizerOptions) (*ContextSummarizer, error) {
	if implementation == nil || isTypedNil(implementation) {
		return nil, errors.New("context summarizer requires a provider")
	}
	requestOptions := RequestOptions{
		ThinkingLevel: options.ThinkingLevel,
		Stream:        CloneStreamOptions(options.Stream),
	}
	if _, err := NewRequestWithOptions(model, "", nil, requestOptions); err != nil {
		return nil, fmt.Errorf("context summarizer model: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	retry, err := NewRetryController(options.Retry)
	if err != nil {
		return nil, fmt.Errorf("context summarizer retry: %w", err)
	}
	return &ContextSummarizer{
		provider: implementation, model: model, now: now, retry: retry, request: requestOptions,
	}, nil
}

func (s *ContextSummarizer) Summarize(ctx context.Context, input session.SummaryInput) (session.SummaryOutput, error) {
	return s.SummarizeWithRetryObserver(ctx, input, nil)
}

// SummarizeBranch uses the same isolated request, retry, auth/header and
// collection path as compaction, with pi's fixed 2048-token branch budget.
func (s *ContextSummarizer) SummarizeBranch(ctx context.Context, input session.BranchSummaryInput) (session.BranchSummaryOutput, error) {
	return s.SummarizeBranchWithRetryObserver(ctx, input, nil)
}

func (s *ContextSummarizer) SummarizeBranchWithRetryObserver(ctx context.Context, input session.BranchSummaryInput, observer RetryObserver) (session.BranchSummaryOutput, error) {
	if s == nil || s.provider == nil {
		return session.BranchSummaryOutput{}, errors.New("context summarizer is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	maxTokens := input.MaxTokens
	if maxTokens == 0 {
		maxTokens = session.BranchSummaryMaxOutputTokens
	}
	output, err := s.summarizeCall(ctx, input.SystemPrompt, input.Prompt, &maxTokens, observer, true)
	if err != nil {
		if context.Cause(ctx) != nil {
			return session.BranchSummaryOutput{Aborted: true}, nil
		}
		return session.BranchSummaryOutput{Error: err.Error()}, nil
	}
	return session.BranchSummaryOutput{Text: output.Text, Usage: output.Usage}, nil
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
	// Direct low-level callers predating the complete preparation model still
	// provide one already-built prompt. Production inputs always carry Settings.
	if !input.Settings.Enabled && len(input.MessagesToSummarize) == 0 && len(input.TurnPrefixMessages) == 0 {
		return s.summarizeCall(ctx, input.SystemPrompt, input.Prompt, s.request.Stream.MaxTokens, observer, false)
	}
	historyBudget := fractionBudget(input.Settings.ReserveTokens, 4, 5)
	prefixBudget := fractionBudget(input.Settings.ReserveTokens, 1, 2)
	if input.IsSplitTurn && len(input.TurnPrefixMessages) > 0 {
		historyText := "No prior history."
		var historyUsage *session.CompactionUsage
		if len(input.MessagesToSummarize) > 0 {
			history, err := s.summarizeCall(ctx, input.SystemPrompt, input.Prompt, &historyBudget, observer, false)
			if err != nil {
				return session.SummaryOutput{}, err
			}
			historyText, historyUsage = history.Text, history.Usage
		}
		prefix, err := s.summarizeCall(ctx, input.SystemPrompt, input.TurnPrefixPrompt, &prefixBudget, observer, false)
		if err != nil {
			return session.SummaryOutput{}, fmt.Errorf("turn prefix summarization: %w", err)
		}
		usage := prefix.Usage
		if historyUsage != nil {
			usage, err = combineCompactionUsage(historyUsage, prefix.Usage)
			if err != nil {
				return session.SummaryOutput{}, fmt.Errorf("combine summary usage: %w", err)
			}
		}
		return session.SummaryOutput{
			Text:  historyText + "\n\n---\n\n**Turn Context (split turn):**\n\n" + prefix.Text,
			Usage: usage,
		}, nil
	}
	return s.summarizeCall(ctx, input.SystemPrompt, input.Prompt, &historyBudget, observer, false)
}

func fractionBudget(reserve, numerator, denominator uint64) uint64 {
	budget := reserve/denominator*numerator + reserve%denominator*numerator/denominator
	return budget
}

func (s *ContextSummarizer) summarizeCall(ctx context.Context, systemPrompt, prompt string, maxTokens *uint64, observer RetryObserver, allowEmpty bool) (session.SummaryOutput, error) {
	timestamp := s.now().UTC()
	summarySessionID, generationErr := session.NewSessionID(timestamp)
	if generationErr != nil {
		return session.SummaryOutput{}, fmt.Errorf("build summary session id: %w", generationErr)
	}
	user, err := llm.NewUserTextMessage(prompt, timestamp)
	if err != nil {
		return session.SummaryOutput{}, fmt.Errorf("build summary prompt: %w", err)
	}
	requestOptions := s.request
	requestOptions.Stream = CloneStreamOptions(requestOptions.Stream)
	requestOptions.Stream.CacheRetention = CacheRetentionNone
	requestOptions.Stream.SessionID = summarySessionID
	if maxTokens != nil {
		value := *maxTokens
		if modelMaximum := s.model.MaxTokens(); modelMaximum > 0 && value > modelMaximum {
			value = modelMaximum
		}
		requestOptions.Stream.MaxTokens = &value
	}
	request, err := NewRequestWithOptions(s.model, systemPrompt, []llm.ConversationMessage{user}, requestOptions)
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
		text, usage, err := summaryTerminal(terminal, allowEmpty)
		if err != nil {
			if kind == 0 {
				kind = FailureInvalidResponse
			}
			finishRetry(kind, status, false, failureReason, errorMessage)
			return session.SummaryOutput{}, err
		}
		finishRetry(0, 0, true, RetryFinishSucceeded, "")
		return session.SummaryOutput{Text: text, Usage: &session.CompactionUsage{Usage: usage, Cost: session.UsageCostFromLLM(usage.Cost())}}, nil
	}
}

func combineCompactionUsage(first, second *session.CompactionUsage) (*session.CompactionUsage, error) {
	if first == nil {
		return second, nil
	}
	if second == nil {
		return first, nil
	}
	add := func(left, right uint64) (uint64, error) {
		value, carry := bits.Add64(left, right, 0)
		if carry != 0 {
			return 0, llm.ErrUsageOverflow
		}
		return value, nil
	}
	input, err := add(first.Usage.Input(), second.Usage.Input())
	if err != nil {
		return nil, err
	}
	output, err := add(first.Usage.Output(), second.Usage.Output())
	if err != nil {
		return nil, err
	}
	cacheRead, err := add(first.Usage.CacheRead(), second.Usage.CacheRead())
	if err != nil {
		return nil, err
	}
	cacheWrite, err := add(first.Usage.CacheWrite(), second.Usage.CacheWrite())
	if err != nil {
		return nil, err
	}
	spec := llm.UsageSpec{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite}
	firstReasoning, firstHasReasoning := first.Usage.Reasoning()
	secondReasoning, secondHasReasoning := second.Usage.Reasoning()
	if firstHasReasoning || secondHasReasoning {
		value, addErr := add(firstReasoning, secondReasoning)
		if addErr != nil {
			return nil, addErr
		}
		spec.Reasoning = &value
	}
	firstCache1h, firstHasCache1h := first.Usage.CacheWrite1h()
	secondCache1h, secondHasCache1h := second.Usage.CacheWrite1h()
	if firstHasCache1h || secondHasCache1h {
		value, addErr := add(firstCache1h, secondCache1h)
		if addErr != nil {
			return nil, addErr
		}
		spec.CacheWrite1h = &value
	}
	firstCost, secondCost := first.Usage.Cost(), second.Usage.Cost()
	cost := llm.Cost{
		Input: firstCost.Input + secondCost.Input, Output: firstCost.Output + secondCost.Output,
		CacheRead: firstCost.CacheRead + secondCost.CacheRead, CacheWrite: firstCost.CacheWrite + secondCost.CacheWrite,
		Total: firstCost.Total + secondCost.Total,
	}
	spec.Cost = &cost
	usage, err := llm.NewUsage(spec)
	if err != nil {
		return nil, err
	}
	return &session.CompactionUsage{Usage: usage, Cost: session.UsageCostFromLLM(cost)}, nil
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

func summaryTerminal(terminal llm.AssistantTerminal, allowEmpty bool) (string, llm.Usage, error) {
	textFromBlocks := func(blocks []llm.AssistantBlock) string {
		text := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if value, ok := block.(llm.TextBlock); ok {
				text = append(text, value.Text())
			}
		}
		return strings.Join(text, "\n")
	}
	switch value := terminal.(type) {
	case llm.AssistantTextMessage:
		text := textFromBlocks(value.Blocks())
		if !allowEmpty && strings.TrimSpace(text) == "" {
			return "", llm.Usage{}, errors.New("summary response was empty")
		}
		return text, value.Usage(), nil
	case llm.AssistantRichMessage:
		text := textFromBlocks(value.Blocks())
		if !allowEmpty && strings.TrimSpace(text) == "" {
			return "", llm.Usage{}, errors.New("summary response was empty")
		}
		return text, value.Usage(), nil
	case llm.AssistantToolUseMessage:
		text := textFromBlocks(value.Blocks())
		if !allowEmpty && strings.TrimSpace(text) == "" {
			return "", llm.Usage{}, errors.New("summary response was empty")
		}
		return text, value.Usage(), nil
	case llm.AssistantFailureMessage:
		return "", llm.Usage{}, fmt.Errorf("summary provider failed: %w", value.Failure().Cause())
	default:
		return "", llm.Usage{}, fmt.Errorf("summary provider returned unsupported terminal %T", terminal)
	}
}

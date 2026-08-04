package provider_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestScriptedProviderConsumesFIFOAndCapturesRequests(t *testing.T) {
	t.Parallel()

	p := mustProvider(t, provider.ScriptedConfig{})
	mustSetResponses(t, p,
		mustFixedStep(t, mustTextTerminal(t, "first")),
		mustFixedStep(t, mustTextTerminal(t, "second")),
	)
	request := mustRequest(t, "hello")

	_, first := collectStream(t, p.Stream(context.Background(), request))
	_, second := collectStream(t, p.Stream(context.Background(), request))
	exhaustedEvents, exhausted := collectStream(t, p.Stream(context.Background(), request))

	if got := terminalText(t, first); got != "first" {
		t.Fatalf("first response = %q, want first", got)
	}
	if got := terminalText(t, second); got != "second" {
		t.Fatalf("second response = %q, want second", got)
	}
	failure := terminalFailure(t, exhausted)
	if failure.FinishReason() != llm.FinishError || failure.ErrorMessage() != provider.ErrQueueExhausted.Error() {
		t.Fatalf("exhausted failure = (%v, %q)", failure.FinishReason(), failure.ErrorMessage())
	}
	if len(exhaustedEvents) != 1 {
		t.Fatalf("exhausted event count = %d, want 1", len(exhaustedEvents))
	}
	assertProviderFailure(t, failure, provider.FailureQueueExhausted, provider.ErrQueueExhausted)
	if p.PendingResponses() != 0 || p.CallCount() != 3 || len(p.Requests()) != 3 {
		t.Fatalf("state = pending %d, calls %d, requests %d", p.PendingResponses(), p.CallCount(), len(p.Requests()))
	}

	captured := p.Requests()
	captured[0] = provider.Request{}
	if len(p.Requests()[0].Messages()) != 1 {
		t.Fatal("captured request changed through returned slice")
	}
}

func TestScriptedProviderSetAndAppendAreAtomic(t *testing.T) {
	t.Parallel()

	p := mustProvider(t, provider.ScriptedConfig{})
	valid := mustFixedStep(t, mustTextTerminal(t, "kept"))
	mustSetResponses(t, p, valid)

	if err := p.AppendResponses([]provider.ScriptStep{valid, {}}); !errors.Is(err, provider.ErrInvalidScriptStep) {
		t.Fatalf("AppendResponses(invalid) error = %v, want ErrInvalidScriptStep", err)
	}
	if p.PendingResponses() != 1 {
		t.Fatalf("pending after failed append = %d, want 1", p.PendingResponses())
	}
	if err := p.SetResponses([]provider.ScriptStep{{}}); !errors.Is(err, provider.ErrInvalidScriptStep) {
		t.Fatalf("SetResponses(invalid) error = %v, want ErrInvalidScriptStep", err)
	}
	if p.PendingResponses() != 1 {
		t.Fatalf("pending after failed replace = %d, want 1", p.PendingResponses())
	}

	mustAppendResponses(t, p,
		mustFixedStep(t, mustTextTerminal(t, "second")),
		mustFixedStep(t, mustTextTerminal(t, "third")),
	)
	if p.PendingResponses() != 3 {
		t.Fatalf("pending after append = %d, want 3", p.PendingResponses())
	}
	mustSetResponses(t, p)
	if p.PendingResponses() != 0 {
		t.Fatalf("pending after clear = %d, want 0", p.PendingResponses())
	}
}

func TestScriptedProviderFactoryIsLazyAndErrorsBecomeTerminal(t *testing.T) {
	t.Parallel()

	p := mustProvider(t, provider.ScriptedConfig{})
	called := false
	factory, err := provider.FactoryResponseStep(func(
		_ context.Context,
		request provider.Request,
		callIndex uint64,
	) (llm.AssistantTerminal, error) {
		called = true
		if callIndex != 1 || request.Model().ID() != "scripted-1" || len(request.Messages()) != 1 {
			return nil, fmt.Errorf("unexpected factory input")
		}
		return mustTextTerminal(t, "factory"), nil
	})
	if err != nil {
		t.Fatalf("FactoryResponseStep() error = %v", err)
	}
	factoryCause := errors.New("boom")
	failing, err := provider.FactoryResponseStep(func(
		context.Context,
		provider.Request,
		uint64,
	) (llm.AssistantTerminal, error) {
		return nil, factoryCause
	})
	if err != nil {
		t.Fatalf("FactoryResponseStep() error = %v", err)
	}
	mustSetResponses(t, p, factory, failing)

	stream := p.Stream(context.Background(), mustRequest(t, "hello"))
	if called {
		t.Fatal("factory ran before stream consumption")
	}
	_, terminal := collectStream(t, stream)
	if !called || terminalText(t, terminal) != "factory" {
		t.Fatalf("factory response = (%t, %q)", called, terminalText(t, terminal))
	}

	events, terminal := collectStream(t, p.Stream(context.Background(), mustRequest(t, "again")))
	if len(events) != 1 || terminalFailure(t, terminal).ErrorMessage() != "boom" {
		t.Fatalf("factory error events/result = (%d, %q)", len(events), terminalFailure(t, terminal).ErrorMessage())
	}
	assertProviderFailure(t, terminalFailure(t, terminal), provider.FailureFactory, factoryCause)
	if _, err := provider.FactoryResponseStep(nil); !errors.Is(err, provider.ErrInvalidScriptStep) {
		t.Fatalf("FactoryResponseStep(nil) error = %v, want ErrInvalidScriptStep", err)
	}
	if _, err := provider.FixedResponseStep(llm.AssistantTextMessage{}); !errors.Is(err, provider.ErrInvalidScriptStep) {
		t.Fatalf("FixedResponseStep(zero) error = %v, want ErrInvalidScriptStep", err)
	}
}

func TestScriptedProviderRejectsInvalidInputsWithoutConsumingScript(t *testing.T) {
	t.Parallel()

	fixedTime := time.Date(2026, time.August, 1, 8, 9, 10, 0, time.UTC)
	p := mustProvider(t, provider.ScriptedConfig{Clock: func() time.Time { return fixedTime }})
	mustSetResponses(t, p, mustFixedStep(t, mustTextTerminal(t, "kept")))

	for _, tt := range []struct {
		name    string
		ctx     context.Context
		request provider.Request
	}{
		{name: "invalid request", ctx: context.Background()},
		{name: "nil context", request: mustRequest(t, "hello")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			events, terminal := collectStream(t, p.Stream(tt.ctx, tt.request))
			failure := terminalFailure(t, terminal)
			if len(events) != 1 || failure.FinishReason() != llm.FinishError || !failure.Timestamp().Equal(fixedTime) {
				t.Fatalf("invalid input terminal = events %d, finish %v, timestamp %v", len(events), failure.FinishReason(), failure.Timestamp())
			}
			assertProviderFailure(t, failure, provider.FailureInvalidRequest, provider.ErrInvalidRequest)
		})
	}
	if p.PendingResponses() != 1 || p.CallCount() != 0 || len(p.Requests()) != 0 {
		t.Fatalf("invalid inputs mutated state: pending %d, calls %d, requests %d", p.PendingResponses(), p.CallCount(), len(p.Requests()))
	}
	_, terminal := collectStream(t, p.Stream(context.Background(), mustRequest(t, "valid")))
	if terminalText(t, terminal) != "kept" {
		t.Fatalf("valid response after invalid inputs = %q", terminalText(t, terminal))
	}

	if _, err := provider.NewScriptedProvider(provider.ScriptedConfig{ChunkRunes: -1}); !errors.Is(err, provider.ErrInvalidScriptConfig) {
		t.Fatalf("NewScriptedProvider(negative chunk) error = %v, want ErrInvalidScriptConfig", err)
	}
}

func TestScriptedProviderFactoryInvalidTerminalBecomesErrorEvent(t *testing.T) {
	t.Parallel()

	p := mustProvider(t, provider.ScriptedConfig{})
	step, err := provider.FactoryResponseStep(func(
		context.Context,
		provider.Request,
		uint64,
	) (llm.AssistantTerminal, error) {
		return llm.AssistantTextMessage{}, nil
	})
	if err != nil {
		t.Fatalf("FactoryResponseStep() error = %v", err)
	}
	mustSetResponses(t, p, step)
	events, terminal := collectStream(t, p.Stream(context.Background(), mustRequest(t, "hello")))
	if len(events) != 1 || terminalFailure(t, terminal).FinishReason() != llm.FinishError {
		t.Fatalf("invalid factory terminal = events %d, result %T/%v", len(events), terminal, terminal.FinishReason())
	}
	assertProviderFailure(t, terminalFailure(t, terminal), provider.FailureInvalidResponse, nil)
}

func TestScriptedProviderFactoryPanicBecomesSingleTypedTerminal(t *testing.T) {
	t.Parallel()

	panicCause := errors.New("factory panic sentinel")
	p := mustProvider(t, provider.ScriptedConfig{})
	step, err := provider.FactoryResponseStep(func(
		context.Context,
		provider.Request,
		uint64,
	) (llm.AssistantTerminal, error) {
		panic(panicCause)
	})
	if err != nil {
		t.Fatalf("FactoryResponseStep() error = %v", err)
	}
	mustSetResponses(t, p, step)
	events, terminal := collectStream(t, p.Stream(context.Background(), mustRequest(t, "hello")))
	failure := terminalFailure(t, terminal)
	if got := eventKinds(events); !equalStrings(got, []string{"error"}) {
		t.Fatalf("factory panic event kinds = %v, want [error]", got)
	}
	assertProviderFailure(t, failure, provider.FailureFactory, panicCause)
	var panicError *provider.FactoryPanicError
	if !errors.As(failure.Failure(), &panicError) || panicError.PanicType() == "" || len(panicError.Stack()) == 0 {
		t.Fatalf("panic cause = %#v, want retained type and stack", panicError)
	}
	stack := panicError.Stack()
	stack[0] ^= 0xff
	if bytes.Equal(stack, panicError.Stack()) {
		t.Fatal("FactoryPanicError.Stack() aliases retained stack")
	}
}

type panickingError struct{}

func (*panickingError) Error() string { panic("error formatter panicked") }

func TestScriptedProviderFactoryErrorFormatterCannotEscapeTerminal(t *testing.T) {
	t.Parallel()

	cause := &panickingError{}
	p := mustProvider(t, provider.ScriptedConfig{})
	step, err := provider.FactoryResponseStep(func(
		context.Context,
		provider.Request,
		uint64,
	) (llm.AssistantTerminal, error) {
		return nil, cause
	})
	if err != nil {
		t.Fatalf("FactoryResponseStep() error = %v", err)
	}
	mustSetResponses(t, p, step)
	events, terminal := collectStream(t, p.Stream(context.Background(), mustRequest(t, "hello")))
	failure := terminalFailure(t, terminal)
	if got := eventKinds(events); !equalStrings(got, []string{"error"}) {
		t.Fatalf("factory error event kinds = %v, want [error]", got)
	}
	if failure.ErrorMessage() != "scripted provider failed" {
		t.Fatalf("failure message = %q, want controlled fallback", failure.ErrorMessage())
	}
	assertProviderFailure(t, failure, provider.FailureFactory, cause)
}

func TestScriptedProviderStreamsDeterministicTextChunks(t *testing.T) {
	t.Parallel()

	p := mustProvider(t, provider.ScriptedConfig{ChunkRunes: 2})
	mustSetResponses(t, p, mustFixedStep(t, mustTextTerminal(t, "你好abc")))
	events, terminal := collectStream(t, p.Stream(context.Background(), mustRequest(t, "hello")))

	if got, want := eventKinds(events), []string{"start", "text_start", "text_delta", "text_delta", "text_delta", "text_end", "done"}; !equalStrings(got, want) {
		t.Fatalf("event kinds = %v, want %v", got, want)
	}
	var deltas []string
	for _, event := range events {
		if delta, ok := event.(llm.TextDeltaEvent); ok {
			if delta.ContentIndex() != 0 {
				t.Fatalf("delta index = %d, want 0", delta.ContentIndex())
			}
			deltas = append(deltas, delta.Delta())
		}
	}
	if !equalStrings(deltas, []string{"你好", "ab", "c"}) {
		t.Fatalf("text deltas = %q", deltas)
	}
	if terminalText(t, terminal) != "你好abc" {
		t.Fatalf("terminal text = %q", terminalText(t, terminal))
	}
}

func TestScriptedProviderAcceptsMaximumChunkSizeWithoutOverflow(t *testing.T) {
	t.Parallel()

	p := mustProvider(t, provider.ScriptedConfig{ChunkRunes: math.MaxInt})
	mustSetResponses(t, p, mustFixedStep(t, mustTextTerminal(t, "ab")))
	events, terminal := collectStream(t, p.Stream(context.Background(), mustRequest(t, "hello")))
	if terminalText(t, terminal) != "ab" {
		t.Fatalf("terminal text = %q, want ab", terminalText(t, terminal))
	}
	var deltas int
	for _, event := range events {
		if _, ok := event.(llm.TextDeltaEvent); ok {
			deltas++
		}
	}
	if deltas != 1 {
		t.Fatalf("text delta count = %d, want 1", deltas)
	}
}

func TestScriptedProviderStreamsToolArgumentsExactly(t *testing.T) {
	t.Parallel()

	p := mustProvider(t, provider.ScriptedConfig{ChunkRunes: 3})
	raw := []byte(`{"text":"你好","count":12}`)
	terminal := mustToolTerminal(t, "call-1", "echo", raw)
	mustSetResponses(t, p, mustFixedStep(t, terminal))
	events, result := collectStream(t, p.Stream(context.Background(), mustRequest(t, "hello")))

	var joined []byte
	for _, event := range events {
		delta, ok := event.(llm.ToolCallDeltaEvent)
		if !ok {
			continue
		}
		piece := delta.Delta()
		joined = append(joined, piece...)
		if len(piece) != 0 {
			piece[0] ^= 0xff
			if bytes.Equal(piece, delta.Delta()) {
				t.Fatal("tool delta aliases caller-owned bytes")
			}
		}
	}
	if !bytes.Equal(joined, raw) {
		t.Fatalf("joined arguments = %q, want %q", joined, raw)
	}
	toolUse, ok := result.(llm.AssistantToolUseMessage)
	if !ok || toolUse.FinishReason() != llm.FinishToolUse {
		t.Fatalf("result = %T/%v, want tool use", result, result.FinishReason())
	}
	call := toolUse.Blocks()[0].(llm.ToolCallBlock)
	if !bytes.Equal(call.ArgumentsJSON(), raw) {
		t.Fatalf("result arguments = %q, want %q", call.ArgumentsJSON(), raw)
	}
}

func TestScriptedProviderStreamsExplicitFailureContent(t *testing.T) {
	t.Parallel()

	p := mustProvider(t, provider.ScriptedConfig{ChunkRunes: 4})
	explicitCause := errors.New("upstream cause")
	failureDescriptor, err := llm.NewFailure("upstream failed", explicitCause)
	if err != nil {
		t.Fatalf("NewFailure() error = %v", err)
	}
	failure, err := newAssistantFailureMessageWithFailure(
		[]llm.TextBlock{mustTextBlock(t, "partial")},
		llm.FinishError,
		failureDescriptor,
		llm.Usage{},
		time.Time{},
	)
	if err != nil {
		t.Fatalf("NewAssistantFailureMessage() error = %v", err)
	}
	mustSetResponses(t, p, mustFixedStep(t, failure))
	events, result := collectStream(t, p.Stream(context.Background(), mustRequest(t, "hello")))

	want := []string{"start", "text_start", "text_delta", "text_delta", "text_end", "error"}
	if got := eventKinds(events); !equalStrings(got, want) {
		t.Fatalf("event kinds = %v, want %v", got, want)
	}
	gotFailure := terminalFailure(t, result)
	if gotFailure.ErrorMessage() != "upstream failed" || gotFailure.Content()[0].Text() != "partial" {
		t.Fatalf("failure = (%q, %q)", gotFailure.ErrorMessage(), gotFailure.Content()[0].Text())
	}
	if !errors.Is(gotFailure.Failure(), explicitCause) {
		t.Fatalf("explicit failure cause = %v, want retained sentinel", gotFailure.Failure().Cause())
	}
}

func TestScriptedProviderCancellationBeforeStartAndMidText(t *testing.T) {
	t.Parallel()

	t.Run("before start", func(t *testing.T) {
		p := mustProvider(t, provider.ScriptedConfig{})
		called := false
		step, err := provider.FactoryResponseStep(func(
			context.Context,
			provider.Request,
			uint64,
		) (llm.AssistantTerminal, error) {
			called = true
			return mustTextTerminal(t, "unused"), nil
		})
		if err != nil {
			t.Fatalf("FactoryResponseStep() error = %v", err)
		}
		mustSetResponses(t, p, step)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		events, result := collectStream(t, p.Stream(ctx, mustRequest(t, "hello")))
		if called {
			t.Fatal("pre-cancelled request invoked response factory")
		}
		if len(events) != 1 || terminalFailure(t, result).FinishReason() != llm.FinishAborted {
			t.Fatalf("pre-cancel result = events %d, finish %v", len(events), result.FinishReason())
		}
		assertProviderFailure(t, terminalFailure(t, result), provider.FailureCancelled, context.Canceled)
		if p.PendingResponses() != 0 || p.CallCount() != 1 {
			t.Fatalf("pre-cancel state = pending %d, calls %d", p.PendingResponses(), p.CallCount())
		}
	})

	t.Run("mid text", func(t *testing.T) {
		p := mustProvider(t, provider.ScriptedConfig{ChunkRunes: 3})
		mustSetResponses(t, p, mustFixedStep(t, mustTextTerminal(t, "abcdefghijklmnopqrstuvwxyz")))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stream := p.Stream(ctx, mustRequest(t, "hello"))
		collector := &llm.StreamCollector{}
		var kinds []string
		for {
			event, err := stream.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("Next() error = %v", err)
			}
			kinds = append(kinds, eventKind(event))
			if err := collector.Accept(event); err != nil {
				t.Fatalf("collector.Accept(%T) error = %v", event, err)
			}
			if _, ok := event.(llm.TextDeltaEvent); ok {
				cancel()
			}
		}
		if err := collector.Close(); err != nil {
			t.Fatalf("collector.Close() error = %v", err)
		}
		result, err := collector.Result()
		if err != nil {
			t.Fatalf("collector.Result() error = %v", err)
		}
		want := []string{"start", "text_start", "text_delta", "error"}
		if !equalStrings(kinds, want) {
			t.Fatalf("event kinds = %v, want %v", kinds, want)
		}
		failure := terminalFailure(t, result)
		if failure.Content()[0].Text() != "abc" || failure.FinishReason() != llm.FinishAborted {
			t.Fatalf("mid-cancel failure = (%q, %v)", failure.Content()[0].Text(), failure.FinishReason())
		}
		assertProviderFailure(t, failure, provider.FailureCancelled, context.Canceled)
	})
}

func TestScriptedProviderCancellationDuringFactoryIsAborted(t *testing.T) {
	t.Parallel()

	p := mustProvider(t, provider.ScriptedConfig{})
	entered := make(chan struct{})
	step, err := provider.FactoryResponseStep(func(
		ctx context.Context,
		_ provider.Request,
		_ uint64,
	) (llm.AssistantTerminal, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatalf("FactoryResponseStep() error = %v", err)
	}
	mustSetResponses(t, p, step)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := p.Stream(ctx, mustRequest(t, "hello"))

	type nextResult struct {
		event llm.StreamEvent
		err   error
	}
	resultCh := make(chan nextResult, 1)
	go func() {
		event, nextErr := stream.Next()
		resultCh <- nextResult{event: event, err: nextErr}
	}()
	<-entered
	cancel()
	result := <-resultCh
	if result.err != nil {
		t.Fatalf("Next() error = %v", result.err)
	}
	event, ok := result.event.(llm.ErrorEvent)
	if !ok || event.Reason() != llm.FinishAborted {
		t.Fatalf("event = %T/%v, want aborted ErrorEvent", result.event, event.Reason())
	}
	if !errors.Is(event.Failure(), context.Canceled) || !errors.Is(event.Failure(), provider.ErrRequestAborted) {
		t.Fatalf("factory cancellation cause = %v, want context.Canceled and ErrRequestAborted", event.Failure().Cause())
	}
	var providerFailure *provider.ProviderFailure
	if !errors.As(event.Failure(), &providerFailure) || providerFailure.Kind() != provider.FailureCancelled {
		t.Fatalf("factory cancellation category = %#v, want cancelled", providerFailure)
	}
	if _, err := stream.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next() after aborted terminal error = %v, want EOF", err)
	}
}

func TestScriptedProviderSupportsConcurrentStreamAllocation(t *testing.T) {
	t.Parallel()

	const calls = 24
	p := mustProvider(t, provider.ScriptedConfig{})
	steps := make([]provider.ScriptStep, calls)
	for index := range steps {
		step, err := provider.FactoryResponseStep(func(
			_ context.Context,
			_ provider.Request,
			callIndex uint64,
		) (llm.AssistantTerminal, error) {
			block, blockErr := llm.NewTextBlock(strconv.FormatUint(callIndex, 10))
			if blockErr != nil {
				return nil, blockErr
			}
			return newAssistantTextMessage([]llm.TextBlock{block}, llm.FinishStop, llm.Usage{}, time.Time{})
		})
		if err != nil {
			t.Fatalf("FactoryResponseStep() error = %v", err)
		}
		steps[index] = step
	}
	mustSetResponses(t, p, steps...)

	request := mustRequest(t, "hello")
	results := make(chan int, calls)
	errorsCh := make(chan error, calls)
	var wait sync.WaitGroup
	for range calls {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, terminal, err := collectStreamResult(p.Stream(context.Background(), request))
			if err != nil {
				errorsCh <- err
				return
			}
			message, ok := terminal.(llm.AssistantTextMessage)
			if !ok || len(message.Content()) != 1 {
				errorsCh <- fmt.Errorf("unexpected terminal %T", terminal)
				return
			}
			value, err := strconv.Atoi(message.Content()[0].Text())
			if err != nil {
				errorsCh <- err
				return
			}
			results <- value
		}()
	}
	wait.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("concurrent stream error: %v", err)
	}
	got := make([]int, 0, calls)
	for result := range results {
		got = append(got, result)
	}
	sort.Ints(got)
	if len(got) != calls {
		t.Fatalf("result count = %d, want %d", len(got), calls)
	}
	for index, value := range got {
		if value != index+1 {
			t.Fatalf("sorted result[%d] = %d, want %d", index, value, index+1)
		}
	}
	if p.CallCount() != calls || p.PendingResponses() != 0 || len(p.Requests()) != calls {
		t.Fatalf("state = calls %d, pending %d, requests %d", p.CallCount(), p.PendingResponses(), len(p.Requests()))
	}
}

func TestScriptedProviderSerializesInjectedClockAcrossStreams(t *testing.T) {
	t.Parallel()

	const calls = 20
	clockCalls := 0
	p := mustProvider(t, provider.ScriptedConfig{Clock: func() time.Time {
		clockCalls++
		return time.Unix(int64(clockCalls), 0)
	}})
	request := mustRequest(t, "hello")
	results := make(chan time.Time, calls)
	errorsCh := make(chan error, calls)
	var wait sync.WaitGroup
	for range calls {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, terminal, err := collectStreamResult(p.Stream(context.Background(), request))
			if err != nil {
				errorsCh <- err
				return
			}
			failure, ok := terminal.(llm.AssistantFailureMessage)
			if !ok {
				errorsCh <- fmt.Errorf("unexpected terminal %T", terminal)
				return
			}
			results <- failure.Timestamp()
		}()
	}
	wait.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("concurrent clock stream error: %v", err)
	}
	seen := make(map[time.Time]struct{}, calls)
	for timestamp := range results {
		seen[timestamp] = struct{}{}
	}
	if clockCalls != calls || len(seen) != calls {
		t.Fatalf("clock calls/unique timestamps = %d/%d, want %d/%d", clockCalls, len(seen), calls, calls)
	}
}

func TestScriptedStreamCloseStopsWithoutBackgroundProduction(t *testing.T) {
	t.Parallel()

	p := mustProvider(t, provider.ScriptedConfig{})
	mustSetResponses(t, p, mustFixedStep(t, mustTextTerminal(t, "hello")))
	stream := p.Stream(context.Background(), mustRequest(t, "hello"))
	if _, err := stream.Next(); err != nil {
		t.Fatalf("first Next() error = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := stream.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next() after Close error = %v, want EOF", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func FuzzScriptedTextRoundTrip(f *testing.F) {
	f.Add("hello", uint64(1))
	f.Add("你好🙂abc", uint64(3))
	f.Add("", uint64(0))
	f.Add("max", uint64(0))
	f.Add("max", uint64(math.MaxUint64))

	f.Fuzz(func(t *testing.T, text string, rawChunk uint64) {
		if len(text) > 4096 {
			t.Skip()
		}
		block, err := llm.NewTextBlock(text)
		if err != nil {
			t.Skip()
		}
		terminal, err := newAssistantTextMessage(
			[]llm.TextBlock{block},
			llm.FinishStop,
			llm.Usage{},
			time.Time{},
		)
		if err != nil {
			t.Fatalf("NewAssistantTextMessage() error = %v", err)
		}
		chunkRunes := int(rawChunk%32) + 1
		if rawChunk%4 == 0 {
			chunkRunes = math.MaxInt - int(rawChunk%1024)
		}
		p, err := provider.NewScriptedProvider(provider.ScriptedConfig{ChunkRunes: chunkRunes})
		if err != nil {
			t.Fatalf("NewScriptedProvider() error = %v", err)
		}
		step, err := provider.FixedResponseStep(terminal)
		if err != nil {
			t.Fatalf("FixedResponseStep() error = %v", err)
		}
		if err := p.SetResponses([]provider.ScriptStep{step}); err != nil {
			t.Fatalf("SetResponses() error = %v", err)
		}
		_, result, err := collectStreamResult(p.Stream(context.Background(), mustRequest(t, "prompt")))
		if err != nil {
			t.Fatalf("collect stream: %v", err)
		}
		message, ok := result.(llm.AssistantTextMessage)
		if !ok {
			t.Fatalf("round trip type = %T, want AssistantTextMessage", result)
		}
		if len(message.Content()) != 1 || message.Content()[0].Text() != text {
			t.Fatalf("round trip text = %v, want %q", message.Content(), text)
		}
	})
}

func mustProvider(t *testing.T, config provider.ScriptedConfig) *provider.ScriptedProvider {
	t.Helper()
	p, err := provider.NewScriptedProvider(config)
	if err != nil {
		t.Fatalf("NewScriptedProvider() error = %v", err)
	}
	return p
}

func mustSetResponses(t *testing.T, p *provider.ScriptedProvider, steps ...provider.ScriptStep) {
	t.Helper()
	if err := p.SetResponses(steps); err != nil {
		t.Fatalf("SetResponses() error = %v", err)
	}
}

func mustAppendResponses(t *testing.T, p *provider.ScriptedProvider, steps ...provider.ScriptStep) {
	t.Helper()
	if err := p.AppendResponses(steps); err != nil {
		t.Fatalf("AppendResponses() error = %v", err)
	}
}

func mustFixedStep(t *testing.T, terminal llm.AssistantTerminal) provider.ScriptStep {
	t.Helper()
	step, err := provider.FixedResponseStep(terminal)
	if err != nil {
		t.Fatalf("FixedResponseStep() error = %v", err)
	}
	return step
}

func mustTextBlock(t *testing.T, text string) llm.TextBlock {
	t.Helper()
	block, err := llm.NewTextBlock(text)
	if err != nil {
		t.Fatalf("NewTextBlock() error = %v", err)
	}
	return block
}

func mustTextTerminal(t *testing.T, text string) llm.AssistantTextMessage {
	t.Helper()
	message, err := newAssistantTextMessage(
		[]llm.TextBlock{mustTextBlock(t, text)},
		llm.FinishStop,
		llm.Usage{},
		time.Time{},
	)
	if err != nil {
		t.Fatalf("NewAssistantTextMessage() error = %v", err)
	}
	return message
}

func mustToolTerminal(t *testing.T, id, name string, raw []byte) llm.AssistantToolUseMessage {
	t.Helper()
	call, err := llm.NewToolCallBlock(id, name, raw)
	if err != nil {
		t.Fatalf("NewToolCallBlock() error = %v", err)
	}
	message, err := newAssistantToolUseMessage(
		[]llm.AssistantBlock{call},
		llm.Usage{},
		time.Time{},
	)
	if err != nil {
		t.Fatalf("NewAssistantToolUseMessage() error = %v", err)
	}
	return message
}

func collectStream(t *testing.T, stream provider.EventStream) ([]llm.StreamEvent, llm.AssistantTerminal) {
	t.Helper()
	events, terminal, err := collectStreamResult(stream)
	if err != nil {
		t.Fatalf("collect stream: %v", err)
	}
	return events, terminal
}

func collectStreamResult(stream provider.EventStream) ([]llm.StreamEvent, llm.AssistantTerminal, error) {
	collector := &llm.StreamCollector{}
	var events []llm.StreamEvent
	for {
		event, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		events = append(events, event)
		if err := collector.Accept(event); err != nil {
			return nil, nil, err
		}
	}
	if err := stream.Close(); err != nil {
		return nil, nil, err
	}
	if err := collector.Close(); err != nil {
		return nil, nil, err
	}
	terminal, err := collector.Result()
	if err != nil {
		return nil, nil, err
	}
	return events, terminal, nil
}

func terminalText(t *testing.T, terminal llm.AssistantTerminal) string {
	t.Helper()
	message, ok := terminal.(llm.AssistantTextMessage)
	if !ok || len(message.Content()) != 1 {
		t.Fatalf("terminal = %T, want one-block AssistantTextMessage", terminal)
	}
	return message.Content()[0].Text()
}

func terminalFailure(t *testing.T, terminal llm.AssistantTerminal) llm.AssistantFailureMessage {
	t.Helper()
	failure, ok := terminal.(llm.AssistantFailureMessage)
	if !ok {
		t.Fatalf("terminal = %T, want AssistantFailureMessage", terminal)
	}
	return failure
}

func assertProviderFailure(
	t *testing.T,
	failure llm.AssistantFailureMessage,
	wantKind provider.FailureKind,
	wantCause error,
) {
	t.Helper()
	var providerFailure *provider.ProviderFailure
	if !errors.As(failure.Failure(), &providerFailure) {
		t.Fatalf("failure cause = %T, want *provider.ProviderFailure", failure.Failure().Cause())
	}
	if providerFailure.Kind() != wantKind {
		t.Fatalf("provider failure kind = %v, want %v", providerFailure.Kind(), wantKind)
	}
	if wantCause != nil && !errors.Is(failure.Failure(), wantCause) {
		t.Fatalf("provider failure cause = %v, want errors.Is(..., %v)", providerFailure.Cause(), wantCause)
	}
}

func eventKinds(events []llm.StreamEvent) []string {
	kinds := make([]string, len(events))
	for index, event := range events {
		kinds[index] = eventKind(event)
	}
	return kinds
}

func eventKind(event llm.StreamEvent) string {
	switch event.(type) {
	case llm.StartEvent:
		return "start"
	case llm.TextStartEvent:
		return "text_start"
	case llm.TextDeltaEvent:
		return "text_delta"
	case llm.TextEndEvent:
		return "text_end"
	case llm.ThinkingStartEvent:
		return "thinking_start"
	case llm.ThinkingDeltaEvent:
		return "thinking_delta"
	case llm.ThinkingEndEvent:
		return "thinking_end"
	case llm.ToolCallStartEvent:
		return "toolcall_start"
	case llm.ToolCallDeltaEvent:
		return "toolcall_delta"
	case llm.ToolCallEndEvent:
		return "toolcall_end"
	case llm.DoneEvent:
		return "done"
	case llm.ErrorEvent:
		return "error"
	default:
		return fmt.Sprintf("unknown:%T", event)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

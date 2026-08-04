package llm_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

func TestStreamCollectorSuccessAndImmutableSnapshots(t *testing.T) {
	t.Parallel()

	collector := &llm.StreamCollector{}
	accept(t, collector, newStartEvent(t))
	accept(t, collector, textStart(t, 0))
	accept(t, collector, textDelta(t, 0, "你"))

	firstSnapshot, err := collector.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	assertTextContent(t, firstSnapshot.TextContent(), "你")
	if firstSnapshot.FinishReason() != llm.FinishPending || firstSnapshot.Terminal() {
		t.Fatalf("first snapshot = (%v, terminal=%t), want pending non-terminal", firstSnapshot.FinishReason(), firstSnapshot.Terminal())
	}

	accept(t, collector, textDelta(t, 0, "好"))
	accept(t, collector, textEnd(t, 0, "你好"))
	accept(t, collector, textStart(t, 1))
	accept(t, collector, textDelta(t, 1, "done"))
	accept(t, collector, textEnd(t, 1, "done"))
	timestamp := time.Date(2026, time.August, 1, 2, 3, 4, 0, time.UTC)
	accept(t, collector, done(t, llm.FinishStop, timestamp))

	// A terminal event is not a result until provider EOF proves uniqueness.
	if _, err := collector.Result(); !errors.Is(err, llm.ErrStreamNotClosed) {
		t.Fatalf("Result() before Close error = %v, want ErrStreamNotClosed", err)
	}
	if err := collector.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	result, err := collector.Result()
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if result.FinishReason() != llm.FinishStop {
		t.Fatalf("FinishReason() = %v, want stop", result.FinishReason())
	}
	if !result.Timestamp().Equal(timestamp) {
		t.Fatalf("Timestamp() = %v, want %v", result.Timestamp(), timestamp)
	}
	textResult, ok := result.(llm.AssistantTextMessage)
	if !ok {
		t.Fatalf("result type = %T, want AssistantTextMessage", result)
	}
	assertTextContent(t, textResult.Content(), "你好", "done")

	// Later deltas and caller slice mutation cannot rewrite an earlier snapshot.
	assertTextContent(t, firstSnapshot.TextContent(), "你")
	mutated := textResult.Content()
	mutated[0] = mustTextBlock(t, "changed")
	assertTextContent(t, textResult.Content(), "你好", "done")
}

func TestStreamCollectorErrorBeforeStart(t *testing.T) {
	t.Parallel()

	collector := &llm.StreamCollector{}
	accept(t, collector, streamError(t, llm.FinishError, "setup failed"))
	if err := collector.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	result, err := collector.Result()
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	failure, ok := result.(llm.AssistantFailureMessage)
	if !ok {
		t.Fatalf("result type = %T, want AssistantFailureMessage", result)
	}
	if failure.ErrorMessage() != "setup failed" || failure.FinishReason() != llm.FinishError {
		t.Fatalf("failure = (%v, %q), want (error, setup failed)", failure.FinishReason(), failure.ErrorMessage())
	}
	if len(failure.Content()) != 0 {
		t.Fatalf("len(Content()) = %d, want 0", len(failure.Content()))
	}
}

func TestStreamCollectorErrorRetainsPartialText(t *testing.T) {
	t.Parallel()

	collector := &llm.StreamCollector{}
	accept(t, collector, newStartEvent(t))
	accept(t, collector, textStart(t, 0))
	accept(t, collector, textDelta(t, 0, "partial"))
	accept(t, collector, streamError(t, llm.FinishAborted, "cancelled"))
	if err := collector.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	result, err := collector.Result()
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	failure, ok := result.(llm.AssistantFailureMessage)
	if !ok {
		t.Fatalf("result type = %T, want AssistantFailureMessage", result)
	}
	assertTextContent(t, failure.Content(), "partial")
	if failure.FinishReason() != llm.FinishAborted {
		t.Fatalf("FinishReason() = %v, want aborted", failure.FinishReason())
	}
}

func TestStreamCollectorErrorRetainsThinkingAndCompleteToolCall(t *testing.T) {
	t.Parallel()

	collector := &llm.StreamCollector{}
	accept(t, collector, newStartEvent(t))
	thinkingStart, err := llm.NewThinkingStartEvent(0)
	if err != nil {
		t.Fatal(err)
	}
	thinkingDelta, err := llm.NewThinkingDeltaEvent(0, "partial plan")
	if err != nil {
		t.Fatal(err)
	}
	accept(t, collector, thinkingStart)
	accept(t, collector, thinkingDelta)
	thinking, err := llm.NewThinkingBlock("partial plan")
	if err != nil {
		t.Fatal(err)
	}
	thinkingEnd, err := llm.NewThinkingEndEvent(0, thinking)
	if err != nil {
		t.Fatal(err)
	}
	accept(t, collector, thinkingEnd)
	call := mustToolCall(t, "call-1", "inspect", []byte(`{"path":"README.md"}`))
	accept(t, collector, toolCallStart(t, 1, call.ID(), call.Name()))
	accept(t, collector, toolCallDelta(t, 1, call.ArgumentsJSON()))
	accept(t, collector, toolCallEnd(t, 1, call))
	accept(t, collector, streamError(t, llm.FinishError, "failed after planning"))
	if err := collector.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := collector.Result()
	if err != nil {
		t.Fatal(err)
	}
	failure, ok := result.(llm.AssistantFailureMessage)
	if !ok {
		t.Fatalf("result = %T", result)
	}
	blocks := failure.Blocks()
	if len(blocks) != 2 || blocks[0].(llm.ThinkingBlock).Thinking() != "partial plan" || blocks[1].(llm.ToolCallBlock).ID() != "call-1" {
		t.Fatalf("failure blocks = %#v", blocks)
	}
}

func TestStreamCollectorMixedTextAndToolCall(t *testing.T) {
	t.Parallel()

	collector := &llm.StreamCollector{}
	accept(t, collector, newStartEvent(t))
	accept(t, collector, textStart(t, 0))
	accept(t, collector, textDelta(t, 0, "running"))
	accept(t, collector, textEnd(t, 0, "running"))
	accept(t, collector, toolCallStart(t, 1, "call-1", "echo"))
	accept(t, collector, toolCallDelta(t, 1, []byte(`{"text":`)))

	partial, err := collector.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if completed := partial.CompletedBlocks(); len(completed) != 1 || completed[0].Kind() != llm.AssistantBlockText {
		t.Fatalf("completed blocks = %#v, want one text block", completed)
	}
	blocks := partial.Blocks()
	if len(blocks) != 2 {
		t.Fatalf("canonical partial blocks = %#v", blocks)
	}
	partialCall, ok := blocks[1].(llm.PartialToolCallBlock)
	if !ok || partialCall.ID() != "call-1" || partialCall.Name() != "echo" || !bytes.Equal(partialCall.ArgumentsFragment(), []byte(`{"text":`)) {
		t.Fatalf("canonical partial tool call = %#v", blocks[1])
	}
	active, ok := partial.ActiveBlock()
	if !ok || active.Kind() != llm.AssistantBlockToolCall || active.ContentIndex() != 1 {
		t.Fatalf("active block = (%v, %t), want tool call at index 1", active, ok)
	}
	id, name, arguments, ok := active.ToolCall()
	if !ok || id != "call-1" || name != "echo" || !bytes.Equal(arguments, []byte(`{"text":`)) {
		t.Fatalf("active ToolCall() = (%q, %q, %q, %t)", id, name, arguments, ok)
	}
	arguments[0] = '!'
	_, _, argumentsAgain, _ := active.ToolCall()
	if !bytes.Equal(argumentsAgain, []byte(`{"text":`)) {
		t.Fatalf("active arguments mutated through snapshot: %q", argumentsAgain)
	}

	accept(t, collector, toolCallDelta(t, 1, []byte(`"hello"}`)))
	call := mustToolCall(t, "call-1", "echo", []byte(`{"text":"hello"}`))
	accept(t, collector, toolCallEnd(t, 1, call))
	accept(t, collector, done(t, llm.FinishToolUse, time.Time{}))
	if err := collector.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	result, err := collector.Result()
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	toolUse, ok := result.(llm.AssistantToolUseMessage)
	if !ok {
		t.Fatalf("result type = %T, want AssistantToolUseMessage", result)
	}
	if toolUse.FinishReason() != llm.FinishToolUse {
		t.Fatalf("FinishReason() = %v, want toolUse", toolUse.FinishReason())
	}
	content := toolUse.Content()
	if len(content) != 2 || content[0].Kind() != llm.AssistantBlockText || content[1].Kind() != llm.AssistantBlockToolCall {
		t.Fatalf("result content kinds are not text/tool: %#v", content)
	}
	gotCall, ok := content[1].(llm.ToolCallBlock)
	if !ok || !bytes.Equal(gotCall.ArgumentsJSON(), []byte(`{"text":"hello"}`)) {
		t.Fatalf("result tool call = %#v, want preserved arguments", content[1])
	}
}

func TestStreamCollectorSupportsMultipleToolCalls(t *testing.T) {
	t.Parallel()

	collector := &llm.StreamCollector{}
	accept(t, collector, newStartEvent(t))
	for index := range 2 {
		id := "call-1"
		if index == 1 {
			id = "call-2"
		}
		accept(t, collector, toolCallStart(t, index, id, "echo"))
		accept(t, collector, toolCallDelta(t, index, []byte("{}")))
		accept(t, collector, toolCallEnd(t, index, mustToolCall(t, id, "echo", []byte("{}"))))
	}
	accept(t, collector, done(t, llm.FinishToolUse, time.Time{}))
	if err := collector.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	result, err := collector.Result()
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if got := len(result.Blocks()); got != 2 {
		t.Fatalf("len(Blocks()) = %d, want 2", got)
	}
}

func TestStreamCollectorErrorDoesNotExposeToolCall(t *testing.T) {
	t.Parallel()

	collector := &llm.StreamCollector{}
	accept(t, collector, newStartEvent(t))
	accept(t, collector, toolCallStart(t, 0, "call-1", "echo"))
	accept(t, collector, toolCallDelta(t, 0, []byte(`{"partial"`)))
	accept(t, collector, streamError(t, llm.FinishAborted, "cancelled"))
	if err := collector.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	result, err := collector.Result()
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if _, ok := result.(llm.AssistantFailureMessage); !ok {
		t.Fatalf("result type = %T, want AssistantFailureMessage", result)
	}
	if got := len(result.Blocks()); got != 0 {
		t.Fatalf("len(Blocks()) = %d, want 0; failed tool call must not be executable", got)
	}
}

func TestStreamCollectorRejectsMalformedOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		events func(t *testing.T) []llm.StreamEvent
	}{
		{
			name: "delta before start",
			events: func(t *testing.T) []llm.StreamEvent {
				return []llm.StreamEvent{textDelta(t, 0, "x")}
			},
		},
		{
			name: "done before start",
			events: func(t *testing.T) []llm.StreamEvent {
				return []llm.StreamEvent{done(t, llm.FinishStop, time.Time{})}
			},
		},
		{
			name: "second start",
			events: func(t *testing.T) []llm.StreamEvent {
				return []llm.StreamEvent{newStartEvent(t), newStartEvent(t)}
			},
		},
		{
			name: "non-sequential content index",
			events: func(t *testing.T) []llm.StreamEvent {
				return []llm.StreamEvent{newStartEvent(t), textStart(t, 1)}
			},
		},
		{
			name: "duplicate content index",
			events: func(t *testing.T) []llm.StreamEvent {
				return []llm.StreamEvent{newStartEvent(t), textStart(t, 0), textStart(t, 0)}
			},
		},
		{
			name: "delta index mismatch",
			events: func(t *testing.T) []llm.StreamEvent {
				return []llm.StreamEvent{newStartEvent(t), textStart(t, 0), textDelta(t, 1, "x")}
			},
		},
		{
			name: "end content mismatch",
			events: func(t *testing.T) []llm.StreamEvent {
				return []llm.StreamEvent{newStartEvent(t), textStart(t, 0), textDelta(t, 0, "x"), textEnd(t, 0, "y")}
			},
		},
		{
			name: "done with open block",
			events: func(t *testing.T) []llm.StreamEvent {
				return []llm.StreamEvent{newStartEvent(t), textStart(t, 0), done(t, llm.FinishStop, time.Time{})}
			},
		},
		{
			name: "tool-use terminal without a call",
			events: func(t *testing.T) []llm.StreamEvent {
				return []llm.StreamEvent{newStartEvent(t), done(t, llm.FinishToolUse, time.Time{})}
			},
		},
		{
			name: "tool delta without start",
			events: func(t *testing.T) []llm.StreamEvent {
				return []llm.StreamEvent{newStartEvent(t), toolCallDelta(t, 0, []byte("{}"))}
			},
		},
		{
			name: "tool end identity mismatch",
			events: func(t *testing.T) []llm.StreamEvent {
				return []llm.StreamEvent{
					newStartEvent(t),
					toolCallStart(t, 0, "call-1", "echo"),
					toolCallDelta(t, 0, []byte("{}")),
					toolCallEnd(t, 0, mustToolCall(t, "call-2", "echo", []byte("{}"))),
				}
			},
		},
		{
			name: "tool end arguments mismatch",
			events: func(t *testing.T) []llm.StreamEvent {
				return []llm.StreamEvent{
					newStartEvent(t),
					toolCallStart(t, 0, "call-1", "echo"),
					toolCallDelta(t, 0, []byte("{}")),
					toolCallEnd(t, 0, mustToolCall(t, "call-1", "echo", []byte(`{"x":1}`))),
				}
			},
		},
		{
			name: "stop terminal contains tool call",
			events: func(t *testing.T) []llm.StreamEvent {
				call := mustToolCall(t, "call-1", "echo", []byte("{}"))
				return []llm.StreamEvent{
					newStartEvent(t),
					toolCallStart(t, 0, "call-1", "echo"),
					toolCallDelta(t, 0, []byte("{}")),
					toolCallEnd(t, 0, call),
					done(t, llm.FinishStop, time.Time{}),
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			collector := &llm.StreamCollector{}
			var got error
			for _, event := range tt.events(t) {
				if got = collector.Accept(event); got != nil {
					break
				}
			}
			if !errors.Is(got, llm.ErrStreamProtocol) {
				t.Fatalf("Accept() error = %v, want ErrStreamProtocol", got)
			}
			if _, err := collector.Result(); !errors.Is(err, llm.ErrStreamProtocol) {
				t.Fatalf("Result() error = %v, want persisted protocol error", err)
			}
		})
	}
}

func TestStreamCollectorAcceptsInterleavedToolCalls(t *testing.T) {
	t.Parallel()
	collector := &llm.StreamCollector{}
	first := mustToolCall(t, "call-a", "echo", []byte(`{"a":1}`))
	second := mustToolCall(t, "call-b", "echo", []byte(`{"b":2}`))
	events := []llm.StreamEvent{
		newStartEvent(t),
		toolCallStart(t, 0, "call-a", "echo"), toolCallStart(t, 1, "call-b", "echo"),
		toolCallDelta(t, 1, []byte(`{"b":2}`)), toolCallDelta(t, 0, []byte(`{"a":1}`)),
		toolCallEnd(t, 1, second), toolCallEnd(t, 0, first), done(t, llm.FinishToolUse, time.Time{}),
	}
	for _, event := range events {
		if err := collector.Accept(event); err != nil {
			t.Fatalf("Accept(%T): %v", event, err)
		}
	}
	if err := collector.Close(); err != nil {
		t.Fatal(err)
	}
	terminal, err := collector.Result()
	if err != nil {
		t.Fatal(err)
	}
	blocks := terminal.Blocks()
	if len(blocks) != 2 || blocks[0].(llm.ToolCallBlock).ID() != "call-a" || blocks[1].(llm.ToolCallBlock).ID() != "call-b" {
		t.Fatalf("blocks=%#v", blocks)
	}
}

func TestStreamCollectorRejectsDuplicateTerminal(t *testing.T) {
	t.Parallel()

	collector := &llm.StreamCollector{}
	accept(t, collector, newStartEvent(t))
	accept(t, collector, done(t, llm.FinishStop, time.Time{}))
	err := collector.Accept(streamError(t, llm.FinishError, "late error"))
	if !errors.Is(err, llm.ErrDuplicateTerminal) || !errors.Is(err, llm.ErrStreamProtocol) {
		t.Fatalf("duplicate terminal error = %v, want ErrDuplicateTerminal and ErrStreamProtocol", err)
	}
	if err := collector.Close(); !errors.Is(err, llm.ErrDuplicateTerminal) {
		t.Fatalf("Close() error = %v, want persisted duplicate terminal", err)
	}
}

func TestStreamCollectorRejectsEveryDuplicateTerminalPair(t *testing.T) {
	t.Parallel()

	type terminalKind uint8
	const (
		doneTerminal terminalKind = iota
		errorTerminal
	)

	tests := []struct {
		name   string
		first  terminalKind
		second terminalKind
	}{
		{name: "done then done", first: doneTerminal, second: doneTerminal},
		{name: "done then error", first: doneTerminal, second: errorTerminal},
		{name: "error then done", first: errorTerminal, second: doneTerminal},
		{name: "error then error", first: errorTerminal, second: errorTerminal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			collector := &llm.StreamCollector{}
			if tt.first == doneTerminal {
				accept(t, collector, newStartEvent(t))
				accept(t, collector, done(t, llm.FinishStop, time.Time{}))
			} else {
				accept(t, collector, streamError(t, llm.FinishError, "first"))
			}

			var second llm.StreamEvent
			if tt.second == doneTerminal {
				second = done(t, llm.FinishStop, time.Time{})
			} else {
				second = streamError(t, llm.FinishError, "second")
			}
			err := collector.Accept(second)
			if !errors.Is(err, llm.ErrDuplicateTerminal) || !errors.Is(err, llm.ErrStreamProtocol) {
				t.Fatalf("Accept(second terminal) error = %v, want duplicate protocol error", err)
			}
		})
	}
}

func TestStreamCollectorRejectsEventAfterTerminal(t *testing.T) {
	t.Parallel()

	collector := &llm.StreamCollector{}
	accept(t, collector, newStartEvent(t))
	accept(t, collector, done(t, llm.FinishStop, time.Time{}))
	err := collector.Accept(textStart(t, 0))
	if !errors.Is(err, llm.ErrStreamProtocol) || errors.Is(err, llm.ErrDuplicateTerminal) {
		t.Fatalf("event-after-terminal error = %v, want non-duplicate protocol error", err)
	}
}

func TestStreamCollectorRejectsExportedZeroValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, collector *llm.StreamCollector)
		event llm.StreamEvent
	}{
		{name: "zero error", event: llm.ErrorEvent{}},
		{
			name: "zero done",
			setup: func(t *testing.T, collector *llm.StreamCollector) {
				accept(t, collector, newStartEvent(t))
			},
			event: llm.DoneEvent{},
		},
		{
			name: "zero tool start",
			setup: func(t *testing.T, collector *llm.StreamCollector) {
				accept(t, collector, newStartEvent(t))
			},
			event: llm.ToolCallStartEvent{},
		},
		{
			name: "zero tool end",
			setup: func(t *testing.T, collector *llm.StreamCollector) {
				accept(t, collector, newStartEvent(t))
				accept(t, collector, toolCallStart(t, 0, "call", "echo"))
			},
			event: llm.ToolCallEndEvent{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			collector := &llm.StreamCollector{}
			if tt.setup != nil {
				tt.setup(t, collector)
			}
			err := collector.Accept(tt.event)
			if !errors.Is(err, llm.ErrInvalidStreamEvent) || !errors.Is(err, llm.ErrStreamProtocol) {
				t.Fatalf("Accept(zero event) error = %v, want invalid stream protocol error", err)
			}
		})
	}
}

func TestValidateAssistantTerminalRejectsZeroAndPointerValues(t *testing.T) {
	t.Parallel()

	var textPointer *llm.AssistantTextMessage
	terminals := []llm.AssistantTerminal{
		llm.AssistantTextMessage{},
		llm.AssistantToolUseMessage{},
		llm.AssistantFailureMessage{},
		textPointer,
	}
	for _, terminal := range terminals {
		if err := llm.ValidateAssistantTerminal(terminal); err == nil {
			t.Fatalf("ValidateAssistantTerminal(%T) error = nil, want error", terminal)
		}
	}
}

func TestStreamCollectorRejectsUnexpectedEOF(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, collector *llm.StreamCollector)
	}{
		{name: "before start"},
		{
			name: "after start",
			setup: func(t *testing.T, collector *llm.StreamCollector) {
				accept(t, collector, newStartEvent(t))
			},
		},
		{
			name: "mid block",
			setup: func(t *testing.T, collector *llm.StreamCollector) {
				accept(t, collector, newStartEvent(t))
				accept(t, collector, textStart(t, 0))
				accept(t, collector, textDelta(t, 0, "partial"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			collector := &llm.StreamCollector{}
			if tt.setup != nil {
				tt.setup(t, collector)
			}
			err := collector.Close()
			if !errors.Is(err, llm.ErrUnexpectedEOF) || !errors.Is(err, llm.ErrStreamProtocol) {
				t.Fatalf("Close() error = %v, want ErrUnexpectedEOF and ErrStreamProtocol", err)
			}
		})
	}
}

func TestStreamEventConstructorsRejectInvalidTerminalAndText(t *testing.T) {
	t.Parallel()

	if _, err := llm.NewDoneEvent(llm.FinishPending, llm.Usage{}, time.Time{}, testAssistantProvenance()); !errors.Is(err, llm.ErrInvalidStreamEvent) {
		t.Fatalf("NewDoneEvent(pending) error = %v, want ErrInvalidStreamEvent", err)
	}
	if _, err := llm.NewErrorEvent(llm.FinishStop, "bad", llm.Usage{}, time.Time{}, testAssistantProvenance()); !errors.Is(err, llm.ErrInvalidStreamEvent) {
		t.Fatalf("NewErrorEvent(stop) error = %v, want ErrInvalidStreamEvent", err)
	}
	if _, err := llm.NewErrorEvent(llm.FinishError, " ", llm.Usage{}, time.Time{}, testAssistantProvenance()); !errors.Is(err, llm.ErrInvalidStreamEvent) {
		t.Fatalf("NewErrorEvent(blank) error = %v, want ErrInvalidStreamEvent", err)
	}
	if _, err := llm.NewTextDeltaEvent(-1, "x"); !errors.Is(err, llm.ErrInvalidStreamEvent) {
		t.Fatalf("NewTextDeltaEvent(-1) error = %v, want ErrInvalidStreamEvent", err)
	}
	if _, err := llm.NewTextDeltaEvent(0, string([]byte{0xff})); !errors.Is(err, llm.ErrInvalidStreamEvent) {
		t.Fatalf("NewTextDeltaEvent(invalid UTF-8) error = %v, want ErrInvalidStreamEvent", err)
	}
}

func FuzzStreamCollectorDoesNotPanic(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4})
	f.Add([]byte{0, 4, 5})
	f.Add([]byte{5})
	f.Add([]byte{0, 1, 2, 6})

	f.Fuzz(func(t *testing.T, operations []byte) {
		collector := &llm.StreamCollector{}
		for _, operation := range operations {
			var event llm.StreamEvent
			switch operation % 7 {
			case 0:
				event = newStartEvent(t)
			case 1:
				event = textStart(t, int(operation>>4))
			case 2:
				event = textDelta(t, int(operation>>4), string(rune(operation)))
			case 3:
				event = textEnd(t, int(operation>>4), string(rune(operation)))
			case 4:
				event = done(t, llm.FinishStop, time.Time{})
			case 5:
				event = streamError(t, llm.FinishError, "fuzz error")
			case 6:
				event = done(t, llm.FinishToolUse, time.Time{})
			}
			_ = collector.Accept(event)
		}
		_ = collector.Close()
		_, _ = collector.Snapshot()
		_, _ = collector.Result()
	})
}

func accept(t *testing.T, collector *llm.StreamCollector, event llm.StreamEvent) {
	t.Helper()

	if err := collector.Accept(event); err != nil {
		t.Fatalf("Accept(%T) error = %v", event, err)
	}
}

func textStart(t *testing.T, index int) llm.TextStartEvent {
	t.Helper()

	event, err := llm.NewTextStartEvent(index)
	if err != nil {
		t.Fatalf("NewTextStartEvent() error = %v", err)
	}
	return event
}

func textDelta(t *testing.T, index int, delta string) llm.TextDeltaEvent {
	t.Helper()

	event, err := llm.NewTextDeltaEvent(index, delta)
	if err != nil {
		t.Fatalf("NewTextDeltaEvent() error = %v", err)
	}
	return event
}

func textEnd(t *testing.T, index int, content string) llm.TextEndEvent {
	t.Helper()

	event, err := llm.NewTextEndEvent(index, content)
	if err != nil {
		t.Fatalf("NewTextEndEvent() error = %v", err)
	}
	return event
}

func toolCallStart(t *testing.T, index int, id, name string) llm.ToolCallStartEvent {
	t.Helper()

	event, err := llm.NewToolCallStartEvent(index, id, name)
	if err != nil {
		t.Fatalf("NewToolCallStartEvent() error = %v", err)
	}
	return event
}

func toolCallDelta(t *testing.T, index int, delta []byte) llm.ToolCallDeltaEvent {
	t.Helper()

	event, err := llm.NewToolCallDeltaEvent(index, delta)
	if err != nil {
		t.Fatalf("NewToolCallDeltaEvent() error = %v", err)
	}
	return event
}

func toolCallEnd(t *testing.T, index int, call llm.ToolCallBlock) llm.ToolCallEndEvent {
	t.Helper()

	event, err := llm.NewToolCallEndEvent(index, call)
	if err != nil {
		t.Fatalf("NewToolCallEndEvent() error = %v", err)
	}
	return event
}

func mustToolCall(t *testing.T, id, name string, arguments []byte) llm.ToolCallBlock {
	t.Helper()

	call, err := llm.NewToolCallBlock(id, name, arguments)
	if err != nil {
		t.Fatalf("NewToolCallBlock() error = %v", err)
	}
	return call
}

func done(t *testing.T, reason llm.FinishReason, timestamp time.Time) llm.DoneEvent {
	t.Helper()

	event, err := llm.NewDoneEvent(reason, llm.Usage{}, timestamp, testAssistantProvenance())
	if err != nil {
		t.Fatalf("NewDoneEvent() error = %v", err)
	}
	return event
}

func streamError(t *testing.T, reason llm.FinishReason, message string) llm.ErrorEvent {
	t.Helper()

	event, err := llm.NewErrorEvent(reason, message, llm.Usage{}, time.Time{}, testAssistantProvenance())
	if err != nil {
		t.Fatalf("NewErrorEvent() error = %v", err)
	}
	return event
}

func assertTextContent(t *testing.T, content []llm.TextBlock, want ...string) {
	t.Helper()

	if len(content) != len(want) {
		t.Fatalf("len(content) = %d, want %d", len(content), len(want))
	}
	for index, block := range content {
		if got := block.Text(); got != want[index] {
			t.Fatalf("content[%d] = %q, want %q", index, got, want[index])
		}
	}
}

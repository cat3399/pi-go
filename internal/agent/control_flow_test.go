package agent_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
	"github.com/cat3399/pi-go/internal/session"
)

type namedBatchTool struct {
	mu      sync.Mutex
	started map[string]chan struct{}
	release map[string]chan struct{}
	mode    agent.ToolExecutionMode
}

type mixedTool struct{}

type admissionTranscript struct {
	base *session.Session

	mu      sync.Mutex
	armed   bool
	blocked bool
	entered chan struct{}
	release chan struct{}
	reenter func()
}

type queuedAppendFaultTranscript struct {
	base *session.Session

	mu      sync.Mutex
	attempt int
	blockAt int
	failAt  int
	entered chan struct{}
	release chan struct{}
}

func (t *queuedAppendFaultTranscript) Context() session.Context { return t.base.Context() }

func (t *queuedAppendFaultTranscript) Append(
	ctx context.Context,
	message llm.ConversationMessage,
	options session.AppendOptions,
) (session.Entry, error) {
	text, queued := queuedUserText(message)
	if !queued {
		return t.base.Append(ctx, message, options)
	}
	t.mu.Lock()
	t.attempt++
	attempt := t.attempt
	block := attempt == t.blockAt
	fail := attempt == t.failAt
	t.mu.Unlock()
	if block {
		close(t.entered)
		<-t.release
	}
	if fail {
		return session.Entry{}, errors.New("injected queued append failure for " + text)
	}
	return t.base.Append(ctx, message, options)
}

func queuedUserText(message llm.ConversationMessage) (string, bool) {
	user, ok := message.(llm.UserTextMessage)
	if !ok {
		return "", false
	}
	content := user.Content()
	if len(content) != 1 {
		return "", false
	}
	text := content[0].Text()
	return text, strings.HasPrefix(text, "queue:")
}

func queuedTexts(messages []llm.ConversationMessage) []string {
	texts := make([]string, 0)
	for _, message := range messages {
		if text, ok := queuedUserText(message); ok {
			texts = append(texts, text)
		}
	}
	return texts
}

func queueSnapshotTexts(messages []llm.UserTextMessage) []string {
	texts := make([]string, len(messages))
	for index, message := range messages {
		content := message.Content()
		if len(content) == 1 {
			texts[index] = content[0].Text()
		}
	}
	return texts
}

func (t *admissionTranscript) arm(reenter func()) {
	t.mu.Lock()
	t.armed = true
	t.reenter = reenter
	t.mu.Unlock()
}

func (t *admissionTranscript) Context() session.Context {
	t.mu.Lock()
	block := t.armed && !t.blocked
	if block {
		t.blocked = true
	}
	entered, release, reenter := t.entered, t.release, t.reenter
	t.mu.Unlock()
	if block {
		close(entered)
		if reenter != nil {
			reenter()
		}
		<-release
	}
	return t.base.Context()
}

func (t *admissionTranscript) Append(
	ctx context.Context,
	message llm.ConversationMessage,
	options session.AppendOptions,
) (session.Entry, error) {
	return t.base.Append(ctx, message, options)
}

func (mixedTool) Name() string              { return "mixed" }
func (mixedTool) Supports(name string) bool { return name != "missing" }
func (mixedTool) Execute(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return agent.ToolOutput{}, nil
}
func (mixedTool) ExecuteNamed(_ context.Context, name string, _ []byte, _ func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	switch name {
	case "failure":
		return agent.ToolOutput{Text: "failed"}, errors.New("tool failed")
	case "terminate":
		return agent.ToolOutput{Text: "ending", Terminate: true}, nil
	default:
		return agent.ToolOutput{Text: name}, nil
	}
}

func (t *namedBatchTool) Name() string              { return "batch" }
func (t *namedBatchTool) Supports(name string) bool { return name == "slow" || name == "fast" }
func (t *namedBatchTool) ToolExecutionMode(string) (agent.ToolExecutionMode, bool) {
	return t.mode, t.mode != 0
}
func (t *namedBatchTool) Execute(context.Context, []byte, func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	return agent.ToolOutput{}, nil
}
func (t *namedBatchTool) ExecuteNamed(ctx context.Context, name string, _ []byte, report func(agent.ToolUpdate)) (agent.ToolOutput, error) {
	close(t.started[name])
	select {
	case <-t.release[name]:
	case <-ctx.Done():
		return agent.ToolOutput{Text: "cancelled"}, context.Cause(ctx)
	}
	report(agent.ToolUpdate{Text: name + " done"})
	return agent.ToolOutput{Text: name}, nil
}

func newQueueAgent(
	t *testing.T,
	transcript agent.Transcript,
	providerImpl provider.Provider,
	steeringMode agent.QueueMode,
	followUpMode agent.QueueMode,
) *agent.Agent {
	t.Helper()
	model, err := provider.NewModelRef("scripted", "scripted", "scripted-1")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.New(agent.Config{
		Provider:          providerImpl,
		Transcript:        transcript,
		Model:             model,
		SteeringMode:      steeringMode,
		FollowUpMode:      followUpMode,
		Now:               func() time.Time { return agentTestEpoch },
		SettlementTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func TestContinueQueueAllAcknowledgesDurablePrefixAndPreservesFaultRemainder(t *testing.T) {
	base := newSession(t)
	transcript := &queuedAppendFaultTranscript{
		base:    base,
		blockAt: 1,
		failAt:  2,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	scripted := newScriptedProvider(t,
		mustTextTerminal(t, "first"),
		mustTextTerminal(t, "prefix accepted"),
		mustTextTerminal(t, "remainder accepted"),
	)
	runtime := newQueueAgent(t, transcript, scripted, agent.QueueAll, agent.QueueOneAtATime)
	if _, err := runtime.Run(context.Background(), "initial"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"queue:a", "queue:b"} {
		if err := runtime.Steer(text); err != nil {
			t.Fatal(err)
		}
	}
	ackSnapshot := make(chan []string, 1)
	runtime.Subscribe(func(_ context.Context, event agent.Event) {
		text, queued := queuedUserText(event.Message)
		if event.Kind != agent.EventMessageCommitted || !queued || text != "queue:a" {
			return
		}
		steering, _ := runtime.Queues()
		ackSnapshot <- queueSnapshotTexts(steering)
	})
	continueDone := make(chan error, 1)
	go func() { _, err := runtime.Continue(context.Background()); continueDone <- err }()
	waitClosed(t, transcript.entered, "first reserved queue append")
	if err := runtime.Steer("queue:cleared"); err != nil {
		t.Fatal(err)
	}
	runtime.ClearSteeringQueue()
	if err := runtime.Steer("queue:after-clear"); err != nil {
		t.Fatal(err)
	}
	steering, followUp := runtime.Queues()
	if got := queueSnapshotTexts(steering); !reflect.DeepEqual(got, []string{"queue:a", "queue:b", "queue:after-clear"}) || len(followUp) != 0 {
		t.Fatalf("queue during reservation/clear = %v / %d", got, len(followUp))
	}
	close(transcript.release)
	if err := <-continueDone; !errors.Is(err, agent.ErrTranscriptCommit) {
		t.Fatalf("Continue error = %v, want queued append fault", err)
	}
	if got := <-ackSnapshot; !reflect.DeepEqual(got, []string{"queue:b", "queue:after-clear"}) {
		t.Fatalf("queue at durable commit event = %v", got)
	}
	steering, followUp = runtime.Queues()
	if got := queueSnapshotTexts(steering); !reflect.DeepEqual(got, []string{"queue:b", "queue:after-clear"}) || len(followUp) != 0 {
		t.Fatalf("queue after middle append fault = %v / %d", got, len(followUp))
	}
	if got := queuedTexts(base.Context().Messages()); !reflect.DeepEqual(got, []string{"queue:a"}) {
		t.Fatalf("durable prefix after fault = %v", got)
	}
	if _, err := runtime.Continue(context.Background()); err != nil {
		t.Fatalf("Continue retry error = %v", err)
	}
	if got := queuedTexts(base.Context().Messages()); !reflect.DeepEqual(got, []string{"queue:a", "queue:b", "queue:after-clear"}) {
		t.Fatalf("durable queued messages after retry = %v", got)
	}
	if steering, followUp := runtime.Queues(); len(steering) != 0 || len(followUp) != 0 {
		t.Fatalf("queues after retry = %d/%d", len(steering), len(followUp))
	}
}

func TestActiveFollowUpQueueOneFirstAppendFaultRetainsFIFOForContinue(t *testing.T) {
	base := newSession(t)
	transcript := &queuedAppendFaultTranscript{base: base, failAt: 1}
	scripted := newScriptedProvider(t,
		mustTextTerminal(t, "first"),
		mustTextTerminal(t, "first follow-up"),
		mustTextTerminal(t, "second follow-up"),
	)
	runtime := newQueueAgent(t, transcript, scripted, agent.QueueOneAtATime, agent.QueueOneAtATime)
	for _, text := range []string{"queue:one", "queue:two"} {
		if err := runtime.FollowUp(text); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runtime.Run(context.Background(), "initial"); !errors.Is(err, agent.ErrTranscriptCommit) {
		t.Fatalf("Run error = %v, want first follow-up append fault", err)
	}
	steering, followUp := runtime.Queues()
	if got := queueSnapshotTexts(followUp); len(steering) != 0 || !reflect.DeepEqual(got, []string{"queue:one", "queue:two"}) {
		t.Fatalf("queues after first follow-up fault = %d / %v", len(steering), got)
	}
	if got := queuedTexts(base.Context().Messages()); len(got) != 0 {
		t.Fatalf("failed first follow-up became durable: %v", got)
	}
	if _, err := runtime.Continue(context.Background()); err != nil {
		t.Fatalf("Continue retry error = %v", err)
	}
	if got := queuedTexts(base.Context().Messages()); !reflect.DeepEqual(got, []string{"queue:one", "queue:two"}) {
		t.Fatalf("follow-up FIFO after retry = %v", got)
	}
	if steering, followUp := runtime.Queues(); len(steering) != 0 || len(followUp) != 0 {
		t.Fatalf("queues after follow-up retry = %d/%d", len(steering), len(followUp))
	}
}

func TestAbortAndClearPreserveReservedQueueUntilDurableAcknowledgement(t *testing.T) {
	base := newSession(t)
	transcript := &queuedAppendFaultTranscript{
		base:    base,
		blockAt: 1,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	scripted := newScriptedProvider(t, mustTextTerminal(t, "first"), mustTextTerminal(t, "unused"))
	runtime := newQueueAgent(t, transcript, scripted, agent.QueueAll, agent.QueueOneAtATime)
	if _, err := runtime.Run(context.Background(), "initial"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"queue:a", "queue:b"} {
		if err := runtime.Steer(text); err != nil {
			t.Fatal(err)
		}
	}
	continueDone := make(chan error, 1)
	go func() { _, err := runtime.Continue(context.Background()); continueDone <- err }()
	waitClosed(t, transcript.entered, "reserved queue append before abort")
	runtime.ClearAllQueues()
	waitCtx, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	if err := runtime.Abort(waitCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Abort bounded wait error = %v", err)
	}
	if steering, followUp := runtime.Queues(); !reflect.DeepEqual(queueSnapshotTexts(steering), []string{"queue:a", "queue:b"}) || len(followUp) != 0 {
		t.Fatalf("clear removed reserved queue: %v/%d", queueSnapshotTexts(steering), len(followUp))
	}
	close(transcript.release)
	if err := <-continueDone; err != nil {
		t.Fatalf("aborted Continue error = %v", err)
	}
	if got := queuedTexts(base.Context().Messages()); !reflect.DeepEqual(got, []string{"queue:a", "queue:b"}) {
		t.Fatalf("aborted reserved queue durable messages = %v", got)
	}
	if steering, followUp := runtime.Queues(); len(steering) != 0 || len(followUp) != 0 {
		t.Fatalf("queues after aborted durable acknowledgement = %d/%d", len(steering), len(followUp))
	}
}

func TestContinueAdmissionReservationLeavesTranscriptPortReentrant(t *testing.T) {
	base := newSession(t)
	transcript := &admissionTranscript{
		base:    base,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	scripted := newScriptedProvider(t,
		mustTextTerminal(t, "first"),
		mustTextTerminal(t, "continued"),
		mustTextTerminal(t, "drained"),
	)
	runtime := newAgent(t, transcript, scripted, nil)
	if _, err := runtime.Run(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Steer("before snapshot"); err != nil {
		t.Fatal(err)
	}
	reentered := make(chan struct{})
	transcript.arm(func() {
		if steering, followUp := runtime.Queues(); len(steering) != 1 || len(followUp) != 0 {
			t.Errorf("reentrant queue snapshot = %d/%d", len(steering), len(followUp))
		}
		if err := runtime.Steer("during snapshot"); err != nil {
			t.Errorf("reentrant Steer() error = %v", err)
		}
		close(reentered)
	})
	continueDone := make(chan error, 1)
	go func() { _, err := runtime.Continue(context.Background()); continueDone <- err }()
	waitClosed(t, transcript.entered, "continuation transcript snapshot")
	waitClosed(t, reentered, "reentrant agent access")
	if _, err := runtime.Run(context.Background(), "must not reserve over continuation"); !errors.Is(err, agent.ErrBusy) {
		t.Fatalf("Run during continuation reservation error = %v, want busy", err)
	}
	if _, err := runtime.Continue(context.Background()); !errors.Is(err, agent.ErrBusy) {
		t.Fatalf("Continue during continuation reservation error = %v, want busy", err)
	}
	if steering, followUp := runtime.Queues(); len(steering) != 2 || len(followUp) != 0 {
		t.Fatalf("queues during continuation reservation = %d/%d", len(steering), len(followUp))
	}
	close(transcript.release)
	if err := <-continueDone; err != nil {
		t.Fatal(err)
	}
	if scripted.CallCount() != 3 {
		t.Fatalf("provider calls = %d, want continuation plus queued drain", scripted.CallCount())
	}
	if steering, followUp := runtime.Queues(); len(steering) != 0 || len(followUp) != 0 {
		t.Fatalf("queues after continuation = %d/%d", len(steering), len(followUp))
	}
}

func TestContinueTailFailureReleasesAdmissionReservation(t *testing.T) {
	base := newSession(t)
	transcript := &admissionTranscript{
		base:    base,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	scripted := newScriptedProvider(t, mustTextTerminal(t, "first"), mustTextTerminal(t, "next"))
	runtime := newAgent(t, transcript, scripted, nil)
	if _, err := runtime.Run(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	transcript.arm(nil)
	continueDone := make(chan error, 1)
	go func() { _, err := runtime.Continue(context.Background()); continueDone <- err }()
	waitClosed(t, transcript.entered, "tail validation transcript snapshot")
	if _, err := runtime.Run(context.Background(), "must wait for failed admission"); !errors.Is(err, agent.ErrBusy) {
		t.Fatalf("Run during tail validation error = %v, want busy", err)
	}
	close(transcript.release)
	if err := <-continueDone; !errors.Is(err, agent.ErrCannotContinue) {
		t.Fatalf("Continue error = %v, want assistant-tail rejection", err)
	}
	if _, err := runtime.Run(context.Background(), "next"); err != nil {
		t.Fatalf("Run after failed continuation admission error = %v", err)
	}
}

func TestContinueCancellationDuringSnapshotReleasesReservationWithoutDraining(t *testing.T) {
	base := newSession(t)
	transcript := &admissionTranscript{
		base:    base,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	scripted := newScriptedProvider(t, mustTextTerminal(t, "first"), mustTextTerminal(t, "after cancellation"))
	runtime := newAgent(t, transcript, scripted, nil)
	if _, err := runtime.Run(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Steer("must remain queued"); err != nil {
		t.Fatal(err)
	}
	transcript.arm(nil)
	ctx, cancel := context.WithCancel(context.Background())
	continueDone := make(chan error, 1)
	go func() { _, err := runtime.Continue(ctx); continueDone <- err }()
	waitClosed(t, transcript.entered, "cancelled continuation transcript snapshot")
	cancel()
	close(transcript.release)
	if err := <-continueDone; !errors.Is(err, agent.ErrInvalidRun) {
		t.Fatalf("Continue error = %v, want cancelled-admission rejection", err)
	}
	if steering, followUp := runtime.Queues(); len(steering) != 1 || len(followUp) != 0 {
		t.Fatalf("cancelled admission drained queues: %d/%d", len(steering), len(followUp))
	}
	if _, err := runtime.Continue(context.Background()); err != nil {
		t.Fatalf("Continue after cancelled admission error = %v", err)
	}
}

func TestStatePendingToolCallsTracksSequentialAndParallelBatches(t *testing.T) {
	t.Run("sequential changes the legacy single-call view per call", func(t *testing.T) {
		transcript := newSession(t)
		first, err := llm.NewToolCallBlock("one", "slow", []byte(`{"x":1}`))
		if err != nil {
			t.Fatal(err)
		}
		second, err := llm.NewToolCallBlock("two", "fast", []byte(`{"x":2}`))
		if err != nil {
			t.Fatal(err)
		}
		assistant, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{first, second}, mustUsage(t, 3, 2), agentTestEpoch)
		if err != nil {
			t.Fatal(err)
		}
		scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "done"))
		tool := &namedBatchTool{
			mode:    agent.ToolExecutionSequential,
			started: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
			release: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
		}
		runtime := newAgent(t, transcript, scripted, tool)
		states := make(chan agent.State, 2)
		runtime.Subscribe(func(_ context.Context, event agent.Event) {
			if event.Kind == agent.EventToolStarted {
				states <- runtime.State()
			}
		})
		done := make(chan error, 1)
		go func() { _, err := runtime.Run(context.Background(), "go"); done <- err }()
		waitClosed(t, tool.started["slow"], "first sequential tool")
		assertSinglePendingCall(t, <-states, "one")
		close(tool.release["slow"])
		waitClosed(t, tool.started["fast"], "second sequential tool")
		assertSinglePendingCall(t, <-states, "two")
		close(tool.release["fast"])
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("parallel exposes the full immutable batch and hides legacy singleton", func(t *testing.T) {
		transcript := newSession(t)
		first, err := llm.NewToolCallBlock("one", "slow", []byte(`{"x":1}`))
		if err != nil {
			t.Fatal(err)
		}
		second, err := llm.NewToolCallBlock("two", "fast", []byte(`{"x":2}`))
		if err != nil {
			t.Fatal(err)
		}
		assistant, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{first, second}, mustUsage(t, 3, 2), agentTestEpoch)
		if err != nil {
			t.Fatal(err)
		}
		scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "done"))
		tool := &namedBatchTool{
			started: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
			release: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
		}
		runtime := newAgent(t, transcript, scripted, tool)
		type stateEvent struct {
			callID string
			state  agent.State
		}
		started := make(chan stateEvent, 2)
		settled := make(chan stateEvent, 2)
		runtime.Subscribe(func(_ context.Context, event agent.Event) {
			switch event.Kind {
			case agent.EventToolStarted:
				started <- stateEvent{callID: event.ToolCallID, state: runtime.State()}
			case agent.EventToolSettled:
				settled <- stateEvent{callID: event.ToolCallID, state: runtime.State()}
			}
		})
		done := make(chan error, 1)
		go func() { _, err := runtime.Run(context.Background(), "go"); done <- err }()
		waitClosed(t, tool.started["slow"], "parallel slow tool")
		waitClosed(t, tool.started["fast"], "parallel fast tool")
		firstStart := <-started
		if firstStart.callID != "one" {
			t.Fatalf("first parallel start = %q", firstStart.callID)
		}
		assertSinglePendingCall(t, firstStart.state, "one")
		secondStart := <-started
		if secondStart.callID != "two" {
			t.Fatalf("second parallel start = %q", secondStart.callID)
		}
		if got := secondStart.state.PendingToolCalls(); !reflect.DeepEqual(got, []string{"one", "two"}) {
			t.Fatalf("parallel pending calls = %v", got)
		}
		if call, ok := secondStart.state.PendingToolCall(); ok || call != "" {
			t.Fatalf("legacy pending call for parallel batch = %q/%t", call, ok)
		}
		mutated := secondStart.state.PendingToolCalls()
		mutated[0] = "changed"
		if got := secondStart.state.PendingToolCalls(); !reflect.DeepEqual(got, []string{"one", "two"}) {
			t.Fatalf("pending snapshot mutated through caller slice: %v", got)
		}
		close(tool.release["fast"])
		fastSettled := <-settled
		if fastSettled.callID != "two" {
			t.Fatalf("first parallel settled call = %q", fastSettled.callID)
		}
		assertSinglePendingCall(t, fastSettled.state, "one")
		close(tool.release["slow"])
		slowSettled := <-settled
		if slowSettled.callID != "one" {
			t.Fatalf("second parallel settled call = %q", slowSettled.callID)
		}
		if got := slowSettled.state.PendingToolCalls(); len(got) != 0 {
			t.Fatalf("pending calls after final parallel settlement = %v", got)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func assertSinglePendingCall(t *testing.T, state agent.State, want string) {
	t.Helper()
	if got := state.PendingToolCalls(); !reflect.DeepEqual(got, []string{want}) {
		t.Fatalf("pending calls = %v, want [%s]", got, want)
	}
	if got, ok := state.PendingToolCall(); !ok || got != want {
		t.Fatalf("legacy pending call = %q/%t, want %q/true", got, ok, want)
	}
}

func TestParallelBatchSettlesEventsByCompletionButCommitsSourceOrder(t *testing.T) {
	transcript := newSession(t)
	slow, err := llm.NewToolCallBlock("one", "slow", []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	fast, err := llm.NewToolCallBlock("two", "fast", []byte(`{"x":2}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{slow, fast}, mustUsage(t, 3, 2), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "complete"))
	tool := &namedBatchTool{started: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})}, release: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})}}
	runtime := newAgent(t, transcript, scripted, tool)
	var mu sync.Mutex
	var settled []string
	runtime.Subscribe(func(_ context.Context, event agent.Event) {
		if event.Kind == agent.EventToolSettled {
			mu.Lock()
			settled = append(settled, event.ToolName)
			mu.Unlock()
		}
	})
	done := make(chan error, 1)
	go func() { _, err := runtime.Run(context.Background(), "go"); done <- err }()
	<-tool.started["slow"]
	<-tool.started["fast"]
	close(tool.release["fast"])
	time.Sleep(10 * time.Millisecond)
	close(tool.release["slow"])
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotSettled := append([]string(nil), settled...)
	mu.Unlock()
	if !reflect.DeepEqual(gotSettled, []string{"fast", "slow"}) {
		t.Fatalf("settled order = %v, want completion order", gotSettled)
	}
	messages := transcript.Context().Messages()
	if got := []string{toolResultAt(t, messages, 2).ToolCallID(), toolResultAt(t, messages, 3).ToolCallID()}; !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Fatalf("durable tool result order = %v", got)
	}
}

func TestSteeringFollowUpContinueAndTransformBoundaries(t *testing.T) {
	transcript := newSession(t)
	scripted := newScriptedProvider(t, mustTextTerminal(t, "first"), mustTextTerminal(t, "steered"), mustTextTerminal(t, "followed"), mustTextTerminal(t, "continued"))
	runtime := newAgent(t, transcript, scripted, nil)
	if err := runtime.Steer("steer now"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.FollowUp("follow later"); err != nil {
		t.Fatal(err)
	}
	if steering, followUp := runtime.Queues(); len(steering) != 1 || len(followUp) != 1 {
		t.Fatalf("queue snapshot = %d/%d", len(steering), len(followUp))
	}
	if _, err := runtime.Run(context.Background(), "initial"); err != nil {
		t.Fatal(err)
	}
	if scripted.CallCount() != 3 {
		t.Fatalf("provider calls after queues = %d", scripted.CallCount())
	}
	if err := runtime.Steer("continue"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Continue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if scripted.CallCount() != 4 {
		t.Fatalf("provider calls after continue = %d", scripted.CallCount())
	}
	if steering, followUp := runtime.Queues(); len(steering) != 0 || len(followUp) != 0 {
		t.Fatalf("queues after drain = %d/%d", len(steering), len(followUp))
	}
	if got := messageRoles(transcript.Context().Messages()); !reflect.DeepEqual(got, []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant}) {
		t.Fatalf("transcript roles = %v", got)
	}
}

func TestTransformContextIsProviderOnlyAndFailsBeforeProvider(t *testing.T) {
	t.Run("provider sees replacement snapshot, transcript does not", func(t *testing.T) {
		transcript := newSession(t)
		scripted := newScriptedProvider(t, mustTextTerminal(t, "done"))
		model, err := provider.NewModelRef("scripted", "scripted", "scripted-1")
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := agent.New(agent.Config{Provider: scripted, Transcript: transcript, Model: model, Now: func() time.Time { return agentTestEpoch }, TransformContext: func(_ context.Context, messages []llm.ConversationMessage) ([]llm.ConversationMessage, error) {
			if len(messages) != 1 {
				t.Fatalf("transform input messages = %d", len(messages))
			}
			return nil, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Run(context.Background(), "durable"); err != nil {
			t.Fatal(err)
		}
		if got := scripted.Requests()[0].Messages(); len(got) != 0 {
			t.Fatalf("provider request retained transformed messages: %d", len(got))
		}
		if got := transcript.Context().Messages(); len(got) != 2 || got[0].Role() != llm.RoleUser {
			t.Fatalf("transform changed transcript: %#v", got)
		}
	})
	t.Run("error is explicit and provider is not called", func(t *testing.T) {
		transcript := newSession(t)
		scripted := newScriptedProvider(t, mustTextTerminal(t, "unused"))
		model, err := provider.NewModelRef("scripted", "scripted", "scripted-1")
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := agent.New(agent.Config{Provider: scripted, Transcript: transcript, Model: model, Now: func() time.Time { return agentTestEpoch }, TransformContext: func(context.Context, []llm.ConversationMessage) ([]llm.ConversationMessage, error) {
			return nil, context.DeadlineExceeded
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Run(context.Background(), "blocked"); !errors.Is(err, agent.ErrContextTransform) {
			t.Fatalf("Run error = %v", err)
		}
		if scripted.CallCount() != 0 {
			t.Fatalf("provider called after transform failure: %d", scripted.CallCount())
		}
	})
}

func TestAbortWaitsForParallelWorkersAndCommitsCancelledBatch(t *testing.T) {
	transcript := newSession(t)
	slow, err := llm.NewToolCallBlock("one", "slow", []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	fast, err := llm.NewToolCallBlock("two", "fast", []byte(`{"x":2}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{slow, fast}, mustUsage(t, 3, 2), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "must not run"))
	tool := &namedBatchTool{started: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})}, release: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})}}
	runtime := newAgent(t, transcript, scripted, tool)
	runDone := make(chan error, 1)
	go func() { _, err := runtime.Run(context.Background(), "go"); runDone <- err }()
	<-tool.started["slow"]
	<-tool.started["fast"]
	if err := runtime.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if scripted.CallCount() != 1 {
		t.Fatalf("provider calls after abort = %d", scripted.CallCount())
	}
	messages := transcript.Context().Messages()
	if got := []string{toolResultAt(t, messages, 2).ToolCallID(), toolResultAt(t, messages, 3).ToolCallID()}; !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Fatalf("cancel result order = %v", got)
	}
	if terminal := failureAt(t, messages, 4); terminal.FinishReason() != llm.FinishAborted {
		t.Fatalf("abort terminal = %s", terminal.FinishReason())
	}
	if state := runtime.State(); state.Phase() != agent.PhaseIdle {
		t.Fatalf("state after abort = %s", state.Phase())
	}
}

func TestToolLevelSequentialOverrideDowngradesWholeBatch(t *testing.T) {
	transcript := newSession(t)
	slow, err := llm.NewToolCallBlock("one", "slow", []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	fast, err := llm.NewToolCallBlock("two", "fast", []byte(`{"x":2}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{slow, fast}, mustUsage(t, 3, 2), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "done"))
	tool := &namedBatchTool{mode: agent.ToolExecutionSequential, started: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})}, release: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})}}
	runtime := newAgent(t, transcript, scripted, tool)
	done := make(chan error, 1)
	go func() { _, err := runtime.Run(context.Background(), "go"); done <- err }()
	<-tool.started["slow"]
	select {
	case <-tool.started["fast"]:
		t.Fatal("fast tool started before sequential override predecessor settled")
	case <-time.After(20 * time.Millisecond):
	}
	close(tool.release["slow"])
	<-tool.started["fast"]
	close(tool.release["fast"])
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMixedToolBatchPreservesEverySourceResultAndOnlyAllTerminateStops(t *testing.T) {
	transcript := newSession(t)
	calls := make([]llm.AssistantBlock, 0, 4)
	for _, name := range []string{"ok", "missing", "failure", "terminate"} {
		call, err := llm.NewToolCallBlock(name+"-id", name, []byte(`{"x":1}`))
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	assistant, err := llm.NewAssistantToolUseMessage(calls, mustUsage(t, 3, 2), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "continued"))
	runtime := newAgent(t, transcript, scripted, mixedTool{})
	if _, err := runtime.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if scripted.CallCount() != 2 {
		t.Fatalf("partial terminate stopped batch/provider: %d calls", scripted.CallCount())
	}
	messages := transcript.Context().Messages()
	if got := []string{toolResultAt(t, messages, 2).ToolCallID(), toolResultAt(t, messages, 3).ToolCallID(), toolResultAt(t, messages, 4).ToolCallID(), toolResultAt(t, messages, 5).ToolCallID()}; !reflect.DeepEqual(got, []string{"ok-id", "missing-id", "failure-id", "terminate-id"}) {
		t.Fatalf("mixed source order = %v", got)
	}
	if !toolResultAt(t, messages, 3).IsError() || !toolResultAt(t, messages, 4).IsError() {
		t.Fatalf("missing/failure were not durable errors")
	}
}

func TestTerminatingBatchStillDrainsSteeringThenFollowUp(t *testing.T) {
	transcript := newSession(t)
	first, err := llm.NewToolCallBlock("first", "terminate", []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := llm.NewToolCallBlock("second", "terminate", []byte(`{"x":2}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{first, second}, mustUsage(t, 3, 2), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "steered"), mustTextTerminal(t, "followed"))
	runtime := newAgent(t, transcript, scripted, mixedTool{})
	if err := runtime.Steer("steer after terminate"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.FollowUp("follow after terminate"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	if scripted.CallCount() != 3 {
		t.Fatalf("provider calls = %d, want terminating batch plus steering/follow-up drain", scripted.CallCount())
	}
	if got := messageRoles(transcript.Context().Messages()); !reflect.DeepEqual(got, []llm.Role{
		llm.RoleUser, llm.RoleAssistant, llm.RoleToolResult, llm.RoleToolResult,
		llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant,
	}) {
		t.Fatalf("terminating batch transcript roles = %v", got)
	}
}

func TestSequentialCancellationAssociatesUnstartedCallsAndStopsProvider(t *testing.T) {
	transcript := newSession(t)
	first, err := llm.NewToolCallBlock("first", "slow", []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := llm.NewToolCallBlock("second", "fast", []byte(`{"x":2}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{first, second}, mustUsage(t, 3, 2), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "must not run"))
	tool := &namedBatchTool{
		started: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
		release: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
	}
	model, err := provider.NewModelRef("scripted", "scripted", "scripted-1")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.New(agent.Config{
		Provider:      scripted,
		Transcript:    transcript,
		Model:         model,
		Tool:          tool,
		ToolExecution: agent.ToolExecutionSequential,
		Now:           func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { _, err := runtime.Run(context.Background(), "go"); runDone <- err }()
	waitClosed(t, tool.started["slow"], "first sequential tool start")
	select {
	case <-tool.started["fast"]:
		t.Fatal("second sequential tool started before cancellation")
	default:
	}
	if err := runtime.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if scripted.CallCount() != 1 {
		t.Fatalf("provider calls after sequential cancellation = %d", scripted.CallCount())
	}
	select {
	case <-tool.started["fast"]:
		t.Fatal("second sequential tool started after cancellation")
	default:
	}
	messages := transcript.Context().Messages()
	if got := []string{toolResultAt(t, messages, 2).ToolCallID(), toolResultAt(t, messages, 3).ToolCallID()}; !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("cancelled result associations = %v", got)
	}
	if !toolResultAt(t, messages, 2).IsError() || !toolResultAt(t, messages, 3).IsError() {
		t.Fatalf("cancelled batch results must be durable errors")
	}
	if failure := failureAt(t, messages, 4); failure.FinishReason() != llm.FinishAborted {
		t.Fatalf("terminal finish = %s, want aborted", failure.FinishReason())
	}
}

func TestCancelledSequentialBatchCommitFaultStopsBeforeSuccessor(t *testing.T) {
	base := newSession(t)
	transcript := &selectiveFailingTranscript{
		base: base,
		fail: func(message llm.ConversationMessage) bool {
			result, ok := message.(llm.ToolResultMessage)
			return ok && result.ToolCallID() == "second"
		},
	}
	first, err := llm.NewToolCallBlock("first", "slow", []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := llm.NewToolCallBlock("second", "fast", []byte(`{"x":2}`))
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := llm.NewAssistantToolUseMessage([]llm.AssistantBlock{first, second}, mustUsage(t, 3, 2), agentTestEpoch)
	if err != nil {
		t.Fatal(err)
	}
	scripted := newScriptedProvider(t, assistant, mustTextTerminal(t, "must not run"))
	tool := &namedBatchTool{
		started: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
		release: map[string]chan struct{}{"slow": make(chan struct{}), "fast": make(chan struct{})},
	}
	model, err := provider.NewModelRef("scripted", "scripted", "scripted-1")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.New(agent.Config{
		Provider:      scripted,
		Transcript:    transcript,
		Model:         model,
		Tool:          tool,
		ToolExecution: agent.ToolExecutionSequential,
		Now:           func() time.Time { return agentTestEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { _, err := runtime.Run(context.Background(), "go"); runDone <- err }()
	waitClosed(t, tool.started["slow"], "first sequential tool start")
	if err := runtime.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-runDone; !errors.Is(err, agent.ErrTranscriptCommit) {
		t.Fatalf("Run error = %v, want transcript commit fault", err)
	}
	if scripted.CallCount() != 1 {
		t.Fatalf("provider calls after tool-result fault = %d", scripted.CallCount())
	}
	if got := messageRoles(base.Context().Messages()); !reflect.DeepEqual(got, []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleToolResult}) {
		t.Fatalf("durable messages after second result fault = %v", got)
	}
	if result := toolResultAt(t, base.Context().Messages(), 2); result.ToolCallID() != "first" || !result.IsError() {
		t.Fatalf("first durable cancellation result = id %q error %t", result.ToolCallID(), result.IsError())
	}
}

func TestBatchCompletionClearsPendingToolBeforeNextTurnStarted(t *testing.T) {
	transcript := newSession(t)
	scripted := newScriptedProvider(t,
		mustToolUseTerminal(t, "call", "bash", []byte(`{"command":"one"}`)),
		mustTextTerminal(t, "done"),
	)
	runtime := newAgent(t, transcript, scripted, &fakeTool{name: "bash"})
	var observed bool
	runtime.Subscribe(func(_ context.Context, event agent.Event) {
		if event.Kind != agent.EventTurnStarted || event.Turn != 2 {
			return
		}
		observed = true
		state := runtime.State()
		if state.Phase() != agent.PhaseProvider {
			t.Errorf("phase at next turn start = %s, want provider", state.Phase())
		}
		if pending, ok := state.PendingToolCall(); ok || pending != "" {
			t.Errorf("pending call at next turn start = %q/%t", pending, ok)
		}
		if pending := state.PendingToolCalls(); len(pending) != 0 {
			t.Errorf("pending calls at next turn start = %v", pending)
		}
	})
	if _, err := runtime.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if !observed {
		t.Fatal("did not observe second turn start")
	}
}

func TestContinueBusyAdmissionDoesNotDrainQueuedMessage(t *testing.T) {
	transcript := newSession(t)
	scripted := newScriptedProvider(t,
		mustTextTerminal(t, "first"),
		mustTextTerminal(t, "second"),
		mustTextTerminal(t, "drained"),
	)
	runtime := newAgent(t, transcript, scripted, nil)
	if _, err := runtime.Run(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	release := make(chan struct{})
	blockNextRun := true
	runtime.Subscribe(func(_ context.Context, event agent.Event) {
		if !blockNextRun || event.Kind != agent.EventRunStarted {
			return
		}
		close(blocked)
		<-release
	})
	if err := runtime.Steer("must remain queued"); err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { _, err := runtime.Run(context.Background(), "second"); runDone <- err }()
	waitClosed(t, blocked, "second run admission before prompt commit")
	if _, err := runtime.Continue(context.Background()); !errors.Is(err, agent.ErrBusy) {
		t.Fatalf("Continue while active error = %v, want busy", err)
	}
	if steering, followUp := runtime.Queues(); len(steering) != 1 || len(followUp) != 0 {
		t.Fatalf("failed Continue drained queues: steering=%d follow-up=%d", len(steering), len(followUp))
	}
	close(release)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

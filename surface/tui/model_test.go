package tui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
)

type modelTestAPI struct{ application.API }

func (modelTestAPI) DefaultCWD() string { return "/workspace" }

func newModelForTest(t *testing.T) *Model {
	return newModelWithAPIForTest(t, modelTestAPI{})
}

func newModelWithAPIForTest(t *testing.T, api application.API) *Model {
	t.Helper()
	state := application.State{SessionID: "session-1", CWD: "/workspace", Phase: agent.PhaseIdle}
	model, err := newModel(context.Background(), Options{
		Application: api, SessionID: "session-1", ScreenMode: ScreenFull,
	}, application.SessionSnapshot{
		Revision: 7, SessionID: "session-1", Info: application.SessionInfo{ID: "session-1", CWD: "/workspace"},
		LiveState: &state,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = model.composer.Init()
	return model
}

type dispatchRecordingAPI struct {
	modelTestAPI
	command application.Command
}

func (a *dispatchRecordingAPI) Dispatch(_ context.Context, _ string, command application.Command) (application.CommandResult, error) {
	a.command = command
	return application.PromptStartedResult{OperationID: 1}, nil
}

func TestModelComposerDistinguishesSubmitAndNewline(t *testing.T) {
	model := newModelForTest(t)
	typeKey := func(text string) {
		_, _ = model.Update(tea.KeyPressMsg(tea.Key{Code: []rune(text)[0], Text: text}))
	}
	typeKey("a")
	_, _ = model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter, Mod: tea.ModShift}))
	typeKey("b")
	if value := model.composer.Value(); value != "a\nb" {
		t.Fatalf("composer value = %q", value)
	}
	_, command := model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if command == nil {
		t.Fatal("Enter did not produce a dispatch command")
	}
	if value := model.composer.Value(); value != "" {
		t.Fatalf("composer was not reset after submit: %q", value)
	}
	if len(model.composer.history) != 1 || model.composer.history[0] != "a\nb" {
		t.Fatalf("composer history = %#v", model.composer.history)
	}
}

func TestModelSubmitPreservesComposerImages(t *testing.T) {
	image, err := llm.NewImageDataBlock("image/png", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	api := &dispatchRecordingAPI{}
	model := newModelWithAPIForTest(t, api)
	model.composer.SetDraft("/help", []llm.ImageBlock{image})
	command := model.submit(false)
	if command == nil {
		t.Fatal("rich draft did not produce a dispatch command")
	}
	message, ok := command().(commandFinishedMsg)
	if !ok || message.err != nil {
		t.Fatalf("dispatch message = %#v", message)
	}
	prompt, ok := api.command.(application.PromptCommand)
	if !ok {
		t.Fatalf("rich slash draft dispatched %T, want PromptCommand", api.command)
	}
	if prompt.Message != "/help" || len(prompt.Images) != 1 || string(prompt.Images[0].Data()) != string(image.Data()) {
		t.Fatalf("prompt = %#v", prompt)
	}
	if !model.composer.Empty() {
		t.Fatalf("composer retained submitted draft: %q / %#v", model.composer.Value(), model.composer.Images())
	}
}

func TestModelRendersSafeSmallTerminalFallback(t *testing.T) {
	model := newModelForTest(t)
	for _, size := range []struct{ width, height int }{{12, 3}, {1, 1}} {
		_, _ = model.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
		view := model.View()
		if got := strings.Count(view.Content, "\n") + 1; got != size.height {
			t.Fatalf("%dx%d view has %d rows: %q", size.width, size.height, got, view.Content)
		}
		for _, line := range strings.Split(view.Content, "\n") {
			if got := lipgloss.Width(line); got > size.width {
				t.Fatalf("%dx%d view contains a %d-column row: %q", size.width, size.height, got, line)
			}
		}
		if view.Cursor != nil {
			t.Fatalf("%dx%d fallback exposed an off-screen cursor", size.width, size.height)
		}
		if model.composer.width != size.width {
			t.Fatalf("composer width = %d, want %d", model.composer.width, size.width)
		}
	}
}

func TestModelRendersSafeSmallTerminalFallbackWithAttachment(t *testing.T) {
	image, err := llm.NewImageDataBlock("image/png", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	model := newModelForTest(t)
	model.composer.SetDraft("", []llm.ImageBlock{image})
	_, _ = model.Update(tea.WindowSizeMsg{Width: 40, Height: 5})
	view := model.View()
	if rows := strings.Count(view.Content, "\n") + 1; rows != 5 {
		t.Fatalf("attachment fallback rows = %d, want 5: %q", rows, view.Content)
	}
	if view.Cursor != nil {
		t.Fatal("attachment fallback exposed an off-screen cursor")
	}
}

func TestAutoScreenModeUsesInlineForNonTerminalOutput(t *testing.T) {
	if got := resolveScreenMode(ScreenAuto, &bytes.Buffer{}, []string{"TERM=xterm-256color"}); got != ScreenInline {
		t.Fatalf("buffer auto mode = %q, want inline", got)
	}
	pipeReader, pipeWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pipeReader.Close()
	defer pipeWriter.Close()
	if got := resolveScreenMode(ScreenAuto, pipeWriter, []string{"TERM=xterm-256color"}); got != ScreenInline {
		t.Fatalf("pipe auto mode = %q, want inline", got)
	}
	if got := resolveScreenMode(ScreenFull, &bytes.Buffer{}, []string{"TERM=dumb"}); got != ScreenFull {
		t.Fatalf("explicit full mode changed to %q", got)
	}
}

func TestAutoScreenModeUsesInlineForDumbTerminal(t *testing.T) {
	if got := resolveScreenMode(ScreenAuto, os.Stdout, []string{"TERM=dumb"}); got != ScreenInline {
		t.Fatalf("dumb terminal auto mode = %q, want inline", got)
	}
}

func TestStaleSubscriptionMessagesCannotReplaceCurrentSubscription(t *testing.T) {
	model := newModelForTest(t)
	currentEvents := make(chan application.Event)
	current := &application.EventSubscription{Events: currentEvents}
	model.subscription = current
	model.subscriptionGeneration = 4

	_, _ = model.Update(applicationEventMsg{generation: 3, ok: false})
	if model.subscription != current || model.subscriptionGeneration != 4 {
		t.Fatal("stale closed-stream message invalidated the current subscription")
	}

	staleEvents := make(chan application.Event)
	stale := &application.EventSubscription{Events: staleEvents}
	_, _ = model.Update(subscriptionReadyMsg{subscription: stale, generation: 3})
	if model.subscription != current || model.subscriptionGeneration != 4 {
		t.Fatal("stale ready message replaced the current subscription")
	}
}

func TestStaleProjectionResultsCannotOverwriteCurrentSession(t *testing.T) {
	model := newModelForTest(t)
	model.sessionGeneration = 5
	model.projectionGeneration = 9
	model.state.CWD = "/current"

	_, _ = model.Update(stateLoadedMsg{
		sessionID: "session-1", sessionGeneration: 5, request: 8,
		state: application.State{SessionID: "session-1", CWD: "/stale"}, active: true,
	})
	if model.state.CWD != "/current" {
		t.Fatalf("stale state overwrote current state: %q", model.state.CWD)
	}

	_, _ = model.Update(snapshotLoadedMsg{
		sessionID: "session-1", sessionGeneration: 4, request: 9,
		snapshot: application.SessionSnapshot{
			SessionID: "session-1", Info: application.SessionInfo{ID: "session-1", CWD: "/stale"},
		},
	})
	if model.state.CWD != "/current" || model.revision != 7 {
		t.Fatalf("stale snapshot changed projection: state=%q revision=%d", model.state.CWD, model.revision)
	}
}

func TestNewerStateRequestSupersedesAndRetriesOlderSnapshot(t *testing.T) {
	model := newModelForTest(t)
	snapshotCommand := model.requestSnapshot()
	if snapshotCommand == nil || model.projectionGeneration != 1 || model.snapshotInFlight != 1 {
		t.Fatalf("initial snapshot request state = generation:%d in-flight:%d", model.projectionGeneration, model.snapshotInFlight)
	}
	stateCommand := model.requestState()
	if stateCommand == nil || model.projectionGeneration != 2 {
		t.Fatalf("state request generation = %d, want 2", model.projectionGeneration)
	}

	_, retry := model.Update(snapshotLoadedMsg{
		sessionID: "session-1", sessionGeneration: model.sessionGeneration, request: 1,
		snapshot: application.SessionSnapshot{
			Revision: 1, SessionID: "session-1", Info: application.SessionInfo{ID: "session-1", CWD: "/stale"},
		},
	})
	if retry == nil {
		t.Fatal("superseded required snapshot was not retried")
	}
	if model.state.CWD != "/workspace" || model.revision != 7 {
		t.Fatalf("stale snapshot changed projection: state=%q revision=%d", model.state.CWD, model.revision)
	}
	if model.projectionGeneration != 3 || model.snapshotInFlight != 3 || !model.snapshotNeeded {
		t.Fatalf("retry state = generation:%d in-flight:%d needed:%v",
			model.projectionGeneration, model.snapshotInFlight, model.snapshotNeeded)
	}

	_, _ = model.Update(snapshotLoadedMsg{
		sessionID: "session-1", sessionGeneration: model.sessionGeneration, request: 3,
		snapshot: application.SessionSnapshot{
			Revision: 10, SessionID: "session-1", Info: application.SessionInfo{ID: "session-1", CWD: "/current"},
		},
	})
	if model.revision != 10 || model.snapshotNeeded || model.snapshotInFlight != 0 {
		t.Fatalf("accepted snapshot state = revision:%d in-flight:%d needed:%v",
			model.revision, model.snapshotInFlight, model.snapshotNeeded)
	}
}

func TestFailedRequiredSnapshotRetriesAfterLaterStateRequest(t *testing.T) {
	model := newModelForTest(t)
	_ = model.requestSnapshot()
	_, retryBatch := model.Update(snapshotLoadedMsg{
		sessionID: "session-1", sessionGeneration: model.sessionGeneration, request: 1,
		err: errors.New("temporary snapshot failure"),
	})
	if retryBatch == nil || !model.snapshotNeeded || model.snapshotInFlight != 0 {
		t.Fatalf("failed snapshot state = in-flight:%d needed:%v retry:%v",
			model.snapshotInFlight, model.snapshotNeeded, retryBatch != nil)
	}

	_ = model.requestState()
	if model.projectionGeneration != 2 {
		t.Fatalf("state request generation = %d, want 2", model.projectionGeneration)
	}
	_, retry := model.Update(retrySnapshotMsg{sessionGeneration: model.sessionGeneration})
	if retry == nil || model.projectionGeneration != 3 || model.snapshotInFlight != 3 || !model.snapshotNeeded {
		t.Fatalf("retry state = generation:%d in-flight:%d needed:%v retry:%v",
			model.projectionGeneration, model.snapshotInFlight, model.snapshotNeeded, retry != nil)
	}
}

func TestOutOfOrderCommandCompletionCannotRollProjectionBack(t *testing.T) {
	model := newModelForTest(t)
	model.commandRequest = 2
	newer := commandFinishedMsg{
		sessionID: "session-1", sessionGeneration: model.sessionGeneration, request: 2,
		command: application.GetStateCommand{},
		result:  application.GetStateResult{State: application.State{SessionID: "session-1", CWD: "/newer"}},
	}
	_, _ = model.Update(newer)
	older := newer
	older.request = 1
	older.result = application.GetStateResult{State: application.State{SessionID: "session-1", CWD: "/older"}}
	_, _ = model.Update(older)
	if model.state.CWD != "/newer" {
		t.Fatalf("older completion rolled projection back to %q", model.state.CWD)
	}
}

func TestModelToolLifecycleUsesOneStableVirtualItem(t *testing.T) {
	model := newModelForTest(t)
	start := agent.ToolExecutionStartEvent{
		RunID: 1, Turn: 2, ToolCallID: "call-1", ToolName: "read", Arguments: []byte(`{"path":"a.go"}`),
	}
	model.applyAgentEvent(start)
	item, ok := model.liveItems[liveToolID("call-1")]
	if !ok || len(item.Blocks) != 1 || !item.Live {
		t.Fatalf("start item = %#v", item)
	}
	model.applyAgentEvent(agent.ToolExecutionUpdateEvent{
		RunID: 1, Turn: 2, ToolCallID: "call-1", ToolName: "read", Arguments: start.Arguments,
		PartialResult: agent.ToolUpdate{Text: "partial"},
	})
	item = model.liveItems[liveToolID("call-1")]
	if len(item.Blocks) != 2 || item.Blocks[1].Text != "partial" {
		t.Fatalf("update item = %#v", item)
	}
	model.applyAgentEvent(agent.ToolExecutionEndEvent{
		RunID: 1, Turn: 2, ToolCallID: "call-1", ToolName: "read", Arguments: start.Arguments,
		Result: agent.ToolOutput{Text: "complete"},
	})
	item = model.liveItems[liveToolID("call-1")]
	if item.Live || item.Failed || len(item.Blocks) != 2 || item.Blocks[1].Text != "complete" {
		t.Fatalf("end item = %#v", item)
	}
	if model.transcript.Len() != 1 {
		t.Fatalf("transcript length = %d, want one stable tool item", model.transcript.Len())
	}
}

func TestModelRendersTextWhileThinkingBlockRemainsOpen(t *testing.T) {
	collector := &llm.StreamCollector{}
	provenance := llm.AssistantProvenance{Provider: "fixture", API: "fixture", Model: "fixture"}
	start, err := llm.NewStartEvent(provenance, time.Now().UTC().Truncate(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	thinkingStart, _ := llm.NewThinkingStartEvent(0)
	thinkingDelta, _ := llm.NewThinkingDeltaEvent(0, "still reasoning")
	textStart, _ := llm.NewTextStartEvent(1)
	textDelta, _ := llm.NewTextDeltaEvent(1, "visible before stream end")
	for _, event := range []llm.StreamEvent{start, thinkingStart, thinkingDelta, textStart, textDelta} {
		if err := collector.Accept(event); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := collector.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	partial, err := agentmsg.NewAssistantPartial(agentmsg.AssistantPartialSpec{Snapshot: snapshot, Event: textDelta})
	if err != nil {
		t.Fatal(err)
	}
	model := newModelForTest(t)
	model.applyAgentEvent(agent.MessageUpdateEvent{RunID: 1, Turn: 1, Message: partial})
	view := StripTerminalSequences(model.transcript.View(80, 12, model.renderer))
	if !strings.Contains(view, "still reasoning") || !strings.Contains(view, "visible before stream end") {
		t.Fatalf("overlapping stream view:\n%s", view)
	}
}

func TestModelToolLifecycleJoinsExistingAssistantCall(t *testing.T) {
	model := newModelForTest(t)
	model.transcript.Upsert(contentItem{
		ID: "assistant", Revision: 1, Role: contentRoleAssistant, Title: "Assistant",
		Blocks: []contentBlock{{
			Kind: contentBlockToolCall, ToolCallID: "call-joined", ToolName: "read", Text: `{"path":"a.go"}`,
		}},
	})
	model.applyAgentEvent(agent.ToolExecutionStartEvent{
		RunID: 1, Turn: 2, ToolCallID: "call-joined", ToolName: "read", Arguments: []byte(`{"path":"a.go"}`),
	})
	model.applyAgentEvent(agent.ToolExecutionEndEvent{
		RunID: 1, Turn: 2, ToolCallID: "call-joined", ToolName: "read", Arguments: []byte(`{"path":"a.go"}`),
		Result: agent.ToolOutput{Text: "complete", Details: map[string]any{"kept": true}},
	})
	if model.transcript.Len() != 1 {
		t.Fatalf("transcript length = %d, want one joined execution", model.transcript.Len())
	}
	item := model.transcript.items[0].content
	if len(item.Blocks) != 2 || item.Blocks[0].Kind != contentBlockToolCall || item.Blocks[1].Kind != contentBlockToolResult {
		t.Fatalf("joined item = %#v", item)
	}
	if item.Blocks[1].Text != "complete" || string(item.Blocks[1].ToolDetails) != `{"kept":true}` {
		t.Fatalf("joined result = %#v", item.Blocks[1])
	}
}

func TestModelToolResultMessageReplacesLiveResultWithoutDuplicateItem(t *testing.T) {
	model := newModelForTest(t)
	model.transcript.Upsert(contentItem{
		ID: "live:assistant:1:2", Revision: 1, Role: contentRoleAssistant, Title: "Assistant", Live: true,
		Blocks: []contentBlock{{
			Kind: contentBlockToolCall, ToolCallID: "call-result", ToolName: "read", Text: `{"path":"a.go"}`, Live: true,
		}},
	})
	model.liveItems["live:assistant:1:2"] = model.transcript.items[0].content
	model.upsertToolEnd(
		1, 2, "call-result", "read", []byte(`{"path":"a.go"}`), agent.ToolOutput{Text: "ephemeral"}, false, nil,
	)
	text, err := llm.NewTextBlock("durable")
	if err != nil {
		t.Fatal(err)
	}
	result, err := llm.NewToolResultMessageWithDetails(
		"call-result", "read", []llm.TextBlock{text}, false, time.UnixMilli(3), []byte(`{"source":"durable"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := agentmsg.NewLLM(result)
	if err != nil {
		t.Fatal(err)
	}
	model.upsertAssistant(1, 2, wrapper, false)
	if model.transcript.Len() != 1 {
		t.Fatalf("transcript length = %d, want one execution", model.transcript.Len())
	}
	item := model.transcript.items[0].content
	if len(item.Blocks) != 2 || item.Blocks[1].Text != "durable" || string(item.Blocks[1].ToolDetails) != `{"source":"durable"}` {
		t.Fatalf("merged durable result = %#v", item)
	}
}

func TestModelUsesRoleSpecificLiveMessageIDs(t *testing.T) {
	model := newModelForTest(t)
	now := time.Unix(1700000000, 0)
	userMessage, err := llm.NewUserTextMessage("hello", now)
	if err != nil {
		t.Fatal(err)
	}
	user, err := agentmsg.NewLLM(userMessage)
	if err != nil {
		t.Fatal(err)
	}
	model.upsertAssistant(9, 1, user, true)
	if _, ok := model.liveItems["live:user:9:1"]; !ok {
		t.Fatalf("live items = %#v", model.liveItems)
	}
	if model.liveAssistantID != "" {
		t.Fatalf("user message became assistant anchor %q", model.liveAssistantID)
	}
}

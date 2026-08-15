package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestStateLineUsesRuntimeFactsWithoutStaticShortcutFooter(t *testing.T) {
	model := newModelForTest(t)
	selected, err := provider.NewModel(provider.ModelSpec{
		Provider: "deepseek", API: "openai-completions", ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash",
		ContextWindow: 128_000, MaxTokens: 8_192,
	})
	if err != nil {
		t.Fatal(err)
	}
	percent := 12.5
	model.state.Model, model.state.HasModel = selected, true
	model.state.ThinkingLevel = provider.ThinkingHigh
	model.state.ContextUsage = &agent.ContextUsage{Percent: &percent}
	model.width, model.height = 100, 16
	model.composer.SetWidth(100)

	view := StripTerminalSequences(model.View().Content)
	for _, expected := range []string{"workspace", "session-1", "deepseek-v4-flash", "thinking high", "ctx 12.5%"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("state line omitted %q:\n%s", expected, view)
		}
	}
	for _, removed := range []string{"Enter send", "Ctrl+G help", "PgUp/PgDn scroll"} {
		if strings.Contains(view, removed) {
			t.Fatalf("view retained static shortcut %q:\n%s", removed, view)
		}
	}
	if rows := strings.Count(model.View().Content, "\n") + 1; rows != model.height {
		t.Fatalf("view rows = %d, want %d", rows, model.height)
	}
}

func TestIdleStatusTemporarilyOverridesStateLine(t *testing.T) {
	model := newModelForTest(t)
	model.setStatus("Model updated", statusSuccess)
	if left, _ := model.stateLineLeft(80); !strings.Contains(left, "Model updated") {
		t.Fatalf("success status is not visible: %q", left)
	}
	if !model.statusExpiryPending {
		t.Fatal("idle success did not schedule expiry")
	}
	_, _ = model.Update(statusExpiryMsg{generation: model.statusGeneration})
	if left, _ := model.stateLineLeft(80); strings.Contains(left, "Model updated") || !strings.Contains(left, "workspace") {
		t.Fatalf("state line did not return to runtime facts: %q", left)
	}
}

func TestQueueDockShowsMessagesOnlyWhileQueued(t *testing.T) {
	model := newModelForTest(t)
	model.state.PendingMessageCount = 4
	model.state.QueuedMessages = agent.QueueState{
		Steering: []string{"change direction", "inspect tests"},
		FollowUp: []string{"summarize", "finish"},
	}
	lines := model.renderQueueDock(80, 3)
	if len(lines) != 3 {
		t.Fatalf("queue rows = %d, want 3: %#v", len(lines), lines)
	}
	view := StripTerminalSequences(strings.Join(lines, "\n"))
	if !strings.Contains(view, "steer: change direction") || !strings.Contains(view, "2 more queued") {
		t.Fatalf("queue dock:\n%s", view)
	}
	model.state.PendingMessageCount = 0
	if lines := model.renderQueueDock(80, 3); len(lines) != 0 {
		t.Fatalf("empty queue dock = %#v", lines)
	}
}

func TestRestoreQueuedMessagesReturnsTextToComposerAndClearsProjection(t *testing.T) {
	model := newModelForTest(t)
	model.composer.SetDraft("current draft", nil)
	model.commandRequest = 1
	model.restoreQueueRequest = 1
	model.state.PendingMessageCount = 2
	model.state.QueuedMessages = agent.QueueState{Steering: []string{"old"}}
	model.handleCommandFinished(commandFinishedMsg{
		sessionID: "session-1", sessionGeneration: model.sessionGeneration, request: 1,
		command: application.ClearQueueCommand{},
		result: application.ClearQueueResult{Queue: agent.QueueState{
			Steering: []string{"change direction"}, FollowUp: []string{"afterwards"},
		}},
	})
	if value := model.composer.Value(); value != "change direction\n\nafterwards\n\ncurrent draft" {
		t.Fatalf("restored composer = %q", value)
	}
	if model.state.PendingMessageCount != 0 || len(model.state.QueuedMessages.Steering) != 0 || model.restoreQueueRequest != 0 {
		t.Fatalf("queue projection = %#v, request = %d", model.state.QueuedMessages, model.restoreQueueRequest)
	}
	if model.status.text != "Restored 2 queued messages" {
		t.Fatalf("restore status = %q", model.status.text)
	}
}

func TestRestoreQueuedMessagesPreservesRichImages(t *testing.T) {
	image, err := llm.NewImageDataBlock("image/png", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	text, err := llm.NewTextBlock("look at this")
	if err != nil {
		t.Fatal(err)
	}
	message, err := llm.NewUserContentMessage([]llm.UserContentBlock{text, image}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	model := newModelForTest(t)
	model.commandRequest = 1
	model.restoreQueueRequest = 1
	model.state.PendingMessageCount = 1
	model.handleCommandFinished(commandFinishedMsg{
		sessionID: "session-1", sessionGeneration: model.sessionGeneration, request: 1,
		command: application.ClearQueueCommand{},
		result: application.ClearQueueResult{Queue: agent.QueueState{
			FollowUp: []string{"look at this"}, FollowUpMessages: []llm.ConversationMessage{message},
		}},
	})
	if value := model.composer.Value(); value != "look at this" {
		t.Fatalf("restored composer = %q", value)
	}
	images := model.composer.Images()
	if len(images) != 1 || string(images[0].Data()) != string(image.Data()) {
		t.Fatalf("restored images = %#v", images)
	}
	if !strings.Contains(StripTerminalSequences(model.composer.View()), "1 image") {
		t.Fatalf("composer does not surface attachment count: %q", model.composer.View())
	}
}

func TestRestoreQueuedMessagesDoesNotDispatchForEmptyQueue(t *testing.T) {
	model := newModelForTest(t)
	if command := model.restoreQueuedMessages(); command != nil {
		t.Fatal("empty queue produced a command")
	}
	if model.status.level != statusWarning || model.status.text != "No queued messages to restore" {
		t.Fatalf("empty queue status = %#v", model.status)
	}
}

func TestRestoreQueuedMessagesAppliesAfterNewerUnrelatedCompletion(t *testing.T) {
	model := newModelForTest(t)
	model.commandApplied = 2
	model.restoreQueueRequest = 1
	model.state.PendingMessageCount = 1
	model.handleCommandFinished(commandFinishedMsg{
		sessionID: "session-1", sessionGeneration: model.sessionGeneration, request: 1,
		command: application.ClearQueueCommand{},
		result:  application.ClearQueueResult{Queue: agent.QueueState{Steering: []string{"restore me"}}},
	})
	if model.composer.Value() != "restore me" || model.restoreQueueRequest != 0 {
		t.Fatalf("stale restore result = %q, request %d", model.composer.Value(), model.restoreQueueRequest)
	}
}

func TestRestoreQueuedMessagesRejectsDuplicateRequest(t *testing.T) {
	model := newModelForTest(t)
	model.restoreQueueRequest = 3
	model.state.PendingMessageCount = 1
	if command := model.restoreQueuedMessages(); command != nil {
		t.Fatal("duplicate restore produced a command")
	}
	if model.status.text != "Queue restore is already in progress" {
		t.Fatalf("duplicate restore status = %q", model.status.text)
	}
}

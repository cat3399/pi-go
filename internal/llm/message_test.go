package llm_test

import (
	"errors"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

func testAssistantProvenance() llm.AssistantProvenance {
	return llm.AssistantProvenance{Provider: "fixture", API: "fixture", Model: "fixture"}
}

func newAssistantTextMessage(content []llm.TextBlock, finish llm.FinishReason, usage llm.Usage, timestamp time.Time) (llm.AssistantTextMessage, error) {
	return llm.NewAssistantTextMessage(content, finish, usage, timestamp, testAssistantProvenance())
}

func newAssistantToolUseMessage(content []llm.AssistantBlock, usage llm.Usage, timestamp time.Time) (llm.AssistantToolUseMessage, error) {
	return llm.NewAssistantToolUseMessage(content, usage, timestamp, testAssistantProvenance())
}

func newAssistantRichMessage(content []llm.AssistantBlock, finish llm.FinishReason, usage llm.Usage, timestamp time.Time) (llm.AssistantRichMessage, error) {
	return llm.NewAssistantRichMessage(content, finish, usage, timestamp, testAssistantProvenance())
}

func newAssistantFailureMessage(content []llm.TextBlock, finish llm.FinishReason, message string, usage llm.Usage, timestamp time.Time) (llm.AssistantFailureMessage, error) {
	return llm.NewAssistantFailureMessage(content, finish, message, usage, timestamp, testAssistantProvenance())
}

func newAssistantFailureMessageWithFailure(content []llm.TextBlock, finish llm.FinishReason, failure llm.Failure, usage llm.Usage, timestamp time.Time) (llm.AssistantFailureMessage, error) {
	return llm.NewAssistantFailureMessageWithFailure(content, finish, failure, usage, timestamp, testAssistantProvenance())
}

func newErrorEventWithFailure(reason llm.FinishReason, failure llm.Failure, usage llm.Usage, timestamp time.Time) (llm.ErrorEvent, error) {
	return llm.NewErrorEventWithFailure(reason, failure, usage, timestamp, testAssistantProvenance())
}

func newStartEvent(t *testing.T) llm.StartEvent {
	t.Helper()
	event, err := llm.NewStartEvent(testAssistantProvenance(), time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func TestNewUserTextMessage(t *testing.T) {
	t.Parallel()

	timestamp := time.Date(2026, time.August, 1, 12, 30, 0, 0, time.FixedZone("CST", 8*60*60))
	tests := []struct {
		name string
		text string
	}{
		{name: "plain", text: "hello"},
		{name: "empty", text: ""},
		{name: "unicode", text: "你好，pi"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			message, err := llm.NewUserTextMessage(tt.text, timestamp)
			if err != nil {
				t.Fatalf("NewUserTextMessage() error = %v", err)
			}
			if got := message.Role(); got != llm.RoleUser {
				t.Fatalf("Role() = %v, want %v", got, llm.RoleUser)
			}
			if got := message.Timestamp(); !got.Equal(timestamp) {
				t.Fatalf("Timestamp() = %v, want %v", got, timestamp)
			}

			content := message.Content()
			if len(content) != 1 {
				t.Fatalf("len(Content()) = %d, want 1", len(content))
			}
			if got := content[0].Text(); got != tt.text {
				t.Fatalf("Content()[0].Text() = %q, want %q", got, tt.text)
			}
		})
	}
}

func TestNewUserTextBlocksMessageOwnsContent(t *testing.T) {
	t.Parallel()

	first, err := llm.NewTextBlock("first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := llm.NewTextBlock("second")
	if err != nil {
		t.Fatal(err)
	}
	input := []llm.TextBlock{first, second}
	message, err := llm.NewUserTextBlocksMessage(input, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	input[0] = second
	if got := message.Content(); len(got) != 2 || got[0].Text() != "first" || got[1].Text() != "second" {
		t.Fatalf("Content() = %#v, want first/second", got)
	}
	snapshot := message.Content()
	snapshot[0] = second
	if got := message.Content()[0].Text(); got != "first" {
		t.Fatalf("content mutated through snapshot: %q", got)
	}
}

func TestNewUserTextMessageRejectsInvalidUTF8(t *testing.T) {
	t.Parallel()

	_, err := llm.NewUserTextMessage(string([]byte{0xff}), time.Time{})
	if !errors.Is(err, llm.ErrInvalidText) {
		t.Fatalf("NewUserTextMessage() error = %v, want ErrInvalidText", err)
	}
}

func TestMessageContentReturnsSnapshot(t *testing.T) {
	t.Parallel()

	user, err := llm.NewUserTextMessage("original", time.Time{})
	if err != nil {
		t.Fatalf("NewUserTextMessage() error = %v", err)
	}
	userContent := user.Content()
	replacement, err := llm.NewTextBlock("replacement")
	if err != nil {
		t.Fatalf("NewTextBlock() error = %v", err)
	}
	userContent[0] = replacement
	if got := user.Content()[0].Text(); got != "original" {
		t.Fatalf("user content mutated through snapshot: got %q", got)
	}

	assistant, err := newAssistantTextMessage(
		[]llm.TextBlock{mustTextBlock(t, "first"), mustTextBlock(t, "second")},
		llm.FinishStop,
		llm.Usage{},
		time.Time{},
	)
	if err != nil {
		t.Fatalf("NewAssistantTextMessage() error = %v", err)
	}
	assistantContent := assistant.Content()
	assistantContent[0] = replacement
	if got := assistant.Content()[0].Text(); got != "first" {
		t.Fatalf("assistant content mutated through snapshot: got %q", got)
	}
}

func TestNewAssistantTextMessageFinishReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		finish  llm.FinishReason
		wantErr bool
	}{
		{name: "stop", finish: llm.FinishStop},
		{name: "length", finish: llm.FinishLength},
		{name: "pending", finish: llm.FinishPending, wantErr: true},
		{name: "tool use", finish: llm.FinishToolUse, wantErr: true},
		{name: "error", finish: llm.FinishError, wantErr: true},
		{name: "aborted", finish: llm.FinishAborted, wantErr: true},
		{name: "unknown", finish: llm.FinishReason(255), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			message, err := newAssistantTextMessage(nil, tt.finish, llm.Usage{}, time.Time{})
			if tt.wantErr {
				if !errors.Is(err, llm.ErrInvalidFinishReason) {
					t.Fatalf("error = %v, want ErrInvalidFinishReason", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewAssistantTextMessage() error = %v", err)
			}
			if got := message.FinishReason(); got != tt.finish {
				t.Fatalf("FinishReason() = %v, want %v", got, tt.finish)
			}
			if got := message.Role(); got != llm.RoleAssistant {
				t.Fatalf("Role() = %v, want %v", got, llm.RoleAssistant)
			}
		})
	}
}

func TestNewAssistantToolUseMessageFinishReason(t *testing.T) {
	t.Parallel()
	call := mustToolCall(t, "call-1", "echo", []byte(`{}`))
	tests := []struct {
		name    string
		finish  llm.FinishReason
		wantErr bool
	}{
		{name: "tool use", finish: llm.FinishToolUse},
		{name: "stop", finish: llm.FinishStop},
		{name: "length", finish: llm.FinishLength},
		{name: "pending", finish: llm.FinishPending, wantErr: true},
		{name: "error", finish: llm.FinishError, wantErr: true},
		{name: "aborted", finish: llm.FinishAborted, wantErr: true},
		{name: "unknown", finish: llm.FinishReason(255), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			message, err := llm.NewAssistantToolUseMessageWithFinishAndMetadata(
				[]llm.AssistantBlock{call}, test.finish, llm.Usage{}, time.UnixMilli(1), testAssistantProvenance(), nil, nil,
			)
			if test.wantErr {
				if !errors.Is(err, llm.ErrInvalidFinishReason) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil || message.FinishReason() != test.finish {
				t.Fatalf("message=%#v error=%v", message, err)
			}
		})
	}
}

func mustTextBlock(t *testing.T, text string) llm.TextBlock {
	t.Helper()

	block, err := llm.NewTextBlock(text)
	if err != nil {
		t.Fatalf("NewTextBlock(%q) error = %v", text, err)
	}
	return block
}

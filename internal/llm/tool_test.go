package llm_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

func TestNewToolCallBlockPreservesRawArguments(t *testing.T) {
	t.Parallel()

	arguments := []byte(" {\n  \"text\": \"hello\", \"count\": 2\n} ")
	want := bytes.Clone(arguments)
	call, err := llm.NewToolCallBlock("call-1", "echo", arguments)
	if err != nil {
		t.Fatalf("NewToolCallBlock() error = %v", err)
	}

	arguments[0] = '!'
	if got := call.ArgumentsJSON(); !bytes.Equal(got, want) {
		t.Fatalf("ArgumentsJSON() = %q, want %q", got, want)
	}

	snapshot := call.ArgumentsJSON()
	snapshot[0] = '!'
	if got := call.ArgumentsJSON(); !bytes.Equal(got, want) {
		t.Fatalf("arguments mutated through snapshot: got %q, want %q", got, want)
	}
	if got := call.ID(); got != "call-1" {
		t.Fatalf("ID() = %q, want call-1", got)
	}
	if got := call.Name(); got != "echo" {
		t.Fatalf("Name() = %q, want echo", got)
	}
}

func TestNewToolCallBlockRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		id        string
		toolName  string
		arguments []byte
	}{
		{name: "empty id", id: "", toolName: "echo", arguments: []byte("{}")},
		{name: "blank id", id: " \t", toolName: "echo", arguments: []byte("{}")},
		{name: "empty name", id: "call", toolName: "", arguments: []byte("{}")},
		{name: "invalid name utf8", id: "call", toolName: string([]byte{0xff}), arguments: []byte("{}")},
		{name: "malformed json", id: "call", toolName: "echo", arguments: []byte("{")},
		{name: "null", id: "call", toolName: "echo", arguments: []byte("null")},
		{name: "array", id: "call", toolName: "echo", arguments: []byte("[]")},
		{name: "string", id: "call", toolName: "echo", arguments: []byte(`"value"`)},
		{name: "trailing value", id: "call", toolName: "echo", arguments: []byte("{} {}")},
		{name: "invalid json utf8", id: "call", toolName: "echo", arguments: []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := llm.NewToolCallBlock(tt.id, tt.toolName, tt.arguments)
			if !errors.Is(err, llm.ErrInvalidToolCall) {
				t.Fatalf("NewToolCallBlock() error = %v, want ErrInvalidToolCall", err)
			}
		})
	}
}

func TestNewAssistantToolUseMessage(t *testing.T) {
	t.Parallel()

	call, err := llm.NewToolCallBlock("call-1", "echo", []byte(`{"text":"hello"}`))
	if err != nil {
		t.Fatalf("NewToolCallBlock() error = %v", err)
	}
	timestamp := time.Date(2026, time.August, 1, 1, 2, 3, 0, time.UTC)
	message, err := llm.NewAssistantToolUseMessage(
		[]llm.AssistantBlock{mustTextBlock(t, "running"), call},
		llm.Usage{},
		timestamp,
	)
	if err != nil {
		t.Fatalf("NewAssistantToolUseMessage() error = %v", err)
	}

	if got := message.FinishReason(); got != llm.FinishToolUse {
		t.Fatalf("FinishReason() = %v, want %v", got, llm.FinishToolUse)
	}
	if got := message.Role(); got != llm.RoleAssistant {
		t.Fatalf("Role() = %v, want %v", got, llm.RoleAssistant)
	}
	if got := message.Timestamp(); !got.Equal(timestamp) {
		t.Fatalf("Timestamp() = %v, want %v", got, timestamp)
	}

	content := message.Content()
	if len(content) != 2 {
		t.Fatalf("len(Content()) = %d, want 2", len(content))
	}
	if content[0].Kind() != llm.AssistantBlockText || content[1].Kind() != llm.AssistantBlockToolCall {
		t.Fatalf("content kinds = (%v, %v), want (text, tool call)", content[0].Kind(), content[1].Kind())
	}
	content[0] = call
	if got := message.Content()[0].Kind(); got != llm.AssistantBlockText {
		t.Fatalf("content mutated through snapshot: kind = %v", got)
	}
}

func TestNewAssistantToolUseMessageRejectsMissingOrDuplicateCall(t *testing.T) {
	t.Parallel()

	call, err := llm.NewToolCallBlock("call-1", "echo", []byte("{}"))
	if err != nil {
		t.Fatalf("NewToolCallBlock() error = %v", err)
	}

	var typedNil *llm.TextBlock
	tests := []struct {
		name    string
		content []llm.AssistantBlock
	}{
		{name: "no call", content: []llm.AssistantBlock{mustTextBlock(t, "text")}},
		{name: "duplicate id", content: []llm.AssistantBlock{call, call}},
		{name: "nil block", content: []llm.AssistantBlock{call, nil}},
		{name: "typed nil block", content: []llm.AssistantBlock{call, typedNil}},
		{name: "zero tool call", content: []llm.AssistantBlock{llm.ToolCallBlock{}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := llm.NewAssistantToolUseMessage(tt.content, llm.Usage{}, time.Time{})
			if !errors.Is(err, llm.ErrInvalidToolCall) {
				t.Fatalf("NewAssistantToolUseMessage() error = %v, want ErrInvalidToolCall", err)
			}
		})
	}
}

func TestValidateToolResultAssociationRejectsZeroValues(t *testing.T) {
	t.Parallel()

	if err := llm.ValidateToolResultAssociation(llm.ToolCallBlock{}, llm.ToolResultMessage{}); !errors.Is(err, llm.ErrInvalidToolCall) {
		t.Fatalf("ValidateToolResultAssociation(zero, zero) error = %v, want ErrInvalidToolCall", err)
	}

	call, err := llm.NewToolCallBlock("call", "echo", []byte("{}"))
	if err != nil {
		t.Fatalf("NewToolCallBlock() error = %v", err)
	}
	if err := llm.ValidateToolResultAssociation(call, llm.ToolResultMessage{}); !errors.Is(err, llm.ErrInvalidToolResult) {
		t.Fatalf("ValidateToolResultAssociation(valid, zero) error = %v, want ErrInvalidToolResult", err)
	}
}

func TestToolResultAssociation(t *testing.T) {
	t.Parallel()

	call, err := llm.NewToolCallBlock("call-1", "echo", []byte("{}"))
	if err != nil {
		t.Fatalf("NewToolCallBlock() error = %v", err)
	}
	result, err := llm.NewToolResultMessage(
		"call-1",
		"echo",
		[]llm.TextBlock{mustTextBlock(t, "hello")},
		false,
		time.Time{},
	)
	if err != nil {
		t.Fatalf("NewToolResultMessage() error = %v", err)
	}
	if err := llm.ValidateToolResultAssociation(call, result); err != nil {
		t.Fatalf("ValidateToolResultAssociation() error = %v", err)
	}
	if got := result.Role(); got != llm.RoleToolResult {
		t.Fatalf("Role() = %v, want %v", got, llm.RoleToolResult)
	}
	if result.IsError() {
		t.Fatal("IsError() = true, want false")
	}

	wrongID, err := llm.NewToolResultMessage("call-2", "echo", nil, true, time.Time{})
	if err != nil {
		t.Fatalf("NewToolResultMessage(wrong id) error = %v", err)
	}
	if err := llm.ValidateToolResultAssociation(call, wrongID); !errors.Is(err, llm.ErrToolResultMismatch) {
		t.Fatalf("association error = %v, want ErrToolResultMismatch", err)
	}

	wrongName, err := llm.NewToolResultMessage("call-1", "other", nil, true, time.Time{})
	if err != nil {
		t.Fatalf("NewToolResultMessage(wrong name) error = %v", err)
	}
	if err := llm.ValidateToolResultAssociation(call, wrongName); !errors.Is(err, llm.ErrToolResultMismatch) {
		t.Fatalf("association error = %v, want ErrToolResultMismatch", err)
	}
}

func TestNewToolResultMessageRejectsInvalidIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		toolCallID string
		toolName   string
	}{
		{name: "empty call id", toolCallID: "", toolName: "echo"},
		{name: "blank call id", toolCallID: " ", toolName: "echo"},
		{name: "empty tool name", toolCallID: "call", toolName: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := llm.NewToolResultMessage(tt.toolCallID, tt.toolName, nil, false, time.Time{})
			if !errors.Is(err, llm.ErrInvalidToolResult) {
				t.Fatalf("NewToolResultMessage() error = %v, want ErrInvalidToolResult", err)
			}
		})
	}
}

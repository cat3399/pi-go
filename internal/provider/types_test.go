package provider_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestRequestValidatesAndCopiesConversation(t *testing.T) {
	t.Parallel()

	model := mustModel(t)
	user := mustUser(t, "hello")
	assistant := mustToolTerminal(t, "call-1", "echo", []byte(`{"value":"prior"}`))
	result := mustToolResult(t, "call-1", "echo", "done")
	messages := []llm.ConversationMessage{user, assistant, result}

	request, err := provider.NewRequest(model, "system", messages)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	messages[0] = mustUser(t, "changed")
	assertRoles(t, request.Messages(), llm.RoleUser, llm.RoleAssistant, llm.RoleToolResult)

	returned := request.Messages()
	returned[0] = mustUser(t, "changed again")
	if got := request.Messages()[0].(llm.UserTextMessage).Content()[0].Text(); got != "hello" {
		t.Fatalf("request message changed through returned slice: %q", got)
	}
	if request.Model() != model || request.SystemPrompt() != "system" {
		t.Fatalf("request identity = (%v, %q), want (%v, system)", request.Model(), request.SystemPrompt(), model)
	}
}

func TestRequestValidatesToolResultCausality(t *testing.T) {
	t.Parallel()

	model := mustModel(t)
	firstCall := mustToolCallBlock(t, "call-1", "echo")
	secondCall := mustToolCallBlock(t, "call-2", "read")
	multiple := mustToolUseMessage(t, firstCall, secondCall)
	firstResult := mustToolResult(t, firstCall.ID(), firstCall.Name(), "one")
	secondResult := mustToolResult(t, secondCall.ID(), secondCall.Name(), "two")

	if _, err := provider.NewRequest(model, "", []llm.ConversationMessage{
		mustUser(t, "run both"), multiple, firstResult, secondResult, mustTextTerminal(t, "done"),
	}); err != nil {
		t.Fatalf("ordered multiple calls/results rejected: %v", err)
	}

	failure, err := llm.NewAssistantFailureMessage(nil, llm.FinishError, "failed", llm.Usage{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	wrongID := mustToolResult(t, "call-other", firstCall.Name(), "wrong")
	wrongName := mustToolResult(t, firstCall.ID(), "other", "wrong")
	tests := []struct {
		name     string
		messages []llm.ConversationMessage
		contains string
	}{
		{name: "orphan", messages: []llm.ConversationMessage{firstResult}, contains: "orphan tool result"},
		{name: "out of order", messages: []llm.ConversationMessage{multiple, secondResult, firstResult}, contains: "out-of-order tool result"},
		{name: "id mismatch", messages: []llm.ConversationMessage{mustToolUseMessage(t, firstCall), wrongID}, contains: "does not match tool call"},
		{name: "name mismatch", messages: []llm.ConversationMessage{mustToolUseMessage(t, firstCall), wrongName}, contains: "does not match tool call"},
		{name: "duplicate result", messages: []llm.ConversationMessage{mustToolUseMessage(t, firstCall), firstResult, firstResult}, contains: "duplicate tool result"},
		{name: "missing result", messages: []llm.ConversationMessage{mustToolUseMessage(t, firstCall)}, contains: "ended before result"},
		{name: "interrupted results", messages: []llm.ConversationMessage{multiple, firstResult, mustUser(t, "continue")}, contains: "arrived before result"},
		{name: "failed assistant creates no call", messages: []llm.ConversationMessage{failure, firstResult}, contains: "orphan tool result"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := provider.NewRequest(model, "", test.messages)
			if !errors.Is(err, provider.ErrInvalidRequest) || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("NewRequest() error = %v, want ErrInvalidRequest containing %q", err, test.contains)
			}
		})
	}
}

func TestRequestRejectsInvalidBoundaryValues(t *testing.T) {
	t.Parallel()

	model := mustModel(t)
	var textPointer *llm.AssistantTextMessage
	tests := []struct {
		name     string
		model    provider.ModelRef
		system   string
		messages []llm.ConversationMessage
	}{
		{name: "zero model", system: "system"},
		{name: "invalid system UTF-8", model: model, system: string([]byte{0xff})},
		{name: "zero assistant", model: model, messages: []llm.ConversationMessage{llm.AssistantTextMessage{}}},
		{name: "assistant pointer", model: model, messages: []llm.ConversationMessage{textPointer}},
		{name: "zero tool result", model: model, messages: []llm.ConversationMessage{llm.ToolResultMessage{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := provider.NewRequest(tt.model, tt.system, tt.messages); !errors.Is(err, provider.ErrInvalidRequest) {
				t.Fatalf("NewRequest() error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestModelRefRejectsBlankAndInvalidUTF8(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		api      string
		id       string
	}{
		{provider: "", api: "responses", id: "model"},
		{provider: "openai", api: " ", id: "model"},
		{provider: "openai", api: "responses", id: string([]byte{0xff})},
	}
	for _, tt := range tests {
		if _, err := provider.NewModelRef(tt.provider, tt.api, tt.id); !errors.Is(err, provider.ErrInvalidModel) {
			t.Fatalf("NewModelRef(%q, %q, %q) error = %v, want ErrInvalidModel", tt.provider, tt.api, tt.id, err)
		}
	}
}

func TestProviderFailurePreservesCategoryCauseAndOptionalMetadata(t *testing.T) {
	t.Parallel()

	cause := errors.New("rate limited")
	status := 429
	failure, err := provider.NewProviderFailure(provider.ProviderFailureSpec{
		Kind:       provider.FailureInvalidResponse,
		Message:    "provider rejected request",
		Cause:      cause,
		HTTPStatus: &status,
		VendorCode: "rate_limit",
	})
	if err != nil {
		t.Fatalf("NewProviderFailure() error = %v", err)
	}
	if failure.Kind() != provider.FailureInvalidResponse || !errors.Is(failure, cause) {
		t.Fatalf("failure kind/cause = %v/%v", failure.Kind(), failure.Cause())
	}
	if got, ok := failure.HTTPStatus(); !ok || got != status {
		t.Fatalf("HTTPStatus() = (%d, %t), want (%d, true)", got, ok, status)
	}
	if got, ok := failure.VendorCode(); !ok || got != "rate_limit" {
		t.Fatalf("VendorCode() = (%q, %t), want (rate_limit, true)", got, ok)
	}

	invalidStatus := 99
	invalid := []provider.ProviderFailureSpec{
		{Message: "missing kind", Cause: cause},
		{Kind: provider.FailureFactory, Message: "missing cause"},
		{Kind: provider.FailureFactory, Message: " ", Cause: cause},
		{Kind: provider.FailureFactory, Message: "bad status", Cause: cause, HTTPStatus: &invalidStatus},
		{Kind: provider.FailureFactory, Message: "bad code", Cause: cause, VendorCode: " "},
	}
	for _, spec := range invalid {
		if _, err := provider.NewProviderFailure(spec); !errors.Is(err, provider.ErrInvalidProviderFailure) {
			t.Fatalf("NewProviderFailure(%+v) error = %v, want ErrInvalidProviderFailure", spec, err)
		}
	}
}

func mustModel(t *testing.T) provider.ModelRef {
	t.Helper()
	model, err := provider.NewModelRef("scripted", "scripted", "scripted-1")
	if err != nil {
		t.Fatalf("NewModelRef() error = %v", err)
	}
	return model
}

func mustRequest(t *testing.T, text string) provider.Request {
	t.Helper()
	request, err := provider.NewRequest(
		mustModel(t),
		"system",
		[]llm.ConversationMessage{mustUser(t, text)},
	)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	return request
}

func mustUser(t *testing.T, text string) llm.UserTextMessage {
	t.Helper()
	message, err := llm.NewUserTextMessage(text, time.Time{})
	if err != nil {
		t.Fatalf("NewUserTextMessage() error = %v", err)
	}
	return message
}

func mustToolResult(t *testing.T, id, name, text string) llm.ToolResultMessage {
	t.Helper()
	message, err := llm.NewToolResultMessage(
		id,
		name,
		[]llm.TextBlock{mustTextBlock(t, text)},
		false,
		time.Time{},
	)
	if err != nil {
		t.Fatalf("NewToolResultMessage() error = %v", err)
	}
	return message
}

func mustToolCallBlock(t *testing.T, id, name string) llm.ToolCallBlock {
	t.Helper()
	call, err := llm.NewToolCallBlock(id, name, []byte(`{}`))
	if err != nil {
		t.Fatalf("NewToolCallBlock() error = %v", err)
	}
	return call
}

func mustToolUseMessage(t *testing.T, calls ...llm.ToolCallBlock) llm.AssistantToolUseMessage {
	t.Helper()
	blocks := make([]llm.AssistantBlock, len(calls))
	for index, call := range calls {
		blocks[index] = call
	}
	message, err := llm.NewAssistantToolUseMessage(blocks, llm.Usage{}, time.Time{})
	if err != nil {
		t.Fatalf("NewAssistantToolUseMessage() error = %v", err)
	}
	return message
}

func assertRoles(t *testing.T, messages []llm.ConversationMessage, want ...llm.Role) {
	t.Helper()
	if len(messages) != len(want) {
		t.Fatalf("len(messages) = %d, want %d", len(messages), len(want))
	}
	for index := range want {
		if messages[index].Role() != want[index] {
			t.Fatalf("messages[%d].Role() = %v, want %v", index, messages[index].Role(), want[index])
		}
	}
}

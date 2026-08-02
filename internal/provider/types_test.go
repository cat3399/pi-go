package provider_test

import (
	"errors"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func TestRequestValidatesAndCopiesConversation(t *testing.T) {
	t.Parallel()

	model := mustModel(t)
	user := mustUser(t, "hello")
	assistant := mustTextTerminal(t, "prior")
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

package provider_test

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/provider"
)

func newTestModel(providerID, api, id string) (provider.Model, error) {
	return newModel(provider.ModelSpec{
		Provider:      providerID,
		API:           api,
		ID:            id,
		Name:          id,
		BaseURL:       "",
		Input:         []provider.InputKind{provider.InputText},
		Cost:          provider.CostRates{},
		ContextWindow: 200_000,
		MaxTokens:     8_192,
	})
}

func newModel(spec provider.ModelSpec) (provider.Model, error) {
	if spec.Name == "" {
		spec.Name = spec.ID
	}
	if len(spec.Input) == 0 {
		spec.Input = []provider.InputKind{provider.InputText}
	}
	if spec.ContextWindow == 0 {
		spec.ContextWindow = 200_000
	}
	if spec.MaxTokens == 0 {
		spec.MaxTokens = 8_192
	}
	return provider.NewModel(spec)
}

func uint64Pointer(value uint64) *uint64 { return &value }
func uint32Pointer(value uint32) *uint32 { return &value }

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

func TestThinkingLevelMatchesPiSelection(t *testing.T) {
	medium := "medium"
	model, err := newModel(provider.ModelSpec{Provider: "test", API: "test", ID: "reasoner", Reasoning: true, ThinkingLevelMap: map[provider.ThinkingLevel]*string{provider.ThinkingOff: nil, provider.ThinkingLow: nil, provider.ThinkingHigh: nil, provider.ThinkingMedium: &medium}})
	if err != nil {
		t.Fatal(err)
	}
	if got := model.ClampThinkingLevel(provider.ThinkingOff); got != provider.ThinkingMinimal {
		t.Fatalf("off:null clamp = %q, want minimal", got)
	}
	if got := model.ClampThinkingLevel(provider.ThinkingLow); got != provider.ThinkingMedium {
		t.Fatalf("low clamp = %q, want medium", got)
	}
	if got := model.ClampThinkingLevel(provider.ThinkingXHigh); got != provider.ThinkingMedium {
		t.Fatalf("unmapped xhigh clamp = %q, want medium", got)
	}
	if got := model.ClampThinkingLevel(provider.ThinkingMax); got != provider.ThinkingMedium {
		t.Fatalf("unmapped max clamp = %q, want medium", got)
	}
	if effort, ok := model.ThinkingEffort(provider.ThinkingOff); !ok || effort != "minimal" {
		t.Fatalf("off:null effort = %q/%v, want minimal/true", effort, ok)
	}
}

func TestModelEqualUsesProviderAndIDNotMetadataPointer(t *testing.T) {
	first, err := newModel(provider.ModelSpec{Provider: "same", API: "one", ID: "model", Name: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := newModel(provider.ModelSpec{Provider: "same", API: "two", ID: "model", Name: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(second) {
		t.Fatalf("models with provider/id identity should compare equal")
	}
}

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
	if !request.Model().Equal(model) || request.SystemPrompt() != "system" {
		t.Fatalf("request identity = (%v, %q), want (%v, system)", request.Model(), request.SystemPrompt(), model)
	}
}

func TestRequestAcceptsImportedToolHistoryForAdapterRepair(t *testing.T) {
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

	failure, err := newAssistantFailureMessage(nil, llm.FinishError, "failed", llm.Usage{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	wrongID := mustToolResult(t, "call-other", firstCall.Name(), "wrong")
	wrongName := mustToolResult(t, firstCall.ID(), "other", "wrong")
	tests := []struct {
		name     string
		messages []llm.ConversationMessage
	}{
		{name: "orphan", messages: []llm.ConversationMessage{firstResult}},
		{name: "out of order", messages: []llm.ConversationMessage{multiple, secondResult, firstResult}},
		{name: "id mismatch", messages: []llm.ConversationMessage{mustToolUseMessage(t, firstCall), wrongID}},
		{name: "name mismatch", messages: []llm.ConversationMessage{mustToolUseMessage(t, firstCall), wrongName}},
		{name: "duplicate result", messages: []llm.ConversationMessage{mustToolUseMessage(t, firstCall), firstResult, firstResult}},
		{name: "missing result", messages: []llm.ConversationMessage{mustToolUseMessage(t, firstCall)}},
		{name: "interrupted results", messages: []llm.ConversationMessage{multiple, firstResult, mustUser(t, "continue")}},
		{name: "failed assistant creates no call", messages: []llm.ConversationMessage{failure, firstResult}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request, err := provider.NewRequest(model, "", test.messages)
			if err != nil || len(request.Messages()) != len(test.messages) {
				t.Fatalf("NewRequest() = %#v, %v; imported history must reach adapter repair", request, err)
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
		model    provider.Model
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

func TestModelRequiresCompleteCanonicalFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*provider.ModelSpec)
	}{
		{name: "provider", mutate: func(spec *provider.ModelSpec) { spec.Provider = "" }},
		{name: "api", mutate: func(spec *provider.ModelSpec) { spec.API = "" }},
		{name: "id", mutate: func(spec *provider.ModelSpec) { spec.ID = string([]byte{0xff}) }},
		{name: "name", mutate: func(spec *provider.ModelSpec) { spec.Name = "" }},
		{name: "base URL encoding", mutate: func(spec *provider.ModelSpec) { spec.BaseURL = string([]byte{0xff}) }},
		{name: "context window", mutate: func(spec *provider.ModelSpec) { spec.ContextWindow = 0 }},
		{name: "max tokens", mutate: func(spec *provider.ModelSpec) { spec.MaxTokens = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := provider.ModelSpec{
				Provider: "openai", API: "responses", ID: "model", Name: "Model",
				BaseURL: "https://example.test/v1", Input: []provider.InputKind{provider.InputText},
				Cost: provider.CostRates{}, ContextWindow: 200_000, MaxTokens: 8_192,
			}
			tt.mutate(&spec)
			if _, err := provider.NewModel(spec); !errors.Is(err, provider.ErrInvalidModel) {
				t.Fatalf("NewModel() error = %v, want ErrInvalidModel", err)
			}
		})
	}
}

func TestModelPreservesOpenModelsJSONNumericAndInputValues(t *testing.T) {
	t.Parallel()
	model, err := provider.NewModel(provider.ModelSpec{
		Provider: "fixture", API: "fixture", ID: "model", Name: "Model",
		Input: []provider.InputKind{}, Cost: provider.CostRates{
			Input: -1, Tiers: []provider.CostTier{
				{InputTokensAbove: 100.5, Output: -2},
				{InputTokensAbove: -0.5, Output: -3},
			},
		},
		ContextWindow: 100, MaxTokens: 200,
	})
	if err != nil {
		t.Fatalf("open pi model values were rejected: %v", err)
	}
	if len(model.Input()) != 0 || model.Cost().Input != -1 || model.Cost().Tiers[0].InputTokensAbove != 100.5 || model.Cost().Tiers[1].InputTokensAbove != -0.5 || model.MaxTokens() != 200 {
		t.Fatalf("model values = input %#v cost %#v max %d", model.Input(), model.Cost(), model.MaxTokens())
	}
}

func TestLazyStreamDefersPreparationAndCloseBeforePullSkipsIt(t *testing.T) {
	t.Parallel()
	calls := 0
	want := errors.New("prepare failure")
	stream := provider.LazyStream(func() provider.EventStream {
		calls++
		return provider.FailureStream(want)
	})
	if calls != 0 {
		t.Fatal("lazy stream prepared before the first pull")
	}
	if _, err := stream.Next(); !errors.Is(err, want) || calls != 1 {
		t.Fatalf("first pull = %v, calls = %d", err, calls)
	}
	if _, err := stream.Next(); !errors.Is(err, io.EOF) || calls != 1 {
		t.Fatalf("second pull = %v, calls = %d", err, calls)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}

	closedCalls := 0
	closed := provider.LazyStream(func() provider.EventStream {
		closedCalls++
		return provider.FailureStream(want)
	})
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := closed.Next(); !errors.Is(err, io.EOF) || closedCalls != 0 {
		t.Fatalf("closed lazy stream = %v, calls = %d", err, closedCalls)
	}
}

func TestProviderFailurePreservesCategoryCauseAndOptionalMetadata(t *testing.T) {
	t.Parallel()

	cause := errors.New("rate limited")
	status := 429
	failure, err := provider.NewProviderFailure(provider.ProviderFailureSpec{
		Kind:         provider.FailureInvalidResponse,
		Message:      "provider rejected request",
		RetryMessage: "provider request failed safely",
		Cause:        cause,
		HTTPStatus:   &status,
		VendorCode:   "rate_limit",
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
	if got, ok := failure.RetryMessage(); !ok || got != "provider request failed safely" {
		t.Fatalf("RetryMessage() = (%q, %t), want sanitized message", got, ok)
	}

	invalidStatus := 99
	invalid := []provider.ProviderFailureSpec{
		{Message: "missing kind", Cause: cause},
		{Kind: provider.FailureFactory, Message: "missing cause"},
		{Kind: provider.FailureFactory, Message: " ", Cause: cause},
		{Kind: provider.FailureFactory, Message: "bad status", Cause: cause, HTTPStatus: &invalidStatus},
		{Kind: provider.FailureFactory, Message: "bad code", Cause: cause, VendorCode: " "},
		{Kind: provider.FailureFactory, Message: "bad retry message", RetryMessage: " ", Cause: cause},
	}
	for _, spec := range invalid {
		if _, err := provider.NewProviderFailure(spec); !errors.Is(err, provider.ErrInvalidProviderFailure) {
			t.Fatalf("NewProviderFailure(%+v) error = %v, want ErrInvalidProviderFailure", spec, err)
		}
	}
}

func mustModel(t *testing.T) provider.Model {
	t.Helper()
	model, err := newTestModel("scripted", "scripted", "scripted-1")
	if err != nil {
		t.Fatalf("NewModel() error = %v", err)
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
	message, err := newAssistantToolUseMessage(blocks, llm.Usage{}, time.Time{})
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

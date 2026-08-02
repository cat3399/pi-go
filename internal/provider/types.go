package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
)

var (
	ErrInvalidModel   = errors.New("invalid model reference")
	ErrInvalidRequest = errors.New("invalid provider request")
)

// ModelRef is the minimum stable identity needed to route one provider call.
// Catalog metadata and adapter configuration remain outside this value.
type ModelRef struct {
	provider string
	api      string
	id       string
}

func NewModelRef(provider, api, id string) (ModelRef, error) {
	model := ModelRef{provider: provider, api: api, id: id}
	if err := model.validate(); err != nil {
		return ModelRef{}, err
	}
	return model, nil
}

func (m ModelRef) validate() error {
	fields := []struct {
		name  string
		value string
	}{
		{name: "provider", value: m.provider},
		{name: "api", value: m.api},
		{name: "id", value: m.id},
	}
	for _, field := range fields {
		if !utf8.ValidString(field.value) || strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%w: %s must be non-empty valid UTF-8", ErrInvalidModel, field.name)
		}
	}
	return nil
}

func (m ModelRef) Provider() string { return m.provider }
func (m ModelRef) API() string      { return m.api }
func (m ModelRef) ID() string       { return m.id }

// Request is an immutable snapshot of one provider invocation.
type Request struct {
	model             ModelRef
	systemPrompt      string
	messages          []llm.ConversationMessage
	tools             []ToolDefinition
	parallelToolCalls bool
	replayTarget      llm.AssistantProvenance
}

// RequestOptions contains provider capabilities that must be chosen by the
// coordinator constructing a request. The zero value remains the safe
// single-call policy for callers without a parallel batch scheduler.
type RequestOptions struct {
	Tools                  []ToolDefinition
	AllowParallelToolCalls bool
}

func NewRequest(
	model ModelRef,
	systemPrompt string,
	messages []llm.ConversationMessage,
) (Request, error) {
	return NewRequestWithOptions(model, systemPrompt, messages, RequestOptions{})
}

// NewRequestWithTools creates one immutable provider request. The legacy
// NewRequest convenience remains deliberately tool-free for existing callers.
func NewRequestWithTools(
	model ModelRef,
	systemPrompt string,
	messages []llm.ConversationMessage,
	tools []ToolDefinition,
) (Request, error) {
	return NewRequestWithOptions(model, systemPrompt, messages, RequestOptions{Tools: tools})
}

// NewRequestWithOptions creates one immutable provider request with explicit
// tool-call concurrency capability. Callers that do not own a multi-call
// scheduler must leave AllowParallelToolCalls false.
func NewRequestWithOptions(
	model ModelRef,
	systemPrompt string,
	messages []llm.ConversationMessage,
	options RequestOptions,
) (Request, error) {
	request := Request{
		model:             model,
		systemPrompt:      systemPrompt,
		messages:          append([]llm.ConversationMessage(nil), messages...),
		tools:             append([]ToolDefinition(nil), options.Tools...),
		parallelToolCalls: options.AllowParallelToolCalls,
		replayTarget:      llm.AssistantProvenance{Provider: model.Provider(), API: model.API(), Model: model.ID()},
	}
	if err := request.validate(); err != nil {
		return Request{}, err
	}
	return request, nil
}

func (r Request) validate() error {
	if err := r.model.validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if !utf8.ValidString(r.systemPrompt) {
		return fmt.Errorf("%w: system prompt is not valid UTF-8", ErrInvalidRequest)
	}
	for index, message := range r.messages {
		if err := llm.ValidateConversationMessage(message); err != nil {
			return fmt.Errorf("%w: message %d: %w", ErrInvalidRequest, index, err)
		}
	}
	if err := validateToolResultCausality(r.messages); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	seenTools := make(map[string]struct{}, len(r.tools))
	for index, definition := range r.tools {
		if err := definition.validate(); err != nil {
			return fmt.Errorf("%w: tool %d: %w", ErrInvalidRequest, index, err)
		}
		if _, duplicate := seenTools[definition.Name()]; duplicate {
			return fmt.Errorf("%w: duplicate tool name %q", ErrInvalidRequest, definition.Name())
		}
		seenTools[definition.Name()] = struct{}{}
	}
	return nil
}

type pendingToolCall struct {
	call         llm.ToolCallBlock
	messageIndex int
}

type toolResultIdentity interface {
	ToolCallID() string
	ToolName() string
}

// validateToolResultCausality validates the conversation itself rather than
// relying on an adapter to notice malformed replay incidentally. One
// successful assistant tool-use turn introduces its calls in source order;
// the immediately following tool results must consume that ordered queue.
func validateToolResultCausality(messages []llm.ConversationMessage) error {
	pending := make([]pendingToolCall, 0)
	seenCalls := make(map[string]int)
	seenResults := make(map[string]int)

	for messageIndex, message := range messages {
		switch message := message.(type) {
		case llm.AssistantToolUseMessage:
			if len(pending) != 0 {
				return fmt.Errorf(
					"message %d: assistant tool call arrived before result for call %q from message %d",
					messageIndex,
					pending[0].call.ID(),
					pending[0].messageIndex,
				)
			}
			for _, block := range message.Blocks() {
				call, ok := block.(llm.ToolCallBlock)
				if !ok {
					continue
				}
				if firstIndex, duplicate := seenCalls[call.ID()]; duplicate {
					return fmt.Errorf(
						"message %d: duplicate tool call id %q first used by message %d",
						messageIndex,
						call.ID(),
						firstIndex,
					)
				}
				seenCalls[call.ID()] = messageIndex
				pending = append(pending, pendingToolCall{call: call, messageIndex: messageIndex})
			}

		case llm.ToolResultMessage:
			var err error
			pending, err = consumePendingToolResult(messageIndex, message, pending, seenResults)
			if err != nil {
				return err
			}

		case llm.ToolResultContentMessage:
			var err error
			pending, err = consumePendingToolResult(messageIndex, message, pending, seenResults)
			if err != nil {
				return err
			}

		default:
			if len(pending) != 0 {
				return fmt.Errorf(
					"message %d: %T arrived before result for call %q from message %d",
					messageIndex,
					message,
					pending[0].call.ID(),
					pending[0].messageIndex,
				)
			}
		}
	}

	if len(pending) != 0 {
		return fmt.Errorf(
			"conversation ended before result for call %q from message %d",
			pending[0].call.ID(),
			pending[0].messageIndex,
		)
	}
	return nil
}

func consumePendingToolResult(
	messageIndex int,
	message toolResultIdentity,
	pending []pendingToolCall,
	seenResults map[string]int,
) ([]pendingToolCall, error) {
	if firstIndex, duplicate := seenResults[message.ToolCallID()]; duplicate {
		return pending, fmt.Errorf(
			"message %d: duplicate tool result for call %q first supplied by message %d",
			messageIndex,
			message.ToolCallID(),
			firstIndex,
		)
	}
	if len(pending) == 0 {
		return pending, fmt.Errorf(
			"message %d: orphan tool result for call %q",
			messageIndex,
			message.ToolCallID(),
		)
	}

	expected := pending[0]
	if message.ToolCallID() != expected.call.ID() {
		for _, later := range pending[1:] {
			if message.ToolCallID() == later.call.ID() {
				return pending, fmt.Errorf(
					"message %d: out-of-order tool result for call %q; next call is %q",
					messageIndex,
					message.ToolCallID(),
					expected.call.ID(),
				)
			}
		}
	}
	if message.ToolCallID() != expected.call.ID() {
		return pending, fmt.Errorf(
			"message %d: %w: result call id %q, want %q",
			messageIndex,
			llm.ErrToolResultMismatch,
			message.ToolCallID(),
			expected.call.ID(),
		)
	}
	if message.ToolName() != expected.call.Name() {
		return pending, fmt.Errorf(
			"message %d: %w: result tool name %q, want %q",
			messageIndex,
			llm.ErrToolResultMismatch,
			message.ToolName(),
			expected.call.Name(),
		)
	}
	seenResults[message.ToolCallID()] = messageIndex
	return pending[1:], nil
}

func (r Request) clone() Request {
	r.messages = append([]llm.ConversationMessage(nil), r.messages...)
	r.tools = append([]ToolDefinition(nil), r.tools...)
	return r
}

func (r Request) Model() ModelRef { return r.model }

func (r Request) SystemPrompt() string { return r.systemPrompt }

func (r Request) Messages() []llm.ConversationMessage {
	return append([]llm.ConversationMessage(nil), r.messages...)
}

func (r Request) Tools() []ToolDefinition {
	return append([]ToolDefinition(nil), r.tools...)
}

func (r Request) ParallelToolCalls() bool { return r.parallelToolCalls }

func (r Request) ReplayTarget() llm.AssistantProvenance { return r.replayTarget }

// EventStream is a single-consumer pull stream. All expected provider failures
// are represented by llm.ErrorEvent; io.EOF follows the unique terminal event.
type EventStream interface {
	Next() (llm.StreamEvent, error)
	Close() error
}

// Provider is the narrow stream port consumed by the agent runtime.
type Provider interface {
	Stream(context.Context, Request) EventStream
}

func closedStreamError(err error) error {
	if errors.Is(err, io.EOF) {
		return err
	}
	return fmt.Errorf("provider stream failed: %w", err)
}

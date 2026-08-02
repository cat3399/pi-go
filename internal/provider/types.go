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
	model        ModelRef
	systemPrompt string
	messages     []llm.ConversationMessage
}

func NewRequest(
	model ModelRef,
	systemPrompt string,
	messages []llm.ConversationMessage,
) (Request, error) {
	request := Request{
		model:        model,
		systemPrompt: systemPrompt,
		messages:     append([]llm.ConversationMessage(nil), messages...),
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
	return nil
}

func (r Request) clone() Request {
	r.messages = append([]llm.ConversationMessage(nil), r.messages...)
	return r
}

func (r Request) Model() ModelRef { return r.model }

func (r Request) SystemPrompt() string { return r.systemPrompt }

func (r Request) Messages() []llm.ConversationMessage {
	return append([]llm.ConversationMessage(nil), r.messages...)
}

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

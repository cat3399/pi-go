package llm

import (
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalidText         = errors.New("invalid text")
	ErrInvalidFinishReason = errors.New("invalid finish reason")
)

// Role identifies the owner of a conversation message.
type Role uint8

const (
	RoleUser Role = iota + 1
	RoleAssistant
	RoleToolResult
)

func (r Role) String() string {
	switch r {
	case RoleUser:
		return "user"
	case RoleAssistant:
		return "assistant"
	case RoleToolResult:
		return "toolResult"
	default:
		return "unknown"
	}
}

// FinishReason describes why an assistant turn stopped. Pending is reserved for
// partial stream snapshots and is not a valid terminal reason.
type FinishReason uint8

const (
	FinishPending FinishReason = iota + 1
	FinishStop
	FinishLength
	FinishToolUse
	FinishError
	FinishAborted
)

func (r FinishReason) String() string {
	switch r {
	case FinishPending:
		return "pending"
	case FinishStop:
		return "stop"
	case FinishLength:
		return "length"
	case FinishToolUse:
		return "toolUse"
	case FinishError:
		return "error"
	case FinishAborted:
		return "aborted"
	default:
		return "unknown"
	}
}

// TextBlock is immutable text content. Empty text is valid.
type TextBlock struct {
	text string
}

func (TextBlock) assistantBlock() {}

func (TextBlock) Kind() AssistantBlockKind {
	return AssistantBlockText
}

func NewTextBlock(text string) (TextBlock, error) {
	block := TextBlock{text: text}
	if err := block.validate(); err != nil {
		return TextBlock{}, err
	}
	return block, nil
}

func (b TextBlock) validate() error {
	if !utf8.ValidString(b.text) {
		return fmt.Errorf("%w: content is not valid UTF-8", ErrInvalidText)
	}
	return nil
}

func (b TextBlock) Text() string {
	return b.text
}

// UserTextMessage stores user text as the canonical content-block list used by
// provider requests. NewUserTextMessage is the common one-block convenience
// constructor; session adapters can preserve multiple known text blocks with
// NewUserTextBlocksMessage.
type UserTextMessage struct {
	content   []TextBlock
	timestamp time.Time
}

func (UserTextMessage) conversationMessage() {}

func NewUserTextMessage(text string, timestamp time.Time) (UserTextMessage, error) {
	block, err := NewTextBlock(text)
	if err != nil {
		return UserTextMessage{}, err
	}
	return NewUserTextBlocksMessage([]TextBlock{block}, timestamp)
}

func NewUserTextBlocksMessage(content []TextBlock, timestamp time.Time) (UserTextMessage, error) {
	for _, block := range content {
		if err := block.validate(); err != nil {
			return UserTextMessage{}, err
		}
	}
	return UserTextMessage{content: append([]TextBlock(nil), content...), timestamp: timestamp}, nil
}

func (m UserTextMessage) validate() error {
	for _, block := range m.content {
		if err := block.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (UserTextMessage) Role() Role {
	return RoleUser
}

func (m UserTextMessage) Content() []TextBlock {
	return append([]TextBlock(nil), m.content...)
}

func (m UserTextMessage) Timestamp() time.Time {
	return m.timestamp
}

// AssistantTextMessage is a successful terminal assistant message containing
// only text blocks. Tool-use and failed terminal messages have separate
// constructors in their behavior slices.
type AssistantTextMessage struct {
	content   []TextBlock
	finish    FinishReason
	usage     Usage
	timestamp time.Time
}

func (AssistantTextMessage) assistantTerminal()   {}
func (AssistantTextMessage) conversationMessage() {}

func NewAssistantTextMessage(
	content []TextBlock,
	finish FinishReason,
	usage Usage,
	timestamp time.Time,
) (AssistantTextMessage, error) {
	if finish != FinishStop && finish != FinishLength {
		return AssistantTextMessage{}, fmt.Errorf(
			"%w: successful text message cannot finish with %q",
			ErrInvalidFinishReason,
			finish,
		)
	}
	for _, block := range content {
		if err := block.validate(); err != nil {
			return AssistantTextMessage{}, err
		}
	}

	return AssistantTextMessage{
		content:   append([]TextBlock(nil), content...),
		finish:    finish,
		usage:     usage,
		timestamp: timestamp,
	}, nil
}

func (m AssistantTextMessage) validate() error {
	if m.finish != FinishStop && m.finish != FinishLength {
		return fmt.Errorf(
			"%w: successful text message cannot finish with %q",
			ErrInvalidFinishReason,
			m.finish,
		)
	}
	for _, block := range m.content {
		if err := block.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (AssistantTextMessage) Role() Role {
	return RoleAssistant
}

func (m AssistantTextMessage) Content() []TextBlock {
	return append([]TextBlock(nil), m.content...)
}

func (m AssistantTextMessage) FinishReason() FinishReason {
	return m.finish
}

func (m AssistantTextMessage) Usage() Usage {
	return m.usage
}

func (m AssistantTextMessage) Timestamp() time.Time {
	return m.timestamp
}

// Blocks returns assistant content through the sealed assistant block union.
func (m AssistantTextMessage) Blocks() []AssistantBlock {
	blocks := make([]AssistantBlock, len(m.content))
	for index, block := range m.content {
		blocks[index] = block
	}
	return blocks
}

// ConversationMessage is the sealed, ordered message union used in provider
// requests and agent transcripts.
type ConversationMessage interface {
	Role() Role
	Timestamp() time.Time
	conversationMessage()
}

// ValidateConversationMessage rejects unsupported implementations, pointer
// aliases, and invalid exported zero values at package boundaries.
func ValidateConversationMessage(message ConversationMessage) error {
	switch message := message.(type) {
	case UserTextMessage:
		return message.validate()
	case AssistantTextMessage:
		return message.validate()
	case AssistantToolUseMessage:
		return message.validate()
	case AssistantFailureMessage:
		return message.validate()
	case ToolResultMessage:
		return message.validate()
	default:
		return fmt.Errorf("invalid conversation message %T", message)
	}
}

// AssistantTerminal is the sealed terminal assistant union consumed by the
// provider and agent layers.
type AssistantTerminal interface {
	ConversationMessage
	Blocks() []AssistantBlock
	FinishReason() FinishReason
	Usage() Usage
	assistantTerminal()
}

// ValidateAssistantTerminal rejects zero values and pointer aliases even when
// callers bypass the constructors of exported value types.
func ValidateAssistantTerminal(message AssistantTerminal) error {
	switch message := message.(type) {
	case AssistantTextMessage:
		return message.validate()
	case AssistantToolUseMessage:
		return message.validate()
	case AssistantFailureMessage:
		return message.validate()
	default:
		return fmt.Errorf("invalid assistant terminal %T", message)
	}
}

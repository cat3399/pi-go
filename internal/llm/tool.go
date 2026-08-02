package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalidToolCall    = errors.New("invalid tool call")
	ErrInvalidToolResult  = errors.New("invalid tool result")
	ErrToolResultMismatch = errors.New("tool result does not match tool call")
)

// AssistantBlockKind identifies one member of the assistant content union.
type AssistantBlockKind uint8

const (
	AssistantBlockText AssistantBlockKind = iota + 1
	AssistantBlockToolCall
	AssistantBlockThinking
)

// AssistantBlock is sealed so only validated llm block values can enter an
// assistant message.
type AssistantBlock interface {
	Kind() AssistantBlockKind
	assistantBlock()
}

// ToolCallBlock preserves provider arguments as raw JSON object bytes. Typed
// argument decoding belongs to the selected tool.
type ToolCallBlock struct {
	id        string
	name      string
	arguments []byte
}

func NewToolCallBlock(id, name string, argumentsJSON []byte) (ToolCallBlock, error) {
	call := ToolCallBlock{
		id:        id,
		name:      name,
		arguments: bytes.Clone(argumentsJSON),
	}
	if err := call.validate(); err != nil {
		return ToolCallBlock{}, err
	}
	return call, nil
}

func (c ToolCallBlock) validate() error {
	if err := validateToolIdentity(c.id, "id"); err != nil {
		return err
	}
	if err := validateToolIdentity(c.name, "name"); err != nil {
		return err
	}
	if !utf8.Valid(c.arguments) {
		return fmt.Errorf("%w: arguments are not valid UTF-8", ErrInvalidToolCall)
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(c.arguments, &object); err != nil || object == nil {
		if err == nil {
			err = errors.New("top-level value is not an object")
		}
		return fmt.Errorf("%w: arguments: %v", ErrInvalidToolCall, err)
	}
	return nil
}

func (ToolCallBlock) assistantBlock() {}

func (ToolCallBlock) Kind() AssistantBlockKind {
	return AssistantBlockToolCall
}

func (c ToolCallBlock) ID() string {
	return c.id
}

func (c ToolCallBlock) Name() string {
	return c.name
}

func (c ToolCallBlock) ArgumentsJSON() []byte {
	return bytes.Clone(c.arguments)
}

// AssistantToolUseMessage is a successful terminal assistant message with at
// least one complete tool call. Text and tool blocks retain provider order.
type AssistantToolUseMessage struct {
	content    []AssistantBlock
	usage      Usage
	timestamp  time.Time
	responses  *OpenAIResponsesResponse
	provenance *AssistantProvenance
}

func NewAssistantToolUseMessageWithResponsesReplay(content []AssistantBlock, usage Usage, timestamp time.Time, replay *OpenAIResponsesResponse) (AssistantToolUseMessage, error) {
	return NewAssistantToolUseMessageWithReplay(content, usage, timestamp, nil, replay)
}
func NewAssistantToolUseMessageWithReplay(content []AssistantBlock, usage Usage, timestamp time.Time, provenance *AssistantProvenance, replay *OpenAIResponsesResponse) (AssistantToolUseMessage, error) {
	m, err := NewAssistantToolUseMessage(content, usage, timestamp)
	if err != nil {
		return AssistantToolUseMessage{}, err
	}
	if replay != nil {
		copy := *replay
		if err := copy.validate(); err != nil {
			return AssistantToolUseMessage{}, err
		}
		m.responses = &copy
	}
	if provenance != nil {
		copy := *provenance
		if err := copy.validate(); err != nil {
			return AssistantToolUseMessage{}, err
		}
		m.provenance = &copy
	}
	return m, nil
}

func (AssistantToolUseMessage) assistantTerminal()   {}
func (AssistantToolUseMessage) conversationMessage() {}

func NewAssistantToolUseMessage(
	content []AssistantBlock,
	usage Usage,
	timestamp time.Time,
) (AssistantToolUseMessage, error) {
	seenCalls := make(map[string]struct{})
	toolCalls := 0
	for _, block := range content {
		switch block := block.(type) {
		case TextBlock:
			if err := block.validate(); err != nil {
				return AssistantToolUseMessage{}, err
			}
			continue
		case ToolCallBlock:
			if err := block.validate(); err != nil {
				return AssistantToolUseMessage{}, err
			}
			toolCalls++
			if _, duplicate := seenCalls[block.ID()]; duplicate {
				return AssistantToolUseMessage{}, fmt.Errorf(
					"%w: duplicate id %q",
					ErrInvalidToolCall,
					block.ID(),
				)
			}
			seenCalls[block.ID()] = struct{}{}
		case ThinkingBlock:
			if err := block.validate(); err != nil {
				return AssistantToolUseMessage{}, err
			}
		default:
			return AssistantToolUseMessage{}, fmt.Errorf(
				"%w: unsupported content block %T",
				ErrInvalidToolCall,
				block,
			)
		}
	}
	if toolCalls == 0 {
		return AssistantToolUseMessage{}, fmt.Errorf("%w: tool-use message has no tool call", ErrInvalidToolCall)
	}

	return AssistantToolUseMessage{
		content:   append([]AssistantBlock(nil), content...),
		usage:     usage,
		timestamp: timestamp,
	}, nil
}

// AssistantRichMessage is a completed non-tool assistant response that may
// contain interleaved thinking and text. It is kept distinct from the legacy
// text-only value so existing callers cannot accidentally accept reasoning.
type AssistantRichMessage struct {
	content    []AssistantBlock
	finish     FinishReason
	usage      Usage
	timestamp  time.Time
	responses  *OpenAIResponsesResponse
	provenance *AssistantProvenance
}

func NewAssistantRichMessageWithResponsesReplay(content []AssistantBlock, finish FinishReason, usage Usage, timestamp time.Time, replay *OpenAIResponsesResponse) (AssistantRichMessage, error) {
	return NewAssistantRichMessageWithReplay(content, finish, usage, timestamp, nil, replay)
}
func NewAssistantRichMessageWithReplay(content []AssistantBlock, finish FinishReason, usage Usage, timestamp time.Time, provenance *AssistantProvenance, replay *OpenAIResponsesResponse) (AssistantRichMessage, error) {
	m, err := NewAssistantRichMessage(content, finish, usage, timestamp)
	if err != nil {
		return AssistantRichMessage{}, err
	}
	if replay != nil {
		copy := *replay
		if err := copy.validate(); err != nil {
			return AssistantRichMessage{}, err
		}
		m.responses = &copy
	}
	if provenance != nil {
		copy := *provenance
		if err := copy.validate(); err != nil {
			return AssistantRichMessage{}, err
		}
		m.provenance = &copy
	}
	return m, nil
}

func (AssistantRichMessage) assistantTerminal()   {}
func (AssistantRichMessage) conversationMessage() {}
func NewAssistantRichMessage(content []AssistantBlock, finish FinishReason, usage Usage, timestamp time.Time) (AssistantRichMessage, error) {
	if finish != FinishStop && finish != FinishLength {
		return AssistantRichMessage{}, fmt.Errorf("%w: rich assistant finish %q", ErrInvalidFinishReason, finish)
	}
	if len(content) == 0 {
		return AssistantRichMessage{}, fmt.Errorf("%w: rich assistant has no content", ErrInvalidRichContent)
	}
	for _, b := range content {
		switch b := b.(type) {
		case TextBlock:
			if err := b.validate(); err != nil {
				return AssistantRichMessage{}, err
			}
		case ThinkingBlock:
			if err := b.validate(); err != nil {
				return AssistantRichMessage{}, err
			}
		default:
			return AssistantRichMessage{}, fmt.Errorf("%w: rich assistant block %T", ErrInvalidRichContent, b)
		}
	}
	return AssistantRichMessage{content: append([]AssistantBlock(nil), content...), finish: finish, usage: usage, timestamp: timestamp}, nil
}
func (m AssistantRichMessage) validate() error {
	_, err := NewAssistantRichMessage(m.content, m.finish, m.usage, m.timestamp)
	if err == nil && m.responses != nil {
		err = m.responses.validate()
	}
	if err == nil && m.provenance != nil {
		err = m.provenance.validate()
	}
	return err
}
func (m AssistantRichMessage) AssistantProvenance() (AssistantProvenance, bool) {
	if m.provenance == nil {
		return AssistantProvenance{}, false
	}
	return *m.provenance, true
}
func (m AssistantRichMessage) OpenAIResponsesMetadata() (OpenAIResponsesResponse, bool) {
	if m.responses == nil {
		return OpenAIResponsesResponse{}, false
	}
	return *m.responses, true
}
func (AssistantRichMessage) Role() Role { return RoleAssistant }
func (m AssistantRichMessage) Blocks() []AssistantBlock {
	return append([]AssistantBlock(nil), m.content...)
}
func (m AssistantRichMessage) FinishReason() FinishReason { return m.finish }
func (m AssistantRichMessage) Usage() Usage               { return m.usage }
func (m AssistantRichMessage) Timestamp() time.Time       { return m.timestamp }

// ToolResultContentMessage is the image-capable analogue of ToolResultMessage.
// Text-only results retain the original concrete type for source compatibility.
type ToolResultContentMessage struct {
	toolCallID, toolName string
	content              []ToolResultContentBlock
	isError              bool
	timestamp            time.Time
}

func (ToolResultContentMessage) conversationMessage() {}
func NewToolResultContentMessage(id, name string, content []ToolResultContentBlock, isError bool, timestamp time.Time) (ToolResultContentMessage, error) {
	m := ToolResultContentMessage{toolCallID: id, toolName: name, content: append([]ToolResultContentBlock(nil), content...), isError: isError, timestamp: timestamp}
	if err := m.validate(); err != nil {
		return ToolResultContentMessage{}, err
	}
	return m, nil
}
func (m ToolResultContentMessage) validate() error {
	if err := validateToolResultIdentity(m.toolCallID, "toolCallId"); err != nil {
		return err
	}
	if err := validateToolResultIdentity(m.toolName, "toolName"); err != nil {
		return err
	}
	for _, b := range m.content {
		switch b := b.(type) {
		case TextBlock:
			if err := b.validate(); err != nil {
				return err
			}
		case ImageBlock:
			if err := b.validate(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: tool result block %T", ErrInvalidRichContent, b)
		}
	}
	return nil
}
func (ToolResultContentMessage) Role() Role           { return RoleToolResult }
func (m ToolResultContentMessage) ToolCallID() string { return m.toolCallID }
func (m ToolResultContentMessage) ToolName() string   { return m.toolName }
func (m ToolResultContentMessage) Content() []ToolResultContentBlock {
	return append([]ToolResultContentBlock(nil), m.content...)
}
func (m ToolResultContentMessage) IsError() bool        { return m.isError }
func (m ToolResultContentMessage) Timestamp() time.Time { return m.timestamp }

func (m AssistantToolUseMessage) validate() error {
	_, err := NewAssistantToolUseMessage(m.content, m.usage, m.timestamp)
	if err == nil && m.responses != nil {
		err = m.responses.validate()
	}
	if err == nil && m.provenance != nil {
		err = m.provenance.validate()
	}
	return err
}
func (m AssistantToolUseMessage) AssistantProvenance() (AssistantProvenance, bool) {
	if m.provenance == nil {
		return AssistantProvenance{}, false
	}
	return *m.provenance, true
}
func (m AssistantToolUseMessage) OpenAIResponsesMetadata() (OpenAIResponsesResponse, bool) {
	if m.responses == nil {
		return OpenAIResponsesResponse{}, false
	}
	return *m.responses, true
}

func (AssistantToolUseMessage) Role() Role {
	return RoleAssistant
}

func (AssistantToolUseMessage) FinishReason() FinishReason {
	return FinishToolUse
}

func (m AssistantToolUseMessage) Content() []AssistantBlock {
	return append([]AssistantBlock(nil), m.content...)
}

func (m AssistantToolUseMessage) Blocks() []AssistantBlock {
	return m.Content()
}

func (m AssistantToolUseMessage) Usage() Usage {
	return m.usage
}

func (m AssistantToolUseMessage) Timestamp() time.Time {
	return m.timestamp
}

// ToolResultMessage records one tool execution outcome. The agent runtime owns
// pairing it with the pending call and committing it to the transcript.
type ToolResultMessage struct {
	toolCallID string
	toolName   string
	content    []TextBlock
	isError    bool
	timestamp  time.Time
}

func (ToolResultMessage) conversationMessage() {}

func NewToolResultMessage(
	toolCallID string,
	toolName string,
	content []TextBlock,
	isError bool,
	timestamp time.Time,
) (ToolResultMessage, error) {
	result := ToolResultMessage{
		toolCallID: toolCallID,
		toolName:   toolName,
		content:    append([]TextBlock(nil), content...),
		isError:    isError,
		timestamp:  timestamp,
	}
	if err := result.validate(); err != nil {
		return ToolResultMessage{}, err
	}
	return result, nil
}

func (m ToolResultMessage) validate() error {
	if err := validateToolResultIdentity(m.toolCallID, "toolCallId"); err != nil {
		return err
	}
	if err := validateToolResultIdentity(m.toolName, "toolName"); err != nil {
		return err
	}
	for _, block := range m.content {
		if err := block.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (ToolResultMessage) Role() Role {
	return RoleToolResult
}

func (m ToolResultMessage) ToolCallID() string {
	return m.toolCallID
}

func (m ToolResultMessage) ToolName() string {
	return m.toolName
}

func (m ToolResultMessage) Content() []TextBlock {
	return append([]TextBlock(nil), m.content...)
}

func (m ToolResultMessage) IsError() bool {
	return m.isError
}

func (m ToolResultMessage) Timestamp() time.Time {
	return m.timestamp
}

func ValidateToolResultAssociation(call ToolCallBlock, result ToolResultMessage) error {
	if err := call.validate(); err != nil {
		return err
	}
	if err := result.validate(); err != nil {
		return err
	}
	if call.ID() != result.ToolCallID() {
		return fmt.Errorf(
			"%w: result call id %q, want %q",
			ErrToolResultMismatch,
			result.ToolCallID(),
			call.ID(),
		)
	}
	if call.Name() != result.ToolName() {
		return fmt.Errorf(
			"%w: result tool name %q, want %q",
			ErrToolResultMismatch,
			result.ToolName(),
			call.Name(),
		)
	}
	return nil
}

func validateToolIdentity(value, field string) error {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s must be non-empty valid UTF-8", ErrInvalidToolCall, field)
	}
	return nil
}

func validateToolResultIdentity(value, field string) error {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s must be non-empty valid UTF-8", ErrInvalidToolResult, field)
	}
	return nil
}

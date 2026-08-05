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
	id               string
	name             string
	arguments        []byte
	thoughtSignature string
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

func NewToolCallBlockWithThoughtSignature(id, name string, argumentsJSON []byte, signature string) (ToolCallBlock, error) {
	call := ToolCallBlock{id: id, name: name, arguments: bytes.Clone(argumentsJSON), thoughtSignature: signature}
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
	if err := validateOpaqueSignature(c.thoughtSignature); err != nil {
		return err
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

func (c ToolCallBlock) ThoughtSignature() (string, bool) {
	return c.thoughtSignature, c.thoughtSignature != ""
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
	content   []AssistantBlock
	finish    FinishReason
	usage     Usage
	timestamp time.Time
	metadata  AssistantMetadata
}

func NewAssistantToolUseMessageWithMetadata(content []AssistantBlock, usage Usage, timestamp time.Time, provenance AssistantProvenance, response *AssistantResponseMetadata, diagnostics []AssistantDiagnostic) (AssistantToolUseMessage, error) {
	return NewAssistantToolUseMessageWithFinishAndMetadata(content, FinishToolUse, usage, timestamp, provenance, response, diagnostics)
}

// NewAssistantToolUseMessageWithFinishAndMetadata preserves the provider's
// actual stop reason when a completed tool-call block is present. Providers
// sometimes report stop rather than toolUse, and length is safety-critical:
// arguments from a truncated message are never safe to execute.
func NewAssistantToolUseMessageWithFinishAndMetadata(content []AssistantBlock, finish FinishReason, usage Usage, timestamp time.Time, provenance AssistantProvenance, response *AssistantResponseMetadata, diagnostics []AssistantDiagnostic) (AssistantToolUseMessage, error) {
	message := AssistantToolUseMessage{
		content: append([]AssistantBlock(nil), content...), finish: finish, usage: usage, timestamp: timestamp,
		metadata: cloneAssistantMetadata(AssistantMetadata{Provenance: provenance, Response: response, Diagnostics: diagnostics}),
	}
	if err := message.validate(); err != nil {
		return AssistantToolUseMessage{}, err
	}
	return message, nil
}

func (AssistantToolUseMessage) assistantTerminal()   {}
func (AssistantToolUseMessage) conversationMessage() {}

func NewAssistantToolUseMessage(
	content []AssistantBlock,
	usage Usage,
	timestamp time.Time,
	provenance AssistantProvenance,
) (AssistantToolUseMessage, error) {
	return NewAssistantToolUseMessageWithMetadata(content, usage, timestamp, provenance, nil, nil)
}

func validateAssistantToolUseContent(content []AssistantBlock) error {
	seenCalls := make(map[string]struct{})
	toolCalls := 0
	for _, block := range content {
		switch block := block.(type) {
		case TextBlock:
			if err := block.validate(); err != nil {
				return err
			}
			continue
		case ToolCallBlock:
			if err := block.validate(); err != nil {
				return err
			}
			toolCalls++
			if _, duplicate := seenCalls[block.ID()]; duplicate {
				return fmt.Errorf(
					"%w: duplicate id %q",
					ErrInvalidToolCall,
					block.ID(),
				)
			}
			seenCalls[block.ID()] = struct{}{}
		case ThinkingBlock:
			if err := block.validate(); err != nil {
				return err
			}
		default:
			return fmt.Errorf(
				"%w: unsupported content block %T",
				ErrInvalidToolCall,
				block,
			)
		}
	}
	if toolCalls == 0 {
		return fmt.Errorf("%w: tool-use message has no tool call", ErrInvalidToolCall)
	}
	return nil
}

// AssistantRichMessage is a completed non-tool assistant response that may
// contain interleaved thinking and text. It is kept distinct from the legacy
// text-only value so existing callers cannot accidentally accept reasoning.
type AssistantRichMessage struct {
	content   []AssistantBlock
	finish    FinishReason
	usage     Usage
	timestamp time.Time
	metadata  AssistantMetadata
}

func NewAssistantRichMessageWithMetadata(content []AssistantBlock, finish FinishReason, usage Usage, timestamp time.Time, provenance AssistantProvenance, response *AssistantResponseMetadata, diagnostics []AssistantDiagnostic) (AssistantRichMessage, error) {
	message := AssistantRichMessage{
		content: append([]AssistantBlock(nil), content...), finish: finish, usage: usage, timestamp: timestamp,
		metadata: cloneAssistantMetadata(AssistantMetadata{Provenance: provenance, Response: response, Diagnostics: diagnostics}),
	}
	if err := message.validate(); err != nil {
		return AssistantRichMessage{}, err
	}
	return message, nil
}

func (AssistantRichMessage) assistantTerminal()   {}
func (AssistantRichMessage) conversationMessage() {}
func NewAssistantRichMessage(content []AssistantBlock, finish FinishReason, usage Usage, timestamp time.Time, provenance AssistantProvenance) (AssistantRichMessage, error) {
	return NewAssistantRichMessageWithMetadata(content, finish, usage, timestamp, provenance, nil, nil)
}

func validateAssistantRichContent(content []AssistantBlock, finish FinishReason) error {
	if finish != FinishStop && finish != FinishLength {
		return fmt.Errorf("%w: rich assistant finish %q", ErrInvalidFinishReason, finish)
	}
	if len(content) == 0 {
		return fmt.Errorf("%w: rich assistant has no content", ErrInvalidRichContent)
	}
	for _, b := range content {
		switch b := b.(type) {
		case TextBlock:
			if err := b.validate(); err != nil {
				return err
			}
		case ThinkingBlock:
			if err := b.validate(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: rich assistant block %T", ErrInvalidRichContent, b)
		}
	}
	return nil
}
func (m AssistantRichMessage) validate() error {
	if err := validateAssistantRichContent(m.content, m.finish); err != nil {
		return err
	}
	return m.metadata.validate()
}
func (m AssistantRichMessage) AssistantProvenance() AssistantProvenance { return m.metadata.Provenance }
func (m AssistantRichMessage) ResponseMetadata() (AssistantResponseMetadata, bool) {
	if m.metadata.Response == nil {
		return AssistantResponseMetadata{}, false
	}
	return *m.metadata.Response, true
}
func (m AssistantRichMessage) Diagnostics() []AssistantDiagnostic {
	return cloneAssistantDiagnostics(m.metadata.Diagnostics)
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
	details              json.RawMessage
	usage                *Usage
	addedToolNames       []string
	hasAddedToolNames    bool
}

// ToolResultMetadata is the information carried by pi's ToolResultMessage
// beyond provider-visible content. Details never enters an LLM request;
// Usage and AddedToolNames remain available to provider adapters that support
// deferred tools and to lifecycle observers.
type ToolResultMetadata struct {
	Details        json.RawMessage
	Usage          *Usage
	AddedToolNames []string
	// HasAddedToolNames preserves the difference between an omitted field and
	// an explicitly supplied empty deferred-tool list.
	HasAddedToolNames bool
}

func (ToolResultContentMessage) conversationMessage() {}
func NewToolResultContentMessage(id, name string, content []ToolResultContentBlock, isError bool, timestamp time.Time) (ToolResultContentMessage, error) {
	return NewToolResultContentMessageWithDetails(id, name, content, isError, timestamp, nil)
}
func NewToolResultContentMessageWithDetails(id, name string, content []ToolResultContentBlock, isError bool, timestamp time.Time, details json.RawMessage) (ToolResultContentMessage, error) {
	return NewToolResultContentMessageWithMetadata(id, name, content, isError, timestamp, ToolResultMetadata{Details: details})
}
func NewToolResultContentMessageWithMetadata(id, name string, content []ToolResultContentBlock, isError bool, timestamp time.Time, metadata ToolResultMetadata) (ToolResultContentMessage, error) {
	if err := validateToolNames(metadata.AddedToolNames); err != nil {
		return ToolResultContentMessage{}, err
	}
	var ownedContent []ToolResultContentBlock
	if content != nil {
		ownedContent = make([]ToolResultContentBlock, len(content))
		copy(ownedContent, content)
	}
	m := ToolResultContentMessage{toolCallID: id, toolName: name, content: ownedContent, isError: isError, timestamp: timestamp, details: bytes.Clone(metadata.Details), addedToolNames: cloneToolNames(metadata.AddedToolNames), hasAddedToolNames: metadata.HasAddedToolNames || metadata.AddedToolNames != nil}
	if metadata.Usage != nil {
		usage := *metadata.Usage
		m.usage = &usage
	}
	if err := m.validate(); err != nil {
		return ToolResultContentMessage{}, err
	}
	if len(m.details) != 0 && !json.Valid(m.details) {
		return ToolResultContentMessage{}, fmt.Errorf("%w: details are not valid JSON", ErrInvalidToolResult)
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
	if err := validateToolNames(m.addedToolNames); err != nil {
		return err
	}
	return nil
}
func (ToolResultContentMessage) Role() Role           { return RoleToolResult }
func (m ToolResultContentMessage) ToolCallID() string { return m.toolCallID }
func (m ToolResultContentMessage) ToolName() string   { return m.toolName }
func (m ToolResultContentMessage) Content() []ToolResultContentBlock {
	if m.content == nil {
		return nil
	}
	content := make([]ToolResultContentBlock, len(m.content))
	copy(content, m.content)
	return content
}
func (m ToolResultContentMessage) IsError() bool            { return m.isError }
func (m ToolResultContentMessage) Timestamp() time.Time     { return m.timestamp }
func (m ToolResultContentMessage) Details() json.RawMessage { return bytes.Clone(m.details) }
func (m ToolResultContentMessage) Usage() (Usage, bool) {
	if m.usage == nil {
		return Usage{}, false
	}
	return *m.usage, true
}
func (m ToolResultContentMessage) AddedToolNames() []string {
	return append([]string(nil), m.addedToolNames...)
}
func (m ToolResultContentMessage) HasAddedToolNames() bool { return m.hasAddedToolNames }

func (m AssistantToolUseMessage) validate() error {
	if m.finish != FinishToolUse && m.finish != FinishStop && m.finish != FinishLength {
		return fmt.Errorf("%w: tool-use message cannot finish with %q", ErrInvalidFinishReason, m.finish)
	}
	if err := validateAssistantToolUseContent(m.content); err != nil {
		return err
	}
	return m.metadata.validate()
}
func (m AssistantToolUseMessage) AssistantProvenance() AssistantProvenance {
	return m.metadata.Provenance
}
func (m AssistantToolUseMessage) ResponseMetadata() (AssistantResponseMetadata, bool) {
	if m.metadata.Response == nil {
		return AssistantResponseMetadata{}, false
	}
	return *m.metadata.Response, true
}
func (m AssistantToolUseMessage) Diagnostics() []AssistantDiagnostic {
	return cloneAssistantDiagnostics(m.metadata.Diagnostics)
}

func (AssistantToolUseMessage) Role() Role {
	return RoleAssistant
}

func (m AssistantToolUseMessage) FinishReason() FinishReason {
	return m.finish
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
	toolCallID        string
	toolName          string
	content           []TextBlock
	isError           bool
	timestamp         time.Time
	details           json.RawMessage
	usage             *Usage
	addedToolNames    []string
	hasAddedToolNames bool
}

func (ToolResultMessage) conversationMessage() {}

func NewToolResultMessage(
	toolCallID string,
	toolName string,
	content []TextBlock,
	isError bool,
	timestamp time.Time,
) (ToolResultMessage, error) {
	return NewToolResultMessageWithDetails(toolCallID, toolName, content, isError, timestamp, nil)
}

func NewToolResultMessageWithDetails(toolCallID string, toolName string, content []TextBlock, isError bool, timestamp time.Time, details json.RawMessage) (ToolResultMessage, error) {
	return NewToolResultMessageWithMetadata(toolCallID, toolName, content, isError, timestamp, ToolResultMetadata{Details: details})
}
func NewToolResultMessageWithMetadata(toolCallID string, toolName string, content []TextBlock, isError bool, timestamp time.Time, metadata ToolResultMetadata) (ToolResultMessage, error) {
	if err := validateToolNames(metadata.AddedToolNames); err != nil {
		return ToolResultMessage{}, err
	}
	result := ToolResultMessage{
		toolCallID:     toolCallID,
		toolName:       toolName,
		content:        append([]TextBlock(nil), content...),
		isError:        isError,
		timestamp:      timestamp,
		details:        bytes.Clone(metadata.Details),
		addedToolNames: cloneToolNames(metadata.AddedToolNames), hasAddedToolNames: metadata.HasAddedToolNames || metadata.AddedToolNames != nil,
	}
	if metadata.Usage != nil {
		usage := *metadata.Usage
		result.usage = &usage
	}
	if len(result.details) != 0 && !json.Valid(result.details) {
		return ToolResultMessage{}, fmt.Errorf("%w: details are not valid JSON", ErrInvalidToolResult)
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
	if err := validateToolNames(m.addedToolNames); err != nil {
		return err
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
func (m ToolResultMessage) Details() json.RawMessage { return bytes.Clone(m.details) }
func (m ToolResultMessage) Usage() (Usage, bool) {
	if m.usage == nil {
		return Usage{}, false
	}
	return *m.usage, true
}
func (m ToolResultMessage) AddedToolNames() []string {
	return append([]string(nil), m.addedToolNames...)
}
func (m ToolResultMessage) HasAddedToolNames() bool { return m.hasAddedToolNames }

func cloneToolNames(names []string) []string {
	if names == nil {
		return nil
	}
	return append([]string(nil), names...)
}
func validateToolNames(names []string) error {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if !utf8.ValidString(name) || strings.TrimSpace(name) == "" {
			return fmt.Errorf("%w: invalid added tool name", ErrInvalidToolResult)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("%w: duplicate added tool name %q", ErrInvalidToolResult, name)
		}
		seen[name] = struct{}{}
	}
	return nil
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

// ValidateToolResultContentAssociation is the rich-content equivalent of
// ValidateToolResultAssociation. Keeping the association validation at the
// message boundary means the agent loop can preserve image tool output without
// weakening causality checks.
func ValidateToolResultContentAssociation(call ToolCallBlock, result ToolResultContentMessage) error {
	if err := call.validate(); err != nil {
		return err
	}
	if err := result.validate(); err != nil {
		return err
	}
	if call.ID() != result.ToolCallID() {
		return fmt.Errorf("%w: result call id %q, want %q", ErrToolResultMismatch, result.ToolCallID(), call.ID())
	}
	if call.Name() != result.ToolName() {
		return fmt.Errorf("%w: result tool name %q, want %q", ErrToolResultMismatch, result.ToolName(), call.Name())
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

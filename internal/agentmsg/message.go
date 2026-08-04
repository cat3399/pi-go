// Package agentmsg contains the provider-neutral AgentMessage union.  It is
// deliberately below the stateful agent package so session storage, tools, and
// a future AgentLoop can share the exact same values without an import cycle.
package agentmsg

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
)

const (
	CompactionSummaryPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	CompactionSummarySuffix = "\n</summary>"
	BranchSummaryPrefix     = "The following is a summary of a branch that this conversation came back from:\n\n<summary>\n"
	BranchSummarySuffix     = "</summary>"
)

// Role names the complete coding-agent message vocabulary.  LLM messages keep
// their normal roles; the remaining members must pass ConvertToLLM before a
// provider request.  This is the Go equivalent of pi's extensible AgentMessage
// union, with a sealed interface instead of TypeScript declaration merging.
type Role string

const (
	RoleUser              Role = "user"
	RoleAssistant         Role = "assistant"
	RoleToolResult        Role = "toolResult"
	RoleBashExecution     Role = "bashExecution"
	RoleCustom            Role = "custom"
	RoleBranchSummary     Role = "branchSummary"
	RoleCompactionSummary Role = "compactionSummary"
)

// Message is immutable by convention: constructors copy slice fields and all
// accessors return fresh values.  Applications may add their own custom value
// through OpaqueMessage; its optional LLM projection is explicit.
type Message interface {
	Role() Role
	Timestamp() time.Time
	cloneMessage() Message
}

// LLM wraps one standard provider-visible conversation message.
type LLM struct{ value llm.ConversationMessage }

func NewLLM(value llm.ConversationMessage) (LLM, error) {
	if err := llm.ValidateConversationMessage(value); err != nil {
		return LLM{}, err
	}
	return LLM{value: value}, nil
}
func (m LLM) Role() Role {
	switch m.value.Role() {
	case llm.RoleUser:
		return RoleUser
	case llm.RoleAssistant:
		return RoleAssistant
	case llm.RoleToolResult:
		return RoleToolResult
	default:
		return ""
	}
}
func (m LLM) Timestamp() time.Time                  { return m.value.Timestamp() }
func (m LLM) Conversation() llm.ConversationMessage { return m.value }
func (m LLM) cloneMessage() Message                 { return m }

// AssistantPartial is the provider-streaming form of an assistant message.
// It is deliberately not a durable/provider-context message: Agent emits it
// only at message_start/message_update boundaries and replaces it with a
// terminal LLM message before persistence.
type AssistantPartial struct {
	snapshot llm.StreamSnapshot
	event    llm.StreamEvent
	api      string
	provider string
	model    string
	usage    llm.Usage
	at       time.Time
}

type AssistantPartialSpec struct {
	Snapshot llm.StreamSnapshot
	Event    llm.StreamEvent
	API      string
	Provider string
	Model    string
	At       time.Time
}

func NewAssistantPartial(spec AssistantPartialSpec) (AssistantPartial, error) {
	if spec.Event == nil || spec.Snapshot.Terminal() || spec.Snapshot.FinishReason() != llm.FinishPending {
		return AssistantPartial{}, fmt.Errorf("invalid partial assistant message")
	}
	if !utf8.ValidString(spec.API) || !utf8.ValidString(spec.Provider) || !utf8.ValidString(spec.Model) ||
		strings.TrimSpace(spec.API) == "" || strings.TrimSpace(spec.Provider) == "" || strings.TrimSpace(spec.Model) == "" {
		return AssistantPartial{}, fmt.Errorf("invalid partial assistant provenance")
	}
	zeroCost := llm.Cost{}
	usage, err := llm.NewUsage(llm.UsageSpec{Cost: &zeroCost})
	if err != nil {
		return AssistantPartial{}, fmt.Errorf("invalid partial assistant usage: %w", err)
	}
	return AssistantPartial{
		snapshot: spec.Snapshot,
		event:    spec.Event,
		api:      spec.API,
		provider: spec.Provider,
		model:    spec.Model,
		usage:    usage,
		at:       spec.At,
	}, nil
}
func (m AssistantPartial) Role() Role                     { return RoleAssistant }
func (m AssistantPartial) Timestamp() time.Time           { return m.at }
func (m AssistantPartial) Snapshot() llm.StreamSnapshot   { return m.snapshot }
func (m AssistantPartial) ProviderEvent() llm.StreamEvent { return m.event }
func (m AssistantPartial) API() string                    { return m.api }
func (m AssistantPartial) Provider() string               { return m.provider }
func (m AssistantPartial) Model() string                  { return m.model }
func (m AssistantPartial) Usage() llm.Usage               { return m.usage }
func (m AssistantPartial) FinishReason() llm.FinishReason { return llm.FinishPending }
func (m AssistantPartial) Blocks() []llm.AssistantBlock   { return m.snapshot.Blocks() }
func (m AssistantPartial) cloneMessage() Message          { return m }

type BashExecution struct {
	Command            string
	Output             string
	ExitCode           *int
	Cancelled          bool
	Truncated          bool
	FullOutputPath     string
	ExcludeFromContext bool
	At                 time.Time
}

func NewBashExecution(value BashExecution) (BashExecution, error) {
	if !utf8.ValidString(value.Command) || !utf8.ValidString(value.Output) || !utf8.ValidString(value.FullOutputPath) || strings.TrimSpace(value.Command) == "" {
		return BashExecution{}, fmt.Errorf("invalid bash execution message")
	}
	if value.ExitCode != nil {
		copy := *value.ExitCode
		value.ExitCode = &copy
	}
	return value, nil
}
func (m BashExecution) Role() Role           { return RoleBashExecution }
func (m BashExecution) Timestamp() time.Time { return m.At }
func (m BashExecution) cloneMessage() Message {
	if m.ExitCode != nil {
		value := *m.ExitCode
		m.ExitCode = &value
	}
	return m
}
func (m BashExecution) Text() string {
	text := "Ran `" + m.Command + "`\n"
	if m.Output != "" {
		text += "```\n" + m.Output + "\n```"
	} else {
		text += "(no output)"
	}
	if m.Cancelled {
		text += "\n\n(command cancelled)"
	} else if m.ExitCode != nil && *m.ExitCode != 0 {
		text += fmt.Sprintf("\n\nCommand exited with code %d", *m.ExitCode)
	}
	if m.Truncated && m.FullOutputPath != "" {
		text += "\n\n[Output truncated. Full output: " + m.FullOutputPath + "]"
	}
	return text
}

// CustomSpec is the mutable constructor boundary. Custom itself keeps one
// tagged payload so its string wire form and rich provider projection cannot
// be changed independently after construction.
type CustomSpec struct {
	CustomType    string
	Content       []llm.UserContentBlock
	StringContent *string
	Display       bool
	Details       []byte
	At            time.Time
}

type customContentKind uint8

const (
	customContentRich customContentKind = iota + 1
	customContentString
)

type Custom struct {
	customType  string
	contentKind customContentKind
	content     []llm.UserContentBlock
	text        string
	display     bool
	details     []byte
	at          time.Time
}

func NewCustom(value CustomSpec) (Custom, error) {
	if !utf8.ValidString(value.CustomType) || strings.TrimSpace(value.CustomType) == "" {
		return Custom{}, fmt.Errorf("invalid custom message type")
	}
	for _, block := range value.Content {
		if !validUserContent(block) {
			return Custom{}, fmt.Errorf("invalid custom message content")
		}
	}
	if value.StringContent != nil && len(value.Content) != 0 {
		return Custom{}, fmt.Errorf("custom message must choose string or rich content")
	}
	result := Custom{customType: value.CustomType, display: value.Display, at: value.At}
	if value.StringContent != nil {
		if !utf8.ValidString(*value.StringContent) {
			return Custom{}, fmt.Errorf("invalid custom message string content")
		}
		result.contentKind = customContentString
		result.text = *value.StringContent
	} else {
		result.contentKind = customContentRich
		result.content = append([]llm.UserContentBlock(nil), value.Content...)
	}
	if len(value.Details) != 0 && !jsonValid(value.Details) {
		return Custom{}, fmt.Errorf("invalid custom message details")
	}
	result.details = append([]byte(nil), value.Details...)
	return result, nil
}

// NewCustomText preserves pi's string shorthand. Content derives a temporary
// rich projection only at the provider boundary; the durable wire form remains
// the original string.
func NewCustomText(customType, text string, display bool, details []byte, at time.Time) (Custom, error) {
	return NewCustom(CustomSpec{CustomType: customType, StringContent: &text, Display: display, Details: details, At: at})
}
func (m Custom) Role() Role           { return RoleCustom }
func (m Custom) Timestamp() time.Time { return m.at }
func (m Custom) CustomType() string   { return m.customType }
func (m Custom) Display() bool        { return m.display }
func (m Custom) Details() []byte      { return append([]byte(nil), m.details...) }
func (m Custom) StringContent() (string, bool) {
	return m.text, m.contentKind == customContentString
}
func (m Custom) Content() []llm.UserContentBlock {
	if m.contentKind == customContentString {
		block, err := llm.NewTextBlock(m.text)
		if err != nil {
			return nil
		}
		return []llm.UserContentBlock{block}
	}
	return append([]llm.UserContentBlock(nil), m.content...)
}
func (m Custom) cloneMessage() Message {
	m.content = append([]llm.UserContentBlock(nil), m.content...)
	m.details = append([]byte(nil), m.details...)
	return m
}

type BranchSummary struct {
	Summary, FromID string
	At              time.Time
}

func NewBranchSummary(value BranchSummary) (BranchSummary, error) {
	if !validSummary(value.Summary) || !validID(value.FromID) {
		return BranchSummary{}, fmt.Errorf("invalid branch summary")
	}
	return value, nil
}
func (m BranchSummary) Role() Role            { return RoleBranchSummary }
func (m BranchSummary) Timestamp() time.Time  { return m.At }
func (m BranchSummary) cloneMessage() Message { return m }

type CompactionSummary struct {
	Summary      string
	TokensBefore uint64
	At           time.Time
}

func NewCompactionSummary(value CompactionSummary) (CompactionSummary, error) {
	if !validSummary(value.Summary) {
		return CompactionSummary{}, fmt.Errorf("invalid compaction summary")
	}
	return value, nil
}
func (m CompactionSummary) Role() Role            { return RoleCompactionSummary }
func (m CompactionSummary) Timestamp() time.Time  { return m.At }
func (m CompactionSummary) cloneMessage() Message { return m }

// OpaqueMessage is the extension-neutral durable escape hatch for an unknown
// AgentMessage union member. Data is the sole source of truth: Session writes
// it byte-for-byte and reopens the same role. Unknown roles do not enter LLM
// context until a context hook deliberately replaces them with a known value.
type OpaqueSpec struct {
	Type string
	Data []byte
	At   time.Time
}

type OpaqueMessage struct {
	typ  string
	data []byte
	at   time.Time
}

func NewOpaque(value OpaqueSpec) (OpaqueMessage, error) {
	if !validID(value.Type) || len(value.Data) == 0 || !jsonValid(value.Data) {
		return OpaqueMessage{}, fmt.Errorf("invalid opaque agent message")
	}
	var envelope struct {
		Role      string `json:"role"`
		Timestamp *int64 `json:"timestamp"`
	}
	if json.Unmarshal(value.Data, &envelope) != nil || envelope.Role != value.Type || envelope.Timestamp == nil {
		return OpaqueMessage{}, fmt.Errorf("opaque role does not match message type")
	}
	fromData := time.UnixMilli(*envelope.Timestamp)
	if !value.At.IsZero() && !value.At.Equal(fromData) {
		return OpaqueMessage{}, fmt.Errorf("opaque timestamp does not match durable data")
	}
	return OpaqueMessage{typ: value.Type, data: append([]byte(nil), value.Data...), at: fromData}, nil
}
func (m OpaqueMessage) Role() Role           { return Role(m.typ) }
func (m OpaqueMessage) Timestamp() time.Time { return m.at }
func (m OpaqueMessage) Type() string         { return m.typ }
func (m OpaqueMessage) Data() []byte         { return append([]byte(nil), m.data...) }
func (m OpaqueMessage) cloneMessage() Message {
	m.data = append([]byte(nil), m.data...)
	return m
}

func Clone(messages []Message) []Message {
	out := make([]Message, len(messages))
	for i, m := range messages {
		if m != nil {
			out[i] = m.cloneMessage()
		}
	}
	return out
}
func CloneOne(message Message) Message {
	if message == nil {
		return nil
	}
	return message.cloneMessage()
}

// ConvertToLLM is the only boundary that turns coding-agent messages into
// provider context.  It never flattens rich content or custom details.
func ConvertToLLM(messages []Message) ([]llm.ConversationMessage, error) {
	out := make([]llm.ConversationMessage, 0, len(messages))
	for _, message := range messages {
		switch value := message.(type) {
		case LLM:
			out = append(out, value.Conversation())
		case BashExecution:
			if !value.ExcludeFromContext {
				converted, err := llm.NewUserTextMessage(value.Text(), value.At)
				if err != nil {
					return nil, err
				}
				out = append(out, converted)
			}
		case Custom:
			converted, err := llm.NewUserContentMessage(value.Content(), value.Timestamp())
			if err != nil {
				return nil, err
			}
			out = append(out, converted)
		case BranchSummary:
			converted, err := llm.NewUserTextMessage(BranchSummaryPrefix+value.Summary+BranchSummarySuffix, value.At)
			if err != nil {
				return nil, err
			}
			out = append(out, converted)
		case CompactionSummary:
			converted, err := llm.NewUserTextMessage(CompactionSummaryPrefix+value.Summary+CompactionSummarySuffix, value.At)
			if err != nil {
				return nil, err
			}
			out = append(out, converted)
		case OpaqueMessage:
			// Unknown union members are durable but provider-invisible by default.
			// A ContextHook can replace one with a known custom/LLM message.
		case AssistantPartial:
			return nil, fmt.Errorf("partial assistant message cannot enter provider context")
		default:
			return nil, fmt.Errorf("unsupported agent message %T", message)
		}
	}
	return out, nil
}

func validUserContent(block llm.UserContentBlock) bool {
	switch block.(type) {
	case llm.TextBlock, llm.ImageBlock:
		return true
	default:
		return false
	}
}
func validSummary(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != ""
}
func validID(value string) bool   { return utf8.ValidString(value) && strings.TrimSpace(value) != "" }
func jsonValid(value []byte) bool { return json.Valid(value) }

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

type Custom struct {
	CustomType string
	Content    []llm.UserContentBlock
	// StringContent preserves pi's string-vs-blocks wire distinction. When set,
	// Content contains the canonical one-text-block projection as well.
	StringContent *string
	Display       bool
	Details       []byte // JSON; intentionally not sent to the LLM
	At            time.Time
}

func NewCustom(value Custom) (Custom, error) {
	if !utf8.ValidString(value.CustomType) || strings.TrimSpace(value.CustomType) == "" {
		return Custom{}, fmt.Errorf("invalid custom message type")
	}
	for _, block := range value.Content {
		if !validUserContent(block) {
			return Custom{}, fmt.Errorf("invalid custom message content")
		}
	}
	if value.StringContent != nil {
		if !utf8.ValidString(*value.StringContent) {
			return Custom{}, fmt.Errorf("invalid custom message string content")
		}
		copy := *value.StringContent
		value.StringContent = &copy
		if len(value.Content) == 0 {
			block, err := llm.NewTextBlock(copy)
			if err != nil {
				return Custom{}, err
			}
			value.Content = []llm.UserContentBlock{block}
		} else {
			text, ok := value.Content[0].(llm.TextBlock)
			if len(value.Content) != 1 || !ok || text.Text() != copy {
				return Custom{}, fmt.Errorf("custom string content conflicts with rich content")
			}
		}
	}
	if len(value.Details) != 0 && !jsonValid(value.Details) {
		return Custom{}, fmt.Errorf("invalid custom message details")
	}
	value.Content = append([]llm.UserContentBlock(nil), value.Content...)
	value.Details = append([]byte(nil), value.Details...)
	return value, nil
}

// NewCustomText preserves pi's string shorthand while normalizing it to the
// canonical rich-content form used by the Go provider boundary.
func NewCustomText(customType, text string, display bool, details []byte, at time.Time) (Custom, error) {
	block, err := llm.NewTextBlock(text)
	if err != nil {
		return Custom{}, err
	}
	return NewCustom(Custom{CustomType: customType, Content: []llm.UserContentBlock{block}, StringContent: &text, Display: display, Details: details, At: at})
}
func (m Custom) Role() Role           { return RoleCustom }
func (m Custom) Timestamp() time.Time { return m.At }
func (m Custom) cloneMessage() Message {
	m.Content = append([]llm.UserContentBlock(nil), m.Content...)
	m.Details = append([]byte(nil), m.Details...)
	if m.StringContent != nil {
		value := *m.StringContent
		m.StringContent = &value
	}
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
type OpaqueMessage struct {
	Type string
	Data []byte
	At   time.Time
}

func NewOpaque(value OpaqueMessage) (OpaqueMessage, error) {
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
	value.At = fromData
	value.Data = append([]byte(nil), value.Data...)
	return value, nil
}
func (m OpaqueMessage) Role() Role           { return Role(m.Type) }
func (m OpaqueMessage) Timestamp() time.Time { return m.At }
func (m OpaqueMessage) cloneMessage() Message {
	m.Data = append([]byte(nil), m.Data...)
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
			converted, err := llm.NewUserContentMessage(value.Content, value.At)
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

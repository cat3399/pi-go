package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

var (
	ErrInvalidSession        = errors.New("invalid session")
	ErrUnsupportedVersion    = errors.New("unsupported session version")
	ErrInvalidEntry          = errors.New("invalid session entry")
	ErrUnsupportedTree       = errors.New("unsupported session tree")
	ErrStorage               = errors.New("session storage failure")
	ErrDurabilityUnknown     = errors.New("session creation durability unknown")
	ErrCommitUnknown         = errors.New("session append commit outcome unknown")
	ErrAppendCanceled        = errors.New("session append canceled before commit")
	ErrPoisoned              = errors.New("session writer is poisoned")
	ErrClosed                = errors.New("session is closed")
	ErrWriterActive          = errors.New("session already has an active writer")
	ErrUnsafeWriterAlias     = errors.New("session writer alias cannot be made safe")
	ErrIDGeneration          = errors.New("session id generation failed")
	ErrEntryIDExhausted      = errors.New("unique session entry id exhausted")
	ErrEntryNotFound         = errors.New("session entry not found")
	ErrSourceEqualsTarget    = errors.New("session extraction source and target are the same file")
	ErrNothingToCompact      = errors.New("session has no compactable context")
	ErrAlreadyCompacted      = errors.New("session leaf is already a compaction entry")
	ErrCompactionConflict    = errors.New("session changed while compaction summary was in progress")
	ErrSummaryFailed         = errors.New("session compaction summary failed")
	ErrTokenEstimateOverflow = errors.New("session token estimate overflow")
	ErrRecoveryNotApplicable = errors.New("session recovery is not applicable")
	ErrRecoveryBackupExists  = errors.New("session recovery backup already exists")
	ErrSessionTooLarge       = errors.New("session exceeds safety limit")
)

// ErrAtomicReplaceUnsupported reports that the host cannot replace an open
// session target atomically. The operation fails before publication.
var ErrAtomicReplaceUnsupported = errors.New("atomic session replacement unsupported")

// CompactionSummaryPrefix and CompactionSummarySuffix are the v3 context
// representation of a durable compaction record. They make the checkpoint an
// explicit user-context message without changing the sealed llm message union.
const (
	CompactionSummaryPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	CompactionSummarySuffix = "\n</summary>"
)

// Clock and IDGenerator are injected at the module boundary so session tests
// and the deterministic agent workflow do not depend on wall clock or entropy.
type Clock func() time.Time
type IDGenerator func() (string, error)

type CreateOptions struct {
	ID         string
	WorkingDir string
	// ParentSession records the source file when this session was extracted or
	// forked. It is metadata only; the source is never opened for writing.
	ParentSession string
	Now           Clock
	NewEntryID    IDGenerator
}

type OpenOptions struct {
	Now        Clock
	NewEntryID IDGenerator
}

// RecoveryResult describes an explicit, user-requested repair. Ordinary Open
// never calls this operation: malformed history is evidence, not disposable
// whitespace. Recovery only removes one unterminated, non-JSON final line.
type RecoveryResult struct {
	BackupPath     string
	TruncatedBytes int64
}

// AssistantProvenance supplies the coding-agent v3 storage fields for the
// selected provider/model. Provider/API/model are also projected into the
// typed llm replay provenance; Cost remains a session-boundary value.
type AssistantProvenance struct {
	API      string
	Provider string
	Model    string
	Cost     UsageCost
}

// UsageCost is a storage-boundary value. json.Number preserves the provider's
// already-normalized decimal spelling; M-SESSION validates and persists it but
// deliberately does not define pricing, rounding, or arithmetic semantics.
type UsageCost struct {
	Input      json.Number `json:"input"`
	Output     json.Number `json:"output"`
	CacheRead  json.Number `json:"cacheRead"`
	CacheWrite json.Number `json:"cacheWrite"`
	Total      json.Number `json:"total"`
}

func ZeroUsageCost() UsageCost {
	return UsageCost{Input: "0", Output: "0", CacheRead: "0", CacheWrite: "0", Total: "0"}
}

type AppendOptions struct {
	Assistant AssistantProvenance
}

// CompactionUsage keeps the provider-normalized usage and the coding-agent v3
// cost object together. Pricing remains outside Session; the value is only
// validated and durably preserved here.
type CompactionUsage struct {
	Usage llm.Usage
	Cost  UsageCost
}

// SummaryInput is an immutable, selected-branch snapshot delivered to the
// injected summarizer. The conversation is already serialized so a provider
// cannot mistake it for a continuation request.
type SummaryInput struct {
	SystemPrompt     string
	Prompt           string
	Instructions     string
	PreviousSummary  string
	Messages         []llm.ConversationMessage
	RetainedTail     []llm.ConversationMessage
	FirstKeptEntryID string
	TokensBefore     uint64
	Generation       uint64
	SelectedLeafID   string
}

// SummaryOutput is the only data a summarizer may contribute to durable
// session state. Empty or invalid text is rejected before any file write.
type SummaryOutput struct {
	Text  string
	Usage *CompactionUsage
}

// Summarizer is deliberately narrow: provider routing, retries and UI events
// belong to a later agent/application integration layer.
type Summarizer interface {
	Summarize(context.Context, SummaryInput) (SummaryOutput, error)
}

// CompactRequest starts one manual context-compaction operation. A zero keep
// budget selects the standard 20k-token retention policy; callers can pass a
// smaller explicit value for deterministic/manual control.
type CompactRequest struct {
	KeepRecentTokens uint64
	Instructions     string
	Summarizer       Summarizer
}

// CompactResult reports the durable record and the immutable request snapshot
// that produced it. It never implies that an external provider ran under the
// session lock.
type CompactResult struct {
	Entry     Entry
	Input     SummaryInput
	Committed bool
}

type Header struct {
	id               string
	workingDir       string
	parentSession    string
	hasParentSession bool
	timestamp        time.Time
	raw              []byte
}

func (h Header) ID() string                    { return h.id }
func (h Header) WorkingDir() string            { return h.workingDir }
func (h Header) ParentSession() (string, bool) { return h.parentSession, h.hasParentSession }
func (h Header) Timestamp() time.Time          { return h.timestamp }
func (h Header) Version() int                  { return 3 }
func (h Header) RawJSON() []byte               { return bytes.Clone(h.raw) }
func (h Header) clone() Header                 { h.raw = bytes.Clone(h.raw); return h }

type DiagnosticCode string

const (
	DiagnosticUnknownEntry         DiagnosticCode = "unknown-entry"
	DiagnosticUnknownMessageRole   DiagnosticCode = "unknown-message-role"
	DiagnosticUnknownContentBlock  DiagnosticCode = "unknown-content-block"
	DiagnosticUnprojectableMessage DiagnosticCode = "unprojectable-message"
	DiagnosticUnsafeContentOmitted DiagnosticCode = "unsafe-content-omitted"
)

// Diagnostic deliberately contains no raw JSON or arbitrary upstream text.
// ContentIndex is -1 when the diagnostic applies to the whole entry/message.
type Diagnostic struct {
	Code         DiagnosticCode
	EntryID      string
	ContentIndex int
}

type Entry struct {
	id           string
	parentID     string
	hasParent    bool
	timestamp    time.Time
	typeName     string
	raw          []byte
	message      llm.ConversationMessage
	assistant    AssistantProvenance
	hasAssistant bool
	compaction   *CompactionRecord
	diagnostics  []Diagnostic
	payload      EntryPayload
}

// EntryPayload is the typed v3 session-entry union.  Entry.RawJSON remains
// available for forward compatibility, while this projection gives later
// SessionManager work a complete, non-stringly-typed foundation.
type EntryPayload interface {
	entryPayload()
	CloneEntryPayload() EntryPayload
}
type MessagePayload struct{ Message agentmsg.Message }

func (MessagePayload) entryPayload() {}
func (p MessagePayload) CloneEntryPayload() EntryPayload {
	if p.Message != nil {
		p.Message = agentmsg.CloneOne(p.Message)
	}
	return p
}

type ThinkingLevelChangePayload struct{ ThinkingLevel string }

func (ThinkingLevelChangePayload) entryPayload()                     {}
func (p ThinkingLevelChangePayload) CloneEntryPayload() EntryPayload { return p }

type ModelChangePayload struct {
	Provider, ModelID string
	HasModelID        bool
}

func (ModelChangePayload) entryPayload()                     {}
func (p ModelChangePayload) CloneEntryPayload() EntryPayload { return p }

type BranchSummaryPayload struct {
	FromID, Summary string
	Details         json.RawMessage
	Usage           *CompactionUsage
	FromHook        bool
	HasFromHook     bool
}

func (BranchSummaryPayload) entryPayload() {}
func (p BranchSummaryPayload) CloneEntryPayload() EntryPayload {
	p.Details = bytes.Clone(p.Details)
	if p.Usage != nil {
		u := *p.Usage
		p.Usage = &u
	}
	return p
}

type CustomPayload struct {
	CustomType string
	Data       json.RawMessage
}

func (CustomPayload) entryPayload()                     {}
func (p CustomPayload) CloneEntryPayload() EntryPayload { p.Data = bytes.Clone(p.Data); return p }

type CustomMessagePayload struct{ Message agentmsg.Custom }

func (CustomMessagePayload) entryPayload() {}
func (p CustomMessagePayload) CloneEntryPayload() EntryPayload {
	clone, _ := agentmsg.NewCustom(p.Message)
	p.Message = clone
	return p
}

type LabelPayload struct {
	TargetID string
	Label    *string
}

func (LabelPayload) entryPayload() {}
func (p LabelPayload) CloneEntryPayload() EntryPayload {
	if p.Label != nil {
		x := *p.Label
		p.Label = &x
	}
	return p
}

type SessionInfoPayload struct{ Name *string }

func (SessionInfoPayload) entryPayload() {}
func (p SessionInfoPayload) CloneEntryPayload() EntryPayload {
	if p.Name != nil {
		x := *p.Name
		p.Name = &x
	}
	return p
}

type CompactionPayload struct {
	Record      CompactionRecord
	Details     json.RawMessage
	FromHook    bool
	HasFromHook bool
}

func (CompactionPayload) entryPayload() {}
func (p CompactionPayload) CloneEntryPayload() EntryPayload {
	p.Details = bytes.Clone(p.Details)
	return p.Record.clonePayload(p)
}

// CompactionRecord is the recognized v3 compaction entry payload. Details and
// future fields remain in Entry.RawJSON; this typed view contains only the
// fields needed for safe context projection.
type CompactionRecord struct {
	Summary          string
	FirstKeptEntryID string
	TokensBefore     uint64
	Usage            *CompactionUsage
}

func (r CompactionRecord) clonePayload(p CompactionPayload) EntryPayload {
	if r.Usage != nil {
		u := *r.Usage
		r.Usage = &u
	}
	p.Record = r
	return p
}

// TreeNode is an immutable snapshot of the durable forest. Children preserve
// JSONL append order; this is the only unambiguous ordering for equal clocks.
type TreeNode struct {
	Entry    Entry
	Children []TreeNode
}

func (n TreeNode) clone() TreeNode {
	n.Entry = n.Entry.clone()
	n.Children = append([]TreeNode(nil), n.Children...)
	for index := range n.Children {
		n.Children[index] = n.Children[index].clone()
	}
	return n
}

// ExtractOptions names the durable target and its new header. TargetPath must
// not already exist. The selected source path is copied without mutation.
type ExtractOptions struct {
	TargetPath string
	ID         string
	WorkingDir string
	Now        Clock
	NewEntryID IDGenerator
}

func (e Entry) ID() string           { return e.id }
func (e Entry) Type() string         { return e.typeName }
func (e Entry) Timestamp() time.Time { return e.timestamp }
func (e Entry) ParentID() (string, bool) {
	return e.parentID, e.hasParent
}
func (e Entry) RawJSON() []byte { return bytes.Clone(e.raw) }
func (e Entry) Message() (llm.ConversationMessage, bool) {
	return e.message, e.message != nil
}
func (e Entry) Payload() EntryPayload {
	if e.payload == nil {
		return nil
	}
	return e.payload.CloneEntryPayload()
}
func (e Entry) AgentMessage() (agentmsg.Message, bool) {
	if payload, ok := e.payload.(MessagePayload); ok && payload.Message != nil {
		return agentmsg.CloneOne(payload.Message), true
	}
	if payload, ok := e.payload.(CustomMessagePayload); ok {
		return agentmsg.CloneOne(payload.Message), true
	}
	return nil, false
}
func (e Entry) AssistantProvenance() (AssistantProvenance, bool) {
	return e.assistant, e.hasAssistant
}
func (e Entry) Compaction() (CompactionRecord, bool) {
	if e.compaction == nil {
		return CompactionRecord{}, false
	}
	value := *e.compaction
	if value.Usage != nil {
		usage := *value.Usage
		value.Usage = &usage
	}
	return value, true
}
func (e Entry) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), e.diagnostics...)
}
func (e Entry) clone() Entry {
	e.raw = bytes.Clone(e.raw)
	e.diagnostics = append([]Diagnostic(nil), e.diagnostics...)
	if e.compaction != nil {
		compaction := *e.compaction
		if compaction.Usage != nil {
			usage := *compaction.Usage
			compaction.Usage = &usage
		}
		e.compaction = &compaction
	}
	if e.payload != nil {
		e.payload = e.payload.CloneEntryPayload()
	}
	return e
}

type Context struct {
	messages      []llm.ConversationMessage
	agentMessages []agentmsg.Message
	diagnostics   []Diagnostic
	assistant     AssistantProvenance
	hasAssistant  bool
}

// NewContext constructs an in-memory runtime projection. It is intentionally
// separate from Session's durable selected-branch projection.
func NewContext(messages []llm.ConversationMessage) Context {
	agentMessages := make([]agentmsg.Message, 0, len(messages))
	for _, message := range messages {
		wrapped, err := agentmsg.NewLLM(message)
		if err == nil {
			agentMessages = append(agentMessages, wrapped)
		}
	}
	return Context{messages: append([]llm.ConversationMessage(nil), messages...), agentMessages: agentmsg.Clone(agentMessages)}
}

func (c Context) Messages() []llm.ConversationMessage {
	return append([]llm.ConversationMessage(nil), c.messages...)
}
func (c Context) AgentMessages() []agentmsg.Message { return agentmsg.Clone(c.agentMessages) }

func (c Context) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), c.diagnostics...)
}

// AssistantProvenance returns the last assistant provenance on the active path.
func (c Context) AssistantProvenance() (AssistantProvenance, bool) {
	return c.assistant, c.hasAssistant
}

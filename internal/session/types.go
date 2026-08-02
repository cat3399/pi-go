package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

var (
	ErrInvalidSession     = errors.New("invalid session")
	ErrUnsupportedVersion = errors.New("unsupported session version")
	ErrInvalidEntry       = errors.New("invalid session entry")
	ErrUnsupportedTree    = errors.New("unsupported session tree")
	ErrStorage            = errors.New("session storage failure")
	ErrDurabilityUnknown  = errors.New("session creation durability unknown")
	ErrCommitUnknown      = errors.New("session append commit outcome unknown")
	ErrAppendCanceled     = errors.New("session append canceled before commit")
	ErrPoisoned           = errors.New("session writer is poisoned")
	ErrClosed             = errors.New("session is closed")
	ErrWriterActive       = errors.New("session already has an active writer")
	ErrIDGeneration       = errors.New("session id generation failed")
	ErrEntryIDExhausted   = errors.New("unique session entry id exhausted")
	ErrEntryNotFound      = errors.New("session entry not found")
	ErrSourceEqualsTarget = errors.New("session extraction source and target are the same file")
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

// AssistantProvenance supplies the coding-agent v3 fields that belong to the
// selected provider/model, not to the shared llm message domain.
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
	diagnostics  []Diagnostic
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
func (e Entry) AssistantProvenance() (AssistantProvenance, bool) {
	return e.assistant, e.hasAssistant
}
func (e Entry) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), e.diagnostics...)
}
func (e Entry) clone() Entry {
	e.raw = bytes.Clone(e.raw)
	e.diagnostics = append([]Diagnostic(nil), e.diagnostics...)
	return e
}

type Context struct {
	messages     []llm.ConversationMessage
	diagnostics  []Diagnostic
	assistant    AssistantProvenance
	hasAssistant bool
}

func (c Context) Messages() []llm.ConversationMessage {
	return append([]llm.ConversationMessage(nil), c.messages...)
}

func (c Context) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), c.diagnostics...)
}

// AssistantProvenance returns the last assistant provenance on the active path.
func (c Context) AssistantProvenance() (AssistantProvenance, bool) {
	return c.assistant, c.hasAssistant
}

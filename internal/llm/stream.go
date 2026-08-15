package llm

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalidStreamEvent = errors.New("invalid stream event")
	ErrStreamProtocol     = errors.New("stream protocol error")
	ErrUnexpectedEOF      = errors.New("unexpected end of stream")
	ErrDuplicateTerminal  = errors.New("duplicate terminal event")
	ErrStreamNotStarted   = errors.New("stream has not started")
	ErrStreamNotClosed    = errors.New("stream is not closed")
	ErrStreamClosed       = errors.New("stream is closed")
)

type streamProtocolError struct {
	cause  error
	detail string
}

func (e *streamProtocolError) Error() string {
	if e.cause == nil {
		return fmt.Sprintf("%s: %s", ErrStreamProtocol, e.detail)
	}
	return fmt.Sprintf("%s: %s: %s", ErrStreamProtocol, e.cause, e.detail)
}

func (e *streamProtocolError) Is(target error) bool {
	return target == ErrStreamProtocol || (e.cause != nil && errors.Is(e.cause, target))
}

// StreamEvent is the sealed event vocabulary accepted by StreamCollector.
type StreamEvent interface {
	streamEvent()
}

type StartEvent struct {
	provenance AssistantProvenance
	timestamp  time.Time
}

func NewStartEvent(provenance AssistantProvenance, timestamp time.Time) (StartEvent, error) {
	event := StartEvent{provenance: provenance, timestamp: normalizeStreamTimestamp(timestamp)}
	if err := event.validate(); err != nil {
		return StartEvent{}, err
	}
	return event, nil
}

func (StartEvent) streamEvent() {}
func (e StartEvent) validate() error {
	if err := e.provenance.validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidStreamEvent, err)
	}
	if e.timestamp.IsZero() || !time.UnixMilli(e.timestamp.UnixMilli()).Equal(e.timestamp) {
		return fmt.Errorf("%w: start timestamp", ErrInvalidStreamEvent)
	}
	return nil
}
func (e StartEvent) AssistantProvenance() AssistantProvenance { return e.provenance }
func (e StartEvent) Timestamp() time.Time                     { return e.timestamp }

type TextStartEvent struct {
	contentIndex int
}

func NewTextStartEvent(contentIndex int) (TextStartEvent, error) {
	event := TextStartEvent{contentIndex: contentIndex}
	if err := event.validate(); err != nil {
		return TextStartEvent{}, err
	}
	return event, nil
}

func (TextStartEvent) streamEvent() {}

func (e TextStartEvent) validate() error {
	if e.contentIndex < 0 {
		return fmt.Errorf("%w: negative content index", ErrInvalidStreamEvent)
	}
	return nil
}

func (e TextStartEvent) ContentIndex() int {
	return e.contentIndex
}

type TextDeltaEvent struct {
	contentIndex int
	delta        string
}

func NewTextDeltaEvent(contentIndex int, delta string) (TextDeltaEvent, error) {
	event := TextDeltaEvent{contentIndex: contentIndex, delta: delta}
	if err := event.validate(); err != nil {
		return TextDeltaEvent{}, err
	}
	return event, nil
}

func (TextDeltaEvent) streamEvent() {}

func (e TextDeltaEvent) validate() error {
	if e.contentIndex < 0 {
		return fmt.Errorf("%w: negative content index", ErrInvalidStreamEvent)
	}
	if !utf8.ValidString(e.delta) {
		return fmt.Errorf("%w: delta is not valid UTF-8", ErrInvalidStreamEvent)
	}
	return nil
}

func (e TextDeltaEvent) ContentIndex() int {
	return e.contentIndex
}

func (e TextDeltaEvent) Delta() string {
	return e.delta
}

type TextEndEvent struct {
	contentIndex  int
	content       string
	textSignature string
}

// Thinking events mirror text events but retain their opaque replay handle at
// end-of-block. Providers must not expose an encrypted signature before the
// item is complete.
type ThinkingStartEvent struct{ contentIndex int }

func NewThinkingStartEvent(contentIndex int) (ThinkingStartEvent, error) {
	e := ThinkingStartEvent{contentIndex}
	return e, e.validate()
}
func (ThinkingStartEvent) streamEvent() {}
func (e ThinkingStartEvent) validate() error {
	if e.contentIndex < 0 {
		return fmt.Errorf("%w: negative content index", ErrInvalidStreamEvent)
	}
	return nil
}
func (e ThinkingStartEvent) ContentIndex() int { return e.contentIndex }

type ThinkingDeltaEvent struct {
	contentIndex int
	delta        string
}

func NewThinkingDeltaEvent(contentIndex int, delta string) (ThinkingDeltaEvent, error) {
	e := ThinkingDeltaEvent{contentIndex, delta}
	return e, e.validate()
}
func (ThinkingDeltaEvent) streamEvent() {}
func (e ThinkingDeltaEvent) validate() error {
	if e.contentIndex < 0 || !utf8.ValidString(e.delta) {
		return fmt.Errorf("%w: invalid thinking delta", ErrInvalidStreamEvent)
	}
	return nil
}
func (e ThinkingDeltaEvent) ContentIndex() int { return e.contentIndex }
func (e ThinkingDeltaEvent) Delta() string     { return e.delta }

type ThinkingEndEvent struct {
	contentIndex int
	content      ThinkingBlock
}

func NewThinkingEndEvent(contentIndex int, content ThinkingBlock) (ThinkingEndEvent, error) {
	e := ThinkingEndEvent{contentIndex, content}
	return e, e.validate()
}
func (ThinkingEndEvent) streamEvent() {}
func (e ThinkingEndEvent) validate() error {
	if e.contentIndex < 0 {
		return fmt.Errorf("%w: negative content index", ErrInvalidStreamEvent)
	}
	if err := e.content.validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidStreamEvent, err)
	}
	return nil
}
func (e ThinkingEndEvent) ContentIndex() int      { return e.contentIndex }
func (e ThinkingEndEvent) Content() ThinkingBlock { return e.content }

func NewTextEndEvent(contentIndex int, content string) (TextEndEvent, error) {
	return NewTextEndEventWithSignature(contentIndex, content, "")
}
func NewTextEndEventWithSignature(contentIndex int, content, signature string) (TextEndEvent, error) {
	event := TextEndEvent{contentIndex: contentIndex, content: content, textSignature: signature}
	if err := event.validate(); err != nil {
		return TextEndEvent{}, err
	}
	return event, nil
}

func (TextEndEvent) streamEvent() {}

func (e TextEndEvent) validate() error {
	if e.contentIndex < 0 {
		return fmt.Errorf("%w: negative content index", ErrInvalidStreamEvent)
	}
	if !utf8.ValidString(e.content) {
		return fmt.Errorf("%w: content is not valid UTF-8", ErrInvalidStreamEvent)
	}
	if err := validateOpaqueSignature(e.textSignature); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidStreamEvent, err)
	}
	return nil
}

func (e TextEndEvent) ContentIndex() int {
	return e.contentIndex
}

func (e TextEndEvent) Content() string {
	return e.content
}
func (e TextEndEvent) TextSignature() (string, bool) {
	return e.textSignature, e.textSignature != ""
}

type ToolCallStartEvent struct {
	contentIndex int
	id           string
	name         string
}

func NewToolCallStartEvent(contentIndex int, id, name string) (ToolCallStartEvent, error) {
	event := ToolCallStartEvent{contentIndex: contentIndex, id: id, name: name}
	if err := event.validate(); err != nil {
		return ToolCallStartEvent{}, err
	}
	return event, nil
}

func (ToolCallStartEvent) streamEvent() {}

func (e ToolCallStartEvent) validate() error {
	if e.contentIndex < 0 {
		return fmt.Errorf("%w: negative content index", ErrInvalidStreamEvent)
	}
	if err := validateToolIdentity(e.id, "id"); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidStreamEvent, err)
	}
	if err := validateToolIdentity(e.name, "name"); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidStreamEvent, err)
	}
	return nil
}

func (e ToolCallStartEvent) ContentIndex() int {
	return e.contentIndex
}

func (e ToolCallStartEvent) ID() string {
	return e.id
}

func (e ToolCallStartEvent) Name() string {
	return e.name
}

type ToolCallDeltaEvent struct {
	contentIndex int
	delta        []byte
}

func NewToolCallDeltaEvent(contentIndex int, delta []byte) (ToolCallDeltaEvent, error) {
	event := ToolCallDeltaEvent{contentIndex: contentIndex, delta: bytes.Clone(delta)}
	if err := event.validate(); err != nil {
		return ToolCallDeltaEvent{}, err
	}
	return event, nil
}

func (ToolCallDeltaEvent) streamEvent() {}

func (e ToolCallDeltaEvent) validate() error {
	if e.contentIndex < 0 {
		return fmt.Errorf("%w: negative content index", ErrInvalidStreamEvent)
	}
	if !utf8.Valid(e.delta) {
		return fmt.Errorf("%w: tool arguments delta is not valid UTF-8", ErrInvalidStreamEvent)
	}
	return nil
}

func (e ToolCallDeltaEvent) ContentIndex() int {
	return e.contentIndex
}

func (e ToolCallDeltaEvent) Delta() []byte {
	return bytes.Clone(e.delta)
}

type ToolCallEndEvent struct {
	contentIndex int
	toolCall     ToolCallBlock
}

func NewToolCallEndEvent(contentIndex int, toolCall ToolCallBlock) (ToolCallEndEvent, error) {
	event := ToolCallEndEvent{contentIndex: contentIndex, toolCall: toolCall}
	if err := event.validate(); err != nil {
		return ToolCallEndEvent{}, err
	}
	return event, nil
}

func (ToolCallEndEvent) streamEvent() {}

func (e ToolCallEndEvent) validate() error {
	if e.contentIndex < 0 {
		return fmt.Errorf("%w: negative content index", ErrInvalidStreamEvent)
	}
	if err := e.toolCall.validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidStreamEvent, err)
	}
	return nil
}

func (e ToolCallEndEvent) ContentIndex() int {
	return e.contentIndex
}

func (e ToolCallEndEvent) ToolCall() ToolCallBlock {
	return e.toolCall
}

type DoneEvent struct {
	reason    FinishReason
	usage     Usage
	timestamp time.Time
	metadata  AssistantMetadata
}

func NewDoneEventWithMetadata(reason FinishReason, usage Usage, timestamp time.Time, provenance AssistantProvenance, response *AssistantResponseMetadata, diagnostics []AssistantDiagnostic) (DoneEvent, error) {
	event := DoneEvent{
		reason: reason, usage: usage, timestamp: normalizeStreamTimestamp(timestamp),
		metadata: cloneAssistantMetadata(AssistantMetadata{Provenance: provenance, Response: response, Diagnostics: diagnostics}),
	}
	if err := event.validate(); err != nil {
		return DoneEvent{}, err
	}
	return event, nil
}

func NewDoneEvent(reason FinishReason, usage Usage, timestamp time.Time, provenance AssistantProvenance) (DoneEvent, error) {
	return NewDoneEventWithMetadata(reason, usage, timestamp, provenance, nil, nil)
}

func (DoneEvent) streamEvent() {}

func (e DoneEvent) validate() error {
	if e.reason != FinishStop && e.reason != FinishLength && e.reason != FinishToolUse {
		return fmt.Errorf("%w: done reason %q", ErrInvalidStreamEvent, e.reason)
	}
	return e.metadata.validate()
}

func (e DoneEvent) Reason() FinishReason {
	return e.reason
}

func (e DoneEvent) Usage() Usage {
	return e.usage
}

func (e DoneEvent) Timestamp() time.Time {
	return e.timestamp
}
func (e DoneEvent) ResponseMetadata() (AssistantResponseMetadata, bool) {
	if e.metadata.Response == nil {
		return AssistantResponseMetadata{}, false
	}
	return *e.metadata.Response, true
}
func (e DoneEvent) AssistantProvenance() AssistantProvenance { return e.metadata.Provenance }
func (e DoneEvent) Diagnostics() []AssistantDiagnostic {
	return cloneAssistantDiagnostics(e.metadata.Diagnostics)
}

type ErrorEvent struct {
	reason    FinishReason
	failure   Failure
	usage     Usage
	timestamp time.Time
	metadata  AssistantMetadata
}

func NewErrorEvent(
	reason FinishReason,
	errorMessage string,
	usage Usage,
	timestamp time.Time,
	provenance AssistantProvenance,
) (ErrorEvent, error) {
	failure, err := NewFailure(errorMessage, nil)
	if err != nil {
		return ErrorEvent{}, fmt.Errorf("%w: %w", ErrInvalidStreamEvent, err)
	}
	return NewErrorEventWithFailure(reason, failure, usage, timestamp, provenance)
}

func NewErrorEventWithFailure(
	reason FinishReason,
	failure Failure,
	usage Usage,
	timestamp time.Time,
	provenance AssistantProvenance,
) (ErrorEvent, error) {
	return NewErrorEventWithMetadata(reason, failure, usage, timestamp, provenance, nil, nil)
}

func NewErrorEventWithMetadata(
	reason FinishReason,
	failure Failure,
	usage Usage,
	timestamp time.Time,
	provenance AssistantProvenance,
	response *AssistantResponseMetadata,
	diagnostics []AssistantDiagnostic,
) (ErrorEvent, error) {
	event := ErrorEvent{
		reason:    reason,
		failure:   failure,
		usage:     usage,
		timestamp: normalizeStreamTimestamp(timestamp),
		metadata: cloneAssistantMetadata(AssistantMetadata{
			Provenance: provenance, Response: response, Diagnostics: diagnostics,
		}),
	}
	if err := event.validate(); err != nil {
		return ErrorEvent{}, err
	}
	return event, nil
}

// normalizeStreamTimestamp maps Go's richer time representation to pi's
// millisecond Unix timestamp. A zero time is the deterministic Go spelling
// used by synthetic providers for the wire value 0, not an absent timestamp.
func normalizeStreamTimestamp(timestamp time.Time) time.Time {
	if timestamp.IsZero() {
		return time.UnixMilli(0).UTC()
	}
	return time.UnixMilli(timestamp.UnixMilli()).UTC()
}

func (ErrorEvent) streamEvent() {}

func (e ErrorEvent) validate() error {
	if e.reason != FinishError && e.reason != FinishAborted {
		return fmt.Errorf("%w: error reason %q", ErrInvalidStreamEvent, e.reason)
	}
	if err := e.failure.validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidStreamEvent, err)
	}
	return e.metadata.validate()
}

func (e ErrorEvent) Reason() FinishReason {
	return e.reason
}

func (e ErrorEvent) ErrorMessage() string {
	return e.failure.Message()
}

func (e ErrorEvent) Failure() Failure {
	return e.failure
}

func (e ErrorEvent) Usage() Usage {
	return e.usage
}

func (e ErrorEvent) Timestamp() time.Time {
	return e.timestamp
}
func (e ErrorEvent) AssistantProvenance() AssistantProvenance { return e.metadata.Provenance }
func (e ErrorEvent) ResponseMetadata() (AssistantResponseMetadata, bool) {
	if e.metadata.Response == nil {
		return AssistantResponseMetadata{}, false
	}
	return *e.metadata.Response, true
}
func (e ErrorEvent) Diagnostics() []AssistantDiagnostic {
	return cloneAssistantDiagnostics(e.metadata.Diagnostics)
}

// AssistantFailureMessage is the unique failed terminal value. It retains all
// assistant blocks completed before the failure but never exposes a tool call
// as executable.
type AssistantFailureMessage struct {
	content   []AssistantBlock
	finish    FinishReason
	failure   Failure
	usage     Usage
	timestamp time.Time
	metadata  AssistantMetadata
}

func NewAssistantFailureMessage(
	content []TextBlock,
	finish FinishReason,
	errorMessage string,
	usage Usage,
	timestamp time.Time,
	provenance AssistantProvenance,
) (AssistantFailureMessage, error) {
	failure, err := NewFailure(errorMessage, nil)
	if err != nil {
		return AssistantFailureMessage{}, err
	}
	return NewAssistantFailureMessageWithFailure(content, finish, failure, usage, timestamp, provenance)
}

func NewAssistantFailureMessageWithFailure(
	content []TextBlock,
	finish FinishReason,
	failure Failure,
	usage Usage,
	timestamp time.Time,
	provenance AssistantProvenance,
) (AssistantFailureMessage, error) {
	return NewAssistantFailureMessageWithMetadata(content, finish, failure, usage, timestamp, provenance, nil, nil)
}

func NewAssistantFailureMessageWithMetadata(
	content []TextBlock,
	finish FinishReason,
	failure Failure,
	usage Usage,
	timestamp time.Time,
	provenance AssistantProvenance,
	response *AssistantResponseMetadata,
	diagnostics []AssistantDiagnostic,
) (AssistantFailureMessage, error) {
	blocks := make([]AssistantBlock, len(content))
	for index, block := range content {
		blocks[index] = block
	}
	return NewAssistantFailureMessageWithBlocksAndMetadata(blocks, finish, failure, usage, timestamp, provenance, response, diagnostics)
}

// NewAssistantFailureMessageWithBlocksAndMetadata preserves every complete
// assistant block produced before a failed/aborted terminal. The distinct
// failure concrete type ensures retained tool calls can never be executed.
func NewAssistantFailureMessageWithBlocksAndMetadata(
	content []AssistantBlock,
	finish FinishReason,
	failure Failure,
	usage Usage,
	timestamp time.Time,
	provenance AssistantProvenance,
	response *AssistantResponseMetadata,
	diagnostics []AssistantDiagnostic,
) (AssistantFailureMessage, error) {
	message := AssistantFailureMessage{
		content:   append([]AssistantBlock(nil), content...),
		finish:    finish,
		failure:   failure,
		usage:     usage,
		timestamp: timestamp,
		metadata: cloneAssistantMetadata(AssistantMetadata{
			Provenance: provenance, Response: response, Diagnostics: diagnostics,
		}),
	}
	if err := message.validate(); err != nil {
		return AssistantFailureMessage{}, err
	}
	return message, nil
}

func (AssistantFailureMessage) assistantTerminal()   {}
func (AssistantFailureMessage) conversationMessage() {}

func (m AssistantFailureMessage) validate() error {
	if m.finish != FinishError && m.finish != FinishAborted {
		return fmt.Errorf("%w: failure reason %q", ErrInvalidFinishReason, m.finish)
	}
	if err := m.failure.validate(); err != nil {
		return err
	}
	seenCalls := map[string]struct{}{}
	for _, candidate := range m.content {
		switch block := candidate.(type) {
		case TextBlock:
			if err := block.validate(); err != nil {
				return err
			}
		case ThinkingBlock:
			if err := block.validate(); err != nil {
				return err
			}
		case ToolCallBlock:
			if err := block.validate(); err != nil {
				return err
			}
			if _, duplicate := seenCalls[block.ID()]; duplicate {
				return fmt.Errorf("%w: duplicate id %q", ErrInvalidToolCall, block.ID())
			}
			seenCalls[block.ID()] = struct{}{}
		default:
			return fmt.Errorf("%w: failure assistant block %T", ErrInvalidRichContent, candidate)
		}
	}
	return m.metadata.validate()
}

func (m AssistantFailureMessage) AssistantProvenance() AssistantProvenance {
	return m.metadata.Provenance
}
func (m AssistantFailureMessage) ResponseMetadata() (AssistantResponseMetadata, bool) {
	if m.metadata.Response == nil {
		return AssistantResponseMetadata{}, false
	}
	return *m.metadata.Response, true
}
func (m AssistantFailureMessage) Diagnostics() []AssistantDiagnostic {
	return cloneAssistantDiagnostics(m.metadata.Diagnostics)
}

func (AssistantFailureMessage) Role() Role {
	return RoleAssistant
}

func (m AssistantFailureMessage) Content() []TextBlock {
	content := make([]TextBlock, 0, len(m.content))
	for _, block := range m.content {
		if text, ok := block.(TextBlock); ok {
			content = append(content, text)
		}
	}
	return content
}

func (m AssistantFailureMessage) Blocks() []AssistantBlock {
	return append([]AssistantBlock(nil), m.content...)
}

func (m AssistantFailureMessage) FinishReason() FinishReason {
	return m.finish
}

func (m AssistantFailureMessage) ErrorMessage() string {
	return m.failure.Message()
}

func (m AssistantFailureMessage) Failure() Failure {
	return m.failure
}

func (m AssistantFailureMessage) Usage() Usage {
	return m.usage
}

func (m AssistantFailureMessage) Timestamp() time.Time {
	return m.timestamp
}

// StreamActiveBlock is an immutable snapshot of the currently streaming block.
// Tool arguments may be incomplete JSON until toolcall_end.
type StreamActiveBlock struct {
	kind         AssistantBlockKind
	contentIndex int
	text         string
	toolCallID   string
	toolName     string
	arguments    []byte
}

// PartialThinkingBlock and PartialToolCallBlock are the in-progress members of
// pi's assistant content union. They can appear only in StreamSnapshot.Blocks;
// terminal constructors reject them until their matching end event arrives.
type PartialThinkingBlock struct{ thinking string }

func (PartialThinkingBlock) assistantBlock()          {}
func (PartialThinkingBlock) Kind() AssistantBlockKind { return AssistantBlockThinking }
func (b PartialThinkingBlock) Thinking() string       { return b.thinking }

type PartialToolCallBlock struct {
	id, name  string
	arguments []byte
}

func (PartialToolCallBlock) assistantBlock()          {}
func (PartialToolCallBlock) Kind() AssistantBlockKind { return AssistantBlockToolCall }
func (b PartialToolCallBlock) ID() string             { return b.id }
func (b PartialToolCallBlock) Name() string           { return b.name }
func (b PartialToolCallBlock) ArgumentsFragment() []byte {
	return bytes.Clone(b.arguments)
}

func (b StreamActiveBlock) Kind() AssistantBlockKind {
	return b.kind
}

func (b StreamActiveBlock) ContentIndex() int {
	return b.contentIndex
}

func (b StreamActiveBlock) Text() (string, bool) {
	return b.text, b.kind == AssistantBlockText
}

func (b StreamActiveBlock) ToolCall() (id, name string, argumentsJSON []byte, ok bool) {
	if b.kind != AssistantBlockToolCall {
		return "", "", nil, false
	}
	return b.toolCallID, b.toolName, bytes.Clone(b.arguments), true
}

// StreamSnapshot is an immutable view of completed and active blocks. Some
// provider dialects begin a text block before ending a thinking block, so a
// snapshot must retain every concurrently active content slot.
type StreamSnapshot struct {
	blocks     []AssistantBlock
	active     []StreamActiveBlock
	finish     FinishReason
	terminal   bool
	failure    *Failure
	provenance AssistantProvenance
	timestamp  time.Time
}

func (s StreamSnapshot) Blocks() []AssistantBlock {
	if len(s.active) == 0 {
		return append([]AssistantBlock(nil), s.blocks...)
	}
	result := make([]AssistantBlock, 0, len(s.blocks)+len(s.active))
	completedIndex, activeIndex := 0, 0
	for contentIndex := range len(s.blocks) + len(s.active) {
		if activeIndex < len(s.active) && s.active[activeIndex].contentIndex == contentIndex {
			result = append(result, activeAssistantBlock(s.active[activeIndex]))
			activeIndex++
			continue
		}
		if completedIndex < len(s.blocks) {
			result = append(result, s.blocks[completedIndex])
			completedIndex++
		}
	}
	return result
}

func (s StreamSnapshot) CompletedBlocks() []AssistantBlock {
	return append([]AssistantBlock(nil), s.blocks...)
}

func (s StreamSnapshot) ActiveBlock() (StreamActiveBlock, bool) {
	if len(s.active) == 0 {
		return StreamActiveBlock{}, false
	}
	return cloneStreamActiveBlock(s.active[0]), true
}

// ActiveBlocks returns every open content slot in provider content order.
func (s StreamSnapshot) ActiveBlocks() []StreamActiveBlock {
	result := make([]StreamActiveBlock, len(s.active))
	for index, active := range s.active {
		result[index] = cloneStreamActiveBlock(active)
	}
	return result
}

func (s StreamSnapshot) TextContent() []TextBlock {
	content := make([]TextBlock, 0, len(s.blocks)+len(s.active))
	for _, block := range s.Blocks() {
		if text, ok := block.(TextBlock); ok {
			content = append(content, text)
		}
	}
	return content
}

func cloneStreamActiveBlock(active StreamActiveBlock) StreamActiveBlock {
	active.arguments = bytes.Clone(active.arguments)
	return active
}

func activeAssistantBlock(active StreamActiveBlock) AssistantBlock {
	switch active.kind {
	case AssistantBlockText:
		return TextBlock{text: active.text}
	case AssistantBlockThinking:
		return PartialThinkingBlock{thinking: active.text}
	case AssistantBlockToolCall:
		return PartialToolCallBlock{
			id: active.toolCallID, name: active.toolName,
			arguments: bytes.Clone(active.arguments),
		}
	default:
		return TextBlock{}
	}
}

func (s StreamSnapshot) FinishReason() FinishReason {
	return s.finish
}

func (s StreamSnapshot) Terminal() bool {
	return s.terminal
}

func (s StreamSnapshot) ErrorMessage() (string, bool) {
	if s.failure == nil {
		return "", false
	}
	return s.failure.Message(), true
}

func (s StreamSnapshot) Failure() (Failure, bool) {
	if s.failure == nil {
		return Failure{}, false
	}
	return *s.failure, true
}
func (s StreamSnapshot) AssistantProvenance() AssistantProvenance { return s.provenance }
func (s StreamSnapshot) Timestamp() time.Time                     { return s.timestamp }

type streamPhase uint8

const (
	streamNew streamPhase = iota
	streamActive
	streamTerminal
	streamClosed
	streamFailed
)

// StreamCollector owns the strict start/block/done-or-error protocol. It is
// intentionally synchronous; provider goroutines may produce events but do not
// share or mutate collector state.
type StreamCollector struct {
	phase     streamPhase
	start     StartEvent
	blocks    map[int]AssistantBlock
	slots     map[int]*collectorSlot
	nextIndex int
	terminal  AssistantTerminal
	failure   error
}

type collectorSlot struct {
	kind      AssistantBlockKind
	text      strings.Builder
	id, name  string
	arguments []byte
}

func (c *StreamCollector) Accept(event StreamEvent) error {
	if c.failure != nil {
		return c.failure
	}
	if c.phase == streamClosed {
		return ErrStreamClosed
	}
	if err := validateStreamEvent(event); err != nil {
		return c.fail(err, "event %T failed validation", event)
	}
	if c.phase == streamTerminal {
		cause := error(nil)
		switch event.(type) {
		case DoneEvent, ErrorEvent:
			cause = ErrDuplicateTerminal
		}
		return c.fail(cause, "event %T arrived after terminal", event)
	}

	switch event := event.(type) {
	case StartEvent:
		if c.phase != streamNew {
			return c.fail(nil, "start arrived after stream activity")
		}
		c.phase = streamActive
		c.start = event
		c.blocks = make(map[int]AssistantBlock)
		c.slots = make(map[int]*collectorSlot)
		return nil

	case TextStartEvent:
		if c.phase != streamActive {
			return c.fail(nil, "text_start arrived before start")
		}
		if event.contentIndex != c.nextIndex {
			return c.fail(nil, "text_start index %d, want %d", event.contentIndex, c.nextIndex)
		}
		c.openSlot(event.contentIndex, AssistantBlockText, "", "")
		return nil

	case TextDeltaEvent:
		slot, ok := c.slot(event.contentIndex)
		if c.phase != streamActive || !ok || slot.kind != AssistantBlockText {
			return c.fail(nil, "text_delta arrived without an open text block")
		}
		_, _ = slot.text.WriteString(event.delta)
		return nil

	case TextEndEvent:
		slot, ok := c.slot(event.contentIndex)
		if c.phase != streamActive || !ok || slot.kind != AssistantBlockText {
			return c.fail(nil, "text_end arrived without an open text block")
		}
		if event.content != slot.text.String() {
			return c.fail(nil, "text_end content does not match accumulated deltas")
		}
		signature, _ := event.TextSignature()
		block, err := NewTextBlockWithSignature(event.content, signature)
		if err != nil {
			return c.fail(err, "text_end produced invalid text")
		}
		c.closeSlot(event.contentIndex, block)
		return nil

	case ThinkingStartEvent:
		if c.phase != streamActive || event.contentIndex != c.nextIndex {
			return c.fail(nil, "thinking_start arrived out of order")
		}
		c.openSlot(event.contentIndex, AssistantBlockThinking, "", "")
		return nil
	case ThinkingDeltaEvent:
		slot, ok := c.slot(event.contentIndex)
		if c.phase != streamActive || !ok || slot.kind != AssistantBlockThinking {
			return c.fail(nil, "thinking_delta arrived without open thinking")
		}
		_, _ = slot.text.WriteString(event.delta)
		return nil
	case ThinkingEndEvent:
		slot, ok := c.slot(event.contentIndex)
		if c.phase != streamActive || !ok || slot.kind != AssistantBlockThinking {
			return c.fail(nil, "thinking_end arrived without open thinking")
		}
		// Provider terminal items are authoritative for reasoning summaries. A
		// compatible endpoint may revise an advisory streamed summary at
		// output_item.done, so retain the final block carried by thinking_end.
		// Consumers still receive all progress deltas and can reconcile their
		// display from this closing event, matching pi's stream contract.
		c.closeSlot(event.contentIndex, event.content)
		return nil

	case ToolCallStartEvent:
		if c.phase != streamActive {
			return c.fail(nil, "toolcall_start arrived before start")
		}
		if event.contentIndex != c.nextIndex {
			return c.fail(nil, "toolcall_start index %d, want %d", event.contentIndex, c.nextIndex)
		}
		c.openSlot(event.contentIndex, AssistantBlockToolCall, event.id, event.name)
		return nil

	case ToolCallDeltaEvent:
		slot, ok := c.slot(event.contentIndex)
		if c.phase != streamActive || !ok || slot.kind != AssistantBlockToolCall {
			return c.fail(nil, "toolcall_delta arrived without an open tool call block")
		}
		slot.arguments = append(slot.arguments, event.delta...)
		return nil

	case ToolCallEndEvent:
		slot, ok := c.slot(event.contentIndex)
		if c.phase != streamActive || !ok || slot.kind != AssistantBlockToolCall {
			return c.fail(nil, "toolcall_end arrived without an open tool call block")
		}
		if event.toolCall.ID() != slot.id || event.toolCall.Name() != slot.name {
			return c.fail(nil, "toolcall_end identity does not match toolcall_start")
		}
		if !bytes.Equal(event.toolCall.ArgumentsJSON(), slot.arguments) {
			return c.fail(nil, "toolcall_end arguments do not match accumulated deltas")
		}
		c.closeSlot(event.contentIndex, event.toolCall)
		return nil

	case DoneEvent:
		if c.phase != streamNew && c.phase != streamActive {
			return c.fail(nil, "done arrived in invalid stream phase")
		}
		if len(c.slots) != 0 {
			return c.fail(nil, "done arrived before all content blocks ended")
		}
		blocks := c.orderedBlocks()
		if c.phase == streamActive && !event.AssistantProvenance().Matches(c.start.provenance.Provider, c.start.provenance.API, c.start.provenance.Model) {
			return c.fail(nil, "done provenance does not match start")
		}
		var message AssistantTerminal
		var err error
		switch event.reason {
		case FinishStop, FinishLength:
			hasToolCall := false
			for _, block := range blocks {
				if _, ok := block.(ToolCallBlock); ok {
					hasToolCall = true
					break
				}
			}
			response, hasResponse := event.ResponseMetadata()
			var responsePointer *AssistantResponseMetadata
			if hasResponse {
				responsePointer = &response
			}
			provenance := event.AssistantProvenance()
			if hasToolCall {
				message, err = NewAssistantToolUseMessageWithFinishAndMetadata(blocks, event.reason, event.usage, event.timestamp, provenance, responsePointer, event.Diagnostics())
				break
			}
			text := make([]TextBlock, len(blocks))
			hasThinking := false
			for index, block := range blocks {
				var ok bool
				text[index], ok = block.(TextBlock)
				if _, thinking := block.(ThinkingBlock); thinking {
					hasThinking = true
					continue
				}
				if !ok {
					return c.fail(nil, "%s terminal contains a tool call", event.reason)
				}
			}
			if hasThinking {
				message, err = NewAssistantRichMessageWithMetadata(blocks, event.reason, event.usage, event.timestamp, provenance, responsePointer, event.Diagnostics())
			} else {
				message, err = NewAssistantTextMessageWithMetadata(text, event.reason, event.usage, event.timestamp, provenance, responsePointer, event.Diagnostics())
			}
		case FinishToolUse:
			response, hasResponse := event.ResponseMetadata()
			var responsePointer *AssistantResponseMetadata
			if hasResponse {
				responsePointer = &response
			}
			message, err = NewAssistantToolUseMessageWithMetadata(blocks, event.usage, event.timestamp, event.AssistantProvenance(), responsePointer, event.Diagnostics())
		}
		if err != nil {
			return c.fail(err, "done is not a valid terminal")
		}
		if err := ValidateAssistantTerminal(message); err != nil {
			return c.fail(err, "done produced an invalid terminal")
		}
		c.terminal = message
		c.phase = streamTerminal
		return nil

	case ErrorEvent:
		if c.phase != streamNew && c.phase != streamActive {
			return c.fail(nil, "error arrived in invalid stream phase")
		}
		content, contentErr := c.failureBlocks()
		if contentErr != nil {
			return c.fail(contentErr, "partial failure content is invalid")
		}
		response, hasResponse := event.ResponseMetadata()
		var responsePointer *AssistantResponseMetadata
		if hasResponse {
			responsePointer = &response
		}
		message, err := NewAssistantFailureMessageWithBlocksAndMetadata(
			content,
			event.reason,
			event.failure,
			event.usage,
			event.timestamp,
			event.AssistantProvenance(),
			responsePointer,
			event.Diagnostics(),
		)
		if err != nil {
			return c.fail(err, "error produced an invalid terminal")
		}
		c.terminal = message
		c.phase = streamTerminal
		return nil

	default:
		return c.fail(nil, "unsupported event %T", event)
	}
}

// Close marks provider EOF. A terminal event is mandatory before Close.
func (c *StreamCollector) Close() error {
	if c.failure != nil {
		return c.failure
	}
	switch c.phase {
	case streamTerminal:
		c.phase = streamClosed
		return nil
	case streamClosed:
		return nil
	default:
		return c.fail(ErrUnexpectedEOF, "stream closed without a terminal event")
	}
}

func (c *StreamCollector) Snapshot() (StreamSnapshot, error) {
	if c.failure != nil {
		return StreamSnapshot{}, c.failure
	}
	if c.phase == streamNew {
		return StreamSnapshot{}, ErrStreamNotStarted
	}
	if c.terminal != nil {
		snapshot := StreamSnapshot{
			blocks:     c.terminal.Blocks(),
			finish:     c.terminal.FinishReason(),
			terminal:   true,
			provenance: c.terminal.AssistantProvenance(),
			timestamp:  c.terminal.Timestamp(),
		}
		if failure, ok := c.terminal.(AssistantFailureMessage); ok {
			terminalFailure := failure.Failure()
			snapshot.failure = &terminalFailure
		}
		return snapshot, nil
	}

	snapshot := StreamSnapshot{
		blocks: c.orderedBlocks(), finish: FinishPending,
		provenance: c.start.provenance, timestamp: c.start.timestamp,
	}
	for _, index := range c.openIndices() {
		slot := c.slots[index]
		snapshot.active = append(snapshot.active, StreamActiveBlock{
			kind:         slot.kind,
			contentIndex: index,
			text:         slot.text.String(),
			toolCallID:   slot.id,
			toolName:     slot.name,
			arguments:    bytes.Clone(slot.arguments),
		})
	}
	return snapshot, nil
}

func (c *StreamCollector) Result() (AssistantTerminal, error) {
	if c.failure != nil {
		return nil, c.failure
	}
	if c.phase != streamClosed {
		return nil, ErrStreamNotClosed
	}
	return c.terminal, nil
}

// FailureBlocks returns the canonical terminal-safe projection of all content
// observed so far. Active text and thinking are materialized, a syntactically
// complete active tool call is preserved, and incomplete tool JSON is omitted.
// It is shared by provider ErrorEvent handling and transport-failure adapters.
func (c *StreamCollector) FailureBlocks() ([]AssistantBlock, error) {
	if c.terminal != nil {
		return c.terminal.Blocks(), nil
	}
	return c.failureBlocks()
}

func (c *StreamCollector) fail(cause error, format string, args ...any) error {
	err := &streamProtocolError{cause: cause, detail: fmt.Sprintf(format, args...)}
	c.failure = err
	c.phase = streamFailed
	return err
}

func (c *StreamCollector) openSlot(index int, kind AssistantBlockKind, id, name string) {
	if c.slots == nil {
		c.slots = make(map[int]*collectorSlot)
	}
	c.slots[index] = &collectorSlot{kind: kind, id: id, name: name}
	c.nextIndex++
}
func (c *StreamCollector) slot(index int) (*collectorSlot, bool) {
	slot, ok := c.slots[index]
	return slot, ok
}
func (c *StreamCollector) closeSlot(index int, block AssistantBlock) {
	if c.blocks == nil {
		c.blocks = make(map[int]AssistantBlock)
	}
	c.blocks[index] = block
	delete(c.slots, index)
}
func (c *StreamCollector) orderedBlocks() []AssistantBlock {
	result := make([]AssistantBlock, 0, len(c.blocks))
	for i := 0; i < c.nextIndex; i++ {
		if block, ok := c.blocks[i]; ok {
			result = append(result, block)
		}
	}
	return result
}
func (c *StreamCollector) openIndices() []int {
	result := make([]int, 0, len(c.slots))
	for i := 0; i < c.nextIndex; i++ {
		if _, ok := c.slots[i]; ok {
			result = append(result, i)
		}
	}
	return result
}

func (c *StreamCollector) failureBlocks() ([]AssistantBlock, error) {
	result := make([]AssistantBlock, 0, len(c.blocks)+len(c.slots))
	for index := 0; index < c.nextIndex; index++ {
		if block, ok := c.blocks[index]; ok {
			result = append(result, block)
			continue
		}
		slot, ok := c.slots[index]
		if !ok {
			continue
		}
		switch slot.kind {
		case AssistantBlockText:
			block, err := NewTextBlock(slot.text.String())
			if err != nil {
				return nil, err
			}
			result = append(result, block)
		case AssistantBlockThinking:
			// An untouched thinking_start has no valid terminal representation;
			// preserve it as soon as the provider has emitted actual content.
			if slot.text.Len() == 0 {
				continue
			}
			block, err := NewThinkingBlock(slot.text.String())
			if err != nil {
				return nil, err
			}
			result = append(result, block)
		case AssistantBlockToolCall:
			// Incomplete JSON is a streaming fragment rather than a valid ToolCall
			// member. Complete arguments remain visible on the failure message but
			// the failure concrete type makes them non-executable.
			block, err := NewToolCallBlock(slot.id, slot.name, slot.arguments)
			if err == nil {
				result = append(result, block)
			}
		}
	}
	return result, nil
}

func validateStreamEvent(event StreamEvent) error {
	switch event := event.(type) {
	case StartEvent:
		return event.validate()
	case TextStartEvent:
		return event.validate()
	case TextDeltaEvent:
		return event.validate()
	case TextEndEvent:
		return event.validate()
	case ThinkingStartEvent:
		return event.validate()
	case ThinkingDeltaEvent:
		return event.validate()
	case ThinkingEndEvent:
		return event.validate()
	case ToolCallStartEvent:
		return event.validate()
	case ToolCallDeltaEvent:
		return event.validate()
	case ToolCallEndEvent:
		return event.validate()
	case DoneEvent:
		return event.validate()
	case ErrorEvent:
		return event.validate()
	default:
		return fmt.Errorf("%w: unsupported event %T", ErrInvalidStreamEvent, event)
	}
}

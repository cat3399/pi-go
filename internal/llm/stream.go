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

type StartEvent struct{}

func NewStartEvent() StartEvent {
	return StartEvent{}
}

func (StartEvent) streamEvent() {}

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
	contentIndex int
	content      string
}

func NewTextEndEvent(contentIndex int, content string) (TextEndEvent, error) {
	event := TextEndEvent{contentIndex: contentIndex, content: content}
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
	return nil
}

func (e TextEndEvent) ContentIndex() int {
	return e.contentIndex
}

func (e TextEndEvent) Content() string {
	return e.content
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
}

func NewDoneEvent(reason FinishReason, usage Usage, timestamp time.Time) (DoneEvent, error) {
	event := DoneEvent{reason: reason, usage: usage, timestamp: timestamp}
	if err := event.validate(); err != nil {
		return DoneEvent{}, err
	}
	return event, nil
}

func (DoneEvent) streamEvent() {}

func (e DoneEvent) validate() error {
	if e.reason != FinishStop && e.reason != FinishLength && e.reason != FinishToolUse {
		return fmt.Errorf("%w: done reason %q", ErrInvalidStreamEvent, e.reason)
	}
	return nil
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

type ErrorEvent struct {
	reason    FinishReason
	failure   Failure
	usage     Usage
	timestamp time.Time
}

func NewErrorEvent(
	reason FinishReason,
	errorMessage string,
	usage Usage,
	timestamp time.Time,
) (ErrorEvent, error) {
	failure, err := NewFailure(errorMessage, nil)
	if err != nil {
		return ErrorEvent{}, fmt.Errorf("%w: %w", ErrInvalidStreamEvent, err)
	}
	return NewErrorEventWithFailure(reason, failure, usage, timestamp)
}

func NewErrorEventWithFailure(
	reason FinishReason,
	failure Failure,
	usage Usage,
	timestamp time.Time,
) (ErrorEvent, error) {
	event := ErrorEvent{
		reason:    reason,
		failure:   failure,
		usage:     usage,
		timestamp: timestamp,
	}
	if err := event.validate(); err != nil {
		return ErrorEvent{}, err
	}
	return event, nil
}

func (ErrorEvent) streamEvent() {}

func (e ErrorEvent) validate() error {
	if e.reason != FinishError && e.reason != FinishAborted {
		return fmt.Errorf("%w: error reason %q", ErrInvalidStreamEvent, e.reason)
	}
	if err := e.failure.validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidStreamEvent, err)
	}
	return nil
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

// AssistantFailureMessage is the unique failed terminal value. It retains text
// completed before the failure but never exposes a tool call as executable.
type AssistantFailureMessage struct {
	content   []TextBlock
	finish    FinishReason
	failure   Failure
	usage     Usage
	timestamp time.Time
}

func NewAssistantFailureMessage(
	content []TextBlock,
	finish FinishReason,
	errorMessage string,
	usage Usage,
	timestamp time.Time,
) (AssistantFailureMessage, error) {
	failure, err := NewFailure(errorMessage, nil)
	if err != nil {
		return AssistantFailureMessage{}, err
	}
	return NewAssistantFailureMessageWithFailure(content, finish, failure, usage, timestamp)
}

func NewAssistantFailureMessageWithFailure(
	content []TextBlock,
	finish FinishReason,
	failure Failure,
	usage Usage,
	timestamp time.Time,
) (AssistantFailureMessage, error) {
	message := AssistantFailureMessage{
		content:   append([]TextBlock(nil), content...),
		finish:    finish,
		failure:   failure,
		usage:     usage,
		timestamp: timestamp,
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
	for _, block := range m.content {
		if err := block.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (AssistantFailureMessage) Role() Role {
	return RoleAssistant
}

func (m AssistantFailureMessage) Content() []TextBlock {
	return append([]TextBlock(nil), m.content...)
}

func (m AssistantFailureMessage) Blocks() []AssistantBlock {
	blocks := make([]AssistantBlock, len(m.content))
	for index, block := range m.content {
		blocks[index] = block
	}
	return blocks
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

// StreamSnapshot is an immutable view of completed blocks plus an optional
// active partial block.
type StreamSnapshot struct {
	blocks   []AssistantBlock
	active   *StreamActiveBlock
	finish   FinishReason
	terminal bool
	failure  *Failure
}

func (s StreamSnapshot) Blocks() []AssistantBlock {
	return append([]AssistantBlock(nil), s.blocks...)
}

func (s StreamSnapshot) ActiveBlock() (StreamActiveBlock, bool) {
	if s.active == nil {
		return StreamActiveBlock{}, false
	}
	active := *s.active
	active.arguments = bytes.Clone(active.arguments)
	return active, true
}

func (s StreamSnapshot) TextContent() []TextBlock {
	content := make([]TextBlock, 0, len(s.blocks)+1)
	for _, block := range s.blocks {
		if text, ok := block.(TextBlock); ok {
			content = append(content, text)
		}
	}
	if s.active != nil && s.active.kind == AssistantBlockText {
		content = append(content, TextBlock{text: s.active.text})
	}
	return content
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
	phase         streamPhase
	blocks        []AssistantBlock
	current       strings.Builder
	activeKind    AssistantBlockKind
	currentIndex  int
	nextIndex     int
	toolCallID    string
	toolName      string
	toolArguments []byte
	terminal      AssistantTerminal
	failure       error
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
		return nil

	case TextStartEvent:
		if c.phase != streamActive {
			return c.fail(nil, "text_start arrived before start")
		}
		if c.activeKind != 0 {
			return c.fail(nil, "text_start arrived before content index %d ended", c.currentIndex)
		}
		if event.contentIndex != c.nextIndex {
			return c.fail(nil, "text_start index %d, want %d", event.contentIndex, c.nextIndex)
		}
		c.activeKind = AssistantBlockText
		c.currentIndex = event.contentIndex
		c.current.Reset()
		return nil

	case TextDeltaEvent:
		if c.phase != streamActive || c.activeKind != AssistantBlockText {
			return c.fail(nil, "text_delta arrived without an open text block")
		}
		if event.contentIndex != c.currentIndex {
			return c.fail(nil, "text_delta index %d, want %d", event.contentIndex, c.currentIndex)
		}
		_, _ = c.current.WriteString(event.delta)
		return nil

	case TextEndEvent:
		if c.phase != streamActive || c.activeKind != AssistantBlockText {
			return c.fail(nil, "text_end arrived without an open text block")
		}
		if event.contentIndex != c.currentIndex {
			return c.fail(nil, "text_end index %d, want %d", event.contentIndex, c.currentIndex)
		}
		if event.content != c.current.String() {
			return c.fail(nil, "text_end content does not match accumulated deltas")
		}
		block, err := NewTextBlock(event.content)
		if err != nil {
			return c.fail(err, "text_end produced invalid text")
		}
		c.blocks = append(c.blocks, block)
		c.finishActiveBlock()
		return nil

	case ToolCallStartEvent:
		if c.phase != streamActive {
			return c.fail(nil, "toolcall_start arrived before start")
		}
		if c.activeKind != 0 {
			return c.fail(nil, "toolcall_start arrived before content index %d ended", c.currentIndex)
		}
		if event.contentIndex != c.nextIndex {
			return c.fail(nil, "toolcall_start index %d, want %d", event.contentIndex, c.nextIndex)
		}
		c.activeKind = AssistantBlockToolCall
		c.currentIndex = event.contentIndex
		c.toolCallID = event.id
		c.toolName = event.name
		c.toolArguments = nil
		return nil

	case ToolCallDeltaEvent:
		if c.phase != streamActive || c.activeKind != AssistantBlockToolCall {
			return c.fail(nil, "toolcall_delta arrived without an open tool call block")
		}
		if event.contentIndex != c.currentIndex {
			return c.fail(nil, "toolcall_delta index %d, want %d", event.contentIndex, c.currentIndex)
		}
		c.toolArguments = append(c.toolArguments, event.delta...)
		return nil

	case ToolCallEndEvent:
		if c.phase != streamActive || c.activeKind != AssistantBlockToolCall {
			return c.fail(nil, "toolcall_end arrived without an open tool call block")
		}
		if event.contentIndex != c.currentIndex {
			return c.fail(nil, "toolcall_end index %d, want %d", event.contentIndex, c.currentIndex)
		}
		if event.toolCall.ID() != c.toolCallID || event.toolCall.Name() != c.toolName {
			return c.fail(nil, "toolcall_end identity does not match toolcall_start")
		}
		if !bytes.Equal(event.toolCall.ArgumentsJSON(), c.toolArguments) {
			return c.fail(nil, "toolcall_end arguments do not match accumulated deltas")
		}
		c.blocks = append(c.blocks, event.toolCall)
		c.finishActiveBlock()
		return nil

	case DoneEvent:
		if c.phase != streamActive {
			return c.fail(nil, "done arrived before start")
		}
		if c.activeKind != 0 {
			return c.fail(nil, "done arrived before block end for content index %d", c.currentIndex)
		}
		var message AssistantTerminal
		var err error
		switch event.reason {
		case FinishStop, FinishLength:
			text := make([]TextBlock, len(c.blocks))
			for index, block := range c.blocks {
				var ok bool
				text[index], ok = block.(TextBlock)
				if !ok {
					return c.fail(nil, "%s terminal contains a tool call", event.reason)
				}
			}
			message, err = NewAssistantTextMessage(text, event.reason, event.usage, event.timestamp)
		case FinishToolUse:
			message, err = NewAssistantToolUseMessage(c.blocks, event.usage, event.timestamp)
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
		content := make([]TextBlock, 0, len(c.blocks)+1)
		for _, block := range c.blocks {
			if text, ok := block.(TextBlock); ok {
				content = append(content, text)
			}
		}
		if c.activeKind == AssistantBlockText {
			block, err := NewTextBlock(c.current.String())
			if err != nil {
				return c.fail(err, "partial failure content is invalid")
			}
			content = append(content, block)
		}
		message, err := NewAssistantFailureMessageWithFailure(
			content,
			event.reason,
			event.failure,
			event.usage,
			event.timestamp,
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
			blocks:   c.terminal.Blocks(),
			finish:   c.terminal.FinishReason(),
			terminal: true,
		}
		if failure, ok := c.terminal.(AssistantFailureMessage); ok {
			terminalFailure := failure.Failure()
			snapshot.failure = &terminalFailure
		}
		return snapshot, nil
	}

	snapshot := StreamSnapshot{
		blocks: append([]AssistantBlock(nil), c.blocks...),
		finish: FinishPending,
	}
	if c.activeKind != 0 {
		snapshot.active = &StreamActiveBlock{
			kind:         c.activeKind,
			contentIndex: c.currentIndex,
			text:         c.current.String(),
			toolCallID:   c.toolCallID,
			toolName:     c.toolName,
			arguments:    bytes.Clone(c.toolArguments),
		}
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

func (c *StreamCollector) fail(cause error, format string, args ...any) error {
	err := &streamProtocolError{cause: cause, detail: fmt.Sprintf(format, args...)}
	c.failure = err
	c.phase = streamFailed
	return err
}

func (c *StreamCollector) finishActiveBlock() {
	c.activeKind = 0
	c.currentIndex = 0
	c.nextIndex++
	c.current.Reset()
	c.toolCallID = ""
	c.toolName = ""
	c.toolArguments = nil
}

func validateStreamEvent(event StreamEvent) error {
	switch event := event.(type) {
	case StartEvent:
		return nil
	case TextStartEvent:
		return event.validate()
	case TextDeltaEvent:
		return event.validate()
	case TextEndEvent:
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

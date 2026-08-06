package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

const entryIDAttempts = 100

var validSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)

var errIDGeneratorCallbackPanicked = errors.New("session entry ID generator callback panicked")

type runtimeConfig struct {
	now        Clock
	newEntryID IDGenerator
}

type Session struct {
	mu             sync.RWMutex
	appendGate     chan struct{}
	storage        sessionStorage
	path           string
	header         Header
	entries        []Entry
	byID           map[string]int
	leaf           int
	generation     uint64
	needsSeparator bool
	runtime        runtimeConfig
	poisoned       bool
	closed         bool
	writerClaim    *writerClaim
}

func Create(path string, options CreateOptions) (*Session, error) {
	return createWithStorage(osSessionStorage{}, path, options)
}

func createWithStorage(storage sessionStorage, path string, options CreateOptions) (*Session, error) {
	resolvedPath, err := resolveSessionPath(path)
	if err != nil {
		return nil, err
	}
	claim, err := claimSessionWriter(resolvedPath)
	if err != nil {
		return nil, err
	}
	claimed := true
	defer func() {
		if claimed {
			releaseSessionWriter(claim)
		}
	}()
	workingDir, err := resolveWorkingDir(options.WorkingDir)
	if err != nil {
		return nil, err
	}
	runtime := normalizeRuntime(options.Now, options.NewEntryID)
	timestamp := canonicalTime(runtime.now())
	if timestamp.IsZero() {
		return nil, fmt.Errorf("%w: zero creation timestamp", ErrInvalidSession)
	}
	if err := validateISOTime(timestamp); err != nil {
		return nil, fmt.Errorf("%w: creation timestamp: %v", ErrInvalidSession, err)
	}
	sessionID := options.ID
	if sessionID == "" {
		sessionID, err = newSessionID(timestamp)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrIDGeneration, err)
		}
	}
	if err := ValidateSessionID(sessionID); err != nil {
		return nil, err
	}
	if options.ParentSession != "" && (!utf8.ValidString(options.ParentSession) || strings.TrimSpace(options.ParentSession) == "") {
		return nil, fmt.Errorf("%w: parent session must be valid UTF-8", ErrInvalidSession)
	}
	wire := newHeaderWire{
		Type:          "session",
		Version:       3,
		ID:            sessionID,
		Timestamp:     formatISOTime(timestamp),
		WorkingDir:    workingDir,
		ParentSession: options.ParentSession,
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("%w: encode header: %w", ErrInvalidSession, err)
	}
	created, err := storage.create(resolvedPath, append(append([]byte(nil), raw...), '\n'))
	if err != nil {
		if created {
			return nil, fmt.Errorf("%w: %w", ErrDurabilityUnknown, err)
		}
		return nil, fmt.Errorf("%w: create %s: %w", ErrStorage, resolvedPath, err)
	}
	if err := refreshSessionWriter(claim, resolvedPath); err != nil {
		return nil, err
	}

	session := &Session{
		appendGate: make(chan struct{}, 1),
		storage:    storage,
		path:       resolvedPath,
		header: Header{
			id:               sessionID,
			workingDir:       workingDir,
			parentSession:    options.ParentSession,
			hasParentSession: options.ParentSession != "",
			timestamp:        timestamp,
			raw:              raw,
		},
		byID:        make(map[string]int),
		leaf:        -1,
		runtime:     runtime,
		writerClaim: claim,
	}
	claimed = false
	return session, nil
}

func Open(path string, options OpenOptions) (*Session, error) {
	return openWithStorage(osSessionStorage{}, path, options)
}

func openWithStorage(storage sessionStorage, path string, options OpenOptions) (*Session, error) {
	resolvedPath, err := resolveSessionPath(path)
	if err != nil {
		return nil, err
	}
	claim, err := claimSessionWriter(resolvedPath)
	if err != nil {
		return nil, err
	}
	claimed := true
	defer func() {
		if claimed {
			releaseSessionWriter(claim)
		}
	}()
	data, err := storage.read(resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrStorage, resolvedPath, err)
	}
	if err := checkSessionLimits(data); err != nil {
		return nil, fmt.Errorf("%w: %s", err, resolvedPath)
	}
	version, err := sessionVersion(data)
	if err != nil {
		return nil, err
	}
	if version < 3 {
		migrated, err := migrateLegacySession(resolvedPath, data, version, normalizeRuntime(options.Now, options.NewEntryID).newEntryID)
		if err != nil {
			return nil, err
		}
		// Migration is pure, but it is not trusted merely because it encoded.
		// Validate the exact candidate against every current v3 invariant before
		// any rename can replace evidence from the legacy source.
		if _, _, _, _, err := decodeSessionFile(resolvedPath, migrated); err != nil {
			return nil, err
		}
		if err := storage.validateReplace(resolvedPath); err != nil {
			return nil, err
		}
		// A cooperating writer is excluded by the process lock held with the
		// claim. Re-read the source snapshot so a non-cooperating mutation is
		// never silently overwritten.
		current, readErr := storage.read(resolvedPath)
		if readErr != nil {
			return nil, fmt.Errorf("%w: reread migration source %s: %w", ErrStorage, resolvedPath, readErr)
		}
		if !bytes.Equal(current, data) {
			return nil, fmt.Errorf("%w: migration source changed before publication", ErrStorage)
		}
		replaced, replaceErr := storage.replace(resolvedPath, migrated)
		if replaceErr != nil {
			if replaced {
				return nil, fmt.Errorf("%w: legacy migration publication: %w", ErrDurabilityUnknown, replaceErr)
			}
			return nil, fmt.Errorf("%w: legacy migration publication: %w", ErrStorage, replaceErr)
		}
		if err := refreshSessionWriterAfterRewrite(claim, resolvedPath); err != nil {
			// The target already names the migrated bytes. Identity adoption is
			// therefore a post-publication failure even when the new inode cannot
			// be statted or locked; never return a writable aggregate.
			return nil, fmt.Errorf("%w: adopt migrated session identity: %w", ErrDurabilityUnknown, err)
		}
		data = migrated
	}
	header, entries, byID, needsSeparator, err := decodeSessionFile(resolvedPath, data)
	if err != nil {
		return nil, err
	}
	if err := refreshSessionWriter(claim, resolvedPath); err != nil {
		return nil, err
	}
	leaf := -1
	if len(entries) > 0 {
		leaf = len(entries) - 1
	}
	session := &Session{
		appendGate:     make(chan struct{}, 1),
		storage:        storage,
		path:           resolvedPath,
		header:         header,
		entries:        entries,
		byID:           byID,
		leaf:           leaf,
		needsSeparator: needsSeparator,
		runtime:        normalizeRuntime(options.Now, options.NewEntryID),
		writerClaim:    claim,
	}
	claimed = false
	return session, nil
}

func (s *Session) Path() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

func (s *Session) Header() Header {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.header.clone()
}

func (s *Session) Entries() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries := make([]Entry, len(s.entries))
	for index, entry := range s.entries {
		entries[index] = entry.clone()
	}
	return entries
}

func (s *Session) LeafID() (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.leaf < 0 {
		return "", false
	}
	return s.entries[s.leaf].id, true
}

func (s *Session) Context() Context {
	return s.BuildContext()
}

// BuildContext projects only the selected root-to-leaf path. The newest
// compaction on that path replaces its summarized prefix with one explicit
// checkpoint message; siblings and older summarized messages never leak into
// provider context.
func (s *Session) BuildContext() Context {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.buildContextLocked()
}

func (s *Session) buildContextLocked() Context {
	if s.leaf < 0 {
		return Context{thinkingLevel: "off", hasThinkingLevel: true}
	}

	path := make([]int, 0, len(s.entries))
	current := s.leaf
	for current >= 0 {
		path = append(path, current)
		entry := s.entries[current]
		if !entry.hasParent {
			break
		}
		current = s.byID[entry.parentID]
	}

	compactionIndex := -1
	for index := 0; index < len(path); index++ {
		entry := s.entries[path[index]]
		if entry.compaction != nil {
			compactionIndex = index
			break
		}
	}

	// Settings are not conversation messages, but they are branch state. Scan
	// the whole selected path first so compaction cannot accidentally erase the
	// current selection.
	context := Context{thinkingLevel: "off", hasThinkingLevel: true}
	for index := len(path) - 1; index >= 0; index-- {
		applyContextSettings(&context, s.entries[path[index]])
	}
	if compactionIndex >= 0 {
		compaction := s.entries[path[compactionIndex]]
		summary, err := llm.NewUserTextMessage(CompactionSummaryPrefix+compaction.compaction.Summary+CompactionSummarySuffix, compaction.timestamp)
		if err == nil {
			context.messages = append(context.messages, summary)
			checkpoint, checkpointErr := agentmsg.NewCompactionSummary(agentmsg.CompactionSummary{Summary: compaction.compaction.Summary, TokensBefore: compaction.compaction.TokensBefore, At: compaction.timestamp})
			if checkpointErr == nil {
				context.agentMessages = append(context.agentMessages, checkpoint)
			}
		}
		firstKeptIndex := -1
		for index := 0; index < len(path); index++ {
			if s.entries[path[index]].id == compaction.compaction.FirstKeptEntryID {
				firstKeptIndex = index
				break
			}
		}
		// path is leaf-to-root. From firstKept down to the leaf this includes the
		// retained pre-checkpoint segment and post-checkpoint successors; only the
		// checkpoint record itself is replaced by the summary above.
		for index := firstKeptIndex; index >= 0; index-- {
			if index == compactionIndex {
				continue
			}
			entry := s.entries[path[index]]
			appendEntryToContext(&context, entry)
			if entry.hasAssistant {
				context.assistant = entry.assistant
				context.hasAssistant = true
			}
			context.diagnostics = append(context.diagnostics, entry.diagnostics...)
		}
		return context
	}

	for index := len(path) - 1; index >= 0; index-- {
		entry := s.entries[path[index]]
		appendEntryToContext(&context, entry)
		if entry.hasAssistant {
			context.assistant = entry.assistant
			context.hasAssistant = true
		}
		context.diagnostics = append(context.diagnostics, entry.diagnostics...)
	}
	return context
}

func appendEntryToContext(context *Context, entry Entry) {
	if message, ok := entry.AgentMessage(); ok {
		context.agentMessages = append(context.agentMessages, message)
		converted, err := agentmsg.ConvertToLLM([]agentmsg.Message{message})
		if err == nil {
			context.messages = append(context.messages, converted...)
		}
		return
	}
	switch payload := entry.Payload().(type) {
	case BranchSummaryPayload:
		message, err := agentmsg.NewBranchSummary(agentmsg.BranchSummary{FromID: payload.FromID, Summary: payload.Summary, At: entry.timestamp})
		if err != nil {
			return
		}
		context.agentMessages = append(context.agentMessages, message)
		converted, err := agentmsg.ConvertToLLM([]agentmsg.Message{message})
		if err == nil {
			context.messages = append(context.messages, converted...)
		}
	}
}

func applyContextSettings(context *Context, entry Entry) {
	switch payload := entry.Payload().(type) {
	case ThinkingLevelChangePayload:
		context.thinkingLevel = payload.ThinkingLevel
		context.hasThinkingLevel = true
	case ModelChangePayload:
		context.model = ModelSelection{Provider: payload.Provider, ModelID: payload.ModelID}
		context.hasModel = true
	}
	if entry.hasAssistant {
		context.model = ModelSelection{Provider: entry.assistant.Provider, ModelID: entry.assistant.Model}
		context.hasModel = true
	}
}

// Append commits one entry. Cancellation can win while waiting for another
// append or before storage crosses its write boundary. Once writing begins,
// Append ignores later cancellation and synchronously returns the settled
// durable result (success or ErrCommitUnknown).
func (s *Session) Append(ctx context.Context, message llm.ConversationMessage, options AppendOptions) (Entry, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.acquireAppend(ctx); err != nil {
		return Entry{}, err
	}
	defer s.releaseAppend()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Entry{}, ErrClosed
	}
	if s.poisoned {
		s.mu.Unlock()
		return Entry{}, ErrPoisoned
	}
	if err := llm.ValidateConversationMessage(message); err != nil {
		s.mu.Unlock()
		return Entry{}, err
	}
	messageJSON, err := encodeMessage(message, options)
	if err != nil {
		s.mu.Unlock()
		return Entry{}, err
	}
	entryID, err := s.nextEntryID()
	if err != nil {
		s.mu.Unlock()
		return Entry{}, err
	}
	timestamp := canonicalTime(s.runtime.now())
	if timestamp.IsZero() {
		s.mu.Unlock()
		return Entry{}, fmt.Errorf("%w: zero entry timestamp", ErrInvalidEntry)
	}
	if err := validateISOTime(timestamp); err != nil {
		s.mu.Unlock()
		return Entry{}, fmt.Errorf("%w: entry timestamp: %v", ErrInvalidEntry, err)
	}

	parentID := ""
	hasParent := s.leaf >= 0
	if hasParent {
		parentID = s.entries[s.leaf].id
	}
	raw, err := encodeMessageEntry(entryID, parentID, hasParent, timestamp, messageJSON)
	if err != nil {
		s.mu.Unlock()
		return Entry{}, fmt.Errorf("%w: encode entry: %w", ErrInvalidEntry, err)
	}
	// Build the in-memory value from the exact bytes that will be committed.
	// This keeps timestamps, raw tool arguments, and provenance identical before
	// and after reopen.
	entry, err := decodeEntry(raw)
	if err != nil {
		s.mu.Unlock()
		return Entry{}, fmt.Errorf("%w: decode encoded entry: %w", ErrInvalidEntry, err)
	}
	appendBytes := make([]byte, 0, len(raw)+2)
	if s.needsSeparator {
		appendBytes = append(appendBytes, '\n')
	}
	appendBytes = append(appendBytes, raw...)
	appendBytes = append(appendBytes, '\n')
	s.mu.Unlock()

	started, err := s.storage.append(ctx, s.path, appendBytes)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if started {
			s.poisoned = true
			return Entry{}, fmt.Errorf("%w: %w", ErrCommitUnknown, err)
		}
		if cause := context.Cause(ctx); cause != nil && errors.Is(err, cause) {
			return Entry{}, fmt.Errorf("%w: %w", ErrAppendCanceled, err)
		}
		return Entry{}, fmt.Errorf("%w: append %s: %w", ErrStorage, s.path, err)
	}

	s.entries = append(s.entries, entry)
	s.leaf = len(s.entries) - 1
	s.byID[entry.id] = s.leaf
	s.needsSeparator = false
	s.generation++
	return entry.clone(), nil
}

// AppendPayload appends one non-message member of pi's v3 SessionEntry union.
// MessagePayload is intentionally rejected: callers must use Append so
// assistant provenance and replay metadata cannot be accidentally omitted.
func (s *Session) AppendPayload(ctx context.Context, payload EntryPayload) (Entry, error) {
	return s.appendPayload(ctx, payload, nil, false)
}

// AppendPayloadAt appends a non-message entry with an explicit parent and only
// advances the selected leaf after the durable append succeeds. A nil parent
// creates a root entry. It is the atomic primitive behind tree navigation.
func (s *Session) AppendPayloadAt(ctx context.Context, parentID *string, payload EntryPayload) (Entry, error) {
	return s.appendPayload(ctx, payload, parentID, true)
}

func (s *Session) appendPayload(ctx context.Context, payload EntryPayload, selectedParent *string, explicitParent bool) (Entry, error) {
	if payload == nil {
		return Entry{}, fmt.Errorf("%w: nil entry payload", ErrInvalidEntry)
	}
	if message, ok := payload.(MessagePayload); ok {
		if explicitParent {
			return Entry{}, fmt.Errorf("%w: explicit-parent message payload", ErrInvalidEntry)
		}
		return s.AppendAgentMessage(ctx, message.Message, AppendOptions{})
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.acquireAppend(ctx); err != nil {
		return Entry{}, err
	}
	defer s.releaseAppend()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Entry{}, ErrClosed
	}
	if s.poisoned {
		s.mu.Unlock()
		return Entry{}, ErrPoisoned
	}
	entryID, err := s.nextEntryID()
	if err != nil {
		s.mu.Unlock()
		return Entry{}, err
	}
	timestamp := canonicalTime(s.runtime.now())
	if timestamp.IsZero() || validateISOTime(timestamp) != nil {
		s.mu.Unlock()
		return Entry{}, fmt.Errorf("%w: invalid entry timestamp", ErrInvalidEntry)
	}
	parentID := ""
	hasParent := false
	if explicitParent {
		if selectedParent != nil {
			if _, ok := s.byID[*selectedParent]; !ok {
				s.mu.Unlock()
				return Entry{}, fmt.Errorf("%w: %s", ErrEntryNotFound, *selectedParent)
			}
			parentID, hasParent = *selectedParent, true
		}
	} else if s.leaf >= 0 {
		parentID, hasParent = s.entries[s.leaf].id, true
	}
	raw, err := encodePayloadEntry(entryID, parentID, hasParent, timestamp, payload.CloneEntryPayload())
	if err != nil {
		s.mu.Unlock()
		return Entry{}, fmt.Errorf("%w: encode payload: %w", ErrInvalidEntry, err)
	}
	entry, err := decodeEntry(raw)
	if err != nil {
		s.mu.Unlock()
		return Entry{}, fmt.Errorf("%w: decode payload: %w", ErrInvalidEntry, err)
	}
	appendBytes := make([]byte, 0, len(raw)+2)
	if s.needsSeparator {
		appendBytes = append(appendBytes, '\n')
	}
	appendBytes = append(appendBytes, raw...)
	appendBytes = append(appendBytes, '\n')
	s.mu.Unlock()
	started, err := s.storage.append(ctx, s.path, appendBytes)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if started {
			s.poisoned = true
			return Entry{}, fmt.Errorf("%w: %w", ErrCommitUnknown, err)
		}
		if cause := context.Cause(ctx); cause != nil && errors.Is(err, cause) {
			return Entry{}, fmt.Errorf("%w: %w", ErrAppendCanceled, err)
		}
		return Entry{}, fmt.Errorf("%w: append %s: %w", ErrStorage, s.path, err)
	}
	s.entries = append(s.entries, entry)
	s.leaf = len(s.entries) - 1
	s.byID[entry.id] = s.leaf
	s.needsSeparator = false
	s.generation++
	return entry.clone(), nil
}

// AppendAgentMessage is the v3 `type:"message"` boundary. It persists the
// full AgentMessage union; only its later Context projection invokes
// agentmsg.ConvertToLLM.
func (s *Session) AppendAgentMessage(ctx context.Context, message agentmsg.Message, options AppendOptions) (Entry, error) {
	if message == nil {
		return Entry{}, fmt.Errorf("%w: nil agent message", ErrInvalidEntry)
	}
	if standard, ok := message.(agentmsg.LLM); ok {
		return s.Append(ctx, standard.Conversation(), options)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.acquireAppend(ctx); err != nil {
		return Entry{}, err
	}
	defer s.releaseAppend()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Entry{}, ErrClosed
	}
	if s.poisoned {
		s.mu.Unlock()
		return Entry{}, ErrPoisoned
	}
	messageJSON, err := encodeAgentMessage(message)
	if err != nil {
		s.mu.Unlock()
		return Entry{}, err
	}
	entryID, err := s.nextEntryID()
	if err != nil {
		s.mu.Unlock()
		return Entry{}, err
	}
	timestamp := canonicalTime(s.runtime.now())
	if timestamp.IsZero() || validateISOTime(timestamp) != nil {
		s.mu.Unlock()
		return Entry{}, fmt.Errorf("%w: invalid entry timestamp", ErrInvalidEntry)
	}
	parentID := ""
	hasParent := s.leaf >= 0
	if hasParent {
		parentID = s.entries[s.leaf].id
	}
	raw, err := encodeMessageEntry(entryID, parentID, hasParent, timestamp, messageJSON)
	if err != nil {
		s.mu.Unlock()
		return Entry{}, err
	}
	entry, err := decodeEntry(raw)
	if err != nil {
		s.mu.Unlock()
		return Entry{}, err
	}
	appendBytes := make([]byte, 0, len(raw)+2)
	if s.needsSeparator {
		appendBytes = append(appendBytes, '\n')
	}
	appendBytes = append(appendBytes, raw...)
	appendBytes = append(appendBytes, '\n')
	s.mu.Unlock()
	started, err := s.storage.append(ctx, s.path, appendBytes)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if started {
			s.poisoned = true
			return Entry{}, fmt.Errorf("%w: %w", ErrCommitUnknown, err)
		}
		return Entry{}, fmt.Errorf("%w: append: %w", ErrStorage, err)
	}
	s.entries = append(s.entries, entry)
	s.leaf = len(s.entries) - 1
	s.byID[entry.id] = s.leaf
	s.needsSeparator = false
	s.generation++
	return entry.clone(), nil
}

func (s *Session) Poisoned() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.poisoned
}

func (s *Session) Close() error {
	s.acquireAppendUncancelable()
	defer s.releaseAppend()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.writerClaim != nil {
		releaseSessionWriter(s.writerClaim)
		s.writerClaim = nil
	}
	return nil
}

func (s *Session) acquireAppend(ctx context.Context) error {
	if cause := context.Cause(ctx); cause != nil {
		return fmt.Errorf("%w: %w", ErrAppendCanceled, cause)
	}
	select {
	case s.appendGate <- struct{}{}:
		if cause := context.Cause(ctx); cause != nil {
			s.releaseAppend()
			return fmt.Errorf("%w: %w", ErrAppendCanceled, cause)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrAppendCanceled, context.Cause(ctx))
	}
}

func (s *Session) acquireAppendUncancelable() {
	s.appendGate <- struct{}{}
}

func (s *Session) releaseAppend() {
	<-s.appendGate
}

func (s *Session) nextEntryID() (string, error) {
	for attempt := 0; attempt < entryIDAttempts; attempt++ {
		id, err := s.runtime.newEntryID()
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrIDGeneration, err)
		}
		if err := validateOpaqueID(id, "entry id"); err != nil {
			return "", fmt.Errorf("%w: %v", ErrInvalidEntry, err)
		}
		if _, exists := s.byID[id]; !exists {
			return id, nil
		}
	}
	return "", ErrEntryIDExhausted
}

func normalizeRuntime(now Clock, newEntryID IDGenerator) runtimeConfig {
	if now == nil {
		now = time.Now
	}
	if newEntryID == nil {
		newEntryID = func() (string, error) { return randomHex(4) }
	}
	return runtimeConfig{now: panicSafeClock(now), newEntryID: panicSafeIDGenerator(newEntryID)}
}

// panicSafeClock converts a host callback panic into the clock's existing
// invalid-value channel. Call sites already reject zero timestamps with their
// operation-specific ErrInvalidSession/ErrInvalidEntry classification.
func panicSafeClock(clock Clock) Clock {
	return func() (value time.Time) {
		completed := false
		defer func() {
			if !completed {
				_ = recover()
				value = time.Time{}
			}
		}()
		value = clock()
		completed = true
		return value
	}
}

// panicSafeIDGenerator preserves ordinary callback results and errors. Panic
// payloads are deliberately discarded because host panic text may contain
// credentials or other sensitive data; nextEntryID retains ErrIDGeneration.
func panicSafeIDGenerator(generator IDGenerator) IDGenerator {
	return func() (id string, err error) {
		completed := false
		defer func() {
			if !completed {
				_ = recover()
				id = ""
				err = errIDGeneratorCallbackPanicked
			}
		}()
		id, err = generator()
		completed = true
		return id, err
	}
}

func newSessionID(timestamp time.Time) (string, error) {
	milliseconds := timestamp.UnixMilli()
	if milliseconds < 0 || uint64(milliseconds) >= 1<<48 {
		return "", fmt.Errorf("timestamp is outside UUIDv7 range")
	}
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	value := uint64(milliseconds)
	buffer[0] = byte(value >> 40)
	buffer[1] = byte(value >> 32)
	buffer[2] = byte(value >> 24)
	buffer[3] = byte(value >> 16)
	buffer[4] = byte(value >> 8)
	buffer[5] = byte(value)
	buffer[6] = (buffer[6] & 0x0f) | 0x70
	buffer[8] = (buffer[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(buffer)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

// NewSessionID generates the UUIDv7-shaped identifier used by Create when no
// explicit ID is supplied. Application assembly uses it once so a default
// session filename and its durable header share the same identifier.
func NewSessionID(timestamp time.Time) (string, error) {
	return newSessionID(timestamp)
}

func randomHex(byteCount int) (string, error) {
	buffer := make([]byte, byteCount)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func resolveSessionPath(path string) (string, error) {
	if !utf8.ValidString(path) || strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%w: path must be non-empty valid UTF-8", ErrInvalidSession)
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: resolve path: %w", ErrInvalidSession, err)
	}
	resolved = filepath.Clean(resolved)
	// A final-component symlink names the target session, not an independent
	// replacement destination. Resolve that component so migration/recovery do
	// not replace the link itself. Preserve ordinary lexical paths (including
	// platform aliases such as macOS /var -> /private/var) for API compatibility;
	// the writer descriptor separately canonicalizes them for locking.
	if info, lstatErr := os.Lstat(resolved); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
		if target, evalErr := filepath.EvalSymlinks(resolved); evalErr == nil {
			return filepath.Clean(target), nil
		}
	}
	return resolved, nil
}

func resolveWorkingDir(path string) (string, error) {
	if !utf8.ValidString(path) || strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%w: working directory must be non-empty valid UTF-8", ErrInvalidSession)
	}
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: resolve working directory: %w", ErrInvalidSession, err)
	}
	return filepath.Clean(resolved), nil
}

func validateOpaqueID(value, field string) error {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must be non-empty valid UTF-8", field)
	}
	return nil
}

// ValidateSessionID applies the same side-effect-free ID rule used by Create.
// Application assembly may call it before provisioning durable directories.
func ValidateSessionID(value string) error {
	if !utf8.ValidString(value) || !validSessionIDPattern.MatchString(value) {
		return fmt.Errorf("%w: %w: session id must be non-empty, contain only alphanumeric characters, '-', '_', and '.', and start and end with an alphanumeric character", ErrInvalidSession, ErrInvalidSessionID)
	}
	return nil
}

func formatISOTime(value time.Time) string {
	return canonicalTime(value).Format("2006-01-02T15:04:05.000Z")
}

func canonicalTime(value time.Time) time.Time {
	return value.UTC().Truncate(time.Millisecond)
}

func validateISOTime(value time.Time) error {
	canonical := canonicalTime(value)
	parsed, err := time.Parse(time.RFC3339, formatISOTime(canonical))
	if err != nil || !parsed.Equal(canonical) {
		return fmt.Errorf("cannot be represented as an RFC3339 millisecond timestamp")
	}
	return nil
}

type newHeaderWire struct {
	Type          string `json:"type"`
	Version       int    `json:"version"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	WorkingDir    string `json:"cwd"`
	ParentSession string `json:"parentSession,omitempty"`
}

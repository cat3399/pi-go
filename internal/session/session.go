package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
)

const entryIDAttempts = 100

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
	wire := newHeaderWire{
		Type:       "session",
		Version:    3,
		ID:         sessionID,
		Timestamp:  formatISOTime(timestamp),
		WorkingDir: workingDir,
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
			id:         sessionID,
			workingDir: workingDir,
			timestamp:  timestamp,
			raw:        raw,
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.leaf < 0 {
		return Context{}
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

	context := Context{}
	for index := len(path) - 1; index >= 0; index-- {
		entry := s.entries[path[index]]
		if entry.message != nil {
			context.messages = append(context.messages, entry.message)
		}
		if entry.hasAssistant {
			context.assistant = entry.assistant
			context.hasAssistant = true
		}
		context.diagnostics = append(context.diagnostics, entry.diagnostics...)
	}
	return context
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
	return runtimeConfig{now: now, newEntryID: newEntryID}
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
	return filepath.Clean(resolved), nil
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
	if err := validateOpaqueID(value, "session id"); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSession, err)
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
	Type       string `json:"type"`
	Version    int    `json:"version"`
	ID         string `json:"id"`
	Timestamp  string `json:"timestamp"`
	WorkingDir string `json:"cwd"`
}

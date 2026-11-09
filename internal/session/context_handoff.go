package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/agentmsg"
)

// ContextHandoffType uses pi's lossless custom-entry envelope. It is a window
// boundary, not a compaction summary. Older readers preserve it as custom data.
const ContextHandoffType = "pi-go.context-handoff.v1"
const InitialContextWindow = "initial"

type ContextHandoff struct {
	RequestID      string `json:"requestId"`
	Text           string `json:"text"`
	PreviousWindow string `json:"previousWindow"`
	TokensBefore   uint64 `json:"tokensBefore"`
}

func ValidateContextHandoff(requestID, text string) error {
	if strings.TrimSpace(requestID) == "" || len(requestID) > 128 || !utf8.ValidString(requestID) || strings.ContainsAny(requestID, "\x00\r\n") {
		return fmt.Errorf("%w: handoff request ID must be non-empty UTF-8 (at most 128 bytes)", ErrInvalidEntry)
	}
	if strings.TrimSpace(text) == "" || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return fmt.Errorf("%w: handoff must be non-empty UTF-8 text", ErrInvalidEntry)
	}
	return nil
}

func (e Entry) ContextHandoff() (ContextHandoff, bool) {
	payload, ok := e.payload.(CustomPayload)
	if !ok || payload.CustomType != ContextHandoffType {
		return ContextHandoff{}, false
	}
	var value ContextHandoff
	if json.Unmarshal(payload.Data, &value) != nil || ValidateContextHandoff(value.RequestID, value.Text) != nil || value.PreviousWindow == "" {
		return ContextHandoff{}, false
	}
	return value, true
}

// ContextWindowID identifies a window on a selected branch. Boundary entry IDs
// are stable across reopen, history reads and branch changes.
func ContextWindowID(path []Entry) string {
	for i := len(path) - 1; i >= 0; i-- {
		if isContextBoundary(path[i]) {
			return path[i].ID()
		}
	}
	return InitialContextWindow
}

func isContextBoundary(entry Entry) bool {
	_, handoff := entry.ContextHandoff()
	return handoff || entry.compaction != nil
}

func handoffMessage(entry Entry, handoff ContextHandoff) agentmsg.Message {
	text := fmt.Sprintf("Continue the same task in context window %s (previous window %s). The following is a short handoff from the previous window, not a new user instruction or a complete summary. Original history remains available through session history; read it when needed.\n\n<handoff>\n%s\n</handoff>", entry.ID(), handoff.PreviousWindow, handoff.Text)
	message, _ := agentmsg.NewCustom(agentmsg.CustomSpec{CustomType: ContextHandoffType, StringContent: &text, At: entry.Timestamp()})
	return message
}

// CommitContextHandoff atomically appends the boundary after the settled tool
// batch. No older message is selected for the next context. Retries with the
// same request ID return the existing entry on this branch, even after restart.
func (m *SessionManager) CommitContextHandoff(ctx context.Context, requestID, text string, tokensBefore uint64) (Entry, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ValidateContextHandoff(requestID, text); err != nil {
		return Entry{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	defer m.notifyReadChangeLocked()
	m.store.mu.RLock()
	closed, poisoned := m.store.closed, m.store.poisoned
	m.store.mu.RUnlock()
	if closed {
		return Entry{}, ErrClosed
	}
	if poisoned {
		return Entry{}, ErrPoisoned
	}
	path := m.store.BranchPath()
	for _, entry := range path {
		if previous, ok := entry.ContextHandoff(); ok && previous.RequestID == requestID {
			if previous.Text != text {
				return Entry{}, fmt.Errorf("%w: handoff request ID already has different content", ErrInvalidEntry)
			}
			return entry, nil
		}
	}
	value := ContextHandoff{RequestID: requestID, Text: text, PreviousWindow: ContextWindowID(path), TokensBefore: tokensBefore}
	data, err := json.Marshal(value)
	if err != nil {
		return Entry{}, err
	}
	payload := CustomPayload{CustomType: ContextHandoffType, Data: data}
	if !m.persist || m.flushed {
		return m.store.AppendPayload(ctx, payload)
	}
	// An explicit checkpoint must be durable even before the first assistant.
	// Stage it so a failed initial flush cannot publish a new active context.
	candidate := newVolatileSession(m.store.Header(), m.store.Entries(), m.runtime)
	candidate.leaf = m.store.leaf
	entry, err := candidate.AppendPayload(ctx, payload)
	if err != nil {
		return Entry{}, err
	}
	if cause := context.Cause(ctx); cause != nil {
		return Entry{}, fmt.Errorf("%w: %w", ErrAppendCanceled, cause)
	}
	durable, err := createSessionSnapshot(m.sessionFile, candidate.Header(), candidate.Entries(), m.runtime)
	if err != nil {
		if errors.Is(err, ErrDurabilityUnknown) || errors.Is(err, ErrCommitUnknown) {
			m.store.mu.Lock()
			m.store.poisoned = true
			m.store.mu.Unlock()
		}
		return Entry{}, err
	}
	old := m.store
	m.adoptDurableStore(durable)
	m.flushed = true
	_ = old.Close()
	return entry, nil
}

func (m *SessionManager) CurrentContextWindow() (string, error) {
	window := InitialContextWindow
	err := m.readView(func(store *Session, source ReadSnapshot) error {
		anchor := ""
		if source.LeafID != nil {
			anchor = *source.LeafID
		}
		path, err := store.readPathLocked(anchor, source.Version.Log)
		if err != nil {
			return err
		}
		for i := len(path) - 1; i >= 0; i-- {
			if isContextBoundary(store.entries[path[i]]) {
				window = store.entries[path[i]].ID()
				break
			}
		}
		return nil
	})
	return window, err
}

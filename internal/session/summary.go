package session

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// sessionInfoSummary follows pi's buildSessionInfo projection over the complete
// append log, independently of the selected branch or compacted context.
type sessionInfoSummary struct {
	messageCount int
	firstMessage string
	name         string
	lastActivity time.Time
}

// append returns searchable user/assistant text only when requested. In
// particular, tool output and later message bodies need not be decoded to text
// to maintain the catalog summary.
func (s *sessionInfoSummary) append(typeName string, object map[string]json.RawMessage, collectText bool) string {
	if typeName == "session_info" {
		var name string
		_ = json.Unmarshal(object["name"], &name)
		s.name = strings.TrimSpace(name)
		return ""
	}
	if typeName != "message" {
		return ""
	}
	s.messageCount++
	text, role, activity, hasContent := sessionInfoMessage(object, collectText || s.firstMessage == "")
	if role != "user" && role != "assistant" {
		return ""
	}
	if hasContent && activity.After(time.UnixMilli(0)) && activity.After(s.lastActivity) {
		s.lastActivity = activity
	}
	if s.firstMessage == "" && role == "user" && text != "" {
		s.firstMessage = text
	}
	return text
}

func (s sessionInfoSummary) apply(info SessionInfo) SessionInfo {
	info.MessageCount = s.messageCount
	info.FirstMessage = s.firstMessage
	if info.FirstMessage == "" {
		info.FirstMessage = "(no messages)"
	}
	info.Name, info.HasName = s.name, s.name != ""
	if !s.lastActivity.IsZero() {
		info.Modified = s.lastActivity
	} else if !info.Created.IsZero() {
		info.Modified = info.Created
	}
	return info
}

type managerSummary struct {
	store   *Session
	entries int
	value   sessionInfoSummary
}

// Summary returns a consistent, immutable catalog projection, without copying
// entries or retaining aggregate search text. Each accepted entry is examined
// at most once per Store binding; subsequent reads process only appended entries.
// It includes in-memory messages before the first-assistant persistence boundary.
func (m *SessionManager) Summary() SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	m.summaryMu.Lock()
	defer m.summaryMu.Unlock()
	store := m.store
	store.mu.RLock()
	defer store.mu.RUnlock()
	return m.summaryLocked(store)
}

// summaryLocked requires m.mu, m.summaryMu and store.mu, in that order.
func (m *SessionManager) summaryLocked(store *Session) SessionInfo {
	if m.summary.store != store {
		m.summary = managerSummary{store: store}
	}
	for _, entry := range store.entries[m.summary.entries:] {
		m.summary.value.appendEntry(entry)
	}
	m.summary.entries = len(store.entries)
	header := store.header
	return m.summary.value.apply(SessionInfo{
		Path: m.sessionFile, ID: header.id, Cwd: m.cwd, Created: header.timestamp,
		ParentSessionPath: header.parentSession, HasParentSession: header.hasParentSession,
	})
}

// appendEntry is shared by live summaries and the streaming read-index builder.
// The builder calls it before dropping raw/decoded bodies from its projection.
func (s *sessionInfoSummary) appendEntry(entry Entry) {
	if entry.typeName == "message" {
		s.messageCount++
		if entry.messageActivity.After(time.UnixMilli(0)) && entry.messageActivity.After(s.lastActivity) {
			s.lastActivity = entry.messageActivity
		}
		if s.firstMessage == "" && entry.messageRole == "user" {
			var object map[string]json.RawMessage
			semantic, _ := replaceInvalidUTF8LikeNode(entry.raw)
			if json.Unmarshal(semantic, &object) == nil {
				text, _, _, _ := sessionInfoMessage(object, true)
				s.firstMessage = text
			}
		}
		return
	}
	if entry.typeName == "session_info" {
		var object map[string]json.RawMessage
		semantic, _ := replaceInvalidUTF8LikeNode(entry.raw)
		if json.Unmarshal(semantic, &object) == nil {
			s.append(entry.typeName, object, false)
		}
	}
}

// setStore is called under m.mu exclusively (or during construction), excluding
// Summary readers as well. Drop the old reference so replacement cannot retain
// its log. The separate summaryMu lets ordinary Store readers run concurrently.
func (m *SessionManager) setStore(store *Session) {
	m.store = store
	m.summary = managerSummary{}
	m.readEpoch = ""
	m.readOnce = sync.Once{}
}

// The first assistant makes the existing log durable; it does not replace its
// logical data generation or selected-view revision. The new Store has not yet
// been published, and m.mu excludes every manager reader and writer here.
func (m *SessionManager) adoptDurableStore(store *Session) {
	m.store.mu.RLock()
	store.generation = m.store.generation
	m.store.mu.RUnlock()
	m.store = store
	m.summary = managerSummary{}
}

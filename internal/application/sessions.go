package application

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/host"
	"github.com/cat3399/pi-go/internal/session"
)

// SessionInfo is the surface-neutral session-list projection. Presentation
// adapters decide how to format timestamps and which aliases to expose.
type SessionInfo struct {
	Path            string
	ID              string
	CWD             string
	Name            string
	Created         time.Time
	Modified        time.Time
	MessageCount    int
	FirstMessage    string
	ParentSessionID string
}

// SessionSnapshot is one consistent read of durable session state plus the
// optional live Host state. Entries and trees retain the canonical session
// types so surfaces do not invent a second conversation model.
type SessionSnapshot struct {
	SessionID string
	FilePath  string
	Info      SessionInfo
	LeafID    *string
	Tree      []session.TreeNode
	Entries   []session.Entry
	Context   session.Context
	LiveState *host.State
}

func (s *Supervisor) ListSessions() ([]SessionInfo, error) {
	discovered, err := session.ListAllSessionsInAgentDir(s.paths.AgentDir, nil)
	if err != nil {
		return nil, err
	}
	pathIDs := make(map[string]string, len(discovered))
	for _, info := range discovered {
		pathIDs[cleanPathKey(info.Path)] = info.ID
	}
	byID := make(map[string]SessionInfo, len(discovered))
	for _, info := range discovered {
		byID[info.ID] = discoveredSessionInfo(info, pathIDs)
	}
	for _, managed := range s.activeSessions() {
		managedID, _, _ := managed.identity()
		if _, exists := byID[managedID]; exists {
			continue
		}
		manager := managed.manager()
		if manager == nil {
			continue
		}
		byID[managedID] = activeSessionInfo(manager, managed, pathIDs)
	}
	result := make([]SessionInfo, 0, len(byID))
	for _, info := range byID {
		result = append(result, info)
	}
	sort.SliceStable(result, func(left, right int) bool {
		return result[left].Modified.After(result[right].Modified)
	})
	return result, nil
}

func discoveredSessionInfo(info session.SessionInfo, pathIDs map[string]string) SessionInfo {
	parentID := ""
	if info.HasParentSession {
		parentID = pathIDs[cleanPathKey(info.ParentSessionPath)]
	}
	name := ""
	if info.HasName {
		name = info.Name
	}
	first := info.FirstMessage
	if first == "" {
		first = "(no messages)"
	}
	return SessionInfo{
		Path: info.Path, ID: info.ID, CWD: info.Cwd, Name: name,
		Created: info.Created, Modified: info.Modified,
		MessageCount: info.MessageCount, FirstMessage: first,
		ParentSessionID: parentID,
	}
}

func activeSessionInfo(manager *session.SessionManager, managed *managedSession, pathIDs map[string]string) SessionInfo {
	header := manager.Header()
	created := header.Timestamp()
	var lastActivity time.Time
	entries := manager.Entries()
	messageCount := 0
	firstMessage := "(no messages)"
	for _, entry := range entries {
		if entry.Type() != "message" {
			continue
		}
		messageCount++
		text, role, activity, hasContent := sessionEntryMessage(entry.RawJSON())
		if (role == "user" || role == "assistant") && hasContent && activity.After(time.UnixMilli(0)) && activity.After(lastActivity) {
			lastActivity = activity
		}
		if firstMessage == "(no messages)" && role == "user" && text != "" {
			firstMessage = text
		}
	}
	modified := created
	if !lastActivity.IsZero() {
		modified = lastActivity
	}
	name, _ := manager.SessionName()
	parentID := ""
	if parent, ok := header.ParentSession(); ok {
		parentID = pathIDs[cleanPathKey(parent)]
	}
	_, _, path := managed.identity()
	if current, ok := manager.SessionFile(); ok {
		path = current
	}
	return SessionInfo{
		Path: path, ID: manager.SessionID(), CWD: manager.Cwd(), Name: name,
		Created: created, Modified: modified, MessageCount: messageCount,
		FirstMessage: firstMessage, ParentSessionID: parentID,
	}
}

func sessionEntryMessage(raw []byte) (text, role string, activity time.Time, hasContent bool) {
	var entry map[string]json.RawMessage
	if json.Unmarshal(raw, &entry) != nil {
		return "", "", time.Time{}, false
	}
	var message map[string]json.RawMessage
	if json.Unmarshal(entry["message"], &message) != nil {
		return "", "", time.Time{}, false
	}
	_ = json.Unmarshal(message["role"], &role)
	content, hasContent := message["content"]
	var milliseconds int64
	if json.Unmarshal(message["timestamp"], &milliseconds) == nil {
		activity = time.UnixMilli(milliseconds)
	} else {
		var timestamp string
		_ = json.Unmarshal(entry["timestamp"], &timestamp)
		activity, _ = time.Parse(time.RFC3339, timestamp)
	}
	if json.Unmarshal(content, &text) == nil {
		return text, role, activity, hasContent
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return "", role, activity, hasContent
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, " "), role, activity, hasContent
}

func cleanPathKey(value string) string {
	if value == "" {
		return ""
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return filepath.Clean(value)
	}
	return filepath.Clean(absolute)
}

func (s *Supervisor) SnapshotSession(id, leafID string) (SessionSnapshot, error) {
	manager, managed, info, closeManager, err := s.sessionManagerForRead(id)
	if err != nil {
		return SessionSnapshot{}, err
	}
	if closeManager {
		defer manager.Close()
	}

	var entries []session.Entry
	var contextValue session.Context
	if leafID == "" {
		entries = manager.ContextEntries()
		contextValue = manager.BuildContext()
	} else {
		projection, projectionErr := manager.ProjectContextAt(leafID)
		if projectionErr != nil {
			return SessionSnapshot{}, projectionErr
		}
		entries, contextValue = projection.Entries, projection.Context
	}
	var leaf *string
	if value, ok := manager.LeafID(); ok {
		copy := value
		leaf = &copy
	}
	file, _ := manager.SessionFile()
	result := SessionSnapshot{
		SessionID: id, FilePath: file, Info: info, LeafID: leaf,
		Tree: manager.Tree(), Entries: entries, Context: contextValue,
	}
	if managed != nil {
		state, stateErr := managed.host.State()
		if stateErr != nil {
			return SessionSnapshot{}, stateErr
		}
		result.LiveState = &state
	}
	return result, nil
}

func (s *Supervisor) SessionExists(id string) (bool, error) {
	if _, ok := s.active(id); ok {
		return true, nil
	}
	_, found, err := s.findSession(id)
	return found, err
}

func (s *Supervisor) RenameSession(ctx context.Context, id, name string) error {
	ctx = normalizeSupervisorContext(ctx)
	if _, ok := s.active(id); ok {
		_, err := s.Dispatch(ctx, id, host.SetSessionNameCommand{Name: name})
		return err
	}
	manager, _, _, closeManager, err := s.sessionManagerForRead(id)
	if err != nil {
		return err
	}
	if closeManager {
		defer manager.Close()
	}
	_, err = manager.AppendSessionInfo(ctx, name)
	return err
}

func (s *Supervisor) sessionManagerForRead(id string) (*session.SessionManager, *managedSession, SessionInfo, bool, error) {
	if managed, ok := s.active(id); ok {
		manager := managed.manager()
		if manager == nil {
			return nil, nil, SessionInfo{}, false, errors.New("active session manager is unavailable")
		}
		infos, err := s.ListSessions()
		if err != nil {
			return nil, nil, SessionInfo{}, false, err
		}
		for _, info := range infos {
			if info.ID == id {
				return manager, managed, info, false, nil
			}
		}
		return manager, managed, activeSessionInfo(manager, managed, nil), false, nil
	}
	info, found, err := s.findSession(id)
	if err != nil {
		return nil, nil, SessionInfo{}, false, err
	}
	if !found {
		return nil, nil, SessionInfo{}, false, os.ErrNotExist
	}
	manager, err := session.OpenSessionManager(info.Path, filepath.Dir(info.Path), "")
	if err != nil {
		return nil, nil, SessionInfo{}, false, err
	}
	all, err := session.ListAllSessionsInAgentDir(s.paths.AgentDir, nil)
	if err != nil {
		_ = manager.Close()
		return nil, nil, SessionInfo{}, false, err
	}
	pathIDs := make(map[string]string, len(all))
	for _, item := range all {
		pathIDs[cleanPathKey(item.Path)] = item.ID
	}
	return manager, nil, discoveredSessionInfo(info, pathIDs), true, nil
}

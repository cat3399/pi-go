package application

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

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

// SessionSnapshot contains one consistent SessionManager read and an optional,
// independently sampled live state from the same Runtime binding. Version
// validates the manager resource, not LiveState or catalog-resolved parent IDs.
// Entries and trees retain canonical session types.
type SessionSnapshot struct {
	Revision    uint64
	Version     session.ReadVersion
	SessionID   string
	FilePath    string
	Info        SessionInfo
	LeafID      *string
	Tree        []session.TreeNode
	Entries     []session.Entry
	Context     session.Context
	LiveState   *State
	LatestUsage *session.AssistantUsage
}

func (s *Service) ListSessions() ([]SessionInfo, error) {
	discovered, live, err := s.sessionCatalogSnapshot()
	if err != nil {
		return nil, err
	}
	pathIDs := discoveredSessionPathIDs(discovered)
	byID := make(map[string]SessionInfo, len(discovered)+len(live))
	for _, info := range discovered {
		byID[info.ID] = discoveredSessionInfo(info, pathIDs)
	}
	for _, info := range live {
		byID[info.ID] = discoveredSessionInfo(info, pathIDs)
	}
	result := make([]SessionInfo, 0, len(byID))
	for _, info := range byID {
		result = append(result, info)
	}
	sort.SliceStable(result, func(left, right int) bool {
		if result[left].Modified.Equal(result[right].Modified) {
			return result[left].ID < result[right].ID
		}
		return result[left].Modified.After(result[right].Modified)
	})
	return result, nil
}

func (s *Service) sessionCatalogSnapshot() (discovered, live []session.SessionInfo, err error) {
	if s == nil || s.sessionCatalog == nil {
		return nil, nil, errors.New("session catalog is unavailable")
	}
	live = make([]session.SessionInfo, 0)
	for _, managed := range s.activeSessions() {
		if manager := managed.manager(); manager != nil {
			info := manager.Summary()
			if info.Path == "" {
				_, _, info.Path = managed.identity()
			}
			live = append(live, info)
		}
	}
	discovered, err = s.sessionCatalog.ListAllWithSummaries(live)
	return discovered, live, err
}

func (s *Service) discoveredSessions() ([]session.SessionInfo, error) {
	discovered, _, err := s.sessionCatalogSnapshot()
	return discovered, err
}

func discoveredSessionPathIDs(values []session.SessionInfo) map[string]string {
	result := make(map[string]string, len(values))
	for _, info := range values {
		result[cleanPathKey(info.Path)] = info.ID
	}
	return result
}

func findDiscoveredSession(values []session.SessionInfo, id string) (session.SessionInfo, bool) {
	for _, info := range values {
		if info.ID == id {
			return info, true
		}
	}
	return session.SessionInfo{}, false
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

func (s *Service) SnapshotSession(id, leafID string) (SessionSnapshot, error) {
	revision := s.CurrentRevision()
	manager, managed, closeManager, err := s.sessionManagerForRead(id)
	if err != nil {
		return SessionSnapshot{}, err
	}
	if closeManager {
		defer manager.Close()
	}

	options := session.SnapshotOptions{LeafID: leafID, IncludeContext: true, IncludeTree: true}
	var snapshot session.ReadSnapshot
	var liveState *State
	if managed != nil {
		snapshot, liveState, err = managed.session.snapshotHistory(options)
	} else {
		snapshot, err = manager.ReadSnapshot(options)
	}
	if err != nil {
		return SessionSnapshot{}, err
	}
	if snapshot.Info.ID != id {
		return SessionSnapshot{}, ErrSessionChanged
	}
	var pathIDs map[string]string
	if snapshot.Info.HasParentSession {
		discovered, discoverErr := s.discoveredSessions()
		if discoverErr != nil {
			return SessionSnapshot{}, discoverErr
		}
		pathIDs = discoveredSessionPathIDs(discovered)
	}
	return SessionSnapshot{
		Revision: revision, Version: snapshot.Version, SessionID: id, FilePath: snapshot.Info.Path,
		Info: discoveredSessionInfo(snapshot.Info, pathIDs), LeafID: snapshot.LeafID,
		Tree: snapshot.Tree, Entries: snapshot.Entries, Context: snapshot.Context, LiveState: liveState,
	}, nil
}

func (s *ApplicationSession) snapshotHistory(options session.SnapshotOptions) (session.ReadSnapshot, *State, error) {
	for attempt := 0; attempt < 2; attempt++ {
		current, binding, err := s.currentSession()
		if err != nil {
			return session.ReadSnapshot{}, nil, err
		}
		history, err := current.SessionManager().ReadSnapshot(options)
		var state State
		if err == nil {
			state, err = snapshotSession(current, s.promptRunning())
		}
		if !s.sameBinding(current, binding) {
			continue
		}
		if err != nil {
			return session.ReadSnapshot{}, nil, err
		}
		if history.Info.ID != state.SessionID {
			return session.ReadSnapshot{}, nil, ErrSessionChanged
		}
		return history, &state, nil
	}
	return session.ReadSnapshot{}, nil, ErrSessionChanged
}

func (s *Service) SessionExists(id string) (bool, error) {
	if _, ok := s.active(id); ok {
		return true, nil
	}
	_, found, err := s.findSession(id)
	return found, err
}

func (s *Service) RenameSession(ctx context.Context, id, name string) error {
	ctx = normalizeContext(ctx)
	if _, ok := s.active(id); ok {
		_, err := s.Dispatch(ctx, id, SetSessionNameCommand{Name: name})
		return err
	}
	manager, _, closeManager, err := s.sessionManagerForRead(id)
	if err != nil {
		return err
	}
	if closeManager {
		defer manager.Close()
	}
	_, err = manager.AppendSessionInfo(ctx, name)
	if err == nil {
		s.events.publish(Event{SessionID: id, Value: SessionCatalogEvent{Change: SessionUpdated}})
	}
	return err
}

func (s *Service) sessionManagerForRead(id string) (*session.SessionManager, *managedSession, bool, error) {
	if managed, ok := s.active(id); ok {
		manager := managed.manager()
		if manager == nil {
			return nil, nil, false, errors.New("active session manager is unavailable")
		}
		return manager, managed, false, nil
	}
	discovered, err := s.discoveredSessions()
	if err != nil {
		return nil, nil, false, err
	}
	info, found := findDiscoveredSession(discovered, id)
	if !found {
		return nil, nil, false, os.ErrNotExist
	}
	manager, err := session.OpenSessionManager(info.Path, filepath.Dir(info.Path), "")
	if err != nil {
		return nil, nil, false, err
	}
	return manager, nil, true, nil
}

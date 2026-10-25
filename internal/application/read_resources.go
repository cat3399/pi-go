package application

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/cat3399/pi-go/internal/session"
)

const (
	defaultReadIndexBytes int64 = 128 << 20
	maxReadIdentities           = 256
)

type sessionResourceReader interface {
	ReadSnapshot(session.SnapshotOptions) (session.ReadSnapshot, error)
	ReadHistory(session.HistoryQuery) (session.HistoryPage, error)
	ReadRuns(session.RunsQuery) (session.RunsPage, error)
	ReadNodes(session.NodeQuery) (session.NodePage, error)
	ReadEntry(string, string) (session.EntrySnapshot, error)
}

func (s *Service) ReadEntry(id, entryID, generation string) (session.EntrySnapshot, error) {
	var result session.EntrySnapshot
	err := s.readSessionResource(id, func(reader sessionResourceReader) (string, error) {
		var err error
		result, err = reader.ReadEntry(entryID, generation)
		return result.SessionID, err
	})
	return result, err
}

type cachedReadIndex struct {
	path     string
	identity session.ReadIndexIdentity
	index    *session.ReadIndex
	used     uint64
}

type readIndexLoad struct {
	done  chan struct{}
	index *session.ReadIndex
	err   error
}

type readIndexCache struct {
	mu      sync.Mutex
	entries map[string]*cachedReadIndex
	loads   map[string]*readIndexLoad
	bytes   int64
	budget  int64
	clock   uint64
}

type HistorySnapshot struct {
	Revision  uint64
	SessionID string
	Page      session.HistoryPage
}

type NodesSnapshot struct {
	Revision  uint64
	SessionID string
	Page      session.NodePage
}

type RunsSnapshot struct {
	Revision  uint64
	SessionID string
	Page      session.RunsPage
}

func (s *Service) ReadRuns(id string, query session.RunsQuery) (RunsSnapshot, error) {
	result := RunsSnapshot{Revision: s.CurrentRevision(), SessionID: id}
	err := s.readSessionResource(id, func(reader sessionResourceReader) (string, error) {
		page, err := reader.ReadRuns(query)
		result.Page = page
		return page.Source.Info.ID, err
	})
	return result, err
}

func (s *Service) ReadHistory(id string, query session.HistoryQuery) (HistorySnapshot, error) {
	result := HistorySnapshot{Revision: s.CurrentRevision(), SessionID: id}
	err := s.readSessionResource(id, func(reader sessionResourceReader) (string, error) {
		page, err := reader.ReadHistory(query)
		result.Page = page
		return page.Source.Info.ID, err
	})
	return result, err
}

func (s *Service) ReadNodes(id string, query session.NodeQuery) (NodesSnapshot, error) {
	result := NodesSnapshot{Revision: s.CurrentRevision(), SessionID: id}
	err := s.readSessionResource(id, func(reader sessionResourceReader) (string, error) {
		page, err := reader.ReadNodes(query)
		result.Page = page
		return page.Source.Info.ID, err
	})
	return result, err
}

func (s *Service) ReadSessionMetadata(id string) (SessionSnapshot, error) {
	result := SessionSnapshot{Revision: s.CurrentRevision(), SessionID: id}
	err := s.readSessionResource(id, func(reader sessionResourceReader) (string, error) {
		value, err := reader.ReadSnapshot(session.SnapshotOptions{})
		if err != nil {
			return "", err
		}
		var pathIDs map[string]string
		if value.Info.HasParentSession {
			discovered, err := s.discoveredSessions()
			if err != nil {
				return "", err
			}
			pathIDs = discoveredSessionPathIDs(discovered)
		}
		result.Version, result.FilePath, result.LeafID = value.Version, value.Info.Path, value.LeafID
		result.LatestUsage = value.LatestUsage
		result.Info = discoveredSessionInfo(value.Info, pathIDs)
		return value.Info.ID, nil
	})
	return result, err
}

// A Runtime may be admitted/replaced while an immutable index is loading.
// Retry against the actual owner rather than mixing its identity with a file
// snapshot. No query opens or retains a Runtime just to obtain history.
func (s *Service) readSessionResource(id string, read func(sessionResourceReader) (string, error)) error {
	for attempt := 0; attempt < 3; attempt++ {
		if managed, active := s.active(id); active {
			current, binding, err := managed.session.currentSession()
			if err != nil {
				return err
			}
			readID, err := read(current.SessionManager())
			if !managed.session.sameBinding(current, binding) {
				continue
			}
			if err != nil {
				return err
			}
			if readID != id {
				return ErrSessionChanged
			}
			return nil
		}
		index, err := s.readIndex(id)
		if err != nil {
			return err
		}
		if _, active := s.active(id); active {
			continue
		}
		readID, err := read(index)
		if _, active := s.active(id); active {
			continue
		}
		if err != nil {
			return err
		}
		if readID != id {
			return ErrSessionChanged
		}
		return nil
	}
	return ErrSessionChanged
}

func (s *Service) readIndex(id string) (*session.ReadIndex, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	cache := &s.readCache
	cache.mu.Lock()
	if cache.entries == nil {
		cache.entries = make(map[string]*cachedReadIndex)
		cache.loads = make(map[string]*readIndexLoad)
	}
	if load := cache.loads[id]; load != nil {
		cache.mu.Unlock()
		select {
		case <-load.done:
			return load.index, load.err
		case <-s.ctx.Done():
			return nil, ErrClosed
		}
	}
	cached := cache.entries[id]
	cache.clock++
	if cached != nil {
		cached.used = cache.clock
		copy := *cached
		cached = &copy
	}
	// Singleflight includes validation so concurrent identical reads cannot
	// race an invalidation or repeatedly parse the same large JSONL.
	load := &readIndexLoad{done: make(chan struct{})}
	cache.loads[id] = load
	cache.mu.Unlock()
	index, err := s.loadReadIndex(id, cached)
	s.mu.Lock()
	closed = s.closed
	s.mu.Unlock()
	if closed {
		index, err = nil, ErrClosed
	}
	cache.mu.Lock()
	delete(cache.loads, id)
	if err == nil {
		if old := cache.entries[id]; old != nil && old.index != nil {
			cache.bytes -= old.index.EstimatedBytes()
		}
		cache.clock++
		entry := &cachedReadIndex{path: indexPath(index), identity: index.Identity(), used: cache.clock}
		if index.EstimatedBytes() <= cache.budget && cache.budget > 0 {
			entry.index = index
			cache.bytes += index.EstimatedBytes()
		}
		cache.entries[id] = entry
		cache.evictLocked()
	}
	load.index, load.err = index, err
	close(load.done)
	cache.mu.Unlock()
	return index, err
}

func indexPath(index *session.ReadIndex) string {
	return index.Path()
}

func (s *Service) loadReadIndex(id string, cached *cachedReadIndex) (*session.ReadIndex, error) {
	var previous session.ReadIndexIdentity
	path := ""
	if cached != nil {
		previous, path = cached.identity, cached.path
		if cached.index != nil {
			valid, err := cached.index.ValidateSource()
			if err != nil {
				return nil, err
			}
			if valid {
				return cached.index, nil
			}
		}
	}
	if path == "" {
		var err error
		path, err = s.resolveReadPath(id)
		if err != nil {
			return nil, err
		}
	}
	resolvedAgain := false
	for attempt := 0; attempt < 3; attempt++ {
		index, err := session.OpenReadIndexWithIdentity(path, previous)
		if errors.Is(err, os.ErrNotExist) && cached != nil && !resolvedAgain {
			path, err = s.resolveReadPath(id)
			if err != nil {
				return nil, err
			}
			resolvedAgain = true
			continue
		}
		if errors.Is(err, session.ErrUnsupportedVersion) {
			// Preserve the existing first-open migration and writer ownership.
			// The resulting index itself never retains that writer claim.
			manager, migrateErr := session.OpenSessionManager(path, filepath.Dir(path), "")
			if migrateErr != nil {
				return nil, migrateErr
			}
			if closeErr := manager.Close(); closeErr != nil {
				return nil, closeErr
			}
			continue
		}
		if errors.Is(err, session.ErrReadSourceChanged) {
			continue
		}
		if err != nil {
			return nil, err
		}
		metadata, err := index.ReadSnapshot(session.SnapshotOptions{})
		if err != nil {
			return nil, err
		}
		if metadata.Info.ID != id {
			if cached != nil && !resolvedAgain {
				path, err = s.resolveReadPath(id)
				if err != nil {
					return nil, err
				}
				resolvedAgain = true
				continue
			}
			return nil, fmt.Errorf("%w: source now belongs to %s", ErrSessionChanged, metadata.Info.ID)
		}
		return index, nil
	}
	return nil, session.ErrReadSourceChanged
}

func (s *Service) resolveReadPath(id string) (string, error) {
	info, found, err := s.findSession(id)
	if err != nil {
		return "", err
	}
	if !found {
		return "", os.ErrNotExist
	}
	return info.Path, nil
}

func (c *readIndexCache) evictLocked() {
	for c.bytes > c.budget && c.bytes > 0 {
		var oldest *cachedReadIndex
		for _, entry := range c.entries {
			if entry.index != nil && (oldest == nil || entry.used < oldest.used) {
				oldest = entry
			}
		}
		if oldest == nil {
			break
		}
		c.bytes -= oldest.index.EstimatedBytes()
		oldest.index = nil
	}
	for len(c.entries) > maxReadIdentities {
		oldestID, used := "", ^uint64(0)
		for id, entry := range c.entries {
			if entry.used < used {
				oldestID, used = id, entry.used
			}
		}
		if entry := c.entries[oldestID]; entry.index != nil {
			c.bytes -= entry.index.EstimatedBytes()
		}
		delete(c.entries, oldestID)
	}
}

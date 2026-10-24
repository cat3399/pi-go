package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/cat3399/pi-go/internal/session"
)

const directoryIndexBudget = 16 << 20
const directoryIndexLifetime = 5 * time.Minute

// SessionDirectoryCursor names an immutable, sorted query result, not an offset
// in the current catalog. Activity and renames cannot move rows between pages.
type SessionDirectoryCursor struct {
	Index  string
	Offset int
}

type SessionDirectoryQuery struct {
	Project string
	Search  string
	Cursor  *SessionDirectoryCursor
	Limit   int
}

type SessionDirectoryPage struct {
	Version  string
	Sessions []SessionInfo
	Total    int
	Start    int
	Next     *SessionDirectoryCursor
}

// Overview carries an event anchor sampled before reading. Its fields have
// their original owners; the cursor is not a global atomic resource version.
type Overview struct {
	Cursor            EventCursor
	AgentDir          string
	DefaultCWD        string
	Projects          []ProjectInfo
	SessionCount      int
	RunningSessionIDs []string
}

func (s *Service) ReadOverview() (Overview, error) {
	cursor := s.CurrentCursor()
	sessions, err := s.ListSessions()
	if err != nil {
		return Overview{}, err
	}
	projects, err := s.projectsForSessions(sessions)
	if err != nil {
		return Overview{}, err
	}
	return Overview{Cursor: cursor, AgentDir: s.AgentDir(), DefaultCWD: s.DefaultCWD(), Projects: projects,
		SessionCount: len(sessions), RunningSessionIDs: s.RunningIDs()}, nil
}

type directoryIndex struct {
	project, search, version string
	sessions                 []SessionInfo
	bytes                    int64
	expires                  time.Time
	used                     uint64
}

type directoryCache struct {
	mu      sync.Mutex
	entries map[string]*directoryIndex
	bytes   int64
	clock   uint64
}

func (s *Service) ReadSessionDirectory(query SessionDirectoryQuery) (SessionDirectoryPage, error) {
	if query.Limit < 0 || query.Limit > 200 || len(query.Search) > 4096 {
		return SessionDirectoryPage{}, session.ErrInvalidReadQuery
	}
	if query.Limit == 0 {
		query.Limit = 50
	}
	project, search := "", strings.ToLower(strings.TrimSpace(query.Search))
	if query.Project != "" {
		project = projectPathKey(cleanPathKey(query.Project))
	}
	var index *directoryIndex
	if query.Cursor != nil {
		if len(query.Cursor.Index) != 64 || query.Cursor.Offset < 0 {
			return SessionDirectoryPage{}, session.ErrInvalidReadQuery
		}
		s.directoryCache.mu.Lock()
		index = s.directoryCache.entries[query.Cursor.Index]
		if index != nil && time.Now().Before(index.expires) {
			s.directoryCache.clock++
			index.used = s.directoryCache.clock
		} else {
			index = nil
		}
		s.directoryCache.mu.Unlock()
		if index != nil && (index.project != project || index.search != search) {
			return SessionDirectoryPage{}, session.ErrInvalidReadQuery
		}
	}
	if index == nil {
		values, err := s.ListSessions()
		if err != nil {
			return SessionDirectoryPage{}, err
		}
		filtered := make([]SessionInfo, 0, len(values))
		for _, value := range values {
			if project != "" && projectPathKey(cleanPathKey(value.CWD)) != project {
				continue
			}
			if search != "" && !strings.Contains(strings.ToLower(value.Name+"\n"+value.FirstMessage+"\n"+value.ID), search) {
				continue
			}
			filtered = append(filtered, value)
		}
		index = &directoryIndex{project: project, search: search, sessions: filtered, expires: time.Now().Add(directoryIndexLifetime)}
		hash := sha256.New()
		// Struct encoding is deterministic. Include query identity even when two
		// filters happen to select the same rows.
		if err := json.NewEncoder(hash).Encode(struct {
			Project, Search string
			Sessions        []SessionInfo
		}{project, search, filtered}); err != nil {
			return SessionDirectoryPage{}, err
		}
		index.version = hex.EncodeToString(hash.Sum(nil))
		if query.Cursor != nil && index.version != query.Cursor.Index {
			return SessionDirectoryPage{}, session.ErrReadCursorUnavailable
		}
		index.bytes = 256 + int64(len(project)+len(search)+cap(filtered)*160)
		for _, value := range filtered {
			index.bytes += int64(len(value.Path) + len(value.ID) + len(value.CWD) + len(value.Name) + len(value.FirstMessage) + len(value.ParentSessionID))
		}
		s.directoryCache.retain(index)
	}
	start := 0
	if query.Cursor != nil {
		start = query.Cursor.Offset
	}
	if start > len(index.sessions) {
		return SessionDirectoryPage{}, session.ErrInvalidReadQuery
	}
	end := min(start+query.Limit, len(index.sessions))
	result := SessionDirectoryPage{Version: index.version, Sessions: append([]SessionInfo{}, index.sessions[start:end]...), Total: len(index.sessions), Start: start}
	if end < len(index.sessions) {
		result.Next = &SessionDirectoryCursor{Index: index.version, Offset: end}
	}
	return result, nil
}

func (c *directoryCache) retain(index *directoryIndex) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]*directoryIndex)
	}
	if previous := c.entries[index.version]; previous != nil {
		c.bytes -= previous.bytes
		delete(c.entries, index.version)
	}
	now := time.Now()
	for id, value := range c.entries {
		if !now.Before(value.expires) {
			c.bytes -= value.bytes
			delete(c.entries, id)
		}
	}
	// An index exceeding the budget can still be traversed: reconstruct it on
	// the next read and require the exact content identity before using offset.
	if index.bytes > directoryIndexBudget {
		return
	}
	c.clock++
	index.used = c.clock
	c.entries[index.version] = index
	c.bytes += index.bytes
	for c.bytes > directoryIndexBudget {
		var oldest string
		for id, value := range c.entries {
			if oldest == "" || value.used < c.entries[oldest].used {
				oldest = id
			}
		}
		c.bytes -= c.entries[oldest].bytes
		delete(c.entries, oldest)
	}
}

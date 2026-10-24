package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/cat3399/pi-go/internal/session"
)

const defaultProjectPageSize = 8
const defaultProjectSessionPageSize = 8

// ProjectDirectory is a grouped, transport-neutral catalog read. Every project
// arrives with a first session page from the same catalog snapshot; opening a
// project does not require a second catalog enumeration.
type ProjectDirectoryCursor struct {
	Index  string
	Offset int
}

type ProjectDirectoryQuery struct {
	Cursor       *ProjectDirectoryCursor
	Limit        int
	SessionLimit int
}

type ProjectDirectoryItem struct {
	Project  ProjectInfo
	Sessions SessionDirectoryPage
}

type ProjectDirectoryPage struct {
	Version  string
	Projects []ProjectDirectoryItem
	Start    int
	Total    int
	Next     *ProjectDirectoryCursor
}

func (s *Service) ReadProjectDirectory(query ProjectDirectoryQuery) (ProjectDirectoryPage, error) {
	if query.Limit < 0 || query.Limit > 100 || query.SessionLimit < 0 || query.SessionLimit > 200 {
		return ProjectDirectoryPage{}, session.ErrInvalidReadQuery
	}
	if query.Limit == 0 {
		query.Limit = defaultProjectPageSize
	}
	if query.SessionLimit == 0 {
		query.SessionLimit = defaultProjectSessionPageSize
	}
	var index *directoryIndex
	start := 0
	if cursor := query.Cursor; cursor != nil {
		if len(cursor.Index) != 64 || cursor.Offset < 0 {
			return ProjectDirectoryPage{}, session.ErrInvalidReadQuery
		}
		start = cursor.Offset
		s.directoryCache.mu.Lock()
		index = s.directoryCache.entries[cursor.Index]
		if index != nil && time.Now().Before(index.expires) {
			s.directoryCache.clock++
			index.used = s.directoryCache.clock
		} else {
			index = nil
		}
		s.directoryCache.mu.Unlock()
		if index != nil && !index.projectIndex {
			return ProjectDirectoryPage{}, session.ErrInvalidReadQuery
		}
	}
	if index == nil {
		sessions, err := s.ListSessions()
		if err != nil {
			return ProjectDirectoryPage{}, err
		}
		projects, err := s.projectsForSessions(sessions)
		if err != nil {
			return ProjectDirectoryPage{}, err
		}
		index, err = s.projectDirectoryIndex(projects, sessions)
		if err != nil {
			return ProjectDirectoryPage{}, err
		}
		if query.Cursor != nil && index.version != query.Cursor.Index {
			return ProjectDirectoryPage{}, session.ErrReadCursorUnavailable
		}
	}
	return s.projectDirectoryPage(index, start, query.Limit, query.SessionLimit)
}

func (s *Service) projectDirectoryIndex(projects []ProjectInfo, sessions []SessionInfo) (*directoryIndex, error) {
	hash := sha256.New()
	if err := json.NewEncoder(hash).Encode(struct {
		Kind     string
		Projects []ProjectInfo
		Sessions []SessionInfo
	}{"projects", projects, sessions}); err != nil {
		return nil, err
	}
	index := &directoryIndex{projectIndex: true, projects: projects, sessions: sessions,
		version: hex.EncodeToString(hash.Sum(nil)), expires: time.Now().Add(directoryIndexLifetime)}
	index.bytes = directorySessionsBytes(sessions) + int64(len(projects)*64)
	for _, project := range projects {
		index.bytes += int64(len(project.Path))
	}
	s.directoryCache.retain(index)
	return index, nil
}

func (s *Service) projectDirectoryPage(index *directoryIndex, start, limit, sessionLimit int) (ProjectDirectoryPage, error) {
	if start > len(index.projects) {
		return ProjectDirectoryPage{}, session.ErrInvalidReadQuery
	}
	end := min(start+limit, len(index.projects))
	result := ProjectDirectoryPage{Version: index.version, Start: start, Total: len(index.projects), Projects: []ProjectDirectoryItem{}}
	groups := make(map[string][]SessionInfo, end-start)
	for _, project := range index.projects[start:end] {
		groups[projectPathKey(cleanPathKey(project.Path))] = make([]SessionInfo, 0, project.SessionCount)
	}
	for _, value := range index.sessions {
		key := projectPathKey(cleanPathKey(value.CWD))
		if values, ok := groups[key]; ok {
			groups[key] = append(values, value)
		}
	}
	for _, project := range index.projects[start:end] {
		key := projectPathKey(cleanPathKey(project.Path))
		values := groups[key]
		// Use exactly the ordinary directory identity, so its next cursor can
		// continue through ReadSessionDirectory without a special adapter path.
		hash := sha256.New()
		if err := json.NewEncoder(hash).Encode(struct {
			Project, Search string
			Sessions        []SessionInfo
		}{key, "", values}); err != nil {
			return ProjectDirectoryPage{}, err
		}
		child := &directoryIndex{project: key, sessions: values, version: hex.EncodeToString(hash.Sum(nil)),
			bytes: directorySessionsBytes(values) + int64(len(key)), expires: time.Now().Add(directoryIndexLifetime)}
		s.directoryCache.retain(child)
		count := min(sessionLimit, len(values))
		page := SessionDirectoryPage{Version: child.version, Sessions: append([]SessionInfo{}, values[:count]...), Total: len(values)}
		if count < len(values) {
			page.Next = &SessionDirectoryCursor{Index: child.version, Offset: count}
		}
		result.Projects = append(result.Projects, ProjectDirectoryItem{Project: project, Sessions: page})
	}
	if end < len(index.projects) {
		result.Next = &ProjectDirectoryCursor{Index: index.version, Offset: end}
	}
	return result, nil
}

func directorySessionsBytes(values []SessionInfo) int64 {
	size := int64(256 + len(values)*160)
	for _, value := range values {
		size += int64(len(value.Path) + len(value.ID) + len(value.CWD) + len(value.Name) + len(value.FirstMessage) + len(value.ParentSessionID))
	}
	return size
}

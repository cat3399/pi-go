package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	projectCatalogVersion  = 1
	maxProjectCatalogBytes = 1 << 20
)

// ProjectInfo is the surface-neutral project directory projection. Removing a
// project only hides this grouping; source files and durable sessions remain
// untouched.
type ProjectInfo struct {
	Path         string
	Modified     time.Time
	SessionCount int
}

type storedProject struct {
	Path    string    `json:"path"`
	AddedAt time.Time `json:"addedAt"`
}

type projectCatalogFile struct {
	Version  int             `json:"version"`
	Projects []storedProject `json:"projects,omitempty"`
	Hidden   []string        `json:"hidden,omitempty"`
}

func (s *Service) ListProjects() ([]ProjectInfo, error) {
	if s == nil {
		return nil, errors.New("application service is unavailable")
	}

	s.projectMu.Lock()
	catalog, err := s.readProjectCatalogLocked()
	s.projectMu.Unlock()
	if err != nil {
		return nil, err
	}
	sessions, err := s.ListSessions()
	if err != nil {
		return nil, err
	}

	hidden := make(map[string]struct{}, len(catalog.Hidden))
	for _, path := range catalog.Hidden {
		hidden[projectPathKey(path)] = struct{}{}
	}
	projects := make(map[string]ProjectInfo, len(catalog.Projects)+len(sessions))
	for _, stored := range catalog.Projects {
		key := projectPathKey(stored.Path)
		if key == "" {
			continue
		}
		if _, removed := hidden[key]; removed {
			continue
		}
		projects[key] = ProjectInfo{Path: stored.Path, Modified: stored.AddedAt}
	}
	for _, session := range sessions {
		path, pathErr := normalizeProjectPath(session.CWD)
		if pathErr != nil {
			continue
		}
		key := projectPathKey(path)
		if _, removed := hidden[key]; removed {
			continue
		}
		project := projects[key]
		if project.Path == "" {
			project.Path = path
		}
		project.SessionCount++
		if session.Modified.After(project.Modified) {
			project.Modified = session.Modified
		}
		projects[key] = project
	}

	result := make([]ProjectInfo, 0, len(projects))
	for _, project := range projects {
		result = append(result, project)
	}
	sort.SliceStable(result, func(left, right int) bool {
		if !result[left].Modified.Equal(result[right].Modified) {
			return result[left].Modified.After(result[right].Modified)
		}
		return strings.ToLower(result[left].Path) < strings.ToLower(result[right].Path)
	})
	return result, nil
}

func (s *Service) AddProject(ctx context.Context, requested string) (ProjectInfo, error) {
	if s == nil {
		return ProjectInfo{}, errors.New("application service is unavailable")
	}
	if err := normalizeContext(ctx).Err(); err != nil {
		return ProjectInfo{}, err
	}
	path, err := ValidateCWD(requested)
	if err != nil {
		return ProjectInfo{}, err
	}
	now := time.Now().UTC()

	s.projectMu.Lock()
	catalog, err := s.readProjectCatalogLocked()
	if err == nil {
		key := projectPathKey(path)
		updated := false
		for index := range catalog.Projects {
			if projectPathKey(catalog.Projects[index].Path) == key {
				catalog.Projects[index] = storedProject{Path: path, AddedAt: now}
				updated = true
				break
			}
		}
		if !updated {
			catalog.Projects = append(catalog.Projects, storedProject{Path: path, AddedAt: now})
		}
		catalog.Hidden = removeProjectPath(catalog.Hidden, key)
		err = s.writeProjectCatalogLocked(catalog)
	}
	s.projectMu.Unlock()
	if err != nil {
		return ProjectInfo{}, err
	}

	s.events.publish(Event{Value: ProjectCatalogEvent{Change: ProjectAdded, Path: path}})
	return ProjectInfo{Path: path, Modified: now}, nil
}

func (s *Service) RemoveProject(ctx context.Context, requested string) error {
	if s == nil {
		return errors.New("application service is unavailable")
	}
	if err := normalizeContext(ctx).Err(); err != nil {
		return err
	}
	path, err := normalizeProjectPath(requested)
	if err != nil {
		return err
	}
	key := projectPathKey(path)

	s.projectMu.Lock()
	catalog, err := s.readProjectCatalogLocked()
	if err == nil {
		projects := catalog.Projects[:0]
		for _, project := range catalog.Projects {
			if projectPathKey(project.Path) != key {
				projects = append(projects, project)
			}
		}
		catalog.Projects = projects
		if !containsProjectPath(catalog.Hidden, key) {
			catalog.Hidden = append(catalog.Hidden, path)
		}
		err = s.writeProjectCatalogLocked(catalog)
	}
	s.projectMu.Unlock()
	if err != nil {
		return err
	}

	s.events.publish(Event{Value: ProjectCatalogEvent{Change: ProjectRemoved, Path: path}})
	return nil
}

// activateProject restores a previously hidden project when the user
// explicitly creates a new durable session in it. The following session
// catalog event is enough to refresh every surface.
func (s *Service) activateProject(path string) error {
	key := projectPathKey(path)
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	catalog, err := s.readProjectCatalogLocked()
	if err != nil {
		return err
	}
	next := removeProjectPath(catalog.Hidden, key)
	if len(next) == len(catalog.Hidden) {
		return nil
	}
	catalog.Hidden = next
	return s.writeProjectCatalogLocked(catalog)
}

func (s *Service) readProjectCatalogLocked() (projectCatalogFile, error) {
	path := filepath.Join(s.paths.AgentDir, "projects.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return projectCatalogFile{Version: projectCatalogVersion}, nil
	}
	if err != nil {
		return projectCatalogFile{}, err
	}
	if len(data) > maxProjectCatalogBytes {
		return projectCatalogFile{}, fmt.Errorf("projects.json exceeds %d bytes", maxProjectCatalogBytes)
	}
	var catalog projectCatalogFile
	if err := json.Unmarshal(data, &catalog); err != nil {
		return projectCatalogFile{}, fmt.Errorf("decode projects.json: %w", err)
	}
	if catalog.Version != 0 && catalog.Version != projectCatalogVersion {
		return projectCatalogFile{}, fmt.Errorf("unsupported projects.json version %d", catalog.Version)
	}
	catalog.Version = projectCatalogVersion
	catalog.Projects = uniqueStoredProjects(catalog.Projects)
	catalog.Hidden = uniqueProjectPaths(catalog.Hidden)
	return catalog, nil
}

func (s *Service) writeProjectCatalogLocked(catalog projectCatalogFile) error {
	catalog.Version = projectCatalogVersion
	catalog.Projects = uniqueStoredProjects(catalog.Projects)
	catalog.Hidden = uniqueProjectPaths(catalog.Hidden)
	sort.SliceStable(catalog.Projects, func(left, right int) bool {
		return catalog.Projects[left].AddedAt.After(catalog.Projects[right].AddedAt)
	})
	sort.Slice(catalog.Hidden, func(left, right int) bool {
		return strings.ToLower(catalog.Hidden[left]) < strings.ToLower(catalog.Hidden[right])
	})
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(s.paths.AgentDir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.paths.AgentDir, "projects.json"), data, 0o600)
}

func normalizeProjectPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("project path is required")
	}
	path, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve project path: %w", err)
	}
	return filepath.Clean(path), nil
}

func projectPathKey(value string) string {
	path, err := normalizeProjectPath(value)
	if err != nil {
		return ""
	}
	return path
}

func containsProjectPath(paths []string, key string) bool {
	for _, path := range paths {
		if projectPathKey(path) == key {
			return true
		}
	}
	return false
}

func removeProjectPath(paths []string, key string) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if projectPathKey(path) != key {
			result = append(result, path)
		}
	}
	return result
}

func uniqueProjectPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, value := range paths {
		path, err := normalizeProjectPath(value)
		if err != nil {
			continue
		}
		key := projectPathKey(path)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, path)
	}
	return result
}

func uniqueStoredProjects(projects []storedProject) []storedProject {
	seen := make(map[string]int, len(projects))
	result := make([]storedProject, 0, len(projects))
	for _, project := range projects {
		path, err := normalizeProjectPath(project.Path)
		if err != nil {
			continue
		}
		key := projectPathKey(path)
		if index, exists := seen[key]; exists {
			if project.AddedAt.After(result[index].AddedAt) {
				result[index] = storedProject{Path: path, AddedAt: project.AddedAt}
			}
			continue
		}
		seen[key] = len(result)
		result = append(result, storedProject{Path: path, AddedAt: project.AddedAt})
	}
	return result
}

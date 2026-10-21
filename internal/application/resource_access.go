package application

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

var ErrResourceAccessDenied = errors.New("access denied")

func (s *Service) resourceRoots() ([]string, error) {
	roots := []string{s.paths.WorkingDir}
	s.projectMu.Lock()
	projects, err := s.readProjectCatalogLocked()
	s.projectMu.Unlock()
	if err != nil {
		return nil, err
	}
	for _, project := range projects.Projects {
		if project.Path != "" && !containsProjectPath(projects.Hidden, projectPathKey(project.Path)) {
			roots = append(roots, project.Path)
		}
	}
	cwds, err := s.sessionResourceDirectories()
	if err != nil {
		return nil, err
	}
	for _, cwd := range cwds {
		if cwd != "" {
			roots = append(roots, cwd)
		}
	}
	s.allowedRootMu.RLock()
	for root := range s.allowedRoots {
		roots = append(roots, root)
	}
	s.allowedRootMu.RUnlock()
	return uniqueCleanPaths(roots), nil
}

func (s *Service) sessionResourceDirectories() ([]string, error) {
	locations, err := s.sessionCatalog.ListDirectories()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]string, len(locations))
	for _, location := range locations {
		if _, duplicate := byID[location.ID]; duplicate {
			// Existing discovery resolves duplicate IDs in activity order. Only
			// that exceptional case requires full metadata to preserve its choice.
			sessions, err := s.ListSessions()
			if err != nil {
				return nil, err
			}
			result := make([]string, 0, len(sessions))
			for _, info := range sessions {
				result = append(result, info.CWD)
			}
			return result, nil
		}
		byID[location.ID] = location.Cwd
	}
	for _, managed := range s.activeSessions() {
		if manager := managed.manager(); manager != nil {
			location := manager.Location()
			byID[location.ID] = location.Cwd
		}
	}
	result := make([]string, 0, len(byID))
	for _, cwd := range byID {
		result = append(result, cwd)
	}
	return result, nil
}

func (s *Service) allowResourceRoot(root string) {
	resolved, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil || resolved == "" {
		return
	}
	s.allowedRootMu.Lock()
	s.allowedRoots[filepath.Clean(resolved)] = struct{}{}
	s.allowedRootMu.Unlock()
}

func (s *Service) authorizeResourcePath(target string, mustExist bool) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" || !filepath.IsAbs(target) {
		return "", ErrResourceAccessDenied
	}
	resolved := filepath.Clean(target)
	roots, err := s.resourceRoots()
	if err != nil {
		return "", err
	}
	allowed := false
	for _, root := range roots {
		if resourcePathWithin(root, resolved) {
			allowed = true
			break
		}
		if realRoot, rootErr := filepath.EvalSymlinks(root); rootErr == nil && resourcePathWithin(realRoot, resolved) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", ErrResourceAccessDenied
	}
	if !mustExist {
		existing := resolved
		for {
			realExisting, evalErr := filepath.EvalSymlinks(existing)
			if evalErr == nil {
				relativeTail, relativeErr := filepath.Rel(existing, resolved)
				if relativeErr != nil || filepath.IsAbs(relativeTail) || relativeTail == ".." || strings.HasPrefix(relativeTail, ".."+string(filepath.Separator)) {
					return "", ErrResourceAccessDenied
				}
				realResolved := filepath.Clean(filepath.Join(realExisting, relativeTail))
				for _, root := range roots {
					realRoot, rootErr := filepath.EvalSymlinks(root)
					if rootErr == nil && resourcePathWithin(realRoot, realResolved) {
						return realResolved, nil
					}
				}
				return "", ErrResourceAccessDenied
			}
			if !errors.Is(evalErr, os.ErrNotExist) {
				return "", evalErr
			}
			parent := filepath.Dir(existing)
			if parent == existing {
				return "", ErrResourceAccessDenied
			}
			existing = parent
		}
	}
	realTarget, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", err
	}
	for _, root := range roots {
		realRoot, rootErr := filepath.EvalSymlinks(root)
		if rootErr == nil && resourcePathWithin(realRoot, realTarget) {
			return realTarget, nil
		}
	}
	return "", ErrResourceAccessDenied
}

func resourcePathWithin(root, target string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return false
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func uniqueCleanPaths(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		resolved, err := filepath.Abs(value)
		if err != nil {
			continue
		}
		resolved = filepath.Clean(resolved)
		if _, exists := seen[resolved]; exists {
			continue
		}
		seen[resolved] = struct{}{}
		result = append(result, resolved)
	}
	return result
}

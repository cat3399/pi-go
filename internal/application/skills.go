package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/product"
	"github.com/cat3399/pi-go/internal/resource"
)

type SkillInstallScope string

const (
	SkillScopeGlobal  SkillInstallScope = "global"
	SkillScopeProject SkillInstallScope = "project"
)

type SkillSourceInfo struct {
	Source string
	Scope  string
}

type SkillInstallInfo struct {
	Package            string
	Scope              SkillInstallScope
	Source             string
	SourceType         string
	SkillsShURL        string
	SkillPath          string
	Ref                string
	VersionHash        string
	CanCheckForUpdates bool
}

type SkillInfo struct {
	Name                   string
	Description            string
	FilePath               string
	BaseDir                string
	DisableModelInvocation bool
	SourceInfo             SkillSourceInfo
	Install                *SkillInstallInfo
}

type ResourceCollision struct {
	ResourceType string
	Name         string
	WinnerPath   string
	LoserPath    string
	WinnerSource string
	LoserSource  string
}

type ResourceDiagnostic struct {
	Type      string
	Message   string
	Path      string
	Collision *ResourceCollision
}

type SkillsSnapshot struct {
	Skills                 []SkillInfo
	Diagnostics            []ResourceDiagnostic
	ProjectResourcesLoaded bool
}

type skillLockEntry struct {
	Source          string `json:"source,omitempty"`
	SourceType      string `json:"sourceType,omitempty"`
	SkillPath       string `json:"skillPath,omitempty"`
	Ref             string `json:"ref,omitempty"`
	SkillFolderHash string `json:"skillFolderHash,omitempty"`
	ComputedHash    string `json:"computedHash,omitempty"`
}

type skillLockFile struct {
	Version int                       `json:"version,omitempty"`
	Skills  map[string]skillLockEntry `json:"skills"`
}

func (s *Service) ListSkills(ctx context.Context, cwd string) (SkillsSnapshot, error) {
	cwd, err := ValidateCWD(cwd)
	if err != nil {
		return SkillsSnapshot{}, err
	}
	snapshot, err := s.loadResourceSnapshot(ctx, cwd)
	if err != nil {
		return SkillsSnapshot{}, err
	}
	globalLock := readSkillLock(s.globalSkillLockPath())
	projectLock := readSkillLock(filepath.Join(cwd, "skills-lock.json"))
	result := SkillsSnapshot{
		Skills:                 make([]SkillInfo, 0, len(snapshot.Skills)),
		Diagnostics:            make([]ResourceDiagnostic, 0, len(snapshot.Diagnostics)),
		ProjectResourcesLoaded: snapshot.Trusted,
	}
	for _, value := range snapshot.Skills {
		info := SkillInfo{
			Name: value.Name, Description: value.Description, FilePath: value.Path, BaseDir: value.BaseDir,
			DisableModelInvocation: value.DisableModelInvocation,
			SourceInfo:             SkillSourceInfo{Source: value.Source.Source, Scope: string(value.Source.Scope)},
		}
		info.Install = s.skillInstallInfo(cwd, value, globalLock, projectLock)
		result.Skills = append(result.Skills, info)
	}
	for _, diagnostic := range snapshot.Diagnostics {
		mapped := ResourceDiagnostic{Type: diagnostic.Kind, Message: diagnostic.Message, Path: diagnostic.Path}
		if diagnostic.Kind == "collision" {
			mapped.Collision = &ResourceCollision{
				ResourceType: diagnostic.Resource, Name: diagnostic.Name,
				WinnerPath: diagnostic.WinnerPath, LoserPath: diagnostic.LoserPath,
				WinnerSource: diagnostic.WinnerSource.Source, LoserSource: diagnostic.LoserSource.Source,
			}
		}
		result.Diagnostics = append(result.Diagnostics, mapped)
	}
	return result, nil
}

func (s *Service) skillInstallInfo(cwd string, skill resource.Skill, global, project map[string]skillLockEntry) *SkillInstallInfo {
	var scope SkillInstallScope
	var entries map[string]skillLockEntry
	file := filepath.Clean(skill.Path)
	switch {
	case pathWithin(file, filepath.Join(s.paths.AgentDir, "skills")), pathWithin(file, s.globalSkillsDirectory()):
		scope, entries = SkillScopeGlobal, global
	case pathWithin(file, filepath.Join(cwd, product.DirectoryName, "skills")):
		scope, entries = SkillScopeProject, project
	default:
		return nil
	}
	entry, ok := findSkillLockEntry(entries, skill.Name)
	if !ok || strings.TrimSpace(entry.Source) == "" {
		return nil
	}
	source := normalizeSkillSource(entry.Source, entry.SourceType)
	if source == "" {
		return nil
	}
	version := entry.ComputedHash
	if scope == SkillScopeGlobal {
		version = entry.SkillFolderHash
	}
	isGitHub := entry.SourceType == "github" && validGitHubSource(source)
	canCheck := isGitHub && entry.SkillPath != "" && version != "" && (scope == SkillScopeGlobal || entry.Ref == "")
	return &SkillInstallInfo{
		Package: source + "@" + skill.Name, Scope: scope, Source: source, SourceType: entry.SourceType,
		SkillsShURL: skillsShSkillURL(s.skillsAPI, source, skill.Name, entry.SourceType),
		SkillPath:   entry.SkillPath, Ref: entry.Ref, VersionHash: version, CanCheckForUpdates: canCheck,
	}
}

func (s *Service) SetSkillModelInvocation(ctx context.Context, cwd, filePath string, disabled bool) error {
	ctx = normalizeContext(ctx)
	cwd, err := ValidateCWD(cwd)
	if err != nil {
		return err
	}
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	snapshot, err := s.ListSkills(ctx, cwd)
	if err != nil {
		return err
	}
	requested, err := canonicalExistingPath(filePath)
	if err != nil {
		return os.ErrNotExist
	}
	var selected *SkillInfo
	for index := range snapshot.Skills {
		candidate, candidateErr := canonicalExistingPath(snapshot.Skills[index].FilePath)
		if candidateErr == nil && candidate == requested {
			selected = &snapshot.Skills[index]
			break
		}
	}
	if selected == nil {
		return os.ErrPermission
	}
	if selected.DisableModelInvocation == disabled {
		return nil
	}
	info, err := os.Stat(requested)
	if err != nil || !info.Mode().IsRegular() {
		return os.ErrNotExist
	}
	data, err := os.ReadFile(requested)
	if err != nil {
		return err
	}
	if int64(len(data)) > resource.DefaultMaxFileBytes || !utf8.Valid(data) {
		return errors.New("skill file is invalid or too large")
	}
	updated, err := rewriteSkillInvocation(data, disabled)
	if err != nil {
		return err
	}
	if bytes.Equal(updated, data) {
		return nil
	}
	if err := writeFileAtomic(requested, updated, info.Mode().Perm()); err != nil {
		return err
	}
	verified, verifyErr := s.ListSkills(ctx, cwd)
	if verifyErr == nil {
		for _, value := range verified.Skills {
			candidate, candidateErr := canonicalExistingPath(value.FilePath)
			if candidateErr == nil && candidate == requested && value.DisableModelInvocation == disabled {
				return nil
			}
		}
		verifyErr = errors.New("updated skill was not discoverable")
	}
	rollbackErr := writeFileAtomic(requested, data, info.Mode().Perm())
	return errors.Join(fmt.Errorf("verify updated skill: %w", verifyErr), rollbackErr)
}

func rewriteSkillInvocation(data []byte, disabled bool) ([]byte, error) {
	newline := []byte("\n")
	opening := []byte("---\n")
	if bytes.HasPrefix(data, []byte("---\r\n")) {
		newline, opening = []byte("\r\n"), []byte("---\r\n")
	}
	if !bytes.HasPrefix(data, opening) {
		return nil, errors.New("skill frontmatter is required")
	}
	frontEnd := bytes.Index(data[len(opening):], append(newline, []byte("---")...))
	if frontEnd < 0 {
		return nil, errors.New("skill frontmatter is malformed")
	}
	frontEnd += len(opening) + len(newline)
	front := data[len(opening):frontEnd]
	lines := bytes.SplitAfter(front, newline)
	key := []byte("disable-model-invocation")
	found := -1
	for index, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if colon := bytes.IndexByte(trimmed, ':'); colon >= 0 && bytes.Equal(bytes.TrimSpace(trimmed[:colon]), key) {
			found = index
			break
		}
	}
	if found >= 0 {
		if disabled {
			lines[found] = append(append([]byte{}, key...), append([]byte(": true"), newline...)...)
		} else {
			lines = append(lines[:found], lines[found+1:]...)
		}
	} else if disabled {
		line := append(append([]byte{}, key...), append([]byte(": true"), newline...)...)
		lines = append([][]byte{line}, lines...)
	} else {
		return append([]byte(nil), data...), nil
	}
	result := make([]byte, 0, len(data)+32)
	result = append(result, opening...)
	for _, line := range lines {
		result = append(result, line...)
	}
	result = append(result, data[frontEnd:]...)
	return result, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) (err error) {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".pi-go-write-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporary != nil {
			err = errors.Join(err, temporary.Close())
		}
		if cleanupErr := os.Remove(temporaryPath); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			err = errors.Join(err, cleanupErr)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	temporary = nil
	return os.Rename(temporaryPath, path)
}

func canonicalExistingPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", os.ErrNotExist
	}
	value, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	value, err = filepath.EvalSymlinks(value)
	if err != nil {
		return "", err
	}
	return filepath.Clean(value), nil
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func readSkillLock(path string) map[string]skillLockEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]skillLockEntry{}
	}
	var lock skillLockFile
	if json.Unmarshal(data, &lock) != nil || lock.Skills == nil {
		return map[string]skillLockEntry{}
	}
	return lock.Skills
}

func findSkillLockEntry(entries map[string]skillLockEntry, name string) (skillLockEntry, bool) {
	if value, ok := entries[name]; ok {
		return value, true
	}
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.EqualFold(key, name) {
			return entries[key], true
		}
	}
	return skillLockEntry{}, false
}

func normalizeSkillSource(source, sourceType string) string {
	source = strings.TrimSuffix(strings.TrimSpace(source), "/")
	if sourceType != "github" {
		return source
	}
	source = strings.TrimPrefix(source, "git+")
	source = strings.TrimPrefix(source, "https://github.com/")
	source = strings.TrimPrefix(source, "http://github.com/")
	source = strings.TrimPrefix(source, "git@github.com:")
	source = strings.TrimSuffix(source, ".git")
	return strings.TrimSuffix(source, "/")
}

func skillsShSkillURL(base, source, name, sourceType string) string {
	if sourceType == "local" || !validGitHubSource(source) {
		return ""
	}
	return strings.TrimRight(base, "/") + "/" + source + "/" + name
}

func (s *Service) globalSkillsDirectory() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(s.paths.AgentDir, "skills")
	}
	return filepath.Join(home, ".agents", "skills")
}

func (s *Service) globalSkillLockPath() string {
	if state := environmentValue(s.production.Environment, "XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "skills", ".skill-lock.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(s.paths.AgentDir, ".skill-lock.json")
	}
	return filepath.Join(home, ".agents", ".skill-lock.json")
}

func environmentValue(environment []string, key string) string {
	if environment == nil {
		return os.Getenv(key)
	}
	prefix := key + "="
	for index := len(environment) - 1; index >= 0; index-- {
		if strings.HasPrefix(environment[index], prefix) {
			return strings.TrimPrefix(environment[index], prefix)
		}
	}
	return ""
}

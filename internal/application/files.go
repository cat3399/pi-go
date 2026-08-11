package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	ErrNotFile         = errors.New("not a file")
	ErrUploadConflict  = errors.New("one or more files already exist")
	ErrInvalidFileName = errors.New("invalid file name")
)

var webSessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

var ignoredFileNames = map[string]struct{}{
	"node_modules": {}, ".git": {}, ".next": {}, "dist": {}, "build": {}, "__pycache__": {},
	".turbo": {}, ".cache": {}, "coverage": {}, ".pytest_cache": {}, ".mypy_cache": {},
	"target": {}, "vendor": {}, ".DS_Store": {},
}

var ignoredFileSuffixes = []string{".pyc"}

type FileResource struct {
	Path     string
	Name     string
	Size     int64
	Modified time.Time
	IsFile   bool
	IsDir    bool
}

type FileEntry struct {
	Name     string `json:"name"`
	IsDir    bool   `json:"isDir"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
}

type FileList struct {
	Path    string
	Entries []FileEntry
}

type UploadTargetInspection struct {
	Conflicts      []string `json:"conflicts"`
	NonReplaceable []string `json:"nonReplaceable"`
}

type UploadConflictStrategy string

const (
	UploadConflictError     UploadConflictStrategy = "error"
	UploadConflictOverwrite UploadConflictStrategy = "overwrite"
	UploadConflictSkip      UploadConflictStrategy = "skip"
)

type UploadFile struct {
	Name string
	Data []byte
}

type UploadFileError struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

type UploadResult struct {
	Uploaded   []string
	Skipped    []string
	Errors     []UploadFileError
	Inspection UploadTargetInspection
}

func (s *Service) ResolveFile(ctx context.Context, target, sessionID string) (FileResource, error) {
	_ = ctx
	resolved, err := s.authorizeResourcePath(target, true)
	if errors.Is(err, ErrResourceAccessDenied) && strings.TrimSpace(sessionID) != "" {
		allowed, referenceErr := s.sessionReferencesFile(sessionID, target)
		if referenceErr != nil {
			return FileResource{}, referenceErr
		}
		if allowed {
			resolved, err = filepath.EvalSymlinks(filepath.Clean(target))
		}
	}
	if err != nil {
		return FileResource{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return FileResource{}, err
	}
	return FileResource{
		Path: resolved, Name: info.Name(), Size: info.Size(), Modified: info.ModTime(),
		IsFile: info.Mode().IsRegular(), IsDir: info.IsDir(),
	}, nil
}

func (s *Service) ListFiles(ctx context.Context, target string) (FileList, error) {
	resource, err := s.ResolveFile(ctx, target, "")
	if err != nil {
		return FileList{}, err
	}
	if !resource.IsDir {
		return FileList{}, ErrPathNotDirectory
	}
	dirEntries, err := os.ReadDir(resource.Path)
	if err != nil {
		return FileList{}, err
	}
	entries := make([]FileEntry, 0, len(dirEntries))
	for _, entry := range dirEntries {
		if ignoredFileName(entry.Name()) {
			continue
		}
		isDirectory, include := resolveDirectoryEntry(filepath.Join(resource.Path, entry.Name()), entry)
		if !include {
			continue
		}
		entries = append(entries, FileEntry{Name: entry.Name(), IsDir: isDirectory, Size: 0, Modified: ""})
	}
	sort.SliceStable(entries, func(left, right int) bool {
		if entries[left].IsDir != entries[right].IsDir {
			return entries[left].IsDir
		}
		leftName, rightName := strings.ToLower(entries[left].Name), strings.ToLower(entries[right].Name)
		if leftName == rightName {
			return entries[left].Name < entries[right].Name
		}
		return leftName < rightName
	})
	return FileList{Path: resource.Path, Entries: entries}, nil
}

func ignoredFileName(name string) bool {
	if _, ignored := ignoredFileNames[name]; ignored {
		return true
	}
	for _, suffix := range ignoredFileSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func resolveDirectoryEntry(path string, entry os.DirEntry) (bool, bool) {
	if entry.IsDir() {
		return true, true
	}
	if entry.Type().IsRegular() {
		return false, true
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, false
	}
	return info.IsDir(), info.IsDir() || info.Mode().IsRegular()
}

func (s *Service) InspectUploadTargets(ctx context.Context, directory string, fileNames []string) (UploadTargetInspection, error) {
	resource, err := s.ResolveFile(ctx, directory, "")
	if err != nil {
		return UploadTargetInspection{}, err
	}
	if !resource.IsDir {
		return UploadTargetInspection{}, ErrPathNotDirectory
	}
	if err := validateUploadFileNames(fileNames); err != nil {
		return UploadTargetInspection{}, err
	}
	return inspectUploadTargets(resource.Path, fileNames)
}

func (s *Service) SaveUploads(ctx context.Context, directory string, files []UploadFile, strategy UploadConflictStrategy) (UploadResult, error) {
	if strategy != UploadConflictError && strategy != UploadConflictOverwrite && strategy != UploadConflictSkip {
		return UploadResult{}, errors.New("invalid conflict strategy")
	}
	fileNames := make([]string, len(files))
	for index, file := range files {
		fileNames[index] = file.Name
	}
	if err := validateUploadFileNames(fileNames); err != nil {
		return UploadResult{}, err
	}

	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	resource, err := s.ResolveFile(ctx, directory, "")
	if err != nil {
		return UploadResult{}, err
	}
	if !resource.IsDir {
		return UploadResult{}, ErrPathNotDirectory
	}
	inspection, err := inspectUploadTargets(resource.Path, fileNames)
	if err != nil {
		return UploadResult{}, err
	}
	result := UploadResult{
		Uploaded: []string{}, Skipped: []string{}, Errors: []UploadFileError{}, Inspection: inspection,
	}
	if strategy == UploadConflictError && len(inspection.Conflicts) != 0 {
		return result, ErrUploadConflict
	}
	conflicts := stringSet(inspection.Conflicts)
	nonReplaceable := stringSet(inspection.NonReplaceable)
	for _, file := range files {
		destination := filepath.Join(resource.Path, file.Name)
		if _, exists := conflicts[file.Name]; exists && strategy == UploadConflictSkip {
			result.Skipped = append(result.Skipped, file.Name)
			continue
		}
		if _, exists := conflicts[file.Name]; exists {
			if _, blocked := nonReplaceable[file.Name]; blocked {
				result.Errors = append(result.Errors, UploadFileError{Name: file.Name, Error: "Cannot replace a directory or symbolic link"})
				continue
			}
			if err := os.Remove(destination); err != nil {
				result.Errors = append(result.Errors, UploadFileError{Name: file.Name, Error: err.Error()})
				continue
			}
		}
		handle, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if err == nil {
			_, err = handle.Write(file.Data)
			if closeErr := handle.Close(); err == nil {
				err = closeErr
			}
		}
		if err != nil {
			result.Errors = append(result.Errors, UploadFileError{Name: file.Name, Error: err.Error()})
			continue
		}
		result.Uploaded = append(result.Uploaded, file.Name)
	}
	return result, nil
}

func validateUploadFileNames(fileNames []string) error {
	if len(fileNames) == 0 {
		return fmt.Errorf("%w: No files selected", ErrInvalidFileName)
	}
	seen := make(map[string]struct{}, len(fileNames))
	for _, name := range fileNames {
		switch {
		case name == "", name == ".", name == "..", strings.ContainsRune(name, '\x00'):
			label := name
			if label == "" {
				label = "(empty)"
			}
			return fmt.Errorf("%w: Invalid file name: %s", ErrInvalidFileName, label)
		case strings.ContainsAny(name, `/\\`), filepath.Base(name) != name:
			return fmt.Errorf("%w: File names must not contain a path: %s", ErrInvalidFileName, name)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("%w: Duplicate file name in upload: %s", ErrInvalidFileName, name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func inspectUploadTargets(directory string, fileNames []string) (UploadTargetInspection, error) {
	result := UploadTargetInspection{Conflicts: []string{}, NonReplaceable: []string{}}
	for _, name := range fileNames {
		info, err := os.Lstat(filepath.Join(directory, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return UploadTargetInspection{}, err
		}
		result.Conflicts = append(result.Conflicts, name)
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			result.NonReplaceable = append(result.NonReplaceable, name)
		}
	}
	return result, nil
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func (s *Service) sessionReferencesFile(sessionID, filePath string) (bool, error) {
	if !webSessionIDPattern.MatchString(sessionID) || !filepath.IsAbs(filePath) {
		return false, nil
	}
	manager, _, _, closeManager, err := s.sessionManagerForRead(sessionID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if closeManager {
		defer manager.Close()
	}
	for _, entry := range manager.Entries() {
		var value any
		if json.Unmarshal(entry.RawJSON(), &value) != nil {
			continue
		}
		stringsInEntry := make([]string, 0)
		collectJSONStrings(value, &stringsInEntry)
		for _, candidate := range stringsInEntry {
			if containsExactPathReference(candidate, filePath) {
				return true, nil
			}
		}
	}
	return false, nil
}

func collectJSONStrings(value any, result *[]string) {
	switch typed := value.(type) {
	case string:
		*result = append(*result, typed)
	case []any:
		for _, item := range typed {
			collectJSONStrings(item, result)
		}
	case map[string]any:
		for _, item := range typed {
			collectJSONStrings(item, result)
		}
	}
}

func containsExactPathReference(text, filePath string) bool {
	target := normalizeReferenceSlashes(filePath)
	targets := []string{target}
	if strings.HasPrefix(target, "/") {
		targets = append(targets, "file://"+target)
	}
	haystacks := []string{normalizeReferenceSlashes(text)}
	if decoded, err := url.PathUnescape(text); err == nil && decoded != text {
		haystacks = append(haystacks, normalizeReferenceSlashes(decoded))
	}
	for _, haystack := range haystacks {
		for _, candidate := range targets {
			for offset := 0; ; {
				position := strings.Index(haystack[offset:], candidate)
				if position < 0 {
					break
				}
				position += offset
				beforeOK := position == 0 || !isPathReferenceCharacter(haystack[position-1])
				after := position + len(candidate)
				afterOK := after >= len(haystack) || !isPathReferenceCharacter(haystack[after]) || haystack[after] == ':' && after+1 < len(haystack) && haystack[after+1] >= '0' && haystack[after+1] <= '9'
				if beforeOK && afterOK {
					return true
				}
				offset = position + 1
			}
		}
	}
	return false
}

func normalizeReferenceSlashes(value string) string {
	return strings.ReplaceAll(value, "\\", "/")
}

func isPathReferenceCharacter(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || strings.ContainsRune("._~+%@/\\:-", rune(value))
}

package application

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

var (
	ErrDirectoryNotFound = errors.New("directory does not exist")
	ErrPathNotDirectory  = errors.New("path is not a directory")
)

type DirectoryEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type DirectoryBrowseResult struct {
	Path        string
	ParentPath  *string
	Directories []DirectoryEntry
	Drives      []DirectoryEntry
}

func (s *Service) BrowseDirectories(ctx context.Context, requested string) (DirectoryBrowseResult, error) {
	if cause := context.Cause(normalizeContext(ctx)); cause != nil {
		return DirectoryBrowseResult{}, cause
	}
	requested = strings.TrimSpace(requested)
	if runtime.GOOS == "windows" && requested == "" {
		return DirectoryBrowseResult{Drives: listWindowsDrives()}, nil
	}
	if requested == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return DirectoryBrowseResult{}, err
		}
		requested = home
	}
	resolved, err := normalizeBrowseDirectory(requested)
	if err != nil {
		return DirectoryBrowseResult{}, err
	}
	real, err := filepath.EvalSymlinks(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return DirectoryBrowseResult{}, ErrDirectoryNotFound
	}
	if err != nil {
		return DirectoryBrowseResult{}, err
	}
	info, err := os.Stat(real)
	if errors.Is(err, os.ErrNotExist) {
		return DirectoryBrowseResult{}, ErrDirectoryNotFound
	}
	if err != nil {
		return DirectoryBrowseResult{}, err
	}
	if !info.IsDir() {
		return DirectoryBrowseResult{}, ErrPathNotDirectory
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		return DirectoryBrowseResult{}, err
	}
	directories := make([]DirectoryEntry, 0)
	for _, entry := range entries {
		entryPath := filepath.Join(real, entry.Name())
		isDirectory := entry.IsDir()
		if entry.Type()&os.ModeSymlink != 0 {
			target, targetErr := filepath.EvalSymlinks(entryPath)
			if targetErr != nil {
				continue
			}
			targetInfo, targetErr := os.Stat(target)
			if targetErr != nil || !targetInfo.IsDir() {
				continue
			}
			isDirectory = true
		}
		if isDirectory {
			directories = append(directories, DirectoryEntry{Name: entry.Name(), Path: entryPath})
		}
	}
	sort.SliceStable(directories, func(left, right int) bool {
		return strings.ToLower(directories[left].Name) < strings.ToLower(directories[right].Name)
	})
	result := DirectoryBrowseResult{Path: real, Directories: directories}
	parent := filepath.Dir(real)
	if parent != real {
		result.ParentPath = &parent
	}
	return result, nil
}

func normalizeBrowseDirectory(value string) (string, error) {
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if value == "~" {
			value = home
		} else {
			value = filepath.Join(home, value[2:])
		}
	}
	return filepath.Abs(value)
}

func listWindowsDrives() []DirectoryEntry {
	result := make([]DirectoryEntry, 0)
	for letter := 'A'; letter <= 'Z'; letter++ {
		path := string(letter) + `:\`
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			result = append(result, DirectoryEntry{Name: string(letter) + ":", Path: path})
		}
	}
	return result
}

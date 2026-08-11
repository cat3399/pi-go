package application

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	fileIndexClientCap  = 5_000
	fileIndexGitCap     = 200_000
	fileIndexWalkCap    = 50_000
	fileIndexWalkDepth  = 8
	fileIndexQueryLimit = 500
	fileIndexCacheTTL   = 10 * time.Second
	fileIndexCacheLimit = 20
	fileIndexGitBytes   = 64 * 1024 * 1024
	fileIndexMatchLimit = 20
)

type FileIndexEntry struct {
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
}

type FileIndexResult struct {
	Files     []string
	Truncated bool
	Matches   []FileIndexEntry
	HasQuery  bool
}

type fileIndexListing struct {
	files         []string
	hardTruncated bool
}

type fileIndexCacheEntry struct {
	listing fileIndexListing
	entries []FileIndexEntry
	expires time.Time
}

func (s *Service) QueryFileIndex(ctx context.Context, cwd, query string) (FileIndexResult, error) {
	resolved, err := s.authorizeResourcePath(cwd, true)
	if err != nil {
		return FileIndexResult{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return FileIndexResult{}, err
	}
	if !info.IsDir() {
		return FileIndexResult{}, ErrPathNotDirectory
	}
	query = truncateRunes(query, fileIndexQueryLimit)
	now := time.Now()

	s.fileIndexMu.Lock()
	cached, exists := s.fileIndexCache[resolved]
	if !exists || !now.Before(cached.expires) {
		listing, listed := listFilesWithGit(ctx, resolved)
		if !listed {
			listing = listFilesWithWalk(resolved)
		}
		for key, entry := range s.fileIndexCache {
			if !now.Before(entry.expires) {
				delete(s.fileIndexCache, key)
			}
		}
		if len(s.fileIndexCache) >= fileIndexCacheLimit {
			clear(s.fileIndexCache)
		}
		cached = fileIndexCacheEntry{listing: listing, expires: now.Add(fileIndexCacheTTL)}
		s.fileIndexCache[resolved] = cached
	}
	if query != "" {
		if cached.entries == nil {
			cached.entries = buildFileIndexEntries(cached.listing.files)
			s.fileIndexCache[resolved] = cached
		}
		matches := filterFileIndexEntries(cached.entries, query, fileIndexMatchLimit)
		s.fileIndexMu.Unlock()
		return FileIndexResult{Matches: matches, HasQuery: true}, nil
	}
	files := append([]string(nil), cached.listing.files...)
	s.fileIndexMu.Unlock()
	truncated := cached.listing.hardTruncated || len(files) > fileIndexClientCap
	if len(files) > fileIndexClientCap {
		files = files[:fileIndexClientCap]
	}
	return FileIndexResult{Files: files, Truncated: truncated}, nil
}

func listFilesWithGit(ctx context.Context, cwd string) (fileIndexListing, bool) {
	output, err := runGitRaw(ctx, cwd, fileIndexGitBytes,
		"ls-files", "--cached", "--others", "--exclude-standard", "-z",
	)
	if err != nil {
		return fileIndexListing{}, false
	}
	records := strings.Split(string(output), "\x00")
	files := make([]string, 0, min(len(records), fileIndexGitCap))
	for _, record := range records {
		if record == "" {
			continue
		}
		if len(files) >= fileIndexGitCap {
			return fileIndexListing{files: files, hardTruncated: true}, true
		}
		files = append(files, record)
	}
	return fileIndexListing{files: files}, true
}

func listFilesWithWalk(cwd string) fileIndexListing {
	type queuedDirectory struct {
		absolute string
		relative string
		depth    int
	}
	files := make([]string, 0)
	queue := []queuedDirectory{{absolute: cwd}}
	for head := 0; head < len(queue); head++ {
		current := queue[head]
		entries, err := os.ReadDir(current.absolute)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if ignoredFileName(entry.Name()) {
				continue
			}
			relative := entry.Name()
			if current.relative != "" {
				relative = current.relative + "/" + entry.Name()
			}
			if entry.IsDir() {
				if current.depth+1 <= fileIndexWalkDepth {
					queue = append(queue, queuedDirectory{
						absolute: filepath.Join(current.absolute, entry.Name()), relative: relative, depth: current.depth + 1,
					})
				}
			} else if entry.Type().IsRegular() {
				if len(files) >= fileIndexWalkCap {
					return fileIndexListing{files: files, hardTruncated: true}
				}
				files = append(files, relative)
			}
		}
	}
	return fileIndexListing{files: files}
}

func buildFileIndexEntries(files []string) []FileIndexEntry {
	directories := make(map[string]struct{})
	for _, file := range files {
		for offset := strings.IndexByte(file, '/'); offset >= 0; {
			directories[file[:offset]] = struct{}{}
			next := strings.IndexByte(file[offset+1:], '/')
			if next < 0 {
				break
			}
			offset += next + 1
		}
	}
	entries := make([]FileIndexEntry, 0, len(directories)+len(files))
	for directory := range directories {
		entries = append(entries, FileIndexEntry{Path: directory, IsDir: true})
	}
	for _, file := range files {
		if file != "" {
			entries = append(entries, FileIndexEntry{Path: file})
		}
	}
	sort.SliceStable(entries, func(left, right int) bool {
		leftDepth, rightDepth := strings.Count(entries[left].Path, "/"), strings.Count(entries[right].Path, "/")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return entries[left].Path < entries[right].Path
	})
	return entries
}

func filterFileIndexEntries(entries []FileIndexEntry, query string, limit int) []FileIndexEntry {
	lowerQuery := strings.ToLower(query)
	if lowerQuery == "" {
		if len(entries) < limit {
			limit = len(entries)
		}
		return append([]FileIndexEntry(nil), entries[:limit]...)
	}
	type scoredEntry struct {
		entry FileIndexEntry
		score int
	}
	scored := make([]scoredEntry, 0)
	for _, entry := range entries {
		score := scoreFileIndexEntry(entry, lowerQuery)
		if score > 0 {
			scored = append(scored, scoredEntry{entry: entry, score: score})
		}
	}
	sort.SliceStable(scored, func(left, right int) bool {
		if scored[left].score != scored[right].score {
			return scored[left].score > scored[right].score
		}
		leftDepth, rightDepth := strings.Count(scored[left].entry.Path, "/"), strings.Count(scored[right].entry.Path, "/")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return scored[left].entry.Path < scored[right].entry.Path
	})
	if len(scored) < limit {
		limit = len(scored)
	}
	result := make([]FileIndexEntry, limit)
	for index := range result {
		result[index] = scored[index].entry
	}
	return result
}

func scoreFileIndexEntry(entry FileIndexEntry, lowerQuery string) int {
	lowerPath := strings.ToLower(entry.Path)
	score := 0
	if strings.Contains(lowerQuery, "/") {
		switch {
		case lowerPath == lowerQuery:
			score = 100
		case strings.HasPrefix(lowerPath, lowerQuery):
			score = 80
		case strings.Contains(lowerPath, lowerQuery):
			score = 50
		case isStringSubsequence(lowerQuery, lowerPath):
			score = 10
		}
	} else {
		name := lowerPath
		if slash := strings.LastIndexByte(lowerPath, '/'); slash >= 0 {
			name = lowerPath[slash+1:]
		}
		switch {
		case name == lowerQuery:
			score = 100
		case strings.HasPrefix(name, lowerQuery):
			score = 80
		case strings.Contains(name, lowerQuery):
			score = 50
		case strings.Contains(lowerPath, lowerQuery):
			score = 30
		case isStringSubsequence(lowerQuery, lowerPath):
			score = 10
		}
	}
	if entry.IsDir && score > 0 {
		score += 10
	}
	return score
}

func isStringSubsequence(needle, haystack string) bool {
	if needle == "" {
		return true
	}
	needleRunes := []rune(needle)
	position := 0
	for _, candidate := range haystack {
		if candidate == needleRunes[position] {
			position++
			if position == len(needleRunes) {
				return true
			}
		}
	}
	return false
}

package application

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	gitChangesTimeout   = 10 * time.Second
	gitStatusMaxBytes   = 8 * 1024 * 1024
	gitPatchMaxBytes    = textPreviewMaxBytes * 4
	textPreviewMaxBytes = 256 * 1024
)

type GitFileStatusKind string

const (
	GitStatusModified  GitFileStatusKind = "modified"
	GitStatusAdded     GitFileStatusKind = "added"
	GitStatusDeleted   GitFileStatusKind = "deleted"
	GitStatusRenamed   GitFileStatusKind = "renamed"
	GitStatusUntracked GitFileStatusKind = "untracked"
	GitStatusConflict  GitFileStatusKind = "conflict"
)

type GitFileStatus struct {
	FilePath       string            `json:"filePath"`
	Status         GitFileStatusKind `json:"status"`
	Code           string            `json:"code"`
	IndexStatus    string            `json:"indexStatus"`
	WorktreeStatus string            `json:"worktreeStatus"`
}

type GitStatus struct {
	IsGitRepository bool            `json:"isGitRepository"`
	RepositoryRoot  *string         `json:"repositoryRoot"`
	Files           []GitFileStatus `json:"files"`
	Additions       int64           `json:"additions"`
	Deletions       int64           `json:"deletions"`
}

type GitFileDiff struct {
	Supported bool              `json:"supported"`
	Status    GitFileStatusKind `json:"status,omitempty"`
	Patch     string            `json:"patch,omitempty"`
}

type gitPorcelainEntry struct {
	path           string
	originalPath   string
	indexStatus    byte
	worktreeStatus byte
}

func (s *Service) GetGitStatus(ctx context.Context, cwd string) (GitStatus, error) {
	resolved, err := s.authorizeResourcePath(cwd, true)
	if err != nil {
		return GitStatus{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return GitStatus{}, err
	}
	if !info.IsDir() {
		return GitStatus{}, ErrPathNotDirectory
	}
	repositoryRoot, err := findGitRepositoryRoot(ctx, resolved)
	if err != nil {
		return GitStatus{Files: []GitFileStatus{}}, nil
	}
	entries, err := readGitStatusEntries(ctx, repositoryRoot)
	if err != nil {
		return GitStatus{}, err
	}
	additions, deletions := readTrackedLineStats(ctx, repositoryRoot, resolved)
	files := make([]GitFileStatus, 0, len(entries))
	for _, entry := range entries {
		filePath := filepath.Join(repositoryRoot, filepath.FromSlash(entry.path))
		if !resourcePathWithin(resolved, filePath) {
			continue
		}
		status, code := classifyGitStatus(entry)
		files = append(files, GitFileStatus{
			FilePath: filePath, Status: status, Code: code,
			IndexStatus: string(entry.indexStatus), WorktreeStatus: string(entry.worktreeStatus),
		})
		if status == GitStatusUntracked {
			additions += countUntrackedTextLines(filePath)
		}
	}
	root := repositoryRoot
	return GitStatus{
		IsGitRepository: true, RepositoryRoot: &root, Files: files,
		Additions: additions, Deletions: deletions,
	}, nil
}

func (s *Service) GetGitFileDiff(ctx context.Context, cwd, filePath string) (GitFileDiff, error) {
	resolvedCWD, err := s.authorizeResourcePath(cwd, true)
	if err != nil {
		return GitFileDiff{}, err
	}
	info, err := os.Stat(resolvedCWD)
	if err != nil {
		return GitFileDiff{}, err
	}
	if !info.IsDir() {
		return GitFileDiff{}, ErrPathNotDirectory
	}
	resolvedFile, err := s.authorizeResourcePath(filePath, false)
	if err != nil {
		return GitFileDiff{}, err
	}
	repositoryRoot, err := findGitRepositoryRoot(ctx, resolvedCWD)
	if err != nil || !resourcePathWithin(repositoryRoot, resolvedFile) {
		return GitFileDiff{Supported: false}, nil
	}
	relativePath, err := filepath.Rel(repositoryRoot, resolvedFile)
	if err != nil {
		return GitFileDiff{Supported: false}, nil
	}
	relativePath = filepath.ToSlash(relativePath)
	entries, err := readGitStatusEntries(ctx, repositoryRoot)
	if err != nil {
		return GitFileDiff{}, err
	}
	var selected *gitPorcelainEntry
	for index := range entries {
		if entries[index].path == relativePath {
			selected = &entries[index]
			break
		}
	}
	if selected == nil {
		return GitFileDiff{Supported: false}, nil
	}
	status, _ := classifyGitStatus(*selected)
	if status == GitStatusDeleted {
		patch, ok := createTrackedFilePatch(ctx, repositoryRoot, relativePath, selected.originalPath)
		if !ok || !strings.Contains(patch, "\n@@ ") {
			return GitFileDiff{Supported: false}, nil
		}
		return GitFileDiff{Supported: true, Status: status, Patch: patch}, nil
	}
	fileInfo, err := os.Lstat(resolvedFile)
	if err != nil || !fileInfo.Mode().IsRegular() || fileInfo.Size() > textPreviewMaxBytes {
		return GitFileDiff{Supported: false}, nil
	}
	content, err := os.ReadFile(resolvedFile)
	if err != nil || bytes.IndexByte(content, 0) >= 0 {
		return GitFileDiff{Supported: false}, nil
	}
	var patch string
	if status == GitStatusUntracked {
		patch = createAddedFilePatch(relativePath, string(content))
	} else if trackedPatch, ok := createTrackedFilePatch(ctx, repositoryRoot, relativePath, selected.originalPath); ok {
		patch = trackedPatch
	} else if status == GitStatusAdded {
		patch = createAddedFilePatch(relativePath, string(content))
	} else {
		return GitFileDiff{Supported: false}, nil
	}
	if !strings.Contains(patch, "\n@@ ") {
		return GitFileDiff{Supported: false}, nil
	}
	return GitFileDiff{Supported: true, Status: status, Patch: patch}, nil
}

func findGitRepositoryRoot(ctx context.Context, cwd string) (string, error) {
	output, err := runGitRaw(ctx, cwd, gitStatusMaxBytes, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(string(output))
	if root == "" {
		return "", errors.New("git repository root is empty")
	}
	return root, nil
}

func readGitStatusEntries(ctx context.Context, repositoryRoot string) ([]gitPorcelainEntry, error) {
	output, err := runGitRaw(ctx, repositoryRoot, gitStatusMaxBytes,
		"status", "--porcelain=v1", "-z", "--untracked-files=all",
	)
	if err != nil {
		return nil, err
	}
	return parseGitPorcelainV1(output), nil
}

func parseGitPorcelainV1(output []byte) []gitPorcelainEntry {
	records := bytes.Split(output, []byte{0})
	entries := make([]gitPorcelainEntry, 0, len(records))
	for index := 0; index < len(records); index++ {
		record := records[index]
		if len(record) < 4 || record[2] != ' ' {
			continue
		}
		entry := gitPorcelainEntry{path: string(record[3:]), indexStatus: record[0], worktreeStatus: record[1]}
		if usesRenamePath(record[0], record[1]) {
			index++
			if index < len(records) {
				entry.originalPath = string(records[index])
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

func usesRenamePath(indexStatus, worktreeStatus byte) bool {
	return indexStatus == 'R' || indexStatus == 'C' || worktreeStatus == 'R' || worktreeStatus == 'C'
}

func classifyGitStatus(entry gitPorcelainEntry) (GitFileStatusKind, string) {
	pair := string([]byte{entry.indexStatus, entry.worktreeStatus})
	if pair == "??" {
		return GitStatusUntracked, "U"
	}
	if pair == "DD" || pair == "AU" || pair == "UD" || pair == "UA" || pair == "DU" || pair == "AA" || pair == "UU" || strings.ContainsRune(pair, 'U') {
		return GitStatusConflict, "C"
	}
	if strings.ContainsRune(pair, 'D') {
		return GitStatusDeleted, "D"
	}
	if strings.ContainsAny(pair, "RC") {
		return GitStatusRenamed, "R"
	}
	if strings.ContainsRune(pair, 'A') {
		return GitStatusAdded, "A"
	}
	return GitStatusModified, "M"
}

func readTrackedLineStats(ctx context.Context, repositoryRoot, cwd string) (int64, int64) {
	relativeCWD, err := filepath.Rel(repositoryRoot, cwd)
	if err != nil {
		return 0, 0
	}
	pathspec := filepath.ToSlash(relativeCWD)
	if pathspec == "" || pathspec == "." {
		pathspec = "."
	}
	output, err := runGitRaw(ctx, repositoryRoot, gitStatusMaxBytes,
		"diff", "--no-color", "--no-ext-diff", "--numstat", "HEAD", "--", pathspec,
	)
	if err != nil {
		return 0, 0
	}
	var additions, deletions int64
	for _, line := range strings.Split(string(output), "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 {
			continue
		}
		if value, parseErr := strconv.ParseInt(parts[0], 10, 64); parseErr == nil {
			additions += value
		}
		if value, parseErr := strconv.ParseInt(parts[1], 10, 64); parseErr == nil {
			deletions += value
		}
	}
	return additions, deletions
}

func countUntrackedTextLines(filePath string) int64 {
	info, err := os.Lstat(filePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > textPreviewMaxBytes {
		return 0
	}
	content, err := os.ReadFile(filePath)
	if err != nil || len(content) == 0 || bytes.IndexByte(content, 0) >= 0 {
		return 0
	}
	lines := int64(bytes.Count(content, []byte{'\n'}))
	if content[len(content)-1] != '\n' {
		lines++
	}
	return lines
}

func createAddedFilePatch(gitPath, content string) string {
	hasTrailingNewline := strings.HasSuffix(content, "\n")
	lines := strings.Split(content, "\n")
	if hasTrailingNewline {
		lines = lines[:len(lines)-1]
	}
	prefixed := make([]string, len(lines))
	for index, line := range lines {
		prefixed[index] = "+" + line
	}
	body := strings.Join(prefixed, "\n")
	noNewlineMarker := ""
	if !hasTrailingNewline && len(lines) != 0 {
		noNewlineMarker = "\n\\ No newline at end of file"
	}
	return strings.Join([]string{
		"diff --git a/" + gitPath + " b/" + gitPath,
		"new file mode 100644",
		"--- /dev/null",
		"+++ b/" + gitPath,
		fmt.Sprintf("@@ -0,0 +1,%d @@", len(lines)),
		body + noNewlineMarker,
	}, "\n")
}

func createTrackedFilePatch(ctx context.Context, repositoryRoot, relativePath, originalPath string) (string, bool) {
	paths := []string{relativePath}
	if originalPath != "" && originalPath != relativePath {
		paths = []string{originalPath, relativePath}
	}
	arguments := []string{"diff", "--no-color", "--no-ext-diff", "--unified=3", "HEAD", "--"}
	arguments = append(arguments, paths...)
	output, err := runGitRaw(ctx, repositoryRoot, gitPatchMaxBytes, arguments...)
	if err != nil {
		return "", false
	}
	return string(output), true
}

func runGitRaw(ctx context.Context, cwd string, maxBytes int, arguments ...string) ([]byte, error) {
	operationContext, cancel := context.WithTimeout(normalizeContext(ctx), gitChangesTimeout)
	defer cancel()
	command := exec.CommandContext(operationContext, "git", append([]string{"-C", cwd}, arguments...)...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	output, err := command.Output()
	if errors.Is(operationContext.Err(), context.DeadlineExceeded) {
		return nil, errors.New("git operation timed out")
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if message := strings.TrimSpace(string(exit.Stderr)); message != "" {
				return nil, errors.New(message)
			}
		}
		return nil, err
	}
	if len(output) > maxBytes {
		return nil, fmt.Errorf("git output exceeded %d bytes", maxBytes)
	}
	return output, nil
}

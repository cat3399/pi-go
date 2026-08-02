package tool

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

func errorsJoinCancel(cause error) error { return errors.Join(ErrOperationCancelled, cause) }

type GrepInput struct {
	Pattern             string
	Path                *string
	Glob                *string
	IgnoreCase, Literal *bool
	Context, Limit      *int
}
type FindInput struct {
	Pattern string
	Path    *string
	Limit   *int
}
type LsInput struct {
	Path  *string
	Limit *int
}

func (s *FilesystemSuite) Find(ctx context.Context, input FindInput) (ToolResult, error) {
	if err := validText("pattern", input.Pattern); err != nil {
		return inputError(err)
	}
	rootInput := "."
	if input.Path != nil {
		rootInput = *input.Path
		if err := validText("path", rootInput); err != nil {
			return inputError(err)
		}
	}
	root, err := resolveToolPath(s.workingDir, rootInput)
	if err != nil {
		return ToolResult{}, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return ToolResult{}, fmt.Errorf("path not found: %s", root)
	}
	if !info.IsDir() {
		return ToolResult{}, fmt.Errorf("not a directory: %s", root)
	}
	limit := DefaultFindResults
	if input.Limit != nil {
		limit = *input.Limit
	}
	if limit < 1 {
		return inputError(fmt.Errorf("%w: limit must be at least 1", ErrInvalidFilesystemInput))
	}
	matcher, err := compileGlob(input.Pattern)
	if err != nil {
		return ToolResult{}, fmt.Errorf("invalid glob pattern %q: %w", input.Pattern, err)
	}
	entries, err := s.walk(ctx, root)
	if err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
	}
	var result []string
	more := false
	for _, entry := range entries {
		if matcher(entry.relative) {
			if len(result) >= limit {
				more = true
				break
			}
			if entry.isDir {
				result = append(result, entry.relative+"/")
			} else {
				result = append(result, entry.relative)
			}
		}
	}
	if len(result) == 0 {
		return ToolResult{Text: "No files found matching pattern"}, nil
	}
	return s.searchResult(result, more, limit, "results"), nil
}

func (s *FilesystemSuite) Ls(ctx context.Context, input LsInput) (ToolResult, error) {
	rootInput := "."
	if input.Path != nil {
		rootInput = *input.Path
		if err := validText("path", rootInput); err != nil {
			return inputError(err)
		}
	}
	root, err := resolveToolPath(s.workingDir, rootInput)
	if err != nil {
		return ToolResult{}, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return ToolResult{}, fmt.Errorf("path not found: %s", root)
	}
	if !info.IsDir() {
		return ToolResult{}, fmt.Errorf("not a directory: %s", root)
	}
	limit := DefaultLsEntries
	if input.Limit != nil {
		limit = *input.Limit
	}
	if limit < 1 {
		return inputError(fmt.Errorf("%w: limit must be at least 1", ErrInvalidFilesystemInput))
	}
	directoryEntries, err := os.ReadDir(root)
	if err != nil {
		return ToolResult{}, fmt.Errorf("cannot read directory: %w", err)
	}
	sort.Slice(directoryEntries, func(left, right int) bool {
		l, r := strings.ToLower(directoryEntries[left].Name()), strings.ToLower(directoryEntries[right].Name())
		if l == r {
			return directoryEntries[left].Name() < directoryEntries[right].Name()
		}
		return l < r
	})
	result := make([]string, 0, minInt(limit, len(directoryEntries)))
	more := false
	for _, entry := range directoryEntries {
		if err := context.Cause(ctx); err != nil {
			return ToolResult{Text: operationErrorText(err)}, errorsJoinCancel(err)
		}
		if len(result) >= limit {
			more = true
			break
		}
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		result = append(result, name)
	}
	if len(result) == 0 {
		return ToolResult{Text: "(empty directory)"}, nil
	}
	return s.searchResult(result, more, limit, "entries"), nil
}

func (s *FilesystemSuite) Grep(ctx context.Context, input GrepInput) (ToolResult, error) {
	if err := validText("pattern", input.Pattern); err != nil {
		return inputError(err)
	}
	rootInput := "."
	if input.Path != nil {
		rootInput = *input.Path
		if err := validText("path", rootInput); err != nil {
			return inputError(err)
		}
	}
	root, err := resolveToolPath(s.workingDir, rootInput)
	if err != nil {
		return ToolResult{}, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return ToolResult{}, fmt.Errorf("path not found: %s", root)
	}
	limit := DefaultGrepMatches
	if input.Limit != nil {
		limit = *input.Limit
	}
	if limit < 1 {
		return inputError(fmt.Errorf("%w: limit must be at least 1", ErrInvalidFilesystemInput))
	}
	contextLines := 0
	if input.Context != nil {
		contextLines = *input.Context
	}
	if contextLines < 0 {
		return inputError(fmt.Errorf("%w: context must not be negative", ErrInvalidFilesystemInput))
	}
	ignoreCase := input.IgnoreCase != nil && *input.IgnoreCase
	literal := input.Literal != nil && *input.Literal
	pattern := input.Pattern
	if literal {
		pattern = regexp.QuoteMeta(pattern)
	}
	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	expression, err := regexp.Compile(pattern)
	if err != nil {
		return ToolResult{}, fmt.Errorf("invalid grep pattern: %w", err)
	}
	var glob func(string) bool
	if input.Glob != nil {
		glob, err = compileGlob(*input.Glob)
		if err != nil {
			return ToolResult{}, fmt.Errorf("invalid glob pattern %q: %w", *input.Glob, err)
		}
	}
	var candidates []walkEntry
	if info.IsDir() {
		candidates, err = s.walk(ctx, root)
		if err != nil {
			return ToolResult{Text: operationErrorText(err)}, err
		}
	} else {
		candidates = []walkEntry{{absolute: root, relative: filepath.Base(root)}}
	}
	var output []string
	matches := 0
	linesTruncated := false
	more := false
	for _, candidate := range candidates {
		if candidate.isDir || (glob != nil && !glob(candidate.relative)) {
			continue
		}
		if err := context.Cause(ctx); err != nil {
			return ToolResult{Text: operationErrorText(err)}, errorsJoinCancel(err)
		}
		data, readErr := os.ReadFile(candidate.absolute)
		if readErr != nil || isBinary(data) || !utf8.Valid(data) {
			continue
		}
		lines := strings.Split(normalizeLF(string(data)), "\n")
		for lineIndex, line := range lines {
			if !expression.MatchString(line) {
				continue
			}
			if matches >= limit {
				more = true
				break
			}
			matches++
			start, end := lineIndex, lineIndex
			if contextLines > 0 {
				start = maxInt(0, lineIndex-contextLines)
				end = minInt(len(lines)-1, lineIndex+contextLines)
			}
			for index := start; index <= end; index++ {
				text, truncated := truncateGrepLine(lines[index], DefaultGrepLineRunes)
				linesTruncated = linesTruncated || truncated
				if index == lineIndex {
					output = append(output, fmt.Sprintf("%s:%d: %s", candidate.relative, index+1, text))
				} else {
					output = append(output, fmt.Sprintf("%s-%d- %s", candidate.relative, index+1, text))
				}
			}
		}
		if more {
			break
		}
	}
	if matches == 0 {
		return ToolResult{Text: "No matches found"}, nil
	}
	result := s.searchResult(output, more, limit, "matches")
	if linesTruncated {
		if result.Details == nil {
			result.Details = map[string]any{}
		}
		result.Details["linesTruncated"] = true
		result.Text += "\n\n[Some lines truncated to 500 chars. Use read to see full lines.]"
	}
	return result, nil
}

type walkEntry struct {
	absolute, relative string
	isDir              bool
}

func (s *FilesystemSuite) walk(ctx context.Context, root string) ([]walkEntry, error) {
	matcher, err := newIgnoreMatcher(root)
	if err != nil {
		return nil, err
	}
	var entries []walkEntry
	err = filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := context.Cause(ctx); err != nil {
			return errorsJoinCancel(err)
		}
		if current == root {
			return nil
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if matcher.ignored(relative, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entries = append(entries, walkEntry{absolute: current, relative: relative, isDir: entry.IsDir()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].relative < entries[right].relative })
	return entries, nil
}

func (s *FilesystemSuite) searchResult(lines []string, reached bool, limit int, kind string) ToolResult {
	raw := strings.Join(lines, "\n")
	truncation := truncateFilesystemHead(raw, DefaultFilesystemMaxLines, s.maxBytes)
	output := truncation.Content
	details := map[string]any{}
	if reached {
		details[kind+"LimitReached"] = limit
		output += fmt.Sprintf("\n\n[%d %s limit reached. Use limit=%d for more, or refine pattern.]", limit, kind, limit*2)
	}
	if truncation.Truncated {
		details["truncation"] = s.truncationDetails(truncation)
		output += fmt.Sprintf("\n\n[%s limit reached.]", formatSize(s.maxBytes))
	}
	if len(details) == 0 {
		details = nil
	}
	return ToolResult{Text: output, Details: details}
}

// compileGlob supports Pi's documented *, ?, ** and character-class patterns
// with slash-aware matching. Patterns are anchored to the search root.
func compileGlob(pattern string) (func(string) bool, error) {
	if pattern == "" {
		return nil, fmt.Errorf("empty pattern")
	}
	expression := "^"
	for index := 0; index < len(pattern); index++ {
		character := pattern[index]
		switch character {
		case '*':
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				index++
				if index+1 < len(pattern) && pattern[index+1] == '/' {
					index++
					expression += "(?:.*/)?"
				} else {
					expression += ".*"
				}
			} else {
				expression += "[^/]*"
			}
		case '?':
			expression += "[^/]"
		case '[':
			end := index + 1
			for end < len(pattern) && pattern[end] != ']' {
				end++
			}
			if end == len(pattern) {
				return nil, fmt.Errorf("unterminated character class")
			}
			expression += pattern[index : end+1]
			index = end
		default:
			expression += regexp.QuoteMeta(string(character))
		}
	}
	expression += "$"
	compiled, err := regexp.Compile(expression)
	if err != nil {
		return nil, err
	}
	return compiled.MatchString, nil
}

type ignoreMatcher struct {
	root  string
	rules []ignoreRule
}
type ignoreRule struct {
	pattern           string
	negate, directory bool
}

func newIgnoreMatcher(root string) (ignoreMatcher, error) {
	matcher := ignoreMatcher{root: root}
	_ = filepath.WalkDir(root, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(filepath.Join(current, ".gitignore"))
		if readErr != nil {
			return nil
		}
		base, _ := filepath.Rel(root, current)
		for _, line := range strings.Split(normalizeLF(string(data)), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			rule := ignoreRule{negate: strings.HasPrefix(line, "!")}
			if rule.negate {
				line = line[1:]
			}
			rule.directory = strings.HasSuffix(line, "/")
			line = strings.TrimSuffix(line, "/")
			if line == "" {
				continue
			}
			if base != "." {
				line = path.Join(filepath.ToSlash(base), line)
			}
			rule.pattern = line
			matcher.rules = append(matcher.rules, rule)
		}
		return nil
	})
	return matcher, nil
}
func (m ignoreMatcher) ignored(relative string, isDirectory bool) bool {
	if relative == ".git" || strings.HasPrefix(relative, ".git/") || relative == "node_modules" || strings.HasPrefix(relative, "node_modules/") {
		return true
	}
	ignored := false
	for _, rule := range m.rules {
		if rule.directory && !isDirectory {
			continue
		}
		match, _ := compileGlob(rule.pattern)
		if match(relative) || (!strings.Contains(rule.pattern, "/") && match(path.Base(relative))) {
			ignored = !rule.negate
		}
	}
	return ignored
}

func decodeGrepInput(raw []byte) (GrepInput, error) {
	values, err := strictObject(raw, fields("pattern", "path", "glob", "ignoreCase", "literal", "context", "limit"))
	if err != nil {
		return GrepInput{}, err
	}
	pattern, err := requiredString(values, "pattern")
	if err != nil {
		return GrepInput{}, err
	}
	pathValue, err := optionalString(values, "path")
	if err != nil {
		return GrepInput{}, err
	}
	glob, err := optionalString(values, "glob")
	if err != nil {
		return GrepInput{}, err
	}
	ignoreCase, err := optionalBool(values, "ignoreCase")
	if err != nil {
		return GrepInput{}, err
	}
	literal, err := optionalBool(values, "literal")
	if err != nil {
		return GrepInput{}, err
	}
	contextValue, err := optionalInt(values, "context")
	if err != nil {
		return GrepInput{}, err
	}
	limit, err := optionalInt(values, "limit")
	return GrepInput{pattern, pathValue, glob, ignoreCase, literal, contextValue, limit}, err
}
func decodeFindInput(raw []byte) (FindInput, error) {
	values, err := strictObject(raw, fields("pattern", "path", "limit"))
	if err != nil {
		return FindInput{}, err
	}
	pattern, err := requiredString(values, "pattern")
	if err != nil {
		return FindInput{}, err
	}
	pathValue, err := optionalString(values, "path")
	if err != nil {
		return FindInput{}, err
	}
	limit, err := optionalInt(values, "limit")
	return FindInput{pattern, pathValue, limit}, err
}
func decodeLsInput(raw []byte) (LsInput, error) {
	values, err := strictObject(raw, fields("path", "limit"))
	if err != nil {
		return LsInput{}, err
	}
	pathValue, err := optionalString(values, "path")
	if err != nil {
		return LsInput{}, err
	}
	limit, err := optionalInt(values, "limit")
	return LsInput{pathValue, limit}, err
}

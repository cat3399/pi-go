package tool

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
	if err := contextFailure(ctx); err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
	}
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
	if err := contextFailure(ctx); err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
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
		if err := contextFailure(ctx); err != nil {
			return ToolResult{Text: operationErrorText(err)}, err
		}
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
	if err := contextFailure(ctx); err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
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
	if err := contextFailure(ctx); err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
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
	if err := contextFailure(ctx); err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
	}
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
	if err := contextFailure(ctx); err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
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
		if err := contextFailure(ctx); err != nil {
			return ToolResult{Text: operationErrorText(err)}, err
		}
		if readErr != nil || isBinary(data) || !utf8.Valid(data) {
			continue
		}
		lines := strings.Split(normalizeLF(string(data)), "\n")
		for lineIndex, line := range lines {
			if err := contextFailure(ctx); err != nil {
				return ToolResult{Text: operationErrorText(err)}, err
			}
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
	result := s.grepResult(output, more, limit)
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
	matcher, err := newIgnoreMatcher(ctx, root)
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
		if matcher.ignored(current, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if err := matcher.addFile(ctx, current); err != nil {
				return err
			}
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

func (s *FilesystemSuite) grepResult(lines []string, reached bool, limit int) ToolResult {
	raw := strings.Join(lines, "\n")
	// Grep's match limit already bounds matches, while context can legitimately
	// exceed 2,000 lines. Only the byte ceiling applies to the formatted output.
	truncation := truncateFilesystemHead(raw, len(lines)+1, s.maxBytes)
	output := truncation.Content
	details := map[string]any{}
	if reached {
		details["matchesLimitReached"] = limit
		output += fmt.Sprintf("\n\n[%d matches limit reached. Use limit=%d for more, or refine pattern.]", limit, limit*2)
	}
	if truncation.Truncated {
		details["truncation"] = map[string]any{
			"truncated": truncation.Truncated, "truncatedBy": truncation.TruncatedBy,
			"totalLines": truncation.TotalLines, "totalBytes": truncation.TotalBytes,
			"outputLines": truncation.OutputLines, "outputBytes": truncation.OutputBytes,
			"maxLines": len(lines) + 1, "maxBytes": s.maxBytes,
			"firstLineExceedsLimit": truncation.FirstLineLarge,
		}
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
			class := pattern[index+1 : end]
			if class == "" {
				return nil, fmt.Errorf("empty character class")
			}
			if strings.HasPrefix(class, "!") || strings.HasPrefix(class, "^") {
				class = "^" + class[1:]
			}
			expression += "[" + class + "]"
			index = end
		case '\\':
			return nil, fmt.Errorf("%w: backslash escapes are not supported in glob patterns", ErrUnsupportedFilesystemFeature)
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

type ignoreMatcher struct{ rules []ignoreRule }
type ignoreRule struct {
	base              string
	pattern           string
	match             func(string) bool
	basename          bool
	negate, directory bool
}

func newIgnoreMatcher(ctx context.Context, root string) (*ignoreMatcher, error) {
	if err := contextFailure(ctx); err != nil {
		return nil, err
	}
	matcher := &ignoreMatcher{}
	boundary, err := gitIgnoreBoundary(ctx, root)
	if err != nil {
		return nil, err
	}
	for _, directory := range directoryChain(boundary, root) {
		if err := matcher.addFile(ctx, directory); err != nil {
			return nil, err
		}
	}
	return matcher, nil
}

func gitIgnoreBoundary(ctx context.Context, root string) (string, error) {
	root = filepath.Clean(root)
	for current := root; ; current = filepath.Dir(current) {
		if err := contextFailure(ctx); err != nil {
			return "", err
		}
		_, err := os.Lstat(filepath.Join(current, ".git"))
		if err == nil {
			return current, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("inspect git boundary: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return root, nil
		}
	}
}

func directoryChain(first, last string) []string {
	first, last = filepath.Clean(first), filepath.Clean(last)
	chain := []string{last}
	for current := last; current != first; {
		parent := filepath.Dir(current)
		if parent == current {
			return []string{last}
		}
		chain = append(chain, parent)
		current = parent
	}
	for left, right := 0, len(chain)-1; left < right; left, right = left+1, right-1 {
		chain[left], chain[right] = chain[right], chain[left]
	}
	return chain
}

func (m *ignoreMatcher) addFile(ctx context.Context, directory string) error {
	if err := contextFailure(ctx); err != nil {
		return err
	}
	ignorePath := filepath.Join(directory, ".gitignore")
	data, err := os.ReadFile(ignorePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read ignore rules %s: %w", ignorePath, err)
	}
	if err := contextFailure(ctx); err != nil {
		return err
	}
	for lineNumber, line := range strings.Split(normalizeLF(string(data)), "\n") {
		if err := contextFailure(ctx); err != nil {
			return err
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rule := ignoreRule{base: directory, negate: strings.HasPrefix(line, "!")}
		if rule.negate {
			line = strings.TrimPrefix(line, "!")
		}
		if strings.Contains(line, `\`) {
			return fmt.Errorf("%w: %s:%d uses unsupported escape syntax", ErrUnsupportedFilesystemFeature, ignorePath, lineNumber+1)
		}
		rule.directory = strings.HasSuffix(line, "/")
		line = strings.TrimSuffix(line, "/")
		anchored := strings.HasPrefix(line, "/")
		line = strings.TrimPrefix(line, "/")
		if line == "" {
			return fmt.Errorf("%w: %s:%d has an empty rule", ErrInvalidFilesystemInput, ignorePath, lineNumber+1)
		}
		rule.basename = !anchored && !strings.Contains(line, "/")
		rule.pattern = line
		rule.match, err = compileGlob(line)
		if err != nil {
			return fmt.Errorf("%w: invalid .gitignore pattern at %s:%d: %v", ErrInvalidFilesystemInput, ignorePath, lineNumber+1, err)
		}
		m.rules = append(m.rules, rule)
	}
	return nil
}

func (m *ignoreMatcher) ignored(absolute string, isDirectory bool) bool {
	baseName := filepath.Base(absolute)
	if baseName == ".git" || baseName == "node_modules" {
		return true
	}
	ignored := false
	for _, rule := range m.rules {
		if rule.directory && !isDirectory {
			continue
		}
		relative, err := filepath.Rel(rule.base, absolute)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		relative = filepath.ToSlash(relative)
		candidate := relative
		if rule.basename {
			candidate = filepath.Base(relative)
		}
		if rule.match(candidate) {
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

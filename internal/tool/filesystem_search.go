package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
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
	if err := validFilesystemArgument("pattern", input.Pattern); err != nil {
		return inputError(err)
	}
	rootInput := "."
	if input.Path != nil {
		rootInput = *input.Path
		if err := validFilesystemArgument("path", rootInput); err != nil {
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
	effectivePattern := input.Pattern
	matchAbsolutePath := filepath.IsAbs(input.Pattern)
	if strings.Contains(input.Pattern, "/") && !matchAbsolutePath && !strings.HasPrefix(input.Pattern, "**/") && input.Pattern != "**" {
		effectivePattern = "**/" + input.Pattern
	}
	matcher, err := compileFindGlob(filepath.ToSlash(effectivePattern))
	if err != nil {
		return ToolResult{}, fmt.Errorf("invalid glob pattern %q: %w", input.Pattern, err)
	}
	var result []string
	more := false
	err = s.walk(ctx, root, []string{".gitignore", ".ignore", ".fdignore"}, vcsIgnoreGlobal, func(entry walkEntry) (bool, error) {
		candidate := entry.relative
		if matchAbsolutePath {
			candidate = filepath.ToSlash(entry.absolute)
		} else if !strings.Contains(input.Pattern, "/") {
			candidate = filepath.Base(candidate)
		}
		if matcher(candidate) {
			if len(result) >= limit {
				more = true
				return false, nil
			}
			if entry.isDir {
				result = append(result, entry.relative+"/")
			} else {
				result = append(result, entry.relative)
			}
			if len(result) >= limit {
				more = true
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
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
		if err := validFilesystemArgument("path", rootInput); err != nil {
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
	result := make([]string, 0, maxInt(0, minInt(limit, len(directoryEntries))))
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
		entryInfo, statErr := os.Stat(filepath.Join(root, name))
		if statErr != nil {
			continue
		}
		if entryInfo.IsDir() {
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
	if err := validFilesystemArgument("pattern", input.Pattern); err != nil {
		return inputError(err)
	}
	rootInput := "."
	if input.Path != nil {
		rootInput = *input.Path
		if err := validFilesystemArgument("path", rootInput); err != nil {
			return inputError(err)
		}
	}
	root, err := resolveToolPath(s.workingDir, rootInput)
	if err != nil {
		return ToolResult{}, err
	}
	// A filesystem driver can make Stat/ReadDir uninterruptible while a hard
	// mount is unhealthy. Candidate file reads below become cancellable once a
	// regular handle exists; no background goroutine is abandoned for this
	// platform-level limitation.
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
	limit = maxInt(1, limit)
	contextLines := 0
	if input.Context != nil {
		contextLines = *input.Context
	}
	contextLines = maxInt(0, contextLines)
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
		if err := validFilesystemArgument("glob", *input.Glob); err != nil {
			return inputError(err)
		}
		glob, err = compileGrepGlob(*input.Glob)
		if err != nil {
			return ToolResult{}, fmt.Errorf("invalid glob pattern %q: %w", *input.Glob, err)
		}
	}
	globPattern := ""
	if input.Glob != nil {
		globPattern = *input.Glob
	}
	output := newGrepOutput(s.maxBytes)
	matches := 0
	linesTruncated := false
	more := false
	consumeCandidate := func(candidate walkEntry) (bool, error) {
		globCandidate := candidate.relative
		if glob != nil && !strings.Contains(globPattern, "/") {
			globCandidate = filepath.Base(globCandidate)
		}
		if candidate.isDir || (glob != nil && !glob(globCandidate)) {
			return true, nil
		}
		if err := context.Cause(ctx); err != nil {
			return false, errorsJoinCancel(err)
		}
		openFile := s.openSearchFile
		if openFile == nil {
			openFile = openBoundedRegularSearchFile
		}
		file, inputBytes, openErr := openFile(candidate.absolute)
		if openErr != nil {
			// ripgrep ignores FIFOs, devices, sockets, and unreadable candidates.
			// openBoundedRegularSearchFile rejects those before an I/O read.
			return true, nil
		}
		stopCancellation := watchReadCancellation(ctx, file)
		fileResult, scanErr := scanGrepRegularFile(
			ctx, file, inputBytes, candidate.relative, expression,
			contextLines, limit-matches, s.maxBytes,
		)
		stopCancellation()
		closeErr := file.Close()
		if err := contextFailure(ctx); err != nil {
			return false, err
		}
		if scanErr != nil {
			if errors.Is(scanErr, ErrOperationCancelled) {
				return false, scanErr
			}
			return true, nil
		}
		if closeErr != nil || fileResult.binary {
			return true, nil
		}
		output.append(fileResult.output)
		matches += fileResult.matches
		linesTruncated = linesTruncated || fileResult.linesTruncated
		if matches >= limit {
			more = true
			return false, nil
		}
		return true, nil
	}
	if info.IsDir() {
		err = s.walk(ctx, root, []string{".gitignore", ".ignore", ".rgignore"}, vcsIgnoreDisabled, consumeCandidate)
	} else {
		_, err = consumeCandidate(walkEntry{absolute: root, relative: filepath.Base(root)})
	}
	if err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
	}
	if matches == 0 {
		return ToolResult{Text: "No matches found"}, nil
	}
	return s.grepResult(output, more, limit, linesTruncated), nil
}

type walkEntry struct {
	absolute, relative string
	isDir              bool
}

type vcsIgnoreScope uint8

const (
	// vcsIgnoreDisabled is ripgrep outside a repository: .gitignore files are
	// skipped until traversal enters a real Git boundary.
	vcsIgnoreDisabled vcsIgnoreScope = iota
	// vcsIgnoreRepository applies only rules at or below the nearest Git root;
	// every nested repository advances the active VCS floor.
	vcsIgnoreRepository
	// vcsIgnoreGlobal is fd --no-require-git outside a repository: ancestor
	// .gitignore rules remain active across nested repositories, while nested
	// .gitignore frames are still loaded and popped lexically.
	vcsIgnoreGlobal
)

func (s *FilesystemSuite) walk(
	ctx context.Context,
	root string,
	ignoreFiles []string,
	outsideRepositoryVCS vcsIgnoreScope,
	visit func(walkEntry) (bool, error),
) error {
	matcher, err := newIgnoreMatcher(ctx, root, ignoreFiles, outsideRepositoryVCS)
	if err != nil {
		return err
	}
	readDirectory := s.readSearchDir
	if readDirectory == nil {
		readDirectory = os.ReadDir
	}
	var walkDirectory func(string) (bool, error)
	walkDirectory = func(directory string) (bool, error) {
		if err := context.Cause(ctx); err != nil {
			return false, errorsJoinCancel(err)
		}
		directoryEntries, err := readDirectory(directory)
		if err != nil {
			return false, err
		}
		sort.Slice(directoryEntries, func(left, right int) bool {
			return directoryEntries[left].Name() < directoryEntries[right].Name()
		})
		for _, entry := range directoryEntries {
			if err := context.Cause(ctx); err != nil {
				return false, errorsJoinCancel(err)
			}
			current := filepath.Join(directory, entry.Name())
			isDirectory := entry.IsDir()
			ignored, err := matcher.ignored(current, isDirectory)
			if err != nil {
				return false, err
			}
			if ignored {
				continue
			}
			relative, err := filepath.Rel(root, current)
			if err != nil {
				return false, err
			}
			relative = filepath.ToSlash(relative)
			keepGoing, err := visit(walkEntry{absolute: current, relative: relative, isDir: isDirectory})
			if err != nil || !keepGoing {
				return false, err
			}
			if isDirectory {
				// Rules and repository boundaries inside a directory affect only
				// its descendants. Delay loading them until the visitor actually
				// elects to recurse so a result limit stops all further I/O. The
				// checkpoint also gives every directory a lexical ignore scope:
				// rules are discarded as soon as recursion returns, before a
				// sibling is visited.
				checkpoint := matcher.checkpoint()
				nestedBoundary := false
				if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
					nestedBoundary = true
				} else if !errors.Is(err, fs.ErrNotExist) {
					return false, fmt.Errorf("inspect git boundary: %w", err)
				}
				if err := matcher.pushDirectory(ctx, current, nestedBoundary); err != nil {
					matcher.restore(checkpoint)
					return false, err
				}
				keepGoing, err = walkDirectory(current)
				matcher.restore(checkpoint)
				if err != nil || !keepGoing {
					return false, err
				}
			}
		}
		return true, nil
	}
	_, err = walkDirectory(root)
	return err
}

func (s *FilesystemSuite) searchResult(lines []string, reached bool, limit int, kind string) ToolResult {
	raw := strings.Join(lines, "\n")
	maxLines := int(^uint(0) >> 1)
	truncation := truncateFilesystemHead(raw, maxLines, s.maxBytes)
	output := truncation.Content
	details := map[string]any{}
	if reached {
		key := "resultLimitReached"
		notice := fmt.Sprintf("%d results limit reached. Use limit=%d for more, or refine pattern", limit, limit*2)
		if kind == "entries" {
			key = "entryLimitReached"
			notice = fmt.Sprintf("%d entries limit reached. Use limit=%d for more", limit, limit*2)
		}
		details[key] = limit
		output += "\n\n[" + notice + "]"
	}
	if truncation.Truncated {
		details["truncation"] = truncationDetails(truncation, maxLines, s.maxBytes)
		output += fmt.Sprintf("\n\n[%s limit reached.]", formatSize(s.maxBytes))
	}
	if len(details) == 0 {
		details = nil
	}
	return ToolResult{Text: output, Details: details}
}

func (s *FilesystemSuite) grepResult(result *grepOutput, reached bool, limit int, linesTruncated bool) ToolResult {
	truncation := result.truncation()
	output := truncation.Content
	details := map[string]any{}
	var notices []string
	if reached {
		details["matchLimitReached"] = limit
		notices = append(notices, fmt.Sprintf("%d matches limit reached. Use limit=%d for more, or refine pattern", limit, limit*2))
	}
	if truncation.Truncated {
		details["truncation"] = map[string]any{
			"content": truncation.Content, "truncated": truncation.Truncated, "truncatedBy": truncation.TruncatedBy,
			"totalLines": truncation.TotalLines, "totalBytes": truncation.TotalBytes,
			"outputLines": truncation.OutputLines, "outputBytes": truncation.OutputBytes,
			"maxLines": int(^uint(0) >> 1), "maxBytes": s.maxBytes,
			"lastLinePartial": false, "firstLineExceedsLimit": truncation.FirstLineLarge,
		}
		notices = append(notices, fmt.Sprintf("%s limit reached", formatSize(s.maxBytes)))
	}
	if linesTruncated {
		details["linesTruncated"] = true
		notices = append(notices, fmt.Sprintf("Some lines truncated to %d chars. Use read tool to see full lines", DefaultGrepLineRunes))
	}
	if len(notices) > 0 {
		output += "\n\n[" + strings.Join(notices, ". ") + "]"
	}
	if len(details) == 0 {
		details = nil
	}
	return ToolResult{Text: output, Details: details}
}

// compileGlob supports Pi's documented *, ?, ** and character-class patterns
// with slash-aware matching. Patterns are anchored to the search root.
func compileGlob(pattern string) (func(string) bool, error) {
	expression, err := globRegexpExpression(pattern)
	if err != nil {
		return nil, err
	}
	compiled, err := regexp.Compile(expression)
	if err != nil {
		return nil, err
	}
	return compiled.MatchString, nil
}

func globRegexpExpression(pattern string) (string, error) {
	if pattern == "" {
		return "", fmt.Errorf("empty pattern")
	}
	runes := []rune(pattern)
	var expression strings.Builder
	expression.WriteByte('^')
	for index := 0; index < len(runes); index++ {
		character := runes[index]
		switch character {
		case '*':
			if index+1 < len(runes) && runes[index+1] == '*' {
				index++
				for index+1 < len(runes) && runes[index+1] == '*' {
					index++
				}
				if index+1 < len(runes) && runes[index+1] == '/' {
					index++
					expression.WriteString("(?:.*/)?")
				} else {
					expression.WriteString(".*")
				}
			} else {
				expression.WriteString("[^/]*")
			}
		case '?':
			expression.WriteString("[^/]")
		case '[':
			end := index + 1
			for end < len(runes) && runes[end] != ']' {
				if runes[end] == '\\' && end+1 < len(runes) {
					end++
				}
				end++
			}
			if end == len(runes) {
				return "", fmt.Errorf("unterminated character class")
			}
			class := string(runes[index+1 : end])
			if class == "" {
				return "", fmt.Errorf("empty character class")
			}
			if strings.HasPrefix(class, "!") || strings.HasPrefix(class, "^") {
				class = "^" + class[1:]
			}
			expression.WriteString("[")
			expression.WriteString(class)
			expression.WriteString("]")
			index = end
		case '\\':
			if index+1 >= len(runes) {
				return "", fmt.Errorf("trailing escape")
			}
			index++
			expression.WriteString(regexp.QuoteMeta(string(runes[index])))
		default:
			expression.WriteString(regexp.QuoteMeta(string(character)))
		}
	}
	expression.WriteByte('$')
	return expression.String(), nil
}

// compileGrepGlob implements ripgrep's --glob surface without changing Find's
// fd-style pattern semantics. An unescaped leading ! excludes matching paths,
// while brace alternatives are matched as a union. Find continues to treat !
// as an ordinary pattern character.
func compileGrepGlob(pattern string) (func(string) bool, error) {
	if pattern == "" {
		return nil, fmt.Errorf("empty pattern")
	}
	exclude := strings.HasPrefix(pattern, "!")
	if exclude {
		pattern = pattern[1:]
		if pattern == "" {
			return nil, fmt.Errorf("empty exclusion pattern")
		}
	}
	matcher, err := compileBraceGlob(pattern)
	if err != nil {
		return nil, err
	}
	if !exclude {
		return matcher, nil
	}
	return func(candidate string) bool { return !matcher(candidate) }, nil
}

// fd also accepts brace alternatives, but unlike ripgrep an unescaped leading
// ! is not an exclusion operator.
func compileFindGlob(pattern string) (func(string) bool, error) {
	return compileBraceGlob(pattern)
}

func compileBraceGlob(pattern string) (func(string) bool, error) {
	alternatives, err := expandGlobBraces(pattern, 256)
	if err != nil {
		return nil, err
	}
	matchers := make([]func(string) bool, 0, len(alternatives))
	for _, alternative := range alternatives {
		matcher, err := compileGlob(alternative)
		if err != nil {
			return nil, err
		}
		matchers = append(matchers, matcher)
	}
	return func(candidate string) bool {
		for _, matcher := range matchers {
			if matcher(candidate) {
				return true
			}
		}
		return false
	}, nil
}

func expandGlobBraces(pattern string, maximum int) ([]string, error) {
	open, close, commas, err := firstBraceAlternative(pattern)
	if err != nil {
		return nil, err
	}
	if open < 0 {
		return []string{pattern}, nil
	}
	parts := make([]string, 0, len(commas)+1)
	start := open + 1
	for _, comma := range append(commas, close) {
		parts = append(parts, pattern[start:comma])
		start = comma + 1
	}
	if len(parts) > maximum {
		return nil, fmt.Errorf("too many brace alternatives")
	}
	var expanded []string
	for _, part := range parts {
		values, err := expandGlobBraces(pattern[:open]+part+pattern[close+1:], maximum-len(expanded))
		if err != nil {
			return nil, err
		}
		expanded = append(expanded, values...)
		if len(expanded) > maximum {
			return nil, fmt.Errorf("too many brace alternatives")
		}
	}
	return expanded, nil
}

func firstBraceAlternative(pattern string) (int, int, []int, error) {
	depth, open := 0, -1
	inClass, escaped := false, false
	var commas []int
	for index := 0; index < len(pattern); index++ {
		character := pattern[index]
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		if inClass {
			if character == ']' {
				inClass = false
			}
			continue
		}
		if character == '[' {
			inClass = true
			continue
		}
		switch character {
		case '{':
			if depth == 0 {
				open = index
				commas = nil
			}
			depth++
		case ',':
			if depth == 1 {
				commas = append(commas, index)
			}
		case '}':
			if depth == 0 {
				return -1, -1, nil, fmt.Errorf("unmatched closing brace")
			}
			depth--
			if depth == 0 {
				if len(commas) == 0 {
					// A brace pair without a comma is literal. Keep scanning
					// for a later alternative group.
					open = -1
					continue
				}
				return open, index, commas, nil
			}
		}
	}
	if escaped {
		return -1, -1, nil, fmt.Errorf("trailing escape")
	}
	if depth != 0 {
		return -1, -1, nil, fmt.Errorf("unterminated brace alternative")
	}
	return -1, -1, nil, nil
}

// ignoreMatcher keeps only rule frames on the active recursion branch. This
// mirrors the lexical scope of hierarchical ignore files: a/.gitignore is
// pushed before walking a and popped before walking its sibling b. A nested
// repository-scoped traversal advances vcsBoundaryFloor so parent .gitignore
// rules remain allocated for the caller but cannot affect the nested traversal.
// fd's non-repository --no-require-git mode instead keeps VCS rules global
// across nested repositories. Generic .ignore/.fdignore/.rgignore rules always
// cross VCS boundaries, matching the pinned fd/ripgrep behavior.
type ignoreMatcher struct {
	frames           []ignoreRuleFrame
	vcsBoundaryFloor int
	vcsScope         vcsIgnoreScope
	ignoreFiles      []string
}

type ignoreMatcherCheckpoint struct {
	frameCount, vcsBoundaryFloor int
	vcsScope                     vcsIgnoreScope
}

type ignoreRuleFrame struct {
	base                  string
	nextOrder             uint64
	rules, versionControl compactIgnoreRules
	ruleCount             int
}

type ignoreDecision struct {
	order   uint64
	ignored bool
}

type ignoreDecisionIndex struct {
	allEntries      map[string]ignoreDecision
	directoriesOnly map[string]ignoreDecision
}

type compactIgnoreRules struct {
	exactBasename  ignoreDecisionIndex
	exactPath      ignoreDecisionIndex
	prefixBasename ignoreDecisionIndex
	suffixBasename ignoreDecisionIndex
	basenameGlobs  ignoreGlobIndex
	pathGlobs      ignoreGlobIndex
	count          int
}

type ignoreGlobRule struct {
	pattern, requiredLiteral string
	decision                 ignoreDecision
	directoryOnly            bool
}

// ignoreGlobIndex keeps complex patterns out of the hot literal path. Every
// complex glob is indexed by its longest literal fragment that every match
// must contain. Candidate substrings select only the corresponding buckets;
// matching cost is independent of unrelated rule count. Selected patterns are
// compiled on demand, and compiled matchers are shared by equal patterns.
// Wildcard/class-only forms have no sound literal key and stay in the fallback
// slice; there is no hidden rule-count cap.
type ignoreGlobIndex struct {
	rules                []ignoreGlobRule
	byLiteral            map[string][]int
	literalLengths       map[int]struct{}
	sortedLiteralLengths []int
	literalLengthsDirty  bool
	fallback             []int
	compiled             map[string]func(string) bool
}

type ignorePatternClass uint8

const (
	ignorePatternExact ignorePatternClass = iota
	ignorePatternPrefix
	ignorePatternSuffix
	ignorePatternGlob
)

func newIgnoreMatcher(
	ctx context.Context,
	root string,
	ignoreFiles []string,
	outsideRepositoryVCS vcsIgnoreScope,
) (*ignoreMatcher, error) {
	if err := contextFailure(ctx); err != nil {
		return nil, err
	}
	matcher := &ignoreMatcher{ignoreFiles: append([]string(nil), ignoreFiles...)}
	repositoryRoot, insideRepository, err := gitRepositoryRoot(ctx, root)
	if err != nil {
		return nil, err
	}
	// fd uses --no-require-git when Find starts outside a repository, while
	// ripgrep does not. Inside a repository both tools begin VCS ignore scope at
	// the nearest Git root. Generic and tool-specific ignore files always begin
	// at the filesystem ancestor chain and cross repository boundaries.
	matcher.vcsScope = vcsIgnoreDisabled
	if !insideRepository {
		matcher.vcsScope = outsideRepositoryVCS
	}
	for _, directory := range ancestorDirectoryChain(root) {
		boundary := insideRepository && filepath.Clean(directory) == repositoryRoot
		if err := matcher.pushDirectory(ctx, directory, boundary); err != nil {
			return nil, err
		}
	}
	return matcher, nil
}

func gitRepositoryRoot(ctx context.Context, root string) (string, bool, error) {
	root = filepath.Clean(root)
	for current := root; ; current = filepath.Dir(current) {
		if err := contextFailure(ctx); err != nil {
			return "", false, err
		}
		_, err := os.Lstat(filepath.Join(current, ".git"))
		if err == nil {
			return current, true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", false, fmt.Errorf("inspect git boundary: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false, nil
		}
	}
}

func ancestorDirectoryChain(last string) []string {
	first := filepath.Clean(last)
	for {
		parent := filepath.Dir(first)
		if parent == first {
			break
		}
		first = parent
	}
	return directoryChain(first, last)
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

func (m *ignoreMatcher) checkpoint() ignoreMatcherCheckpoint {
	return ignoreMatcherCheckpoint{
		frameCount: len(m.frames), vcsBoundaryFloor: m.vcsBoundaryFloor, vcsScope: m.vcsScope,
	}
}

func (m *ignoreMatcher) restore(checkpoint ignoreMatcherCheckpoint) {
	clear(m.frames[checkpoint.frameCount:])
	m.frames = m.frames[:checkpoint.frameCount]
	m.vcsBoundaryFloor = checkpoint.vcsBoundaryFloor
	m.vcsScope = checkpoint.vcsScope
}

func (m *ignoreMatcher) pushDirectory(ctx context.Context, directory string, boundary bool) error {
	if err := contextFailure(ctx); err != nil {
		return err
	}
	checkpoint := m.checkpoint()
	if boundary {
		switch m.vcsScope {
		case vcsIgnoreDisabled:
			m.vcsScope = vcsIgnoreRepository
			m.vcsBoundaryFloor = len(m.frames)
		case vcsIgnoreRepository:
			m.vcsBoundaryFloor = len(m.frames)
		case vcsIgnoreGlobal:
			// fd --no-require-git treats ancestor .gitignore rules as
			// global even when descent encounters a nested repository.
		}
	}
	frame := ignoreRuleFrame{base: filepath.Clean(directory)}
	for _, name := range m.ignoreFiles {
		versionControl := name == ".gitignore"
		if versionControl && m.vcsScope == vcsIgnoreDisabled {
			continue
		}
		ignorePath := filepath.Join(directory, name)
		err := streamFiniteRegularSearchFile(ctx, ignorePath, func(reader io.Reader) error {
			return streamIgnoreControlLines(ctx, reader, func(lineNumber int, line string) error {
				return frame.addRule(ctx, ignorePath, lineNumber, line, versionControl)
			})
		})
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			m.restore(checkpoint)
			return fmt.Errorf("read ignore rules %s: %w", ignorePath, err)
		}
	}
	if frame.ruleCount > 0 {
		m.frames = append(m.frames, frame)
	}
	return nil
}

func (f *ignoreRuleFrame) addRule(ctx context.Context, ignorePath string, lineNumber int, line string, versionControl bool) error {
	if err := contextFailure(ctx); err != nil {
		return err
	}
	line = trimGitIgnoreTrailingSpaces(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil
	}
	escapedNegation := strings.HasPrefix(line, `\!`)
	if strings.HasPrefix(line, `\#`) || escapedNegation {
		line = line[1:]
	}
	negate := !escapedNegation && strings.HasPrefix(line, "!")
	if negate {
		line = strings.TrimPrefix(line, "!")
	}
	directoryOnly := strings.HasSuffix(line, "/")
	line = strings.TrimSuffix(line, "/")
	anchored := strings.HasPrefix(line, "/")
	line = strings.TrimPrefix(line, "/")
	if line == "" {
		return fmt.Errorf("%w: %s:%d has an empty rule", ErrInvalidFilesystemInput, ignorePath, lineNumber)
	}
	basename := !anchored && !strings.Contains(line, "/")
	class, key, requiredLiteral, err := classifyIgnorePattern(line, basename)
	if err != nil {
		return fmt.Errorf("%w: invalid ignore pattern at %s:%d: %v", ErrInvalidFilesystemInput, ignorePath, lineNumber, err)
	}
	if err := contextFailure(ctx); err != nil {
		return err
	}
	f.nextOrder++
	decision := ignoreDecision{order: f.nextOrder, ignored: !negate}
	rules := &f.rules
	if versionControl {
		rules = &f.versionControl
	}
	switch class {
	case ignorePatternExact:
		if basename {
			rules.exactBasename.add(key, decision, directoryOnly)
		} else {
			rules.exactPath.add(key, decision, directoryOnly)
		}
	case ignorePatternPrefix:
		rules.prefixBasename.add(key, decision, directoryOnly)
	case ignorePatternSuffix:
		rules.suffixBasename.add(key, decision, directoryOnly)
	case ignorePatternGlob:
		glob := &rules.pathGlobs
		if basename {
			glob = &rules.basenameGlobs
		}
		glob.add(ignoreGlobRule{
			pattern: line, requiredLiteral: requiredLiteral,
			decision: decision, directoryOnly: directoryOnly,
		})
	}
	rules.count++
	f.ruleCount++
	return nil
}

func (i *ignoreDecisionIndex) add(key string, decision ignoreDecision, directoryOnly bool) {
	target := &i.allEntries
	if directoryOnly {
		target = &i.directoriesOnly
	}
	if *target == nil {
		*target = make(map[string]ignoreDecision)
	}
	(*target)[key] = decision
}

func (i *ignoreDecisionIndex) latest(key string, directory bool) (ignoreDecision, bool) {
	decision, found := i.allEntries[key]
	if directory {
		if directoryDecision, ok := i.directoriesOnly[key]; ok && (!found || directoryDecision.order > decision.order) {
			decision, found = directoryDecision, true
		}
	}
	return decision, found
}

func laterIgnoreDecision(current ignoreDecision, found bool, candidate ignoreDecision, matched bool) (ignoreDecision, bool) {
	if matched && (!found || candidate.order > current.order) {
		return candidate, true
	}
	return current, found
}

func latestIgnorePrefix(index *ignoreDecisionIndex, candidate string, directory bool) (ignoreDecision, bool) {
	if len(index.allEntries) == 0 && (!directory || len(index.directoriesOnly) == 0) {
		return ignoreDecision{}, false
	}
	var latest ignoreDecision
	found := false
	// Including zero handles the all-wildcard basename rule without a special
	// allocation or branch. Byte boundaries that split UTF-8 cannot equal a
	// valid stored prefix and are harmless.
	for length := 0; length <= len(candidate); length++ {
		decision, matched := index.latest(candidate[:length], directory)
		latest, found = laterIgnoreDecision(latest, found, decision, matched)
	}
	return latest, found
}

func latestIgnoreSuffix(index *ignoreDecisionIndex, candidate string, directory bool) (ignoreDecision, bool) {
	if len(index.allEntries) == 0 && (!directory || len(index.directoriesOnly) == 0) {
		return ignoreDecision{}, false
	}
	var latest ignoreDecision
	found := false
	for start := 0; start <= len(candidate); start++ {
		decision, matched := index.latest(candidate[start:], directory)
		latest, found = laterIgnoreDecision(latest, found, decision, matched)
	}
	return latest, found
}

func classifyIgnorePattern(pattern string, basename bool) (ignorePatternClass, string, string, error) {
	starStart, starEnd, starGroups := -1, -1, 0
	otherGlob := false
	hasCharacterClass := false
	escaped := false
	inStarRun := false
	for index := 0; index < len(pattern); index++ {
		character := pattern[index]
		if escaped {
			escaped = false
			inStarRun = false
			continue
		}
		if character == '\\' {
			escaped = true
			inStarRun = false
			continue
		}
		switch character {
		case '*':
			if !inStarRun {
				starGroups++
				if starStart < 0 {
					starStart = index
				}
			}
			starEnd = index + 1
			inStarRun = true
		case '?', '[':
			otherGlob = true
			hasCharacterClass = hasCharacterClass || character == '['
			inStarRun = false
		default:
			inStarRun = false
		}
	}
	if escaped {
		return 0, "", "", fmt.Errorf("trailing escape")
	}
	if starGroups == 0 && !otherGlob {
		literal, err := unescapeIgnoreLiteral(pattern)
		return ignorePatternExact, literal, "", err
	}
	// A basename with exactly one leading or trailing star run needs only a
	// prefix/suffix lookup. Path patterns stay in the glob index because a
	// single '*' must not cross a slash while '**' may do so.
	if basename && !otherGlob && starGroups == 1 {
		switch {
		case starStart == 0:
			literal, err := unescapeIgnoreLiteral(pattern[starEnd:])
			return ignorePatternSuffix, compactIgnoreKey(literal, len(pattern)), "", err
		case starEnd == len(pattern):
			literal, err := unescapeIgnoreLiteral(pattern[:starStart])
			return ignorePatternPrefix, compactIgnoreKey(literal, len(pattern)), "", err
		}
	}
	// Validate malformed classes and escapes now without compiling a regexp.
	// The selected complex pattern is compiled only if its literal index makes
	// it a candidate during traversal, then cached by pattern.
	expression, err := globRegexpExpression(pattern)
	if err != nil {
		return 0, "", "", err
	}
	if hasCharacterClass {
		if _, err := syntax.Parse(expression, syntax.Perl); err != nil {
			return 0, "", "", err
		}
	}
	return ignorePatternGlob, "", longestRequiredIgnoreLiteral(pattern), nil
}

func compactIgnoreKey(key string, sourceLength int) string {
	if len(key)*2 < sourceLength {
		return strings.Clone(key)
	}
	return key
}

func unescapeIgnoreLiteral(pattern string) (string, error) {
	if !strings.Contains(pattern, `\`) {
		return pattern, nil
	}
	var literal strings.Builder
	literal.Grow(len(pattern))
	escaped := false
	for _, character := range pattern {
		if escaped {
			literal.WriteRune(character)
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		literal.WriteRune(character)
	}
	if escaped {
		return "", fmt.Errorf("trailing escape")
	}
	return literal.String(), nil
}

func longestRequiredIgnoreLiteral(pattern string) string {
	runes := []rune(pattern)
	var current, longest strings.Builder
	flush := func() {
		if current.Len() >= longest.Len() {
			longest.Reset()
			longest.WriteString(current.String())
		}
		current.Reset()
	}
	for index := 0; index < len(runes); index++ {
		character := runes[index]
		switch character {
		case '\\':
			if index+1 < len(runes) {
				index++
				current.WriteRune(runes[index])
			}
		case '*':
			flush()
			starCount := 1
			for index+1 < len(runes) && runes[index+1] == '*' {
				index++
				starCount++
			}
			// compileGlob makes the slash following a ** run optional as
			// part of (?:.*/)?. It is therefore not a required literal.
			if starCount >= 2 && index+1 < len(runes) && runes[index+1] == '/' {
				index++
			}
		case '?':
			flush()
		case '[':
			flush()
			for index+1 < len(runes) {
				index++
				if runes[index] == '\\' && index+1 < len(runes) {
					index++
					continue
				}
				if runes[index] == ']' {
					break
				}
			}
		default:
			current.WriteRune(character)
		}
	}
	flush()
	return longest.String()
}

func (i *ignoreGlobIndex) add(rule ignoreGlobRule) {
	index := len(i.rules)
	i.rules = append(i.rules, rule)
	if rule.requiredLiteral == "" {
		i.fallback = append(i.fallback, index)
		return
	}
	if i.byLiteral == nil {
		i.byLiteral = make(map[string][]int)
		i.literalLengths = make(map[int]struct{})
	}
	i.byLiteral[rule.requiredLiteral] = append(i.byLiteral[rule.requiredLiteral], index)
	length := len(rule.requiredLiteral)
	if _, exists := i.literalLengths[length]; !exists {
		i.literalLengths[length] = struct{}{}
		i.literalLengthsDirty = true
	}
}

func (i *ignoreGlobIndex) prepareLiteralLengths() {
	if !i.literalLengthsDirty {
		return
	}
	i.sortedLiteralLengths = i.sortedLiteralLengths[:0]
	for length := range i.literalLengths {
		i.sortedLiteralLengths = append(i.sortedLiteralLengths, length)
	}
	sort.Ints(i.sortedLiteralLengths)
	i.literalLengthsDirty = false
}

func (i *ignoreGlobIndex) latest(candidate string, directory bool) (ignoreDecision, bool, error) {
	var latest ignoreDecision
	found := false
	check := func(index int) error {
		rule := &i.rules[index]
		if rule.directoryOnly && !directory {
			return nil
		}
		if rule.requiredLiteral != "" && !strings.Contains(candidate, rule.requiredLiteral) {
			return nil
		}
		if i.compiled == nil {
			i.compiled = make(map[string]func(string) bool)
		}
		match := i.compiled[rule.pattern]
		if match == nil {
			var err error
			match, err = compileGlob(rule.pattern)
			if err != nil {
				return fmt.Errorf("compile validated ignore glob %q: %w", rule.pattern, err)
			}
			i.compiled[rule.pattern] = match
		}
		if match(candidate) {
			latest, found = laterIgnoreDecision(latest, found, rule.decision, true)
		}
		return nil
	}
	for _, index := range i.fallback {
		if err := check(index); err != nil {
			return ignoreDecision{}, false, err
		}
	}
	i.prepareLiteralLengths()
	var selected map[string]struct{}
	for _, length := range i.sortedLiteralLengths {
		if length > len(candidate) {
			break
		}
		for start := 0; start+length <= len(candidate); start++ {
			literal := candidate[start : start+length]
			bucket := i.byLiteral[literal]
			if len(bucket) == 0 {
				continue
			}
			if selected == nil {
				selected = make(map[string]struct{})
			}
			if _, duplicate := selected[literal]; duplicate {
				continue
			}
			selected[literal] = struct{}{}
			for _, ruleIndex := range bucket {
				if err := check(ruleIndex); err != nil {
					return ignoreDecision{}, false, err
				}
			}
		}
	}
	return latest, found, nil
}

func (r *compactIgnoreRules) latest(relative, basename string, directory bool) (ignoreDecision, bool, error) {
	var latest ignoreDecision
	found := false
	merge := func(decision ignoreDecision, matched bool) {
		latest, found = laterIgnoreDecision(latest, found, decision, matched)
	}
	decision, matched := r.exactBasename.latest(basename, directory)
	merge(decision, matched)
	decision, matched = r.exactPath.latest(relative, directory)
	merge(decision, matched)
	decision, matched = latestIgnorePrefix(&r.prefixBasename, basename, directory)
	merge(decision, matched)
	decision, matched = latestIgnoreSuffix(&r.suffixBasename, basename, directory)
	merge(decision, matched)
	decision, matched, err := r.basenameGlobs.latest(basename, directory)
	if err != nil {
		return ignoreDecision{}, false, err
	}
	merge(decision, matched)
	decision, matched, err = r.pathGlobs.latest(relative, directory)
	if err != nil {
		return ignoreDecision{}, false, err
	}
	merge(decision, matched)
	return latest, found, nil
}

// streamIgnoreControlLines deliberately has no product-specific line, rule,
// or aggregate-byte cap. The pinned rg/fd implementations likewise require at
// least one active rule line and the compiled rules to fit in memory. Unlike a
// whole-file read, this parser retains only the current non-comment line;
// arbitrarily large comment lines and trimmed trailing spaces stay O(32 KiB).
func streamIgnoreControlLines(ctx context.Context, reader io.Reader, yield func(int, string) error) error {
	return streamIgnoreControlLinesMeasured(ctx, reader, yield, nil)
}

type ignoreControlStreamStats struct {
	maxRetainedLineBytes int
	yieldedLines         int
}

func streamIgnoreControlLinesMeasured(
	ctx context.Context,
	reader io.Reader,
	yield func(int, string) error,
	stats *ignoreControlStreamStats,
) error {
	buffer := make([]byte, 32*1024)
	line := make([]byte, 0, 256)
	lineNumber := 1
	pendingSpaces := 0
	atLineStart := true
	comment := false
	skipLF := false

	finishLine := func() error {
		if !comment {
			if pendingSpaces > 0 {
				backslashes := 0
				for index := len(line) - 1; index >= 0 && line[index] == '\\'; index-- {
					backslashes++
				}
				if backslashes%2 == 1 {
					line = append(line, ' ')
				}
			}
			if stats != nil {
				stats.maxRetainedLineBytes = maxInt(stats.maxRetainedLineBytes, len(line))
				stats.yieldedLines++
			}
			if err := yield(lineNumber, string(line)); err != nil {
				return err
			}
		}
		line = line[:0]
		lineNumber++
		pendingSpaces = 0
		atLineStart = true
		comment = false
		return nil
	}
	appendPendingSpaces := func() error {
		if pendingSpaces == 0 {
			return nil
		}
		maximum := int(^uint(0) >> 1)
		if pendingSpaces > maximum-len(line) {
			return fmt.Errorf("%w: ignore rule exceeds platform string capacity", ErrInvalidFilesystemInput)
		}
		start := len(line)
		line = append(line, make([]byte, pendingSpaces)...)
		for index := start; index < len(line); index++ {
			line[index] = ' '
		}
		if stats != nil {
			stats.maxRetainedLineBytes = maxInt(stats.maxRetainedLineBytes, len(line))
		}
		pendingSpaces = 0
		return nil
	}

	for {
		if err := contextFailure(ctx); err != nil {
			return err
		}
		count, readErr := reader.Read(buffer)
		for _, value := range buffer[:count] {
			if skipLF {
				skipLF = false
				if value == '\n' {
					continue
				}
			}
			if value == '\n' || value == '\r' {
				if err := finishLine(); err != nil {
					return err
				}
				skipLF = value == '\r'
				continue
			}
			if atLineStart {
				atLineStart = false
				if value == '#' {
					comment = true
					continue
				}
			}
			if comment {
				continue
			}
			if value == ' ' {
				pendingSpaces++
				continue
			}
			if err := appendPendingSpaces(); err != nil {
				return err
			}
			line = append(line, value)
			if stats != nil {
				stats.maxRetainedLineBytes = maxInt(stats.maxRetainedLineBytes, len(line))
			}
		}
		if errors.Is(readErr, io.EOF) {
			if !atLineStart || comment || pendingSpaces > 0 || len(line) > 0 {
				return finishLine()
			}
			return nil
		}
		if readErr != nil {
			if failure := contextFailure(ctx); failure != nil {
				return failure
			}
			return readErr
		}
		if count == 0 {
			return io.ErrNoProgress
		}
	}
}

func trimGitIgnoreTrailingSpaces(line string) string {
	for len(line) > 0 && line[len(line)-1] == ' ' {
		backslashes := 0
		for index := len(line) - 2; index >= 0 && line[index] == '\\'; index-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			break
		}
		line = line[:len(line)-1]
	}
	return line
}

func (m *ignoreMatcher) ignored(absolute string, isDirectory bool) (bool, error) {
	baseName := filepath.Base(absolute)
	if baseName == ".git" {
		return true, nil
	}
	ignored := false
	for index := 0; index < len(m.frames); index++ {
		frame := &m.frames[index]
		relative, err := filepath.Rel(frame.base, absolute)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		relative = filepath.ToSlash(relative)
		decision, matched, err := frame.rules.latest(relative, baseName, isDirectory)
		if err != nil {
			return false, err
		}
		if m.vcsScope != vcsIgnoreDisabled && (m.vcsScope == vcsIgnoreGlobal || index >= m.vcsBoundaryFloor) {
			vcsDecision, vcsMatched, vcsErr := frame.versionControl.latest(relative, baseName, isDirectory)
			if vcsErr != nil {
				return false, vcsErr
			}
			decision, matched = laterIgnoreDecision(decision, matched, vcsDecision, vcsMatched)
		}
		if matched {
			ignored = decision.ignored
		}
	}
	return ignored, nil
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

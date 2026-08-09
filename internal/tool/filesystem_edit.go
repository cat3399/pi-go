package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

func (s *FilesystemSuite) Edit(ctx context.Context, input EditInput) (ToolResult, error) {
	if err := contextFailure(ctx); err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
	}
	if err := validFilesystemArgument("path", input.Path); err != nil {
		return inputError(err)
	}
	if len(input.Edits) == 0 {
		return inputError(fmt.Errorf("%w: edits must contain at least one replacement", ErrInvalidFilesystemInput))
	}
	for index, edit := range input.Edits {
		if !utf8.ValidString(edit.OldText) || !utf8.ValidString(edit.NewText) {
			return inputError(fmt.Errorf("%w: edits[%d] must be valid UTF-8", ErrInvalidFilesystemInput, index))
		}
	}
	path, err := resolveToolPath(s.workingDir, input.Path)
	if err != nil {
		return ToolResult{}, err
	}
	key, err := mutationKey(path)
	if err != nil {
		return ToolResult{}, err
	}
	var outcome ToolResult
	err = s.mutations.with(ctx, key, func() error {
		currentKey, err := mutationKey(path)
		if err != nil {
			return err
		}
		if filepath.Clean(currentKey) != filepath.Clean(key) {
			return fmt.Errorf("%w: edit target changed before read", ErrFilesystemPath)
		}
		readPath, err := resolveMutationDestination(path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(readPath)
		if err != nil {
			return fmt.Errorf("could not edit file %s: %w", input.Path, err)
		}
		if err := context.Cause(ctx); err != nil {
			return errors.Join(ErrOperationCancelled, err)
		}
		before := strings.ToValidUTF8(string(data), "�")
		after, baseContent, newContent, edits, _, err := applyEditsDetailed(before, input.Edits, input.Path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(after), 0o666); err != nil {
			return fmt.Errorf("write edited file: %w", err)
		}
		if err := context.Cause(ctx); err != nil {
			return errors.Join(ErrOperationCancelled, err)
		}
		diff, patch, firstChanged := makeEditDiff(input.Path, baseContent, newContent)
		outcome = ToolResult{Text: fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(edits), input.Path), Details: map[string]any{"diff": diff, "patch": patch, "firstChangedLine": firstChanged}}
		return nil
	})
	if err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
	}
	return outcome, nil
}

type resolvedEdit struct {
	index, start, length int
	replacement          string
}

func applyEdits(original string, edits []Edit, path string) (string, []resolvedEdit, bool, error) {
	result, _, _, resolved, fuzzy, err := applyEditsDetailed(original, edits, path)
	return result, resolved, fuzzy, err
}

func applyEditsDetailed(original string, edits []Edit, path string) (string, string, string, []resolvedEdit, bool, error) {
	bom, content := "", original
	if strings.HasPrefix(content, "\ufeff") {
		bom, content = "\ufeff", strings.TrimPrefix(content, "\ufeff")
	}
	ending := detectLineEnding(content)
	normalized := normalizeLF(content)
	normalizedEdits := make([]Edit, len(edits))
	for index, edit := range edits {
		normalizedEdits[index] = Edit{OldText: normalizeLF(edit.OldText), NewText: normalizeLF(edit.NewText)}
		if normalizedEdits[index].OldText == "" {
			return "", "", "", nil, false, emptyOldTextError(path, index, len(edits))
		}
	}

	usedFuzzy := false
	for _, edit := range normalizedEdits {
		_, _, fuzzy, found := fuzzyFindText(normalized, edit.OldText)
		usedFuzzy = usedFuzzy || found && fuzzy
	}
	replacementBase := normalized
	if usedFuzzy {
		replacementBase = fuzzyNormalize(normalized)
	}

	matched := make([]resolvedEdit, 0, len(edits))
	for index, edit := range normalizedEdits {
		start, length, _, found := fuzzyFindText(replacementBase, edit.OldText)
		if !found {
			return "", "", "", nil, false, notFoundEditError(path, index, len(edits))
		}
		occurrences := countFuzzyOccurrences(replacementBase, edit.OldText)
		if occurrences > 1 {
			return "", "", "", nil, false, duplicateEditError(path, index, len(edits), occurrences)
		}
		matched = append(matched, resolvedEdit{index: index, start: start, length: length, replacement: edit.NewText})
	}
	sort.Slice(matched, func(left, right int) bool { return matched[left].start < matched[right].start })
	for index := 1; index < len(matched); index++ {
		previous, current := matched[index-1], matched[index]
		if previous.start+previous.length > current.start {
			return "", "", "", nil, false, fmt.Errorf("%w: edits[%d] and edits[%d] overlap in %s. Merge them into one edit or target disjoint regions", ErrEditConflict, previous.index, current.index, path)
		}
	}
	newContent := applyResolvedEdits(replacementBase, matched, 0)
	if usedFuzzy {
		var err error
		newContent, err = applyEditsPreservingUnchangedLines(normalized, replacementBase, matched)
		if err != nil {
			return "", "", "", nil, false, err
		}
	}
	if newContent == normalized {
		if len(edits) == 1 {
			return "", "", "", nil, false, fmt.Errorf("%w: no changes made to %s. The replacement produced identical content. This might indicate an issue with special characters or the text not existing as expected", ErrEditConflict, path)
		}
		return "", "", "", nil, false, fmt.Errorf("%w: no changes made to %s. The replacements produced identical content", ErrEditConflict, path)
	}
	return bom + restoreLineEnding(newContent, ending), normalized, newContent, matched, usedFuzzy, nil
}

func normalizeLF(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
}
func detectLineEnding(value string) string {
	crlf := strings.Index(value, "\r\n")
	lf := strings.IndexByte(value, '\n')
	if lf >= 0 && crlf >= 0 && crlf < lf {
		return "\r\n"
	}
	return "\n"
}
func restoreLineEnding(value, ending string) string {
	if ending == "\r\n" {
		return strings.ReplaceAll(value, "\n", "\r\n")
	}
	return value
}

func fuzzyFindText(content, target string) (start, length int, fuzzy, found bool) {
	if index := strings.Index(content, target); index >= 0 {
		return index, len(target), false, true
	}
	normalizedContent, normalizedTarget := fuzzyNormalize(content), fuzzyNormalize(target)
	if index := strings.Index(normalizedContent, normalizedTarget); index >= 0 {
		return index, len(normalizedTarget), true, true
	}
	return -1, 0, false, false
}

func allOccurrences(content, target string) []int {
	if target == "" {
		return nil
	}
	var positions []int
	for from := 0; ; {
		index := strings.Index(content[from:], target)
		if index < 0 {
			return positions
		}
		index += from
		positions = append(positions, index)
		from = index + len(target)
	}
}

func fuzzyNormalize(value string) string {
	value = norm.NFKC.String(value)
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimRightFunc(line, isJavaScriptTrimWhitespace)
	}
	value = strings.Join(lines, "\n")
	replacer := strings.NewReplacer(
		"‘", "'", "’", "'", "‚", "'", "‛", "'", "“", "\"", "”", "\"", "„", "\"", "‟", "\"",
		"‐", "-", "‑", "-", "‒", "-", "–", "-", "—", "-", "―", "-", "−", "-",
		"\u00a0", " ", "\u2002", " ", "\u2003", " ", "\u2004", " ", "\u2005", " ", "\u2006", " ",
		"\u2007", " ", "\u2008", " ", "\u2009", " ", "\u200a", " ", "\u202f", " ", "\u205f", " ", "\u3000", " ",
	)
	return replacer.Replace(value)
}

func isJavaScriptTrimWhitespace(r rune) bool {
	switch r {
	case '\u0009', '\u000a', '\u000b', '\u000c', '\u000d', '\u0020', '\u00a0', '\u1680',
		'\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	default:
		return r >= '\u2000' && r <= '\u200a'
	}
}

func countFuzzyOccurrences(content, target string) int {
	return len(allOccurrences(fuzzyNormalize(content), fuzzyNormalize(target)))
}

func notFoundEditError(path string, index, total int) error {
	if total == 1 {
		return fmt.Errorf("%w: could not find the exact text in %s. The old text must match exactly including all whitespace and newlines", ErrEditConflict, path)
	}
	return fmt.Errorf("%w: could not find edits[%d] in %s. The oldText must match exactly including all whitespace and newlines", ErrEditConflict, index, path)
}

func duplicateEditError(path string, index, total, occurrences int) error {
	if total == 1 {
		return fmt.Errorf("%w: found %d occurrences of the text in %s. The text must be unique. Please provide more context to make it unique", ErrEditConflict, occurrences, path)
	}
	return fmt.Errorf("%w: found %d occurrences of edits[%d] in %s. Each oldText must be unique. Please provide more context to make it unique", ErrEditConflict, occurrences, index, path)
}

func emptyOldTextError(path string, index, total int) error {
	if total == 1 {
		return fmt.Errorf("%w: oldText must not be empty in %s", ErrEditConflict, path)
	}
	return fmt.Errorf("%w: edits[%d].oldText must not be empty in %s", ErrEditConflict, index, path)
}

func applyResolvedEdits(content string, edits []resolvedEdit, offset int) string {
	result := content
	for index := len(edits) - 1; index >= 0; index-- {
		edit := edits[index]
		start := edit.start - offset
		result = result[:start] + edit.replacement + result[start+edit.length:]
	}
	return result
}

type editLineSpan struct{ start, end int }

func splitLinesWithEndings(content string) []string {
	if content == "" {
		return nil
	}
	var lines []string
	for len(content) > 0 {
		index := strings.IndexByte(content, '\n')
		if index < 0 {
			return append(lines, content)
		}
		lines = append(lines, content[:index+1])
		content = content[index+1:]
	}
	return lines
}

func editLineSpans(content string) []editLineSpan {
	lines := splitLinesWithEndings(content)
	spans := make([]editLineSpan, len(lines))
	offset := 0
	for index, line := range lines {
		spans[index] = editLineSpan{start: offset, end: offset + len(line)}
		offset += len(line)
	}
	return spans
}

func replacementLineRange(lines []editLineSpan, edit resolvedEdit) (int, int, error) {
	startLine := -1
	for index, line := range lines {
		if edit.start >= line.start && edit.start < line.end {
			startLine = index
			break
		}
	}
	if startLine < 0 {
		return 0, 0, fmt.Errorf("%w: replacement range is outside the base content", ErrEditConflict)
	}
	endOffset := edit.start + edit.length
	endLine := startLine
	for endLine < len(lines) && lines[endLine].end < endOffset {
		endLine++
	}
	if endLine >= len(lines) {
		return 0, 0, fmt.Errorf("%w: replacement range is outside the base content", ErrEditConflict)
	}
	return startLine, endLine + 1, nil
}

func applyEditsPreservingUnchangedLines(original, base string, edits []resolvedEdit) (string, error) {
	originalLines := splitLinesWithEndings(original)
	baseLines := editLineSpans(base)
	if len(originalLines) != len(baseLines) {
		return "", fmt.Errorf("%w: normalized content changed line count", ErrEditConflict)
	}
	type group struct {
		startLine, endLine int
		edits              []resolvedEdit
	}
	groups := make([]group, 0, len(edits))
	for _, edit := range edits {
		startLine, endLine, err := replacementLineRange(baseLines, edit)
		if err != nil {
			return "", err
		}
		if len(groups) > 0 && startLine < groups[len(groups)-1].endLine {
			current := &groups[len(groups)-1]
			current.endLine = maxInt(current.endLine, endLine)
			current.edits = append(current.edits, edit)
		} else {
			groups = append(groups, group{startLine: startLine, endLine: endLine, edits: []resolvedEdit{edit}})
		}
	}
	var out strings.Builder
	originalIndex := 0
	for _, group := range groups {
		for _, line := range originalLines[originalIndex:group.startLine] {
			out.WriteString(line)
		}
		startOffset := baseLines[group.startLine].start
		endOffset := baseLines[group.endLine-1].end
		out.WriteString(applyResolvedEdits(base[startOffset:endOffset], group.edits, startOffset))
		originalIndex = group.endLine
	}
	for _, line := range originalLines[originalIndex:] {
		out.WriteString(line)
	}
	return out.String(), nil
}

func makeEditDiff(path, before, after string) (display, patch string, firstChangedLine int) {
	operations := lineDiff(splitDiffLines(before), splitDiffLines(after))
	hunks := diffHunks(operations, 4)
	if len(hunks) == 0 {
		return "", fmt.Sprintf("--- %s\n+++ %s\n", path, path), 0
	}
	for index, operation := range operations {
		if operation.kind != ' ' {
			firstChangedLine = hunkLineStarts(operations, index).new
			break
		}
	}
	var patchBuilder strings.Builder
	fmt.Fprintf(&patchBuilder, "--- %s\n+++ %s\n", path, path)
	var displayLines []string
	for hunkIndex, hunk := range hunks {
		starts := hunkLineStarts(operations, hunk.first)
		oldCount, newCount := hunkCounts(operations[hunk.first:hunk.last])
		oldStart, newStart := starts.old, starts.new
		if oldCount == 0 {
			oldStart--
		}
		if newCount == 0 {
			newStart--
		}
		fmt.Fprintf(&patchBuilder, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		if hunkIndex > 0 {
			displayLines = append(displayLines, " ...")
		}
		oldLine, newLine := starts.old, starts.new
		for _, operation := range operations[hunk.first:hunk.last] {
			writePatchLine(&patchBuilder, operation.kind, operation.text)
			displayText := strings.TrimSuffix(operation.text, "\n")
			switch operation.kind {
			case '-':
				displayLines = append(displayLines, fmt.Sprintf("-%d %s", oldLine, displayText))
				oldLine++
			case '+':
				displayLines = append(displayLines, fmt.Sprintf("+%d %s", newLine, displayText))
				newLine++
			default:
				displayLines = append(displayLines, fmt.Sprintf(" %d %s", newLine, displayText))
				oldLine++
				newLine++
			}
		}
	}
	return strings.Join(displayLines, "\n"), patchBuilder.String(), firstChangedLine
}

type lineDiffOperation struct {
	kind byte
	text string
}

type diffHunk struct{ first, last int }

func splitDiffLines(value string) []string {
	if value == "" {
		return nil
	}
	lines := make([]string, 0, strings.Count(value, "\n")+1)
	for len(value) > 0 {
		newline := strings.IndexByte(value, '\n')
		if newline < 0 {
			lines = append(lines, value)
			break
		}
		lines = append(lines, value[:newline+1])
		value = value[newline+1:]
	}
	return lines
}

// lineDiff emits a deterministic transformation. At each mismatch it chooses
// the nearest common line as a resynchronization anchor; the result need not be
// the mathematically shortest diff, but every operation is exact and yields an
// applicable patch without quadratic memory on large source files.
func lineDiff(before, after []string) []lineDiffOperation {
	operations := make([]lineDiffOperation, 0, len(before)+len(after))
	for oldIndex, newIndex := 0, 0; oldIndex < len(before) || newIndex < len(after); {
		if oldIndex < len(before) && newIndex < len(after) && before[oldIndex] == after[newIndex] {
			operations = append(operations, lineDiffOperation{kind: ' ', text: before[oldIndex]})
			oldIndex++
			newIndex++
			continue
		}
		anchorOld, anchorNew := nearestLineAnchor(before, after, oldIndex, newIndex)
		if anchorOld < 0 {
			for ; oldIndex < len(before); oldIndex++ {
				operations = append(operations, lineDiffOperation{kind: '-', text: before[oldIndex]})
			}
			for ; newIndex < len(after); newIndex++ {
				operations = append(operations, lineDiffOperation{kind: '+', text: after[newIndex]})
			}
			break
		}
		for ; oldIndex < anchorOld; oldIndex++ {
			operations = append(operations, lineDiffOperation{kind: '-', text: before[oldIndex]})
		}
		for ; newIndex < anchorNew; newIndex++ {
			operations = append(operations, lineDiffOperation{kind: '+', text: after[newIndex]})
		}
	}
	return operations
}

func nearestLineAnchor(before, after []string, oldStart, newStart int) (int, int) {
	afterPositions := make(map[string]int, len(after)-newStart)
	for index := newStart; index < len(after); index++ {
		if _, exists := afterPositions[after[index]]; !exists {
			afterPositions[after[index]] = index
		}
	}
	bestOld, bestNew, bestDistance := -1, -1, int(^uint(0)>>1)
	for oldIndex := oldStart; oldIndex < len(before); oldIndex++ {
		newIndex, exists := afterPositions[before[oldIndex]]
		if !exists {
			continue
		}
		distance := oldIndex - oldStart + newIndex - newStart
		if distance < bestDistance {
			bestOld, bestNew, bestDistance = oldIndex, newIndex, distance
		}
		if distance == 0 {
			break
		}
	}
	return bestOld, bestNew
}

func diffHunks(operations []lineDiffOperation, contextLines int) []diffHunk {
	var changed []int
	for index, operation := range operations {
		if operation.kind != ' ' {
			changed = append(changed, index)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	start := maxInt(0, changed[0]-contextLines)
	lastChange := changed[0]
	var hunks []diffHunk
	for _, change := range changed[1:] {
		if change-lastChange > contextLines*2+1 {
			hunks = append(hunks, diffHunk{first: start, last: minInt(len(operations), lastChange+contextLines+1)})
			start = maxInt(0, change-contextLines)
		}
		lastChange = change
	}
	hunks = append(hunks, diffHunk{first: start, last: minInt(len(operations), lastChange+contextLines+1)})
	return hunks
}

type lineStarts struct{ old, new int }

func hunkLineStarts(operations []lineDiffOperation, limit int) lineStarts {
	starts := lineStarts{old: 1, new: 1}
	for _, operation := range operations[:limit] {
		if operation.kind != '+' {
			starts.old++
		}
		if operation.kind != '-' {
			starts.new++
		}
	}
	return starts
}

func hunkCounts(operations []lineDiffOperation) (old, new int) {
	for _, operation := range operations {
		if operation.kind != '+' {
			old++
		}
		if operation.kind != '-' {
			new++
		}
	}
	return old, new
}

func writePatchLine(builder *strings.Builder, kind byte, text string) {
	builder.WriteByte(kind)
	builder.WriteString(text)
	if strings.HasSuffix(text, "\n") {
		return
	}
	builder.WriteString("\n\\ No newline at end of file\n")
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

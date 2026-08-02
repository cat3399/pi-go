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
)

func (s *FilesystemSuite) Edit(ctx context.Context, input EditInput) (ToolResult, error) {
	if err := contextFailure(ctx); err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
	}
	if err := validText("path", input.Path); err != nil {
		return inputError(err)
	}
	if len(input.Edits) == 0 {
		return inputError(fmt.Errorf("%w: edits must contain at least one replacement", ErrInvalidFilesystemInput))
	}
	for index, edit := range input.Edits {
		if !utf8.ValidString(edit.OldText) || !utf8.ValidString(edit.NewText) {
			return inputError(fmt.Errorf("%w: edits[%d] must be valid UTF-8", ErrInvalidFilesystemInput, index))
		}
		if edit.OldText == "" {
			return inputError(fmt.Errorf("%w: edits[%d].oldText must not be empty", ErrInvalidFilesystemInput, index))
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
		if isBinary(data) || !utf8.Valid(data) {
			return fmt.Errorf("%w: %s", ErrBinaryFile, input.Path)
		}
		if err := context.Cause(ctx); err != nil {
			return errors.Join(ErrOperationCancelled, err)
		}
		before := string(data)
		after, edits, fuzzy, err := applyEdits(before, input.Edits, input.Path)
		if err != nil {
			return err
		}
		if err := atomicWrite(ctx, path, key, []byte(after)); err != nil {
			return err
		}
		diff, patch, firstChanged := makeEditDiff(input.Path, before, after)
		outcome = ToolResult{Text: fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(edits), input.Path), Details: map[string]any{"diff": diff, "patch": patch, "firstChangedLine": firstChanged, "fuzzyMatched": fuzzy}}
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
	bom, content := "", original
	if strings.HasPrefix(content, "\ufeff") {
		bom, content = "\ufeff", strings.TrimPrefix(content, "\ufeff")
	}
	ending := detectLineEnding(content)
	normalized := normalizeLF(content)
	matched := make([]resolvedEdit, 0, len(edits))
	usedFuzzy := false
	for index, edit := range edits {
		oldText := normalizeLF(edit.OldText)
		newText := normalizeLF(edit.NewText)
		start, length, fuzzy, occurrences := findUnique(normalized, oldText)
		if occurrences == 0 && requiresCompatibilityNormalization(normalized, oldText) {
			return "", nil, false, fmt.Errorf("%w: NFKC fuzzy matching is not supported; provide exact text for %s", ErrUnsupportedFilesystemFeature, path)
		}
		if occurrences == 0 {
			return "", nil, false, fmt.Errorf("%w: could not find the exact text in %s", ErrEditConflict, path)
		}
		if occurrences > 1 {
			return "", nil, false, fmt.Errorf("%w: found %d occurrences of edits[%d] in %s; oldText must be unique", ErrEditConflict, occurrences, index, path)
		}
		matched = append(matched, resolvedEdit{index: index, start: start, length: length, replacement: newText})
		usedFuzzy = usedFuzzy || fuzzy
	}
	sort.Slice(matched, func(left, right int) bool { return matched[left].start < matched[right].start })
	for index := 1; index < len(matched); index++ {
		previous, current := matched[index-1], matched[index]
		if previous.start+previous.length > current.start {
			return "", nil, false, fmt.Errorf("%w: edits[%d] and edits[%d] overlap in %s", ErrEditConflict, previous.index, current.index, path)
		}
	}
	result := normalized
	for index := len(matched) - 1; index >= 0; index-- {
		edit := matched[index]
		result = result[:edit.start] + edit.replacement + result[edit.start+edit.length:]
	}
	if result == normalized {
		return "", nil, false, fmt.Errorf("%w: no changes made to %s", ErrEditConflict, path)
	}
	return bom + restoreLineEnding(result, ending), matched, usedFuzzy, nil
}

func normalizeLF(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
}
func detectLineEnding(value string) string {
	if strings.Index(value, "\r\n") >= 0 {
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

func findUnique(content, target string) (start, length int, fuzzy bool, occurrences int) {
	positions := allOccurrences(content, target)
	if len(positions) == 1 {
		return positions[0], len(target), false, 1
	}
	if len(positions) > 1 {
		return -1, 0, false, len(positions)
	}
	// The upstream tool deliberately uses fuzzy retry for model-produced text.
	// We preserve its safe subset (trailing whitespace, smart quotes/dashes and
	// Unicode spaces) without changing unrelated lines. NFKC-only matches are
	// explicitly deferred in the ledger because byte-index mapping is not safe.
	normalizedContent := fuzzyNormalize(content)
	normalizedTarget := fuzzyNormalize(target)
	positions = allOccurrences(normalizedContent, normalizedTarget)
	if len(positions) != 1 {
		return -1, 0, true, len(positions)
	}
	start = positions[0]
	// Each supported replacement is one rune -> one rune UTF-8-width may vary.
	// Map through rune prefixes to retain byte ranges in the original content.
	originalStart, originalLength, ok := fuzzyByteRange(content, target, start, len(normalizedTarget))
	if !ok {
		return -1, 0, true, 0
	}
	return originalStart, originalLength, true, 1
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
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimRight(line, " \t\r")
	}
	value = strings.Join(lines, "\n")
	replacer := strings.NewReplacer("‘", "'", "’", "'", "‚", "'", "‛", "'", "“", "\"", "”", "\"", "„", "\"", "‟", "\"", "‐", "-", "‑", "-", "‒", "-", "–", "-", "—", "-", "―", "-", "−", "-", "\u00a0", " ", "\u202f", " ", "\u3000", " ")
	return replacer.Replace(value)
}

func requiresCompatibilityNormalization(values ...string) bool {
	for _, value := range values {
		for _, runeValue := range value {
			// Fullwidth ASCII and combining marks are the common upstream NFKC
			// cases. We reject a fuzzy fallback explicitly rather than applying
			// byte offsets from a lossy normalization.
			if runeValue >= 0xFF01 && runeValue <= 0xFF5E || runeValue >= 0x0300 && runeValue <= 0x036F {
				return true
			}
		}
	}
	return false
}

// fuzzyByteRange maps only transformations with equal rune counts. Trailing
// whitespace is more subtle: determine the original matching span by scanning
// each line from the normalized byte start. Ambiguous mappings fail closed.
func fuzzyByteRange(content, target string, normalizedStart, normalizedLength int) (int, int, bool) {
	normalized := fuzzyNormalize(content)
	if normalizedStart < 0 || normalizedStart+normalizedLength > len(normalized) {
		return 0, 0, false
	}
	want := normalized[normalizedStart : normalizedStart+normalizedLength]
	for start := range content {
		for end := start; end <= len(content); {
			if fuzzyNormalize(content[start:end]) == want {
				// Accept only a unique original range; a second range means hidden
				// whitespace made mapping ambiguous.
				matches := 0
				for candidateStart := range content {
					for candidateEnd := candidateStart; candidateEnd <= len(content); {
						if fuzzyNormalize(content[candidateStart:candidateEnd]) == want {
							matches++
							if candidateStart != start || candidateEnd != end {
								return 0, 0, false
							}
						}
						if candidateEnd == len(content) {
							break
						}
						_, size := utf8.DecodeRuneInString(content[candidateEnd:])
						candidateEnd += size
					}
				}
				return start, end - start, matches == 1
			}
			if end == len(content) {
				break
			}
			_, size := utf8.DecodeRuneInString(content[end:])
			end += size
		}
	}
	return 0, 0, false
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

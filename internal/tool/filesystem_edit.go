package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

func (s *FilesystemSuite) Edit(ctx context.Context, input EditInput) (ToolResult, error) {
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
		data, err := os.ReadFile(path)
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
		if err := atomicWrite(ctx, path, []byte(after)); err != nil {
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
	beforeLines, afterLines := strings.Split(before, "\n"), strings.Split(after, "\n")
	first := 0
	for first < len(beforeLines) && first < len(afterLines) && beforeLines[first] == afterLines[first] {
		first++
	}
	firstChangedLine = first + 1
	lastBefore, lastAfter := len(beforeLines)-1, len(afterLines)-1
	for lastBefore >= first && lastAfter >= first && beforeLines[lastBefore] == afterLines[lastAfter] {
		lastBefore--
		lastAfter--
	}
	var lines []string
	for index := maxInt(0, first-4); index < first; index++ {
		lines = append(lines, fmt.Sprintf(" %d %s", index+1, beforeLines[index]))
	}
	for index := first; index <= lastBefore; index++ {
		lines = append(lines, fmt.Sprintf("-%d %s", index+1, beforeLines[index]))
	}
	for index := first; index <= lastAfter; index++ {
		lines = append(lines, fmt.Sprintf("+%d %s", index+1, afterLines[index]))
	}
	for index := lastAfter + 1; index <= minInt(len(afterLines)-1, lastAfter+4); index++ {
		lines = append(lines, fmt.Sprintf(" %d %s", index+1, afterLines[index]))
	}
	display = strings.Join(lines, "\n")
	patch = fmt.Sprintf("--- %s\n+++ %s\n@@ -%d,%d +%d,%d @@\n", path, path, first+1, maxInt(0, lastBefore-first+1), first+1, maxInt(0, lastAfter-first+1))
	for index := first; index <= lastBefore; index++ {
		patch += "-" + beforeLines[index] + "\n"
	}
	for index := first; index <= lastAfter; index++ {
		patch += "+" + afterLines[index] + "\n"
	}
	return display, patch, firstChangedLine
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

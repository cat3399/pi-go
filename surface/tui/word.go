package tui

import (
	"unicode"
	"unicode/utf8"
)

// WordBackward and WordForward use byte offsets, the natural index domain for
// Go strings. CJK runes are individual word units; ASCII punctuation forms a
// boundary, matching the observable editor-navigation behavior upstream.
func WordBackward(s string, cursor int) int {
	cursor = clampBoundary(s, cursor)
	for cursor > 0 {
		r, n := utf8DecodeLast(s[:cursor])
		if !unicode.IsSpace(r) {
			_ = n
			break
		}
		cursor -= n
	}
	if cursor == 0 {
		return 0
	}
	r, n := utf8DecodeLast(s[:cursor])
	kind := wordKind(r)
	cursor -= n
	for cursor > 0 {
		q, m := utf8DecodeLast(s[:cursor])
		if wordKind(q) != kind || kind == 2 {
			break
		}
		cursor -= m
	}
	return cursor
}
func WordForward(s string, cursor int) int {
	cursor = clampBoundary(s, cursor)
	for cursor < len(s) {
		r, n := utf8Decode(s[cursor:])
		if !unicode.IsSpace(r) {
			_ = n
			break
		}
		cursor += n
	}
	if cursor >= len(s) {
		return len(s)
	}
	r, n := utf8Decode(s[cursor:])
	kind := wordKind(r)
	cursor += n
	for cursor < len(s) {
		q, m := utf8Decode(s[cursor:])
		if wordKind(q) != kind || kind == 2 {
			break
		}
		cursor += m
	}
	return cursor
}
func wordKind(r rune) int {
	if isCJK(r) {
		return 2
	}
	if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' {
		return 1
	}
	return 0
}
func isCJK(r rune) bool {
	return (r >= 0x3400 && r <= 0x9fff) || (r >= 0x3040 && r <= 0x30ff) || (r >= 0xac00 && r <= 0xd7af)
}
func clampBoundary(s string, n int) int {
	if n < 0 {
		return 0
	}
	if n > len(s) {
		return len(s)
	}
	for n > 0 && n < len(s) && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}
func utf8Decode(s string) (rune, int)     { r, n := utf8.DecodeRuneInString(s); return r, n }
func utf8DecodeLast(s string) (rune, int) { r, n := utf8.DecodeLastRuneInString(s); return r, n }

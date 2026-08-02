package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/width"
)

const TabWidth = 3 // Matches the fixed-layout convention used by upstream pi.

// VisibleWidth counts terminal cells. Control strings do not consume cells;
// invalid UTF-8 is one replacement-rune cell per invalid byte.
func VisibleWidth(s string) int {
	w := 0
	for _, token := range tokenizeTerminal(s) {
		if !token.control {
			w += clusterWidth(token.text)
		}
	}
	return w
}

type terminalToken struct {
	text    string
	control bool
}

func tokenizeTerminal(s string) []terminalToken {
	var out []terminalToken
	for i := 0; i < len(s); {
		if s[i] == esc {
			if n := terminalControlLength(s[i:]); n > 0 {
				out = append(out, terminalToken{s[i : i+n], true})
				i += n
				continue
			}
		}
		start := i
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		// Extend a grapheme enough for terminal cell contracts: combining marks,
		// variation selectors, ZWJ chains, skin tones, and flag pairs.
		for i < len(s) {
			q, m := utf8.DecodeRuneInString(s[i:])
			if unicode.IsMark(q) || q == 0xfe0e || q == 0xfe0f || (q >= 0x1f3fb && q <= 0x1f3ff) {
				i += m
				continue
			}
			if q == 0x200d {
				i += m
				if i < len(s) {
					_, z := utf8.DecodeRuneInString(s[i:])
					i += z
				}
				continue
			}
			if regional(r) && regional(q) {
				i += m
			}
			break
		}
		out = append(out, terminalToken{s[start:i], false})
	}
	return out
}

func terminalControlLength(s string) int {
	if len(s) < 2 || s[0] != esc {
		return 0
	}
	switch s[1] {
	case '[':
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
	case ']', 'P', '_':
		for i := 2; i < len(s); i++ {
			if s[i] == 7 {
				return i + 1
			}
			if s[i] == esc && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
	case 'O':
		if len(s) >= 3 {
			return 3
		}
	}
	return 0
}
func regional(r rune) bool { return r >= 0x1f1e6 && r <= 0x1f1ff }
func clusterWidth(s string) int {
	if s == "\t" {
		return TabWidth
	}
	rs := []rune(s)
	if len(rs) == 0 {
		return 0
	}
	base := rs[0]
	if unicode.IsControl(base) || unicode.IsMark(base) || base == 0x200d {
		return 0
	}
	if regional(base) || hasEmoji(rs) || isWide(base) {
		return 2
	}
	return 1
}
func hasEmoji(rs []rune) bool {
	for _, r := range rs {
		if (r >= 0x1f000 && r <= 0x1faff) || (r >= 0x2600 && r <= 0x27bf) || r == 0xfe0f {
			return true
		}
	}
	return false
}
func isWide(r rune) bool {
	p := width.LookupRune(r).Kind()
	return p == width.EastAsianWide || p == width.EastAsianFullwidth || r >= 0x1100 && r <= 0x115f
}

// StripTerminalSequences removes complete CSI/OSC/DCS/APC/SS3 strings and
// preserves malformed prefixes literally, preventing accidental data loss.
func StripTerminalSequences(s string) string {
	var b strings.Builder
	for _, t := range tokenizeTerminal(s) {
		if !t.control {
			b.WriteString(t.text)
		}
	}
	return b.String()
}

func NormalizeTerminalOutput(s string) string {
	var b strings.Builder
	for _, t := range tokenizeTerminal(s) {
		if t.control {
			b.WriteString(t.text)
			continue
		}
		for _, r := range t.text {
			switch r {
			case '\u0e33':
				b.WriteString("\u0e4d\u0e32")
			case '\u0eb3':
				b.WriteString("\u0ecd\u0eb2")
			case '\t':
				b.WriteString(strings.Repeat(" ", TabWidth))
			default:
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// Truncate keeps a contiguous cell-safe prefix. ANSI state is closed with a
// reset before an ellipsis so no style leaks into later terminal output.
func Truncate(s string, max int, ellipsis string, pad bool) string {
	if max <= 0 {
		return ""
	}
	if VisibleWidth(s) <= max {
		if pad {
			return s + strings.Repeat(" ", max-VisibleWidth(s))
		}
		return s
	}
	ew := VisibleWidth(ellipsis)
	if ew >= max {
		ellipsis = takeCells(ellipsis, max)
		ew = VisibleWidth(ellipsis)
	}
	prefix := takeCells(s, max-ew)
	result := prefix + "\x1b[0m" + ellipsis + "\x1b[0m"
	if pad {
		result += strings.Repeat(" ", max-VisibleWidth(result))
	}
	return result
}
func takeCells(s string, max int) string {
	var b strings.Builder
	w := 0
	for _, t := range tokenizeTerminal(s) {
		if t.control {
			b.WriteString(t.text)
			continue
		}
		cw := clusterWidth(t.text)
		if w+cw > max {
			break
		}
		b.WriteString(t.text)
		w += cw
	}
	return b.String()
}

// SliceColumns returns cells in [start,start+length); strict excludes a wide
// grapheme that straddles the right boundary.
func SliceColumns(s string, start, length int, strict bool) (string, int) {
	if length <= 0 {
		return "", 0
	}
	end := start + length
	col := 0
	var b strings.Builder
	out := 0
	for _, t := range tokenizeTerminal(s) {
		if t.control {
			if col >= start && col < end {
				b.WriteString(t.text)
			}
			continue
		}
		w := clusterWidth(t.text)
		if col >= start && col < end && (!strict || col+w <= end) {
			b.WriteString(t.text)
			out += w
		}
		col += w
		if col >= end {
			break
		}
	}
	return b.String(), out
}

// Wrap splits at whitespace when possible and otherwise at grapheme cells.
// ANSI controls are carried with their adjacent visible cluster; it never
// splits UTF-8 or a grapheme cluster.
func Wrap(s string, max int) []string {
	if max <= 0 {
		return []string{""}
	}
	var lines []string
	for _, raw := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }) {
		lines = append(lines, wrapLine(raw, max)...)
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}
func wrapLine(s string, max int) []string {
	if VisibleWidth(s) <= max {
		return []string{s}
	}
	var out []string
	var b strings.Builder
	w := 0
	for _, t := range tokenizeTerminal(s) {
		cw := 0
		if !t.control {
			cw = clusterWidth(t.text)
		}
		if cw > 0 && w+cw > max && w > 0 {
			out = append(out, strings.TrimRight(b.String(), " \t"))
			b.Reset()
			w = 0
		}
		b.WriteString(t.text)
		w += cw
	}
	out = append(out, strings.TrimRight(b.String(), " \t"))
	return out
}

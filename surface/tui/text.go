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
		joinNext := false
		for i < len(s) {
			q, m := utf8.DecodeRuneInString(s[i:])
			if unicode.IsMark(q) || q == 0xfe0e || q == 0xfe0f || (q >= 0x1f3fb && q <= 0x1f3ff) {
				i += m
				joinNext = joinNext || isVirama(q)
				continue
			}
			if q == 0x200d {
				i += m
				if i < len(s) {
					next, z := utf8.DecodeRuneInString(s[i:])
					// A ZWJ only joins a following printable rune. Consuming an
					// ESC/control here would hide it from the ANSI tokenizer and
					// turn the rest of the control string into visible text.
					if (next == utf8.RuneError && z == 1) || unicode.IsControl(next) {
						break
					}
					i += z
				}
				joinNext = false
				continue
			}
			if joinNext && (unicode.IsLetter(q) || unicode.IsNumber(q)) {
				i += m
				joinNext = false
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
func isVirama(r rune) bool {
	switch r {
	case 0x094d, 0x09cd, 0x0a4d, 0x0acd, 0x0b4d, 0x0bcd, 0x0c4d, 0x0ccd, 0x0d4d:
		return true
	default:
		return false
	}
}
func clusterWidth(s string) int {
	if s == "\t" {
		return TabWidth
	}
	rs := []rune(s)
	if len(rs) == 0 {
		return 0
	}
	allSpacingMarks := true
	for _, r := range rs {
		if !terminalSpacingMark(r) {
			allSpacingMarks = false
			break
		}
	}
	if allSpacingMarks {
		return len(rs)
	}
	baseIndex := -1
	for i, r := range rs {
		if !nonPrinting(r) {
			baseIndex = i
			break
		}
	}
	if baseIndex < 0 {
		return 0
	}
	base := rs[baseIndex]
	if regional(base) || hasEmoji(rs) || isWide(base) {
		return 2
	}
	w := runeWidth(base)
	followsMark := false
	for _, r := range rs[baseIndex+1:] {
		switch {
		case terminalSpacingMark(r):
			w++
			followsMark = false
		case unicode.IsMark(r):
			followsMark = true
		case nonPrinting(r):
			// Keep followsMark across format/default-ignorable code points.
		default:
			if followsMark || (r >= 0xff00 && r <= 0xffef) {
				w += runeWidth(r)
			}
			if r == 0x0e33 || r == 0x0eb3 {
				w++
			}
			followsMark = false
		}
	}
	return w
}

func terminalSpacingMark(r rune) bool {
	if r == 0x1734 || r == 0x302e || r == 0x302f {
		return false
	}
	if unicode.Is(unicode.Mc, r) {
		return true
	}
	return r == 0x065f || r == 0x0f7f || r == 0x102b || r == 0x102c || r == 0x1031 ||
		(r >= 0x1033 && r <= 0x1035) || r == 0x1038 || (r >= 0x103a && r <= 0x103e)
}
func nonPrinting(r rune) bool {
	return unicode.IsControl(r) || unicode.IsMark(r) || unicode.Is(unicode.Cf, r) || r == 0x200d
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
func runeWidth(r rune) int {
	if isWide(r) {
		return 2
	}
	return 1
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
		clipped := takeCells(ellipsis, max)
		if VisibleWidth(clipped) == 0 {
			return truncateEmpty(max, pad)
		}
		result := "\x1b[0m" + clipped + "\x1b[0m"
		if pad {
			result += strings.Repeat(" ", max-VisibleWidth(result))
		}
		return result
	}
	prefix := takeCells(s, max-ew)
	if VisibleWidth(prefix) == 0 && ew == 0 {
		return truncateEmpty(max, pad)
	}
	result := prefix + "\x1b[0m"
	if ellipsis != "" {
		result += ellipsis + "\x1b[0m"
	}
	if pad {
		result += strings.Repeat(" ", max-VisibleWidth(result))
	}
	return result
}

func truncateEmpty(max int, pad bool) string {
	if pad {
		return strings.Repeat(" ", max)
	}
	return ""
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
	if start < 0 {
		start = 0
	}
	end := start + length
	col := 0
	var b strings.Builder
	out := 0
	started := false
	state := ansiState{}
	for _, t := range tokenizeTerminal(s) {
		if t.control {
			if col >= end {
				break
			}
			if started && col >= start {
				b.WriteString(t.text)
			}
			state.process(t.text)
			continue
		}
		w := clusterWidth(t.text)
		if col >= start && col < end && (!strict || col+w <= end) {
			if !started {
				b.WriteString(state.prefix())
				started = true
			}
			b.WriteString(t.text)
			out += w
		}
		col += w
		if col >= end {
			break
		}
	}
	if !started {
		return "", 0
	}
	if state.hyperlinkClose != "" {
		b.WriteString(state.hyperlinkClose)
	}
	if state.sgr != "" {
		b.WriteString("\x1b[0m")
	}
	return b.String(), out
}

// Wrap splits at whitespace when possible and otherwise at grapheme cells.
// Explicit empty lines are preserved. SGR and OSC-8 state is replayed on
// continuation lines, and active hyperlinks are closed at every line edge.
func Wrap(s string, max int) []string {
	if max <= 0 {
		return []string{""}
	}
	state := ansiState{}
	physical := splitPhysicalLines(s)
	lines := make([]string, 0, len(physical))
	for _, raw := range physical {
		lines = append(lines, wrapLine(raw, max, &state)...)
	}
	return lines
}

type wrapChunk struct {
	text       string
	width      int
	whitespace bool
}

func makeWrapChunks(s string) []wrapChunk {
	var chunks []wrapChunk
	var pending strings.Builder
	for _, token := range tokenizeTerminal(s) {
		if token.control {
			pending.WriteString(token.text)
			continue
		}
		space := true
		for _, r := range token.text {
			if !unicode.IsSpace(r) {
				space = false
				break
			}
		}
		w := clusterWidth(token.text)
		first, _ := utf8.DecodeRuneInString(token.text)
		if !space && isCJK(first) {
			chunks = append(chunks, wrapChunk{text: pending.String() + token.text, width: w})
			pending.Reset()
			continue
		}
		if len(chunks) == 0 || chunks[len(chunks)-1].whitespace != space {
			chunks = append(chunks, wrapChunk{whitespace: space})
		}
		chunk := &chunks[len(chunks)-1]
		chunk.text += pending.String() + token.text
		pending.Reset()
		chunk.width += w
	}
	if pending.Len() > 0 {
		if len(chunks) == 0 {
			chunks = append(chunks, wrapChunk{})
		}
		chunks[len(chunks)-1].text += pending.String()
	}
	return chunks
}

func wrapLine(s string, max int, state *ansiState) []string {
	chunks := makeWrapChunks(s)
	followingContent := make([]bool, len(chunks))
	seenContent := false
	for i := len(chunks) - 1; i >= 0; i-- {
		followingContent[i] = seenContent
		if !chunks[i].whitespace && chunks[i].width > 0 {
			seenContent = true
		}
	}
	var out []string
	line := state.prefix()
	lineWidth := 0
	lineHasContent := false
	emit := func(trimBreak bool) {
		rendered := line
		if trimBreak && lineHasContent {
			rendered = trimTerminalSpace(rendered)
		}
		out = append(out, rendered+state.lineEndReset()+state.hyperlinkClose)
		line = state.prefix()
		lineWidth = 0
		lineHasContent = false
	}
	appendRaw := func(raw string, w int, content bool) {
		line += raw
		lineWidth += w
		lineHasContent = lineHasContent || content
		state.processText(raw)
	}
	for chunkIndex, chunk := range chunks {
		if chunk.whitespace {
			// Whitespace is data too: retain indentation, blank lines, and
			// trailing spaces. It is trimmed only when it is the legal break
			// separator before a word moved to the next line.
			if lineHasContent && lineWidth+chunk.width > max && followingContent[chunkIndex] {
				appendRaw(terminalControls(chunk.text), 0, false)
				emit(true)
				continue
			}
			for _, token := range tokenizeTerminal(chunk.text) {
				if token.control {
					appendRaw(token.text, 0, false)
					continue
				}
				w := clusterWidth(token.text)
				if lineWidth > 0 && lineWidth+w > max {
					emit(false)
				}
				appendRaw(token.text, w, false)
			}
			continue
		}
		if chunk.width <= max {
			if lineWidth > 0 && lineWidth+chunk.width > max {
				emit(lineHasContent)
			}
			appendRaw(chunk.text, chunk.width, chunk.width > 0)
			continue
		}
		if lineWidth > 0 {
			emit(lineHasContent)
		}
		for _, token := range tokenizeTerminal(chunk.text) {
			if token.control {
				appendRaw(token.text, 0, false)
				continue
			}
			w := clusterWidth(token.text)
			if lineWidth > 0 && lineWidth+w > max {
				emit(lineHasContent)
			}
			appendRaw(token.text, w, true)
		}
	}
	// Every physical input line yields one output line, including an explicit
	// empty line or a line containing only terminal controls.
	out = append(out, line+state.hyperlinkClose)
	return out
}

func terminalControls(s string) string {
	var b strings.Builder
	for _, t := range tokenizeTerminal(s) {
		if t.control {
			b.WriteString(t.text)
		}
	}
	return b.String()
}

func splitPhysicalLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '\r' && s[i] != '\n' {
			continue
		}
		out = append(out, s[start:i])
		if s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n' {
			i++
		}
		start = i + 1
	}
	return append(out, s[start:])
}

func trimTerminalSpace(s string) string {
	tokens := tokenizeTerminal(s)
	last := -1
	for i, t := range tokens {
		if t.control {
			continue
		}
		space := true
		for _, r := range t.text {
			if !unicode.IsSpace(r) {
				space = false
				break
			}
		}
		if !space {
			last = i
		}
	}
	if last < 0 {
		var b strings.Builder
		for _, t := range tokens {
			if t.control {
				b.WriteString(t.text)
			}
		}
		return b.String()
	}
	var b strings.Builder
	for i, t := range tokens {
		if i <= last || t.control {
			b.WriteString(t.text)
		}
	}
	return b.String()
}

type ansiState struct {
	sgr            string
	hyperlink      string
	hyperlinkClose string
	underline      bool
}

func (s *ansiState) prefix() string { return s.sgr + s.hyperlink }
func (s *ansiState) lineEndReset() string {
	if s.underline {
		return "\x1b[24m"
	}
	return ""
}
func (s *ansiState) processText(text string) {
	for _, t := range tokenizeTerminal(text) {
		if t.control {
			s.process(t.text)
		}
	}
}
func (s *ansiState) process(code string) {
	if strings.HasPrefix(code, "\x1b[") && strings.HasSuffix(code, "m") {
		body := code[2 : len(code)-1]
		parts := strings.Split(body, ";")
		reset := body == ""
		nonzero := false
		for _, p := range parts {
			switch p {
			case "0":
				reset = true
			case "4":
				s.underline = true
			case "24":
				s.underline = false
			}
			if p != "" && p != "0" {
				nonzero = true
			}
		}
		if reset {
			s.sgr = ""
			s.underline = false
		}
		if nonzero {
			s.sgr += code
		}
		return
	}
	if !strings.HasPrefix(code, "\x1b]8;") {
		return
	}
	term := "\x1b\\"
	if strings.HasSuffix(code, "\a") {
		term = "\a"
	}
	body := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(code, "\x1b]8;"), "\a"), "\x1b\\")
	parts := strings.SplitN(body, ";", 2)
	if len(parts) != 2 {
		return
	}
	if parts[1] == "" {
		s.hyperlink = ""
		s.hyperlinkClose = ""
		return
	}
	s.hyperlink = code
	s.hyperlinkClose = "\x1b]8;;" + term
}

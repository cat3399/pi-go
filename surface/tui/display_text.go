package tui

import (
	"strings"
	"unicode"
)

// sanitizeDisplayText removes terminal controls supplied by models and tools.
// Renderers add their own trusted SGR/OSC sequences only after this boundary.
func sanitizeDisplayText(value string) string {
	value = strings.ToValidUTF8(value, "�")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = StripTerminalSequences(value)
	return strings.Map(func(value rune) rune {
		switch value {
		case '\n', '\t':
			return value
		default:
			if unicode.IsControl(value) {
				return -1
			}
			return value
		}
	}, value)
}

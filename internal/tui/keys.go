package tui

import (
	"strconv"
	"strings"
	"unicode"
)

type Modifiers uint8

const (
	Shift Modifiers = 1 << iota
	Alt
	Ctrl
	Super
)

type KeyEventType uint8

const (
	KeyPress KeyEventType = iota
	KeyRepeat
	KeyRelease
)

type KeyEvent struct {
	Key       string
	Modifiers Modifiers
	Type      KeyEventType
	Text      string
}

// ParseKey accepts legacy VT, xterm modifyOtherKeys, and Kitty CSI-u input.
// Unknown control strings return false rather than becoming printable text.
func ParseKey(s string) (KeyEvent, bool) {
	if ev, ok := parseCSI(s); ok {
		return ev, true
	}
	legacy := map[string]string{"\x1b": "escape", "\r": "enter", "\n": "enter", "\t": "tab", "\x7f": "backspace", "\x1b[A": "up", "\x1b[B": "down", "\x1b[C": "right", "\x1b[D": "left", "\x1bOA": "up", "\x1bOB": "down", "\x1bOC": "right", "\x1bOD": "left", "\x1b[H": "home", "\x1b[F": "end", "\x1b[Z": "tab"}
	if key, ok := legacy[s]; ok {
		ev := KeyEvent{Key: key}
		if s == "\x1b[Z" {
			ev.Modifiers = Shift
		}
		return ev, true
	}
	if len(s) == 2 && s[0] == esc {
		return KeyEvent{Key: strings.ToLower(s[1:]), Modifiers: Alt, Text: s[1:]}, true
	}
	r := []rune(s)
	if len(r) != 1 {
		return KeyEvent{}, false
	}
	if r[0] >= 1 && r[0] <= 26 {
		return KeyEvent{Key: string(rune('a' + r[0] - 1)), Modifiers: Ctrl}, true
	}
	if unicode.IsUpper(r[0]) {
		return KeyEvent{Key: strings.ToLower(s), Modifiers: Shift, Text: s}, true
	}
	return KeyEvent{Key: s, Text: s}, true
}

func parseCSI(s string) (KeyEvent, bool) {
	if !strings.HasPrefix(s, "\x1b[") {
		return KeyEvent{}, false
	}
	body := strings.TrimPrefix(s, "\x1b[")
	if strings.HasSuffix(body, "u") {
		parts := strings.Split(strings.TrimSuffix(body, "u"), ";")
		if len(parts) > 2 || len(parts) == 0 {
			return KeyEvent{}, false
		}
		cpFields := strings.Split(parts[0], ":")
		cp, err := strconv.Atoi(cpFields[0])
		if err != nil || cp < 0 {
			return KeyEvent{}, false
		}
		mod, event := "1", ""
		if len(parts) == 2 {
			x := strings.Split(parts[1], ":")
			mod = x[0]
			if len(x) > 1 {
				event = x[1]
			}
		}
		m, err := strconv.Atoi(mod)
		if err != nil || m < 1 {
			return KeyEvent{}, false
		}
		key := kittyKey(cp)
		if key == "" {
			key = string(rune(cp))
		}
		ev := KeyEvent{Key: strings.ToLower(key), Modifiers: Modifiers((m - 1) & 15), Type: kittyType(event), Text: key}
		if len([]rune(key)) == 1 && unicode.IsUpper([]rune(key)[0]) {
			ev.Key = strings.ToLower(key)
			ev.Modifiers |= Shift
		}
		return ev, true
	}
	if strings.HasPrefix(body, "27;") && strings.HasSuffix(body, "~") {
		p := strings.Split(strings.TrimSuffix(strings.TrimPrefix(body, "27;"), "~"), ";")
		if len(p) != 2 {
			return KeyEvent{}, false
		}
		m, _ := strconv.Atoi(p[0])
		cp, _ := strconv.Atoi(p[1])
		return KeyEvent{Key: strings.ToLower(string(rune(cp))), Modifiers: Modifiers((m - 1) & 15), Text: string(rune(cp))}, true
	}
	if strings.HasSuffix(body, "~") {
		p := strings.Split(strings.TrimSuffix(body, "~"), ";")
		n, err := strconv.Atoi(p[0])
		if err != nil {
			return KeyEvent{}, false
		}
		key := map[int]string{2: "insert", 3: "delete", 5: "pageup", 6: "pagedown", 7: "home", 8: "end"}[n]
		if key == "" {
			return KeyEvent{}, false
		}
		m := Modifiers(0)
		if len(p) > 1 {
			x, _ := strconv.Atoi(p[1])
			m = Modifiers((x - 1) & 15)
		}
		return KeyEvent{Key: key, Modifiers: m}, true
	}
	if len(body) >= 1 {
		final := body[len(body)-1]
		keys := map[byte]string{'A': "up", 'B': "down", 'C': "right", 'D': "left", 'H': "home", 'F': "end"}
		if key := keys[final]; key != "" {
			m := Modifiers(0)
			p := strings.Split(strings.TrimSuffix(body, string(final)), ";")
			if len(p) == 2 {
				x, _ := strconv.Atoi(p[1])
				m = Modifiers((x - 1) & 15)
			}
			return KeyEvent{Key: key, Modifiers: m}, true
		}
	}
	return KeyEvent{}, false
}
func kittyType(s string) KeyEventType {
	if s == "2" {
		return KeyRepeat
	}
	if s == "3" {
		return KeyRelease
	}
	return KeyPress
}
func kittyKey(cp int) string {
	return map[int]string{27: "escape", 9: "tab", 13: "enter", 127: "backspace", 57350: "escape", 57352: "tab", 57357: "backspace", 57414: "enter", 57417: "left", 57418: "right", 57419: "up", 57420: "down", 57421: "pageup", 57422: "pagedown", 57423: "home", 57424: "end", 57425: "insert", 57426: "delete"}[cp]
}

// Matches canonicalizes modifier ordering, so ctrl+shift+p and shift+ctrl+p
// are equivalent. "esc", "return", "pageUp" and "pageDown" are aliases.
func Matches(ev KeyEvent, id string) bool {
	p := strings.Split(strings.ToLower(id), "+")
	if len(p) == 0 {
		return false
	}
	want := Modifiers(0)
	for _, x := range p[:len(p)-1] {
		switch x {
		case "shift":
			want |= Shift
		case "alt":
			want |= Alt
		case "ctrl", "control":
			want |= Ctrl
		case "super", "meta":
			want |= Super
		default:
			return false
		}
	}
	key := p[len(p)-1]
	aliases := map[string]string{"esc": "escape", "return": "enter", "pageup": "pageup", "pagedown": "pagedown"}
	if v := aliases[key]; v != "" {
		key = v
	}
	return ev.Modifiers == want && strings.ToLower(ev.Key) == key
}

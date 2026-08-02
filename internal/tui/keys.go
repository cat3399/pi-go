package tui

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
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
	if ev, ok := legacyKeyEvents[s]; ok {
		return ev, true
	}
	if len(s) == 2 && s[0] == esc {
		r, n := utf8.DecodeRuneInString(s[1:])
		if n != len(s)-1 {
			return KeyEvent{}, false
		}
		if r >= 1 && r <= 26 {
			return KeyEvent{Key: string(rune('a' + r - 1)), Modifiers: Ctrl | Alt}, true
		}
		key := strings.ToLower(s[1:])
		if r == ' ' {
			key = "space"
		}
		if unicode.IsControl(r) {
			return KeyEvent{}, false
		}
		return KeyEvent{Key: key, Modifiers: Alt, Text: s[1:]}, true
	}
	r := []rune(s)
	if len(r) != 1 {
		return KeyEvent{}, false
	}
	if r[0] >= 1 && r[0] <= 26 {
		return KeyEvent{Key: string(rune('a' + r[0] - 1)), Modifiers: Ctrl}, true
	}
	switch r[0] {
	case 0x1c:
		return KeyEvent{Key: "\\", Modifiers: Ctrl}, true
	case 0x1d:
		return KeyEvent{Key: "]", Modifiers: Ctrl}, true
	case 0x1f:
		return KeyEvent{Key: "-", Modifiers: Ctrl}, true
	}
	if r[0] == ' ' {
		return KeyEvent{Key: "space", Text: s}, true
	}
	if unicode.IsUpper(r[0]) {
		return KeyEvent{Key: strings.ToLower(s), Modifiers: Shift, Text: s}, true
	}
	if unicode.IsControl(r[0]) {
		return KeyEvent{}, false
	}
	return KeyEvent{Key: s, Text: s}, true
}

var (
	kittyCSIRe    = regexp.MustCompile(`^\x1b\[([0-9]+)(:([0-9]*))?(:([0-9]+))?(;([0-9]+))?(:([123]))?u$`)
	modifyOtherRe = regexp.MustCompile(`^\x1b\[27;([0-9]+);([0-9]+)~$`)
	functionRe    = regexp.MustCompile(`^\x1b\[([0-9]+)(;([0-9]+))?(:([123]))?~$`)
	arrowRe       = regexp.MustCompile(`^\x1b\[1;([0-9]+)(:([123]))?([ABCDHF])$`)
)

var legacyKeyEvents = map[string]KeyEvent{
	"\x1b": {Key: "escape"},
	"\r":   {Key: "enter"},
	"\n":   {Key: "enter"},
	"\t":   {Key: "tab"},
	"\x08": {Key: "backspace"},
	"\x7f": {Key: "backspace"},
	"\x00": {Key: "space", Modifiers: Ctrl},

	"\x1b[A":   {Key: "up"},
	"\x1b[B":   {Key: "down"},
	"\x1b[C":   {Key: "right"},
	"\x1b[D":   {Key: "left"},
	"\x1bOA":   {Key: "up"},
	"\x1bOB":   {Key: "down"},
	"\x1bOC":   {Key: "right"},
	"\x1bOD":   {Key: "left"},
	"\x1b[H":   {Key: "home"},
	"\x1bOH":   {Key: "home"},
	"\x1b[1~":  {Key: "home"},
	"\x1b[7~":  {Key: "home"},
	"\x1b[F":   {Key: "end"},
	"\x1bOF":   {Key: "end"},
	"\x1b[4~":  {Key: "end"},
	"\x1b[8~":  {Key: "end"},
	"\x1b[E":   {Key: "clear"},
	"\x1bOE":   {Key: "clear"},
	"\x1b[2~":  {Key: "insert"},
	"\x1b[3~":  {Key: "delete"},
	"\x1b[5~":  {Key: "pageup"},
	"\x1b[[5~": {Key: "pageup"},
	"\x1b[6~":  {Key: "pagedown"},
	"\x1b[[6~": {Key: "pagedown"},

	"\x1b[Z":  {Key: "tab", Modifiers: Shift},
	"\x1b[a":  {Key: "up", Modifiers: Shift},
	"\x1b[b":  {Key: "down", Modifiers: Shift},
	"\x1b[c":  {Key: "right", Modifiers: Shift},
	"\x1b[d":  {Key: "left", Modifiers: Shift},
	"\x1b[e":  {Key: "clear", Modifiers: Shift},
	"\x1bOa":  {Key: "up", Modifiers: Ctrl},
	"\x1bOb":  {Key: "down", Modifiers: Ctrl},
	"\x1bOc":  {Key: "right", Modifiers: Ctrl},
	"\x1bOd":  {Key: "left", Modifiers: Ctrl},
	"\x1bOe":  {Key: "clear", Modifiers: Ctrl},
	"\x1b[2$": {Key: "insert", Modifiers: Shift},
	"\x1b[3$": {Key: "delete", Modifiers: Shift},
	"\x1b[5$": {Key: "pageup", Modifiers: Shift},
	"\x1b[6$": {Key: "pagedown", Modifiers: Shift},
	"\x1b[7$": {Key: "home", Modifiers: Shift},
	"\x1b[8$": {Key: "end", Modifiers: Shift},
	"\x1b[2^": {Key: "insert", Modifiers: Ctrl},
	"\x1b[3^": {Key: "delete", Modifiers: Ctrl},
	"\x1b[5^": {Key: "pageup", Modifiers: Ctrl},
	"\x1b[6^": {Key: "pagedown", Modifiers: Ctrl},
	"\x1b[7^": {Key: "home", Modifiers: Ctrl},
	"\x1b[8^": {Key: "end", Modifiers: Ctrl},

	"\x1bOP":  {Key: "f1"},
	"\x1bOQ":  {Key: "f2"},
	"\x1bOR":  {Key: "f3"},
	"\x1bOS":  {Key: "f4"},
	"\x1b[[A": {Key: "f1"},
	"\x1b[[B": {Key: "f2"},
	"\x1b[[C": {Key: "f3"},
	"\x1b[[D": {Key: "f4"},
	"\x1b[[E": {Key: "f5"},

	"\x1bB":    {Key: "left", Modifiers: Alt},
	"\x1bF":    {Key: "right", Modifiers: Alt},
	"\x1bb":    {Key: "left", Modifiers: Alt},
	"\x1bf":    {Key: "right", Modifiers: Alt},
	"\x1bp":    {Key: "up", Modifiers: Alt},
	"\x1bn":    {Key: "down", Modifiers: Alt},
	"\x1b\r":   {Key: "enter", Modifiers: Alt},
	"\x1b\x7f": {Key: "backspace", Modifiers: Alt},
	"\x1b\x08": {Key: "backspace", Modifiers: Alt},
	"\x1b\x1b": {Key: "[", Modifiers: Ctrl | Alt},
	"\x1b\x1c": {Key: "\\", Modifiers: Ctrl | Alt},
	"\x1b\x1d": {Key: "]", Modifiers: Ctrl | Alt},
	"\x1b\x1f": {Key: "-", Modifiers: Ctrl | Alt},
}

func parseCSI(s string) (KeyEvent, bool) {
	if !strings.HasPrefix(s, "\x1b[") {
		return KeyEvent{}, false
	}
	if match := kittyCSIRe.FindStringSubmatch(s); match != nil {
		cp, ok := parseCodepoint(match[1])
		if !ok {
			return KeyEvent{}, false
		}
		if match[3] != "" {
			if _, ok = parseCodepoint(match[3]); !ok {
				return KeyEvent{}, false
			}
		}
		base := -1
		if match[5] != "" {
			if base, ok = parseCodepoint(match[5]); !ok {
				return KeyEvent{}, false
			}
		}
		modifiers, ok := parseModifier(match[7])
		if !ok {
			return KeyEvent{}, false
		}
		return keyFromCodepoint(cp, base, modifiers, kittyType(match[9]))
	}
	if match := modifyOtherRe.FindStringSubmatch(s); match != nil {
		modifiers, ok := parseModifier(match[1])
		if !ok {
			return KeyEvent{}, false
		}
		cp, ok := parseCodepoint(match[2])
		if !ok {
			return KeyEvent{}, false
		}
		return keyFromCodepoint(cp, -1, modifiers, KeyPress)
	}
	if match := functionRe.FindStringSubmatch(s); match != nil {
		n, ok := strictDecimal(match[1])
		if !ok {
			return KeyEvent{}, false
		}
		key := map[int]string{2: "insert", 3: "delete", 5: "pageup", 6: "pagedown", 7: "home", 8: "end", 11: "f1", 12: "f2", 13: "f3", 14: "f4", 15: "f5", 17: "f6", 18: "f7", 19: "f8", 20: "f9", 21: "f10", 23: "f11", 24: "f12"}[n]
		if key == "" {
			return KeyEvent{}, false
		}
		modifiers, ok := parseModifier(match[3])
		if !ok {
			return KeyEvent{}, false
		}
		return KeyEvent{Key: key, Modifiers: modifiers, Type: kittyType(match[5])}, true
	}
	if match := arrowRe.FindStringSubmatch(s); match != nil {
		modifiers, ok := parseModifier(match[1])
		if !ok {
			return KeyEvent{}, false
		}
		key := map[byte]string{'A': "up", 'B': "down", 'C': "right", 'D': "left", 'H': "home", 'F': "end"}[match[4][0]]
		return KeyEvent{Key: key, Modifiers: modifiers, Type: kittyType(match[3])}, true
	}
	return KeyEvent{}, false
}

func parseModifier(field string) (Modifiers, bool) {
	if field == "" {
		return 0, true
	}
	n, ok := strictDecimal(field)
	if !ok || n < 1 {
		return 0, false
	}
	raw := n - 1
	const lockMask = 64 | 128
	if raw & ^(15|lockMask) != 0 {
		return 0, false
	}
	return Modifiers(raw & 15), true
}

func parseCodepoint(field string) (int, bool) {
	n, ok := strictDecimal(field)
	if !ok || n > utf8.MaxRune || !utf8.ValidRune(rune(n)) {
		return 0, false
	}
	return n, true
}

func keyFromCodepoint(cp, base int, modifiers Modifiers, eventType KeyEventType) (KeyEvent, bool) {
	cp = normalizeKittyCodepoint(cp)
	identity := cp
	if identity >= 'A' && identity <= 'Z' {
		identity += 'a' - 'A'
	}
	if !canonicalASCII(identity) && base >= 0 {
		identity = normalizeKittyCodepoint(base)
	}
	key := kittyKey(identity)
	if key == "" {
		r := rune(identity)
		if !canonicalASCII(identity) {
			return KeyEvent{}, false
		}
		key = strings.ToLower(string(r))
	}
	text := ""
	if len([]rune(key)) == 1 {
		text = string(rune(cp))
	}
	return KeyEvent{Key: key, Modifiers: modifiers, Type: eventType, Text: text}, true
}

func canonicalASCII(cp int) bool {
	return cp >= 'a' && cp <= 'z' || cp >= 'A' && cp <= 'Z' || cp >= '0' && cp <= '9' || strings.ContainsRune("`-=[]\\;',./!@#$%^&*()_+|~{}:<>?", rune(cp))
}

func normalizeKittyCodepoint(cp int) int {
	if normalized, ok := map[int]int{57399: '0', 57400: '1', 57401: '2', 57402: '3', 57403: '4', 57404: '5', 57405: '6', 57406: '7', 57407: '8', 57408: '9', 57409: '.', 57410: '/', 57411: '*', 57412: '-', 57413: '+', 57415: '=', 57416: ',', 57417: -4, 57418: -3, 57419: -1, 57420: -2, 57421: -12, 57422: -13, 57423: -14, 57424: -15, 57425: -11, 57426: -10}[cp]; ok {
		return normalized
	}
	return cp
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
	return map[int]string{27: "escape", 9: "tab", 13: "enter", 32: "space", 127: "backspace", 57350: "escape", 57352: "tab", 57357: "backspace", 57414: "enter", -1: "up", -2: "down", -3: "right", -4: "left", -10: "delete", -11: "insert", -12: "pageup", -13: "pagedown", -14: "home", -15: "end"}[cp]
}

// Matches canonicalizes modifier ordering, so ctrl+shift+p and shift+ctrl+p
// are equivalent. "esc", "return", "pageUp" and "pageDown" are aliases.
func Matches(ev KeyEvent, id string) bool {
	key, want, ok := parseBindingID(id)
	if !ok {
		return false
	}
	return ev.Modifiers == want && strings.ToLower(ev.Key) == key
}

func parseBindingID(id string) (string, Modifiers, bool) {
	id = strings.ToLower(id)
	key := ""
	modifierPart := ""
	if id == "+" {
		key = "+"
	} else if strings.HasSuffix(id, "++") {
		key = "+"
		modifierPart = strings.TrimSuffix(id, "++")
	} else {
		at := strings.LastIndexByte(id, '+')
		if at < 0 {
			key = id
		} else {
			key = id[at+1:]
			modifierPart = id[:at]
		}
	}
	if key == "" {
		return "", 0, false
	}
	want := Modifiers(0)
	if modifierPart != "" {
		for _, x := range strings.Split(modifierPart, "+") {
			before := want
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
				return "", 0, false
			}
			if want == before {
				return "", 0, false
			}
		}
	}
	aliases := map[string]string{"esc": "escape", "return": "enter", "pgup": "pageup", "pgdown": "pagedown"}
	if v := aliases[key]; v != "" {
		key = v
	}
	return key, want, true
}

// ID returns the canonical keybinding identifier for an event. Every event
// produced by ParseKey has an ID that Matches accepts.
func (ev KeyEvent) ID() string {
	var parts []string
	if ev.Modifiers&Shift != 0 {
		parts = append(parts, "shift")
	}
	if ev.Modifiers&Ctrl != 0 {
		parts = append(parts, "ctrl")
	}
	if ev.Modifiers&Alt != 0 {
		parts = append(parts, "alt")
	}
	if ev.Modifiers&Super != 0 {
		parts = append(parts, "super")
	}
	parts = append(parts, strings.ToLower(ev.Key))
	return strings.Join(parts, "+")
}

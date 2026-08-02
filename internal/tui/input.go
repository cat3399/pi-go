// Package tui contains the terminal-facing primitives shared by future
// interactive components.  It deliberately owns bytes and terminal state, not
// editor state, layout, or rendering.
package tui

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	esc       = byte(0x1b)
	pasteOpen = "\x1b[200~"
	pasteEnd  = "\x1b[201~"
)

var (
	ErrInputTooLarge = errors.New("tui: buffered input exceeds configured limit")
	ErrInvalidUTF8   = errors.New("tui: invalid UTF-8 input")
)

// InvalidUTF8Policy controls malformed terminal bytes. Replace is appropriate
// for a human-facing terminal; Reject lets an embedding fail closed.
type InvalidUTF8Policy uint8

const (
	ReplaceInvalidUTF8 InvalidUTF8Policy = iota
	RejectInvalidUTF8
)

type InputKind uint8

const (
	InputSequence InputKind = iota
	InputPaste
	InputFocusIn
	InputFocusOut
	InputMouse
	InputResize
)

type InputEvent struct {
	Kind   InputKind
	Data   string
	Mouse  *MouseEvent
	Width  int
	Height int
}

type MouseEvent struct {
	Button    int
	X, Y      int // zero-based terminal cells
	Press     bool
	Motion    bool
	Modifiers Modifiers
}

func ResizeEvent(width, height int) InputEvent {
	return InputEvent{Kind: InputResize, Width: width, Height: height}
}

type FramerOptions struct {
	// MaxBufferedBytes bounds an unfinished control string or paste. Zero uses
	// 1 MiB. A bound violation clears only the untrusted pending input.
	MaxBufferedBytes int
	InvalidUTF8      InvalidUTF8Policy
}

// Framer turns arbitrary stdin chunks into atomic key/control sequences. Feed
// never starts goroutines or timers: callers choose when to Flush incomplete
// input, making cancellation and timeout ownership explicit.
type Framer struct {
	buf       []byte
	paste     []byte
	inPaste   bool
	max       int
	utf8Mode  InvalidUTF8Policy
	pendingCP rune
}

func NewFramer(opts FramerOptions) *Framer {
	max := opts.MaxBufferedBytes
	if max == 0 {
		max = 1 << 20
	}
	return &Framer{max: max, utf8Mode: opts.InvalidUTF8}
}

func (f *Framer) Buffered() int { return len(f.buf) + len(f.paste) }

func (f *Framer) Reset() { f.buf, f.paste, f.inPaste, f.pendingCP = nil, nil, false, 0 }

func (f *Framer) Feed(chunk []byte) ([]InputEvent, error) {
	if len(chunk) == 0 {
		return nil, nil
	}
	if f.inPaste {
		f.paste = append(f.paste, chunk...)
		return f.consumePaste(nil)
	}
	f.buf = append(f.buf, chunk...)
	if len(f.buf) > f.max {
		f.Reset()
		return nil, ErrInputTooLarge
	}
	var out []InputEvent
	for len(f.buf) > 0 {
		if bytes.HasPrefix(f.buf, []byte(pasteOpen)) {
			f.buf = f.buf[len(pasteOpen):]
			f.inPaste = true
			f.paste = append(f.paste, f.buf...)
			f.buf = nil
			more, err := f.consumePaste(out)
			return more, err
		}
		if f.buf[0] == esc {
			n, complete := escapeLength(f.buf)
			if !complete {
				break
			}
			out = f.emit(out, string(f.buf[:n]))
			f.buf = f.buf[n:]
			continue
		}
		r, n := utf8.DecodeRune(f.buf)
		if r == utf8.RuneError && n == 1 {
			if !utf8.FullRune(f.buf) {
				break
			}
			if f.utf8Mode == RejectInvalidUTF8 {
				f.Reset()
				return nil, ErrInvalidUTF8
			}
		}
		out = f.emit(out, string(f.buf[:n]))
		f.buf = f.buf[n:]
	}
	return out, nil
}

func (f *Framer) consumePaste(out []InputEvent) ([]InputEvent, error) {
	if len(f.paste) > f.max {
		f.Reset()
		return nil, ErrInputTooLarge
	}
	if at := bytes.Index(f.paste, []byte(pasteEnd)); at >= 0 {
		data, err := f.text(f.paste[:at])
		if err != nil {
			f.Reset()
			return nil, err
		}
		out = append(out, InputEvent{Kind: InputPaste, Data: data})
		rest := append([]byte(nil), f.paste[at+len(pasteEnd):]...)
		f.paste, f.inPaste = nil, false
		if len(rest) > 0 {
			more, err := f.Feed(rest)
			return append(out, more...), err
		}
	}
	return out, nil
}

// Flush reports a partial control sequence or unfinished paste as a normal
// sequence/paste according to the UTF-8 policy. It is safe to call on EOF.
func (f *Framer) Flush() ([]InputEvent, error) {
	if f.inPaste {
		data, err := f.text(f.paste)
		f.Reset()
		if err != nil {
			return nil, err
		}
		return []InputEvent{{Kind: InputPaste, Data: data}}, nil
	}
	if len(f.buf) == 0 {
		return nil, nil
	}
	data, err := f.text(f.buf)
	f.Reset()
	if err != nil {
		return nil, err
	}
	return []InputEvent{{Kind: InputSequence, Data: data}}, nil
}

func (f *Framer) text(raw []byte) (string, error) {
	if utf8.Valid(raw) {
		return string(raw), nil
	}
	if f.utf8Mode == RejectInvalidUTF8 {
		return "", ErrInvalidUTF8
	}
	return string(bytes.ToValidUTF8(raw, []byte(string(utf8.RuneError)))), nil
}

func (f *Framer) emit(out []InputEvent, s string) []InputEvent {
	// Kitty can report an unmodified printable key and terminals such as
	// WezTerm may also send the raw rune. Suppress only that exact duplicate.
	if r, ok := kittyPrintable(s); ok {
		f.pendingCP = r
	} else if r, n := utf8.DecodeRuneInString(s); n == len(s) && r == f.pendingCP {
		f.pendingCP = 0
		return out
	} else {
		f.pendingCP = 0
	}
	ev := InputEvent{Kind: InputSequence, Data: s}
	switch s {
	case "\x1b[I":
		ev.Kind = InputFocusIn
	case "\x1b[O":
		ev.Kind = InputFocusOut
	default:
		if mouse, ok := ParseMouse(s); ok {
			ev.Kind, ev.Mouse = InputMouse, &mouse
		}
	}
	return append(out, ev)
}

// ParseMouse supports SGR 1006 and legacy X10 reporting. Coordinates are
// normalized to zero-based cells and malformed reports are never guessed.
func ParseMouse(s string) (MouseEvent, bool) {
	if strings.HasPrefix(s, "\x1b[<") && len(s) > 5 && (s[len(s)-1] == 'M' || s[len(s)-1] == 'm') {
		var code, x, y int
		if _, err := fmt.Sscanf(s, "\x1b[<%d;%d;%d", &code, &x, &y); err != nil || x < 1 || y < 1 {
			return MouseEvent{}, false
		}
		return mouseFromCode(code, x-1, y-1, s[len(s)-1] == 'M')
	}
	if len(s) == 6 && strings.HasPrefix(s, "\x1b[M") {
		return mouseFromCode(int(s[3])-32, int(s[4])-33, int(s[5])-33, true)
	}
	return MouseEvent{}, false
}

func mouseFromCode(code, x, y int, press bool) (MouseEvent, bool) {
	if code < 0 || x < 0 || y < 0 {
		return MouseEvent{}, false
	}
	m := MouseEvent{Button: code & 3, X: x, Y: y, Press: press, Motion: code&32 != 0}
	if code&4 != 0 {
		m.Modifiers |= Shift
	}
	if code&8 != 0 {
		m.Modifiers |= Alt
	}
	if code&16 != 0 {
		m.Modifiers |= Ctrl
	}
	return m, true
}

func kittyPrintable(s string) (rune, bool) {
	var cp int
	if _, err := fmt.Sscanf(s, "\x1b[%d", &cp); err != nil || cp < 32 {
		return 0, false
	}
	if len(s) < 4 || s[len(s)-1] != 'u' {
		return 0, false
	}
	// Only plain CSI-u (or alternate key fields), never a modified event.
	if bytes.Contains([]byte(s), []byte(";")) {
		return 0, false
	}
	return rune(cp), utf8.ValidRune(rune(cp))
}

func escapeLength(b []byte) (int, bool) {
	if len(b) == 1 {
		return 0, false
	}
	switch b[1] {
	case '[':
		if len(b) >= 3 && b[2] == 'M' {
			if len(b) >= 6 {
				return 6, true
			}
			return 0, false
		}
		for i := 2; i < len(b); i++ {
			if b[i] >= 0x40 && b[i] <= 0x7e {
				if b[2] == '<' && (b[i] != 'M' && b[i] != 'm') {
					return 0, false
				}
				return i + 1, true
			}
		}
		return 0, false
	case ']', 'P', '_':
		for i := 2; i < len(b); i++ {
			if b[i] == 7 {
				return i + 1, true
			}
			if b[i] == esc && i+1 < len(b) && b[i+1] == '\\' {
				return i + 2, true
			}
		}
		return 0, false
	case 'O':
		if len(b) >= 3 {
			return 3, true
		}
		return 0, false
	default:
		return 2, true
	}
}

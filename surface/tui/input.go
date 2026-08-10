// Package tui contains the terminal-facing primitives shared by future
// interactive components.  It deliberately owns bytes and terminal state, not
// editor state, layout, or rendering.
package tui

import (
	"bytes"
	"errors"
	"strconv"
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
	Button int
	X, Y   int // zero-based terminal cells
	Press  bool
	Motion bool
	// Scroll is +1 for wheel up and -1 for wheel down. Wheel events use
	// Button=-1 and never masquerade as a primary mouse button.
	Scroll    int
	Modifiers Modifiers
}

func ResizeEvent(width, height int) InputEvent {
	return InputEvent{Kind: InputResize, Width: width, Height: height}
}

type FramerOptions struct {
	// MaxBufferedBytes bounds incomplete UTF-8/control data and paste content.
	// Non-positive values use 1 MiB. Complete ordinary text is emitted without
	// counting the caller-owned chunk against this limit.
	MaxBufferedBytes int
	InvalidUTF8      InvalidUTF8Policy
}

// Framer turns arbitrary stdin chunks into atomic key/control sequences. Feed
// never starts goroutines or timers: callers choose when to Flush incomplete
// input, making cancellation and timeout ownership explicit.
type Framer struct {
	buf        []byte
	paste      []byte
	pasteTerm  []byte
	inPaste    bool
	max        int
	utf8Mode   InvalidUTF8Policy
	pendingCP  rune
	invalidRun bool
}

func NewFramer(opts FramerOptions) *Framer {
	max := opts.MaxBufferedBytes
	if max <= 0 {
		max = 1 << 20
	}
	return &Framer{max: max, utf8Mode: opts.InvalidUTF8}
}

func (f *Framer) Buffered() int { return len(f.buf) + len(f.paste) }

func (f *Framer) Reset() {
	f.buf, f.paste, f.pasteTerm, f.inPaste, f.pendingCP, f.invalidRun = nil, nil, nil, false, 0, false
}

func (f *Framer) Feed(chunk []byte) ([]InputEvent, error) {
	if len(chunk) == 0 {
		return nil, nil
	}
	var out []InputEvent
	for len(chunk) > 0 {
		if f.inPaste {
			var n int
			var err error
			out, n, err = f.consumePaste(out, chunk)
			chunk = chunk[n:]
			if err != nil {
				return out, err
			}
			continue
		}

		room := f.max - len(f.buf)
		if room == 0 {
			var released bool
			var err error
			out, released, err = f.releaseBoundedPrefix(out, chunk)
			if err != nil {
				f.Reset()
				return out, err
			}
			if released {
				continue
			}
			f.Reset()
			return out, ErrInputTooLarge
		}
		n := min(room, len(chunk))
		f.buf = append(f.buf, chunk[:n]...)
		chunk = chunk[n:]
		var err error
		out, err = f.drainNormal(out)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// FeedTo is the bounded-allocation streaming form of Feed. It advances one
// input byte at a time, so a sink failure never consumes later input from the
// same chunk and no result slice scales with chunk size. Feed remains convenient
// for tests and callers that explicitly want a slice.
func (f *Framer) FeedTo(chunk []byte, emit func(InputEvent) error) error {
	if emit == nil {
		return errors.New("tui: nil input event sink")
	}
	for len(chunk) > 0 {
		events, feedErr := f.Feed(chunk[:1])
		for _, event := range events {
			if err := emit(event); err != nil {
				return err
			}
		}
		if feedErr != nil {
			return feedErr
		}
		chunk = chunk[1:]
	}
	return nil
}

// consumePaste retains only paste content against max. A possible suffix of
// the closing marker lives in pasteTerm, a fixed-size protocol overhead, so an
// exactly-maximal paste can still be closed without being rejected.
func (f *Framer) consumePaste(out []InputEvent, raw []byte) ([]InputEvent, int, error) {
	end := []byte(pasteEnd)
	for i, c := range raw {
		f.pasteTerm = append(f.pasteTerm, c)
		for len(f.pasteTerm) > 0 && !bytes.HasPrefix(end, f.pasteTerm) {
			if len(f.paste) == f.max {
				f.Reset()
				return out, i + 1, ErrInputTooLarge
			}
			f.paste = append(f.paste, f.pasteTerm[0])
			f.pasteTerm = f.pasteTerm[1:]
		}
		if bytes.Equal(f.pasteTerm, end) {
			data, err := f.text(f.paste)
			if err != nil {
				f.Reset()
				return out, i + 1, err
			}
			out = append(out, InputEvent{Kind: InputPaste, Data: data})
			f.paste, f.pasteTerm, f.inPaste = nil, nil, false
			return out, i + 1, nil
		}
	}
	return out, len(raw), nil
}

func (f *Framer) drainNormal(out []InputEvent) ([]InputEvent, error) {
	for len(f.buf) > 0 {
		if bytes.HasPrefix(f.buf, []byte(pasteOpen)) {
			f.invalidRun = false
			rest := append([]byte(nil), f.buf[len(pasteOpen):]...)
			f.buf = nil
			f.inPaste = true
			var consumed int
			var err error
			out, consumed, err = f.consumePaste(out, rest)
			if err != nil {
				return out, err
			}
			if consumed == len(rest) {
				return out, nil
			}
			f.buf = append(f.buf, rest[consumed:]...)
			continue
		}
		if f.buf[0] == esc {
			f.invalidRun = false
			// WezTerm can frame an Alt/escape prefix immediately before a full
			// CSI/OSC/SS3 string. The first ESC is its own event; the second is
			// the introducer. Two ESC bytes alone remain one legacy sequence.
			if len(f.buf) >= 2 && f.buf[1] == esc {
				if len(f.buf) == 2 {
					return out, nil
				}
				if escapeIntroducer(f.buf[2]) {
					var err error
					out, err = f.emitRaw(out, f.buf[:1])
					if err != nil {
						f.Reset()
						return out, err
					}
					f.buf = f.buf[1:]
					continue
				}
			}
			n, complete := escapeLength(f.buf)
			if !complete {
				return out, nil
			}
			var err error
			out, err = f.emitRaw(out, f.buf[:n])
			if err != nil {
				f.Reset()
				return out, err
			}
			f.buf = f.buf[n:]
			continue
		}
		r, n := utf8.DecodeRune(f.buf)
		if r == utf8.RuneError && n == 1 && !utf8.FullRune(f.buf) {
			return out, nil
		}
		if r == utf8.RuneError && n == 1 && f.utf8Mode == ReplaceInvalidUTF8 {
			if !f.invalidRun {
				var err error
				out, err = f.emitRaw(out, f.buf[:1])
				if err != nil {
					f.Reset()
					return out, err
				}
			}
			f.invalidRun = true
			f.buf = f.buf[1:]
			continue
		}
		f.invalidRun = false
		var err error
		out, err = f.emitRaw(out, f.buf[:n])
		if err != nil {
			f.Reset()
			return out, err
		}
		f.buf = f.buf[n:]
	}
	f.buf = nil
	return out, nil
}

func escapeIntroducer(c byte) bool {
	return c == '[' || c == ']' || c == 'O' || c == 'P' || c == '_'
}

// releaseBoundedPrefix uses one byte of lookahead to split an exactly-full
// double-ESC prefix without allowing genuinely pending data to grow past max.
func (f *Framer) releaseBoundedPrefix(out []InputEvent, next []byte) ([]InputEvent, bool, error) {
	if len(next) == 0 {
		return out, false, nil
	}
	if len(f.buf) == 2 && f.buf[0] == esc && f.buf[1] == esc && escapeIntroducer(next[0]) {
		var err error
		out, err = f.emitRaw(out, f.buf[:1])
		if err != nil {
			return out, false, err
		}
		f.buf = f.buf[1:]
		return out, true, nil
	}
	return out, false, nil
}

// Flush reports a partial control sequence or unfinished paste as a normal
// sequence/paste according to the UTF-8 policy. It is safe to call on EOF.
func (f *Framer) Flush() ([]InputEvent, error) {
	if f.inPaste {
		raw := make([]byte, 0, len(f.paste)+len(f.pasteTerm))
		raw = append(raw, f.paste...)
		raw = append(raw, f.pasteTerm...)
		data, err := f.text(raw)
		f.Reset()
		if err != nil {
			return nil, err
		}
		return []InputEvent{{Kind: InputPaste, Data: data}}, nil
	}
	if len(f.buf) == 0 {
		f.invalidRun = false
		return nil, nil
	}
	if f.invalidRun {
		// drainNormal only leaves bytes here when a UTF-8 prefix is
		// incomplete. At EOF it belongs to the already-replaced invalid run.
		f.Reset()
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
	// Replacement mode emits one U+FFFD for each maximal run of malformed
	// bytes. It applies equally to printable data and completed control strings.
	return string(bytes.ToValidUTF8(raw, []byte(string(utf8.RuneError)))), nil
}

func (f *Framer) emitRaw(out []InputEvent, raw []byte) ([]InputEvent, error) {
	s, err := f.text(raw)
	if err != nil {
		return out, err
	}
	// Kitty can report an unmodified printable key and terminals such as
	// WezTerm may also send the raw rune. Suppress only that exact duplicate.
	if r, ok := kittyPrintable(s); ok {
		f.pendingCP = r
	} else if r, n := utf8.DecodeRuneInString(s); n == len(s) && r == f.pendingCP {
		f.pendingCP = 0
		return out, nil
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
	return append(out, ev), nil
}

// ParseMouse supports SGR 1006 and legacy X10 reporting. Coordinates are
// normalized to zero-based cells and malformed reports are never guessed.
func ParseMouse(s string) (MouseEvent, bool) {
	if strings.HasPrefix(s, "\x1b[<") && len(s) > 5 && (s[len(s)-1] == 'M' || s[len(s)-1] == 'm') {
		fields := strings.Split(s[3:len(s)-1], ";")
		if len(fields) != 3 {
			return MouseEvent{}, false
		}
		code, ok1 := strictDecimal(fields[0])
		x, ok2 := strictDecimal(fields[1])
		y, ok3 := strictDecimal(fields[2])
		if !ok1 || !ok2 || !ok3 || x < 1 || y < 1 {
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
	const supported = 3 | 4 | 8 | 16 | 32 | 64
	if code < 0 || code & ^supported != 0 || x < 0 || y < 0 {
		return MouseEvent{}, false
	}
	button := code & 3
	if code&64 != 0 {
		if code&32 != 0 || button > 1 || !press {
			return MouseEvent{}, false
		}
		delta := 1
		if button == 1 {
			delta = -1
		}
		return MouseEvent{Button: -1, X: x, Y: y, Press: true, Scroll: delta, Modifiers: mouseModifiers(code)}, true
	}
	m := MouseEvent{Button: button, X: x, Y: y, Press: press && button != 3, Motion: code&32 != 0, Modifiers: mouseModifiers(code)}
	if button == 3 {
		m.Button = -1
	}
	return m, true
}

func mouseModifiers(code int) Modifiers {
	var modifiers Modifiers
	if code&4 != 0 {
		modifiers |= Shift
	}
	if code&8 != 0 {
		modifiers |= Alt
	}
	if code&16 != 0 {
		modifiers |= Ctrl
	}
	return modifiers
}

func kittyPrintable(s string) (rune, bool) {
	if !strings.HasPrefix(s, "\x1b[") || !strings.HasSuffix(s, "u") || strings.Contains(s, ";") {
		return 0, false
	}
	body := s[2 : len(s)-1]
	fields := strings.Split(body, ":")
	if len(fields) > 3 || fields[0] == "" {
		return 0, false
	}
	cp, ok := strictDecimal(fields[0])
	if !ok || cp < 32 || !utf8.ValidRune(rune(cp)) {
		return 0, false
	}
	for _, field := range fields[1:] {
		if field != "" {
			if _, ok := strictDecimal(field); !ok {
				return 0, false
			}
		}
	}
	return rune(cp), true
}

func strictDecimal(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
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

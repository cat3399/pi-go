package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/term"
)

var (
	ErrTerminalStarted        = errors.New("tui: terminal already started")
	ErrTerminalCleanupPending = errors.New("tui: terminal cleanup is pending")
)

type RGB struct{ R, G, B uint8 }
type ColorScheme string

const (
	Dark  ColorScheme = "dark"
	Light ColorScheme = "light"
)

func ParseOSC11(s string) (RGB, bool) {
	if !strings.HasPrefix(s, "\x1b]11;") || !(strings.HasSuffix(s, "\a") || strings.HasSuffix(s, "\x1b\\")) {
		return RGB{}, false
	}
	v := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(s, "\x1b]11;"), "\a"), "\x1b\\")
	if strings.HasPrefix(v, "#") {
		h := strings.TrimPrefix(v, "#")
		if len(h) == 6 {
			n, e := strconv.ParseUint(h, 16, 32)
			if e == nil {
				return RGB{uint8(n >> 16), uint8(n >> 8), uint8(n)}, true
			}
		}
		if len(h) == 12 {
			return parseChannels(h[0:4], h[4:8], h[8:12])
		}
	}
	v = strings.TrimPrefix(strings.TrimPrefix(v, "rgb:"), "rgba:")
	p := strings.Split(v, "/")
	if len(p) != 3 {
		return RGB{}, false
	}
	return parseChannels(p[0], p[1], p[2])
}
func parseChannels(a, b, c string) (RGB, bool) {
	xs := []string{a, b, c}
	var out RGB
	v := []*uint8{&out.R, &out.G, &out.B}
	for i, x := range xs {
		if x == "" || len(x) > 8 {
			return RGB{}, false
		}
		n, e := strconv.ParseUint(x, 16, 64)
		if e != nil {
			return RGB{}, false
		}
		max := (uint64(1) << uint(len(x)*4)) - 1
		*v[i] = uint8((n*255 + max/2) / max)
	}
	return out, true
}
func ParseColorSchemeReport(s string) (ColorScheme, bool) {
	if s == "\x1b[?997;1n" {
		return Dark, true
	}
	if s == "\x1b[?997;2n" {
		return Light, true
	}
	return "", false
}
func ParseKeyboardNegotiation(s string) (kitty bool, flags int, deviceAttributes bool, ok bool) {
	if strings.HasPrefix(s, "\x1b[?") && strings.HasSuffix(s, "u") {
		n, valid := strictDecimal(strings.TrimSuffix(strings.TrimPrefix(s, "\x1b[?"), "u"))
		return true, n, false, valid
	}
	if strings.HasPrefix(s, "\x1b[?") && strings.HasSuffix(s, "c") {
		body := strings.TrimSuffix(strings.TrimPrefix(s, "\x1b[?"), "c")
		for i := range len(body) {
			if (body[i] < '0' || body[i] > '9') && body[i] != ';' {
				return false, 0, false, false
			}
		}
		return false, 0, true, true
	}
	return false, 0, false, false
}

func Dimensions(columns, rows int, env func(string) string) (int, int) {
	if columns <= 0 {
		columns, _ = strconv.Atoi(env("COLUMNS"))
	}
	if rows <= 0 {
		rows, _ = strconv.Atoi(env("LINES"))
	}
	if columns <= 0 {
		columns = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return columns, rows
}

type RawMode interface {
	MakeRaw(fd int) (restore func() error, err error)
}
type systemRawMode struct{}

func (systemRawMode) MakeRaw(fd int) (func() error, error) {
	state, e := term.MakeRaw(fd)
	if e != nil {
		return nil, e
	}
	return func() error { return term.Restore(fd, state) }, nil
}

// Terminal owns raw-mode and escape-mode restoration. It is deliberately
// synchronous: callers own input pumping and can cancel without an orphaned
// reader goroutine. Stop is idempotent and always attempts every restoration.
type Terminal struct {
	In             *os.File
	Out            io.Writer
	Raw            RawMode
	mu             sync.Mutex
	restore        func() error
	started        bool
	paste          bool
	keyboardPushed bool
	kitty          bool
	modifyOther    bool
}

func NewTerminal(in *os.File, out io.Writer) *Terminal {
	return &Terminal{In: in, Out: out, Raw: systemRawMode{}}
}
func (t *Terminal) Start() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started {
		return ErrTerminalStarted
	}
	if t.restore != nil || t.paste || t.keyboardPushed || t.modifyOther {
		return ErrTerminalCleanupPending
	}
	if t.In == nil || t.Out == nil {
		return errors.New("tui: terminal requires input and output")
	}
	raw := t.Raw
	if raw == nil {
		raw = systemRawMode{}
	}
	restore, e := raw.MakeRaw(int(t.In.Fd()))
	if e != nil {
		var restoreErr error
		if restore != nil {
			restoreErr = restore()
			if restoreErr != nil {
				t.restore = restore
			}
		}
		return errors.Join(fmt.Errorf("tui: raw mode: %w", e), wrapRestoreError(restoreErr))
	}
	if restore == nil {
		return errors.New("tui: raw mode returned no restore function")
	}
	t.restore = restore
	t.started = true
	t.paste = true
	e = t.write("\x1b[?2004h")
	if e == nil {
		t.keyboardPushed = true
		e = t.write("\x1b[>7u")
	}
	if e == nil {
		e = t.write("\x1b[?u")
	}
	if e == nil {
		e = t.write("\x1b[c")
	}
	if e != nil {
		return errors.Join(fmt.Errorf("tui: enable terminal modes: %w", e), t.stopLocked())
	}
	return nil
}
func (t *Terminal) HandleNegotiation(sequence string) (bool, error) {
	kitty, flags, da, ok := ParseKeyboardNegotiation(sequence)
	if !ok {
		return false, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.started {
		return false, nil
	}
	if kitty && flags != 0 {
		var err error
		if t.modifyOther {
			err = t.write("\x1b[>4;0m")
			if err == nil {
				t.modifyOther = false
			}
		}
		t.kitty = true
		return true, err
	}
	if da || kitty {
		if !t.kitty && !t.modifyOther {
			t.modifyOther = true
			if err := t.write("\x1b[>4;2m"); err != nil {
				return true, err
			}
		}
		return true, nil
	}
	return false, nil
}
func (t *Terminal) KittyActive() bool { t.mu.Lock(); defer t.mu.Unlock(); return t.kitty }
func (t *Terminal) Write(s string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.started {
		return errors.New("tui: terminal is stopped")
	}
	return t.write(s)
}
func (t *Terminal) write(s string) error {
	if t.Out != nil {
		n, err := io.WriteString(t.Out, s)
		if err != nil {
			return err
		}
		if n != len(s) {
			return io.ErrShortWrite
		}
		return nil
	}
	return errors.New("tui: terminal output is nil")
}
func (t *Terminal) Stop() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopLocked()
}
func (t *Terminal) stopLocked() error {
	if !t.started && !t.paste && !t.keyboardPushed && !t.modifyOther && t.restore == nil {
		return nil
	}
	t.started = false
	var errs []error
	if t.paste {
		if err := t.write("\x1b[?2004l"); err != nil {
			errs = append(errs, fmt.Errorf("disable bracketed paste: %w", err))
		} else {
			t.paste = false
		}
	}
	if t.keyboardPushed {
		if err := t.write("\x1b[<u"); err != nil {
			errs = append(errs, fmt.Errorf("disable kitty keyboard: %w", err))
		} else {
			t.keyboardPushed = false
		}
	}
	if t.modifyOther {
		if err := t.write("\x1b[>4;0m"); err != nil {
			errs = append(errs, fmt.Errorf("disable modifyOtherKeys: %w", err))
		} else {
			t.modifyOther = false
		}
	}
	if t.restore != nil {
		if e := t.restore(); e != nil {
			errs = append(errs, fmt.Errorf("restore raw mode: %w", e))
		} else {
			t.restore = nil
		}
	}
	t.kitty = false
	return errors.Join(errs...)
}

func wrapRestoreError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("tui: restore partial raw mode: %w", err)
}

// Pump reads terminal data until EOF, cancellation, or framing error. Context
// cancellation is observed between reads; callers needing to interrupt a
// blocking OS read should close their owned reader, then Pump returns its EOF.
func Pump(ctx context.Context, r io.Reader, f *Framer, handle func(InputEvent) error) error {
	buf := make([]byte, 4096)
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		n, e := r.Read(buf)
		if n > 0 {
			if x := f.FeedTo(buf[:n], handle); x != nil {
				return x
			}
		}
		if e != nil {
			if errors.Is(e, io.EOF) {
				events, x := f.Flush()
				if x != nil {
					return x
				}
				for _, ev := range events {
					if x = handle(ev); x != nil {
						return x
					}
				}
				return nil
			}
			return e
		}
	}
}

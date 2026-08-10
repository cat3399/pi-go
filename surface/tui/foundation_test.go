package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func eventData(events []InputEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Data
	}
	return out
}
func TestFramerBoundariesAndPaste(t *testing.T) {
	f := NewFramer(FramerOptions{MaxBufferedBytes: 128})
	got, e := f.Feed([]byte("a\x1b[<35"))
	if e != nil || !same(eventData(got), []string{"a"}) {
		t.Fatalf("first=%q %v", eventData(got), e)
	}
	got, e = f.Feed([]byte(";20;5m\x1b[200~hello\nworld\x1b[201~z"))
	if e != nil {
		t.Fatal(e)
	}
	if len(got) != 3 || got[0].Data != "\x1b[<35;20;5m" || got[1].Kind != InputPaste || got[1].Data != "hello\nworld" || got[2].Data != "z" {
		t.Fatalf("events=%#v", got)
	}
}

func TestFramerClassifiesFocusMouseAndResize(t *testing.T) {
	f := NewFramer(FramerOptions{})
	got, err := f.Feed([]byte("\x1b[I\x1b[<36;3;2M\x1b[O"))
	if err != nil || len(got) != 3 || got[0].Kind != InputFocusIn || got[1].Kind != InputMouse || got[2].Kind != InputFocusOut {
		t.Fatalf("events=%#v err=%v", got, err)
	}
	if m := got[1].Mouse; m == nil || !m.Motion || m.X != 2 || m.Y != 1 || m.Modifiers != Shift {
		t.Fatalf("mouse=%#v", m)
	}
	r := ResizeEvent(100, 40)
	if r.Kind != InputResize || r.Width != 100 || r.Height != 40 {
		t.Fatal(r)
	}
}
func TestFramerPartialEscapeUTF8AndEOF(t *testing.T) {
	f := NewFramer(FramerOptions{})
	if got, _ := f.Feed([]byte("\x1b[")); len(got) != 0 {
		t.Fatal(got)
	}
	got, e := f.Flush()
	if e != nil || !same(eventData(got), []string{"\x1b["}) {
		t.Fatalf("flush=%q %v", eventData(got), e)
	}
	f = NewFramer(FramerOptions{InvalidUTF8: RejectInvalidUTF8})
	_, e = f.Feed([]byte{0xff})
	if !errors.Is(e, ErrInvalidUTF8) {
		t.Fatalf("err=%v", e)
	}
}
func TestFramerKittyAndLimit(t *testing.T) {
	f := NewFramer(FramerOptions{MaxBufferedBytes: 8})
	got, e := f.Feed([]byte("\x1b[97u"))
	if e != nil || !same(eventData(got), []string{"\x1b[97u"}) {
		t.Fatal(got, e)
	}
	got, e = f.Feed([]byte("a"))
	if e != nil || len(got) != 0 {
		t.Fatalf("duplicate=%q %v", eventData(got), e)
	}
	_, e = f.Feed([]byte("\x1b]0123456789"))
	if !errors.Is(e, ErrInputTooLarge) {
		t.Fatalf("limit=%v", e)
	}
}
func TestParseKeyAndMatch(t *testing.T) {
	cases := []struct {
		s, id string
		typ   KeyEventType
	}{{"\x03", "ctrl+c", KeyPress}, {"\x1b[13;2u", "shift+enter", KeyPress}, {"\x1b[97;5:3u", "ctrl+a", KeyRelease}, {"\x1b[1;3D", "alt+left", KeyPress}, {"\x1b[27;5;100~", "ctrl+d", KeyPress}}
	for _, tc := range cases {
		ev, ok := ParseKey(tc.s)
		if !ok || !Matches(ev, tc.id) || ev.Type != tc.typ {
			t.Errorf("%q=%+v %v does not match %q", tc.s, ev, ok, tc.id)
		}
	}
}
func TestTextCellsWrapAndSlice(t *testing.T) {
	if got := VisibleWidth("a\x1b[31m好\x1b[0m🙂\t"); got != 8 {
		t.Fatalf("width=%d", got)
	}
	if got := VisibleWidth("🇨"); got != 2 {
		t.Fatalf("regional=%d", got)
	}
	if got := VisibleWidth("e\u0301"); got != 1 {
		t.Fatalf("combining=%d", got)
	}
	tr := Truncate("\x1b[31m你好world", 5, "...", true)
	if got := VisibleWidth(tr); got != 5 {
		t.Fatalf("truncate %q width=%d", tr, got)
	}
	lines := Wrap("ab你好cd", 4)
	if len(lines) != 2 || VisibleWidth(lines[0]) > 4 || VisibleWidth(lines[1]) > 4 {
		t.Fatalf("wrap=%q", lines)
	}
	part, w := SliceColumns("a好b", 1, 2, true)
	if part != "好" || w != 2 {
		t.Fatalf("slice=%q %d", part, w)
	}
}
func TestWordNavigation(t *testing.T) {
	s := "foo...bar 你好"
	if got := WordForward(s, 0); got != 3 {
		t.Fatalf("forward=%d", got)
	}
	if got := WordBackward(s, len(s)); got != len(s)-len("你") {
		t.Fatalf("backward=%d", got)
	}
	if got := WordBackward("  hello  ", 9); got != 2 {
		t.Fatalf("space=%d", got)
	}
}
func TestColorAndDimensions(t *testing.T) {
	rgb, ok := ParseOSC11("\x1b]11;rgb:0000/8000/ffff\a")
	if !ok || rgb != (RGB{0, 128, 255}) {
		t.Fatal(rgb, ok)
	}
	if _, ok := ParseOSC11("x\x1b]11;#ffffff\a"); ok {
		t.Fatal("accepted prefix")
	}
	if s, ok := ParseColorSchemeReport("\x1b[?997;2n"); !ok || s != Light {
		t.Fatal(s, ok)
	}
	c, r := Dimensions(0, 0, func(k string) string {
		if k == "COLUMNS" {
			return "123"
		}
		return "45"
	})
	if c != 123 || r != 45 {
		t.Fatal(c, r)
	}
}

type fakeRaw struct {
	restored int
	err      error
}

func (f *fakeRaw) MakeRaw(int) (func() error, error) {
	return func() error { f.restored++; return f.err }, nil
}
func TestTerminalLifecycleAndNegotiation(t *testing.T) {
	in, err := osPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	var out bytes.Buffer
	raw := &fakeRaw{}
	term := NewTerminal(in, &out)
	term.Raw = raw
	if err := term.Start(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\x1b[?2004h") {
		t.Fatal(out.String())
	}
	handled, err := term.HandleNegotiation("\x1b[?7u")
	if err != nil || !handled || !term.KittyActive() {
		t.Fatal("kitty")
	}
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}
	if raw.restored != 1 || !strings.Contains(out.String(), "\x1b[<u") {
		t.Fatal(raw.restored, out.String())
	}
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}
}
func TestPumpEOFAndCancellation(t *testing.T) {
	f := NewFramer(FramerOptions{})
	var got []InputEvent
	if err := Pump(context.Background(), strings.NewReader("a\x1b[A"), f, func(e InputEvent) error { got = append(got, e); return nil }); err != nil {
		t.Fatal(err)
	}
	if !same(eventData(got), []string{"a", "\x1b[A"}) {
		t.Fatal(got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Pump(ctx, strings.NewReader("x"), NewFramer(FramerOptions{}), func(InputEvent) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func FuzzFramer(f *testing.F) {
	f.Add([]byte("\x1b[200~hi\x1b[201~"))
	f.Add([]byte{0xff, 0x1b, '['})
	f.Fuzz(func(t *testing.T, b []byte) {
		d := NewFramer(FramerOptions{MaxBufferedBytes: 256})
		for len(b) > 0 {
			n := 1
			if len(b) > 3 {
				n = 3
			}
			_, err := d.Feed(b[:n])
			if err != nil && !errors.Is(err, ErrInputTooLarge) && !errors.Is(err, ErrInvalidUTF8) {
				t.Fatal(err)
			}
			b = b[n:]
		}
		_, _ = d.Flush()
	})
}
func same(a, b []string) bool { return strings.Join(a, "\x00") == strings.Join(b, "\x00") }
func osPipe() (*os.File, error) {
	r, w, e := os.Pipe()
	if e != nil {
		return nil, e
	}
	_ = w.Close()
	return r, nil
}

var _ io.Reader = (*strings.Reader)(nil)

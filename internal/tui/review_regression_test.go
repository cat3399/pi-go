package tui

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

func TestReviewFramerStreamsLargePlainChunksWithinPendingBound(t *testing.T) {
	input := strings.Repeat("a界🙂", 200)
	for _, chunkSize := range []int{1, 2, 7, len(input)} {
		f := NewFramer(FramerOptions{MaxBufferedBytes: 8})
		var got strings.Builder
		for pos := 0; pos < len(input); {
			end := min(len(input), pos+chunkSize)
			events, err := f.Feed([]byte(input[pos:end]))
			if err != nil {
				t.Fatalf("chunk=%d pos=%d: %v", chunkSize, pos, err)
			}
			if f.Buffered() > 8 {
				t.Fatalf("buffer=%d", f.Buffered())
			}
			for _, ev := range events {
				got.WriteString(ev.Data)
			}
			pos = end
		}
		if got.String() != input || f.Buffered() != 0 {
			t.Fatalf("chunk=%d len=%d buffered=%d", chunkSize, got.Len(), f.Buffered())
		}
	}
	f := NewFramer(FramerOptions{MaxBufferedBytes: 16})
	count := 0
	if err := f.FeedTo([]byte(strings.Repeat("x", 1<<20)), func(InputEvent) error { count++; return nil }); err != nil || count != 1<<20 || f.Buffered() != 0 {
		t.Fatalf("stream count=%d buffered=%d err=%v", count, f.Buffered(), err)
	}
}

func TestReviewFramerBoundsOnlyPendingControlAndPaste(t *testing.T) {
	f := NewFramer(FramerOptions{MaxBufferedBytes: 8})
	if _, err := f.Feed([]byte("ordinary text much longer than the bound")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Feed([]byte("\x1b]123456")); err != nil || f.Buffered() != 8 {
		t.Fatalf("exact control buffered=%d err=%v", f.Buffered(), err)
	}
	if _, err := f.Feed([]byte("7")); !errors.Is(err, ErrInputTooLarge) {
		t.Fatalf("over-limit control err=%v", err)
	}
	if f.Buffered() != 0 {
		t.Fatalf("control retained=%d", f.Buffered())
	}
	f = NewFramer(FramerOptions{MaxBufferedBytes: 12})
	var events []InputEvent
	for _, chunk := range [][]byte{[]byte("\x1b[200~abc"), []byte("defghijklm"), []byte("\x1b[201~")} {
		got, err := f.Feed(chunk)
		events = append(events, got...)
		if err != nil {
			if !errors.Is(err, ErrInputTooLarge) {
				t.Fatal(err)
			}
			break
		}
		if f.Buffered() > 12 {
			t.Fatalf("paste retained=%d", f.Buffered())
		}
	}
	if len(events) != 0 || f.Buffered() != 0 {
		t.Fatalf("oversize paste events=%#v retained=%d", events, f.Buffered())
	}
	f = NewFramer(FramerOptions{MaxBufferedBytes: 32})
	for _, chunk := range [][]byte{[]byte("\x1b[200~ab"), []byte("cd\x1b[20"), []byte("1~z")} {
		got, err := f.Feed(chunk)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, got...)
	}
	last := events[len(events)-2:]
	if last[0].Kind != InputPaste || last[0].Data != "abcd" || last[1].Data != "z" {
		t.Fatalf("split paste=%#v", events)
	}
}

func TestReviewWezTermDoubleEscapeKittyFraming(t *testing.T) {
	fixtures := []string{"\x1b\x1b[27;129:3u", "\x1b\x1b[27;1:3u"}
	for _, fixture := range fixtures {
		variants := [][][]byte{
			{[]byte(fixture)},
			{[]byte(fixture[:1]), []byte(fixture[1:])},
			{[]byte(fixture[:2]), []byte(fixture[2:5]), []byte(fixture[5:])},
		}
		var bytewise [][]byte
		for i := range len(fixture) {
			bytewise = append(bytewise, []byte(fixture[i:i+1]))
		}
		variants = append(variants, bytewise)
		for variant, chunks := range variants {
			f := NewFramer(FramerOptions{MaxBufferedBytes: 64})
			var got []InputEvent
			for _, chunk := range chunks {
				events, err := f.Feed(chunk)
				if err != nil {
					t.Fatalf("fixture=%q variant=%d: %v", fixture, variant, err)
				}
				got = append(got, events...)
			}
			want := []string{"\x1b", fixture[1:]}
			if !same(eventData(got), want) || f.Buffered() != 0 {
				t.Fatalf("fixture=%q variant=%d got=%q buffered=%d", fixture, variant, eventData(got), f.Buffered())
			}
		}
	}
	f := NewFramer(FramerOptions{})
	if got, err := f.Feed([]byte("\x1b\x1b")); err != nil || len(got) != 0 {
		t.Fatalf("ambiguous double ESC=%q err=%v", eventData(got), err)
	}
	got, err := f.Flush()
	if err != nil || !same(eventData(got), []string{"\x1b\x1b"}) {
		t.Fatalf("double ESC flush=%q err=%v", eventData(got), err)
	}
}

func TestReviewFramerExactBoundsAndPasteTerminator(t *testing.T) {
	f := NewFramer(FramerOptions{MaxBufferedBytes: 5})
	events, err := f.Feed([]byte("\x1b]12\a"))
	if err != nil || !same(eventData(events), []string{"\x1b]12\a"}) || f.Buffered() != 0 {
		t.Fatalf("exact complete control=%q buffered=%d err=%v", eventData(events), f.Buffered(), err)
	}

	f = NewFramer(FramerOptions{MaxBufferedBytes: 8})
	var got []InputEvent
	for _, chunk := range [][]byte{[]byte("\x1b[200~12345678"), []byte("\x1b[20"), []byte("1~z")} {
		events, err = f.Feed(chunk)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, events...)
		if f.Buffered() > 8 {
			t.Fatalf("paste buffer=%d", f.Buffered())
		}
	}
	if len(got) != 2 || got[0].Kind != InputPaste || got[0].Data != "12345678" || got[1].Data != "z" {
		t.Fatalf("exact paste=%#v", got)
	}

	f = NewFramer(FramerOptions{MaxBufferedBytes: 8})
	if _, err = f.Feed([]byte("\x1b[200~12345678")); err != nil || f.Buffered() != 8 {
		t.Fatalf("exact pending paste buffered=%d err=%v", f.Buffered(), err)
	}
	if _, err = f.Feed([]byte("x")); !errors.Is(err, ErrInputTooLarge) || f.Buffered() != 0 {
		t.Fatalf("over-limit paste buffered=%d err=%v", f.Buffered(), err)
	}

	f = NewFramer(FramerOptions{MaxBufferedBytes: 8})
	if _, err = f.Feed([]byte("\x1b[200~12345678")); err != nil {
		t.Fatal(err)
	}
	got, err = f.Flush()
	if err != nil || len(got) != 1 || got[0].Kind != InputPaste || got[0].Data != "12345678" {
		t.Fatalf("exact paste flush=%#v err=%v", got, err)
	}
}

func TestReviewReplaceInvalidUTF8MaximalRuns(t *testing.T) {
	for variant, chunks := range [][][]byte{
		{{0xff, 0xfe, 'x'}},
		{{0xff}, {0xfe}, {'x'}},
	} {
		f := NewFramer(FramerOptions{InvalidUTF8: ReplaceInvalidUTF8})
		var got strings.Builder
		for _, chunk := range chunks {
			events, err := f.Feed(chunk)
			if err != nil {
				t.Fatalf("variant=%d: %v", variant, err)
			}
			for _, event := range events {
				got.WriteString(event.Data)
			}
		}
		if got.String() != "�x" {
			t.Fatalf("variant=%d got=%q", variant, got.String())
		}
	}
	f := NewFramer(FramerOptions{InvalidUTF8: ReplaceInvalidUTF8})
	events, err := f.Feed([]byte{0xff, 0xfe})
	if err != nil {
		t.Fatal(err)
	}
	flushed, err := f.Flush()
	if err != nil || !same(eventData(events), []string{"�"}) || len(flushed) != 0 {
		t.Fatalf("invalid run=%q flush=%q err=%v", eventData(events), eventData(flushed), err)
	}

	f = NewFramer(FramerOptions{InvalidUTF8: ReplaceInvalidUTF8})
	events, err = f.Feed([]byte{0xff, 'a', 0xfe, 'b'})
	if err != nil || strings.Join(eventData(events), "") != "�a�b" {
		t.Fatalf("separated invalid runs=%q err=%v", eventData(events), err)
	}

	f = NewFramer(FramerOptions{InvalidUTF8: ReplaceInvalidUTF8})
	events, err = f.Feed([]byte{0xff, 0xe2})
	if err != nil {
		t.Fatal(err)
	}
	flushed, err = f.Flush()
	if err != nil || strings.Join(eventData(events), "") != "�" || len(flushed) != 0 {
		t.Fatalf("invalid run with incomplete tail=%q flush=%q err=%v", eventData(events), eventData(flushed), err)
	}
}

func TestReviewRejectInvalidUTF8AcrossAllInputClasses(t *testing.T) {
	cases := [][]byte{{0xff}, {0xe2}, {0x1b, '[', '3', '1', ';', 0xff, 'm'}, {0x1b, ']', '0', ';', 0xff, 7}, {0x1b, 'P', 0xff, 0x1b, '\\'}, {0x1b, '_', 0xff, 0x1b, '\\'}}
	for _, input := range cases {
		f := NewFramer(FramerOptions{InvalidUTF8: RejectInvalidUTF8})
		_, err := f.Feed(input)
		if err == nil {
			_, err = f.Flush()
		}
		if !errors.Is(err, ErrInvalidUTF8) {
			t.Errorf("% x: %v", input, err)
		}
	}
	f := NewFramer(FramerOptions{InvalidUTF8: ReplaceInvalidUTF8})
	events, err := f.Feed([]byte{0x1b, ']', '0', ';', 0xff, 0xfe, 7})
	if err != nil || len(events) != 1 || !utf8.ValidString(events[0].Data) || strings.Count(events[0].Data, "�") != 1 {
		t.Fatalf("replacement=%#v err=%v", events, err)
	}
}

func TestReviewKeyGrammarAndCanonicalSpace(t *testing.T) {
	valid := map[string]string{" ": "space", "\x1b ": "alt+space", "\x1b[27;5;100~": "ctrl+d", "\x1b[97;69u": "ctrl+a", "\x1b[1057::99;5u": "ctrl+c", "\x1b[57400u": "1", "\x1b[1;3:2D": "alt+left"}
	for raw, want := range valid {
		ev, ok := ParseKey(raw)
		if !ok || !Matches(ev, want) || !Matches(ev, ev.ID()) {
			t.Errorf("valid %q => %+v ok=%v id=%q want=%q", raw, ev, ok, ev.ID(), want)
		}
	}
	invalid := []string{"\x1b[bogusA", "\x1b[27;x;y~", "\x1b[27;5;x~", "\x1b[1;xA", "\x1b[1;3:4A", "\x1b[97;0u", "\x1b[97;5:4u", "\x1b[1114112u", "\x1b[97ux", "\x1b[3;foo~", "\x1b[3;2;9~"}
	for _, raw := range invalid {
		if ev, ok := ParseKey(raw); ok {
			t.Errorf("invalid %q => %+v", raw, ev)
		}
	}
	ev, _ := ParseKey("A")
	if Matches(ev, "shift+shift+a") {
		t.Fatal("duplicate modifiers accepted")
	}
	for _, raw := range []string{"+", "\x1b[43;5u"} {
		ev, ok := ParseKey(raw)
		if !ok || !Matches(ev, ev.ID()) {
			t.Errorf("plus %q => %+v id=%q", raw, ev, ev.ID())
		}
	}
	for raw, want := range map[string]string{"\x1bOP": "f1", "\x1b[24~": "f12", "\x1b[3$": "shift+delete", "\x1bOa": "ctrl+up", "\x1b\x1f": "ctrl+alt+-"} {
		ev, ok := ParseKey(raw)
		if !ok || !Matches(ev, want) {
			t.Errorf("legacy %q => %+v want=%q", raw, ev, want)
		}
	}
}

func TestReviewMyanmarIndicAndCombiningCellFixtures(t *testing.T) {
	cases := []struct {
		input string
		width int
	}{
		{"र्क", 2},
		{"नेटवर्क", 5},
		{"सर्वाधिकार सुरक्षित। ऑर्डर पर क्लिक करें", 33},
		{"র্ক", 2},
		{"ર્ક", 2},
		{"ର୍କ", 2},
		{"ర్క", 2},
		{"ര്‍ക", 2},
		{"e\u0301", 1},
		{"čřžůú", 5},
		{"שָׁ", 1},
		{"بّ", 1},
		{"རྐ", 1},
		{"ᜠ᜴", 1},
		{"가〮", 2},
		{"가〯", 2},
		{"网络", 4},
		{"ネットワーク", 12},
		{"が", 2},
		{"か\u3099", 2},
		{"ကာ", 2},
		{"ကေ", 2},
		{"က်", 2},
		{"ကျ", 2},
		{"ကြ", 2},
		{"ကဳ", 2},
		{"ကဴ", 2},
		{"ကဵ", 2},
		{"ကး", 2},
		{"ကို", 1},
		{"က္", 1},
		{"ำ", 1},
		{"ຳ", 1},
		{"กำ", 2},
		{"ກຳ", 2},
	}
	for _, tc := range cases {
		if got := VisibleWidth(tc.input); got != tc.width {
			t.Errorf("%q width=%d want=%d", tc.input, got, tc.width)
		}
		wrapped := Wrap(tc.input+"x", tc.width)
		if len(wrapped) == 0 || VisibleWidth(wrapped[0]) > tc.width {
			t.Errorf("%q wrap=%q", tc.input, wrapped)
		}
		part, w := SliceColumns(tc.input+"x", 0, tc.width, true)
		if part != tc.input || w != tc.width {
			t.Errorf("%q slice=%q/%d", tc.input, part, w)
		}
		tr := Truncate(tc.input+"x", tc.width, "", false)
		if StripTerminalSequences(tr) != tc.input {
			t.Errorf("%q truncate=%q", tc.input, tr)
		}
	}
}

func TestReviewWrapWhitespaceEmptyLinesAndANSIState(t *testing.T) {
	if got := Wrap("hello world", 10); !equalStrings(got, []string{"hello", "world"}) {
		t.Fatalf("word wrap=%q", got)
	}
	if got := Wrap("a\n\nb\r\n", 20); !equalStrings(got, []string{"a", "", "b", ""}) {
		t.Fatalf("empty lines=%q", got)
	}
	if got := Wrap("first\nsecond\r\nthird\rfourth", 80); !equalStrings(got, []string{"first", "second", "third", "fourth"}) {
		t.Fatalf("line endings=%q", got)
	}
	cjk := "This is an example 中文汉字测试段落内容中文汉字测试段落内容."
	if got := Wrap(cjk, 40); !equalStrings(got, []string{"This is an example 中文汉字测试段落内容", "中文汉字测试段落内容."}) {
		t.Fatalf("cjk=%q", got)
	}
	red, reset := "\x1b[31m", "\x1b[0m"
	got := Wrap(red+"hello world"+reset, 5)
	if !equalStrings(got, []string{red + "hello", red + "world" + reset}) {
		t.Fatalf("sgr=%q", got)
	}
	underline := "\x1b[4m"
	got = Wrap(underline+"abcdefghij"+"\x1b[24m", 4)
	if len(got) != 3 || !strings.HasSuffix(got[0], "\x1b[24m") || !strings.HasPrefix(got[1], underline) {
		t.Fatalf("underline=%q", got)
	}
	got = Wrap("read this thread "+underline+"https://example.com/very/long/path/that/will/wrap\x1b[24m", 40)
	if len(got) < 2 || strings.Contains(got[0], underline) || !strings.HasPrefix(got[1], underline) {
		t.Fatalf("underline boundary=%q", got)
	}
	blue := "\x1b[44m"
	got = Wrap(blue+"hello world this is blue background text"+reset, 15)
	for i, line := range got {
		if !strings.Contains(line, blue) {
			t.Fatalf("background line %d=%q", i, line)
		}
		if i < len(got)-1 && strings.HasSuffix(line, reset) {
			t.Fatalf("background reset line %d=%q", i, line)
		}
	}
	open, close := "\x1b]8;;https://example.test\x1b\\", "\x1b]8;;\x1b\\"
	got = Wrap(open+"abcdef"+close, 3)
	if len(got) != 2 || !strings.HasSuffix(got[0], close) || !strings.HasPrefix(got[1], open) {
		t.Fatalf("link=%q", got)
	}
}

func TestReviewWrapPreservesIndentationAndBlankWhitespace(t *testing.T) {
	got := Wrap("      - 🇨", 9)
	if !equalStrings(got, []string{"      -", "🇨"}) || VisibleWidth(got[0]) != 7 || VisibleWidth(got[1]) != 2 {
		t.Fatalf("regional indentation=%q widths=%d/%d", got, VisibleWidth(got[0]), VisibleWidth(got[1]))
	}
	if got = Wrap("  hello", 20); !equalStrings(got, []string{"  hello"}) {
		t.Fatalf("leading spaces=%q", got)
	}
	if got = Wrap("   ", 2); !equalStrings(got, []string{"  ", " "}) {
		t.Fatalf("blank whitespace=%q", got)
	}
	if got = Wrap("  a  b  ", 4); strings.Join(got, "|") != "  a|b  " {
		t.Fatalf("break spaces=%q", got)
	}
}

func TestReviewTruncateWideEllipsisBudget(t *testing.T) {
	if got := Truncate("abcdef", 1, "🙂", false); got != "" {
		t.Fatalf("wide ellipsis in one cell=%q", got)
	}
	if got := Truncate("abcdef", 2, "🙂", false); got != "\x1b[0m🙂\x1b[0m" {
		t.Fatalf("wide ellipsis exact=%q", got)
	}
	if got := Truncate("a", 1, "🙂", false); got != "a" {
		t.Fatalf("fitting source=%q", got)
	}
}

func TestReviewZWJDoesNotConsumeTerminalControls(t *testing.T) {
	red, reset := "\x1b[31m", "\x1b[0m"
	s := "👨\u200d" + red + "X" + reset
	if got := StripTerminalSequences(s); got != "👨\u200dX" {
		t.Fatalf("strip=%q", got)
	}
	if got := VisibleWidth(s); got != 3 {
		t.Fatalf("width=%d", got)
	}
	part, width := SliceColumns(s, 2, 1, true)
	if part != red+"X"+reset || width != 1 {
		t.Fatalf("slice=%q/%d", part, width)
	}
	lines := Wrap(s, 2)
	if len(lines) != 2 || StripTerminalSequences(lines[0]) != "👨\u200d" || StripTerminalSequences(lines[1]) != "X" {
		t.Fatalf("wrap=%q", lines)
	}
	if got := VisibleWidth("👩\u200d💻"); got != 2 {
		t.Fatalf("valid ZWJ width=%d", got)
	}
}

func TestReviewSliceColumnsReplaysAndClosesANSIState(t *testing.T) {
	red, reset := "\x1b[31m", "\x1b[0m"
	part, width := SliceColumns(red+"ab"+reset+"c", 1, 1, true)
	if part != red+"b"+reset || width != 1 {
		t.Fatalf("SGR slice=%q/%d", part, width)
	}
	part, width = SliceColumns(red+"ab"+reset+"c", 2, 1, true)
	if part != "c" || width != 1 {
		t.Fatalf("SGR pollution=%q/%d", part, width)
	}

	open, close := "\x1b]8;;https://example.test\x1b\\", "\x1b]8;;\x1b\\"
	part, width = SliceColumns(open+"abc"+close+"x", 1, 1, true)
	if part != open+"b"+close || width != 1 {
		t.Fatalf("OSC8 slice=%q/%d", part, width)
	}
	part, width = SliceColumns(open+"abc"+close+"x", 3, 1, true)
	if part != "x" || width != 1 {
		t.Fatalf("OSC8 pollution=%q/%d", part, width)
	}
}

type partialRaw struct {
	restored            atomic.Int64
	makeErr, restoreErr error
}

func (r *partialRaw) MakeRaw(int) (func() error, error) {
	return func() error { r.restored.Add(1); return r.restoreErr }, r.makeErr
}

func TestReviewTerminalRestoresPartialRawAndRejectsLateNegotiation(t *testing.T) {
	in, err := osPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	makeErr := errors.New("make raw failed")
	restoreErr := errors.New("restore failed")
	raw := &partialRaw{makeErr: makeErr, restoreErr: restoreErr}
	term := NewTerminal(in, &bytes.Buffer{})
	term.Raw = raw
	err = term.Start()
	if !errors.Is(err, makeErr) || !errors.Is(err, restoreErr) || raw.restored.Load() != 1 {
		t.Fatalf("start err=%v restores=%d", err, raw.restored.Load())
	}
	raw.restoreErr = nil
	if err = term.Stop(); err != nil || raw.restored.Load() != 2 {
		t.Fatalf("retry cleanup err=%v restores=%d", err, raw.restored.Load())
	}
	raw = &partialRaw{}
	var out bytes.Buffer
	term = NewTerminal(in, &out)
	term.Raw = raw
	if err = term.Start(); err != nil {
		t.Fatal(err)
	}
	if err = term.Stop(); err != nil {
		t.Fatal(err)
	}
	before := out.String()
	handled, err := term.HandleNegotiation("\x1b[?7u")
	if err != nil || handled || out.String() != before || term.KittyActive() {
		t.Fatalf("late handled=%v err=%v out=%q", handled, err, out.String()[len(before):])
	}
}

func TestReviewTerminalConcurrentLifecycleBalancesModes(t *testing.T) {
	in, err := osPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	raw := &partialRaw{}
	var out bytes.Buffer
	term := NewTerminal(in, &out)
	term.Raw = raw
	for round := 0; round < 30; round++ {
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			err := term.Start()
			if err != nil && !errors.Is(err, ErrTerminalStarted) {
				t.Errorf("start: %v", err)
			}
		}()
		go func() { defer wg.Done(); _, _ = term.HandleNegotiation("\x1b[?0u") }()
		go func() { defer wg.Done(); _ = term.Stop() }()
		wg.Wait()
		_ = term.Stop()
	}
	s := out.String()
	pairs := [][2]string{{"\x1b[?2004h", "\x1b[?2004l"}, {"\x1b[>7u", "\x1b[<u"}, {"\x1b[>4;2m", "\x1b[>4;0m"}}
	for _, pair := range pairs {
		if a, b := strings.Count(s, pair[0]), strings.Count(s, pair[1]); a != b {
			t.Errorf("unbalanced %q=%d %q=%d", pair[0], a, pair[1], b)
		}
	}
}

func TestReviewTerminalFallbackTransitionHasOneDisablePerEnable(t *testing.T) {
	in, err := osPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	var out bytes.Buffer
	term := NewTerminal(in, &out)
	term.Raw = &partialRaw{}
	if err = term.Start(); err != nil {
		t.Fatal(err)
	}
	if handled, e := term.HandleNegotiation("\x1b[?0u"); e != nil || !handled {
		t.Fatalf("fallback handled=%v err=%v", handled, e)
	}
	if handled, e := term.HandleNegotiation("\x1b[?7u"); e != nil || !handled {
		t.Fatalf("kitty handled=%v err=%v", handled, e)
	}
	if err = term.Stop(); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if strings.Count(s, "\x1b[>4;2m") != 1 || strings.Count(s, "\x1b[>4;0m") != 1 || strings.Count(s, "\x1b[>7u") != 1 || strings.Count(s, "\x1b[<u") != 1 {
		t.Fatalf("modes=%q", s)
	}
}

type failCallWriter struct {
	mu            sync.Mutex
	calls, failAt int
	data          bytes.Buffer
}

func (w *failCallWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.calls == w.failAt {
		return 0, errors.New("injected write failure")
	}
	return w.data.Write(p)
}
func (w *failCallWriter) String() string { w.mu.Lock(); defer w.mu.Unlock(); return w.data.String() }

func TestReviewTerminalWriteFailureStillBalancesAttemptedModes(t *testing.T) {
	in, err := osPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	raw := &partialRaw{}
	out := &failCallWriter{failAt: 3}
	term := NewTerminal(in, out)
	term.Raw = raw
	if err = term.Start(); err == nil {
		t.Fatal("start succeeded across query write failure")
	}
	if raw.restored.Load() != 1 {
		t.Fatalf("restore=%d", raw.restored.Load())
	}
	s := out.String()
	if strings.Count(s, "\x1b[?2004h") != strings.Count(s, "\x1b[?2004l") || strings.Count(s, "\x1b[>7u") != strings.Count(s, "\x1b[<u") {
		t.Fatalf("unbalanced output=%q", s)
	}
	out = &failCallWriter{}
	term = NewTerminal(in, out)
	term.Raw = &partialRaw{}
	if err = term.Start(); err != nil {
		t.Fatal(err)
	}
	out.mu.Lock()
	out.failAt = out.calls + 1
	out.mu.Unlock()
	if err = term.Stop(); err == nil {
		t.Fatal("stop hid disable failure")
	}
	if err = term.Start(); !errors.Is(err, ErrTerminalCleanupPending) {
		t.Fatalf("restart err=%v", err)
	}
	out.mu.Lock()
	out.failAt = 0
	out.mu.Unlock()
	if err = term.Stop(); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
	if err = term.Start(); err != nil {
		t.Fatalf("restart after cleanup: %v", err)
	}
	if err = term.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewMouseWheelIsNotAButton(t *testing.T) {
	for raw, delta := range map[string]int{"\x1b[<64;10;20M": 1, "\x1b[<65;10;20M": -1} {
		m, ok := ParseMouse(raw)
		if !ok || m.Scroll != delta || m.Button != -1 || !m.Press {
			t.Errorf("%q => %+v %v", raw, m, ok)
		}
	}
	for _, raw := range []string{"\x1b[<66;10;20M", "\x1b[<64;10;20m", "\x1b[<64;10;20Mx", "\x1b[<64;x;20M"} {
		if m, ok := ParseMouse(raw); ok {
			t.Errorf("accepted %q => %+v", raw, m)
		}
	}
	m, ok := ParseMouse("\x1b[<0;10;20M")
	if !ok || m.Scroll != 0 || m.Button != 0 {
		t.Fatalf("button=%+v %v", m, ok)
	}
	x10 := string([]byte{0x1b, '[', 'M', 32 + 64, 33 + 2, 33 + 3})
	m, ok = ParseMouse(x10)
	if !ok || m.Scroll != 1 || m.X != 2 || m.Y != 3 || m.Button != -1 {
		t.Fatalf("x10 wheel=%+v %v", m, ok)
	}
}

func FuzzReviewStrictKeyAndFramerBound(f *testing.F) {
	f.Add([]byte("\x1b[97;5:3u"))
	f.Add([]byte{0x1b, ']', 0xff, 7})
	f.Fuzz(func(t *testing.T, input []byte) {
		for _, policy := range []InvalidUTF8Policy{ReplaceInvalidUTF8, RejectInvalidUTF8} {
			framer := NewFramer(FramerOptions{MaxBufferedBytes: 64, InvalidUTF8: policy})
			remaining := input
			for len(remaining) > 0 {
				n := min(len(remaining), 5)
				_, err := framer.Feed(remaining[:n])
				if framer.Buffered() > 64 {
					t.Fatalf("buffer=%d", framer.Buffered())
				}
				if err != nil {
					break
				}
				remaining = remaining[n:]
			}
			_, _ = framer.Flush()
		}
		if ev, ok := ParseKey(string(input)); ok && !Matches(ev, ev.ID()) {
			t.Fatalf("noncanonical %+v id=%q", ev, ev.ID())
		}
	})
}

func equalStrings(a, b []string) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

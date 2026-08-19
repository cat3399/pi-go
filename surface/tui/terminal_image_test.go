package tui

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	value := image.NewRGBA(image.Rect(0, 0, 2, 1))
	value.Set(0, 0, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, value); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestTerminalImageCapabilityDetection(t *testing.T) {
	if got := detectTerminalImageProtocol([]string{"TERM=xterm-kitty", "KITTY_WINDOW_ID=1"}); got != terminalImageKitty {
		t.Fatalf("kitty protocol = %d", got)
	}
	if got := detectTerminalImageProtocol([]string{"TERM=xterm-256color", "TERM_PROGRAM=iTerm.app"}); got != terminalImageITerm2 {
		t.Fatalf("iTerm protocol = %d", got)
	}
	if got := detectTerminalImageProtocol([]string{"TERM=tmux-256color", "KITTY_WINDOW_ID=1"}); got != terminalImageNone {
		t.Fatalf("tmux protocol = %d", got)
	}
}

func TestContentRendererEmitsKittyImageAndAccountsForRows(t *testing.T) {
	renderer := newContentRenderer(DefaultTheme())
	renderer.SetImageProtocol(terminalImageKitty)
	data := tinyPNG(t)
	lines := renderer.Render(contentItem{
		Role: contentRoleUser, Title: "You",
		Blocks: []contentBlock{{Kind: contentBlockImage, MediaType: "image/png", ByteSize: len(data), ImageData: data}},
	}, 40)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "\x1b_Ga=T,f=100") || strings.Contains(StripTerminalSequences(joined), "image/png") {
		t.Fatalf("kitty image render = %q", joined)
	}
	for _, line := range lines {
		if VisibleWidth(line) > 40 {
			t.Fatalf("line width = %d", VisibleWidth(line))
		}
	}
}

func TestDecodeAppleScriptClipboardData(t *testing.T) {
	want := tinyPNG(t)
	output := []byte("«data PNGf" + strings.ToUpper(fmtHex(want)) + "»\n")
	got, code, ok := decodeAppleScriptData(output)
	if !ok || code != "PNGf" || !bytes.Equal(got, want) {
		t.Fatalf("decoded code=%q ok=%t bytes=%d", code, ok, len(got))
	}
}

func fmtHex(value []byte) string {
	const alphabet = "0123456789abcdef"
	result := make([]byte, len(value)*2)
	for index, current := range value {
		result[index*2] = alphabet[current>>4]
		result[index*2+1] = alphabet[current&15]
	}
	return string(result)
}

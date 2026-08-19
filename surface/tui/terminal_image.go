package tui

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"math"
	"strings"

	_ "golang.org/x/image/webp"
)

type terminalImageProtocol uint8

const (
	terminalImageNone terminalImageProtocol = iota
	terminalImageKitty
	terminalImageITerm2
)

func detectTerminalImageProtocol(environment []string) terminalImageProtocol {
	term := strings.ToLower(environmentValue(environment, "TERM"))
	termProgram := strings.ToLower(environmentValue(environment, "TERM_PROGRAM"))
	if environmentValue(environment, "TMUX") != "" || strings.HasPrefix(term, "tmux") || strings.HasPrefix(term, "screen") {
		return terminalImageNone
	}
	if environmentValue(environment, "KITTY_WINDOW_ID") != "" || termProgram == "kitty" ||
		termProgram == "ghostty" || strings.Contains(term, "ghostty") ||
		environmentValue(environment, "GHOSTTY_RESOURCES_DIR") != "" ||
		environmentValue(environment, "WEZTERM_PANE") != "" || termProgram == "wezterm" ||
		termProgram == "warpterminal" || environmentValue(environment, "WARP_SESSION_ID") != "" ||
		environmentValue(environment, "WARP_TERMINAL_SESSION_UUID") != "" {
		return terminalImageKitty
	}
	if environmentValue(environment, "ITERM_SESSION_ID") != "" || termProgram == "iterm.app" {
		return terminalImageITerm2
	}
	return terminalImageNone
}

func renderTerminalImage(block contentBlock, width int, protocol terminalImageProtocol) []string {
	if protocol == terminalImageNone || len(block.ImageData) == 0 || width <= 0 {
		return nil
	}
	data := block.ImageData
	if protocol == terminalImageKitty && strings.ToLower(block.MediaType) != "image/png" {
		decoded, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return nil
		}
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, decoded); err != nil {
			return nil
		}
		data = encoded.Bytes()
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return nil
	}
	columns, rows := terminalImageCellSize(config.Width, config.Height, min(60, width), 20)
	payload := base64.StdEncoding.EncodeToString(data)
	switch protocol {
	case terminalImageKitty:
		sequence := encodeKittyImage(payload, columns, rows, stableImageID(data))
		lines := []string{sequence}
		return append(lines, make([]string, rows-1)...)
	case terminalImageITerm2:
		sequence := fmt.Sprintf("\x1b]1337;File=inline=1;width=%d;height=auto:%s\a", columns, payload)
		lines := make([]string, rows)
		if rows > 1 {
			sequence = fmt.Sprintf("\x1b[%dA%s", rows-1, sequence)
		}
		lines[rows-1] = sequence
		return lines
	default:
		return nil
	}
}

func terminalImageCellSize(pixelWidth, pixelHeight, maxColumns, maxRows int) (int, int) {
	maxColumns = max(1, maxColumns)
	maxRows = max(1, maxRows)
	const cellWidth, cellHeight = 9.0, 18.0
	widthScale := float64(maxColumns) * cellWidth / float64(pixelWidth)
	heightScale := float64(maxRows) * cellHeight / float64(pixelHeight)
	scale := math.Min(widthScale, heightScale)
	columns := int(math.Ceil(float64(pixelWidth) * scale / cellWidth))
	rows := int(math.Ceil(float64(pixelHeight) * scale / cellHeight))
	return max(1, min(maxColumns, columns)), max(1, min(maxRows, rows))
}

func encodeKittyImage(payload string, columns, rows int, imageID uint32) string {
	const chunkSize = 4096
	parameters := fmt.Sprintf("a=T,f=100,q=2,C=1,c=%d,r=%d,i=%d", columns, rows, imageID)
	if len(payload) <= chunkSize {
		return "\x1b_G" + parameters + ";" + payload + "\x1b\\"
	}
	var encoded strings.Builder
	for offset := 0; offset < len(payload); offset += chunkSize {
		end := min(len(payload), offset+chunkSize)
		more := 1
		if end == len(payload) {
			more = 0
		}
		if offset == 0 {
			fmt.Fprintf(&encoded, "\x1b_G%s,m=%d;%s\x1b\\", parameters, more, payload[offset:end])
		} else {
			fmt.Fprintf(&encoded, "\x1b_Gm=%d;%s\x1b\\", more, payload[offset:end])
		}
	}
	return encoded.String()
}

func stableImageID(data []byte) uint32 {
	hash := fnv.New32a()
	_, _ = hash.Write(data)
	value := hash.Sum32()
	if value == 0 {
		return 1
	}
	return value
}

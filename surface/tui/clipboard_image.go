package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

var errNoClipboardImage = errors.New("clipboard does not contain a supported image")

const maxClipboardCommandOutput = 64 << 20

func readSystemClipboardImage(ctx context.Context) (llm.ImageBlock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var data []byte
	var mediaType string
	var err error
	switch runtime.GOOS {
	case "darwin":
		data, mediaType, err = readDarwinClipboardImage(ctx)
	case "linux":
		data, mediaType, err = readLinuxClipboardImage(ctx)
	case "windows":
		data, mediaType, err = readWindowsClipboardImage(ctx, "powershell.exe")
	default:
		err = errNoClipboardImage
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return llm.ImageBlock{}, errors.New("timed out while reading clipboard image")
		}
		return llm.ImageBlock{}, err
	}
	image, createErr := llm.NewImageDataBlock(mediaType, data)
	if createErr != nil {
		return llm.ImageBlock{}, fmt.Errorf("clipboard image is invalid: %w", createErr)
	}
	return image, nil
}

func readDarwinClipboardImage(ctx context.Context) ([]byte, string, error) {
	script := `try
set imageData to the clipboard as «class PNGf»
return imageData
on error
return ""
end try`
	output, err := clipboardCommandOutput(ctx, maxClipboardCommandOutput*2, "osascript", "-e", script)
	if err != nil {
		return nil, "", errNoClipboardImage
	}
	data, code, ok := decodeAppleScriptData(output)
	if !ok || len(data) == 0 {
		return nil, "", errNoClipboardImage
	}
	mediaType := "image/png"
	if strings.EqualFold(code, "JPEG") {
		mediaType = "image/jpeg"
	}
	return data, mediaType, nil
}

func decodeAppleScriptData(output []byte) ([]byte, string, bool) {
	value := strings.TrimSpace(string(output))
	start := strings.Index(value, "«data ")
	end := strings.LastIndex(value, "»")
	if start < 0 || end <= start+10 {
		return nil, "", false
	}
	body := value[start+len("«data ") : end]
	if len(body) < 4 {
		return nil, "", false
	}
	code := body[:4]
	hexText := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, body[4:])
	decoded, err := hex.DecodeString(hexText)
	return decoded, code, err == nil
}

func readLinuxClipboardImage(ctx context.Context) ([]byte, string, error) {
	if os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSLENV") != "" {
		if data, mediaType, err := readWindowsClipboardImage(ctx, "powershell.exe"); err == nil {
			return data, mediaType, nil
		}
	}
	if list, err := clipboardCommandOutput(ctx, 1<<20, "wl-paste", "--list-types"); err == nil {
		if mediaType := preferredClipboardImageType(string(list)); mediaType != "" {
			if data, readErr := clipboardCommandOutput(ctx, maxClipboardCommandOutput, "wl-paste", "--type", mediaType, "--no-newline"); readErr == nil && len(data) != 0 {
				return data, baseImageMediaType(mediaType), nil
			}
		}
	}
	for _, mediaType := range []string{"image/png", "image/jpeg", "image/webp", "image/gif"} {
		data, err := clipboardCommandOutput(ctx, maxClipboardCommandOutput, "xclip", "-selection", "clipboard", "-t", mediaType, "-o")
		if err == nil && len(data) != 0 {
			return data, mediaType, nil
		}
	}
	return nil, "", errNoClipboardImage
}

func readWindowsClipboardImage(ctx context.Context, executable string) ([]byte, string, error) {
	script := `[Console]::OutputEncoding=[Text.Encoding]::ASCII; Add-Type -AssemblyName System.Windows.Forms; Add-Type -AssemblyName System.Drawing; $img=[System.Windows.Forms.Clipboard]::GetImage(); if ($img) { $stream=New-Object IO.MemoryStream; $img.Save($stream,[Drawing.Imaging.ImageFormat]::Png); [Convert]::ToBase64String($stream.ToArray()) }`
	output, err := clipboardCommandOutput(ctx, maxClipboardCommandOutput*2, executable, "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil || len(bytes.TrimSpace(output)) == 0 {
		return nil, "", errNoClipboardImage
	}
	decoded, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(output)))
	if decodeErr != nil || len(decoded) == 0 {
		return nil, "", errNoClipboardImage
	}
	return decoded, "image/png", nil
}

func clipboardCommandOutput(ctx context.Context, limit int64, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return nil, err
	}
	output, readErr := io.ReadAll(io.LimitReader(stdout, limit+1))
	if int64(len(output)) > limit {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, errors.New("clipboard image is too large")
	}
	waitErr := command.Wait()
	if readErr != nil {
		return nil, readErr
	}
	if waitErr != nil {
		return nil, waitErr
	}
	return output, nil
}

func preferredClipboardImageType(types string) string {
	available := strings.Fields(types)
	for _, preferred := range []string{"image/png", "image/jpeg", "image/webp", "image/gif"} {
		for _, candidate := range available {
			if baseImageMediaType(candidate) == preferred {
				return candidate
			}
		}
	}
	return ""
}

func baseImageMediaType(value string) string {
	base, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(value)), ";")
	return strings.TrimSpace(base)
}

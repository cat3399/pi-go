package tool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf8"
)

func (s *FilesystemSuite) read(ctx context.Context, input ReadInput) (ToolResult, error) {
	if err := contextFailure(ctx); err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
	}
	if err := validFilesystemArgument("path", input.Path); err != nil {
		return inputError(err)
	}
	path, err := resolveReadPath(s.workingDir, input.Path)
	if err != nil {
		return ToolResult{}, err
	}
	openFile := s.openReadFile
	if openFile == nil {
		openFile = openRegularReadFile
	}
	// Go cannot forcibly interrupt a filesystem driver's blocked open/stat
	// syscall (notably an unhealthy hard mount). Once a handle is returned, the
	// watcher below closes it on cancellation and is joined before Read returns.
	file, err := openFile(path)
	if err != nil {
		return ToolResult{}, fmt.Errorf("read %s: %w", input.Path, err)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return ToolResult{}, fmt.Errorf("read %s: inspect opened file: %w", input.Path, statErr)
	}
	stopCancellation := watchReadCancellation(ctx, file)
	defer func() {
		stopCancellation()
		_ = file.Close()
	}()

	prefix, _, err := readPrefix(ctx, file, imageSniffBytes)
	if err != nil {
		return readFailure(input.Path, ctx, err)
	}
	if mimeType := detectSupportedImageMIME(prefix); mimeType != "" {
		processed, processErr := processReadImage(
			ctx, file, info.Size(), mimeType, s.autoResizeImages,
			s.maxImagePixels, s.maxImageBytes,
		)
		if err := contextFailure(ctx); err != nil {
			return ToolResult{Text: operationErrorText(err)}, err
		}
		if processErr != nil {
			message := "[Image omitted: could not be resized below the inline image size limit.]"
			if errors.Is(processErr, errImageConversion) {
				message = "[Image omitted: could not be converted to a supported inline image format.]"
			} else if errors.Is(processErr, errImageSafety) {
				message = "[Image omitted: image dimensions exceed the safe in-process decode limit.]"
			} else if errors.Is(processErr, errImageOutput) {
				message = "[Image omitted: encoded image exceeds the inline image size limit.]"
			}
			return ToolResult{Text: fmt.Sprintf("Read image file [%s]\n%s", mimeType, message)}, nil
		}
		result, err := imageToolResult(processed)
		if err != nil {
			return ToolResult{}, err
		}
		if err := contextFailure(ctx); err != nil {
			return ToolResult{Text: operationErrorText(err)}, err
		}
		return result, nil
	}

	reader := io.MultiReader(bytes.NewReader(prefix), file)
	state, err := streamReadText(ctx, reader, input, s.maxLines, s.maxBytes, s.maxTextUnits)
	if err != nil {
		return readFailure(input.Path, ctx, err)
	}
	if state.start >= state.totalFileLines {
		return ToolResult{}, fmt.Errorf("%w: offset %d is beyond end of file (%d lines total)", ErrFilesystemPath, state.start+1, state.totalFileLines)
	}

	truncation := state.truncation()
	output := truncation.Content
	var details map[string]any
	if truncation.Truncated {
		details = map[string]any{"truncation": truncationDetails(truncation, s.maxLines, s.maxBytes)}
		if truncation.FirstLineLarge {
			output = fmt.Sprintf("[Line %d is %s, exceeds %s limit. Use bash: sed -n '%dp' %s | head -c %d]", state.start+1, formatSize(state.firstSelectedLineBytes), formatSize(s.maxBytes), state.start+1, input.Path, s.maxBytes)
		} else {
			end := state.start + truncation.OutputLines
			if truncation.TruncatedBy == "lines" {
				output += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]", state.start+1, end, state.totalFileLines, end+1)
			} else {
				output += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Use offset=%d to continue.]", state.start+1, end, state.totalFileLines, formatSize(s.maxBytes), end+1)
			}
		}
	} else if state.userLimited {
		next := state.start + state.selectedLines + 1
		output += fmt.Sprintf("\n\n[%d more lines in file. Use offset=%d to continue.]", state.totalFileLines-(state.start+state.selectedLines), next)
	}
	if err := contextFailure(ctx); err != nil {
		return ToolResult{Text: operationErrorText(err)}, err
	}
	return ToolResult{Text: output, Details: details}, nil
}

func watchReadCancellation(ctx context.Context, file *os.File) func() {
	if ctx == nil || ctx.Done() == nil {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			_ = file.Close()
		case <-stop:
		}
	}()
	return func() {
		once.Do(func() { close(stop) })
		<-done
	}
}

func readPrefix(ctx context.Context, reader io.Reader, maximum int) ([]byte, bool, error) {
	buffer := make([]byte, 32*1024)
	prefix := make([]byte, 0, maximum)
	for len(prefix) < maximum {
		if err := contextFailure(ctx); err != nil {
			return nil, false, err
		}
		want := minInt(len(buffer), maximum-len(prefix))
		count, err := reader.Read(buffer[:want])
		if count > 0 {
			prefix = append(prefix, buffer[:count]...)
		}
		if errors.Is(err, io.EOF) {
			return prefix, true, nil
		}
		if err != nil {
			return nil, false, err
		}
		if count == 0 {
			return nil, false, io.ErrNoProgress
		}
	}
	return prefix, false, nil
}

func readFailure(path string, ctx context.Context, err error) (ToolResult, error) {
	if cause := context.Cause(ctx); cause != nil || errors.Is(err, ErrOperationCancelled) {
		if cause == nil {
			cause = err
		}
		failure := errors.Join(ErrOperationCancelled, cause)
		return ToolResult{Text: operationErrorText(failure)}, failure
	}
	return ToolResult{}, fmt.Errorf("read %s: %w", path, err)
}

type streamedReadText struct {
	start, lineLimit, maxLines, maxBytes  int
	hasLineLimit                          bool
	totalFileLines, selectedLines         int
	selectedBytes, firstSelectedLineBytes int
	outputLines, outputBytes              int
	output                                []string
	truncatedBy                           string
	firstLineLarge                        bool
	userLimited                           bool

	current          []byte
	currentLineBytes int
	pendingUTF8      []byte
	decodedUnits     int64
	maxTextUnits     int64
	tooLarge         bool
}

func streamReadText(ctx context.Context, reader io.Reader, input ReadInput, maxLines, maxBytes int, maxTextUnits int64) (streamedReadText, error) {
	state := streamedReadText{maxLines: maxLines, maxBytes: maxBytes, maxTextUnits: maxTextUnits}
	if input.Offset != nil {
		state.start = maxInt(0, *input.Offset-1)
	}
	if input.Limit != nil {
		state.hasLineLimit = true
		state.lineLimit = maxInt(0, *input.Limit)
	}
	buffer := make([]byte, 32*1024)
	for {
		if err := contextFailure(ctx); err != nil {
			return streamedReadText{}, err
		}
		count, err := reader.Read(buffer)
		if count > 0 {
			state.decode(buffer[:count], false)
			if state.tooLarge {
				return streamedReadText{}, fmt.Errorf("%w: decoded text exceeds %d UTF-16 units", ErrFilesystemReadTooLarge, maxTextUnits)
			}
		}
		if errors.Is(err, io.EOF) {
			state.decode(nil, true)
			if state.tooLarge {
				return streamedReadText{}, fmt.Errorf("%w: decoded text exceeds %d UTF-16 units", ErrFilesystemReadTooLarge, maxTextUnits)
			}
			state.finishLine()
			state.userLimited = state.hasLineLimit && state.totalFileLines-state.start > state.lineLimit
			return state, nil
		}
		if err != nil {
			return streamedReadText{}, err
		}
		if count == 0 {
			return streamedReadText{}, io.ErrNoProgress
		}
	}
}

func (s *streamedReadText) decode(raw []byte, final bool) {
	combined := make([]byte, 0, len(s.pendingUTF8)+len(raw))
	combined = append(combined, s.pendingUTF8...)
	combined = append(combined, raw...)
	s.pendingUTF8 = nil
	for len(combined) > 0 {
		if !utf8.FullRune(combined) {
			if final {
				s.appendDecodedRune(utf8.RuneError)
			} else {
				s.pendingUTF8 = append([]byte(nil), combined...)
			}
			return
		}
		runeValue, width := utf8.DecodeRune(combined)
		combined = combined[width:]
		s.appendDecodedRune(runeValue)
		if s.tooLarge {
			return
		}
	}
}

func (s *streamedReadText) appendDecodedRune(value rune) {
	units := int64(1)
	if value > 0xffff {
		units = 2
	}
	if s.decodedUnits > s.maxTextUnits-units {
		s.tooLarge = true
		return
	}
	s.decodedUnits += units
	s.appendDecoded([]byte(string(value)))
}

func (s *streamedReadText) appendDecoded(decoded []byte) {
	for _, value := range decoded {
		if value == '\n' {
			s.finishLine()
			continue
		}
		s.currentLineBytes++
		if len(s.current) <= s.maxBytes {
			s.current = append(s.current, value)
		}
	}
}

func (s *streamedReadText) finishLine() {
	lineIndex := s.totalFileLines
	s.totalFileLines++
	selected := lineIndex >= s.start && (!s.hasLineLimit || s.selectedLines < s.lineLimit)
	if selected {
		if s.selectedLines == 0 {
			s.firstSelectedLineBytes = s.currentLineBytes
		} else {
			s.selectedBytes++
		}
		s.selectedBytes += s.currentLineBytes
		s.selectedLines++
		if s.truncatedBy == "" {
			separator := 0
			if s.outputLines > 0 {
				separator = 1
			}
			switch {
			case s.outputLines == 0 && s.currentLineBytes > s.maxBytes:
				s.truncatedBy = "bytes"
				s.firstLineLarge = true
			case s.outputLines >= s.maxLines:
				s.truncatedBy = "lines"
			case s.outputBytes+separator+s.currentLineBytes > s.maxBytes:
				s.truncatedBy = "bytes"
			default:
				s.output = append(s.output, string(s.current))
				s.outputLines++
				s.outputBytes += separator + s.currentLineBytes
			}
		}
	}
	s.current = s.current[:0]
	s.currentLineBytes = 0
}

func (s streamedReadText) truncation() FilesystemTruncation {
	return FilesystemTruncation{
		Content: strings.Join(s.output, "\n"), Truncated: s.truncatedBy != "", TruncatedBy: s.truncatedBy,
		TotalLines: s.selectedLines, TotalBytes: s.selectedBytes, OutputLines: s.outputLines,
		OutputBytes: s.outputBytes, FirstLineLarge: s.firstLineLarge,
	}
}

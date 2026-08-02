package tool

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode/utf8"
)

const (
	DefaultMaxOutputLines = 2000
	DefaultMaxOutputBytes = 50 * 1024
)

type outputSnapshot struct {
	content          string
	truncation       Truncation
	fullOutputPath   string
	lastLineBytes    uint64
	artifactComplete bool
}

type outputAccumulator struct {
	maxLines        int
	maxBytes        int
	maxRollingBytes int
	store           artifactFactory

	pendingUTF8        []byte
	tail               []byte
	tailAtLineBoundary bool
	atStreamStart      bool

	rawChunks              [][]byte
	artifact               artifactFile
	artifactPath           string
	artifactErr            error
	artifactComplete       bool
	totalRawBytes          uint64
	totalDecodedBytes      uint64
	completedLines         uint64
	currentLineBytes       uint64
	lastCompletedLineBytes uint64
	hasOpenLine            bool
	finished               bool
	closed                 bool
}

func newOutputAccumulator(maxLines, maxBytes int, store artifactFactory) (*outputAccumulator, error) {
	if maxLines <= 0 {
		return nil, fmt.Errorf("%w: max output lines must be greater than zero", ErrInvalidBashOptions)
	}
	if maxBytes <= 0 || maxBytes > math.MaxInt/4 {
		return nil, fmt.Errorf("%w: max output bytes is outside the supported range", ErrInvalidBashOptions)
	}
	if store == nil {
		return nil, fmt.Errorf("%w: artifact store is required", ErrInvalidBashOptions)
	}
	return &outputAccumulator{
		maxLines:           maxLines,
		maxBytes:           maxBytes,
		maxRollingBytes:    maxBytes * 2,
		store:              store,
		tailAtLineBoundary: true,
		atStreamStart:      true,
	}, nil
}

func (a *outputAccumulator) append(data []byte) error {
	if a.finished {
		return errors.New("cannot append to a finished output accumulator")
	}
	if a.artifactErr != nil {
		return a.artifactErr
	}
	if len(data) == 0 {
		return nil
	}

	a.totalRawBytes = saturatingAdd(a.totalRawBytes, uint64(len(data)))
	a.appendDecoded(a.decode(data, false))
	if a.artifact != nil {
		if err := writeAll(a.artifact, data); err != nil {
			failure := fmt.Errorf("%w: write %s: %w", ErrArtifactIO, a.artifactPath, err)
			if closeErr := a.artifact.Close(); closeErr != nil {
				failure = errors.Join(failure, fmt.Errorf("%w: close partial %s: %w", ErrArtifactIO, a.artifactPath, closeErr))
			}
			a.artifact = nil
			a.artifactErr = failure
			a.artifactComplete = false
			return failure
		}
		return nil
	}
	a.rawChunks = append(a.rawChunks, bytes.Clone(data))
	if a.shouldPersist() {
		if err := a.ensureArtifact(); err != nil {
			return err
		}
	}
	return nil
}

func (a *outputAccumulator) finish() error {
	if a.finished {
		return nil
	}
	a.finished = true
	a.appendDecoded(a.decode(nil, true))
	if a.shouldPersist() {
		return a.ensureArtifact()
	}
	return nil
}

func (a *outputAccumulator) close() error {
	if a.closed {
		return a.artifactErr
	}
	a.closed = true
	if a.artifact == nil {
		return a.artifactErr
	}
	file := a.artifact
	a.artifact = nil
	if err := file.Close(); err != nil {
		failure := fmt.Errorf("%w: close %s: %w", ErrArtifactIO, a.artifactPath, err)
		a.artifactErr = failure
		a.artifactComplete = false
		return failure
	}
	a.artifactComplete = true
	return nil
}

func (a *outputAccumulator) snapshot() outputSnapshot {
	content := a.snapshotText()
	tail := truncateOutputTail(content, a.maxLines, a.maxBytes)
	truncated := a.totalLines() > uint64(a.maxLines) || a.totalDecodedBytes > uint64(a.maxBytes)
	if truncated {
		tail.truncated = true
		if tail.truncatedBy == 0 {
			if a.totalDecodedBytes > uint64(a.maxBytes) {
				tail.truncatedBy = TruncatedByBytes
			} else {
				tail.truncatedBy = TruncatedByLines
			}
		}
	}
	tail.totalLines = a.totalLines()
	tail.totalBytes = a.totalDecodedBytes
	tail.maxLines = a.maxLines
	tail.maxBytes = a.maxBytes

	return outputSnapshot{
		content:          tailContent(content, a.maxLines, a.maxBytes),
		truncation:       tail,
		fullOutputPath:   a.artifactPath,
		lastLineBytes:    a.lastLogicalLineBytes(),
		artifactComplete: a.artifactPath != "" && a.artifactComplete,
	}
}

func (a *outputAccumulator) decode(data []byte, final bool) string {
	combined := make([]byte, 0, len(a.pendingUTF8)+len(data))
	combined = append(combined, a.pendingUTF8...)
	combined = append(combined, data...)
	a.pendingUTF8 = nil

	var decoded strings.Builder
	for len(combined) > 0 {
		if !utf8.FullRune(combined) {
			if final {
				decoded.WriteRune(utf8.RuneError)
			} else {
				a.pendingUTF8 = bytes.Clone(combined)
			}
			break
		}
		runeValue, width := utf8.DecodeRune(combined)
		combined = combined[width:]
		if a.atStreamStart {
			a.atStreamStart = false
			if runeValue == '\ufeff' {
				continue
			}
		}
		decoded.WriteRune(runeValue)
	}
	return decoded.String()
}

func (a *outputAccumulator) appendDecoded(text string) {
	if text == "" {
		return
	}
	decoded := []byte(text)
	a.totalDecodedBytes = saturatingAdd(a.totalDecodedBytes, uint64(len(decoded)))
	a.tail = append(a.tail, decoded...)
	if len(a.tail) > a.maxRollingBytes*2 {
		a.trimTail()
	}

	segmentStart := 0
	for index, value := range decoded {
		if value != '\n' {
			continue
		}
		lineBytes := uint64(index - segmentStart)
		if segmentStart == 0 {
			lineBytes = saturatingAdd(a.currentLineBytes, lineBytes)
		}
		a.lastCompletedLineBytes = lineBytes
		a.completedLines = saturatingAdd(a.completedLines, 1)
		a.currentLineBytes = 0
		a.hasOpenLine = false
		segmentStart = index + 1
	}
	if segmentStart < len(decoded) {
		a.currentLineBytes = saturatingAdd(a.currentLineBytes, uint64(len(decoded)-segmentStart))
		a.hasOpenLine = true
	}
}

func (a *outputAccumulator) trimTail() {
	if len(a.tail) <= a.maxRollingBytes {
		return
	}
	start := len(a.tail) - a.maxRollingBytes
	for start < len(a.tail) && !utf8.RuneStart(a.tail[start]) {
		start++
	}
	if start > 0 {
		a.tailAtLineBoundary = a.tail[start-1] == '\n'
	}
	a.tail = bytes.Clone(a.tail[start:])
}

func (a *outputAccumulator) snapshotText() string {
	if a.tailAtLineBoundary {
		return string(a.tail)
	}
	firstNewline := bytes.IndexByte(a.tail, '\n')
	if firstNewline < 0 {
		return string(a.tail)
	}
	return string(a.tail[firstNewline+1:])
}

func (a *outputAccumulator) totalLines() uint64 {
	if a.hasOpenLine {
		return saturatingAdd(a.completedLines, 1)
	}
	return a.completedLines
}

func (a *outputAccumulator) lastLogicalLineBytes() uint64 {
	if a.hasOpenLine {
		return a.currentLineBytes
	}
	return a.lastCompletedLineBytes
}

func (a *outputAccumulator) shouldPersist() bool {
	return a.totalDecodedBytes > uint64(a.maxBytes) ||
		a.totalLines() > uint64(a.maxLines)
}

func (a *outputAccumulator) ensureArtifact() error {
	if a.artifactErr != nil {
		return a.artifactErr
	}
	if a.artifact != nil || a.artifactPath != "" {
		return nil
	}
	file, path, err := a.store.create()
	if err != nil {
		a.artifactErr = err
		return err
	}
	for _, chunk := range a.rawChunks {
		if err := writeAll(file, chunk); err != nil {
			failure := fmt.Errorf("%w: write %s: %w", ErrArtifactIO, path, err)
			if closeErr := file.Close(); closeErr != nil {
				failure = errors.Join(failure, fmt.Errorf("%w: close partial %s: %w", ErrArtifactIO, path, closeErr))
			}
			a.artifactErr = failure
			return failure
		}
	}
	a.rawChunks = nil
	a.artifact = file
	a.artifactPath = path
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func truncateOutputTail(content string, maxLines, maxBytes int) Truncation {
	lines := splitOutputLines(content)
	totalBytes := len([]byte(content))
	if len(lines) <= maxLines && totalBytes <= maxBytes {
		return Truncation{
			totalLines:  uint64(len(lines)),
			totalBytes:  uint64(totalBytes),
			outputLines: uint64(len(lines)),
			outputBytes: uint64(totalBytes),
			maxLines:    maxLines,
			maxBytes:    maxBytes,
		}
	}

	output := make([]string, 0, min(len(lines), maxLines))
	outputBytes := 0
	truncatedBy := TruncatedByLines
	lastLinePartial := false
	for index := len(lines) - 1; index >= 0 && len(output) < maxLines; index-- {
		line := lines[index]
		lineBytes := len([]byte(line))
		if len(output) > 0 {
			lineBytes++
		}
		if outputBytes+lineBytes > maxBytes {
			truncatedBy = TruncatedByBytes
			if len(output) == 0 {
				partial := utf8Tail(line, maxBytes)
				output = append(output, partial)
				outputBytes = len([]byte(partial))
				lastLinePartial = true
			}
			break
		}
		output = append(output, line)
		outputBytes += lineBytes
	}
	if len(output) >= maxLines && outputBytes <= maxBytes {
		truncatedBy = TruncatedByLines
	}
	reverseStrings(output)
	joined := strings.Join(output, "\n")
	return Truncation{
		truncated:       true,
		truncatedBy:     truncatedBy,
		totalLines:      uint64(len(lines)),
		totalBytes:      uint64(totalBytes),
		outputLines:     uint64(len(output)),
		outputBytes:     uint64(len([]byte(joined))),
		lastLinePartial: lastLinePartial,
		maxLines:        maxLines,
		maxBytes:        maxBytes,
	}
}

func tailContent(content string, maxLines, maxBytes int) string {
	lines := splitOutputLines(content)
	if len(lines) <= maxLines && len([]byte(content)) <= maxBytes {
		return content
	}
	output := make([]string, 0, min(len(lines), maxLines))
	outputBytes := 0
	for index := len(lines) - 1; index >= 0 && len(output) < maxLines; index-- {
		line := lines[index]
		lineBytes := len([]byte(line))
		if len(output) > 0 {
			lineBytes++
		}
		if outputBytes+lineBytes > maxBytes {
			if len(output) == 0 {
				output = append(output, utf8Tail(line, maxBytes))
			}
			break
		}
		output = append(output, line)
		outputBytes += lineBytes
	}
	reverseStrings(output)
	return strings.Join(output, "\n")
}

func splitOutputLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func utf8Tail(text string, maxBytes int) string {
	data := []byte(text)
	if len(data) <= maxBytes {
		return text
	}
	start := len(data) - maxBytes
	for start < len(data) && !utf8.RuneStart(data[start]) {
		start++
	}
	return string(data[start:])
}

func reverseStrings(values []string) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func saturatingAdd(value, delta uint64) uint64 {
	if math.MaxUint64-value < delta {
		return math.MaxUint64
	}
	return value + delta
}

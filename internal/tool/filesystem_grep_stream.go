package tool

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// openBoundedRegularSearchFile establishes both safety properties needed by
// the pure-Go grep path: only a regular file is admitted, and the scan is
// bounded by the opened file's observed size. A concurrently growing file can
// therefore never turn into an endless input stream. The bytes are consumed
// incrementally; a large regular file is not allocated in memory. As with Read,
// the platform cannot interrupt an open/stat syscall stuck inside a failed hard
// mount; cancellation becomes enforceable as soon as this function returns.
func openBoundedRegularSearchFile(path string) (*os.File, int64, error) {
	file, err := openRegularReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 {
		_ = file.Close()
		return nil, 0, fmt.Errorf("%w: grep only supports finite regular files", ErrUnsupportedFilesystemFeature)
	}
	return file, info.Size(), nil
}

func streamFiniteRegularSearchFile(ctx context.Context, path string, consume func(io.Reader) error) error {
	file, size, err := openBoundedRegularSearchFile(path)
	if err != nil {
		return err
	}
	stopCancellation := watchReadCancellation(ctx, file)
	consumeErr := consume(io.LimitReader(file, size))
	stopCancellation()
	closeErr := file.Close()
	if failure := contextFailure(ctx); failure != nil {
		return failure
	}
	return errors.Join(consumeErr, closeErr)
}

type grepFileResult struct {
	output         *grepOutput
	matches        int
	linesTruncated bool
	binary         bool
}

type grepLineSnapshot struct {
	number    int
	text      string
	truncated bool
}

type grepLineRing struct {
	values []grepLineSnapshot
	start  int
	count  int
}

func newGrepLineRing(limit int) *grepLineRing {
	if limit <= 0 {
		return &grepLineRing{}
	}
	return &grepLineRing{values: make([]grepLineSnapshot, limit)}
}

func (r *grepLineRing) add(line grepLineSnapshot) {
	if len(r.values) == 0 {
		return
	}
	if r.count < len(r.values) {
		r.values[(r.start+r.count)%len(r.values)] = line
		r.count++
		return
	}
	r.values[r.start] = line
	r.start = (r.start + 1) % len(r.values)
}

func (r *grepLineRing) each(yield func(grepLineSnapshot) bool) {
	for index := 0; index < r.count; index++ {
		if !yield(r.values[(r.start+index)%len(r.values)]) {
			return
		}
	}
}

type grepStorageBudget struct{ remaining int }

type grepPendingBlock struct {
	lines          []string
	totalLines     int
	totalTextBytes int
	remaining      int
	linesTruncated bool
	storageClosed  bool
}

func (b *grepPendingBlock) add(path string, line grepLineSnapshot, match bool, budget *grepStorageBudget) {
	separator := '-'
	if match {
		separator = ':'
	}
	formatted := fmt.Sprintf("%s%c%d%c %s", path, separator, line.number, separator, line.text)
	b.totalLines = saturatedIntAdd(b.totalLines, 1)
	b.totalTextBytes = saturatedIntAdd(b.totalTextBytes, len(formatted))
	b.linesTruncated = b.linesTruncated || line.truncated
	if b.storageClosed {
		return
	}
	// Include a conservative byte for the eventual newline. This keeps all
	// queued context blocks plus the retained output below the byte ceiling.
	required := saturatedIntAdd(len(formatted), 1)
	if required > budget.remaining {
		b.storageClosed = true
		return
	}
	b.lines = append(b.lines, formatted)
	budget.remaining -= required
}

func (b *grepPendingBlock) omit(lines int) {
	if lines <= 0 {
		return
	}
	b.totalLines = saturatedIntAdd(b.totalLines, lines)
	b.storageClosed = true
}

func scanGrepRegularFile(
	ctx context.Context,
	file *os.File,
	inputBytes int64,
	path string,
	expression *regexp.Regexp,
	contextLines int,
	matchLimit int,
	maxOutputBytes int,
) (grepFileResult, error) {
	result := grepFileResult{output: newGrepOutput(maxOutputBytes)}
	if inputBytes < 0 {
		return result, fmt.Errorf("invalid negative grep input size")
	}
	if matchLimit <= 0 || inputBytes == 0 {
		return result, nil
	}

	// Pure Go cannot retain an unbounded before-context window. Every formatted
	// row costs at least six bytes including its newline, so rows beyond this
	// bound cannot all fit in the provider-visible result. For an extreme context
	// request this keeps the nearest bounded window (a documented hardening
	// difference from upstream's unbounded readFile context cache).
	effectiveContext := minInt(contextLines, maxOutputBytes/6+1)
	previous := newGrepLineRing(effectiveContext)
	reader := bufio.NewReaderSize(io.LimitReader(file, inputBytes), 32*1024)
	budget := &grepStorageBudget{remaining: maxOutputBytes}
	var pending []*grepPendingBlock
	formatBlocks := true
	trailingEmpty := false

	for lineNumber := 1; ; lineNumber++ {
		if err := contextFailure(ctx); err != nil {
			return grepFileResult{}, err
		}
		line := newGrepLogicalLine(ctx, reader)
		matched := false
		if result.matches < matchLimit {
			matched = expression.MatchReader(line)
		}
		if err := line.drain(); err != nil {
			if failure := contextFailure(ctx); failure != nil {
				return grepFileResult{}, failure
			}
			return grepFileResult{}, err
		}
		if !line.exists {
			if !trailingEmpty {
				break
			}
			// Upstream context formatting uses String.split("\n"), which
			// preserves the final empty element after a line delimiter.
			line.exists = true
			line.fileEOF = true
			trailingEmpty = false
			// ripgrep does not emit a match event for this synthetic cache-only
			// element; it is used solely when formatting context around a real
			// preceding line.
			matched = false
		}
		if line.binary {
			return grepFileResult{output: newGrepOutput(maxOutputBytes), binary: true}, nil
		}
		snapshot := grepLineSnapshot{number: lineNumber, text: line.text(), truncated: line.truncated()}

		if formatBlocks {
			for _, block := range pending {
				if block.remaining > 0 {
					block.add(path, snapshot, false, budget)
					block.remaining--
				}
			}
			pending = flushCompleteGrepBlocks(result.output, pending, &result.linesTruncated)
			if result.output.truncated {
				formatBlocks = false
				pending = nil
			}
		}

		if matched && result.matches < matchLimit {
			result.matches++
			if formatBlocks {
				block := &grepPendingBlock{remaining: effectiveContext}
				previous.each(func(contextLine grepLineSnapshot) bool {
					block.add(path, contextLine, false, budget)
					return !block.storageClosed
				})
				if block.storageClosed && previous.count > block.totalLines {
					block.omit(previous.count - block.totalLines)
				}
				block.add(path, snapshot, true, budget)
				pending = append(pending, block)
				pending = flushCompleteGrepBlocks(result.output, pending, &result.linesTruncated)
				if block.storageClosed || result.output.truncated {
					// The formatted prefix has filled its byte budget. Further
					// context cannot become visible, so discard pending block state
					// while continuing only the bounded match scan.
					for _, queued := range pending {
						result.output.appendBlock(queued)
						result.linesTruncated = result.linesTruncated || queued.linesTruncated
					}
					pending = nil
					formatBlocks = false
				}
			}
		}
		previous.add(snapshot)
		trailingEmpty = line.delimited

		// Match-limit behavior mirrors upstream's killed rg process. With
		// context, read only far enough to finish the last visible block.
		if result.matches >= matchLimit && (!formatBlocks || len(pending) == 0) {
			break
		}
		if line.fileEOF {
			break
		}
	}

	for _, block := range pending {
		result.output.appendBlock(block)
		result.linesTruncated = result.linesTruncated || block.linesTruncated
	}
	return result, nil
}

func flushCompleteGrepBlocks(output *grepOutput, pending []*grepPendingBlock, linesTruncated *bool) []*grepPendingBlock {
	completed := 0
	for completed < len(pending) && pending[completed].remaining == 0 {
		block := pending[completed]
		output.appendBlock(block)
		*linesTruncated = *linesTruncated || block.linesTruncated
		completed++
	}
	if completed == 0 {
		return pending
	}
	copy(pending, pending[completed:])
	return pending[:len(pending)-completed]
}

// grepLogicalLine is an io.RuneReader view over one normalized logical line.
// regexp.MatchReader can therefore evaluate an arbitrarily long line without
// materializing it. Only the provider-visible 500 UTF-16-unit prefix is kept.
type grepLogicalLine struct {
	ctx       context.Context
	reader    *bufio.Reader
	units     []uint16
	total     int
	exists    bool
	binary    bool
	ended     bool
	fileEOF   bool
	delimited bool
	readError error
}

func newGrepLogicalLine(ctx context.Context, reader *bufio.Reader) *grepLogicalLine {
	return &grepLogicalLine{ctx: ctx, reader: reader, units: make([]uint16, 0, DefaultGrepLineRunes)}
}

func (l *grepLogicalLine) ReadRune() (rune, int, error) {
	if l.ended {
		return 0, 0, io.EOF
	}
	if err := contextFailure(l.ctx); err != nil {
		l.ended = true
		l.readError = err
		return 0, 0, err
	}
	value, width, err := l.reader.ReadRune()
	if errors.Is(err, io.EOF) {
		l.ended = true
		l.fileEOF = true
		return 0, 0, io.EOF
	}
	if err != nil {
		l.ended = true
		l.readError = err
		return 0, 0, err
	}
	l.exists = true
	if value == '\n' {
		l.ended = true
		l.delimited = true
		return 0, 0, io.EOF
	}
	if value == '\r' {
		if next, peekErr := l.reader.Peek(1); peekErr == nil && len(next) == 1 && next[0] == '\n' {
			_, _ = l.reader.Discard(1)
		} else if peekErr != nil && !errors.Is(peekErr, io.EOF) {
			l.readError = peekErr
		}
		l.ended = true
		l.delimited = true
		return 0, 0, io.EOF
	}
	if value == 0 {
		l.binary = true
	}
	l.capture(value)
	return value, width, nil
}

func (l *grepLogicalLine) capture(value rune) {
	if value > utf8.MaxRune || value < 0 {
		value = utf8.RuneError
	}
	if value <= 0xffff {
		l.captureUnit(uint16(value))
		return
	}
	first, second := utf16.EncodeRune(value)
	l.captureUnit(uint16(first))
	l.captureUnit(uint16(second))
}

func (l *grepLogicalLine) captureUnit(value uint16) {
	l.total = saturatedIntAdd(l.total, 1)
	if len(l.units) < DefaultGrepLineRunes {
		l.units = append(l.units, value)
	}
}

func (l *grepLogicalLine) drain() error {
	for !l.ended {
		if _, _, err := l.ReadRune(); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	return l.readError
}

func (l *grepLogicalLine) truncated() bool { return l.total > DefaultGrepLineRunes }

func (l *grepLogicalLine) text() string {
	text := string(utf16.Decode(l.units))
	if l.truncated() {
		text += "... [truncated]"
	}
	return text
}

type grepOutput struct {
	maxBytes       int
	lines          []string
	totalLines     int
	totalBytes     int
	outputBytes    int
	truncated      bool
	firstLineLarge bool
}

func newGrepOutput(maxBytes int) *grepOutput { return &grepOutput{maxBytes: maxBytes} }

func (o *grepOutput) addLine(line string) {
	separator := 0
	if o.totalLines > 0 {
		separator = 1
	}
	o.totalLines = saturatedIntAdd(o.totalLines, 1)
	o.totalBytes = saturatedIntAdd(o.totalBytes, saturatedIntAdd(separator, len(line)))
	if o.truncated {
		return
	}
	required := saturatedIntAdd(separator, len(line))
	if required > o.maxBytes-o.outputBytes {
		o.truncated = true
		o.firstLineLarge = len(o.lines) == 0 && len(line) > o.maxBytes
		return
	}
	o.lines = append(o.lines, line)
	o.outputBytes += required
}

func (o *grepOutput) accountOmitted(lines, textBytes int) {
	if lines <= 0 {
		return
	}
	separators := lines
	if o.totalLines == 0 {
		separators--
	}
	o.totalLines = saturatedIntAdd(o.totalLines, lines)
	o.totalBytes = saturatedIntAdd(o.totalBytes, saturatedIntAdd(maxInt(0, separators), textBytes))
	o.truncated = true
}

func (o *grepOutput) appendBlock(block *grepPendingBlock) {
	for _, line := range block.lines {
		o.addLine(line)
	}
	omittedLines := block.totalLines - len(block.lines)
	storedTextBytes := 0
	for _, line := range block.lines {
		storedTextBytes = saturatedIntAdd(storedTextBytes, len(line))
	}
	o.accountOmitted(omittedLines, maxInt(0, block.totalTextBytes-storedTextBytes))
}

func (o *grepOutput) append(other *grepOutput) {
	if other == nil {
		return
	}
	for _, line := range other.lines {
		o.addLine(line)
	}
	omittedLines := other.totalLines - len(other.lines)
	storedTextBytes := 0
	for _, line := range other.lines {
		storedTextBytes = saturatedIntAdd(storedTextBytes, len(line))
	}
	otherTextBytes := other.totalBytes - maxInt(0, other.totalLines-1)
	o.accountOmitted(omittedLines, maxInt(0, otherTextBytes-storedTextBytes))
}

func (o *grepOutput) truncation() FilesystemTruncation {
	return FilesystemTruncation{
		Content: strings.Join(o.lines, "\n"), Truncated: o.truncated, TruncatedBy: "bytes",
		TotalLines: o.totalLines, TotalBytes: o.totalBytes, OutputLines: len(o.lines),
		OutputBytes: o.outputBytes, FirstLineLarge: o.firstLineLarge,
	}
}

func saturatedIntAdd(left, right int) int {
	maximum := int(^uint(0) >> 1)
	if right > 0 && left > maximum-right {
		return maximum
	}
	return left + right
}

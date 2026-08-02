package provider

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

var (
	errResponsesEventTooLarge   = errors.New("OpenAI Responses SSE event exceeds byte limit")
	errResponsesIncompleteFrame = errors.New("OpenAI Responses SSE frame ended before empty-line delimiter")
)

type responsesSSEDecoder struct {
	reader   *bufio.Reader
	maxBytes int
	eof      bool
}

func newResponsesSSEDecoder(reader io.Reader, maxBytes int) *responsesSSEDecoder {
	return &responsesSSEDecoder{
		reader:   bufio.NewReaderSize(reader, min(maxBytes, 64<<10)),
		maxBytes: maxBytes,
	}
}

func (d *responsesSSEDecoder) NextData() ([]byte, error) {
	if d.eof {
		return nil, io.EOF
	}
	var data [][]byte
	used := 0
	for {
		line, err := readResponsesSSELine(d.reader, d.maxBytes-used)
		if err != nil {
			switch {
			case errors.Is(err, io.EOF):
				d.eof = true
				if len(line) != 0 || used != 0 || len(data) != 0 {
					return nil, errResponsesIncompleteFrame
				}
				return nil, io.EOF
			default:
				return nil, err
			}
		}
		used += len(line)
		if used > d.maxBytes {
			return nil, errResponsesEventTooLarge
		}
		if len(line) != 0 {
			field, value := splitResponsesSSEField(line)
			if bytes.Equal(field, []byte("data")) {
				data = append(data, append([]byte(nil), value...))
			}
		}

		switch {
		case len(line) == 0:
			if len(data) != 0 {
				return bytes.Join(data, []byte{'\n'}), nil
			}
			used = 0
		default:
			continue
		}
	}
}

func readResponsesSSELine(reader *bufio.Reader, limit int) ([]byte, error) {
	if limit < 0 {
		return nil, errResponsesEventTooLarge
	}
	line := make([]byte, 0, min(limit, 4096))
	for {
		value, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return line, io.EOF
			}
			return nil, fmt.Errorf("read SSE line: %w", err)
		}
		switch value {
		case '\n':
			return line, nil
		case '\r':
			// SSE accepts CR, LF, and CRLF line endings. Peek preserves a
			// non-LF byte for the next line and EOF still leaves CR as a
			// complete line delimiter.
			if next, peekErr := reader.Peek(1); peekErr == nil && next[0] == '\n' {
				_, _ = reader.Discard(1)
			}
			return line, nil
		default:
			if len(line) >= limit {
				return nil, errResponsesEventTooLarge
			}
			line = append(line, value)
		}
	}
}

func splitResponsesSSEField(line []byte) ([]byte, []byte) {
	if len(line) == 0 || line[0] == ':' {
		return nil, nil
	}
	colon := bytes.IndexByte(line, ':')
	if colon < 0 {
		return line, nil
	}
	value := line[colon+1:]
	if len(value) != 0 && value[0] == ' ' {
		value = value[1:]
	}
	return line[:colon], value
}

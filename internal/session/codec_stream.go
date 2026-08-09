package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const sessionReadBufferSize = 1024 * 1024

// physicalJSONLineReader exposes exactly one physical JSONL record without
// first materializing the complete file (or even a malformed physical line).
// json.Decoder can therefore reject an obviously corrupt sparse line early;
// the unread remainder is drained with bounded memory before the next record.
type physicalJSONLineReader struct {
	reader      *bufio.Reader
	fragment    []byte
	captured    []byte
	capturing   bool
	done        bool
	exists      bool
	terminated  bool
	terminalErr error
	reportedErr bool
}

func (r *physicalJSONLineReader) Read(destination []byte) (int, error) {
	for len(destination) > 0 {
		if len(r.fragment) > 0 {
			written := copy(destination, r.fragment)
			if r.capturing {
				r.captured = append(r.captured, destination[:written]...)
			}
			r.fragment = r.fragment[written:]
			return written, nil
		}
		if r.done {
			if r.terminalErr != nil && !r.reportedErr {
				r.reportedErr = true
				return 0, r.terminalErr
			}
			return 0, io.EOF
		}

		fragment, err := r.reader.ReadSlice('\n')
		if len(fragment) > 0 {
			r.exists = true
		}
		switch {
		case err == nil:
			r.done = true
			r.terminated = true
			fragment = fragment[:len(fragment)-1]
		case errors.Is(err, bufio.ErrBufferFull):
			// Keep reading the same physical line. There is deliberately no line
			// size limit: a valid large record is decoded normally.
		case errors.Is(err, io.EOF):
			r.done = true
		case err != nil:
			r.done = true
			r.terminalErr = err
		}
		r.fragment = fragment
	}
	return 0, nil
}

func (r *physicalJSONLineReader) discardCapture() {
	r.capturing = false
	r.captured = nil
}

func (r *physicalJSONLineReader) drain() error {
	_, err := io.Copy(io.Discard, r)
	return err
}

type streamedJSONLine struct {
	raw        json.RawMessage
	exists     bool
	terminated bool
	blank      bool
	malformed  bool
}

// nextStreamedJSONLine parses one physical line. A successfully decoded raw
// value is the only raw line allocation retained by the caller. Malformed
// records are drained rather than accumulated, which is important for sparse
// or externally damaged files with a very large unterminated tail.
func nextStreamedJSONLine(reader *bufio.Reader) (streamedJSONLine, error) {
	physical := &physicalJSONLineReader{reader: reader, capturing: true}
	decoder := json.NewDecoder(physical)
	var raw json.RawMessage
	err := decoder.Decode(&raw)
	if err != nil {
		physical.discardCapture()
		if drainErr := physical.drain(); drainErr != nil {
			return streamedJSONLine{}, drainErr
		}
		if physical.terminalErr != nil {
			return streamedJSONLine{}, physical.terminalErr
		}
		if errors.Is(err, io.EOF) {
			return streamedJSONLine{exists: physical.exists, terminated: physical.terminated, blank: physical.exists}, nil
		}
		return streamedJSONLine{exists: physical.exists, terminated: physical.terminated, malformed: physical.exists}, nil
	}

	// A JSONL record contains exactly one JSON value. Scan the decoder's
	// buffered suffix and the remainder of the physical line for JSON
	// whitespace instead of decoding a second value: the first non-whitespace
	// byte already proves the line malformed, so even a huge second value can
	// be discarded with bounded memory.
	malformed, trailingErr := drainTrailingJSONWhitespace(decoder.Buffered(), physical)
	if trailingErr != nil {
		return streamedJSONLine{}, trailingErr
	}
	if malformed {
		return streamedJSONLine{exists: physical.exists, terminated: physical.terminated, malformed: true}, nil
	}
	if len(bytes.TrimSpace(physical.captured)) == 0 {
		return streamedJSONLine{exists: physical.exists, terminated: physical.terminated, blank: true}, nil
	}
	// RawMessage contains only the JSON value. Retain the captured physical
	// record instead so leading/trailing JSON whitespace survives RawJSON,
	// Fork, and ExtractBranch byte-for-byte. The newline itself is represented
	// separately by terminated/needsSeparator.
	return streamedJSONLine{raw: json.RawMessage(physical.captured), exists: physical.exists, terminated: physical.terminated}, nil
}

func drainTrailingJSONWhitespace(buffered io.Reader, physical *physicalJSONLineReader) (bool, error) {
	stream := io.MultiReader(buffered, physical)
	malformed := false
	buffer := make([]byte, 32*1024)
	for {
		read, err := stream.Read(buffer)
		if !malformed {
			for _, value := range buffer[:read] {
				if value != ' ' && value != '\t' && value != '\r' && value != '\n' {
					malformed = true
					physical.discardCapture()
					break
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return malformed, nil
		}
		if err != nil {
			return malformed, err
		}
	}
}

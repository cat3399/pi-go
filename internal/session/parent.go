package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// RewriteParentSession atomically changes only the v3 header's parentSession
// field. It preserves every other physical record byte-for-byte, including
// malformed lines tolerated by the compatibility loader.
func RewriteParentSession(ctx context.Context, path, parentPath string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	resolved, err := resolveSessionPath(path)
	if err != nil {
		return err
	}
	if parentPath != "" {
		parentPath, err = filepath.Abs(parentPath)
		if err != nil {
			return fmt.Errorf("%w: resolve parent session path: %v", ErrInvalidSession, err)
		}
		parentPath = filepath.Clean(parentPath)
	}
	claim, err := claimSessionWriter(resolved)
	if err != nil {
		return err
	}
	defer releaseSessionWriter(claim)

	data, err := os.ReadFile(resolved)
	if err != nil {
		return fmt.Errorf("%w: read session for parent rewrite: %v", ErrStorage, err)
	}
	start, end, newline, err := locateSessionHeaderRecord(data)
	if err != nil {
		return err
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(data[start:end]), &header); err != nil || header == nil {
		return fmt.Errorf("%w: decode session header", ErrInvalidSession)
	}
	if parentPath == "" {
		delete(header, "parentSession")
	} else {
		encodedParent, marshalErr := json.Marshal(parentPath)
		if marshalErr != nil {
			return fmt.Errorf("%w: encode parent session path: %v", ErrInvalidSession, marshalErr)
		}
		header["parentSession"] = encodedParent
	}
	encoded, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("%w: encode session header: %v", ErrInvalidSession, err)
	}
	candidate := make([]byte, 0, len(data)-end+start+len(encoded)+len(newline))
	candidate = append(candidate, data[:start]...)
	candidate = append(candidate, encoded...)
	candidate = append(candidate, newline...)
	candidate = append(candidate, data[end+len(newline):]...)
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	replaced, err := (osSessionStorage{}).replace(resolved, candidate)
	if err != nil {
		if replaced {
			return fmt.Errorf("%w: rewrite parent session: %v", ErrDurabilityUnknown, err)
		}
		return fmt.Errorf("%w: rewrite parent session: %v", ErrStorage, err)
	}
	return nil
}

func locateSessionHeaderRecord(data []byte) (start, end int, newline []byte, err error) {
	for offset := 0; offset < len(data); {
		lineEnd := bytes.IndexByte(data[offset:], '\n')
		if lineEnd < 0 {
			lineEnd = len(data)
			newline = nil
		} else {
			lineEnd += offset
			newline = []byte{'\n'}
		}
		line := bytes.TrimSpace(data[offset:lineEnd])
		if len(line) != 0 && json.Valid(line) {
			if _, decodeErr := decodeDiscoverableHeader(line); decodeErr != nil {
				return 0, 0, nil, fmt.Errorf("%w: invalid session header: %v", ErrInvalidSession, decodeErr)
			}
			return offset, lineEnd, newline, nil
		}
		if lineEnd == len(data) {
			break
		}
		offset = lineEnd + 1
	}
	return 0, 0, nil, errors.New("session header not found")
}

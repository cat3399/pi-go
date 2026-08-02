package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"unicode/utf8"
)

const maxProductionConfigBytes = 4 << 20

var errDuplicateJSONField = errors.New("duplicate JSON object field")

func readConfigObject(
	path string,
	label string,
	allowLineComments bool,
	requirePrivate bool,
) (map[string]any, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s %s: %w", label, path, err)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return nil, false, fmt.Errorf("inspect %s %s: %w", label, path, statErr)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, false, fmt.Errorf("read %s %s: path is not a regular file", label, path)
	}
	if requirePrivate && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		_ = file.Close()
		return nil, false, fmt.Errorf("read %s %s: credential file permissions must not grant group or other access", label, path)
	}
	if info.Size() > maxProductionConfigBytes {
		_ = file.Close()
		return nil, false, fmt.Errorf("read %s %s: file exceeds the %d-byte limit", label, path, maxProductionConfigBytes)
	}

	data, readErr := io.ReadAll(io.LimitReader(file, maxProductionConfigBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, false, fmt.Errorf("read %s %s: %w", label, path, readErr)
	}
	if closeErr != nil {
		return nil, false, fmt.Errorf("close %s %s: %w", label, path, closeErr)
	}
	if len(data) > maxProductionConfigBytes {
		return nil, false, fmt.Errorf("read %s %s: file exceeds the %d-byte limit", label, path, maxProductionConfigBytes)
	}
	if !utf8.Valid(data) {
		return nil, false, fmt.Errorf("parse %s %s: file is not valid UTF-8", label, path)
	}
	if allowLineComments {
		data = normalizeJSONWithLineComments(data)
	}

	value, err := decodeStrictJSON(data)
	if err != nil {
		var syntaxError *json.SyntaxError
		if errors.As(err, &syntaxError) {
			return nil, false, fmt.Errorf("parse %s %s: invalid JSON near byte %d", label, path, syntaxError.Offset)
		}
		return nil, false, fmt.Errorf("parse %s %s: %w", label, path, err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("parse %s %s: root must be a JSON object", label, path)
	}
	return object, true, nil
}

func decodeStrictJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeStrictJSONValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("JSON contains trailing values")
		}
		return nil, err
	}
	return value, nil
}

func decodeStrictJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, errors.New("JSON nesting exceeds the supported limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}

	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("JSON object field name is not a string")
			}
			if _, duplicate := object[key]; duplicate {
				return nil, errDuplicateJSONField
			}
			value, err := decodeStrictJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		closing, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if closing != json.Delim('}') {
			return nil, errors.New("JSON object is not terminated")
		}
		return object, nil

	case '[':
		array := make([]any, 0)
		for decoder.More() {
			value, err := decodeStrictJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		closing, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if closing != json.Delim(']') {
			return nil, errors.New("JSON array is not terminated")
		}
		return array, nil

	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}

// normalizeJSONWithLineComments mirrors the fixed upstream models.json
// admission: // comments and trailing commas are accepted, while strings are
// left byte-for-byte intact. Block comments remain invalid JSON.
func normalizeJSONWithLineComments(data []byte) []byte {
	normalized := append([]byte(nil), data...)
	inString := false
	escaped := false
	for index := 0; index < len(normalized); index++ {
		current := normalized[index]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if current == '\\' {
				escaped = true
				continue
			}
			if current == '"' {
				inString = false
			}
			continue
		}
		if current == '"' {
			inString = true
			continue
		}
		if current == '/' && index+1 < len(normalized) && normalized[index+1] == '/' {
			normalized[index] = ' '
			index++
			normalized[index] = ' '
			for index+1 < len(normalized) && normalized[index+1] != '\n' && normalized[index+1] != '\r' {
				index++
				normalized[index] = ' '
			}
		}
	}

	inString = false
	escaped = false
	for index := 0; index < len(normalized); index++ {
		current := normalized[index]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if current == '\\' {
				escaped = true
				continue
			}
			if current == '"' {
				inString = false
			}
			continue
		}
		if current == '"' {
			inString = true
			continue
		}
		if current != ',' {
			continue
		}
		lookahead := index + 1
		for lookahead < len(normalized) && isJSONWhitespace(normalized[lookahead]) {
			lookahead++
		}
		if lookahead < len(normalized) && (normalized[lookahead] == '}' || normalized[lookahead] == ']') {
			normalized[index] = ' '
		}
	}
	return normalized
}

func isJSONWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

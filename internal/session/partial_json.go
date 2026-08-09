package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// parseStreamingJSONObject mirrors pi-ai's parseStreamingJson behavior for
// tool-call arguments: complete JSON wins, otherwise every fully observed
// member plus the current partial string/collection is exposed. Malformed
// input falls back to an empty object; a streaming UI must never be able to
// interrupt the authoritative Agent loop merely because one delta is partial.
func parseStreamingJSONObject(fragment []byte) map[string]any {
	trimmed := bytes.TrimSpace(fragment)
	if len(trimmed) == 0 {
		return map[string]any{}
	}
	var complete map[string]any
	if json.Unmarshal(trimmed, &complete) == nil && complete != nil {
		return complete
	}

	repaired := repairStreamingJSON(trimmed)
	if !bytes.Equal(repaired, trimmed) {
		complete = nil
		if json.Unmarshal(repaired, &complete) == nil && complete != nil {
			return complete
		}
	}
	candidates := [][]byte{trimmed}
	if !bytes.Equal(repaired, trimmed) {
		candidates = append(candidates, repaired)
	}
	for _, candidate := range candidates {
		parser := partialJSONParser{input: candidate}
		value, err := parser.parseAny()
		if err != nil {
			continue
		}
		if object, ok := value.(map[string]any); ok && object != nil {
			return object
		}
	}
	return map[string]any{}
}

var errPartialJSON = errors.New("partial JSON is not parseable")

type partialJSONParser struct {
	input []byte
	index int
}

func (p *partialJSONParser) parseAny() (any, error) {
	p.skipSpace()
	if p.index >= len(p.input) {
		return nil, errPartialJSON
	}
	switch p.input[p.index] {
	case '"':
		value, _, err := p.parseString()
		return value, err
	case '{':
		return p.parseObject()
	case '[':
		return p.parseArray()
	}
	remaining := string(p.input[p.index:])
	for _, literal := range []struct {
		text  string
		value any
	}{{"null", nil}, {"true", true}, {"false", false}} {
		if strings.HasPrefix(remaining, literal.text) || strings.HasPrefix(literal.text, remaining) {
			p.index += len(literal.text)
			return literal.value, nil
		}
	}
	return p.parseNumber()
}

func (p *partialJSONParser) parseString() (string, bool, error) {
	if p.index >= len(p.input) || p.input[p.index] != '"' {
		return "", false, errPartialJSON
	}
	start := p.index
	p.index++
	escaped := false
	for p.index < len(p.input) {
		current := p.input[p.index]
		if current == '"' && !escaped {
			p.index++
			var value string
			if err := json.Unmarshal(p.input[start:p.index], &value); err != nil {
				return "", false, errPartialJSON
			}
			return value, true, nil
		}
		if current == '\\' && !escaped {
			escaped = true
		} else {
			escaped = false
		}
		p.index++
	}

	partial := append([]byte(nil), p.input[start:p.index]...)
	for {
		candidate := append(append([]byte(nil), partial...), '"')
		var value string
		if json.Unmarshal(candidate, &value) == nil {
			return value, false, nil
		}
		lastSlash := bytes.LastIndexByte(partial, '\\')
		if lastSlash < 0 {
			return "", false, errPartialJSON
		}
		partial = partial[:lastSlash]
	}
}

func (p *partialJSONParser) parseObject() (map[string]any, error) {
	p.index++
	object := make(map[string]any)
	for {
		p.skipSpace()
		if p.index >= len(p.input) {
			return object, nil
		}
		if p.input[p.index] == '}' {
			p.index++
			return object, nil
		}
		key, complete, err := p.parseString()
		if err != nil || !complete {
			return object, nil
		}
		p.skipSpace()
		if p.index >= len(p.input) || p.input[p.index] != ':' {
			return object, nil
		}
		p.index++
		p.skipSpace()
		if p.index >= len(p.input) {
			return object, nil
		}
		value, err := p.parseAny()
		if err != nil {
			return object, nil
		}
		object[key] = value
		p.skipSpace()
		if p.index >= len(p.input) {
			return object, nil
		}
		switch p.input[p.index] {
		case ',':
			p.index++
		case '}':
			p.index++
			return object, nil
		default:
			return object, nil
		}
	}
}

func (p *partialJSONParser) parseArray() ([]any, error) {
	p.index++
	values := make([]any, 0)
	for {
		p.skipSpace()
		if p.index >= len(p.input) {
			return values, nil
		}
		if p.input[p.index] == ']' {
			p.index++
			return values, nil
		}
		value, err := p.parseAny()
		if err != nil {
			return values, nil
		}
		values = append(values, value)
		p.skipSpace()
		if p.index >= len(p.input) {
			return values, nil
		}
		switch p.input[p.index] {
		case ',':
			p.index++
		case ']':
			p.index++
			return values, nil
		default:
			return values, nil
		}
	}
}

func (p *partialJSONParser) parseNumber() (any, error) {
	start := p.index
	for p.index < len(p.input) && !bytes.ContainsRune([]byte(",]}"), rune(p.input[p.index])) {
		p.index++
	}
	token := bytes.TrimSpace(p.input[start:p.index])
	if len(token) == 0 || bytes.Equal(token, []byte("-")) {
		return nil, errPartialJSON
	}
	var value any
	if json.Unmarshal(token, &value) == nil {
		return value, nil
	}
	lastExponent := bytes.LastIndexAny(token, "eE")
	if lastExponent > 0 && json.Unmarshal(bytes.TrimSpace(token[:lastExponent]), &value) == nil {
		return value, nil
	}
	return nil, errPartialJSON
}

func (p *partialJSONParser) skipSpace() {
	for p.index < len(p.input) {
		switch p.input[p.index] {
		case ' ', '\n', '\r', '\t':
			p.index++
		default:
			return
		}
	}
}

func repairStreamingJSON(input []byte) []byte {
	result := make([]byte, 0, len(input)+8)
	inString := false
	for index := 0; index < len(input); index++ {
		current := input[index]
		if !inString {
			result = append(result, current)
			if current == '"' {
				inString = true
			}
			continue
		}
		if current == '"' {
			result = append(result, current)
			inString = false
			continue
		}
		if current == '\\' {
			if index+1 >= len(input) {
				result = append(result, '\\', '\\')
				continue
			}
			next := input[index+1]
			if next == 'u' && index+5 < len(input) && allJSONHex(input[index+2:index+6]) {
				result = append(result, input[index:index+6]...)
				index += 5
				continue
			}
			if bytes.ContainsRune([]byte(`"\\/bfnrt`), rune(next)) {
				result = append(result, current, next)
				index++
				continue
			}
			result = append(result, '\\', '\\')
			continue
		}
		if current <= 0x1f {
			switch current {
			case '\b':
				result = append(result, `\b`...)
			case '\f':
				result = append(result, `\f`...)
			case '\n':
				result = append(result, `\n`...)
			case '\r':
				result = append(result, `\r`...)
			case '\t':
				result = append(result, `\t`...)
			default:
				const hex = "0123456789abcdef"
				result = append(result, '\\', 'u', '0', '0', hex[current>>4], hex[current&0x0f])
			}
			continue
		}
		result = append(result, current)
	}
	return result
}

func allJSONHex(value []byte) bool {
	for _, candidate := range value {
		if candidate >= '0' && candidate <= '9' || candidate >= 'a' && candidate <= 'f' || candidate >= 'A' && candidate <= 'F' {
			continue
		}
		return false
	}
	return true
}

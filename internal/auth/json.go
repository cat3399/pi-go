package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

var errDuplicateField = errors.New("duplicate JSON object field")

// decodeObject rejects duplicate fields and trailing values. Raw values retain
// unknown provider records byte-for-byte through a mutation of another entry.
func decodeObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	value, err := decodeValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("trailing JSON value")
		}
		return nil, err
	}
	object, ok := value.(map[string]json.RawMessage)
	if !ok {
		return nil, errors.New("root is not an object")
	}
	return object, nil
}

func decodeValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, errors.New("JSON nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		object := make(map[string]json.RawMessage)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("object key is not a string")
			}
			if _, exists := object[key]; exists {
				return nil, errDuplicateField
			}
			var raw json.RawMessage
			if err := decoder.Decode(&raw); err != nil {
				return nil, err
			}
			if err := validateRaw(raw); err != nil {
				return nil, err
			}
			object[key] = append(json.RawMessage(nil), raw...)
		}
		closing, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if closing != json.Delim('}') {
			return nil, errors.New("object is not terminated")
		}
		return object, nil
	case '[':
		for decoder.More() {
			if _, err := decodeValue(decoder, depth+1); err != nil {
				return nil, err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if closing != json.Delim(']') {
			return nil, errors.New("array is not terminated")
		}
		return struct{}{}, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func validateRaw(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if _, err := decodeValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func parseCredential(raw json.RawMessage, provider string) (Credential, error) {
	var value struct {
		Type string            `json:"type"`
		Key  *string           `json:"key"`
		Env  map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return Credential{}, failure(KindMalformed, "read credential", provider, err)
	}
	if value.Type == "" {
		return Credential{}, failure(KindMalformed, "read credential", provider, nil)
	}
	if value.Type != "api_key" {
		return Credential{Type: value.Type}, nil
	}
	if value.Key == nil || !utf8Valid(*value.Key) {
		return Credential{}, failure(KindMalformed, "read credential", provider, nil)
	}
	for name, value := range value.Env {
		if !validEnvironmentName(name) || !utf8Valid(value) {
			return Credential{}, failure(KindMalformed, "read credential", provider, nil)
		}
	}
	return Credential{Type: value.Type, Key: *value.Key, Env: cloneEnv(value.Env)}, nil
}

func utf8Valid(value string) bool { return utf8.ValidString(value) }

package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
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
	if value.Type == "oauth" {
		return parseOAuthCredential(raw, provider)
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

func parseOAuthCredential(raw json.RawMessage, provider string) (Credential, error) {
	root, err := decodeObject(raw)
	if err != nil {
		return Credential{}, failure(KindMalformed, "read credential", provider, err)
	}
	var value struct {
		Type      string `json:"type"`
		Access    string `json:"access"`
		Refresh   string `json:"refresh"`
		Expires   int64  `json:"expires"`
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(raw, &value); err != nil || value.Type != "oauth" ||
		!validOAuthText(value.Access) || !validOAuthText(value.Refresh) || value.Expires <= 0 ||
		(value.AccountID != "" && !validOAuthText(value.AccountID)) {
		return Credential{}, failure(KindMalformed, "read credential", provider, err)
	}
	for _, key := range []string{"type", "access", "refresh", "expires", "accountId"} {
		delete(root, key)
	}
	return Credential{Type: "oauth", OAuth: OAuthCredential{
		Access: value.Access, Refresh: value.Refresh, Expires: value.Expires,
		AccountID: value.AccountID, Extra: root,
	}}, nil
}

func encodeOAuthCredential(value OAuthCredential, provider string) (json.RawMessage, error) {
	if !validOAuthText(value.Access) || !validOAuthText(value.Refresh) || value.Expires <= 0 ||
		(value.AccountID != "" && !validOAuthText(value.AccountID)) {
		return nil, failure(KindInvalid, "set OAuth credential", provider, nil)
	}
	root := make(map[string]json.RawMessage, len(value.Extra)+5)
	for key, raw := range value.Extra {
		if key == "type" || key == "access" || key == "refresh" || key == "expires" || key == "accountId" || !validJSONFieldName(key) {
			continue
		}
		if err := validateRaw(raw); err != nil {
			return nil, failure(KindInvalid, "set OAuth credential", provider, err)
		}
		root[key] = append(json.RawMessage(nil), raw...)
	}
	put := func(key string, value any) error {
		encoded, err := json.Marshal(value)
		if err == nil {
			root[key] = encoded
		}
		return err
	}
	if err := put("type", "oauth"); err != nil {
		return nil, err
	}
	if err := put("access", value.Access); err != nil {
		return nil, err
	}
	if err := put("refresh", value.Refresh); err != nil {
		return nil, err
	}
	if err := put("expires", value.Expires); err != nil {
		return nil, err
	}
	if value.AccountID != "" {
		if err := put("accountId", value.AccountID); err != nil {
			return nil, err
		}
	}
	return json.Marshal(root)
}

func validJSONFieldName(value string) bool {
	return utf8.ValidString(value) && value != "" && !strings.ContainsFunc(value, unicode.IsControl)
}

func utf8Valid(value string) bool { return utf8.ValidString(value) }

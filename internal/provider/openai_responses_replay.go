package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const maxResponsesSignatureBytes = 2 << 20

type responsesTextSignature struct {
	ID    string
	Phase string
}

func encodeResponsesTextSignature(id, phase string) (string, error) {
	if !validResponsesTextSignature(responsesTextSignature{ID: id, Phase: phase}) {
		return "", fmt.Errorf("invalid Responses text signature")
	}
	raw, err := json.Marshal(struct {
		Version int    `json:"v"`
		ID      string `json:"id"`
		Phase   string `json:"phase,omitempty"`
	}{Version: 1, ID: id, Phase: phase})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func decodeResponsesTextSignature(signature string) (responsesTextSignature, bool) {
	if !utf8.ValidString(signature) || len(signature) == 0 || len(signature) > maxResponsesSignatureBytes {
		return responsesTextSignature{}, false
	}
	trimmed := strings.TrimSpace(signature)
	if !strings.HasPrefix(trimmed, "{") {
		value := responsesTextSignature{ID: signature}
		return value, validResponsesTextSignature(value)
	}
	var wire struct {
		Version int    `json:"v"`
		ID      string `json:"id"`
		Phase   string `json:"phase"`
	}
	if json.Unmarshal([]byte(signature), &wire) != nil || wire.Version != 1 {
		return responsesTextSignature{}, false
	}
	value := responsesTextSignature{ID: wire.ID, Phase: wire.Phase}
	return value, validResponsesTextSignature(value)
}

func validResponsesTextSignature(value responsesTextSignature) bool {
	if !utf8.ValidString(value.ID) || strings.TrimSpace(value.ID) == "" || len(value.ID) > 256 {
		return false
	}
	return value.Phase == "" || value.Phase == "commentary" || value.Phase == "final_answer"
}

// decodeResponsesReasoningSignature validates the generic opaque signature
// just enough for safe replay, then returns the original JSON unchanged. Old
// pi-go sessions may contain encrypted reasoning reconstructed without the
// schema-required summary field; repair only that historical shape while
// preserving every other field.
func decodeResponsesReasoningSignature(signature string) (json.RawMessage, bool) {
	raw, fields, encrypted, ok := inspectResponsesReasoningItem([]byte(signature))
	if !ok {
		return nil, false
	}
	if _, hasSummary := fields["summary"]; encrypted != "" && !hasSummary {
		fields["summary"] = json.RawMessage("[]")
		repaired, err := json.Marshal(fields)
		if err != nil || len(repaired) > maxResponsesSignatureBytes {
			return nil, false
		}
		return repaired, true
	}
	return raw, true
}

// preserveResponsesReasoningItem validates a newly completed reasoning item
// without rewriting it. This is the Go equivalent of the upstream
// JSON.stringify(item) persistence contract.
func preserveResponsesReasoningItem(item json.RawMessage) (json.RawMessage, error) {
	raw, _, _, ok := inspectResponsesReasoningItem(item)
	if !ok {
		return nil, fmt.Errorf("invalid Responses reasoning output item")
	}
	return raw, nil
}

// patchResponsesReasoningEncryption mirrors upstream's terminal Azure
// backfill: retain the completed item's full JSON and replace only
// encrypted_content.
func patchResponsesReasoningEncryption(item json.RawMessage, encryptedContent string) (json.RawMessage, error) {
	_, fields, _, ok := inspectResponsesReasoningItem(item)
	if !ok || !utf8.ValidString(encryptedContent) || encryptedContent == "" || len(encryptedContent) > 1<<20 {
		return nil, fmt.Errorf("invalid Responses reasoning encryption backfill")
	}
	rawEncrypted, err := json.Marshal(encryptedContent)
	if err != nil {
		return nil, err
	}
	fields["encrypted_content"] = rawEncrypted
	patched, err := json.Marshal(fields)
	if err != nil || len(patched) > maxResponsesSignatureBytes {
		return nil, fmt.Errorf("invalid Responses reasoning encryption backfill")
	}
	return patched, nil
}

func inspectResponsesReasoningItem(item []byte) (json.RawMessage, map[string]json.RawMessage, string, bool) {
	if !utf8.Valid(item) || len(item) == 0 || len(item) > maxResponsesSignatureBytes || !json.Valid(item) {
		return nil, nil, "", false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(item, &fields) != nil || fields == nil {
		return nil, nil, "", false
	}
	var typeName, id string
	if json.Unmarshal(fields["type"], &typeName) != nil || typeName != "reasoning" || json.Unmarshal(fields["id"], &id) != nil || !validResponsesReasoningIdentity(id) {
		return nil, nil, "", false
	}
	var encryptedContent string
	if raw, ok := fields["encrypted_content"]; ok {
		if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			if json.Unmarshal(raw, &encryptedContent) != nil || !utf8.ValidString(encryptedContent) || len(encryptedContent) > 1<<20 {
				return nil, nil, "", false
			}
		}
	}
	if raw, ok := fields["content"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && len(raw) > 1<<20 {
		return nil, nil, "", false
	}
	return append(json.RawMessage(nil), item...), fields, encryptedContent, true
}

func validResponsesReasoningIdentity(id string) bool {
	return utf8.ValidString(id) && strings.TrimSpace(id) != "" && len(id) <= 256
}

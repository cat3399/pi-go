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

func encodeResponsesReasoningSignature(id, encryptedContent, plaintextContent, summaryText string) (string, error) {
	if !validResponsesReasoningIdentity(id) || !utf8.ValidString(encryptedContent) || !utf8.ValidString(plaintextContent) || !utf8.ValidString(summaryText) || len(encryptedContent) > 1<<20 || len(plaintextContent) > 1<<20 || len(summaryText) > 1<<20 || (encryptedContent != "" && plaintextContent != "") {
		return "", fmt.Errorf("invalid Responses reasoning signature")
	}
	wire := responsesReasoningInput{
		Type:             "reasoning",
		ID:               id,
		EncryptedContent: encryptedContent,
		Content:          plaintextContent,
	}
	if encryptedContent != "" && summaryText != "" {
		wire.Summary = []responsesReasoningSummary{{Type: "summary_text", Text: summaryText}}
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// decodeResponsesReasoningSignature validates the generic opaque signature
// just enough for safe replay, then returns the original JSON unchanged. This
// preserves future Responses item fields instead of teaching llm about them.
func decodeResponsesReasoningSignature(signature string) (json.RawMessage, bool) {
	if !utf8.ValidString(signature) || len(signature) == 0 || len(signature) > maxResponsesSignatureBytes || !json.Valid([]byte(signature)) {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(signature), &fields) != nil || fields == nil {
		return nil, false
	}
	var typeName, id string
	if json.Unmarshal(fields["type"], &typeName) != nil || typeName != "reasoning" || json.Unmarshal(fields["id"], &id) != nil || !validResponsesReasoningIdentity(id) {
		return nil, false
	}
	if raw, ok := fields["encrypted_content"]; ok {
		var value string
		if json.Unmarshal(raw, &value) != nil || !utf8.ValidString(value) || len(value) > 1<<20 {
			return nil, false
		}
	}
	if raw, ok := fields["content"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && len(raw) > 1<<20 {
		return nil, false
	}
	return append(json.RawMessage(nil), signature...), true
}

func validResponsesReasoningIdentity(id string) bool {
	return utf8.ValidString(id) && strings.TrimSpace(id) != "" && len(id) <= 256
}

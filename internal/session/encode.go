package session

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
)

func encodeMessage(message llm.ConversationMessage, options AppendOptions) (json.RawMessage, error) {
	switch message := message.(type) {
	case llm.UserTextMessage:
		content, err := encodeTextBlocks(message.Content())
		if err != nil {
			return nil, err
		}
		encoded := append([]byte(nil), `{"role":"user","content":`...)
		encoded = appendJSONArray(encoded, content)
		encoded = append(encoded, `,"timestamp":`...)
		encoded = strconv.AppendInt(encoded, message.Timestamp().UnixMilli(), 10)
		return append(encoded, '}'), nil
	case llm.UserContentMessage:
		content, err := encodeUserContentBlocks(message.Content())
		if err != nil {
			return nil, err
		}
		encoded := append([]byte(nil), `{"role":"user","content":`...)
		encoded = appendJSONArray(encoded, content)
		encoded = append(encoded, `,"timestamp":`...)
		encoded = strconv.AppendInt(encoded, message.Timestamp().UnixMilli(), 10)
		return append(encoded, '}'), nil
	case llm.AssistantTextMessage:
		if err := validateMessageAssistantProvenance(message, options.Assistant); err != nil {
			return nil, err
		}
		replay, _ := message.OpenAIResponsesMetadata()
		return encodeAssistant(message.Blocks(), message.FinishReason(), message.Usage(), "", message.Timestamp().UnixMilli(), options.Assistant, &replay)
	case llm.AssistantToolUseMessage:
		if err := validateMessageAssistantProvenance(message, options.Assistant); err != nil {
			return nil, err
		}
		replay, ok := message.OpenAIResponsesMetadata()
		if !ok {
			replay = llm.OpenAIResponsesResponse{}
		}
		return encodeAssistant(message.Blocks(), message.FinishReason(), message.Usage(), "", message.Timestamp().UnixMilli(), options.Assistant, &replay)
	case llm.AssistantRichMessage:
		if err := validateMessageAssistantProvenance(message, options.Assistant); err != nil {
			return nil, err
		}
		replay, ok := message.OpenAIResponsesMetadata()
		if !ok {
			replay = llm.OpenAIResponsesResponse{}
		}
		return encodeAssistant(message.Blocks(), message.FinishReason(), message.Usage(), "", message.Timestamp().UnixMilli(), options.Assistant, &replay)
	case llm.AssistantFailureMessage:
		return encodeAssistant(message.Blocks(), message.FinishReason(), message.Usage(), message.ErrorMessage(), message.Timestamp().UnixMilli(), options.Assistant, nil)
	case llm.ToolResultMessage:
		content, err := encodeTextBlocks(message.Content())
		if err != nil {
			return nil, err
		}
		encoded := append([]byte(nil), `{"role":"toolResult","toolCallId":`...)
		encoded, err = appendJSONValue(encoded, message.ToolCallID())
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, `,"toolName":`...)
		encoded, err = appendJSONValue(encoded, message.ToolName())
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, `,"content":`...)
		encoded = appendJSONArray(encoded, content)
		encoded = append(encoded, `,"isError":`...)
		encoded = strconv.AppendBool(encoded, message.IsError())
		encoded = append(encoded, `,"timestamp":`...)
		encoded = strconv.AppendInt(encoded, message.Timestamp().UnixMilli(), 10)
		return append(encoded, '}'), nil
	case llm.ToolResultContentMessage:
		content, err := encodeToolResultContentBlocks(message.Content())
		if err != nil {
			return nil, err
		}
		encoded := append([]byte(nil), `{"role":"toolResult","toolCallId":`...)
		encoded, err = appendJSONValue(encoded, message.ToolCallID())
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, `,"toolName":`...)
		encoded, err = appendJSONValue(encoded, message.ToolName())
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, `,"content":`...)
		encoded = appendJSONArray(encoded, content)
		encoded = append(encoded, `,"isError":`...)
		encoded = strconv.AppendBool(encoded, message.IsError())
		encoded = append(encoded, `,"timestamp":`...)
		encoded = strconv.AppendInt(encoded, message.Timestamp().UnixMilli(), 10)
		return append(encoded, '}'), nil
	default:
		return nil, fmt.Errorf("invalid conversation message %T", message)
	}
}

type llmAssistantProvenanceCarrier interface {
	AssistantProvenance() (llm.AssistantProvenance, bool)
}

func validateMessageAssistantProvenance(message llmAssistantProvenanceCarrier, identity AssistantProvenance) error {
	provenance, ok := message.AssistantProvenance()
	if !ok {
		return nil
	}
	if provenance.Provider != identity.Provider || provenance.API != identity.API || provenance.Model != identity.Model {
		return fmt.Errorf("%w: assistant message provenance does not match append provenance", ErrInvalidEntry)
	}
	return nil
}

func encodeAssistant(
	blocks []llm.AssistantBlock,
	finish llm.FinishReason,
	usage llm.Usage,
	errorMessage string,
	timestamp int64,
	identity AssistantProvenance, replay *llm.OpenAIResponsesResponse,
) (json.RawMessage, error) {
	if err := validateAssistantProvenance(identity); err != nil {
		return nil, err
	}
	content := make([]json.RawMessage, 0, len(blocks))
	for _, block := range blocks {
		switch block := block.(type) {
		case llm.TextBlock:
			wire := textBlockWire{Type: "text", Text: block.Text()}
			if replay, ok := block.TextReplay(); ok {
				wire.TextSignature = encodeTextReplay(replay)
			}
			raw, err := json.Marshal(wire)
			if err != nil {
				return nil, err
			}
			content = append(content, raw)
		case llm.ToolCallBlock:
			raw, err := encodeToolCallBlock(block)
			if err != nil {
				return nil, err
			}
			content = append(content, raw)
		case llm.ThinkingBlock:
			wire := thinkingBlockWire{Type: "thinking", Thinking: block.Thinking()}
			if replay, ok := block.OpenAIResponsesReplay(); ok {
				wire.ThinkingSignature = encodeReasoningReplay(replay)
				wire.Redacted = replay.Redacted
			}
			raw, err := json.Marshal(wire)
			if err != nil {
				return nil, err
			}
			content = append(content, raw)
		default:
			return nil, fmt.Errorf("unsupported assistant block %T", block)
		}
	}
	usageJSON, err := json.Marshal(encodeUsage(usage, identity.Cost))
	if err != nil {
		return nil, err
	}
	encoded := append([]byte(nil), `{"role":"assistant","content":`...)
	encoded = appendJSONArray(encoded, content)
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "api", value: identity.API},
		{name: "provider", value: identity.Provider},
		{name: "model", value: identity.Model},
	} {
		encoded = append(encoded, ',')
		encoded, err = appendJSONValue(encoded, field.name)
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, ':')
		encoded, err = appendJSONValue(encoded, field.value)
		if err != nil {
			return nil, err
		}
	}
	encoded = append(encoded, `,"usage":`...)
	encoded = append(encoded, usageJSON...)
	encoded = append(encoded, `,"stopReason":`...)
	encoded, err = appendJSONValue(encoded, finish.String())
	if err != nil {
		return nil, err
	}
	if errorMessage != "" {
		encoded = append(encoded, `,"errorMessage":`...)
		encoded, err = appendJSONValue(encoded, errorMessage)
		if err != nil {
			return nil, err
		}
	}
	if replay != nil && (replay.ResponseID != "" || replay.RawStopReason != "") {
		encoded = append(encoded, `,"responseId":`...)
		encoded, err = appendJSONValue(encoded, replay.ResponseID)
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, `,"rawStopReason":`...)
		encoded, err = appendJSONValue(encoded, replay.RawStopReason)
		if err != nil {
			return nil, err
		}
	}
	encoded = append(encoded, `,"timestamp":`...)
	encoded = strconv.AppendInt(encoded, timestamp, 10)
	return append(encoded, '}'), nil
}

func encodeTextBlocks(blocks []llm.TextBlock) ([]json.RawMessage, error) {
	content := make([]json.RawMessage, 0, len(blocks))
	for _, block := range blocks {
		wire := textBlockWire{Type: "text", Text: block.Text()}
		if replay, ok := block.TextReplay(); ok {
			wire.TextSignature = encodeTextReplay(replay)
		}
		raw, err := json.Marshal(wire)
		if err != nil {
			return nil, err
		}
		content = append(content, raw)
	}
	return content, nil
}
func encodeUserContentBlocks(blocks []llm.UserContentBlock) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(blocks))
	for _, block := range blocks {
		switch block := block.(type) {
		case llm.TextBlock:
			raw, err := json.Marshal(textBlockWire{Type: "text", Text: block.Text()})
			if err != nil {
				return nil, err
			}
			out = append(out, raw)
		case llm.ImageBlock:
			raw, err := encodeImageBlock(block)
			if err != nil {
				return nil, err
			}
			out = append(out, raw)
		default:
			return nil, fmt.Errorf("unsupported user block %T", block)
		}
	}
	return out, nil
}
func encodeToolResultContentBlocks(blocks []llm.ToolResultContentBlock) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(blocks))
	for _, block := range blocks {
		switch block := block.(type) {
		case llm.TextBlock:
			raw, err := json.Marshal(textBlockWire{Type: "text", Text: block.Text()})
			if err != nil {
				return nil, err
			}
			out = append(out, raw)
		case llm.ImageBlock:
			raw, err := encodeImageBlock(block)
			if err != nil {
				return nil, err
			}
			out = append(out, raw)
		default:
			return nil, fmt.Errorf("unsupported tool result block %T", block)
		}
	}
	return out, nil
}
func encodeImageBlock(block llm.ImageBlock) (json.RawMessage, error) {
	wire := imageBlockWire{Type: "image", MimeType: block.MediaType()}
	if block.Source() == llm.ImageSourceData {
		wire.Data = base64.StdEncoding.EncodeToString(block.Data())
	} else {
		wire.URL = block.URL()
	}
	return json.Marshal(wire)
}
func encodeTextReplay(value llm.TextReplay) string {
	raw, _ := json.Marshal(struct {
		Version int    `json:"v"`
		ID      string `json:"id"`
		Phase   string `json:"phase,omitempty"`
	}{Version: 1, ID: value.MessageID, Phase: value.Phase})
	return string(raw)
}
func encodeReasoningReplay(value llm.OpenAIResponsesReasoning) string {
	raw, _ := json.Marshal(struct {
		Type             string `json:"type"`
		ID               string `json:"id"`
		EncryptedContent string `json:"encrypted_content,omitempty"`
	}{Type: "reasoning", ID: value.ItemID, EncryptedContent: value.EncryptedContent})
	return string(raw)
}

func encodeToolCallBlock(block llm.ToolCallBlock) (json.RawMessage, error) {
	arguments := block.ArgumentsJSON()
	if !json.Valid(arguments) {
		return nil, fmt.Errorf("invalid tool arguments JSON")
	}
	compactArguments, err := compactJSON(arguments)
	if err != nil {
		return nil, fmt.Errorf("compact tool arguments JSON: %w", err)
	}
	encoded := append([]byte(nil), `{"type":"toolCall","id":`...)
	encoded, err = appendJSONValue(encoded, block.ID())
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, `,"name":`...)
	encoded, err = appendJSONValue(encoded, block.Name())
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, `,"arguments":`...)
	encoded = append(encoded, compactArguments...)
	// JSONL cannot embed lexical whitespace containing a physical newline. Keep
	// the upstream-compatible object in arguments and retain the exact provider
	// bytes in a namespaced string for lossless Go resume.
	encoded = append(encoded, `,"_piGoRawArguments":`...)
	encoded, err = appendJSONValue(encoded, string(arguments))
	if err != nil {
		return nil, err
	}
	return append(encoded, '}'), nil
}

func encodeMessageEntry(
	id string,
	parentID string,
	hasParent bool,
	timestamp time.Time,
	message json.RawMessage,
) (json.RawMessage, error) {
	if !json.Valid(message) {
		return nil, fmt.Errorf("invalid encoded message JSON")
	}
	encoded := append([]byte(nil), `{"type":"message","id":`...)
	var err error
	encoded, err = appendJSONValue(encoded, id)
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, `,"parentId":`...)
	if hasParent {
		encoded, err = appendJSONValue(encoded, parentID)
		if err != nil {
			return nil, err
		}
	} else {
		encoded = append(encoded, "null"...)
	}
	encoded = append(encoded, `,"timestamp":`...)
	encoded, err = appendJSONValue(encoded, formatISOTime(timestamp))
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, `,"message":`...)
	encoded = append(encoded, message...)
	return append(encoded, '}'), nil
}

func appendJSONArray(destination []byte, values []json.RawMessage) []byte {
	destination = append(destination, '[')
	for index, value := range values {
		if index > 0 {
			destination = append(destination, ',')
		}
		destination = append(destination, value...)
	}
	return append(destination, ']')
}

func appendJSONValue(destination []byte, value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return append(destination, encoded...), nil
}

func compactJSON(value []byte) ([]byte, error) {
	var compact bytes.Buffer
	compact.Grow(len(value))
	if err := json.Compact(&compact, value); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}

func validateAssistantProvenance(identity AssistantProvenance) error {
	for _, field := range []struct {
		name  string
		value string
	}{{"api", identity.API}, {"provider", identity.Provider}, {"model", identity.Model}} {
		if !utf8.ValidString(field.value) || strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%w: assistant %s must be non-empty valid UTF-8", ErrInvalidEntry, field.name)
		}
	}
	return validateUsageCost(identity.Cost)
}

func encodeUsage(usage llm.Usage, cost UsageCost) usageWire {
	wire := usageWire{
		Input:       usage.Input(),
		Output:      usage.Output(),
		CacheRead:   usage.CacheRead(),
		CacheWrite:  usage.CacheWrite(),
		TotalTokens: usage.TotalTokens(),
		Cost:        cost,
	}
	if value, ok := usage.Reasoning(); ok {
		wire.Reasoning = &value
	}
	if value, ok := usage.CacheWrite1h(); ok {
		wire.CacheWrite1h = &value
	}
	return wire
}

func validateUsageCost(cost UsageCost) error {
	for _, field := range []struct {
		name  string
		value json.Number
	}{
		{"input", cost.Input},
		{"output", cost.Output},
		{"cacheRead", cost.CacheRead},
		{"cacheWrite", cost.CacheWrite},
		{"total", cost.Total},
	} {
		text := field.value.String()
		value, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsInf(value, 0) || math.IsNaN(value) || value < 0 {
			return fmt.Errorf("%w: assistant usage cost %s is not a finite non-negative JSON number", ErrInvalidEntry, field.name)
		}
		if _, err := json.Marshal(field.value); err != nil {
			return fmt.Errorf("%w: assistant usage cost %s: %v", ErrInvalidEntry, field.name, err)
		}
	}
	return nil
}

type textBlockWire struct {
	Type          string `json:"type"`
	Text          string `json:"text"`
	TextSignature string `json:"textSignature,omitempty"`
}
type thinkingBlockWire struct {
	Type              string `json:"type"`
	Thinking          string `json:"thinking"`
	ThinkingSignature string `json:"thinkingSignature,omitempty"`
	Redacted          bool   `json:"redacted,omitempty"`
}
type imageBlockWire struct {
	Type     string `json:"type"`
	Data     string `json:"data,omitempty"`
	URL      string `json:"url,omitempty"`
	MimeType string `json:"mimeType"`
}

type usageWire struct {
	Input        uint64    `json:"input"`
	Output       uint64    `json:"output"`
	CacheRead    uint64    `json:"cacheRead"`
	CacheWrite   uint64    `json:"cacheWrite"`
	Reasoning    *uint64   `json:"reasoning,omitempty"`
	CacheWrite1h *uint64   `json:"cacheWrite1h,omitempty"`
	TotalTokens  uint64    `json:"totalTokens"`
	Cost         UsageCost `json:"cost"`
}

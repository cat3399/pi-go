package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
)

func decodeSessionFile(path string, data []byte) (Header, []Entry, map[string]int, bool, error) {
	var header Header
	entries := make([]Entry, 0)
	byID := make(map[string]int)
	haveHeader := false

	for _, line := range physicalLines(data) {
		if len(bytes.TrimSpace(line.data)) == 0 {
			continue
		}
		if !utf8.Valid(line.data) {
			return Header{}, nil, nil, false, parseError(ErrInvalidSession, path, line.number, "record is not valid UTF-8", nil)
		}
		if !haveHeader {
			decoded, err := decodeHeader(line.data)
			if err != nil {
				return Header{}, nil, nil, false, parseError(ErrInvalidSession, path, line.number, "invalid header", err)
			}
			header = decoded
			haveHeader = true
			continue
		}

		entry, err := decodeEntry(line.data)
		if err != nil {
			return Header{}, nil, nil, false, parseError(ErrInvalidEntry, path, line.number, "invalid entry", err)
		}
		if _, duplicate := byID[entry.id]; duplicate {
			return Header{}, nil, nil, false, parseError(ErrInvalidEntry, path, line.number, "duplicate entry id", nil)
		}
		if entry.hasParent {
			if _, parentExists := byID[entry.parentID]; !parentExists {
				return Header{}, nil, nil, false, parseError(ErrUnsupportedTree, path, line.number, "parent must reference an earlier entry", nil)
			}
		}
		byID[entry.id] = len(entries)
		entries = append(entries, entry)
	}

	if !haveHeader {
		return Header{}, nil, nil, false, fmt.Errorf("%w: %s: missing header", ErrInvalidSession, path)
	}
	needsSeparator := len(data) > 0 && data[len(data)-1] != '\n'
	return header, entries, byID, needsSeparator, nil
}

type physicalLine struct {
	number int
	data   []byte
}

func physicalLines(data []byte) []physicalLine {
	lines := make([]physicalLine, 0, bytes.Count(data, []byte{'\n'})+1)
	start := 0
	lineNumber := 1
	for start < len(data) {
		index := bytes.IndexByte(data[start:], '\n')
		if index < 0 {
			lines = append(lines, physicalLine{number: lineNumber, data: data[start:]})
			break
		}
		lines = append(lines, physicalLine{number: lineNumber, data: data[start : start+index]})
		start += index + 1
		lineNumber++
	}
	return lines
}

func decodeHeader(raw []byte) (Header, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return Header{}, err
	}
	typeName, err := requiredString(object, "type")
	if err != nil || typeName != "session" {
		return Header{}, fmt.Errorf("first record is not a session header")
	}
	var version int
	if value, exists := object["version"]; !exists || json.Unmarshal(value, &version) != nil {
		return Header{}, fmt.Errorf("invalid session version")
	}
	if version != 3 {
		return Header{}, fmt.Errorf("%w: version %d", ErrUnsupportedVersion, version)
	}
	id, err := requiredString(object, "id")
	if err != nil {
		return Header{}, err
	}
	if err := validateOpaqueID(id, "session id"); err != nil {
		return Header{}, err
	}
	timestampText, err := requiredString(object, "timestamp")
	if err != nil {
		return Header{}, err
	}
	timestamp, err := time.Parse(time.RFC3339, timestampText)
	if err != nil {
		return Header{}, fmt.Errorf("invalid header timestamp")
	}
	workingDir, err := requiredString(object, "cwd")
	if err != nil || strings.TrimSpace(workingDir) == "" {
		return Header{}, fmt.Errorf("invalid header cwd")
	}
	parentSession := ""
	hasParentSession := false
	if parent, exists := object["parentSession"]; exists {
		var value string
		if json.Unmarshal(parent, &value) != nil {
			return Header{}, fmt.Errorf("invalid header parentSession")
		}
		if !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
			return Header{}, fmt.Errorf("invalid header parentSession")
		}
		parentSession, hasParentSession = value, true
	}
	return Header{id: id, workingDir: workingDir, parentSession: parentSession, hasParentSession: hasParentSession, timestamp: timestamp, raw: bytes.Clone(raw)}, nil
}

func decodeEntry(raw []byte) (Entry, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return Entry{}, err
	}
	typeName, err := requiredString(object, "type")
	if err != nil || strings.TrimSpace(typeName) == "" || typeName == "session" {
		return Entry{}, fmt.Errorf("invalid entry type")
	}
	id, err := requiredString(object, "id")
	if err != nil {
		return Entry{}, err
	}
	if err := validateOpaqueID(id, "entry id"); err != nil {
		return Entry{}, err
	}
	parentID, hasParent, err := decodeParentID(object)
	if err != nil {
		return Entry{}, err
	}
	timestampText, err := requiredString(object, "timestamp")
	if err != nil {
		return Entry{}, err
	}
	timestamp, err := time.Parse(time.RFC3339, timestampText)
	if err != nil {
		return Entry{}, fmt.Errorf("invalid entry timestamp")
	}

	entry := Entry{
		id:        id,
		parentID:  parentID,
		hasParent: hasParent,
		timestamp: timestamp,
		typeName:  typeName,
		raw:       bytes.Clone(raw),
	}
	if typeName == "message" {
		messageRaw, exists := object["message"]
		if !exists {
			return Entry{}, fmt.Errorf("message entry is missing message")
		}
		entry.message, entry.diagnostics, err = decodeMessage(id, messageRaw)
		if err != nil {
			return Entry{}, err
		}
		entry.assistant, entry.hasAssistant, err = decodeAssistantProvenance(messageRaw)
		if err != nil {
			return Entry{}, err
		}
	} else {
		entry.diagnostics = []Diagnostic{{Code: DiagnosticUnknownEntry, EntryID: id, ContentIndex: -1}}
	}
	return entry, nil
}

func decodeAssistantProvenance(raw []byte) (AssistantProvenance, bool, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return AssistantProvenance{}, false, err
	}
	role, err := requiredString(object, "role")
	if err != nil || role != "assistant" {
		return AssistantProvenance{}, false, nil
	}
	identity := AssistantProvenance{}
	identity.API, err = requiredString(object, "api")
	if err != nil {
		return AssistantProvenance{}, false, err
	}
	identity.Provider, err = requiredString(object, "provider")
	if err != nil {
		return AssistantProvenance{}, false, err
	}
	identity.Model, err = requiredString(object, "model")
	if err != nil {
		return AssistantProvenance{}, false, err
	}
	usage, err := decodeObject(object["usage"])
	if err != nil {
		return AssistantProvenance{}, false, fmt.Errorf("invalid assistant usage")
	}
	costRaw, exists := usage["cost"]
	if !exists {
		return AssistantProvenance{}, false, fmt.Errorf("assistant usage is missing cost")
	}
	if err := json.Unmarshal(costRaw, &identity.Cost); err != nil {
		return AssistantProvenance{}, false, fmt.Errorf("invalid assistant usage cost")
	}
	if err := validateAssistantProvenance(identity); err != nil {
		return AssistantProvenance{}, false, err
	}
	return identity, true, nil
}

func decodeParentID(object map[string]json.RawMessage) (string, bool, error) {
	raw, exists := object["parentId"]
	if !exists {
		return "", false, fmt.Errorf("entry is missing parentId")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false, nil
	}
	var parent string
	if json.Unmarshal(raw, &parent) != nil {
		return "", false, fmt.Errorf("invalid parentId")
	}
	if err := validateOpaqueID(parent, "parent id"); err != nil {
		return "", false, err
	}
	return parent, true, nil
}

func decodeMessage(entryID string, raw []byte) (llm.ConversationMessage, []Diagnostic, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid message object: %w", err)
	}
	role, err := requiredString(object, "role")
	if err != nil {
		return nil, nil, err
	}
	switch role {
	case "user":
		return decodeUserMessage(entryID, object)
	case "assistant":
		return decodeAssistantMessage(entryID, object)
	case "toolResult":
		return decodeToolResultMessage(entryID, object)
	default:
		return nil, []Diagnostic{{Code: DiagnosticUnknownMessageRole, EntryID: entryID, ContentIndex: -1}}, nil
	}
}

func decodeUserMessage(entryID string, object map[string]json.RawMessage) (llm.ConversationMessage, []Diagnostic, error) {
	timestamp, err := decodeMessageTimestamp(object)
	if err != nil {
		return nil, nil, err
	}
	content, exists := object["content"]
	if !exists {
		return nil, nil, fmt.Errorf("user message is missing content")
	}
	var text string
	if json.Unmarshal(content, &text) == nil {
		message, err := llm.NewUserTextMessage(text, timestamp)
		return message, nil, err
	}
	blocks, diagnostics, err := decodeBlocks(entryID, content, false)
	if err != nil {
		return nil, nil, err
	}
	texts := textBlocks(blocks)
	if len(texts) == 0 && len(diagnostics) > 0 {
		diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnprojectableMessage, EntryID: entryID, ContentIndex: -1})
		return nil, diagnostics, nil
	}
	message, err := llm.NewUserTextBlocksMessage(texts, timestamp)
	return message, diagnostics, err
}

func decodeAssistantMessage(entryID string, object map[string]json.RawMessage) (llm.ConversationMessage, []Diagnostic, error) {
	timestamp, err := decodeMessageTimestamp(object)
	if err != nil {
		return nil, nil, err
	}
	content, exists := object["content"]
	if !exists {
		return nil, nil, fmt.Errorf("assistant message is missing content")
	}
	blocks, diagnostics, err := decodeBlocks(entryID, content, true)
	if err != nil {
		return nil, nil, err
	}
	stopReason, err := requiredString(object, "stopReason")
	if err != nil {
		return nil, nil, err
	}
	usage, err := decodeUsage(object["usage"])
	if err != nil {
		return nil, nil, err
	}

	switch stopReason {
	case "stop", "length":
		finish := llm.FinishStop
		if stopReason == "length" {
			finish = llm.FinishLength
		}
		message, err := llm.NewAssistantTextMessage(textBlocks(blocks), finish, usage, timestamp)
		if err != nil {
			return nil, nil, err
		}
		if len(textBlocks(blocks)) != len(blocks) {
			return nil, nil, fmt.Errorf("successful text assistant contains a tool call")
		}
		return message, diagnostics, nil
	case "toolUse":
		message, err := llm.NewAssistantToolUseMessage(blocks, usage, timestamp)
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnprojectableMessage, EntryID: entryID, ContentIndex: -1})
			return nil, diagnostics, nil
		}
		return message, diagnostics, nil
	case "error", "aborted":
		finish := llm.FinishError
		if stopReason == "aborted" {
			finish = llm.FinishAborted
		}
		errorMessage, errorMessageErr := requiredString(object, "errorMessage")
		if errorMessageErr != nil || strings.TrimSpace(errorMessage) == "" {
			diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnprojectableMessage, EntryID: entryID, ContentIndex: -1})
			return nil, diagnostics, nil
		}
		if len(textBlocks(blocks)) != len(blocks) {
			diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnsafeContentOmitted, EntryID: entryID, ContentIndex: -1})
		}
		message, err := llm.NewAssistantFailureMessage(textBlocks(blocks), finish, errorMessage, usage, timestamp)
		return message, diagnostics, err
	default:
		diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnprojectableMessage, EntryID: entryID, ContentIndex: -1})
		return nil, diagnostics, nil
	}
}

func decodeToolResultMessage(entryID string, object map[string]json.RawMessage) (llm.ConversationMessage, []Diagnostic, error) {
	timestamp, err := decodeMessageTimestamp(object)
	if err != nil {
		return nil, nil, err
	}
	toolCallID, err := requiredString(object, "toolCallId")
	if err != nil {
		return nil, nil, err
	}
	toolName, err := requiredString(object, "toolName")
	if err != nil {
		return nil, nil, err
	}
	var isError bool
	if raw, exists := object["isError"]; !exists || json.Unmarshal(raw, &isError) != nil {
		return nil, nil, fmt.Errorf("tool result has invalid isError")
	}
	content, exists := object["content"]
	if !exists {
		return nil, nil, fmt.Errorf("tool result is missing content")
	}
	blocks, diagnostics, err := decodeBlocks(entryID, content, false)
	if err != nil {
		return nil, nil, err
	}
	message, err := llm.NewToolResultMessage(toolCallID, toolName, textBlocks(blocks), isError, timestamp)
	return message, diagnostics, err
}

func decodeBlocks(entryID string, raw []byte, allowToolCalls bool) ([]llm.AssistantBlock, []Diagnostic, error) {
	var encoded []json.RawMessage
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, nil, fmt.Errorf("message content must be an array")
	}
	blocks := make([]llm.AssistantBlock, 0, len(encoded))
	diagnostics := make([]Diagnostic, 0)
	for index, encodedBlock := range encoded {
		object, err := decodeObject(encodedBlock)
		if err != nil {
			return nil, nil, fmt.Errorf("content block %d is invalid", index)
		}
		typeName, err := requiredString(object, "type")
		if err != nil {
			return nil, nil, fmt.Errorf("content block %d has invalid type", index)
		}
		switch typeName {
		case "text":
			text, err := requiredString(object, "text")
			if err != nil {
				return nil, nil, fmt.Errorf("content block %d has invalid text", index)
			}
			block, err := llm.NewTextBlock(text)
			if err != nil {
				return nil, nil, err
			}
			blocks = append(blocks, block)
		case "toolCall":
			if !allowToolCalls {
				diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnknownContentBlock, EntryID: entryID, ContentIndex: index})
				continue
			}
			id, err := requiredString(object, "id")
			if err != nil {
				return nil, nil, fmt.Errorf("content block %d has invalid tool id", index)
			}
			name, err := requiredString(object, "name")
			if err != nil {
				return nil, nil, fmt.Errorf("content block %d has invalid tool name", index)
			}
			arguments, exists := object["arguments"]
			if !exists {
				return nil, nil, fmt.Errorf("content block %d is missing tool arguments", index)
			}
			if preserved, exists := object["_piGoRawArguments"]; exists {
				var lexical string
				if err := json.Unmarshal(preserved, &lexical); err != nil {
					return nil, nil, fmt.Errorf("content block %d has invalid preserved tool arguments", index)
				}
				lexicalJSON := []byte(lexical)
				equal, err := semanticJSONEqual(lexicalJSON, arguments)
				if err != nil || !equal {
					return nil, nil, fmt.Errorf("content block %d preserved tool arguments do not match arguments", index)
				}
				arguments = lexicalJSON
			}
			block, err := llm.NewToolCallBlock(id, name, arguments)
			if err != nil {
				return nil, nil, err
			}
			blocks = append(blocks, block)
		default:
			diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnknownContentBlock, EntryID: entryID, ContentIndex: index})
		}
	}
	return blocks, diagnostics, nil
}

func textBlocks(blocks []llm.AssistantBlock) []llm.TextBlock {
	texts := make([]llm.TextBlock, 0, len(blocks))
	for _, block := range blocks {
		if text, ok := block.(llm.TextBlock); ok {
			texts = append(texts, text)
		}
	}
	return texts
}

func decodeUsage(raw []byte) (llm.Usage, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return llm.Usage{}, fmt.Errorf("invalid assistant usage")
	}
	input, err := requiredUint64(object, "input")
	if err != nil {
		return llm.Usage{}, err
	}
	output, err := requiredUint64(object, "output")
	if err != nil {
		return llm.Usage{}, err
	}
	cacheRead, err := requiredUint64(object, "cacheRead")
	if err != nil {
		return llm.Usage{}, err
	}
	cacheWrite, err := requiredUint64(object, "cacheWrite")
	if err != nil {
		return llm.Usage{}, err
	}
	var reasoning *uint64
	if value, exists := object["reasoning"]; exists {
		decoded, err := decodeUint64(value)
		if err != nil {
			return llm.Usage{}, fmt.Errorf("invalid usage reasoning")
		}
		reasoning = &decoded
	}
	var cacheWrite1h *uint64
	if value, exists := object["cacheWrite1h"]; exists {
		decoded, err := decodeUint64(value)
		if err != nil {
			return llm.Usage{}, fmt.Errorf("invalid usage cacheWrite1h")
		}
		cacheWrite1h = &decoded
	}
	usage, err := llm.NewUsage(llm.UsageSpec{
		Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite,
		Reasoning: reasoning, CacheWrite1h: cacheWrite1h,
	})
	if err != nil {
		return llm.Usage{}, err
	}
	if value, exists := object["totalTokens"]; exists {
		total, err := decodeUint64(value)
		if err != nil || total != usage.TotalTokens() {
			return llm.Usage{}, fmt.Errorf("invalid usage totalTokens")
		}
	}
	costRaw, exists := object["cost"]
	if !exists {
		return llm.Usage{}, fmt.Errorf("assistant usage is missing cost")
	}
	var cost UsageCost
	if err := json.Unmarshal(costRaw, &cost); err != nil {
		return llm.Usage{}, fmt.Errorf("invalid assistant usage cost")
	}
	if err := validateUsageCost(cost); err != nil {
		return llm.Usage{}, err
	}
	return usage, nil
}

func decodeMessageTimestamp(object map[string]json.RawMessage) (time.Time, error) {
	raw, exists := object["timestamp"]
	if !exists {
		return time.Time{}, fmt.Errorf("message is missing timestamp")
	}
	value, err := decodeInt64(raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid message timestamp")
	}
	return time.UnixMilli(value).UTC(), nil
}

func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || !utf8.Valid(raw) {
		return nil, fmt.Errorf("invalid JSON object")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("invalid JSON object")
	}
	return object, nil
}

func requiredString(object map[string]json.RawMessage, key string) (string, error) {
	raw, exists := object[key]
	if !exists {
		return "", fmt.Errorf("missing %s", key)
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || !utf8.ValidString(value) {
		return "", fmt.Errorf("invalid %s", key)
	}
	return value, nil
}

func requiredUint64(object map[string]json.RawMessage, key string) (uint64, error) {
	raw, exists := object[key]
	if !exists {
		return 0, fmt.Errorf("missing usage %s", key)
	}
	value, err := decodeUint64(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid usage %s", key)
	}
	return value, nil
}

func decodeUint64(raw []byte) (uint64, error) {
	text := string(bytes.TrimSpace(raw))
	if text == "" || strings.ContainsAny(text, ".eE+-") {
		return 0, fmt.Errorf("not an unsigned integer")
	}
	return strconv.ParseUint(text, 10, 64)
}

func decodeInt64(raw []byte) (int64, error) {
	text := string(bytes.TrimSpace(raw))
	if text == "" || strings.ContainsAny(text, ".eE+") {
		return 0, fmt.Errorf("not an integer")
	}
	return strconv.ParseInt(text, 10, 64)
}

func parseError(kind error, path string, line int, message string, cause error) error {
	if cause != nil {
		return fmt.Errorf("%w: %s: line %d: %s: %w", kind, path, line, message, cause)
	}
	return fmt.Errorf("%w: %s: line %d: %s", kind, path, line, message)
}

package provider

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cat3399/pi-go/internal/llm"
)

type responsesRequestPayload struct {
	Model  string                  `json:"model"`
	Input  []any                   `json:"input"`
	Tools  []responsesFunctionTool `json:"tools,omitempty"`
	Stream bool                    `json:"stream"`
	Store  bool                    `json:"store"`
}

type responsesFunctionTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type responsesEasyMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type responsesInputText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesOutputText struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type responsesOutputMessage struct {
	Type    string                `json:"type"`
	Role    string                `json:"role"`
	Content []responsesOutputText `json:"content"`
	Status  string                `json:"status"`
	ID      string                `json:"id"`
}

func encodeOpenAIResponsesRequest(request Request, systemRole string) ([]byte, error) {
	input := make([]any, 0, len(request.Messages())+1)
	if request.SystemPrompt() != "" {
		input = append(input, responsesEasyMessage{
			Role:    systemRole,
			Content: request.SystemPrompt(),
		})
	}
	wireMessageIndex := 0
	for sourceIndex, message := range request.Messages() {
		switch message := message.(type) {
		case llm.UserTextMessage:
			blocks := message.Content()
			if len(blocks) == 0 {
				continue
			}
			content := make([]responsesInputText, len(blocks))
			for index, block := range blocks {
				content[index] = responsesInputText{Type: "input_text", Text: block.Text()}
			}
			input = append(input, responsesEasyMessage{Role: "user", Content: content})
			wireMessageIndex++

		case llm.AssistantTextMessage:
			blocks := message.Content()
			if len(blocks) == 0 {
				continue
			}
			input = appendResponsesAssistantText(input, wireMessageIndex, blocks)
			wireMessageIndex++

		case llm.AssistantFailureMessage:
			// Failed and aborted assistant turns may retain partial text for the
			// transcript, but that text was never a completed model response and
			// must not be acknowledged as one on the next request.
			continue

		case llm.AssistantToolUseMessage:
			encoded, err := appendResponsesAssistantToolUse(input, wireMessageIndex, message)
			if err != nil {
				return nil, fmt.Errorf("%w: message %d: %w", ErrOpenAIResponsesRequest, sourceIndex, err)
			}
			input = encoded
			wireMessageIndex++

		case llm.ToolResultMessage:
			output, err := responsesToolResultOutput(message)
			if err != nil {
				return nil, fmt.Errorf("%w: message %d: %w", ErrOpenAIResponsesRequest, sourceIndex, err)
			}
			callID, _ := splitResponsesToolID(message.ToolCallID())
			input = append(input, responsesFunctionCallOutput{Type: "function_call_output", CallID: callID, Output: output})
			wireMessageIndex++

		default:
			return nil, fmt.Errorf(
				"%w: message %d has unsupported type %T",
				ErrOpenAIResponsesRequest,
				sourceIndex,
				message,
			)
		}
	}
	tools, err := encodeResponsesTools(request.Tools())
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(responsesRequestPayload{
		Model:  request.Model().ID(),
		Input:  input,
		Tools:  tools,
		Stream: true,
		Store:  false,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: encode JSON: %w", ErrOpenAIResponsesRequest, err)
	}
	return payload, nil
}

type responsesFunctionCall struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type responsesFunctionCallOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

func appendResponsesAssistantToolUse(input []any, messageIndex int, message llm.AssistantToolUseMessage) ([]any, error) {
	textBlockIndex := 0
	seenCallIDs := make(map[string]struct{})
	for _, block := range message.Blocks() {
		switch block := block.(type) {
		case llm.TextBlock:
			input = appendResponsesAssistantText(input, messageIndex, []llm.TextBlock{block})
			textBlockIndex++
			// appendResponsesAssistantText has deterministic fallback IDs. For a
			// mixed message its only input is this one text block, so rewrite the
			// final ID when this is not the first text block.
			if textBlockIndex > 1 {
				last := input[len(input)-1].(responsesOutputMessage)
				last.ID = fmt.Sprintf("msg_pi_%d_%d", messageIndex, textBlockIndex-1)
				input[len(input)-1] = last
			}
		case llm.ToolCallBlock:
			callID, itemID := splitResponsesToolID(block.ID())
			if _, duplicate := seenCallIDs[callID]; duplicate {
				return nil, fmt.Errorf("duplicate normalized tool call ID %q", callID)
			}
			seenCallIDs[callID] = struct{}{}
			input = append(input, responsesFunctionCall{
				Type:      "function_call",
				ID:        normalizeResponsesFunctionItemID(itemID),
				CallID:    callID,
				Name:      block.Name(),
				Arguments: string(block.ArgumentsJSON()),
			})
		default:
			return nil, fmt.Errorf("unsupported assistant block %T", block)
		}
	}
	return input, nil
}

func responsesToolResultOutput(message llm.ToolResultMessage) (string, error) {
	blocks := message.Content()
	parts := make([]string, len(blocks))
	for index, block := range blocks {
		parts[index] = block.Text()
	}
	output := strings.Join(parts, "\n")
	// OpenAI's function_call_output cannot distinguish no block from an empty
	// text block. Both map to the upstream-compatible explicit placeholder.
	if output == "" {
		output = "(no tool output)"
	}
	return output, nil
}

func encodeResponsesTools(definitions []ToolDefinition) ([]responsesFunctionTool, error) {
	tools := make([]responsesFunctionTool, len(definitions))
	for index, definition := range definitions {
		if err := definition.validate(); err != nil {
			return nil, fmt.Errorf("%w: tool %d: %w", ErrOpenAIResponsesRequest, index, err)
		}
		tools[index] = responsesFunctionTool{
			Type:        "function",
			Name:        definition.Name(),
			Description: definition.Description(),
			Parameters:  json.RawMessage(definition.ParametersJSON()),
			Strict:      definition.Strict(),
		}
	}
	return tools, nil
}

func splitResponsesToolID(value string) (callID, itemID string) {
	callID, itemID, _ = strings.Cut(value, "|")
	return normalizeResponsesCallID(callID), itemID
}

func normalizeResponsesCallID(value string) string {
	value = normalizeResponsesIDPart(value)
	if value == "" {
		return "call_pi"
	}
	return value
}

func normalizeResponsesFunctionItemID(value string) string {
	if value == "" {
		return ""
	}
	normalized := normalizeResponsesIDPart(value)
	if strings.HasPrefix(normalized, "fc_") {
		return normalized
	}
	// Without provider/API provenance the request cannot prove that an item is
	// native. Hash every non-fc shape from its full raw value so very long or
	// punctuation-heavy foreign IDs remain bounded, stable, and do not collide
	// merely because their normalized 64-byte prefixes are equal.
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("fc_%x", sum[:12])
}

func normalizeResponsesIDPart(value string) string {
	var result strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			result.WriteRune(character)
		} else {
			result.WriteByte('_')
		}
	}
	normalized := result.String()
	if len(normalized) > 64 {
		normalized = normalized[:64]
	}
	return strings.TrimRight(normalized, "_")
}

func appendResponsesAssistantText(
	input []any,
	messageIndex int,
	blocks []llm.TextBlock,
) []any {
	for blockIndex, block := range blocks {
		id := fmt.Sprintf("msg_pi_%d", messageIndex)
		if blockIndex != 0 {
			id = fmt.Sprintf("msg_pi_%d_%d", messageIndex, blockIndex)
		}
		input = append(input, responsesOutputMessage{
			Type: "message",
			Role: "assistant",
			Content: []responsesOutputText{{
				Type:        "output_text",
				Text:        block.Text(),
				Annotations: []any{},
			}},
			Status: "completed",
			ID:     id,
		})
	}
	return input
}

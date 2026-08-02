package provider

import (
	"crypto/sha256"
	"encoding/base64"
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
type responsesInputImage struct {
	Type     string `json:"type"`
	Detail   string `json:"detail"`
	ImageURL string `json:"image_url"`
}
type responsesReasoningInput struct {
	Type             string                      `json:"type"`
	ID               string                      `json:"id"`
	EncryptedContent string                      `json:"encrypted_content,omitempty"`
	Summary          []responsesReasoningSummary `json:"summary,omitempty"`
}
type responsesReasoningSummary struct {
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
	Phase   string                `json:"phase,omitempty"`
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
		case llm.UserContentMessage:
			content, err := responsesInputContent(message.Content())
			if err != nil {
				return nil, fmt.Errorf("%w: message %d: %w", ErrOpenAIResponsesRequest, sourceIndex, err)
			}
			if len(content) != 0 {
				input = append(input, responsesEasyMessage{Role: "user", Content: content})
				wireMessageIndex++
			}

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
		case llm.AssistantRichMessage:
			encoded, err := appendResponsesAssistantBlocks(input, wireMessageIndex, message.Blocks())
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
		case llm.ToolResultContentMessage:
			output, err := responsesToolResultContentOutput(message.Content())
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
	Output any    `json:"output"`
}

func appendResponsesAssistantToolUse(input []any, messageIndex int, message llm.AssistantToolUseMessage) ([]any, error) {
	return appendResponsesAssistantBlocks(input, messageIndex, message.Blocks())
}
func appendResponsesAssistantBlocks(input []any, messageIndex int, blocks []llm.AssistantBlock) ([]any, error) {
	textBlockIndex := 0
	seenCallIDs := make(map[string]struct{})
	for _, block := range blocks {
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
		case llm.ThinkingBlock:
			replay, ok := block.OpenAIResponsesReplay()
			if !ok {
				if strings.TrimSpace(block.Thinking()) == "" {
					continue
				}
				text, err := llm.NewTextBlock(block.Thinking())
				if err != nil {
					return nil, err
				}
				input = appendResponsesAssistantText(input, messageIndex, []llm.TextBlock{text})
				textBlockIndex++
				continue
			}
			reasoning := responsesReasoningInput{Type: "reasoning", ID: replay.ItemID, EncryptedContent: replay.EncryptedContent}
			if block.Thinking() != "" {
				reasoning.Summary = []responsesReasoningSummary{{Type: "summary_text", Text: block.Thinking()}}
			}
			input = append(input, reasoning)
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

func responsesInputContent(blocks []llm.UserContentBlock) ([]any, error) {
	content := make([]any, 0, len(blocks))
	for _, block := range blocks {
		switch block := block.(type) {
		case llm.TextBlock:
			content = append(content, responsesInputText{Type: "input_text", Text: block.Text()})
		case llm.ImageBlock:
			content = append(content, responsesImageInput(block))
		default:
			return nil, fmt.Errorf("unsupported user block %T", block)
		}
	}
	return content, nil
}
func responsesImageInput(image llm.ImageBlock) responsesInputImage {
	url := image.URL()
	if image.Source() == llm.ImageSourceData {
		url = "data:" + image.MediaType() + ";base64," + base64.StdEncoding.EncodeToString(image.Data())
	}
	return responsesInputImage{Type: "input_image", Detail: "auto", ImageURL: url}
}

func responsesToolResultOutput(message llm.ToolResultMessage) (any, error) {
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
func responsesToolResultContentOutput(blocks []llm.ToolResultContentBlock) (any, error) {
	texts := make([]string, 0, len(blocks))
	images := make([]llm.ImageBlock, 0)
	for _, block := range blocks {
		switch block := block.(type) {
		case llm.TextBlock:
			texts = append(texts, block.Text())
		case llm.ImageBlock:
			images = append(images, block)
		default:
			return nil, fmt.Errorf("unsupported tool result block %T", block)
		}
	}
	text := strings.Join(texts, "\n")
	if len(images) == 0 {
		if text == "" {
			text = "(no tool output)"
		}
		return text, nil
	}
	out := make([]any, 0, len(images)+1)
	if text != "" {
		out = append(out, responsesInputText{Type: "input_text", Text: text})
	}
	for _, image := range images {
		out = append(out, responsesImageInput(image))
	}
	return out, nil
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
	value = normalizeResponsesIDPart(value)
	if value == "" {
		return ""
	}
	if !strings.HasPrefix(value, "fc_") {
		sum := sha256.Sum256([]byte(value))
		return fmt.Sprintf("fc_%x", sum[:12])
	}
	return value
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
	normalized := strings.TrimRight(result.String(), "_")
	if len(normalized) > 64 {
		normalized = normalized[:64]
	}
	return normalized
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
		message := responsesOutputMessage{
			Type: "message",
			Role: "assistant",
			Content: []responsesOutputText{{
				Type:        "output_text",
				Text:        block.Text(),
				Annotations: []any{},
			}},
			Status: "completed",
			ID:     id,
		}
		if replay, ok := block.TextReplay(); ok {
			message.ID = replay.MessageID
			message.Phase = replay.Phase
		}
		input = append(input, message)
	}
	return input
}

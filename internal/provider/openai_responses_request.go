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
	Model             string                     `json:"model"`
	Input             []any                      `json:"input"`
	Tools             []responsesFunctionTool    `json:"tools,omitempty"`
	ParallelToolCalls bool                       `json:"parallel_tool_calls"`
	Stream            bool                       `json:"stream"`
	Store             bool                       `json:"store"`
	Reasoning         *responsesReasoningOptions `json:"reasoning,omitempty"`
	Include           []string                   `json:"include,omitempty"`
	MaxOutputTokens   uint64                     `json:"max_output_tokens,omitempty"`
}

type responsesReasoningOptions struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary,omitempty"`
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
	Content          string                      `json:"content,omitempty"`
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
			policy := responsesReplayPolicyFor(message, request.ReplayTarget())
			input = appendResponsesAssistantText(input, wireMessageIndex, blocks, policy.sameModel)
			wireMessageIndex++

		case llm.AssistantFailureMessage:
			// Failed and aborted assistant turns may retain partial text for the
			// transcript, but that text was never a completed model response and
			// must not be acknowledged as one on the next request.
			continue

		case llm.AssistantToolUseMessage:
			encoded, err := appendResponsesAssistantToolUse(input, wireMessageIndex, message, responsesReplayPolicyFor(message, request.ReplayTarget()))
			if err != nil {
				return nil, fmt.Errorf("%w: message %d: %w", ErrOpenAIResponsesRequest, sourceIndex, err)
			}
			input = encoded
			wireMessageIndex++
		case llm.AssistantRichMessage:
			encoded, err := appendResponsesAssistantBlocks(input, wireMessageIndex, message.Blocks(), responsesReplayPolicyFor(message, request.ReplayTarget()))
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
	payloadValue := responsesRequestPayload{
		Model:             request.Model().ID(),
		Input:             input,
		Tools:             tools,
		ParallelToolCalls: request.ParallelToolCalls(),
		Stream:            true,
		Store:             false,
	}
	if effort, enabled := request.Model().ThinkingEffort(request.ThinkingLevel()); enabled {
		payloadValue.Reasoning = &responsesReasoningOptions{Effort: effort}
		if request.ThinkingLevel() != "" && request.ThinkingLevel() != ThinkingOff {
			payloadValue.Reasoning.Summary = "auto"
			payloadValue.Include = []string{"reasoning.encrypted_content"}
		}
	}
	maxTokens := request.Model().MaxTokens()
	if request.StreamOptions().MaxTokens != 0 {
		maxTokens = request.StreamOptions().MaxTokens
	}
	if maxTokens != 0 {
		if maxTokens < 16 {
			maxTokens = 16
		}
		payloadValue.MaxOutputTokens = maxTokens
	}
	payload, err := json.Marshal(payloadValue)
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

type responsesReplayPolicy struct{ sourced, sameDialect, sameModel bool }
type responsesProvenanceCarrier interface {
	AssistantProvenance() (llm.AssistantProvenance, bool)
}

func responsesReplayPolicyFor(message responsesProvenanceCarrier, target llm.AssistantProvenance) responsesReplayPolicy {
	source, ok := message.AssistantProvenance()
	if !ok {
		return responsesReplayPolicy{}
	}
	sameDialect := source.Provider == target.Provider && source.API == target.API
	return responsesReplayPolicy{
		sourced:     true,
		sameDialect: sameDialect,
		sameModel:   source.Matches(target.Provider, target.API, target.Model),
	}
}

func appendResponsesAssistantToolUse(input []any, messageIndex int, message llm.AssistantToolUseMessage, policy responsesReplayPolicy) ([]any, error) {
	return appendResponsesAssistantBlocks(input, messageIndex, message.Blocks(), policy)
}
func appendResponsesAssistantBlocks(input []any, messageIndex int, blocks []llm.AssistantBlock, policy responsesReplayPolicy) ([]any, error) {
	textBlockIndex := 0
	seenCallIDs := make(map[string]struct{})
	for _, block := range blocks {
		switch block := block.(type) {
		case llm.TextBlock:
			input = appendResponsesAssistantTextAt(input, messageIndex, textBlockIndex, block, policy.sameModel)
			textBlockIndex++
		case llm.ThinkingBlock:
			replay, ok := block.OpenAIResponsesReplay()
			if !ok || !policy.sameModel {
				if replay.Redacted || strings.TrimSpace(block.Thinking()) == "" {
					continue
				}
				text, err := llm.NewTextBlock(block.Thinking())
				if err != nil {
					return nil, err
				}
				input = appendResponsesAssistantTextAt(input, messageIndex, textBlockIndex, text, false)
				textBlockIndex++
				continue
			}
			reasoning := responsesReasoningInput{Type: "reasoning", ID: replay.ItemID, EncryptedContent: replay.EncryptedContent}
			plaintext := replay.PlaintextContent != ""
			if plaintext {
				reasoning.EncryptedContent = ""
				reasoning.Content = replay.PlaintextContent
				if reasoning.Content == "" {
					reasoning.Content = block.Thinking()
				}
			} else if block.Thinking() != "" {
				reasoning.Summary = []responsesReasoningSummary{{Type: "summary_text", Text: block.Thinking()}}
			}
			input = append(input, reasoning)
		case llm.ToolCallBlock:
			callID, itemID := splitResponsesToolID(block.ID())
			if _, duplicate := seenCallIDs[callID]; duplicate {
				return nil, fmt.Errorf("duplicate normalized tool call ID %q", callID)
			}
			seenCallIDs[callID] = struct{}{}
			itemID = responsesReplayToolItemID(itemID, policy)
			input = append(input, responsesFunctionCall{
				Type:      "function_call",
				ID:        itemID,
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

func responsesReplayToolItemID(itemID string, policy responsesReplayPolicy) string {
	if itemID == "" {
		return ""
	}
	if policy.sameModel {
		return normalizeResponsesFunctionItemID(itemID)
	}
	if policy.sourced && policy.sameDialect {
		return ""
	}
	sum := sha256.Sum256([]byte(itemID))
	return fmt.Sprintf("fc_%x", sum[:12])
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
	allowReplay bool,
) []any {
	for blockIndex, block := range blocks {
		input = appendResponsesAssistantTextAt(input, messageIndex, blockIndex, block, allowReplay)
	}
	return input
}
func appendResponsesAssistantTextAt(input []any, messageIndex, blockIndex int, block llm.TextBlock, allowReplay bool) []any {
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
	if replay, ok := block.TextReplay(); ok && allowReplay {
		message.ID = replay.MessageID
		message.Phase = replay.Phase
	}
	return append(input, message)
}

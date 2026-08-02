package provider

import (
	"encoding/json"
	"fmt"

	"github.com/cat3399/pi-go/internal/llm"
)

type responsesRequestPayload struct {
	Model  string `json:"model"`
	Input  []any  `json:"input"`
	Stream bool   `json:"stream"`
	Store  bool   `json:"store"`
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

		case llm.AssistantToolUseMessage, llm.ToolResultMessage:
			return nil, fmt.Errorf(
				"%w: message %d (%s): %w: tool replay is outside the text adapter milestone",
				ErrOpenAIResponsesRequest,
				sourceIndex,
				message.Role(),
				ErrOpenAIResponsesUnsupported,
			)

		default:
			return nil, fmt.Errorf(
				"%w: message %d has unsupported type %T",
				ErrOpenAIResponsesRequest,
				sourceIndex,
				message,
			)
		}
	}
	payload, err := json.Marshal(responsesRequestPayload{
		Model:  request.Model().ID(),
		Input:  input,
		Stream: true,
		Store:  false,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: encode JSON: %w", ErrOpenAIResponsesRequest, err)
	}
	return payload, nil
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

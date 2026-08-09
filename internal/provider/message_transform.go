package provider

import (
	"fmt"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

// transformConversationMessages performs pi's provider-neutral second-pass
// transcript repair. Failed/aborted assistant turns are not replayable, and a
// missing result is represented explicitly before the next assistant/user turn
// (or at end of history) so signed reasoning and tool-call ordering remain
// valid for strict provider APIs.
func transformConversationMessages(messages []llm.ConversationMessage) ([]llm.ConversationMessage, error) {
	result := make([]llm.ConversationMessage, 0, len(messages))
	var pending []llm.ToolCallBlock
	existingResults := map[string]struct{}{}

	insertSynthetic := func() error {
		for _, call := range pending {
			if _, exists := existingResults[call.ID()]; exists {
				continue
			}
			block, err := llm.NewTextBlock("No result provided")
			if err != nil {
				return err
			}
			message, err := llm.NewToolResultMessage(call.ID(), call.Name(), []llm.TextBlock{block}, true, time.Now())
			if err != nil {
				return err
			}
			result = append(result, message)
		}
		pending = nil
		existingResults = map[string]struct{}{}
		return nil
	}

	for _, message := range messages {
		switch value := message.(type) {
		case llm.AssistantFailureMessage:
			if err := insertSynthetic(); err != nil {
				return nil, fmt.Errorf("repair incomplete tool history: %w", err)
			}
			continue
		case llm.AssistantToolUseMessage:
			if err := insertSynthetic(); err != nil {
				return nil, fmt.Errorf("repair incomplete tool history: %w", err)
			}
			for _, block := range value.Blocks() {
				if call, ok := block.(llm.ToolCallBlock); ok {
					pending = append(pending, call)
				}
			}
			result = append(result, message)
		case llm.AssistantTextMessage, llm.AssistantRichMessage:
			if err := insertSynthetic(); err != nil {
				return nil, fmt.Errorf("repair incomplete tool history: %w", err)
			}
			result = append(result, message)
		case llm.ToolResultMessage:
			existingResults[value.ToolCallID()] = struct{}{}
			result = append(result, message)
		case llm.ToolResultContentMessage:
			existingResults[value.ToolCallID()] = struct{}{}
			result = append(result, message)
		case llm.UserTextMessage, llm.UserContentMessage:
			if err := insertSynthetic(); err != nil {
				return nil, fmt.Errorf("repair incomplete tool history: %w", err)
			}
			result = append(result, message)
		default:
			result = append(result, message)
		}
	}
	if err := insertSynthetic(); err != nil {
		return nil, fmt.Errorf("repair incomplete tool history: %w", err)
	}
	return result, nil
}

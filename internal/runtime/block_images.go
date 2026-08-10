package agentruntime

import (
	"context"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

const blockedImagePlaceholder = "Image reading is disabled."

func convertToLLMWithBlockImages(base agent.AgentLoopConvertToLLM, blocked func() bool) agent.AgentLoopConvertToLLM {
	if base == nil {
		base = func(_ context.Context, messages []agentmsg.Message) ([]llm.ConversationMessage, error) {
			return agentmsg.ConvertToLLM(messages)
		}
	}
	return func(ctx context.Context, messages []agentmsg.Message) ([]llm.ConversationMessage, error) {
		converted, err := base(ctx, messages)
		if err != nil || blocked == nil || !blocked() {
			return converted, err
		}
		return replaceConversationImages(converted)
	}
}

func replaceConversationImages(messages []llm.ConversationMessage) ([]llm.ConversationMessage, error) {
	result := make([]llm.ConversationMessage, len(messages))
	for index, message := range messages {
		var err error
		switch value := message.(type) {
		case llm.UserContentMessage:
			result[index], err = replaceUserMessageImages(value)
		case *llm.UserContentMessage:
			if value == nil {
				result[index] = message
				continue
			}
			result[index], err = replaceUserMessageImages(*value)
		case llm.ToolResultContentMessage:
			result[index], err = replaceToolResultImages(value)
		case *llm.ToolResultContentMessage:
			if value == nil {
				result[index] = message
				continue
			}
			result[index], err = replaceToolResultImages(*value)
		default:
			result[index] = message
		}
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func replaceUserMessageImages(message llm.UserContentMessage) (llm.ConversationMessage, error) {
	content, changed, err := replaceUserContentImages(message.Content())
	if err != nil || !changed {
		return message, err
	}
	return llm.NewUserContentMessage(content, message.Timestamp())
}

func replaceToolResultImages(message llm.ToolResultContentMessage) (llm.ConversationMessage, error) {
	content, changed, err := replaceToolResultContentImages(message.Content())
	if err != nil || !changed {
		return message, err
	}
	var usage *llm.Usage
	if value, ok := message.Usage(); ok {
		usage = &value
	}
	return llm.NewToolResultContentMessageWithMetadata(
		message.ToolCallID(), message.ToolName(), content, message.IsError(), message.Timestamp(),
		llm.ToolResultMetadata{
			Details: message.Details(), Usage: usage, AddedToolNames: message.AddedToolNames(),
			HasAddedToolNames: message.HasAddedToolNames(),
		},
	)
}

func replaceUserContentImages(content []llm.UserContentBlock) ([]llm.UserContentBlock, bool, error) {
	hasImage := false
	for _, block := range content {
		if _, ok := block.(llm.ImageBlock); ok {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return content, false, nil
	}
	result := make([]llm.UserContentBlock, 0, len(content))
	for _, block := range content {
		if _, image := block.(llm.ImageBlock); image {
			placeholder, err := llm.NewTextBlock(blockedImagePlaceholder)
			if err != nil {
				return nil, false, err
			}
			if !lastUserBlockIsPlaceholder(result) {
				result = append(result, placeholder)
			}
			continue
		}
		if text, ok := block.(llm.TextBlock); ok && text.Text() == blockedImagePlaceholder && lastUserBlockIsPlaceholder(result) {
			continue
		}
		result = append(result, block)
	}
	return result, true, nil
}

func replaceToolResultContentImages(content []llm.ToolResultContentBlock) ([]llm.ToolResultContentBlock, bool, error) {
	hasImage := false
	for _, block := range content {
		if _, ok := block.(llm.ImageBlock); ok {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return content, false, nil
	}
	result := make([]llm.ToolResultContentBlock, 0, len(content))
	for _, block := range content {
		if _, image := block.(llm.ImageBlock); image {
			placeholder, err := llm.NewTextBlock(blockedImagePlaceholder)
			if err != nil {
				return nil, false, err
			}
			if !lastToolResultBlockIsPlaceholder(result) {
				result = append(result, placeholder)
			}
			continue
		}
		if text, ok := block.(llm.TextBlock); ok && text.Text() == blockedImagePlaceholder && lastToolResultBlockIsPlaceholder(result) {
			continue
		}
		result = append(result, block)
	}
	return result, true, nil
}

func lastUserBlockIsPlaceholder(content []llm.UserContentBlock) bool {
	if len(content) == 0 {
		return false
	}
	text, ok := content[len(content)-1].(llm.TextBlock)
	return ok && text.Text() == blockedImagePlaceholder
}

func lastToolResultBlockIsPlaceholder(content []llm.ToolResultContentBlock) bool {
	if len(content) == 0 {
		return false
	}
	text, ok := content[len(content)-1].(llm.TextBlock)
	return ok && text.Text() == blockedImagePlaceholder
}

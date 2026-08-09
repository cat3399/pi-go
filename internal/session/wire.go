package session

import (
	"encoding/json"
	"fmt"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

// MarshalAgentMessage emits pi's public AgentMessage JSON shape. Session
// persistence and RPC events intentionally share this encoder so a web host
// cannot drift from the durable transcript vocabulary.
func MarshalAgentMessage(message agentmsg.Message) (json.RawMessage, error) {
	switch value := message.(type) {
	case agentmsg.LLM:
		return encodeMessage(value.Conversation(), AppendOptions{})
	case agentmsg.AssistantPartial:
		return marshalAssistantPartial(value)
	case agentmsg.BranchSummary:
		return json.Marshal(map[string]any{
			"role": "branchSummary", "summary": value.Summary,
			"fromId": value.FromID, "timestamp": value.Timestamp().UnixMilli(),
		})
	case agentmsg.CompactionSummary:
		return json.Marshal(map[string]any{
			"role": "compactionSummary", "summary": value.Summary,
			"tokensBefore": value.TokensBefore, "timestamp": value.Timestamp().UnixMilli(),
		})
	default:
		return encodeAgentMessage(message)
	}
}

// MarshalToolResultContent emits pi's TextContent/ImageContent array used by
// AgentToolResult events.
func MarshalToolResultContent(content []llm.ToolResultContentBlock) ([]json.RawMessage, error) {
	return encodeToolResultContentBlocks(content)
}

// MarshalUsage emits pi's provider-neutral Usage object.
func MarshalUsage(usage llm.Usage) (json.RawMessage, error) {
	return encodePortableUsage(usage)
}

func marshalAssistantPartial(message agentmsg.AssistantPartial) (json.RawMessage, error) {
	content := make([]json.RawMessage, 0, len(message.Blocks()))
	for _, block := range message.Blocks() {
		var (
			raw json.RawMessage
			err error
		)
		switch value := block.(type) {
		case llm.TextBlock:
			wire := textBlockWire{Type: "text", Text: value.Text()}
			wire.TextSignature, _ = value.TextSignature()
			raw, err = json.Marshal(wire)
		case llm.ThinkingBlock:
			wire := thinkingBlockWire{Type: "thinking", Thinking: value.Thinking(), Redacted: value.Redacted()}
			wire.ThinkingSignature, _ = value.ThinkingSignature()
			raw, err = json.Marshal(wire)
		case llm.PartialThinkingBlock:
			raw, err = json.Marshal(map[string]any{"type": "thinking", "thinking": value.Thinking()})
		case llm.ToolCallBlock:
			raw, err = encodeToolCallBlock(value)
		case llm.PartialToolCallBlock:
			raw, err = json.Marshal(map[string]any{
				"type": "toolCall", "id": value.ID(), "name": value.Name(),
				"arguments": parseStreamingJSONObject(value.ArgumentsFragment()),
			})
		default:
			return nil, fmt.Errorf("unsupported partial assistant block %T", block)
		}
		if err != nil {
			return nil, err
		}
		content = append(content, raw)
	}
	usage, err := encodePortableUsage(message.Usage())
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Role       string            `json:"role"`
		Content    []json.RawMessage `json:"content"`
		API        string            `json:"api"`
		Provider   string            `json:"provider"`
		Model      string            `json:"model"`
		Usage      json.RawMessage   `json:"usage"`
		StopReason string            `json:"stopReason"`
		Timestamp  int64             `json:"timestamp"`
	}{
		Role: "assistant", Content: content, API: message.API(), Provider: message.Provider(),
		Model: message.Model(), Usage: usage, StopReason: message.FinishReason().String(),
		Timestamp: message.Timestamp().UnixMilli(),
	})
}

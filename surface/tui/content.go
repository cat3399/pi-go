package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
)

// contentItem is the terminal-independent conversation projection consumed by
// the TUI renderer. It deliberately retains semantic blocks instead of
// flattening an AgentMessage to one string, so later image, diff, tool, and
// extension renderers do not require changes to the application contract.
type contentItem struct {
	ID        string
	Revision  uint64
	Role      contentRole
	Title     string
	Timestamp time.Time
	Blocks    []contentBlock
	Live      bool
	Failed    bool
}

type contentRole uint8

const (
	contentRoleUser contentRole = iota + 1
	contentRoleAssistant
	contentRoleTool
	contentRoleSystem
)

type contentBlockKind uint8

const (
	contentBlockText contentBlockKind = iota + 1
	contentBlockMarkdown
	contentBlockThinking
	contentBlockToolCall
	contentBlockToolResult
	contentBlockCode
	contentBlockImage
	contentBlockNotice
)

type contentBlock struct {
	Kind        contentBlockKind
	Text        string
	Language    string
	ToolCallID  string
	ToolName    string
	ToolDetails json.RawMessage
	MediaType   string
	ByteSize    int
	IsError     bool
	Live        bool
}

func contentItemsFromSnapshot(snapshot application.SessionSnapshot) []contentItem {
	items := make([]contentItem, 0, len(snapshot.Entries))
	toolOwners := make(map[string]int)
	for _, entry := range snapshot.Entries {
		item, ok := contentItemFromEntry(entry)
		if !ok {
			continue
		}
		if callID, resultBlocks, result := toolResultBlocks(item); result {
			if owner, found := toolOwners[callID]; found && mergeToolResultBlocks(&items[owner], callID, resultBlocks) {
				continue
			}
		}
		items = append(items, item)
		itemIndex := len(items) - 1
		for _, block := range item.Blocks {
			if block.Kind == contentBlockToolCall && block.ToolCallID != "" {
				toolOwners[block.ToolCallID] = itemIndex
			}
		}
	}
	return items
}

func contentItemFromEntry(entry session.Entry) (contentItem, bool) {
	if message, ok := entry.AgentMessage(); ok {
		return contentItemFromAgentMessage(entry.ID(), 1, message, false)
	}
	item := contentItem{
		ID: entry.ID(), Revision: 1, Role: contentRoleSystem,
		Timestamp: entry.Timestamp(),
	}
	switch payload := entry.Payload().(type) {
	case session.CompactionPayload:
		item.Title = "Compaction"
		item.Blocks = []contentBlock{{Kind: contentBlockMarkdown, Text: payload.Record.Summary}}
		return item, true
	case session.BranchSummaryPayload:
		item.Title = "Branch summary"
		item.Blocks = []contentBlock{{Kind: contentBlockMarkdown, Text: payload.Summary}}
		return item, true
	case session.CustomPayload:
		item.Title = payload.CustomType
		item.Blocks = []contentBlock{{Kind: contentBlockNotice, Text: compactJSON(payload.Data)}}
		return item, true
	default:
		return contentItem{}, false
	}
}

func contentItemFromAgentMessage(id string, revision uint64, message agentmsg.Message, live bool) (contentItem, bool) {
	if message == nil {
		return contentItem{}, false
	}
	item := contentItem{ID: id, Revision: revision, Timestamp: message.Timestamp(), Live: live}
	switch value := message.(type) {
	case agentmsg.LLM:
		return contentItemFromConversation(item, value.Conversation(), live)
	case agentmsg.AssistantPartial:
		item.Role = contentRoleAssistant
		item.Title = "Assistant"
		item.Live = true
		item.Blocks = assistantBlocks(value.Blocks(), true)
		return item, true
	case agentmsg.BashExecution:
		item.Role = contentRoleTool
		item.Title = "Bash"
		callID := "bash:" + id
		body := value.Output
		if body == "" {
			body = "(no output)"
		}
		item.Blocks = []contentBlock{
			{Kind: contentBlockToolCall, ToolCallID: callID, ToolName: "bash", Text: value.Command},
			{Kind: contentBlockToolResult, ToolCallID: callID, ToolName: "bash", Language: "text", Text: body, IsError: value.Cancelled || value.ExitCode != nil && *value.ExitCode != 0},
		}
		if value.Truncated && value.FullOutputPath != "" {
			item.Blocks = append(item.Blocks, contentBlock{
				Kind: contentBlockNotice, ToolCallID: callID, ToolName: "bash",
				Text: "Full output: " + value.FullOutputPath,
			})
		}
		return item, true
	case agentmsg.Custom:
		if !value.Display() {
			return contentItem{}, false
		}
		item.Role = contentRoleSystem
		item.Title = value.CustomType()
		item.Blocks = userContentBlocks(value.Content())
		if len(item.Blocks) == 0 {
			if text, ok := value.StringContent(); ok {
				item.Blocks = []contentBlock{{Kind: contentBlockText, Text: text}}
			}
		}
		return item, len(item.Blocks) != 0
	case agentmsg.BranchSummary:
		item.Role = contentRoleSystem
		item.Title = "Branch summary"
		item.Blocks = []contentBlock{{Kind: contentBlockMarkdown, Text: value.Summary}}
		return item, true
	case agentmsg.CompactionSummary:
		item.Role = contentRoleSystem
		item.Title = "Compaction"
		item.Blocks = []contentBlock{{Kind: contentBlockMarkdown, Text: value.Summary}}
		return item, true
	case agentmsg.OpaqueMessage:
		item.Role = contentRoleSystem
		item.Title = value.Type()
		item.Blocks = []contentBlock{{Kind: contentBlockNotice, Text: compactJSON(value.Data())}}
		return item, true
	default:
		item.Role = contentRoleSystem
		item.Title = "Unsupported message"
		item.Blocks = []contentBlock{{Kind: contentBlockNotice, Text: fmt.Sprintf("%T", message)}}
		return item, true
	}
}

func contentItemFromConversation(item contentItem, message llm.ConversationMessage, live bool) (contentItem, bool) {
	switch value := message.(type) {
	case llm.UserTextMessage:
		item.Role, item.Title = contentRoleUser, "You"
		for _, block := range value.Content() {
			item.Blocks = append(item.Blocks, contentBlock{Kind: contentBlockText, Text: block.Text()})
		}
	case llm.UserContentMessage:
		item.Role, item.Title = contentRoleUser, "You"
		item.Blocks = userContentBlocks(value.Content())
	case llm.ToolResultMessage:
		item.Role, item.Title = contentRoleTool, value.ToolName()
		details := value.Details()
		for _, block := range value.Content() {
			item.Blocks = append(item.Blocks, contentBlock{
				Kind: contentBlockToolResult, Text: block.Text(), ToolCallID: value.ToolCallID(),
				ToolName: value.ToolName(), ToolDetails: details, IsError: value.IsError(),
			})
		}
		if len(item.Blocks) == 0 {
			item.Blocks = []contentBlock{{
				Kind: contentBlockToolResult, ToolCallID: value.ToolCallID(), ToolName: value.ToolName(),
				ToolDetails: details, IsError: value.IsError(),
			}}
		}
	case llm.ToolResultContentMessage:
		item.Role, item.Title = contentRoleTool, value.ToolName()
		details := value.Details()
		for _, block := range value.Content() {
			switch block := block.(type) {
			case llm.TextBlock:
				item.Blocks = append(item.Blocks, contentBlock{
					Kind: contentBlockToolResult, Text: block.Text(), ToolCallID: value.ToolCallID(),
					ToolName: value.ToolName(), ToolDetails: details, IsError: value.IsError(),
				})
			case llm.ImageBlock:
				image := imageContentBlock(block)
				image.ToolCallID, image.ToolName = value.ToolCallID(), value.ToolName()
				image.ToolDetails, image.IsError = details, value.IsError()
				item.Blocks = append(item.Blocks, image)
			}
		}
		if len(item.Blocks) == 0 {
			item.Blocks = []contentBlock{{
				Kind: contentBlockToolResult, ToolCallID: value.ToolCallID(), ToolName: value.ToolName(),
				ToolDetails: details, IsError: value.IsError(),
			}}
		}
	case llm.AssistantTerminal:
		item.Role, item.Title = contentRoleAssistant, "Assistant"
		item.Blocks = assistantBlocks(value.Blocks(), live)
		if failure, ok := value.(llm.AssistantFailureMessage); ok {
			item.Failed = true
			if strings.TrimSpace(failure.ErrorMessage()) != "" {
				item.Blocks = append(item.Blocks, contentBlock{Kind: contentBlockNotice, Text: failure.ErrorMessage(), IsError: true})
			}
		}
	default:
		return contentItem{}, false
	}
	return item, true
}

func userContentBlocks(blocks []llm.UserContentBlock) []contentBlock {
	result := make([]contentBlock, 0, len(blocks))
	for _, block := range blocks {
		switch block := block.(type) {
		case llm.TextBlock:
			result = append(result, contentBlock{Kind: contentBlockText, Text: block.Text()})
		case llm.ImageBlock:
			result = append(result, imageContentBlock(block))
		}
	}
	return result
}

func assistantBlocks(blocks []llm.AssistantBlock, live bool) []contentBlock {
	result := make([]contentBlock, 0, len(blocks))
	for _, block := range blocks {
		switch block := block.(type) {
		case llm.TextBlock:
			kind := contentBlockMarkdown
			if live {
				kind = contentBlockText
			}
			result = append(result, contentBlock{Kind: kind, Text: block.Text(), Live: live})
		case llm.ThinkingBlock:
			result = append(result, contentBlock{Kind: contentBlockThinking, Text: block.Thinking(), Live: live})
		case llm.PartialThinkingBlock:
			result = append(result, contentBlock{Kind: contentBlockThinking, Text: block.Thinking(), Live: true})
		case llm.ToolCallBlock:
			result = append(result, contentBlock{
				Kind: contentBlockToolCall, Text: prettyJSON(block.ArgumentsJSON()),
				ToolCallID: block.ID(), ToolName: block.Name(), Live: live,
			})
		case llm.PartialToolCallBlock:
			result = append(result, contentBlock{
				Kind: contentBlockToolCall, Text: string(block.ArgumentsFragment()),
				ToolCallID: block.ID(), ToolName: block.Name(), Live: true,
			})
		}
	}
	return result
}

func imageContentBlock(image llm.ImageBlock) contentBlock {
	size := 0
	if image.Source() == llm.ImageSourceData {
		size = len(image.Data())
	}
	return contentBlock{Kind: contentBlockImage, MediaType: image.MediaType(), ByteSize: size}
}

func prettyJSON(data []byte) string {
	if len(data) == 0 {
		return "{}"
	}
	var value any
	if json.Unmarshal(data, &value) != nil {
		return string(data)
	}
	formatted, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return string(data)
	}
	return string(formatted)
}

func compactJSON(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	var value any
	if json.Unmarshal(data, &value) != nil {
		return string(data)
	}
	formatted, err := json.Marshal(value)
	if err != nil {
		return string(data)
	}
	return string(formatted)
}

func toolResultBlocks(item contentItem) (string, []contentBlock, bool) {
	if item.Role != contentRoleTool || len(item.Blocks) == 0 {
		return "", nil, false
	}
	callID := ""
	for _, block := range item.Blocks {
		if block.Kind == contentBlockToolCall {
			return "", nil, false
		}
		if block.ToolCallID == "" {
			continue
		}
		if callID == "" {
			callID = block.ToolCallID
		} else if callID != block.ToolCallID {
			return "", nil, false
		}
	}
	if callID == "" {
		return "", nil, false
	}
	return callID, append([]contentBlock(nil), item.Blocks...), true
}

func mergeToolResultBlocks(item *contentItem, callID string, result []contentBlock) bool {
	if item == nil || callID == "" {
		return false
	}
	callIndex := -1
	blocks := make([]contentBlock, 0, len(item.Blocks)+len(result))
	for _, block := range item.Blocks {
		if block.Kind == contentBlockToolCall && block.ToolCallID == callID {
			callIndex = len(blocks)
			block.Live = false
			blocks = append(blocks, block)
			continue
		}
		if block.ToolCallID == callID {
			continue
		}
		blocks = append(blocks, block)
	}
	if callIndex < 0 {
		return false
	}
	owned := append([]contentBlock(nil), result...)
	blocks = append(blocks, make([]contentBlock, len(owned))...)
	copy(blocks[callIndex+1+len(owned):], blocks[callIndex+1:len(blocks)-len(owned)])
	copy(blocks[callIndex+1:], owned)
	item.Blocks = blocks
	item.Revision++
	for _, block := range owned {
		item.Failed = item.Failed || block.IsError
	}
	return true
}

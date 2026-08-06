package agent

import (
	"fmt"
	"math"
	"math/bits"
	"strings"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
	"github.com/cat3399/pi-go/internal/session"
)

// UserMessageForForking is one durable user-message candidate from the full
// session entry log. EntryID identifies the exact point a fork selector chose.
type UserMessageForForking struct {
	EntryID string
	Text    string
}

// SessionTokenTotals is the component-wise total of every billed usage record
// in a session, including history no longer present in the active context.
type SessionTokenTotals struct {
	Input      uint64
	Output     uint64
	CacheRead  uint64
	CacheWrite uint64
	Total      uint64
}

// ContextUsage reports the current selected model's context occupancy. Nil
// Tokens and Percent mean that the value is temporarily unknown immediately
// after compaction, until a successful assistant response reports fresh usage.
type ContextUsage struct {
	Tokens        *uint64
	ContextWindow uint64
	Percent       *float64
}

// SessionStats mirrors coding-agent's all-entry session statistics. A nil
// SessionFile represents an in-memory session, and a nil ContextUsage means no
// model with a usable context window is selected.
type SessionStats struct {
	SessionFile       *string
	SessionID         string
	UserMessages      uint64
	AssistantMessages uint64
	ToolCalls         uint64
	ToolResults       uint64
	TotalMessages     uint64
	Tokens            SessionTokenTotals
	Cost              float64
	ContextUsage      *ContextUsage
}

// GetUserMessagesForForking enumerates user messages from every durable entry,
// not just the currently selected branch. Text blocks are concatenated with no
// separator, matching contentText(message.content, "") in coding-agent.
func (s *AgentSession) GetUserMessagesForForking() []UserMessageForForking {
	if s == nil || s.sessionManager == nil {
		return nil
	}
	entries := s.sessionManager.Entries()
	result := make([]UserMessageForForking, 0)
	for _, entry := range entries {
		message, ok := entry.AgentMessage()
		if !ok {
			continue
		}
		wrapped, ok := message.(agentmsg.LLM)
		if !ok || wrapped.Role() != agentmsg.RoleUser {
			continue
		}
		text, ok := userMessageText(wrapped.Conversation())
		if ok && text != "" {
			result = append(result, UserMessageForForking{EntryID: entry.ID(), Text: text})
		}
	}
	return result
}

// GetSessionStats aggregates usage over the complete append-only entry log.
// It returns an error only when fixed-width Go accounting cannot represent a
// total that JavaScript's number arithmetic would otherwise make imprecise.
func (s *AgentSession) GetSessionStats() (SessionStats, error) {
	if s == nil || s.sessionManager == nil {
		return SessionStats{}, nil
	}
	stats := SessionStats{SessionID: s.sessionManager.SessionID()}
	if path, ok := s.sessionManager.SessionFile(); ok {
		stats.SessionFile = stringPointer(path)
	}

	var totals usageTotals
	for _, entry := range s.sessionManager.Entries() {
		switch payload := entry.Payload().(type) {
		case session.BranchSummaryPayload:
			if payload.Usage != nil {
				if err := totals.addCompaction(*payload.Usage); err != nil {
					return SessionStats{}, err
				}
			}
		case session.CompactionPayload:
			if payload.Record.Usage != nil {
				if err := totals.addCompaction(*payload.Record.Usage); err != nil {
					return SessionStats{}, err
				}
			}
		}

		// The original counts every durable type:"message" entry, including
		// bashExecution (and any legacy custom-role message), before applying
		// role-specific counters and usage accounting.
		if entry.Type() != "message" {
			continue
		}
		stats.TotalMessages++
		message, ok := entry.AgentMessage()
		if !ok {
			continue
		}
		wrapped, ok := message.(agentmsg.LLM)
		if !ok {
			continue
		}
		conversation := wrapped.Conversation()
		switch conversation.Role() {
		case llm.RoleUser:
			stats.UserMessages++
		case llm.RoleToolResult:
			stats.ToolResults++
			if usage, ok := toolResultUsage(conversation); ok {
				if err := totals.add(usage, usage.Cost().Total); err != nil {
					return SessionStats{}, err
				}
			}
		case llm.RoleAssistant:
			stats.AssistantMessages++
			assistant, ok := conversation.(llm.AssistantTerminal)
			if !ok {
				continue
			}
			for _, block := range assistant.Blocks() {
				if block.Kind() == llm.AssistantBlockToolCall {
					stats.ToolCalls++
				}
			}
			usage := assistant.Usage()
			if err := totals.add(usage, usage.Cost().Total); err != nil {
				return SessionStats{}, err
			}
		}
	}

	total, carry := bits.Add64(totals.input, totals.output, 0)
	if carry == 0 {
		total, carry = bits.Add64(total, totals.cacheRead, 0)
	}
	if carry == 0 {
		total, carry = bits.Add64(total, totals.cacheWrite, 0)
	}
	if carry != 0 {
		return SessionStats{}, fmt.Errorf("session stats token total overflow")
	}
	stats.Tokens = SessionTokenTotals{
		Input: totals.input, Output: totals.output, CacheRead: totals.cacheRead,
		CacheWrite: totals.cacheWrite, Total: total,
	}
	stats.Cost = totals.cost
	contextUsage, present, err := s.GetContextUsage()
	if err != nil {
		return SessionStats{}, err
	}
	if present {
		stats.ContextUsage = &contextUsage
	}
	return stats, nil
}

// GetContextUsage estimates only the current Agent message state. After the
// latest compaction on the selected branch, pre-compaction assistant usage is
// not trusted until a non-error, non-aborted, non-zero assistant usage appears
// after that checkpoint.
func (s *AgentSession) GetContextUsage() (ContextUsage, bool, error) {
	if s == nil {
		return ContextUsage{}, false, nil
	}
	model, hasModel, _ := s.selectionSnapshot()
	if !hasModel || model.ContextWindow() == 0 {
		return ContextUsage{}, false, nil
	}
	contextWindow := model.ContextWindow()

	branch, err := s.sessionManager.BranchPath("")
	if err != nil {
		return ContextUsage{}, false, err
	}
	if latest, ok := session.LatestCompactionEntry(branch); ok {
		compactionIndex := -1
		for index := len(branch) - 1; index >= 0; index-- {
			if branch[index].ID() == latest.ID() {
				compactionIndex = index
				break
			}
		}
		hasPostCompactionUsage := false
		for index := len(branch) - 1; index > compactionIndex; index-- {
			message, exists := branch[index].AgentMessage()
			if !exists {
				continue
			}
			wrapped, exists := message.(agentmsg.LLM)
			if !exists {
				continue
			}
			assistant, exists := wrapped.Conversation().(llm.AssistantTerminal)
			if !exists || assistant.FinishReason() == llm.FinishAborted || assistant.FinishReason() == llm.FinishError {
				continue
			}
			if assistant.Usage().TotalTokens() > 0 {
				hasPostCompactionUsage = true
				break
			}
		}
		if !hasPostCompactionUsage {
			return ContextUsage{ContextWindow: contextWindow}, true, nil
		}
	}

	messages := s.loop.State().Messages()
	estimate, err := session.EstimateAgentContextTokens(messages)
	if err != nil {
		return ContextUsage{}, false, err
	}
	tokens := estimate.Tokens
	percent := (float64(tokens) / float64(contextWindow)) * 100
	return ContextUsage{Tokens: &tokens, ContextWindow: contextWindow, Percent: &percent}, true, nil
}

// GetLastAssistantText returns the concatenated text blocks from the most
// recent assistant message in the current Agent state. An aborted assistant
// with no content is ignored so the preceding complete answer remains usable.
func (s *AgentSession) GetLastAssistantText() (string, bool) {
	if s == nil || s.loop == nil {
		return "", false
	}
	messages := s.loop.State().Messages()
	for index := len(messages) - 1; index >= 0; index-- {
		wrapped, ok := messages[index].(agentmsg.LLM)
		if !ok {
			continue
		}
		assistant, ok := wrapped.Conversation().(llm.AssistantTerminal)
		if !ok {
			continue
		}
		blocks := assistant.Blocks()
		if assistant.FinishReason() == llm.FinishAborted && len(blocks) == 0 {
			continue
		}
		var text strings.Builder
		for _, block := range blocks {
			if value, ok := block.(llm.TextBlock); ok {
				text.WriteString(value.Text())
			}
		}
		value := strings.TrimFunc(text.String(), isECMAScriptTrimSpace)
		return value, value != ""
	}
	return "", false
}

type usageTotals struct {
	input, output, cacheRead, cacheWrite uint64
	cost                                 float64
}

func (t *usageTotals) add(usage llm.Usage, cost float64) error {
	var carry uint64
	if t.input, carry = bits.Add64(t.input, usage.Input(), 0); carry != 0 {
		return fmt.Errorf("session stats input token overflow")
	}
	if t.output, carry = bits.Add64(t.output, usage.Output(), 0); carry != 0 {
		return fmt.Errorf("session stats output token overflow")
	}
	if t.cacheRead, carry = bits.Add64(t.cacheRead, usage.CacheRead(), 0); carry != 0 {
		return fmt.Errorf("session stats cache-read token overflow")
	}
	if t.cacheWrite, carry = bits.Add64(t.cacheWrite, usage.CacheWrite(), 0); carry != 0 {
		return fmt.Errorf("session stats cache-write token overflow")
	}
	t.cost += cost
	if math.IsNaN(t.cost) || math.IsInf(t.cost, 0) {
		return fmt.Errorf("session stats cost overflow")
	}
	return nil
}

func (t *usageTotals) addCompaction(value session.CompactionUsage) error {
	cost, err := value.Cost.Total.Float64()
	if err != nil {
		return fmt.Errorf("session stats summary cost: %w", err)
	}
	return t.add(value.Usage, cost)
}

func toolResultUsage(message llm.ConversationMessage) (llm.Usage, bool) {
	switch value := message.(type) {
	case llm.ToolResultMessage:
		return value.Usage()
	case llm.ToolResultContentMessage:
		return value.Usage()
	default:
		return llm.Usage{}, false
	}
}

func userMessageText(message llm.ConversationMessage) (string, bool) {
	var text strings.Builder
	switch value := message.(type) {
	case llm.UserTextMessage:
		for _, block := range value.Content() {
			text.WriteString(block.Text())
		}
	case llm.UserContentMessage:
		for _, block := range value.Content() {
			if value, ok := block.(llm.TextBlock); ok {
				text.WriteString(value.Text())
			}
		}
	default:
		return "", false
	}
	return text.String(), true
}

func stringPointer(value string) *string { return &value }

// JavaScript String.prototype.trim uses ECMAScript's fixed WhiteSpace and
// LineTerminator sets. Keep FEFF handling and the deliberate U+0085 exclusion
// instead of inheriting Go's Unicode White_Space table.
func isECMAScriptTrimSpace(value rune) bool {
	switch value {
	case '\u0009', '\u000a', '\u000b', '\u000c', '\u000d', '\u0020',
		'\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f',
		'\u3000', '\ufeff':
		return true
	default:
		return value >= '\u2000' && value <= '\u200a'
	}
}

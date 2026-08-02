package session

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/llm"
)

const defaultKeepRecentTokens uint64 = 20_000

// ContextTokenEstimate distinguishes an exact provider usage boundary from the
// heuristic suffix that followed it. A post-compaction caller can use this to
// decide whether automatic policy should run, while manual Compact always uses
// the selected branch snapshot below.
type ContextTokenEstimate struct {
	Tokens         uint64
	UsageTokens    uint64
	TrailingTokens uint64
	LastUsageIndex int
}

// EstimateContextTokens uses the latest successful assistant usage when one is
// available, then adds conservative estimates for the trailing messages.
func EstimateContextTokens(messages []llm.ConversationMessage) ContextTokenEstimate {
	result := ContextTokenEstimate{LastUsageIndex: -1}
	for index := len(messages) - 1; index >= 0; index-- {
		usage, ok := usableAssistantUsage(messages[index])
		if !ok {
			continue
		}
		result.LastUsageIndex = index
		result.UsageTokens = usage.TotalTokens()
		break
	}
	start := 0
	if result.LastUsageIndex >= 0 {
		start = result.LastUsageIndex + 1
	}
	for index := start; index < len(messages); index++ {
		result.TrailingTokens += estimateMessageTokens(messages[index])
	}
	result.Tokens = result.UsageTokens + result.TrailingTokens
	return result
}

func usableAssistantUsage(message llm.ConversationMessage) (llm.Usage, bool) {
	switch message := message.(type) {
	case llm.AssistantTextMessage:
		if message.FinishReason() != llm.FinishError && message.FinishReason() != llm.FinishAborted && message.Usage().TotalTokens() > 0 {
			return message.Usage(), true
		}
	case llm.AssistantToolUseMessage:
		if message.Usage().TotalTokens() > 0 {
			return message.Usage(), true
		}
	}
	return llm.Usage{}, false
}

// ShouldCompact is policy-only. v0.3 deliberately exposes it without wiring
// automatic triggering into the still-evolving agent coordinator.
func ShouldCompact(contextTokens, contextWindow, reserveTokens uint64) bool {
	if contextWindow <= reserveTokens {
		return contextTokens > 0
	}
	return contextTokens > contextWindow-reserveTokens
}

func estimateMessageTokens(message llm.ConversationMessage) uint64 {
	var chars int
	switch value := message.(type) {
	case llm.UserTextMessage:
		for _, block := range value.Content() {
			chars += len(block.Text())
		}
	case llm.AssistantTextMessage:
		for _, block := range value.Content() {
			chars += len(block.Text())
		}
	case llm.AssistantToolUseMessage:
		for _, block := range value.Content() {
			switch block := block.(type) {
			case llm.TextBlock:
				chars += len(block.Text())
			case llm.ToolCallBlock:
				chars += len(block.Name()) + len(block.ArgumentsJSON())
			}
		}
	case llm.AssistantFailureMessage:
		for _, block := range value.Content() {
			chars += len(block.Text())
		}
		chars += len(value.ErrorMessage())
	case llm.ToolResultMessage:
		for _, block := range value.Content() {
			chars += len(block.Text())
		}
		chars += len(value.ToolCallID()) + len(value.ToolName())
	}
	if chars == 0 {
		return 0
	}
	return uint64((chars + 3) / 4)
}

// Compact manually summarizes an immutable selected-branch snapshot, then
// appends a typed v3 compaction record only if selection and durable history
// still match that snapshot. The external summarizer is never called while the
// append gate or Session mutex is held.
func (s *Session) Compact(ctx context.Context, request CompactRequest) (CompactResult, error) {
	if s == nil {
		return CompactResult{}, fmt.Errorf("%w: nil session", ErrInvalidSession)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if request.Summarizer == nil {
		return CompactResult{}, fmt.Errorf("%w: no summarizer", ErrSummaryFailed)
	}
	input, err := s.compactionSnapshot(ctx, request)
	if err != nil {
		return CompactResult{}, err
	}
	output, err := request.Summarizer.Summarize(ctx, input)
	if cause := context.Cause(ctx); cause != nil {
		if err != nil {
			return CompactResult{}, fmt.Errorf("%w: %w", ErrAppendCanceled, errors.Join(cause, err))
		}
		return CompactResult{}, fmt.Errorf("%w: %w", ErrAppendCanceled, cause)
	}
	if err != nil {
		return CompactResult{}, fmt.Errorf("%w: %w", ErrSummaryFailed, err)
	}
	if !utf8.ValidString(output.Text) || strings.TrimSpace(output.Text) == "" {
		return CompactResult{}, fmt.Errorf("%w: empty or invalid summary", ErrSummaryFailed)
	}
	if output.Usage != nil {
		if err := validateUsageCost(output.Usage.Cost); err != nil {
			return CompactResult{}, fmt.Errorf("%w: invalid summary usage: %v", ErrSummaryFailed, err)
		}
	}
	entry, err := s.commitCompaction(ctx, input, output)
	if err != nil {
		return CompactResult{}, err
	}
	return CompactResult{Entry: entry, Input: input, Committed: true}, nil
}

func (s *Session) compactionSnapshot(ctx context.Context, request CompactRequest) (SummaryInput, error) {
	if err := s.acquireAppend(ctx); err != nil {
		return SummaryInput{}, err
	}
	defer s.releaseAppend()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return SummaryInput{}, ErrClosed
	}
	if s.poisoned {
		return SummaryInput{}, ErrPoisoned
	}
	if s.leaf < 0 {
		return SummaryInput{}, ErrNothingToCompact
	}
	path := s.pathLocked(s.leaf)
	if len(path) == 0 {
		return SummaryInput{}, ErrNothingToCompact
	}
	if path[len(path)-1].compaction != nil {
		return SummaryInput{}, ErrAlreadyCompacted
	}
	keep := request.KeepRecentTokens
	if keep == 0 {
		keep = defaultKeepRecentTokens
	}
	preparation, err := prepareCompaction(path, keep)
	if err != nil {
		return SummaryInput{}, err
	}
	if len(preparation.messages) == 0 {
		return SummaryInput{}, ErrNothingToCompact
	}
	return SummaryInput{
		SystemPrompt:     summarizationSystemPrompt,
		Prompt:           summarizePrompt(preparation.messages, preparation.previousSummary, request.Instructions),
		Instructions:     request.Instructions,
		PreviousSummary:  preparation.previousSummary,
		Messages:         append([]llm.ConversationMessage(nil), preparation.messages...),
		RetainedTail:     append([]llm.ConversationMessage(nil), preparation.retained...),
		FirstKeptEntryID: preparation.firstKeptID,
		TokensBefore:     EstimateContextTokens(s.buildContextLocked().Messages()).Tokens,
		Generation:       s.generation,
		SelectedLeafID:   s.entries[s.leaf].id,
	}, nil
}

func (s *Session) commitCompaction(ctx context.Context, input SummaryInput, output SummaryOutput) (Entry, error) {
	if err := s.acquireAppend(ctx); err != nil {
		return Entry{}, err
	}
	defer s.releaseAppend()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Entry{}, ErrClosed
	}
	if s.poisoned {
		s.mu.Unlock()
		return Entry{}, ErrPoisoned
	}
	if s.generation != input.Generation || s.leaf < 0 || s.entries[s.leaf].id != input.SelectedLeafID {
		s.mu.Unlock()
		return Entry{}, ErrCompactionConflict
	}
	entryID, err := s.nextEntryID()
	if err != nil {
		s.mu.Unlock()
		return Entry{}, err
	}
	timestamp := canonicalTime(s.runtime.now())
	if timestamp.IsZero() || validateISOTime(timestamp) != nil {
		s.mu.Unlock()
		return Entry{}, fmt.Errorf("%w: compaction timestamp", ErrInvalidEntry)
	}
	record := CompactionRecord{Summary: output.Text, FirstKeptEntryID: input.FirstKeptEntryID, TokensBefore: input.TokensBefore, Usage: output.Usage}
	raw, err := encodeCompactionEntry(entryID, input.SelectedLeafID, timestamp, record)
	if err != nil {
		s.mu.Unlock()
		return Entry{}, fmt.Errorf("%w: encode compaction: %w", ErrInvalidEntry, err)
	}
	entry, err := decodeEntry(raw)
	if err != nil {
		s.mu.Unlock()
		return Entry{}, fmt.Errorf("%w: decode compaction: %w", ErrInvalidEntry, err)
	}
	appendBytes := make([]byte, 0, len(raw)+2)
	if s.needsSeparator {
		appendBytes = append(appendBytes, '\n')
	}
	appendBytes = append(appendBytes, raw...)
	appendBytes = append(appendBytes, '\n')
	path := s.path
	s.mu.Unlock()

	started, err := s.storage.append(ctx, path, appendBytes)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if started {
			s.poisoned = true
			return Entry{}, fmt.Errorf("%w: %w", ErrCommitUnknown, err)
		}
		if cause := context.Cause(ctx); cause != nil && errors.Is(err, cause) {
			return Entry{}, fmt.Errorf("%w: %w", ErrAppendCanceled, err)
		}
		return Entry{}, fmt.Errorf("%w: append compaction %s: %w", ErrStorage, path, err)
	}
	s.entries = append(s.entries, entry)
	s.leaf = len(s.entries) - 1
	s.byID[entry.id] = s.leaf
	s.needsSeparator = false
	s.generation++
	return entry.clone(), nil
}

type compactionPreparation struct {
	firstKeptID     string
	messages        []llm.ConversationMessage
	retained        []llm.ConversationMessage
	previousSummary string
}

func prepareCompaction(path []Entry, keepRecentTokens uint64) (compactionPreparation, error) {
	previousIndex := -1
	previousSummary := ""
	boundaryStart := 0
	for index := len(path) - 1; index >= 0; index-- {
		if record, ok := path[index].Compaction(); ok {
			previousIndex = index
			previousSummary = record.Summary
			for candidate := 0; candidate < index; candidate++ {
				if path[candidate].id == record.FirstKeptEntryID {
					boundaryStart = candidate
					break
				}
			}
			if boundaryStart == 0 && (len(path) == 0 || path[0].id != record.FirstKeptEntryID) {
				boundaryStart = previousIndex + 1
			}
			break
		}
	}
	cut := findCompactionCut(path, boundaryStart, len(path), keepRecentTokens)
	if cut < 0 || cut >= len(path) || cut == boundaryStart {
		return compactionPreparation{}, ErrNothingToCompact
	}
	firstKept := path[cut]
	messages := messagesFromEntries(path[boundaryStart:cut])
	retained := messagesFromEntries(path[cut:])
	if len(messages) == 0 {
		return compactionPreparation{}, ErrNothingToCompact
	}
	return compactionPreparation{firstKeptID: firstKept.id, messages: messages, retained: retained, previousSummary: previousSummary}, nil
}

func findCompactionCut(entries []Entry, start, end int, keepRecentTokens uint64) int {
	if start >= end {
		return -1
	}
	var accumulated uint64
	for index := end - 1; index >= start; index-- {
		entry := entries[index]
		for _, message := range entryMessages(entry) {
			accumulated += estimateMessageTokens(message)
		}
		if accumulated >= keepRecentTokens {
			for candidate := index; candidate < end; candidate++ {
				if isValidCompactionCut(entries[candidate]) {
					return candidate
				}
			}
			return -1
		}
	}
	return -1
}

func isValidCompactionCut(entry Entry) bool {
	if entry.message == nil {
		return false
	}
	return entry.message.Role() != llm.RoleToolResult
}

func entryMessages(entry Entry) []llm.ConversationMessage {
	if entry.message == nil {
		return nil
	}
	return []llm.ConversationMessage{entry.message}
}

func messagesFromEntries(entries []Entry) []llm.ConversationMessage {
	messages := make([]llm.ConversationMessage, 0, len(entries))
	for _, entry := range entries {
		messages = append(messages, entryMessages(entry)...)
	}
	return messages
}

const summarizationSystemPrompt = "You are a context summarization assistant. Preserve exact file paths, function names, constraints, decisions, and errors needed to continue the work."

func summarizePrompt(messages []llm.ConversationMessage, previousSummary, instructions string) string {
	var builder strings.Builder
	builder.WriteString("<conversation>\n")
	builder.WriteString(SerializeConversation(messages))
	builder.WriteString("\n</conversation>\n\nCreate a concise structured checkpoint summary for the next model turn.")
	if previousSummary != "" {
		builder.WriteString("\n\n<previous-summary>\n")
		builder.WriteString(previousSummary)
		builder.WriteString("\n</previous-summary>")
	}
	if instructions != "" {
		builder.WriteString("\n\nAdditional focus: ")
		builder.WriteString(instructions)
	}
	return builder.String()
}

// SerializeConversation is the stable text representation supplied to a
// summarizer. Tool results are bounded so a single command output cannot make
// a manual compaction request unbounded.
func SerializeConversation(messages []llm.ConversationMessage) string {
	const maxToolResultChars = 2000
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		switch value := message.(type) {
		case llm.UserTextMessage:
			parts = append(parts, "[User]: "+joinTextBlocks(value.Content()))
		case llm.AssistantTextMessage:
			parts = append(parts, "[Assistant]: "+joinTextBlocks(value.Content()))
		case llm.AssistantToolUseMessage:
			text := make([]string, 0)
			calls := make([]string, 0)
			for _, block := range value.Content() {
				switch block := block.(type) {
				case llm.TextBlock:
					text = append(text, block.Text())
				case llm.ToolCallBlock:
					calls = append(calls, block.Name()+"("+string(block.ArgumentsJSON())+")")
				}
			}
			if len(text) > 0 {
				parts = append(parts, "[Assistant]: "+strings.Join(text, ""))
			}
			if len(calls) > 0 {
				parts = append(parts, "[Assistant tool calls]: "+strings.Join(calls, "; "))
			}
		case llm.AssistantFailureMessage:
			parts = append(parts, "[Assistant error]: "+joinTextBlocks(value.Content())+"\n"+value.ErrorMessage())
		case llm.ToolResultMessage:
			text := joinTextBlocks(value.Content())
			if len(text) > maxToolResultChars {
				text = text[:maxToolResultChars] + "\n\n[... " + strconv.Itoa(len(text)-maxToolResultChars) + " more characters truncated]"
			}
			parts = append(parts, "[Tool result]: "+text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func joinTextBlocks(blocks []llm.TextBlock) string {
	values := make([]string, len(blocks))
	for index, block := range blocks {
		values[index] = block.Text()
	}
	return strings.Join(values, "")
}

package session

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
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
func EstimateContextTokens(messages []llm.ConversationMessage) (ContextTokenEstimate, error) {
	result := ContextTokenEstimate{LastUsageIndex: -1}
	var latestPrefixTimestamp time.Time
	for index, message := range messages {
		timestamp := messageTimestamp(message)
		if timestamp.After(latestPrefixTimestamp) {
			latestPrefixTimestamp = timestamp
		}
		usage, ok := usableAssistantUsage(message)
		if !ok {
			continue
		}
		if timestamp.Before(latestPrefixTimestamp) {
			continue
		}
		result.LastUsageIndex = index
		result.UsageTokens = usage.TotalTokens()
	}
	start := 0
	if result.LastUsageIndex >= 0 {
		start = result.LastUsageIndex + 1
	}
	for index := start; index < len(messages); index++ {
		tokens, err := estimateMessageTokens(messages[index])
		if err != nil {
			return ContextTokenEstimate{}, err
		}
		result.TrailingTokens, err = checkedAddTokens(result.TrailingTokens, tokens)
		if err != nil {
			return ContextTokenEstimate{}, err
		}
	}
	var err error
	result.Tokens, err = checkedAddTokens(result.UsageTokens, result.TrailingTokens)
	if err != nil {
		return ContextTokenEstimate{}, err
	}
	return result, nil
}

func messageTimestamp(message llm.ConversationMessage) time.Time {
	switch value := message.(type) {
	case llm.UserTextMessage:
		return value.Timestamp()
	case llm.UserContentMessage:
		return value.Timestamp()
	case llm.AssistantTextMessage:
		return value.Timestamp()
	case llm.AssistantToolUseMessage:
		return value.Timestamp()
	case llm.AssistantRichMessage:
		return value.Timestamp()
	case llm.AssistantFailureMessage:
		return value.Timestamp()
	case llm.ToolResultMessage:
		return value.Timestamp()
	case llm.ToolResultContentMessage:
		return value.Timestamp()
	default:
		return time.Time{}
	}
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
	case llm.AssistantRichMessage:
		if message.FinishReason() != llm.FinishError && message.FinishReason() != llm.FinishAborted && message.Usage().TotalTokens() > 0 {
			return message.Usage(), true
		}
	}
	return llm.Usage{}, false
}

// ShouldCompact is policy-only. It estimates the supplied context itself so an
// overflow cannot be accidentally discarded before applying the threshold.
// v0.3 deliberately does not wire this into the evolving agent coordinator.
func ShouldCompact(messages []llm.ConversationMessage, contextWindow, reserveTokens uint64) (bool, error) {
	estimate, err := EstimateContextTokens(messages)
	if err != nil {
		return false, err
	}
	if contextWindow <= reserveTokens {
		return estimate.Tokens > 0, nil
	}
	return estimate.Tokens > contextWindow-reserveTokens, nil
}

func estimateMessageTokens(message llm.ConversationMessage) (uint64, error) {
	var chars uint64
	add := func(text string) error {
		var err error
		chars, err = checkedAddTokens(chars, uint64(len(text)))
		return err
	}
	addImage := func() error {
		var err error
		chars, err = checkedAddTokens(chars, 4800)
		return err
	}
	switch value := message.(type) {
	case llm.UserTextMessage:
		for _, block := range value.Content() {
			if err := add(block.Text()); err != nil {
				return 0, err
			}
		}
	case llm.UserContentMessage:
		for _, block := range value.Content() {
			switch block := block.(type) {
			case llm.TextBlock:
				if err := add(block.Text()); err != nil {
					return 0, err
				}
			case llm.ImageBlock:
				if err := addImage(); err != nil {
					return 0, err
				}
			}
		}
	case llm.AssistantTextMessage:
		for _, block := range value.Content() {
			if err := add(block.Text()); err != nil {
				return 0, err
			}
		}
	case llm.AssistantRichMessage:
		for _, block := range value.Blocks() {
			switch block := block.(type) {
			case llm.TextBlock:
				if err := add(block.Text()); err != nil {
					return 0, err
				}
			case llm.ThinkingBlock:
				if err := add(block.Thinking()); err != nil {
					return 0, err
				}
			}
		}
	case llm.AssistantToolUseMessage:
		for _, block := range value.Content() {
			switch block := block.(type) {
			case llm.TextBlock:
				if err := add(block.Text()); err != nil {
					return 0, err
				}
			case llm.ToolCallBlock:
				if err := add(block.Name()); err != nil {
					return 0, err
				}
				if err := add(string(block.ArgumentsJSON())); err != nil {
					return 0, err
				}
			}
		}
	case llm.AssistantFailureMessage:
		for _, block := range value.Content() {
			if err := add(block.Text()); err != nil {
				return 0, err
			}
		}
		if err := add(value.ErrorMessage()); err != nil {
			return 0, err
		}
	case llm.ToolResultMessage:
		for _, block := range value.Content() {
			if err := add(block.Text()); err != nil {
				return 0, err
			}
		}
		if err := add(value.ToolCallID()); err != nil {
			return 0, err
		}
		if err := add(value.ToolName()); err != nil {
			return 0, err
		}
	case llm.ToolResultContentMessage:
		for _, block := range value.Content() {
			switch block := block.(type) {
			case llm.TextBlock:
				if err := add(block.Text()); err != nil {
					return 0, err
				}
			case llm.ImageBlock:
				if err := addImage(); err != nil {
					return 0, err
				}
			}
		}
		if err := add(value.ToolCallID()); err != nil {
			return 0, err
		}
		if err := add(value.ToolName()); err != nil {
			return 0, err
		}
	}
	if chars == 0 {
		return 0, nil
	}
	tokens := chars / 4
	if chars%4 != 0 {
		return checkedAddTokens(tokens, 1)
	}
	return tokens, nil
}

func checkedAddTokens(left, right uint64) (uint64, error) {
	if left > math.MaxUint64-right {
		return 0, ErrTokenEstimateOverflow
	}
	return left + right, nil
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
	contextMessages := s.buildContextLocked().Messages()
	estimate, err := EstimateContextTokens(contextMessages)
	if err != nil {
		return SummaryInput{}, err
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
		TokensBefore:     estimate.Tokens,
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
	cut, err := findCompactionCut(path, boundaryStart, len(path), keepRecentTokens)
	if err != nil {
		return compactionPreparation{}, err
	}
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

func findCompactionCut(entries []Entry, start, end int, keepRecentTokens uint64) (int, error) {
	return findCompactionCutWithEstimator(entries, start, end, keepRecentTokens, estimateMessageTokens)
}

func findCompactionCutWithEstimator(
	entries []Entry,
	start int,
	end int,
	keepRecentTokens uint64,
	estimate func(llm.ConversationMessage) (uint64, error),
) (int, error) {
	if start >= end {
		return -1, nil
	}
	var accumulated uint64
	for index := end - 1; index >= start; index-- {
		entry := entries[index]
		for _, message := range entryMessages(entry) {
			tokens, err := estimate(message)
			if err != nil {
				return -1, err
			}
			accumulated, err = checkedAddTokens(accumulated, tokens)
			if err != nil {
				return -1, err
			}
		}
		if accumulated >= keepRecentTokens {
			for candidate := index; candidate < end; candidate++ {
				if isValidCompactionCut(entries[candidate]) {
					return candidate, nil
				}
			}
			return -1, nil
		}
	}
	return -1, nil
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
		case llm.UserContentMessage:
			parts = append(parts, "[User]: "+joinUserContent(value.Content()))
		case llm.AssistantTextMessage:
			parts = append(parts, "[Assistant]: "+joinTextBlocks(value.Content()))
		case llm.AssistantRichMessage:
			parts = append(parts, "[Assistant]: "+joinAssistantBlocks(value.Blocks()))
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
			parts = append(parts, "[Tool result]: "+truncateToolResult(joinTextBlocks(value.Content()), maxToolResultChars))
		case llm.ToolResultContentMessage:
			parts = append(parts, "[Tool result]: "+truncateToolResult(joinToolResultContent(value.Content()), maxToolResultChars))
		}
	}
	return strings.Join(parts, "\n\n")
}

func joinUserContent(blocks []llm.UserContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch block := block.(type) {
		case llm.TextBlock:
			parts = append(parts, block.Text())
		case llm.ImageBlock:
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "")
}

func joinAssistantBlocks(blocks []llm.AssistantBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch block := block.(type) {
		case llm.TextBlock:
			parts = append(parts, block.Text())
		case llm.ThinkingBlock:
			parts = append(parts, block.Thinking())
		case llm.ToolCallBlock:
			parts = append(parts, block.Name()+"("+string(block.ArgumentsJSON())+")")
		}
	}
	return strings.Join(parts, "")
}

func joinToolResultContent(blocks []llm.ToolResultContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch block := block.(type) {
		case llm.TextBlock:
			parts = append(parts, block.Text())
		case llm.ImageBlock:
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "")
}

func truncateToolResult(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return text[:max] + "\n\n[... " + strconv.Itoa(len(text)-max) + " more characters truncated]"
}

func joinTextBlocks(blocks []llm.TextBlock) string {
	values := make([]string, len(blocks))
	for index, block := range blocks {
		values[index] = block.Text()
	}
	return strings.Join(values, "")
}

package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/cat3399/pi-go/internal/agentmsg"
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
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		usage, ok := usableAssistantUsage(message)
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

// EstimateAgentContextTokens is the source-faithful estimator for coding-agent
// state. It preserves custom/bash/branch message sizes instead of estimating
// their provider projection, which may contain additional explanatory text.
func EstimateAgentContextTokens(messages []agentmsg.Message) (ContextTokenEstimate, error) {
	result := ContextTokenEstimate{LastUsageIndex: -1}
	for index := len(messages) - 1; index >= 0; index-- {
		wrapped, ok := messages[index].(agentmsg.LLM)
		if !ok {
			continue
		}
		usage, ok := usableAssistantUsage(wrapped.Conversation())
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
		tokens, err := estimateAgentMessageTokens(messages[index])
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
		chars, err = checkedAddTokens(chars, uint64(jsStringLength(text)))
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
			case llm.ToolCallBlock:
				if err := add(block.Name()); err != nil {
					return 0, err
				}
				if err := add(compactJSONString(block.ArgumentsJSON())); err != nil {
					return 0, err
				}
			}
		}
	case llm.AssistantFailureMessage:
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
			case llm.ToolCallBlock:
				if err := add(block.Name()); err != nil {
					return 0, err
				}
				if err := add(compactJSONString(block.ArgumentsJSON())); err != nil {
					return 0, err
				}
			}
		}
	case llm.ToolResultMessage:
		for _, block := range value.Content() {
			if err := add(block.Text()); err != nil {
				return 0, err
			}
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
	output, err = finalizeCompactionOutput(input, output)
	if err != nil {
		return CompactResult{}, fmt.Errorf("%w: %w", ErrSummaryFailed, err)
	}
	if err := validateSummaryOutput(output); err != nil {
		return CompactResult{}, err
	}
	entry, err := s.commitCompaction(ctx, input, output)
	if err != nil {
		return CompactResult{}, err
	}
	// Estimation is diagnostic data computed after the durable append. It must
	// never turn a committed compaction into an apparent failed write. The
	// estimator can only fail on arithmetic overflow; retain zero in that
	// unreachable-in-practice case and preserve the known committed outcome.
	var estimatedTokensAfter uint64
	if contextEstimate, estimateErr := EstimateAgentContextTokens(s.BuildContext().AgentMessages()); estimateErr == nil {
		estimatedTokensAfter = contextEstimate.Tokens
	}
	return CompactResult{Entry: entry, Input: input, Output: cloneSummaryOutput(output), EstimatedTokensAfter: estimatedTokensAfter, Committed: true}, nil
}

func validateSummaryOutput(output SummaryOutput) error {
	if !utf8.ValidString(output.Text) || strings.TrimSpace(output.Text) == "" {
		return fmt.Errorf("%w: empty or invalid summary", ErrSummaryFailed)
	}
	if output.Usage != nil {
		if err := validateUsageCost(output.Usage.Cost); err != nil {
			return fmt.Errorf("%w: invalid summary usage: %v", ErrSummaryFailed, err)
		}
	}
	if output.FromExtension {
		if err := validateOpaqueID(output.FirstKeptEntryID, "extension compaction first kept entry id"); err != nil {
			return fmt.Errorf("%w: %v", ErrSummaryFailed, err)
		}
		if len(output.Details) != 0 && !json.Valid(output.Details) {
			return fmt.Errorf("%w: invalid extension compaction details", ErrSummaryFailed)
		}
	}
	return nil
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
	if keep == 0 && !request.KeepRecentTokensSet {
		keep = defaultKeepRecentTokens
	}
	reserve := request.ReserveTokens
	if reserve == 0 && !request.ReserveTokensSet {
		reserve = 16_384
	}
	contextMessages := s.buildContextLocked().AgentMessages()
	estimate, err := EstimateAgentContextTokens(contextMessages)
	if err != nil {
		return SummaryInput{}, err
	}
	enabled := request.Enabled
	if !request.EnabledSet {
		enabled = true
	}
	settings := CompactionSettings{Enabled: enabled, ReserveTokens: reserve, KeepRecentTokens: keep}
	preparation, err := prepareCompaction(path, settings)
	if err != nil {
		return SummaryInput{}, err
	}
	if len(preparation.messages) == 0 && len(preparation.turnPrefix) == 0 {
		return SummaryInput{}, ErrNothingToCompact
	}
	prompt, err := summarizePrompt(preparation.messages, preparation.previousSummary, request.Instructions)
	if err != nil {
		return SummaryInput{}, err
	}
	prefixPrompt := ""
	if preparation.isSplitTurn {
		prefixPrompt, err = turnPrefixPrompt(preparation.turnPrefix)
		if err != nil {
			return SummaryInput{}, err
		}
	}
	messages, err := agentmsg.ConvertToLLM(preparation.messages)
	if err != nil {
		return SummaryInput{}, err
	}
	return SummaryInput{
		SystemPrompt:        summarizationSystemPrompt,
		Prompt:              prompt,
		TurnPrefixPrompt:    prefixPrompt,
		Instructions:        request.Instructions,
		PreviousSummary:     preparation.previousSummary,
		Messages:            messages,
		MessagesToSummarize: agentmsg.Clone(preparation.messages),
		TurnPrefixMessages:  agentmsg.Clone(preparation.turnPrefix),
		RetainedTail:        append([]llm.ConversationMessage(nil), preparation.retained...),
		IsSplitTurn:         preparation.isSplitTurn,
		FileOperations:      cloneFileOperations(preparation.fileOperations),
		Settings:            preparation.settings,
		FirstKeptEntryID:    preparation.firstKeptID,
		TokensBefore:        estimate.Tokens,
		Generation:          s.generation,
		SelectedLeafID:      s.entries[s.leaf].id,
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
	firstKeptEntryID, tokensBefore := input.FirstKeptEntryID, input.TokensBefore
	if output.FromExtension {
		firstKeptEntryID, tokensBefore = output.FirstKeptEntryID, output.TokensBefore
	}
	payload := CompactionPayload{
		Record:  CompactionRecord{Summary: output.Text, FirstKeptEntryID: firstKeptEntryID, TokensBefore: tokensBefore, Usage: output.Usage},
		Details: bytes.Clone(output.Details), FromHook: output.FromExtension, HasFromHook: output.FromExtension,
	}
	raw, err := encodeCompactionEntry(entryID, input.SelectedLeafID, timestamp, payload)
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

func cloneSummaryOutput(value SummaryOutput) SummaryOutput {
	value.Details = bytes.Clone(value.Details)
	if value.Usage != nil {
		usage := *value.Usage
		value.Usage = &usage
	}
	if value.EstimatedTokensAfter != nil {
		estimated := *value.EstimatedTokensAfter
		value.EstimatedTokensAfter = &estimated
	}
	return value
}

type compactionPreparation struct {
	firstKeptID     string
	messages        []agentmsg.Message
	turnPrefix      []agentmsg.Message
	retained        []llm.ConversationMessage
	isSplitTurn     bool
	previousSummary string
	fileOperations  FileOperations
	settings        CompactionSettings
}

type cutPointResult struct {
	firstKeptEntryIndex int
	turnStartIndex      int
	isSplitTurn         bool
}

func prepareCompaction(path []Entry, settings CompactionSettings) (compactionPreparation, error) {
	previousIndex := -1
	previousSummary := ""
	boundaryStart := 0
	for index := len(path) - 1; index >= 0; index-- {
		if record, ok := path[index].Compaction(); ok {
			previousIndex = index
			previousSummary = record.Summary
			firstKeptIndex := -1
			for candidate := range path {
				if path[candidate].id == record.FirstKeptEntryID {
					firstKeptIndex = candidate
					break
				}
			}
			if firstKeptIndex >= 0 {
				boundaryStart = firstKeptIndex
			} else {
				boundaryStart = previousIndex + 1
			}
			break
		}
	}
	cut, err := findCompactionCut(path, boundaryStart, len(path), settings.KeepRecentTokens)
	if err != nil {
		return compactionPreparation{}, err
	}
	if cut.firstKeptEntryIndex < 0 || cut.firstKeptEntryIndex >= len(path) {
		return compactionPreparation{}, ErrNothingToCompact
	}
	historyEnd := cut.firstKeptEntryIndex
	if cut.isSplitTurn {
		historyEnd = cut.turnStartIndex
	}
	messages := messagesFromEntries(path[boundaryStart:historyEnd])
	turnPrefix := []agentmsg.Message(nil)
	if cut.isSplitTurn {
		turnPrefix = messagesFromEntries(path[cut.turnStartIndex:cut.firstKeptEntryIndex])
	}
	if len(messages) == 0 && len(turnPrefix) == 0 {
		return compactionPreparation{}, ErrNothingToCompact
	}
	retainedAgent := messagesFromEntries(path[cut.firstKeptEntryIndex:])
	retained, err := agentmsg.ConvertToLLM(retainedAgent)
	if err != nil {
		return compactionPreparation{}, err
	}
	fileOperations := extractFileOperations(messages, path, previousIndex)
	for _, message := range turnPrefix {
		extractFileOperationsFromMessage(message, &fileOperations)
	}
	return compactionPreparation{
		firstKeptID: path[cut.firstKeptEntryIndex].id, messages: messages, turnPrefix: turnPrefix,
		retained: retained, isSplitTurn: cut.isSplitTurn, previousSummary: previousSummary,
		fileOperations: fileOperations, settings: settings,
	}, nil
}

func findCompactionCut(entries []Entry, start, end int, keepRecentTokens uint64) (cutPointResult, error) {
	cutPoints := make([]int, 0)
	for index := start; index < end; index++ {
		if entries[index].compaction != nil {
			continue
		}
		for _, message := range entryMessages(entries[index]) {
			if isCutPointMessage(message) {
				cutPoints = append(cutPoints, index)
				break
			}
		}
	}
	if len(cutPoints) == 0 {
		return cutPointResult{firstKeptEntryIndex: start, turnStartIndex: -1}, nil
	}
	cutIndex := cutPoints[0]
	var accumulated uint64
	for index := end - 1; index >= start; index-- {
		var messageTokens uint64
		// Unlike the discarded-history projection below, pi's cut budget walks
		// sessionEntryToContextMessages directly. A prior compaction checkpoint is
		// therefore not itself a cut point, but its summary still consumes budget.
		for _, message := range entryContextMessages(entries[index]) {
			tokens, err := estimateAgentMessageTokens(message)
			if err != nil {
				return cutPointResult{}, err
			}
			messageTokens, err = checkedAddTokens(messageTokens, tokens)
			if err != nil {
				return cutPointResult{}, err
			}
		}
		if messageTokens == 0 {
			continue
		}
		var err error
		accumulated, err = checkedAddTokens(accumulated, messageTokens)
		if err != nil {
			return cutPointResult{}, err
		}
		if accumulated >= keepRecentTokens {
			for _, candidate := range cutPoints {
				if candidate >= index {
					cutIndex = candidate
					break
				}
			}
			break
		}
	}
	for cutIndex > start {
		previous := entries[cutIndex-1]
		if previous.compaction != nil || len(entryMessages(previous)) > 0 {
			break
		}
		cutIndex--
	}
	startsTurn := isTurnStartEntry(entries[cutIndex])
	turnStart := -1
	if !startsTurn {
		for index := cutIndex; index >= start; index-- {
			if isTurnStartEntry(entries[index]) {
				turnStart = index
				break
			}
		}
	}
	return cutPointResult{firstKeptEntryIndex: cutIndex, turnStartIndex: turnStart, isSplitTurn: !startsTurn && turnStart != -1}, nil
}

func entryMessages(entry Entry) []agentmsg.Message {
	if entry.compaction != nil {
		return nil
	}
	if message, ok := entry.AgentMessage(); ok {
		return []agentmsg.Message{message}
	}
	if payload, ok := entry.Payload().(BranchSummaryPayload); ok && strings.TrimSpace(payload.Summary) != "" {
		message, err := agentmsg.NewBranchSummary(agentmsg.BranchSummary{Summary: payload.Summary, FromID: payload.FromID, At: entry.timestamp})
		if err == nil {
			return []agentmsg.Message{message}
		}
	}
	return nil
}

func entryContextMessages(entry Entry) []agentmsg.Message {
	if record, ok := entry.Compaction(); ok {
		message, err := agentmsg.NewCompactionSummary(agentmsg.CompactionSummary{
			Summary: record.Summary, TokensBefore: record.TokensBefore, At: entry.timestamp,
		})
		if err == nil {
			return []agentmsg.Message{message}
		}
		return nil
	}
	return entryMessages(entry)
}

func messagesFromEntries(entries []Entry) []agentmsg.Message {
	messages := make([]agentmsg.Message, 0, len(entries))
	for _, entry := range entries {
		messages = append(messages, entryMessages(entry)...)
	}
	return messages
}

func isCutPointMessage(message agentmsg.Message) bool {
	switch message.Role() {
	case agentmsg.RoleUser, agentmsg.RoleAssistant, agentmsg.RoleBashExecution, agentmsg.RoleCustom,
		agentmsg.RoleBranchSummary, agentmsg.RoleCompactionSummary:
		return true
	default:
		return false
	}
}

func isTurnStartEntry(entry Entry) bool {
	if entry.compaction != nil {
		return false
	}
	for _, message := range entryMessages(entry) {
		switch message.Role() {
		case agentmsg.RoleUser, agentmsg.RoleBashExecution, agentmsg.RoleCustom, agentmsg.RoleBranchSummary, agentmsg.RoleCompactionSummary:
			return true
		}
	}
	return false
}

func estimateAgentMessageTokens(message agentmsg.Message) (uint64, error) {
	switch value := message.(type) {
	case agentmsg.LLM:
		return estimateMessageTokens(value.Conversation())
	case agentmsg.BashExecution:
		return tokenEstimateForChars(jsStringLength(value.Command) + jsStringLength(value.Output)), nil
	case agentmsg.Custom:
		var chars int
		for _, block := range value.Content() {
			switch block := block.(type) {
			case llm.TextBlock:
				chars += jsStringLength(block.Text())
			case llm.ImageBlock:
				chars += 4800
			}
		}
		return tokenEstimateForChars(chars), nil
	case agentmsg.BranchSummary:
		return tokenEstimateForChars(jsStringLength(value.Summary)), nil
	case agentmsg.CompactionSummary:
		return tokenEstimateForChars(jsStringLength(value.Summary)), nil
	default:
		return 0, nil
	}
}

// jsStringLength mirrors JavaScript's UTF-16 String.length, which the original
// compaction heuristic uses. Supplementary Unicode code points occupy two
// units; ordinary Go byte length would significantly overcount non-ASCII text.
func jsStringLength(value string) int {
	units := 0
	for _, r := range value {
		if r > 0xffff {
			units += 2
		} else {
			units++
		}
	}
	return units
}

func tokenEstimateForChars(chars int) uint64 {
	if chars <= 0 {
		return 0
	}
	return uint64((chars + 3) / 4)
}

const summarizationSystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

const summarizationPrompt = `The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned by user]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

const updateSummarizationPrompt = `The messages above are NEW conversation messages to incorporate into the existing summary provided in <previous-summary> tags.

Update the existing structured summary with new information. RULES:
- PRESERVE all existing information from the previous summary
- ADD new progress, decisions, and context from the new messages
- UPDATE the Progress section: move items from "In Progress" to "Done" when completed
- UPDATE "Next Steps" based on what was accomplished
- PRESERVE exact file paths, function names, and error messages
- If something is no longer relevant, you may remove it

Use this EXACT format:

## Goal
[Preserve existing goals, add new ones if the task expanded]

## Constraints & Preferences
- [Preserve existing, add new ones discovered]

## Progress
### Done
- [x] [Include previously done items AND newly completed items]

### In Progress
- [ ] [Current work - update based on progress]

### Blocked
- [Current blockers - remove if resolved]

## Key Decisions
- **[Decision]**: [Brief rationale] (preserve all previous, add new)

## Next Steps
1. [Update based on current state]

## Critical Context
- [Preserve important context, add new if needed]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

const turnPrefixSummarizationPrompt = `This is the PREFIX of a turn that was too large to keep. The SUFFIX (recent work) is retained.

Summarize the prefix to provide context for the retained suffix:

## Original Request
[What did the user ask for in this turn?]

## Early Progress
- [Key decisions and work done in the prefix]

## Context for Suffix
- [Information needed to understand the retained recent work]

Be concise. Focus on what's needed to understand the kept suffix.`

func summarizePrompt(messages []agentmsg.Message, previousSummary, instructions string) (string, error) {
	converted, err := agentmsg.ConvertToLLM(messages)
	if err != nil {
		return "", err
	}
	base := summarizationPrompt
	if previousSummary != "" {
		base = updateSummarizationPrompt
	}
	if instructions != "" {
		base += "\n\nAdditional focus: " + instructions
	}
	prompt := "<conversation>\n" + SerializeConversation(converted) + "\n</conversation>\n\n"
	if previousSummary != "" {
		prompt += "<previous-summary>\n" + previousSummary + "\n</previous-summary>\n\n"
	}
	return prompt + base, nil
}

func turnPrefixPrompt(messages []agentmsg.Message) (string, error) {
	converted, err := agentmsg.ConvertToLLM(messages)
	if err != nil {
		return "", err
	}
	return "<conversation>\n" + SerializeConversation(converted) + "\n</conversation>\n\n" + turnPrefixSummarizationPrompt, nil
}

func extractFileOperations(messages []agentmsg.Message, entries []Entry, previousCompactionIndex int) FileOperations {
	operations := FileOperations{}
	if previousCompactionIndex >= 0 {
		if payload, ok := entries[previousCompactionIndex].Payload().(CompactionPayload); ok && !payload.FromHook && len(payload.Details) != 0 {
			var details struct {
				ReadFiles     json.RawMessage `json:"readFiles"`
				ModifiedFiles json.RawMessage `json:"modifiedFiles"`
			}
			if json.Unmarshal(payload.Details, &details) == nil {
				var values []string
				if json.Unmarshal(details.ReadFiles, &values) == nil {
					for _, path := range values {
						operations.Read = appendUnique(operations.Read, path)
					}
				}
				values = nil
				if json.Unmarshal(details.ModifiedFiles, &values) == nil {
					for _, path := range values {
						operations.Edited = appendUnique(operations.Edited, path)
					}
				}
			}
		}
	}
	for _, message := range messages {
		extractFileOperationsFromMessage(message, &operations)
	}
	return operations
}

func extractFileOperationsFromMessage(message agentmsg.Message, operations *FileOperations) {
	wrapped, ok := message.(agentmsg.LLM)
	if !ok {
		return
	}
	var blocks []llm.AssistantBlock
	switch assistant := wrapped.Conversation().(type) {
	case llm.AssistantToolUseMessage:
		blocks = assistant.Blocks()
	case llm.AssistantRichMessage:
		blocks = assistant.Blocks()
	case llm.AssistantFailureMessage:
		blocks = assistant.Blocks()
	default:
		return
	}
	for _, block := range blocks {
		call, ok := block.(llm.ToolCallBlock)
		if !ok {
			continue
		}
		path, ok := toolArgumentString(call.ArgumentsJSON(), "path")
		if !ok || path == "" {
			continue
		}
		switch call.Name() {
		case "read":
			operations.Read = appendUnique(operations.Read, path)
		case "write":
			operations.Written = appendUnique(operations.Written, path)
		case "edit":
			operations.Edited = appendUnique(operations.Edited, path)
		}
	}
}

func toolArgumentString(raw json.RawMessage, name string) (string, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return "", false
	}
	value, ok := object[name]
	if !ok {
		return "", false
	}
	var result string
	if json.Unmarshal(value, &result) != nil {
		return "", false
	}
	return result, true
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

// ComputeFileLists returns files only read and all files modified, exactly as
// pi stores them in CompactionDetails.
func ComputeFileLists(operations FileOperations) CompactionDetails {
	modifiedSet := make(map[string]struct{}, len(operations.Edited)+len(operations.Written))
	for _, path := range append(append([]string(nil), operations.Edited...), operations.Written...) {
		modifiedSet[path] = struct{}{}
	}
	modified := make([]string, 0, len(modifiedSet))
	for path := range modifiedSet {
		modified = append(modified, path)
	}
	read := make([]string, 0, len(operations.Read))
	for _, path := range uniqueSorted(operations.Read) {
		if _, changed := modifiedSet[path]; !changed {
			read = append(read, path)
		}
	}
	sort.Strings(read)
	sort.Strings(modified)
	return CompactionDetails{ReadFiles: read, ModifiedFiles: modified}
}

// FormatFileOperations appends the exact XML sections used by pi.
func FormatFileOperations(details CompactionDetails) string {
	sections := make([]string, 0, 2)
	if len(details.ReadFiles) > 0 {
		sections = append(sections, "<read-files>\n"+strings.Join(details.ReadFiles, "\n")+"\n</read-files>")
	}
	if len(details.ModifiedFiles) > 0 {
		sections = append(sections, "<modified-files>\n"+strings.Join(details.ModifiedFiles, "\n")+"\n</modified-files>")
	}
	if len(sections) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(sections, "\n\n")
}

func finalizeCompactionOutput(input SummaryInput, output SummaryOutput) (SummaryOutput, error) {
	if output.FromExtension {
		return output, nil
	}
	details := ComputeFileLists(input.FileOperations)
	encoded, err := json.Marshal(details)
	if err != nil {
		return SummaryOutput{}, fmt.Errorf("encode compaction details: %w", err)
	}
	output.Text += FormatFileOperations(details)
	output.Details = encoded
	return output, nil
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
			if content := joinTextBlocks(value.Content()); content != "" {
				parts = append(parts, "[User]: "+content)
			}
		case llm.UserContentMessage:
			if content := joinUserContent(value.Content()); content != "" {
				parts = append(parts, "[User]: "+content)
			}
		case llm.AssistantTextMessage:
			appendAssistantSerialization(&parts, value.Blocks())
		case llm.AssistantRichMessage:
			appendAssistantSerialization(&parts, value.Blocks())
		case llm.AssistantToolUseMessage:
			appendAssistantSerialization(&parts, value.Blocks())
		case llm.AssistantFailureMessage:
			appendAssistantSerialization(&parts, value.Blocks())
		case llm.ToolResultMessage:
			if content := joinTextBlocks(value.Content()); content != "" {
				parts = append(parts, "[Tool result]: "+truncateToolResult(content, maxToolResultChars))
			}
		case llm.ToolResultContentMessage:
			if content := joinToolResultContent(value.Content()); content != "" {
				parts = append(parts, "[Tool result]: "+truncateToolResult(content, maxToolResultChars))
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func joinUserContent(blocks []llm.UserContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block, ok := block.(llm.TextBlock); ok {
			parts = append(parts, block.Text())
		}
	}
	return strings.Join(parts, "")
}

func appendAssistantSerialization(parts *[]string, blocks []llm.AssistantBlock) {
	thinking := make([]string, 0)
	text := make([]string, 0)
	calls := make([]string, 0)
	for _, block := range blocks {
		switch block := block.(type) {
		case llm.TextBlock:
			text = append(text, block.Text())
		case llm.ThinkingBlock:
			thinking = append(thinking, block.Thinking())
		case llm.ToolCallBlock:
			calls = append(calls, block.Name()+"("+formatToolArguments(block.ArgumentsJSON())+")")
		}
	}
	if len(thinking) > 0 {
		*parts = append(*parts, "[Assistant thinking]: "+strings.Join(thinking, "\n"))
	}
	if len(text) > 0 {
		*parts = append(*parts, "[Assistant]: "+strings.Join(text, "\n"))
	}
	if len(calls) > 0 {
		*parts = append(*parts, "[Assistant tool calls]: "+strings.Join(calls, "; "))
	}
}

func formatToolArguments(raw json.RawMessage) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ""
	}
	parts := make([]string, 0)
	for decoder.More() {
		key, ok := decoderTokenString(decoder)
		if !ok {
			return ""
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return ""
		}
		var compact bytes.Buffer
		if json.Compact(&compact, value) != nil {
			return ""
		}
		parts = append(parts, key+"="+compact.String())
	}
	return strings.Join(parts, ", ")
}

func decoderTokenString(decoder *json.Decoder) (string, bool) {
	token, err := decoder.Token()
	if err != nil {
		return "", false
	}
	value, ok := token.(string)
	return value, ok
}

func joinToolResultContent(blocks []llm.ToolResultContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block, ok := block.(llm.TextBlock); ok {
			parts = append(parts, block.Text())
		}
	}
	return strings.Join(parts, "")
}

func truncateToolResult(text string, max int) string {
	units := jsStringLength(text)
	if units <= max {
		return text
	}
	// JavaScript slices by UTF-16 code units. Go cannot represent the unpaired
	// surrogate produced when a slice boundary bisects a supplementary rune, so
	// keep the last complete rune while retaining JavaScript's unit count in the
	// marker. All ordinary boundaries are byte-for-byte equivalent.
	var prefix strings.Builder
	kept := 0
	for _, r := range text {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if kept+width > max {
			break
		}
		prefix.WriteRune(r)
		kept += width
	}
	return prefix.String() + "\n\n[... " + strconv.Itoa(units-max) + " more characters truncated]"
}

func compactJSONString(raw []byte) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return string(raw)
	}
	return compact.String()
}

func joinTextBlocks(blocks []llm.TextBlock) string {
	values := make([]string, len(blocks))
	for index, block := range blocks {
		values[index] = block.Text()
	}
	return strings.Join(values, "")
}

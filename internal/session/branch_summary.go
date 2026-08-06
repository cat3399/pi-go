package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

const (
	BranchSummaryDefaultReserveTokens uint64 = 16_384
	BranchSummaryMaxOutputTokens      uint64 = 2_048
)

const BranchSummaryPreamble = `The user explored a different conversation branch before returning here.
Summary of that exploration:

`

const BranchSummaryPrompt = `Create a structured summary of this conversation branch for context when returning later.

Use this EXACT format:

## Goal
[What was the user trying to accomplish in this branch?]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Work that was started but not finished]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [What should happen next to continue this work]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

type BranchSummaryDetails struct {
	ReadFiles     []string `json:"readFiles"`
	ModifiedFiles []string `json:"modifiedFiles"`
}

type BranchPreparation struct {
	Messages    []agentmsg.Message
	FileOps     FileOperations
	TotalTokens uint64
}

type BranchSummaryInput struct {
	SystemPrompt string
	Prompt       string
	Messages     []agentmsg.Message
	FileOps      FileOperations
	TotalTokens  uint64
	TokenBudget  uint64
	MaxTokens    uint64
}

type BranchSummaryOutput struct {
	Text    string
	Usage   *CompactionUsage
	Aborted bool
	Error   string
}

type BranchSummarizer interface {
	SummarizeBranch(context.Context, BranchSummaryInput) (BranchSummaryOutput, error)
}

type CollectEntriesResult struct {
	Entries          []Entry
	CommonAncestorID *string
}

// CollectEntriesForBranchSummary returns the old-branch suffix after the
// deepest common ancestor. Both paths are root-first immutable snapshots.
func CollectEntriesForBranchSummary(oldPath, targetPath []Entry) CollectEntriesResult {
	if len(oldPath) == 0 {
		return CollectEntriesResult{}
	}
	oldIDs := make(map[string]struct{}, len(oldPath))
	for _, entry := range oldPath {
		oldIDs[entry.ID()] = struct{}{}
	}
	common := ""
	for index := len(targetPath) - 1; index >= 0; index-- {
		if _, ok := oldIDs[targetPath[index].ID()]; ok {
			common = targetPath[index].ID()
			break
		}
	}
	start := 0
	if common != "" {
		for index := len(oldPath) - 1; index >= 0; index-- {
			if oldPath[index].ID() == common {
				start = index + 1
				break
			}
		}
	}
	entries := make([]Entry, len(oldPath)-start)
	for index := range entries {
		entries[index] = oldPath[start+index].clone()
	}
	return CollectEntriesResult{Entries: entries, CommonAncestorID: optionalBranchID(common)}
}

func optionalBranchID(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}

// PrepareBranchEntries mirrors pi's newest-to-oldest budget walk. Summary
// details are collected from the complete abandoned suffix before budgeting.
func PrepareBranchEntries(entries []Entry, tokenBudget uint64) (BranchPreparation, error) {
	preparation := BranchPreparation{}
	for _, entry := range entries {
		if payload, ok := entry.Payload().(BranchSummaryPayload); ok && !payload.FromHook {
			inheritBranchDetails(payload.Details, &preparation.FileOps)
		}
	}
	for index := len(entries) - 1; index >= 0; index-- {
		message, ok, err := branchMessageFromEntry(entries[index])
		if err != nil {
			return BranchPreparation{}, err
		}
		if !ok {
			continue
		}
		extractFileOperationsFromMessage(message, &preparation.FileOps)
		tokens, err := estimateAgentMessageTokens(message)
		if err != nil {
			return BranchPreparation{}, err
		}
		wouldExceed := false
		if tokenBudget > 0 {
			if tokens > tokenBudget || preparation.TotalTokens > tokenBudget-tokens {
				wouldExceed = true
			}
		}
		if wouldExceed {
			if isBranchSummaryEntry(entries[index]) && belowNinetyPercent(preparation.TotalTokens, tokenBudget) {
				preparation.Messages = append([]agentmsg.Message{agentmsg.CloneOne(message)}, preparation.Messages...)
				var addErr error
				preparation.TotalTokens, addErr = checkedAddTokens(preparation.TotalTokens, tokens)
				if addErr != nil {
					return BranchPreparation{}, addErr
				}
			}
			break
		}
		preparation.Messages = append([]agentmsg.Message{agentmsg.CloneOne(message)}, preparation.Messages...)
		preparation.TotalTokens, err = checkedAddTokens(preparation.TotalTokens, tokens)
		if err != nil {
			return BranchPreparation{}, err
		}
	}
	return preparation, nil
}

func belowNinetyPercent(total, budget uint64) bool {
	// total < budget * 0.9, without overflowing either side.
	return total/9 < budget/10 || (total/9 == budget/10 && total%9*10 < budget%10*9)
}

func isBranchSummaryEntry(entry Entry) bool {
	if entry.compaction != nil {
		return true
	}
	_, ok := entry.Payload().(BranchSummaryPayload)
	return ok
}

func branchMessageFromEntry(entry Entry) (agentmsg.Message, bool, error) {
	if message, ok := entry.AgentMessage(); ok {
		if message.Role() == agentmsg.RoleToolResult {
			return nil, false, nil
		}
		return message, true, nil
	}
	switch payload := entry.Payload().(type) {
	case BranchSummaryPayload:
		message, err := agentmsg.NewBranchSummary(agentmsg.BranchSummary{Summary: payload.Summary, FromID: payload.FromID, At: entry.Timestamp()})
		return message, err == nil, err
	case CompactionPayload:
		message, err := agentmsg.NewCompactionSummary(agentmsg.CompactionSummary{
			Summary: payload.Record.Summary, TokensBefore: payload.Record.TokensBefore, At: entry.Timestamp(),
		})
		return message, err == nil, err
	default:
		return nil, false, nil
	}
}

func inheritBranchDetails(raw json.RawMessage, operations *FileOperations) {
	if len(raw) == 0 || operations == nil {
		return
	}
	var details struct {
		ReadFiles     json.RawMessage `json:"readFiles"`
		ModifiedFiles json.RawMessage `json:"modifiedFiles"`
	}
	if json.Unmarshal(raw, &details) != nil {
		return
	}
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

func BuildBranchSummaryInput(entries []Entry, contextWindow, reserveTokens uint64, customInstructions string, replaceInstructions bool) (BranchSummaryInput, error) {
	if contextWindow == 0 {
		contextWindow = 128_000
	}
	tokenBudget := uint64(0)
	if contextWindow > reserveTokens {
		tokenBudget = contextWindow - reserveTokens
	}
	preparation, err := PrepareBranchEntries(entries, tokenBudget)
	if err != nil {
		return BranchSummaryInput{}, err
	}
	instructions := BranchSummaryPrompt
	if replaceInstructions && customInstructions != "" {
		instructions = customInstructions
	} else if customInstructions != "" {
		instructions += "\n\nAdditional focus: " + customInstructions
	}
	converted, err := agentmsg.ConvertToLLM(preparation.Messages)
	if err != nil {
		return BranchSummaryInput{}, err
	}
	prompt := "<conversation>\n" + SerializeConversation(converted) + "\n</conversation>\n\n" + instructions
	return BranchSummaryInput{
		SystemPrompt: summarizationSystemPrompt, Prompt: prompt, Messages: agentmsg.Clone(preparation.Messages),
		FileOps: cloneFileOperations(preparation.FileOps), TotalTokens: preparation.TotalTokens,
		TokenBudget: tokenBudget, MaxTokens: BranchSummaryMaxOutputTokens,
	}, nil
}

func FinalizeBranchSummary(text string, operations FileOperations) (string, json.RawMessage, error) {
	details := ComputeFileLists(operations)
	if details.ReadFiles == nil {
		details.ReadFiles = []string{}
	}
	if details.ModifiedFiles == nil {
		details.ModifiedFiles = []string{}
	}
	branchDetails := BranchSummaryDetails{ReadFiles: details.ReadFiles, ModifiedFiles: details.ModifiedFiles}
	encoded, err := json.Marshal(branchDetails)
	if err != nil {
		return "", nil, fmt.Errorf("encode branch summary details: %w", err)
	}
	return BranchSummaryPreamble + text + FormatFileOperations(details), encoded, nil
}

func BranchEditorText(entry Entry) (string, bool) {
	if message, ok := entry.Message(); ok && message.Role() == llm.RoleUser {
		switch value := message.(type) {
		case llm.UserTextMessage:
			return joinTextBlocks(value.Content()), true
		case llm.UserContentMessage:
			return joinUserContent(value.Content()), true
		}
	}
	if payload, ok := entry.Payload().(CustomMessagePayload); ok {
		var text strings.Builder
		for _, block := range payload.Message.Content() {
			if value, ok := block.(llm.TextBlock); ok {
				text.WriteString(value.Text())
			}
		}
		return text.String(), true
	}
	return "", false
}

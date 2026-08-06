package session

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCollectEntriesForBranchSummaryUsesDeepestCommonAncestor(t *testing.T) {
	manager := newManagerForBranchSummaryTest(t)
	root, err := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, "root", time.UnixMilli(1)))
	if err != nil {
		t.Fatal(err)
	}
	common, err := manager.AppendLLMMessage(context.Background(), managerAssistant(t, "common", time.UnixMilli(2)))
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, "target", time.UnixMilli(3)))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Branch(common.ID()); err != nil {
		t.Fatal(err)
	}
	oldOne, err := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, "old", time.UnixMilli(4)))
	if err != nil {
		t.Fatal(err)
	}
	oldTwo, err := manager.AppendLLMMessage(context.Background(), managerAssistant(t, "old answer", time.UnixMilli(5)))
	if err != nil {
		t.Fatal(err)
	}
	oldPath, _ := manager.BranchPath("")
	targetPath, _ := manager.BranchPath(target.ID())
	result := CollectEntriesForBranchSummary(oldPath, targetPath)
	if result.CommonAncestorID == nil || *result.CommonAncestorID != common.ID() || len(result.Entries) != 2 || result.Entries[0].ID() != oldOne.ID() || result.Entries[1].ID() != oldTwo.ID() {
		t.Fatalf("result=%#v", result)
	}
	if empty := CollectEntriesForBranchSummary(nil, []Entry{root}); len(empty.Entries) != 0 || empty.CommonAncestorID != nil {
		t.Fatalf("empty=%#v", empty)
	}
}

func TestPrepareBranchEntriesOnlyInheritsBranchSummaryFileOperationsBeforeBudgeting(t *testing.T) {
	manager := newManagerForBranchSummaryTest(t)
	root, err := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, "root", time.UnixMilli(1)))
	if err != nil {
		t.Fatal(err)
	}
	fromHook := false
	compaction, err := manager.AppendCompaction(context.Background(), "compacted", root.ID(), 10, json.RawMessage(`{"readFiles":["a.go"],"modifiedFiles":["b.go"]}`), &fromHook, nil)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := manager.BranchWithSummary(context.Background(), &[]string{compaction.ID()}[0], "older branch", json.RawMessage(`{"readFiles":["c.go"],"modifiedFiles":["d.go"]}`), &fromHook, nil)
	if err != nil {
		t.Fatal(err)
	}
	entries := manager.Entries()
	prepared, err := PrepareBranchEntries(entries, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prepared.FileOps.Read, []string{"c.go"}) || !reflect.DeepEqual(prepared.FileOps.Edited, []string{"d.go"}) {
		t.Fatalf("file ops=%#v", prepared.FileOps)
	}
	budgeted, err := PrepareBranchEntries([]Entry{branch}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(budgeted.Messages) != 1 || budgeted.TotalTokens == 0 {
		t.Fatalf("budgeted=%#v", budgeted)
	}
}

func TestBuildAndFinalizeBranchSummaryPromptParity(t *testing.T) {
	manager := newManagerForBranchSummaryTest(t)
	entry, err := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, "work", time.UnixMilli(1)))
	if err != nil {
		t.Fatal(err)
	}
	input, err := BuildBranchSummaryInput([]Entry{entry}, 128_000, 16_384, "custom only", true)
	if err != nil {
		t.Fatal(err)
	}
	if input.SystemPrompt != summarizationSystemPrompt || input.TokenBudget != 111_616 || input.MaxTokens != 2_048 || !strings.Contains(input.Prompt, "custom only") || strings.Contains(input.Prompt, "## Goal") {
		t.Fatalf("input=%#v", input)
	}
	text, raw, err := FinalizeBranchSummary("done", FileOperations{})
	if err != nil || text != BranchSummaryPreamble+"done" {
		t.Fatalf("text=%q raw=%s err=%v", text, raw, err)
	}
	var details BranchSummaryDetails
	if err := json.Unmarshal(raw, &details); err != nil || details.ReadFiles == nil || details.ModifiedFiles == nil || len(details.ReadFiles) != 0 || len(details.ModifiedFiles) != 0 {
		t.Fatalf("details=%#v raw=%s err=%v", details, raw, err)
	}
}

func newManagerForBranchSummaryTest(t *testing.T) *SessionManager {
	t.Helper()
	directory := t.TempDir()
	manager, err := CreateSessionManager(directory, directory, NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

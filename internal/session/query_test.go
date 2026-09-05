package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

func TestProjectContextAtReturnsCompactionAwareEntriesWithoutMovingLeaf(t *testing.T) {
	manager, err := InMemorySessionManagerWithOptions(t.TempDir(), ManagerOptions{
		NewSession: NewSessionOptions{ID: "query-context"},
		Now:        sequenceClock(time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)),
		NewEntryID: sequenceIDs("root", "first-answer", "kept", "second-answer", "compact", "tail"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	appendMessage := func(message llm.ConversationMessage) Entry {
		t.Helper()
		entry, appendErr := manager.AppendLLMMessage(context.Background(), message)
		if appendErr != nil {
			t.Fatal(appendErr)
		}
		return entry
	}
	root := appendMessage(mustUserMessage(t, "root", time.UnixMilli(1)))
	firstAnswer := appendMessage(managerAssistant(t, "first", time.UnixMilli(2)))
	kept := appendMessage(mustUserMessage(t, "kept", time.UnixMilli(3)))
	secondAnswer := appendMessage(managerAssistant(t, "second", time.UnixMilli(4)))
	compact, err := manager.AppendCompaction(context.Background(), "checkpoint", kept.ID(), 42, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tail := appendMessage(mustUserMessage(t, "tail", time.UnixMilli(5)))

	selectedBefore, _ := manager.LeafID()
	earlier, err := manager.ProjectContextAt(firstAnswer.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(earlier.EntryIDs, []string{root.ID(), firstAnswer.ID()}) {
		t.Fatalf("earlier entry ids = %v", earlier.EntryIDs)
	}
	if got := messageTexts(earlier.Context.Messages()); !slices.Equal(got, []string{"root", "first"}) {
		t.Fatalf("earlier messages = %v", got)
	}

	current, err := manager.ProjectContextAt(tail.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(current.EntryIDs, []string{compact.ID(), kept.ID(), secondAnswer.ID(), tail.ID()}) {
		t.Fatalf("compaction-aware entry ids = %v", current.EntryIDs)
	}
	if got := messageTexts(current.Context.Messages()); !slices.Equal(got, []string{CompactionSummaryPrefix + "checkpoint" + CompactionSummarySuffix, "kept", "second", "tail"}) {
		t.Fatalf("compaction-aware messages = %v", got)
	}
	selectedAfter, _ := manager.LeafID()
	if selectedAfter != selectedBefore {
		t.Fatalf("ProjectContextAt moved leaf from %q to %q", selectedBefore, selectedAfter)
	}
	if _, err := manager.ProjectContextAt("missing"); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("missing leaf error = %v", err)
	}
}

func TestExplicitAgentDirDiscoveryDoesNotDependOnProcessConfiguration(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	agentDir := filepath.Join(root, "isolated-agent")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := SessionDirForAgentDir(cwd, agentDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SessionDirForAgentDir created directory: %v", err)
	}
	manager, err := CreateSessionManager(cwd, dir, NewSessionOptions{ID: "explicit-agent-dir"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), managerAssistant(t, "persisted", time.UnixMilli(1))); err != nil {
		t.Fatal(err)
	}
	file, _ := manager.SessionFile()
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_GO_AGENT_DIR", filepath.Join(root, "different-agent"))

	project, err := ListSessionsInAgentDir(cwd, agentDir, nil)
	if err != nil || len(project) != 1 || project[0].Path != file {
		t.Fatalf("explicit project discovery = %#v, %v", project, err)
	}
	all, err := ListAllSessionsInAgentDir(agentDir, nil)
	if err != nil || len(all) != 1 || all[0].ID != "explicit-agent-dir" {
		t.Fatalf("explicit all discovery = %#v, %v", all, err)
	}
	recent, err := FindMostRecentSessionInAgentDir(cwd, agentDir)
	if err != nil || recent != file {
		t.Fatalf("explicit recent discovery = %q, %v", recent, err)
	}
}

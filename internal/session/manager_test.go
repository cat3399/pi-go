package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

func TestSessionManagerLifecycleAndFirstAssistantPersistence(t *testing.T) {
	directory := t.TempDir()
	manager, err := CreateSessionManager(directory, directory, NewSessionOptions{ID: "custom.session-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	path, ok := manager.SessionFile()
	if !ok || manager.SessionID() != "custom.session-1" {
		t.Fatalf("file=%q ok=%v id=%q", path, ok, manager.SessionID())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new session file exists before assistant: %v", err)
	}
	user := mustUserMessage(t, "hello", time.UnixMilli(1))
	if _, err := manager.AppendLLMMessage(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("user-only session was persisted: %v", err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), managerAssistant(t, "hi", time.UnixMilli(2))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("assistant did not flush session: %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSessionManager(path, directory, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := len(reopened.Entries()); got != 2 {
		t.Fatalf("reopened entries=%d, want 2", got)
	}
	if got := len(reopened.BuildContext().Messages()); got != 2 {
		t.Fatalf("context messages=%d, want 2", got)
	}
}

func TestOpenSessionManagerInitializesOnlyEmptyExplicitFile(t *testing.T) {
	directory := t.TempDir()
	empty := filepath.Join(directory, "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := OpenSessionManager(empty, directory, directory)
	if err != nil {
		t.Fatal(err)
	}
	if manager.Header().ID() == "" {
		t.Fatal("empty explicit file did not receive a header")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(directory, "invalid.jsonl")
	original := []byte(`{"type":"event","data":"not a session"}` + "\n")
	if err := os.WriteFile(invalid, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSessionManager(invalid, directory, directory); err == nil {
		t.Fatal("non-empty invalid file opened")
	}
	after, err := os.ReadFile(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("invalid file was modified")
	}
}

func TestSessionManagerSetSessionFilePreservesInMemoryMode(t *testing.T) {
	directory := t.TempDir()
	project := filepath.Join(directory, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	manager, err := InMemorySessionManager(project, NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(directory, "explicit.jsonl")
	if err := manager.SetSessionFile(explicit); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if manager.Cwd() != project {
		t.Fatalf("cwd=%q want %q", manager.Cwd(), project)
	}
	if path, ok := manager.SessionFile(); !ok || path != explicit {
		t.Fatalf("path=%q ok=%v", path, ok)
	}
	if manager.IsPersisted() {
		t.Fatal("setSessionFile changed an in-memory manager to persistent")
	}
	if _, err := manager.AppendLLMMessage(context.Background(), managerAssistant(t, "persist", time.UnixMilli(2))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(explicit); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("in-memory manager wrote explicit path: %v", err)
	}
}

func TestSessionManagerSetSessionFileKeepsPersistentManagerCwdAndExplicitPath(t *testing.T) {
	directory := t.TempDir()
	project := filepath.Join(directory, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	manager, err := CreateSessionManager(project, directory, NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(directory, "explicit.jsonl")
	if err := manager.SetSessionFile(explicit); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if manager.Cwd() != project || !manager.IsPersisted() {
		t.Fatalf("cwd=%q persisted=%v", manager.Cwd(), manager.IsPersisted())
	}
	if path, ok := manager.SessionFile(); !ok || path != explicit {
		t.Fatalf("path=%q ok=%v", path, ok)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), managerAssistant(t, "persist", time.UnixMilli(2))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(explicit); err != nil {
		t.Fatal(err)
	}
}

func TestSessionManagerTypedEntriesLabelsNameAndBranchSummary(t *testing.T) {
	manager, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{ID: "memory-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	root, err := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, "root", time.UnixMilli(1)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendThinkingLevelChange(context.Background(), "high"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendModelChange(context.Background(), "deepseek", "deepseek-v4flash"); err != nil {
		t.Fatal(err)
	}
	label := "checkpoint"
	labelEntry, err := manager.AppendLabelChange(context.Background(), root.ID(), &label)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := manager.Label(root.ID()); !ok || got != label {
		t.Fatalf("label=%q ok=%v", got, ok)
	}
	if _, err := manager.AppendSessionInfo(context.Background(), " name\r\n\nwith newline "); err != nil {
		t.Fatal(err)
	}
	if got, ok := manager.SessionName(); !ok || got != "name with newline" {
		t.Fatalf("name=%q ok=%v", got, ok)
	}
	if _, err := manager.AppendCustomEntry(context.Background(), "state", json.RawMessage(`{"enabled":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := manager.Branch(root.ID()); err != nil {
		t.Fatal(err)
	}
	branch, err := manager.BranchWithSummary(context.Background(), stringPointer(root.ID()), "real supplied summary", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent, ok := branch.ParentID()
	if !ok || parent != root.ID() {
		t.Fatalf("branch parent=%q ok=%v", parent, ok)
	}
	if len(manager.BuildContext().AgentMessages()) != 2 {
		t.Fatalf("branch context should contain root and summary")
	}
	var branchWire map[string]json.RawMessage
	if err := json.Unmarshal(branch.RawJSON(), &branchWire); err != nil {
		t.Fatal(err)
	}
	if _, present := branchWire["fromHook"]; present {
		t.Fatal("omitted fromHook was serialized")
	}
	if got := manager.Children(root.ID()); len(got) != 2 {
		t.Fatalf("root children=%d, want thinking branch and summary branch", len(got))
	}
	if labelEntry.Type() != "label" {
		t.Fatalf("label entry type=%q", labelEntry.Type())
	}
}

func TestSessionManagerPreservesOptionalFalseAndEmptyLabelOnWire(t *testing.T) {
	manager, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	root, err := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, "root", time.UnixMilli(1)))
	if err != nil {
		t.Fatal(err)
	}
	empty := ""
	label, err := manager.AppendLabelChange(context.Background(), root.ID(), &empty)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.Label(root.ID()); ok {
		t.Fatal("empty label did not clear resolved label")
	}
	var labelWire map[string]json.RawMessage
	if err := json.Unmarshal(label.RawJSON(), &labelWire); err != nil {
		t.Fatal(err)
	}
	if raw, present := labelWire["label"]; !present || string(raw) != `""` {
		t.Fatalf("empty label wire=%s present=%v", raw, present)
	}
	fromHook := false
	summary, err := manager.BranchWithSummary(context.Background(), stringPointer(root.ID()), "summary", nil, &fromHook, nil)
	if err != nil {
		t.Fatal(err)
	}
	var summaryWire map[string]json.RawMessage
	if err := json.Unmarshal(summary.RawJSON(), &summaryWire); err != nil {
		t.Fatal(err)
	}
	if raw, present := summaryWire["fromHook"]; !present || string(raw) != "false" {
		t.Fatalf("explicit false fromHook wire=%s present=%v", raw, present)
	}
}

func TestSessionManagerEmptyContextHasOriginalThinkingDefault(t *testing.T) {
	manager, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	context := manager.BuildContext()
	if level, ok := context.ThinkingLevel(); !ok || level != "off" {
		t.Fatalf("thinking level=%q ok=%v", level, ok)
	}
	if _, ok := context.Model(); ok {
		t.Fatal("empty context unexpectedly selected a model")
	}
}

func TestSessionManagerCreateBranchedSessionRechainsLabelsAndDefers(t *testing.T) {
	directory := t.TempDir()
	manager, err := CreateSessionManager(directory, directory, NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	first, _ := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, "first", time.UnixMilli(1)))
	label := "important"
	if _, err := manager.AppendLabelChange(context.Background(), first.ID(), &label); err != nil {
		t.Fatal(err)
	}
	model, err := manager.AppendModelChange(context.Background(), "openai", "gpt")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, "second", time.UnixMilli(2)))
	if err != nil {
		t.Fatal(err)
	}
	newPath, ok, err := manager.CreateBranchedSession(context.Background(), second.ID())
	if err != nil || !ok {
		t.Fatalf("path=%q ok=%v err=%v", newPath, ok, err)
	}
	if _, err := os.Stat(newPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("assistant-free branch exists: %v", err)
	}
	modelAfter, found := manager.Entry(model.ID())
	if !found {
		t.Fatal("model change missing")
	}
	parent, ok := modelAfter.ParentID()
	if !ok || parent != first.ID() {
		t.Fatalf("model parent=%q ok=%v, want first message after removed label", parent, ok)
	}
	if got, ok := manager.Label(first.ID()); !ok || got != label {
		t.Fatalf("preserved label=%q ok=%v", got, ok)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), managerAssistant(t, "answer", time.UnixMilli(3))); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}
	var headerCount int
	ids := map[string]bool{}
	for _, line := range splitNonemptyLines(data) {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(line, &object); err != nil {
			t.Fatal(err)
		}
		var kind, id string
		_ = json.Unmarshal(object["type"], &kind)
		_ = json.Unmarshal(object["id"], &id)
		if kind == "session" {
			headerCount++
		} else if ids[id] {
			t.Fatalf("duplicate entry id %q", id)
		} else {
			ids[id] = true
		}
	}
	if headerCount != 1 {
		t.Fatalf("header count=%d", headerCount)
	}
}

func TestSessionDiscoveryContinueRecentAndListSorting(t *testing.T) {
	directory := t.TempDir()
	projectA := filepath.Join(directory, "project-a")
	projectB := filepath.Join(directory, "project-b")
	if err := os.MkdirAll(projectA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectB, 0o755); err != nil {
		t.Fatal(err)
	}
	pathA := createListedManager(t, projectA, directory, "from A", 100)
	time.Sleep(2 * time.Millisecond)
	pathB := createListedManager(t, projectB, directory, "from B", 200)
	if err := os.WriteFile(filepath.Join(directory, "invalid.jsonl"), []byte(`{"type":"event"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	listA, err := ListSessions(projectA, directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listA) != 1 || listA[0].Path != pathA || listA[0].FirstMessage != "from A" {
		t.Fatalf("project list=%+v", listA)
	}
	all, err := ListAllSessions(directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Path != pathB || all[1].Path != pathA {
		t.Fatalf("all ordering=%+v", all)
	}
	continued, err := ContinueRecentSession(projectA, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer continued.Close()
	if path, _ := continued.SessionFile(); path != pathA {
		t.Fatalf("continued=%q want %q", path, pathA)
	}
}

func TestSessionManagerForkFromCopiesForestAndParentMetadata(t *testing.T) {
	directory := t.TempDir()
	source, err := CreateSessionManager(directory, directory, NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	root, _ := source.AppendLLMMessage(context.Background(), mustUserMessage(t, "root", time.UnixMilli(1)))
	if _, err := source.AppendLLMMessage(context.Background(), managerAssistant(t, "a", time.UnixMilli(2))); err != nil {
		t.Fatal(err)
	}
	if err := source.Branch(root.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AppendLLMMessage(context.Background(), mustUserMessage(t, "other", time.UnixMilli(3))); err != nil {
		t.Fatal(err)
	}
	sourcePath, _ := source.SessionFile()
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	targetDir := t.TempDir()
	forked, err := ForkSessionFrom(context.Background(), sourcePath, targetDir, targetDir, NewSessionOptions{ID: "fork-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer forked.Close()
	if got := len(forked.Entries()); got != 3 {
		t.Fatalf("fork entries=%d", got)
	}
	parent, ok := forked.Header().ParentSession()
	if !ok || parent != sourcePath {
		t.Fatalf("parent=%q ok=%v", parent, ok)
	}
	if len(forked.Tree()) != 1 || len(forked.Tree()[0].Children) != 2 {
		t.Fatalf("fork did not retain complete forest")
	}
}

func TestSessionManagerIDValidationMatchesOriginal(t *testing.T) {
	for _, valid := range []string{"a", "abc-123", "a_b.c"} {
		if err := ValidateSessionID(valid); err != nil {
			t.Fatalf("valid %q: %v", valid, err)
		}
	}
	for _, invalid := range []string{"", "-abc", "abc-", "a/b", "hello world"} {
		if err := ValidateSessionID(invalid); !errors.Is(err, ErrInvalidSessionID) {
			t.Fatalf("invalid %q err=%v", invalid, err)
		}
	}
}

func TestSessionManagerConcurrentAppendsHaveOneDurableChain(t *testing.T) {
	directory := t.TempDir()
	manager, err := CreateSessionManager(directory, directory, NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	var wait sync.WaitGroup
	errorsChannel := make(chan error, 8)
	for index := 0; index < 8; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, appendErr := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, string(rune('a'+index)), time.UnixMilli(int64(index+1))))
			errorsChannel <- appendErr
		}()
	}
	wait.Wait()
	close(errorsChannel)
	for appendErr := range errorsChannel {
		if appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	if _, err := manager.AppendLLMMessage(context.Background(), managerAssistant(t, "done", time.UnixMilli(20))); err != nil {
		t.Fatal(err)
	}
	entries := manager.Entries()
	if len(entries) != 9 {
		t.Fatalf("entries=%d, want 9", len(entries))
	}
	for index, entry := range entries {
		parent, ok := entry.ParentID()
		if index == 0 {
			if ok {
				t.Fatalf("root has parent %q", parent)
			}
			continue
		}
		if !ok || parent != entries[index-1].ID() {
			t.Fatalf("entry %d parent=%q ok=%v", index, parent, ok)
		}
	}
}

func TestSessionManagerCompactionBoundaryOnlyPreparesAndCommitsRealOutput(t *testing.T) {
	manager, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	for index, message := range []llm.ConversationMessage{
		mustUserMessage(t, "old question", time.UnixMilli(1)),
		managerAssistant(t, "old answer", time.UnixMilli(2)),
		mustUserMessage(t, "recent question", time.UnixMilli(3)),
		managerAssistant(t, "recent answer", time.UnixMilli(4)),
	} {
		if _, err := manager.AppendLLMMessage(context.Background(), message); err != nil {
			t.Fatalf("append %d: %v", index, err)
		}
	}
	input, err := manager.PrepareCompaction(context.Background(), 1, "focus on files")
	if err != nil {
		t.Fatal(err)
	}
	if input.Instructions != "focus on files" || input.FirstKeptEntryID == "" || len(input.Messages) == 0 {
		t.Fatalf("preparation=%#v", input)
	}
	result, err := manager.CommitCompaction(context.Background(), input, SummaryOutput{Text: "provider-generated summary"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Committed || result.Entry.Type() != "compaction" || result.Output.Text != "provider-generated summary" {
		t.Fatalf("result=%#v", result)
	}
	if _, err := manager.CommitCompaction(context.Background(), input, SummaryOutput{Text: "stale"}); !errors.Is(err, ErrCompactionConflict) {
		t.Fatalf("stale commit error=%v", err)
	}
}

func managerAssistant(t *testing.T, text string, at time.Time) llm.AssistantTextMessage {
	t.Helper()
	message, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{mustTextBlock(t, text)},
		llm.FinishStop,
		mustUsage(t, 1, 1),
		at,
		llm.AssistantProvenance{API: "openai-completions", Provider: "deepseek", Model: "deepseek-v4flash"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func createListedManager(t *testing.T, cwd, directory, text string, timestamp int64) string {
	t.Helper()
	manager, err := CreateSessionManager(cwd, directory, NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, text, time.UnixMilli(timestamp))); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(context.Background(), managerAssistant(t, "reply", time.UnixMilli(timestamp+1))); err != nil {
		t.Fatal(err)
	}
	path, _ := manager.SessionFile()
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func stringPointer(value string) *string { return &value }

func splitNonemptyLines(data []byte) [][]byte {
	var result [][]byte
	start := 0
	for index, value := range data {
		if value != '\n' {
			continue
		}
		if index > start {
			result = append(result, data[start:index])
		}
		start = index + 1
	}
	return result
}

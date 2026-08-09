package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
	"github.com/cat3399/pi-go/internal/llm"
)

func TestSessionManagerMatchesTypeScriptGolden(t *testing.T) {
	expectedBytes, err := os.ReadFile(filepath.Join("testdata", "session_manager_compat.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected any
	if err := json.Unmarshal(expectedBytes, &expected); err != nil {
		t.Fatal(err)
	}
	actualSnapshot := compatGenerateSnapshot(t)
	actualBytes, err := json.Marshal(actualSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	var actual any
	if err := json.Unmarshal(actualBytes, &actual); err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(actual, expected) {
		return
	}
	path, want, got := compatFirstDifference(expected, actual, "$", 0)
	pretty, _ := json.MarshalIndent(actualSnapshot, "", "  ")
	t.Fatalf("TypeScript SessionManager golden mismatch at %s: want=%#v got=%#v\nGo snapshot:\n%s", path, want, got, pretty)
}

func compatGenerateSnapshot(t *testing.T) map[string]any {
	t.Helper()
	root := t.TempDir()
	return map[string]any{
		"formatVersion":          4,
		"optionalAndPersistence": compatOptionalAndPersistence(t, root),
		"treeAndSelection":       compatTreeAndSelection(t, root),
		"branchedAndFork":        compatBranchedAndFork(t, root),
		"reopenAndCompaction":    compatReopenAndCompaction(t, root),
		"damagedRecovery":        compatDamagedRecovery(t, root),
		"structuralRecovery":     compatStructuralRecovery(t, root),
	}
}

type compatIDs struct {
	names map[string]string
	next  int
}

func newCompatIDs() *compatIDs { return &compatIDs{names: make(map[string]string), next: 1} }

func (ids *compatIDs) name(id string, preferred ...string) any {
	if id == "" {
		return nil
	}
	if current, ok := ids.names[id]; ok {
		return current
	}
	name := ""
	if len(preferred) != 0 {
		name = preferred[0]
	} else {
		name = fmt.Sprintf("auto-%d", ids.next)
		ids.next++
	}
	ids.names[id] = name
	return name
}

func compatOptionalAndPersistence(t *testing.T, root string) map[string]any {
	t.Helper()
	cwd := filepath.Join(root, "optional-cwd")
	dir := filepath.Join(root, "optional-sessions")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	manager, err := CreateSessionManager(cwd, dir, NewSessionOptions{ID: "compat-optional"})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	file, ok := manager.SessionFile()
	if !ok {
		t.Fatal("persistent manager has no session file")
	}
	existsAfterCreate := compatExists(file)
	ids := newCompatIDs()
	rootEntry := compatAppendUser(t, manager, "optional root", 1000)
	ids.name(rootEntry.ID(), "root")
	existsAfterUser := compatExists(file)
	if _, err := manager.AppendCustomEntry(context.Background(), "undefined", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendCustomEntry(context.Background(), "null", json.RawMessage("null")); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendCustomEntry(context.Background(), "false", json.RawMessage("false")); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendThinkingLevelChange(context.Background(), "high"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendModelChange(context.Background(), "selected-provider", "selected-model"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendSessionInfo(context.Background(), "  Golden\r\nSession  "); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLabelChange(context.Background(), rootEntry.ID(), nil); err != nil {
		t.Fatal(err)
	}
	empty := ""
	if _, err := manager.AppendLabelChange(context.Background(), rootEntry.ID(), &empty); err != nil {
		t.Fatal(err)
	}
	fromHook := false
	compaction, err := manager.AppendCompaction(context.Background(), "optional summary", rootEntry.ID(), 77, json.RawMessage("null"), &fromHook, nil)
	if err != nil {
		t.Fatal(err)
	}
	ids.name(compaction.ID(), "compaction")
	existsAfterMetadata := compatExists(file)
	rootID := rootEntry.ID()
	summary, err := manager.BranchWithSummary(context.Background(), &rootID, "returned branch", nil, &fromHook, nil)
	if err != nil {
		t.Fatal(err)
	}
	ids.name(summary.ID(), "branch-summary")
	assistant := compatAppendAssistant(t, manager, "optional final", 2000)
	ids.name(assistant.ID(), "assistant")
	existsAfterAssistant := compatExists(file)
	records := compatLoadJSONL(t, file)
	entries := make([]map[string]any, 0, len(records)-1)
	for _, record := range records {
		if record["type"] != "session" {
			entries = append(entries, record)
		}
	}
	progress := make([][]int, 0)
	listed, err := ListSessions(cwd, dir, func(loaded, total int) { progress = append(progress, []int{loaded, total}) })
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{compatAbs(file): "$OPTIONAL_FILE"}
	cwds := map[string]string{compatAbs(cwd): "$OPTIONAL_CWD"}
	sessionIDs := map[string]string{manager.SessionID(): "compat-optional"}
	var finalProgress any
	if len(progress) != 0 {
		finalProgress = progress[len(progress)-1]
	}
	name, hasName := manager.SessionName()
	var sessionName any
	if hasName {
		sessionName = name
	}
	return map[string]any{
		"delayedPersistence": map[string]any{
			"existsAfterCreate": existsAfterCreate, "existsAfterUser": existsAfterUser,
			"existsAfterMetadata": existsAfterMetadata, "existsAfterAssistant": existsAfterAssistant,
		},
		"optionalFields":    compatOptionalFields(records),
		"entries":           compatEntries(entries, ids),
		"sessionName":       sessionName,
		"activeContext":     compatContext(manager, ids),
		"listProgressFinal": finalProgress,
		"list":              compatSessionInfos(listed, paths, cwds, sessionIDs),
	}
}

func compatTreeAndSelection(t *testing.T, root string) map[string]any {
	t.Helper()
	cwd := filepath.Join(root, "tree-cwd")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	manager, err := InMemorySessionManager(cwd, NewSessionOptions{ID: "compat-tree"})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ids := newCompatIDs()
	rootEntry := compatAppendUser(t, manager, "tree root", 10_000)
	ids.name(rootEntry.ID(), "root")
	thinking, err := manager.AppendThinkingLevelChange(context.Background(), "high")
	if err != nil {
		t.Fatal(err)
	}
	ids.name(thinking.ID(), "thinking")
	model, err := manager.AppendModelChange(context.Background(), "selected-provider", "selected-model")
	if err != nil {
		t.Fatal(err)
	}
	ids.name(model.ID(), "model")
	assistant := compatAppendAssistant(t, manager, "tree answer", 11_000)
	ids.name(assistant.ID(), "assistant")
	abandoned := compatAppendUser(t, manager, "abandoned", 12_000)
	ids.name(abandoned.ID(), "abandoned")
	if err := manager.Branch(model.ID()); err != nil {
		t.Fatal(err)
	}
	branch := compatAppendUser(t, manager, "selected branch", 13_000)
	ids.name(branch.ID(), "selected-branch")
	label := "checkpoint"
	labelEntry, err := manager.AppendLabelChange(context.Background(), rootEntry.ID(), &label)
	if err != nil {
		t.Fatal(err)
	}
	ids.name(labelEntry.ID(), "label")
	if err := manager.Branch(branch.ID()); err != nil {
		t.Fatal(err)
	}
	selected := compatContext(manager, ids)
	selectedPath, err := manager.BranchPath("")
	if err != nil {
		t.Fatal(err)
	}
	selectedBranch := compatEntryIDs(selectedPath, ids)
	if err := manager.ResetLeaf(); err != nil {
		t.Fatal(err)
	}
	reset := compatAppendUser(t, manager, "new root", 14_000)
	ids.name(reset.ID(), "reset-root")
	resetContext := compatContext(manager, ids)
	modelID := model.ID()
	summary, err := manager.BranchWithSummary(context.Background(), &modelID, "abandoned summary", nil, compatBool(false), nil)
	if err != nil {
		t.Fatal(err)
	}
	ids.name(summary.ID(), "branch-summary")
	summaryContext := compatContext(manager, ids)
	resolvedLabel, hasLabel := manager.Label(rootEntry.ID())
	var labelValue any
	if hasLabel {
		labelValue = resolvedLabel
	}
	entries := compatEntryRaw(manager.Entries())
	return map[string]any{
		"selected": selected, "selectedBranch": selectedBranch, "reset": resetContext,
		"summary": summaryContext, "resolvedLabel": labelValue,
		"entries": compatEntries(entries, ids), "tree": compatTree(manager.Tree(), ids),
	}
}

func compatBranchedAndFork(t *testing.T, root string) map[string]any {
	t.Helper()
	sourceCwd := filepath.Join(root, "source-cwd")
	targetCwd := filepath.Join(root, "target-cwd")
	dir := filepath.Join(root, "forest-sessions")
	for _, path := range []string{sourceCwd, targetCwd} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	source, err := CreateSessionManager(sourceCwd, dir, NewSessionOptions{ID: "compat-source"})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ids := newCompatIDs()
	rootEntry := compatAppendUser(t, source, "forest root", 100)
	ids.name(rootEntry.ID(), "root")
	assistant := compatAppendAssistant(t, source, "forest answer", 200)
	ids.name(assistant.ID(), "assistant")
	main := compatAppendUser(t, source, "main branch", 300)
	ids.name(main.ID(), "main")
	label := "forest-checkpoint"
	labelEntry, err := source.AppendLabelChange(context.Background(), rootEntry.ID(), &label)
	if err != nil {
		t.Fatal(err)
	}
	ids.name(labelEntry.ID(), "source-label")
	if err := source.Branch(assistant.ID()); err != nil {
		t.Fatal(err)
	}
	selected := compatAppendUser(t, source, "selected branch", 400)
	ids.name(selected.ID(), "selected")
	sourceFile, ok := source.SessionFile()
	if !ok {
		t.Fatal("source has no file")
	}
	sourceEntries := compatEntryRaw(source.Entries())
	sourceTree := compatTree(source.Tree(), ids)
	branchedFile, ok, err := source.CreateBranchedSession(context.Background(), selected.ID())
	if err != nil || !ok {
		t.Fatalf("CreateBranchedSession() = %q, %v, %v", branchedFile, ok, err)
	}
	branchedID := source.SessionID()
	branchedEntries := compatEntryRaw(source.Entries())
	branchedHeader := source.Header()
	branchedTree := compatTree(source.Tree(), ids)
	fork, err := ForkSessionFrom(context.Background(), sourceFile, targetCwd, dir, NewSessionOptions{ID: "compat-fork"})
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()
	forkFile, ok := fork.SessionFile()
	if !ok {
		t.Fatal("fork has no file")
	}
	forkHeader := fork.Header()
	forkEntries := compatEntryRaw(fork.Entries())
	forkTree := compatTree(fork.Tree(), ids)
	paths := map[string]string{
		compatAbs(sourceFile): "$SOURCE_FILE", compatAbs(branchedFile): "$BRANCHED_FILE", compatAbs(forkFile): "$FORK_FILE",
	}
	cwds := map[string]string{compatAbs(sourceCwd): "$SOURCE_CWD", compatAbs(targetCwd): "$TARGET_CWD"}
	sessionIDs := map[string]string{"compat-source": "compat-source", branchedID: "compat-branched", "compat-fork": "compat-fork"}
	projectList, err := ListSessions(sourceCwd, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	allList, err := ListAllSessions(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	branchedParent, hasBranchedParent := branchedHeader.ParentSession()
	forkParent, hasForkParent := forkHeader.ParentSession()
	branchedLabel, hasBranchedLabel := source.Label(rootEntry.ID())
	return map[string]any{
		"source": map[string]any{"entries": compatEntries(sourceEntries, ids), "tree": sourceTree},
		"branched": map[string]any{
			"fileExists": compatExists(branchedFile), "headerParent": compatCanonicalPath(branchedParent, hasBranchedParent, paths),
			"entries": compatEntries(branchedEntries, ids), "tree": branchedTree,
			"resolvedLabel": compatOptionalString(branchedLabel, hasBranchedLabel),
		},
		"fork": map[string]any{
			"fileExists": compatExists(forkFile), "headerParent": compatCanonicalPath(forkParent, hasForkParent, paths),
			"entries": compatEntries(forkEntries, ids), "tree": forkTree,
		},
		"projectList": compatSessionInfos(projectList, paths, cwds, sessionIDs),
		"allList":     compatSessionInfos(allList, paths, cwds, sessionIDs),
	}
}

func compatReopenAndCompaction(t *testing.T, root string) map[string]any {
	t.Helper()
	cwd := filepath.Join(root, "reopen-cwd")
	targetCwd := filepath.Join(root, "reopen-target-cwd")
	dir := filepath.Join(root, "reopen-sessions")
	for _, path := range []string{cwd, targetCwd} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := CreateSessionManagerWithOptions(cwd, dir, ManagerOptions{
		NewSession: NewSessionOptions{ID: "compat-reopen"},
		NewEntryID: sequenceIDs("root-id", "thinking-id", "model-id", "first-assistant-id", "kept-id", "second-assistant-id", "compaction-id", "tail-id"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := newCompatIDs()
	rootEntry := compatAppendUser(t, manager, "reopen root", 1_000)
	ids.name(rootEntry.ID(), "root")
	thinking, err := manager.AppendThinkingLevelChange(context.Background(), "high")
	if err != nil {
		t.Fatal(err)
	}
	ids.name(thinking.ID(), "thinking")
	model, err := manager.AppendModelChange(context.Background(), "selected-provider", "selected-model")
	if err != nil {
		t.Fatal(err)
	}
	ids.name(model.ID(), "model")
	firstAssistant := compatAppendAssistant(t, manager, "first answer", 2_000)
	ids.name(firstAssistant.ID(), "first-assistant")
	kept := compatAppendUser(t, manager, "kept question", 3_000)
	ids.name(kept.ID(), "kept")
	secondAssistant := compatAppendAssistant(t, manager, "second answer", 4_000)
	ids.name(secondAssistant.ID(), "second-assistant")
	compaction, err := manager.AppendCompaction(context.Background(), "reopen checkpoint", kept.ID(), 99, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ids.name(compaction.ID(), "compaction")
	tail := compatAppendUser(t, manager, "tail question", 5_000)
	ids.name(tail.ID(), "tail")
	file, ok := manager.SessionFile()
	if !ok {
		t.Fatal("reopen manager has no file")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSessionManager(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	reopenedLeaf, _ := reopened.LeafID()
	reopenedContext := compatContext(reopened, ids)
	earlier, err := reopened.ProjectContextAt(firstAssistant.ID())
	if err != nil {
		t.Fatal(err)
	}
	earlierContext := compatProjectedContext(earlier, ids)
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	fork, err := ForkSessionFrom(context.Background(), file, targetCwd, dir, NewSessionOptions{ID: "compat-reopen-fork"})
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()
	forkParent, hasForkParent := fork.Header().ParentSession()
	return map[string]any{
		"reopenedLeaf": ids.name(reopenedLeaf),
		"reopened":     reopenedContext,
		"earlierLeaf":  earlierContext,
		"fork": map[string]any{
			"headerParentIsSource": hasForkParent && compatAbs(forkParent) == compatAbs(file),
			"context":              compatContext(fork, ids),
			"entries":              compatEntries(compatEntryRaw(fork.Entries()), ids),
		},
	}
}

func compatDamagedRecovery(t *testing.T, root string) map[string]any {
	t.Helper()
	cwd := filepath.Join(root, "damaged-cwd")
	dir := filepath.Join(root, "damaged-sessions")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "damaged.jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": "compat-damaged", "timestamp": "2026-08-09T00:00:00.000Z", "cwd": cwd})
	rootEntry := `{"type":"message","id":"damaged-root","parentId":null,"timestamp":"2026-08-09T00:00:01.000Z","message":{"role":"user","content":"damaged root","timestamp":1000}}`
	orphanEntry := `{"type":"message","id":"damaged-orphan","parentId":"missing-parent","timestamp":"2026-08-09T00:00:02.000Z","message":{"role":"user","content":"damaged orphan","timestamp":2000}}`
	data := []byte("not-json-before-header\n" + string(header) + "\n" + rootEntry + "\nnot-json-middle\n" + orphanEntry + "\n{")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := OpenSessionManager(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	ids := newCompatIDs()
	ids.name("damaged-root", "root")
	ids.name("damaged-orphan", "orphan")
	entries := compatEntries(compatEntryRaw(manager.Entries()), ids)
	tree := compatTree(manager.Tree(), ids)
	active := compatContext(manager, ids)
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSessionManager(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	return map[string]any{
		"entries": entries, "tree": tree, "activeContext": active,
		"reopenedContext": compatContext(reopened, ids),
	}
}

func compatStructuralRecovery(t *testing.T, root string) map[string]any {
	t.Helper()
	cwd := filepath.Join(root, "structural-cwd")
	dir := filepath.Join(root, "structural-sessions")
	for _, path := range []string{cwd, dir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(dir, "structural.jsonl")
	records := []string{
		`{"type":"session","version":3,"id":"compat-structural","timestamp":"2026-08-09T01:00:00.000Z","cwd":` + strconv.Quote(cwd) + `}`,
		`{"type":"message","id":"forward-child","parentId":"root","timestamp":"2026-08-09T01:00:01.000Z","message":{"role":"user","content":"forward child","timestamp":1000}}`,
		`{"type":"message","id":"root","parentId":null,"timestamp":"2026-08-09T01:00:02.000Z","message":{"role":"user","content":"root","timestamp":2000}}`,
		`{"type":"message","id":"legacy-assistant","parentId":"forward-child","timestamp":"2026-08-09T01:00:03.000Z","message":{"role":"assistant","content":null,"api":"openai-completions","provider":"compat-provider","model":"compat-model","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0},"stopReason":"stop","timestamp":3000}}`,
		`{"type":"compaction","id":"bad-compaction","parentId":"legacy-assistant","timestamp":"2026-08-09T01:00:04.000Z","summary":"damaged checkpoint","firstKeptEntryId":"missing-kept","tokensBefore":12}`,
	}
	prefix := []byte(strings.Join(records, "\n") + "\n")
	tail := append([]byte(`{"type":"message","id":"utf8-tail","parentId":"bad-compaction","timestamp":"2026-08-09T01:00:05.000Z","message":{"role":"user","content":"tail `), 0xe1, 0x80, 'A')
	tail = append(tail, []byte(` text","timestamp":5000}}`+"\n")...)
	source := append(prefix, tail...)
	if err := os.WriteFile(file, source, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := OpenSessionManager(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ids := newCompatIDs()
	ids.name("root", "root")
	ids.name("forward-child", "forward-child")
	ids.name("legacy-assistant", "legacy-assistant")
	ids.name("bad-compaction", "bad-compaction")
	ids.name("utf8-tail", "utf8-tail")
	forward, err := manager.ProjectContextAt("forward-child")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := manager.ProjectContextAt("legacy-assistant")
	if err != nil {
		t.Fatal(err)
	}
	tailProjection, err := manager.ProjectContextAt("utf8-tail")
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"entries":           compatEntries(compatEntryRaw(manager.Entries()), ids),
		"forwardLeaf":       compatProjectedContext(forward, ids),
		"legacyAssistant":   compatProjectedContext(legacy, ids),
		"badCompactionTail": compatProjectedContext(tailProjection, ids),
		"tree":              compatTree(manager.Tree(), ids),
		"sourceUnchanged":   bytes.Equal(after, source),
	}
}

func compatAppendUser(t *testing.T, manager *SessionManager, text string, timestamp int64) Entry {
	t.Helper()
	message, err := llm.NewUserTextMessage(text, time.UnixMilli(timestamp))
	if err != nil {
		t.Fatal(err)
	}
	entry, err := manager.AppendLLMMessage(context.Background(), message)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func compatAppendAssistant(t *testing.T, manager *SessionManager, text string, timestamp int64) Entry {
	t.Helper()
	message, err := llm.NewAssistantTextMessage(
		[]llm.TextBlock{mustTextBlock(t, text)}, llm.FinishStop, mustUsage(t, 1, 1), time.UnixMilli(timestamp),
		llm.AssistantProvenance{API: "openai-completions", Provider: "compat-provider", Model: "compat-model"},
	)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := manager.AppendLLMMessage(context.Background(), message)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func compatEntries(entries []map[string]any, ids *compatIDs) []map[string]any {
	for _, entry := range entries {
		ids.name(fmt.Sprint(entry["id"]))
	}
	result := make([]map[string]any, len(entries))
	for index, entry := range entries {
		result[index] = compatEntry(entry, ids)
	}
	return result
}

func compatEntry(entry map[string]any, ids *compatIDs) map[string]any {
	typeName := fmt.Sprint(entry["type"])
	parentID, _ := entry["parentId"].(string)
	result := map[string]any{
		"type": typeName, "id": ids.name(fmt.Sprint(entry["id"])), "parentId": ids.name(parentID),
		"timestamp": compatTimestamp(entry["timestamp"]),
	}
	switch typeName {
	case "message":
		message, _ := entry["message"].(map[string]any)
		canonical := map[string]any{"role": message["role"], "text": compatTextContent(message), "timestamp": message["timestamp"]}
		if message["role"] == "assistant" {
			canonical["provider"], canonical["model"] = message["provider"], message["model"]
		}
		result["message"] = canonical
	case "thinking_level_change":
		result["thinkingLevel"] = entry["thinkingLevel"]
	case "model_change":
		result["provider"], result["modelId"] = entry["provider"], entry["modelId"]
	case "custom":
		result["customType"] = entry["customType"]
	case "session_info":
		result["name"] = entry["name"]
	case "label":
		result["targetId"] = ids.name(fmt.Sprint(entry["targetId"]))
		result["label"] = compatOwn(entry, "label")
	case "compaction":
		result["summary"], result["firstKeptEntryId"], result["tokensBefore"] = entry["summary"], ids.name(fmt.Sprint(entry["firstKeptEntryId"])), entry["tokensBefore"]
	case "branch_summary":
		fromID := fmt.Sprint(entry["fromId"])
		if fromID == "root" {
			result["fromId"] = "root"
		} else {
			result["fromId"] = ids.name(fromID)
		}
		result["summary"] = entry["summary"]
	}
	return result
}

func compatContext(manager *SessionManager, ids *compatIDs) map[string]any {
	return compatContextParts(manager.ContextEntries(), manager.BuildContext(), ids)
}

func compatProjectedContext(projection ContextProjection, ids *compatIDs) map[string]any {
	return compatContextParts(projection.Entries, projection.Context, ids)
}

func compatContextParts(entries []Entry, contextValue Context, ids *compatIDs) map[string]any {
	messages := contextValue.AgentMessages()
	canonicalMessages := make([]map[string]any, len(messages))
	for index, message := range messages {
		canonicalMessages[index] = compatAgentMessage(message, ids)
	}
	level, hasLevel := contextValue.ThinkingLevel()
	if !hasLevel {
		level = "off"
	}
	model, hasModel := contextValue.Model()
	var modelValue any
	if hasModel {
		modelValue = map[string]any{"provider": model.Provider, "modelId": model.ModelID}
	}
	types := make([]string, len(entries))
	for index, entry := range entries {
		types[index] = entry.Type()
	}
	return map[string]any{
		"entryPath": compatEntryIDs(entries, ids), "entryTypes": types, "messages": canonicalMessages,
		"thinkingLevel": level, "model": modelValue,
	}
}

func compatAgentMessage(message agentmsg.Message, ids *compatIDs) map[string]any {
	switch value := message.(type) {
	case agentmsg.LLM:
		return map[string]any{"role": string(value.Role()), "text": compatConversationText(value.Conversation())}
	case agentmsg.BranchSummary:
		return map[string]any{"role": string(value.Role()), "summary": value.Summary, "fromId": ids.name(value.FromID)}
	case agentmsg.CompactionSummary:
		return map[string]any{"role": string(value.Role()), "summary": value.Summary, "tokensBefore": value.TokensBefore}
	default:
		return map[string]any{"role": string(message.Role())}
	}
}

func compatConversationText(message llm.ConversationMessage) string {
	switch value := message.(type) {
	case llm.UserTextMessage:
		parts := make([]string, 0, len(value.Content()))
		for _, block := range value.Content() {
			parts = append(parts, block.Text())
		}
		return strings.Join(parts, " ")
	case llm.UserContentMessage:
		parts := make([]string, 0)
		for _, block := range value.Content() {
			if text, ok := block.(llm.TextBlock); ok {
				parts = append(parts, text.Text())
			}
		}
		return strings.Join(parts, " ")
	case llm.AssistantTextMessage:
		parts := make([]string, 0, len(value.Content()))
		for _, block := range value.Content() {
			parts = append(parts, block.Text())
		}
		return strings.Join(parts, " ")
	case llm.AssistantRichMessage:
		return compatAssistantBlocks(value.Blocks())
	case llm.AssistantToolUseMessage:
		return compatAssistantBlocks(value.Blocks())
	default:
		return ""
	}
}

func compatAssistantBlocks(blocks []llm.AssistantBlock) string {
	parts := make([]string, 0)
	for _, block := range blocks {
		if text, ok := block.(llm.TextBlock); ok {
			parts = append(parts, text.Text())
		}
	}
	return strings.Join(parts, " ")
}

func compatTree(nodes []TreeNode, ids *compatIDs) []map[string]any {
	result := make([]map[string]any, len(nodes))
	for index, node := range nodes {
		var label any
		if node.Label != nil {
			label = *node.Label
		}
		children := compatTree(node.Children, ids)
		result[index] = map[string]any{
			"id": ids.name(node.Entry.ID()), "type": node.Entry.Type(), "label": label,
			"hasLabelTimestamp": node.LabelTimestamp != nil, "children": children,
		}
	}
	return result
}

func compatSessionInfos(values []SessionInfo, paths, cwds, sessionIDs map[string]string) []map[string]any {
	result := make([]map[string]any, len(values))
	for index, value := range values {
		name := compatOptionalString(value.Name, value.HasName)
		parent := compatCanonicalPath(value.ParentSessionPath, value.HasParentSession, paths)
		id := sessionIDs[value.ID]
		if id == "" {
			id = value.ID
		}
		cwd := cwds[compatAbs(value.Cwd)]
		if cwd == "" {
			cwd = "$UNKNOWN_CWD"
		}
		path := paths[compatAbs(value.Path)]
		if path == "" {
			path = "$UNKNOWN_PATH"
		}
		result[index] = map[string]any{
			"path": path, "id": id, "cwd": cwd, "name": name, "parentSessionPath": parent,
			"createdValid": !value.Created.IsZero(), "modifiedMs": value.Modified.UnixMilli(),
			"messageCount": value.MessageCount, "firstMessage": value.FirstMessage, "allMessagesText": value.AllMessagesText,
		}
	}
	sort.Slice(result, func(i, j int) bool { return fmt.Sprint(result[i]["id"]) < fmt.Sprint(result[j]["id"]) })
	return result
}

func compatOptionalFields(records []map[string]any) []map[string]any {
	result := make([]map[string]any, 0)
	for _, entry := range records {
		switch entry["type"] {
		case "session":
			result = append(result,
				map[string]any{"case": "header.timestamp", "field": compatTimestamp(entry["timestamp"])},
				map[string]any{"case": "header.parentSession", "field": compatOwn(entry, "parentSession")},
			)
		case "custom":
			result = append(result, map[string]any{"case": fmt.Sprintf("custom.%v.data", entry["customType"]), "field": compatOwn(entry, "data")})
		case "label":
			labelCase := "undefined"
			if value, ok := entry["label"]; ok {
				labelCase = fmt.Sprint(value)
			}
			result = append(result, map[string]any{"case": "label." + labelCase, "field": compatOwn(entry, "label")})
		case "compaction":
			for _, field := range []string{"details", "fromHook", "usage"} {
				result = append(result, map[string]any{"case": "compaction." + field, "field": compatOwn(entry, field)})
			}
		case "branch_summary":
			for _, field := range []string{"details", "fromHook", "usage"} {
				result = append(result, map[string]any{"case": "branch_summary." + field, "field": compatOwn(entry, field)})
			}
		}
	}
	return result
}

func compatOwn(object map[string]any, key string) map[string]any {
	value, ok := object[key]
	if !ok {
		return map[string]any{"present": false}
	}
	return map[string]any{"present": true, "value": value}
}

func compatTimestamp(value any) map[string]any {
	timestamp, present := value.(string)
	valid := false
	if present {
		parsed, err := time.Parse("2006-01-02T15:04:05.000Z", timestamp)
		valid = err == nil && parsed.Format("2006-01-02T15:04:05.000Z") == timestamp
	}
	return map[string]any{"present": present, "isoMilliseconds": valid}
}

func compatLoadJSONL(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(data, []byte{'\n'})
	result := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var object map[string]any
		if err := json.Unmarshal(line, &object); err != nil {
			t.Fatal(err)
		}
		result = append(result, object)
	}
	return result
}

func compatEntryRaw(entries []Entry) []map[string]any {
	result := make([]map[string]any, len(entries))
	for index, entry := range entries {
		semantic, _ := replaceInvalidUTF8LikeNode(entry.RawJSON())
		if err := json.Unmarshal(semantic, &result[index]); err != nil {
			panic(err)
		}
	}
	return result
}

func compatEntryIDs(entries []Entry, ids *compatIDs) []any {
	result := make([]any, len(entries))
	for index, entry := range entries {
		result[index] = ids.name(entry.ID())
	}
	return result
}

func compatTextContent(message map[string]any) string {
	if text, ok := message["content"].(string); ok {
		return text
	}
	blocks, _ := message["content"].([]any)
	parts := make([]string, 0)
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		if block["type"] == "text" {
			if text, ok := block["text"].(string); ok {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, " ")
}

func compatExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func compatAbs(path string) string {
	resolved, _ := filepath.Abs(path)
	return filepath.Clean(resolved)
}

func compatCanonicalPath(path string, present bool, paths map[string]string) any {
	if !present {
		return nil
	}
	if value := paths[compatAbs(path)]; value != "" {
		return value
	}
	return "$UNKNOWN_PATH"
}

func compatOptionalString(value string, present bool) any {
	if !present {
		return nil
	}
	return value
}

func compatBool(value bool) *bool { return &value }

func compatFirstDifference(expected, actual any, path string, depth int) (string, any, any) {
	if depth > 128 {
		return path, expected, actual
	}
	switch want := expected.(type) {
	case map[string]any:
		got, ok := actual.(map[string]any)
		if !ok {
			return path, expected, actual
		}
		keys := make([]string, 0, len(want)+len(got))
		seen := make(map[string]struct{})
		for key := range want {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
		for key := range got {
			if _, ok := seen[key]; !ok {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			wantValue, wantOK := want[key]
			gotValue, gotOK := got[key]
			if !wantOK || !gotOK {
				return path + "." + key, wantValue, gotValue
			}
			if !reflect.DeepEqual(wantValue, gotValue) {
				return compatFirstDifference(wantValue, gotValue, path+"."+key, depth+1)
			}
		}
	case []any:
		got, ok := actual.([]any)
		if !ok || len(want) != len(got) {
			return path, expected, actual
		}
		for index := range want {
			if !reflect.DeepEqual(want[index], got[index]) {
				return compatFirstDifference(want[index], got[index], fmt.Sprintf("%s[%d]", path, index), depth+1)
			}
		}
	default:
		return path, expected, actual
	}
	return path, expected, actual
}

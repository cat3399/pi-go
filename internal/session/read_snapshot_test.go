package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

func TestLatestUsageSurvivesToolResultsCompactionAndReopenWithoutCopyingHistory(t *testing.T) {
	dir := t.TempDir()
	m, err := CreateSessionManager(dir, dir, NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	appendMessage := func(message llm.ConversationMessage, err error) Entry {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		entry, err := m.AppendLLMMessage(context.Background(), message)
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	user := appendMessage(llm.NewUserTextMessage("inspect a long process", time.UnixMilli(1)))
	if mustReadSnapshot(t, m, SnapshotOptions{}).LatestUsage != nil {
		t.Fatal("user fabricated usage")
	}
	usage, _ := llm.NewUsage(llm.UsageSpec{Input: 12, Output: 8, CacheRead: 100})
	call, _ := llm.NewToolCallBlock("call", "read", []byte(`{}`))
	response := appendMessage(newAssistantToolUseMessage([]llm.AssistantBlock{call}, usage, time.UnixMilli(2)))
	text, _ := llm.NewTextBlock("tool output")
	appendMessage(llm.NewToolResultMessage("call", "read", []llm.TextBlock{text}, false, time.UnixMilli(3)))
	if _, err := m.AppendCompaction(context.Background(), "checkpoint", user.ID(), 120, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	appendMessage(newAssistantTextMessage([]llm.TextBlock{text}, llm.FinishStop, llm.Usage{}, time.UnixMilli(4)))
	appendMessage(newAssistantFailureMessage(nil, llm.FinishError, "failed request", usage, time.UnixMilli(5)))
	assertUsage := func(value ReadSnapshot) {
		t.Helper()
		got := value.LatestUsage
		if got == nil || got.EntryID != response.ID() || !got.Timestamp.Equal(time.UnixMilli(2)) || got.Usage != usage || got.Provenance != testLLMAssistantProvenance() {
			t.Fatalf("latest completed tool request lost usage: %#v", got)
		}
		if value.Entries != nil || value.Tree != nil {
			t.Fatal("usage read copied historical messages")
		}
	}
	assertUsage(mustReadSnapshot(t, m, SnapshotOptions{}))
	file, _ := m.SessionFile()
	index, err := OpenReadIndex(file)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := index.ReadSnapshot(SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertUsage(reopened)
	if err := m.Branch(user.ID()); err != nil {
		t.Fatal(err)
	}
	if mustReadSnapshot(t, m, SnapshotOptions{}).LatestUsage != nil {
		t.Fatal("future branch usage leaked into selected context")
	}
	assertUsage(mustReadSnapshot(t, m, SnapshotOptions{LeafID: response.ID()}))
}

func TestReadSnapshotVersionsPreserveDelayedPersistence(t *testing.T) {
	dir := t.TempDir()
	m, err := CreateSessionManager(dir, dir, NewSessionOptions{ID: "read-version"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	empty := mustReadSnapshot(t, m, SnapshotOptions{})
	if empty.Version.Generation == "" || empty.Version.Log != 0 || empty.LeafID != nil {
		t.Fatalf("empty version = %#v", empty)
	}
	user, err := m.AppendLLMMessage(context.Background(), mustUserMessage(t, "before flush", time.UnixMilli(1)))
	if err != nil {
		t.Fatal(err)
	}
	before := mustReadSnapshot(t, m, SnapshotOptions{IncludeContext: true, IncludeTree: true})
	if before.Version.Generation != empty.Version.Generation || before.Version.Log != 1 || before.Version.View <= empty.Version.View || *before.LeafID != user.ID() {
		t.Fatalf("user append version = %#v", before.Version)
	}
	file, _ := m.SessionFile()
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot forced user-only persistence: %v", err)
	}
	if _, err := m.AppendLLMMessage(context.Background(), managerAssistant(t, "flush", time.UnixMilli(2))); err != nil {
		t.Fatal(err)
	}
	after := mustReadSnapshot(t, m, SnapshotOptions{IncludeContext: true, IncludeTree: true})
	if after.Version.Generation != before.Version.Generation || after.Version.Log != 2 || after.Version.View <= before.Version.View {
		t.Fatalf("first-assistant flush replaced logical generation: before=%#v after=%#v", before.Version, after.Version)
	}
	if len(before.Entries) != 1 || *before.LeafID != user.ID() || len(before.Tree[0].Children) != 0 {
		t.Fatal("later append mutated the earlier read")
	}
	if err := m.Branch(user.ID()); err != nil {
		t.Fatal(err)
	}
	branched := mustReadSnapshot(t, m, SnapshotOptions{IncludeContext: true})
	if branched.Version.Generation != after.Version.Generation || branched.Version.Log != after.Version.Log || branched.Version.View <= after.Version.View || len(branched.Entries) != 1 {
		t.Fatalf("branch changed log identity: %#v", branched)
	}
	metadata := mustReadSnapshot(t, m, SnapshotOptions{})
	if metadata.Version != branched.Version || !reflect.DeepEqual(metadata.Info, branched.Info) || metadata.Entries != nil || metadata.Tree != nil {
		t.Fatalf("metadata-only read changed state or built history: %#v", metadata)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSessionManager(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored := mustReadSnapshot(t, reopened, SnapshotOptions{IncludeContext: true})
	if restored.Info.ID != before.Info.ID || restored.Version.Generation == before.Version.Generation || restored.Version.Log != 2 || len(restored.Entries) != 2 {
		t.Fatalf("reopen reused a stale generation/selection: %#v", restored)
	}
}

func TestReadSnapshotVersionsDetectSameIDSourceReplacement(t *testing.T) {
	dir := t.TempDir()
	m, err := InMemorySessionManager(dir, NewSessionOptions{ID: "reused"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	path := filepath.Join(dir, "source.jsonl")
	write := func(text string) {
		t.Helper()
		raw := testHeader + "\n" + userEntryJSON(text, "same-entry", "null", 1) + "\n"
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("before")
	if err := m.SetSessionFile(path); err != nil {
		t.Fatal(err)
	}
	before := mustReadSnapshot(t, m, SnapshotOptions{IncludeContext: true})
	write("after")
	if err := m.SetSessionFile(path); err != nil {
		t.Fatal(err)
	}
	after := mustReadSnapshot(t, m, SnapshotOptions{IncludeContext: true})
	if before.Info.ID != after.Info.ID || before.Version.Log != after.Version.Log || before.Version.Generation == after.Version.Generation || reflect.DeepEqual(before.Entries[0].RawJSON(), after.Entries[0].RawJSON()) {
		t.Fatalf("replacement identity = before %#v, after %#v", before.Version, after.Version)
	}
	if _, _, err := m.NewSession(NewSessionOptions{ID: after.Info.ID}); err != nil {
		t.Fatal(err)
	}
	reset := mustReadSnapshot(t, m, SnapshotOptions{})
	if reset.Version.Generation == after.Version.Generation || reset.Version.Log != 0 {
		t.Fatalf("same-ID new session reused old read version: %#v", reset.Version)
	}
}

func TestReadSnapshotKeepsBranchContextAndTreeTogetherDuringWrites(t *testing.T) {
	m, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	root, err := m.AppendLLMMessage(context.Background(), mustUserMessage(t, "root", time.UnixMilli(1)))
	if err != nil {
		t.Fatal(err)
	}
	message := mustUserMessage(t, "next", time.UnixMilli(2))
	done := make(chan error, 1)
	go func() {
		for index := 0; index < 100; index++ {
			if _, err := m.AppendLLMMessage(context.Background(), message); err != nil {
				done <- err
				return
			}
			if index%2 == 0 {
				if err := m.Branch(root.ID()); err != nil {
					done <- err
					return
				}
			}
		}
		done <- nil
	}()
	for index := 0; index < 100; index++ {
		snapshot := mustReadSnapshot(t, m, SnapshotOptions{IncludeContext: true, IncludeTree: true})
		if snapshot.LeafID == nil || len(snapshot.Entries) == 0 || snapshot.Entries[len(snapshot.Entries)-1].ID() != *snapshot.LeafID || len(snapshot.Context.Messages()) != len(snapshot.Entries) {
			t.Fatalf("context/leaf straddled a branch change: %#v", snapshot)
		}
		count := 0
		nodes := append([]TreeNode(nil), snapshot.Tree...)
		for len(nodes) != 0 {
			node := nodes[len(nodes)-1]
			nodes = append(nodes[:len(nodes)-1], node.Children...)
			count++
		}
		if snapshot.Version.Log != uint64(count) || snapshot.Info.MessageCount != count {
			t.Fatalf("summary/tree/version straddled append: version=%#v count=%d summary=%d", snapshot.Version, count, snapshot.Info.MessageCount)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	preview := mustReadSnapshot(t, m, SnapshotOptions{LeafID: root.ID(), IncludeContext: true})
	if len(preview.Entries) != 1 || preview.Entries[0].ID() != root.ID() || preview.Version.Log != 101 {
		t.Fatalf("preview changed the log or included siblings: %#v", preview)
	}
}

func TestReadSnapshotRejectsUncertainDurabilityAndClosedSources(t *testing.T) {
	storage := &fakeStorage{createCreated: true, appendStarted: true, appendErr: fmt.Errorf("sync failed")}
	store, err := createWithStorage(storage, filepath.Join(t.TempDir(), "uncertain.jsonl"), CreateOptions{ID: "uncertain", WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	m := &SessionManager{store: store}
	defer m.Close()
	if _, err := store.Append(context.Background(), mustUserMessage(t, "uncertain", time.UnixMilli(1)), AppendOptions{}); !errors.Is(err, ErrCommitUnknown) {
		t.Fatalf("expected an uncertain append: %v", err)
	}
	if _, err := m.ReadSnapshot(SnapshotOptions{}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("uncertain read validated cached state: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadSnapshot(SnapshotOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed read = %v", err)
	}
}

func mustReadSnapshot(t *testing.T, m *SessionManager, options SnapshotOptions) ReadSnapshot {
	t.Helper()
	value, err := m.ReadSnapshot(options)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

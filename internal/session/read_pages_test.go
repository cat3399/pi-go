package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHistoryPagesKeepCompletePreCompactionBranchAndFixedAnchor(t *testing.T) {
	m, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	var entries []Entry
	for i := 0; i < 30; i++ {
		entry, err := m.AppendLLMMessage(context.Background(), mustUserMessage(t, fmt.Sprintf("message %d", i), time.UnixMilli(int64(i+1))))
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	if _, err := m.AppendCompaction(context.Background(), "summary", entries[25].ID(), 100, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	want, _ := m.BranchPath("")
	page, err := m.ReadHistory(HistoryQuery{Limit: 4})
	if err != nil || page.Total != 31 || page.Start != 27 || page.Older == nil || len(m.ContextEntries()) >= page.Total {
		t.Fatalf("history page used compacted context: %#v, %v", page, err)
	}
	version, anchor := page.Version, page.AnchorID
	if err := m.Branch(entries[2].ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendLLMMessage(context.Background(), mustUserMessage(t, "another branch", time.UnixMilli(100))); err != nil {
		t.Fatal(err)
	}
	got := pageEntryIDs(page.Items)
	for page.Older != nil {
		page, err = m.ReadHistory(HistoryQuery{Cursor: page.Older, Limit: 4})
		if err != nil || page.Version != version || page.AnchorID != anchor || page.Total != len(want) {
			t.Fatalf("append/branch altered old page: %#v, %v", page, err)
		}
		got = append(pageEntryIDs(page.Items), got...)
	}
	var wantIDs []string
	for _, entry := range want {
		wantIDs = append(wantIDs, entry.ID())
	}
	if !reflect.DeepEqual(got, wantIDs) {
		t.Fatalf("paged path = %v, want %v", got, wantIDs)
	}
	if _, _, err := m.NewSession(NewSessionOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadHistory(HistoryQuery{Cursor: &HistoryCursor{Version: version, AnchorID: anchor, Position: 4, Direction: PageOlder}}); !errors.Is(err, ErrReadCursorUnavailable) {
		t.Fatalf("replaced source accepted old cursor: %v", err)
	}
}

func TestHistoryPagesLocateNodesMoveBothWaysAndReadOnlyNewTail(t *testing.T) {
	m, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	var entries []Entry
	for i := 0; i < 20; i++ {
		entry, err := m.AppendLLMMessage(context.Background(), mustUserMessage(t, "text", time.UnixMilli(int64(i+1))))
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	page, err := m.ReadHistory(HistoryQuery{AroundID: entries[9].ID(), Limit: 4})
	if err != nil || page.Start != 6 || page.End != 10 || page.Items[3].Node.ID != entries[9].ID() || page.Newer == nil {
		t.Fatalf("locate = %#v, %v", page, err)
	}
	newer, err := m.ReadHistory(HistoryQuery{Cursor: page.Newer, Limit: 4})
	if err != nil || newer.Start != 10 || newer.End != 14 || newer.Items[0].Node.ID != entries[10].ID() {
		t.Fatalf("newer = %#v, %v", newer, err)
	}
	tail, err := m.ReadHistory(HistoryQuery{AfterID: entries[17].ID(), Limit: 4})
	if err != nil || !tail.Connected || len(tail.Items) != 2 || tail.Items[0].Node.ID != entries[18].ID() {
		t.Fatalf("tail = %#v, %v", tail, err)
	}
	root := ""
	empty, err := m.ReadHistory(HistoryQuery{LeafID: &root})
	if err != nil || len(empty.Items) != 0 || empty.Total != 0 {
		t.Fatalf("virtual root = %#v, %v", empty, err)
	}
}

func TestHistoryCursorDoesNotResolveParentsAddedAfterItsLogPrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "forward.jsonl")
	raw := testHeader + "\n" + userEntryJSON("root", "root", "null", 1) + "\n" + userEntryJSON("one", "child1", `"future"`, 2) + "\n" + userEntryJSON("two", "child2", `"child1"`, 3) + "\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := InMemorySessionManagerWithOptions(dir, ManagerOptions{NewEntryID: func() (string, error) { return "future", nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.SetSessionFile(path); err != nil {
		t.Fatal(err)
	}
	page, err := m.ReadHistory(HistoryQuery{Limit: 1})
	if err != nil || page.Total != 2 || page.Older == nil {
		t.Fatalf("detached path = %#v, %v", page, err)
	}
	if err := m.Branch("root"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendLLMMessage(context.Background(), mustUserMessage(t, "parent arrives", time.UnixMilli(4))); err != nil {
		t.Fatal(err)
	}
	old, err := m.ReadHistory(HistoryQuery{Cursor: page.Older, Limit: 1})
	if err != nil || old.Total != 2 || old.Items[0].Node.ID != "child1" || old.Older != nil {
		t.Fatalf("cursor included a future parent: %#v, %v", old, err)
	}
	leaf := "child2"
	latest, err := m.ReadHistory(HistoryQuery{LeafID: &leaf})
	if err != nil || !reflect.DeepEqual(pageEntryIDs(latest.Items), []string{"root", "future", "child1", "child2"}) {
		t.Fatalf("latest view lost resolved ancestry: %#v, %v", latest, err)
	}
}

func TestOversizedHistoryEntryRemainsAvailableThroughBoundedChunks(t *testing.T) {
	m, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	entry, err := m.AppendLLMMessage(context.Background(), mustUserMessage(t, strings.Repeat("正文", 20000), time.UnixMilli(1)))
	if err != nil {
		t.Fatal(err)
	}
	page, err := m.ReadHistory(HistoryQuery{MaxBytes: 8 << 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].Entry != nil || page.Items[0].Node.ID != entry.ID() || page.Items[0].Node.Preview == "" {
		t.Fatalf("oversized page = %#v, %v", page, err)
	}
	var raw []byte
	for len(raw) < page.Items[0].Node.RawBytes {
		chunk, err := m.ReadEntryChunk(entry.ID(), page.Version.Generation, len(raw), 4096)
		if err != nil || len(chunk.Data) == 0 || len(chunk.Data) > 4096 {
			t.Fatalf("chunk = %#v, %v", chunk, err)
		}
		raw = append(raw, chunk.Data...)
	}
	if !bytes.Equal(raw, entry.RawJSON()) {
		t.Fatal("chunked entry was truncated or re-encoded")
	}
	if _, err := m.ReadEntryChunk(entry.ID(), "wrong-generation", 0, 4096); !errors.Is(err, ErrReadCursorUnavailable) {
		t.Fatalf("chunk accepted another generation: %v", err)
	}
}

func TestNodePagesKeepAllBranchesCandidatesAndVersionedLabels(t *testing.T) {
	m, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	first, _ := m.AppendLLMMessage(context.Background(), mustUserMessage(t, "search first", time.UnixMilli(1)))
	second, _ := m.AppendLLMMessage(context.Background(), mustUserMessage(t, "search second", time.UnixMilli(2)))
	if err := m.Branch(first.ID()); err != nil {
		t.Fatal(err)
	}
	third, _ := m.AppendLLMMessage(context.Background(), mustUserMessage(t, "search third", time.UnixMilli(3)))
	page, err := m.ReadNodes(NodeQuery{Role: "user", Search: "SEARCH", Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != first.ID() || !page.Items[0].HasChildren || page.Next == nil {
		t.Fatalf("first node page = %#v, %v", page, err)
	}
	label := "later"
	if _, err := m.AppendLabelChange(context.Background(), second.ID(), &label); err != nil {
		t.Fatal(err)
	}
	old, err := m.ReadNodes(NodeQuery{Role: "user", Search: "SEARCH", Limit: 10, Cursor: page.Next})
	if err != nil || len(old.Items) != 2 || old.Items[0].ID != second.ID() || old.Items[1].ID != third.ID() || old.Items[0].Label != "" {
		t.Fatalf("old index changed or omitted an off-branch user: %#v, %v", old, err)
	}
	parent := first.ID()
	children, err := m.ReadNodes(NodeQuery{ParentID: &parent, Role: "user"})
	if err != nil || len(children.Items) != 2 || children.Items[0].Label != label {
		t.Fatalf("children/labels = %#v, %v", children, err)
	}
}

func pageEntryIDs(items []PageEntry) []string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.Node.ID
	}
	return ids
}

func TestReadIndexOwnsNoWriterAndDetectsSameSizeTimestampReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.jsonl")
	raw := testHeader + "\n" + userEntryJSON("before", "one", "null", 1) + "\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := OpenReadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := index.ReadHistory(HistoryQuery{})
	writer, err := OpenSessionManager(path, dir, "")
	if err != nil {
		t.Fatalf("read index retained a writer claim: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	replacement := filepath.Join(dir, "replacement.jsonl")
	if err := os.WriteFile(replacement, []byte(strings.Replace(raw, "before", "after!", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".previous"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if valid, err := index.ValidateSource(); err != nil || valid {
		t.Fatalf("same-size/mtime replacement validated old data: %t, %v", valid, err)
	}
	old, _ := index.ReadHistory(HistoryQuery{})
	if !bytes.Equal(old.Items[0].Entry.RawJSON(), before.Items[0].Entry.RawJSON()) {
		t.Fatal("file replacement mutated an existing immutable index")
	}
	next, err := OpenReadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := next.ReadHistory(HistoryQuery{})
	if before.Version.Generation == after.Version.Generation || bytes.Equal(before.Items[0].Entry.RawJSON(), after.Items[0].Entry.RawJSON()) {
		t.Fatal("replacement reused old generation/data")
	}
	info, _ = os.Stat(path)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if valid, err := next.ValidateSource(); err != nil || valid {
		t.Fatalf("in-place rewrite with restored mtime validated old data: %t, %v", valid, err)
	}
}

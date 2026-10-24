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
	forward, err := m.ReadHistory(HistoryQuery{AfterID: entries[9].ID(), Direction: PageNewer, Limit: 4})
	if err != nil || !forward.Connected || forward.Start != 10 || forward.End != 14 || forward.Items[0].Node.ID != entries[10].ID() || forward.Newer == nil {
		t.Fatalf("forward tail = %#v, %v", forward, err)
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

func TestOversizedHistoryEntryIsCompleteAndPagesMakeProgress(t *testing.T) {
	m, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	first, _ := m.AppendLLMMessage(context.Background(), mustUserMessage(t, "before", time.UnixMilli(1)))
	large, err := m.AppendLLMMessage(context.Background(), mustUserMessage(t, strings.Repeat("正文", 20000), time.UnixMilli(2)))
	if err != nil {
		t.Fatal(err)
	}
	last, _ := m.AppendLLMMessage(context.Background(), mustUserMessage(t, "after", time.UnixMilli(3)))
	page, err := m.ReadHistory(HistoryQuery{MaxBytes: 8 << 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].Node.ID != last.ID() || page.Older == nil {
		t.Fatalf("latest page = %#v, %v", page, err)
	}
	middle, err := m.ReadHistory(HistoryQuery{Cursor: page.Older, MaxBytes: 8 << 10})
	if err != nil || len(middle.Items) != 1 || middle.Items[0].Entry == nil || middle.Items[0].Node.ID != large.ID() || middle.Older == nil || middle.Newer == nil {
		t.Fatalf("oversized page = %#v, %v", middle, err)
	}
	if !bytes.Equal(middle.Items[0].Entry.RawJSON(), large.RawJSON()) {
		t.Fatal("oversized content was changed")
	}
	older, err := m.ReadHistory(HistoryQuery{Cursor: middle.Older, MaxBytes: 8 << 10})
	if err != nil || len(older.Items) != 1 || older.Items[0].Node.ID != first.ID() || older.Older != nil {
		t.Fatalf("older page = %#v, %v", older, err)
	}
	newer, err := m.ReadHistory(HistoryQuery{Cursor: older.Newer, MaxBytes: 8 << 10})
	if err != nil || len(newer.Items) != 1 || newer.Items[0].Node.ID != large.ID() || newer.Items[0].Entry == nil {
		t.Fatalf("newer page = %#v, %v", newer, err)
	}
	if _, err := m.ReadEntry(large.ID(), "wrong-generation"); !errors.Is(err, ErrReadCursorUnavailable) {
		t.Fatalf("entry accepted another generation: %v", err)
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

func TestPagedTreeNodesPreserveCompleteTreeOrderAndBranchDepth(t *testing.T) {
	m, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	root, _ := m.AppendLLMMessage(context.Background(), mustUserMessage(t, "root", time.UnixMilli(1)))
	_, _ = m.AppendLLMMessage(context.Background(), mustUserMessage(t, "later branch", time.UnixMilli(20)))
	_, _ = m.AppendLLMMessage(context.Background(), mustUserMessage(t, "later child", time.UnixMilli(21)))
	if err := m.Branch(root.ID()); err != nil {
		t.Fatal(err)
	}
	_, _ = m.AppendLLMMessage(context.Background(), mustUserMessage(t, "earlier branch", time.UnixMilli(10)))
	_, _ = m.AppendLLMMessage(context.Background(), mustUserMessage(t, "earlier child", time.UnixMilli(11)))
	var want []string
	var wantDepth []int
	var visit func([]TreeNode, int)
	visit = func(nodes []TreeNode, depth int) {
		for _, node := range nodes {
			want = append(want, node.Entry.ID())
			wantDepth = append(wantDepth, depth)
			childDepth := depth
			if len(node.Children) > 1 {
				childDepth++
			}
			visit(node.Children, childDepth)
		}
	}
	visit(m.Tree(), 0)
	var got []string
	var gotDepth []int
	query := NodeQuery{Tree: true, Limit: 2}
	for {
		page, err := m.ReadNodes(query)
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range page.Items {
			got = append(got, node.ID)
			gotDepth = append(gotDepth, node.BranchDepth)
		}
		if page.Next == nil {
			break
		}
		query.Cursor = page.Next
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotDepth, wantDepth) {
		t.Fatalf("paged nodes = %v/%v, tree = %v/%v", got, gotDepth, want, wantDepth)
	}
	query.Tree = false
	if _, err := m.ReadNodes(query); !errors.Is(err, ErrInvalidReadQuery) {
		t.Fatalf("tree cursor accepted a different order: %v", err)
	}
}

func TestPathNodeIndexAndTailKeepTheirBranchWithoutReadingBodies(t *testing.T) {
	m, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	var entries []Entry
	for i := 0; i < 10; i++ {
		entry, _ := m.AppendLLMMessage(context.Background(), mustUserMessage(t, fmt.Sprintf("node %d", i), time.UnixMilli(int64(i+1))))
		entries = append(entries, entry)
	}
	anchor := entries[9].ID()
	page, err := m.ReadNodes(NodeQuery{LeafID: &anchor, Role: "user", Limit: 3})
	if err != nil || len(page.Items) != 3 || page.Next == nil {
		t.Fatalf("page = %#v, %v", page, err)
	}
	if err := m.Branch(entries[1].ID()); err != nil {
		t.Fatal(err)
	}
	off, _ := m.AppendLLMMessage(context.Background(), mustUserMessage(t, "off branch", time.UnixMilli(100)))
	next, err := m.ReadNodes(NodeQuery{LeafID: &anchor, Role: "user", Limit: 20, Cursor: page.Next})
	if err != nil || len(next.Items) != 7 || next.Items[0].ID != entries[3].ID() || next.Items[0].Position != 3 || next.Items[6].ID != anchor {
		t.Fatalf("fixed branch index = %#v, %v", next, err)
	}
	if err := m.Branch(anchor); err != nil {
		t.Fatal(err)
	}
	added, _ := m.AppendLLMMessage(context.Background(), mustUserMessage(t, "new tail", time.UnixMilli(101)))
	leaf := added.ID()
	tail, err := m.ReadNodes(NodeQuery{LeafID: &leaf, AfterID: anchor, Role: "user"})
	if err != nil || !tail.Connected || len(tail.Items) != 1 || tail.Items[0].ID != leaf || tail.Items[0].Position != 10 {
		t.Fatalf("tail = %#v, %v", tail, err)
	}
	offLeaf := off.ID()
	diverged, err := m.ReadNodes(NodeQuery{LeafID: &offLeaf, AfterID: leaf, Role: "user"})
	if err != nil || diverged.Connected || len(diverged.Items) != 3 {
		t.Fatalf("changed branch = %#v, %v", diverged, err)
	}
	if _, err := m.ReadNodes(NodeQuery{LeafID: &offLeaf, Cursor: page.Next, Role: "user"}); !errors.Is(err, ErrInvalidReadQuery) {
		t.Fatalf("cursor accepted another path: %v", err)
	}
	if _, err := m.AppendCompaction(context.Background(), "summary", entries[8].ID(), 10, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	leaf, _ = m.LeafID()
	controls, err := m.ReadNodes(NodeQuery{LeafID: &leaf, Type: "compaction"})
	if err != nil || len(controls.Items) != 1 || controls.Items[0].Type != "compaction" {
		t.Fatalf("control nodes = %#v, %v", controls, err)
	}
}

func TestEntryContentIdentitySurvivesReadOwnerChangeAndRejectsReusedID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.jsonl")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(testHeader+"\n"+userEntryJSON(body, "same-id", "null", 1)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	read := func() (ReadVersion, string) {
		t.Helper()
		manager, err := InMemorySessionManager(dir, NewSessionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer manager.Close()
		if err := manager.SetSessionFile(path); err != nil {
			t.Fatal(err)
		}
		page, err := manager.ReadHistory(HistoryQuery{})
		if err != nil {
			t.Fatal(err)
		}
		entry, err := manager.ReadEntry("same-id", page.Version.Generation)
		if err != nil || entry.ContentToken == "" || entry.ContentToken != page.Items[0].Node.ContentToken {
			t.Fatalf("entry/page identity differs: %v", err)
		}
		return page.Version, entry.ContentToken
	}
	write("original")
	first, original := read()
	second, reopened := read()
	if first.Generation == second.Generation || original != reopened {
		t.Fatal("an owner change changed immutable entry identity")
	}
	write("replacement")
	_, replacement := read()
	if replacement == original {
		t.Fatal("same entry ID with new content reused immutable identity")
	}
}

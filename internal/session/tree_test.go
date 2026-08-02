package session

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

func TestBranchTreeContextAndReopenUseSelectedLeaf(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "tree.jsonl")
	session, err := Create(path, CreateOptions{ID: "tree", WorkingDir: directory, Now: sequenceClock(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)), NewEntryID: sequenceIDs("a", "b", "c", "d", "e")})
	if err != nil {
		t.Fatal(err)
	}
	a := appendUser(t, session, "a")
	b := appendUser(t, session, "b")
	c := appendUser(t, session, "old")
	if err := session.SelectLeaf(b.ID()); err != nil {
		t.Fatal(err)
	}
	d := appendUser(t, session, "new")

	if got, want := entryIDs(session.BranchPath()), []string{a.ID(), b.ID(), d.ID()}; !equalIDs(got, want) {
		t.Fatalf("branch = %v, want %v", got, want)
	}
	if got := contextTexts(t, session.Context()); !equalIDs(got, []string{"a", "b", "new"}) {
		t.Fatalf("context = %v", got)
	}
	tree := session.Tree()
	if len(tree) != 1 || tree[0].Entry.ID() != a.ID() || len(tree[0].Children) != 1 || tree[0].Children[0].Entry.ID() != b.ID() {
		t.Fatalf("tree root = %#v", tree)
	}
	children := tree[0].Children[0].Children
	if got, want := nodeIDs(children), []string{c.ID(), d.ID()}; !equalIDs(got, want) {
		t.Fatalf("branch children = %v, want append order %v", got, want)
	}
	// Returned nodes are snapshots, including their raw bytes and child slices.
	tree[0].Children = nil
	if len(session.Tree()[0].Children) != 1 {
		t.Fatal("Tree() exposed mutable session structure")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, OpenOptions{NewEntryID: sequenceIDs("e")})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if leaf, ok := reopened.LeafID(); !ok || leaf != d.ID() {
		t.Fatalf("reopen leaf = (%q, %t), want physical tail %q", leaf, ok, d.ID())
	}
	if err := reopened.SelectLeaf(c.ID()); err != nil {
		t.Fatal(err)
	}
	e := appendUser(t, reopened, "continued-old")
	parent, ok := e.ParentID()
	if !ok || parent != c.ID() {
		t.Fatalf("selected append parent = (%q, %t), want %q", parent, ok, c.ID())
	}
}

func TestResetLeafPersistsNewRootForest(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "forest.jsonl")
	session, err := Create(path, CreateOptions{ID: "forest", WorkingDir: directory, NewEntryID: sequenceIDs("first", "second")})
	if err != nil {
		t.Fatal(err)
	}
	first := appendUser(t, session, "first root")
	if err := session.ResetLeaf(); err != nil {
		t.Fatal(err)
	}
	second := appendUser(t, session, "second root")
	if _, ok := second.ParentID(); ok {
		t.Fatal("reset append has a parent")
	}
	if got := contextTexts(t, session.Context()); !equalIDs(got, []string{"second root"}) {
		t.Fatalf("reset context = %v", got)
	}
	if got, want := nodeIDs(session.Tree()), []string{first.ID(), second.ID()}; !equalIDs(got, want) {
		t.Fatalf("forest roots = %v, want %v", got, want)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatalf("Open forest: %v", err)
	}
	defer reopened.Close()
	if got := contextTexts(t, reopened.Context()); !equalIDs(got, []string{"second root"}) {
		t.Fatalf("reopened context = %v", got)
	}
}

func TestExtractBranchIsAtomicAndNeverMutatesSource(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source.jsonl")
	source, err := Create(sourcePath, CreateOptions{ID: "source", WorkingDir: directory, Now: sequenceClock(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)), NewEntryID: sequenceIDs("a", "b", "c", "d")})
	if err != nil {
		t.Fatal(err)
	}
	a := appendUser(t, source, "a")
	b := appendUser(t, source, "b")
	_ = appendUser(t, source, "discarded")
	if err := source.SelectLeaf(b.ID()); err != nil {
		t.Fatal(err)
	}
	d := appendUser(t, source, "kept")
	before, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(directory, "branch.jsonl")
	target, err := source.ExtractBranch(context.Background(), d.ID(), ExtractOptions{TargetPath: targetPath, ID: "branch", WorkingDir: directory, Now: func() time.Time { return time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC) }, NewEntryID: sequenceIDs("next")})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	after, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("branch extraction changed source bytes")
	}
	if parent, ok := target.Header().ParentSession(); !ok || parent != sourcePath {
		t.Fatalf("target parentSession = (%q, %t), want source path", parent, ok)
	}
	if got, want := entryIDs(target.Entries()), []string{a.ID(), b.ID(), d.ID()}; !equalIDs(got, want) {
		t.Fatalf("extracted records = %v, want %v", got, want)
	}
	sourcePathEntries := source.BranchPath()
	targetEntries := target.Entries()
	for index := range targetEntries {
		if !bytes.Equal(sourcePathEntries[index].RawJSON(), targetEntries[index].RawJSON()) {
			t.Fatalf("entry %d was re-encoded during extraction", index)
		}
	}
	if got := contextTexts(t, target.Context()); !equalIDs(got, []string{"a", "b", "kept"}) {
		t.Fatalf("extracted context = %v", got)
	}
	continued := appendUser(t, target, "next")
	if parent, ok := continued.ParentID(); !ok || parent != d.ID() {
		t.Fatalf("target append parent = (%q, %t)", parent, ok)
	}

	blocked := filepath.Join(directory, "blocked.jsonl")
	original := []byte("do-not-overwrite\n")
	if err := os.WriteFile(blocked, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := source.ExtractBranch(context.Background(), d.ID(), ExtractOptions{TargetPath: blocked, ID: "blocked", WorkingDir: directory}); !errors.Is(err, ErrStorage) {
		t.Fatalf("existing target error = %v, want storage failure", err)
	}
	if got, err := os.ReadFile(blocked); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("existing target changed = %q, %v", got, err)
	}
	if _, err := source.ExtractBranch(context.Background(), d.ID(), ExtractOptions{TargetPath: sourcePath, ID: "same", WorkingDir: directory}); !errors.Is(err, ErrSourceEqualsTarget) {
		t.Fatalf("same target error = %v", err)
	}
}

func TestForkFromCopiesForestAndCancellationCannotCreateTarget(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "fork-source.jsonl")
	source, err := Create(sourcePath, CreateOptions{ID: "source", WorkingDir: directory, NewEntryID: sequenceIDs("a", "b", "c")})
	if err != nil {
		t.Fatal(err)
	}
	a := appendUser(t, source, "a")
	b := appendUser(t, source, "b")
	if err := source.SelectLeaf(a.ID()); err != nil {
		t.Fatal(err)
	}
	c := appendUser(t, source, "branch")
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(directory, "fork.jsonl")
	fork, err := ForkFrom(context.Background(), sourcePath, ExtractOptions{TargetPath: targetPath, ID: "fork", WorkingDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()
	if got, want := entryIDs(fork.Entries()), []string{a.ID(), b.ID(), c.ID()}; !equalIDs(got, want) {
		t.Fatalf("fork entries = %v, want complete forest %v", got, want)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceledPath := filepath.Join(directory, "canceled.jsonl")
	if _, err := ForkFrom(ctx, sourcePath, ExtractOptions{TargetPath: canceledPath, ID: "canceled", WorkingDir: directory}); !errors.Is(err, ErrAppendCanceled) {
		t.Fatalf("canceled fork error = %v", err)
	}
	if _, err := os.Stat(canceledPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled fork created target: %v", err)
	}
}

func TestConcurrentSelectAndAppendLeaveValidTree(t *testing.T) {
	directory := t.TempDir()
	session, err := Create(filepath.Join(directory, "race-tree.jsonl"), CreateOptions{ID: "race-tree", WorkingDir: directory, NewEntryID: func() (string, error) { return NewSessionID(time.Now()) }})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	root := appendUser(t, session, "root")
	const workers = 24
	var group sync.WaitGroup
	errs := make(chan error, workers*2)
	for index := 0; index < workers; index++ {
		group.Add(2)
		go func() { defer group.Done(); errs <- session.SelectLeaf(root.ID()) }()
		go func(index int) {
			defer group.Done()
			_, err := session.Append(context.Background(), mustUserMessage(t, "message", time.UnixMilli(int64(index+1))), AppendOptions{})
			errs <- err
		}(index)
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := session.PathTo(root.ID()); err != nil {
		t.Fatal(err)
	}
	if len(session.Tree()) != 1 {
		t.Fatal("concurrent selection created invalid forest")
	}
}

func appendUser(t *testing.T, session *Session, text string) Entry {
	t.Helper()
	entry, err := session.Append(context.Background(), mustUserMessage(t, text, time.Now()), AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func entryIDs(entries []Entry) []string {
	ids := make([]string, len(entries))
	for index, entry := range entries {
		ids[index] = entry.ID()
	}
	return ids
}

func nodeIDs(nodes []TreeNode) []string {
	ids := make([]string, len(nodes))
	for index, node := range nodes {
		ids[index] = node.Entry.ID()
	}
	return ids
}

func contextTexts(t *testing.T, context Context) []string {
	t.Helper()
	messages := context.Messages()
	texts := make([]string, len(messages))
	for index, message := range messages {
		user, ok := message.(llm.UserTextMessage)
		if ok {
			texts[index] = user.Content()[0].Text()
			continue
		}
		// The v0.2 tree tests create only UserTextMessage. Keep this failure
		// explicit if a fixture changes instead of silently accepting context.
		t.Fatalf("context message %d = %T, want user text", index, message)
	}
	return texts
}

func equalIDs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

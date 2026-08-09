package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

type recordingSummarizer struct {
	mu      sync.Mutex
	inputs  []SummaryInput
	output  SummaryOutput
	err     error
	entered chan struct{}
	release chan struct{}
}

type summarizerFunc func(context.Context, SummaryInput) (SummaryOutput, error)

func (fn summarizerFunc) Summarize(ctx context.Context, input SummaryInput) (SummaryOutput, error) {
	return fn(ctx, input)
}

func (s *recordingSummarizer) Summarize(ctx context.Context, input SummaryInput) (SummaryOutput, error) {
	s.mu.Lock()
	s.inputs = append(s.inputs, input)
	s.mu.Unlock()
	if s.entered != nil {
		close(s.entered)
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return SummaryOutput{}, context.Cause(ctx)
		}
	}
	if s.err != nil {
		return SummaryOutput{}, s.err
	}
	return s.output, nil
}

func (s *recordingSummarizer) input(t *testing.T) SummaryInput {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.inputs) != 1 {
		t.Fatalf("summarizer calls = %d, want 1", len(s.inputs))
	}
	return s.inputs[0]
}

func TestCompactPersistsV3CheckpointAndBuildsSelectedContext(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	clock := sequenceClock(time.Date(2026, time.August, 2, 1, 2, 3, 0, time.UTC))
	session, err := Create(filepath.Join(directory, "compact.jsonl"), CreateOptions{
		ID: "compact", WorkingDir: directory, Now: clock, NewEntryID: sequenceIDs("one", "two", "three", "compact-entry"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, text := range []string{"old one", "old two", "recent"} {
		if _, err := session.Append(context.Background(), mustUserMessage(t, text, clock()), AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := llm.NewUsage(llm.UsageSpec{Input: 7, Output: 3})
	if err != nil {
		t.Fatal(err)
	}
	summarizer := &recordingSummarizer{output: SummaryOutput{Text: "checkpoint", Usage: &CompactionUsage{Usage: usage, Cost: ZeroUsageCost()}}}
	result, err := session.Compact(context.Background(), CompactRequest{KeepRecentTokens: 1, Instructions: "focus on code", Summarizer: summarizer})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Committed || result.Entry.Type() != "compaction" || result.Entry.ID() != "compact-entry" {
		t.Fatalf("result = %#v", result)
	}
	parent, hasParent := result.Entry.ParentID()
	if !hasParent || parent != "three" {
		t.Fatalf("compaction parent = (%q, %t), want three", parent, hasParent)
	}
	record, ok := result.Entry.Compaction()
	if !ok || record.FirstKeptEntryID != "three" || record.TokensBefore == 0 || record.Usage == nil || record.Usage.Usage.TotalTokens() != 10 {
		t.Fatalf("compaction record = %#v, ok=%t", record, ok)
	}
	input := summarizer.input(t)
	if got := messageTexts(input.Messages); fmt.Sprint(got) != fmt.Sprint([]string{"old one", "old two"}) {
		t.Fatalf("summary messages = %v", got)
	}
	if got := messageTexts(input.RetainedTail); fmt.Sprint(got) != fmt.Sprint([]string{"recent"}) {
		t.Fatalf("retained messages = %v", got)
	}
	if input.FirstKeptEntryID != "three" || input.Instructions != "focus on code" || !bytes.Contains([]byte(input.Prompt), []byte("<conversation>")) {
		t.Fatalf("summary input = %#v", input)
	}
	contextMessages := session.BuildContext().Messages()
	if got := messageTexts(contextMessages); fmt.Sprint(got) != fmt.Sprint([]string{CompactionSummaryPrefix + "checkpoint" + CompactionSummarySuffix, "recent"}) {
		t.Fatalf("context messages = %q", got)
	}

	data, err := os.ReadFile(session.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(lines[len(lines)-1], &wire); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"type", "id", "parentId", "timestamp", "summary", "firstKeptEntryId", "tokensBefore", "usage"} {
		if _, exists := wire[field]; !exists {
			t.Fatalf("compaction wire lacks %q: %s", field, lines[len(lines)-1])
		}
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(filepath.Join(directory, "compact.jsonl"), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := messageTexts(reopened.BuildContext().Messages()); fmt.Sprint(got) != fmt.Sprint([]string{CompactionSummaryPrefix + "checkpoint" + CompactionSummarySuffix, "recent"}) {
		t.Fatalf("reopened context = %q", got)
	}
}

func TestCompactExplicitZeroKeepRecentTokensDoesNotUseLegacyDefault(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	clock := sequenceClock(time.Date(2026, time.August, 6, 1, 2, 3, 0, time.UTC))
	session, err := Create(filepath.Join(directory, "explicit-zero.jsonl"), CreateOptions{
		ID: "explicit-zero", WorkingDir: directory, Now: clock, NewEntryID: sequenceIDs("one", "two", "compact"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, text := range []string{"summarized", "retained"} {
		if _, err := session.Append(context.Background(), mustUserMessage(t, text, clock()), AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	summarizer := &recordingSummarizer{output: SummaryOutput{Text: "explicit zero"}}
	result, err := session.Compact(context.Background(), CompactRequest{
		KeepRecentTokens: 0, KeepRecentTokensSet: true, Summarizer: summarizer,
	})
	if err != nil || !result.Committed {
		t.Fatalf("Compact() = %#v, %v", result, err)
	}
	input := summarizer.input(t)
	if got := messageTexts(input.Messages); fmt.Sprint(got) != fmt.Sprint([]string{"summarized"}) {
		t.Fatalf("summary messages = %v", got)
	}
	if got := messageTexts(input.RetainedTail); fmt.Sprint(got) != fmt.Sprint([]string{"retained"}) {
		t.Fatalf("retained messages = %v", got)
	}
}

func TestCompactSnapshotDoesNotBlockAppendAndRejectsChangedBranch(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	session, err := Create(filepath.Join(directory, "race.jsonl"), CreateOptions{ID: "race", WorkingDir: directory, NewEntryID: sequenceIDs("one", "two", "three", "compact")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for index, text := range []string{"old", "recent"} {
		if _, err := session.Append(context.Background(), mustUserMessage(t, text, time.UnixMilli(int64(index+1))), AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	summarizer := &recordingSummarizer{output: SummaryOutput{Text: "summary"}, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := session.Compact(context.Background(), CompactRequest{KeepRecentTokens: 1, Summarizer: summarizer})
		done <- err
	}()
	<-summarizer.entered
	if _, err := session.Append(context.Background(), mustUserMessage(t, "new branch state", time.UnixMilli(2)), AppendOptions{}); err != nil {
		t.Fatalf("append while summarizer blocked = %v", err)
	}
	close(summarizer.release)
	if err := <-done; !errors.Is(err, ErrCompactionConflict) {
		t.Fatalf("Compact() = %v, want ErrCompactionConflict", err)
	}
	if got := messageTexts(session.Context().Messages()); fmt.Sprint(got) != fmt.Sprint([]string{"old", "recent", "new branch state"}) {
		t.Fatalf("conflict overwrote context: %q", got)
	}
}

func TestCompactProviderFailureCancellationAndRepeatNeverAppend(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	session, err := Create(filepath.Join(directory, "failure.jsonl"), CreateOptions{ID: "failure", WorkingDir: directory, NewEntryID: sequenceIDs("one", "two", "compact")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, text := range []string{"old", "new"} {
		if _, err := session.Append(context.Background(), mustUserMessage(t, text, time.Now()), AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	before := len(session.Entries())
	_, err = session.Compact(context.Background(), CompactRequest{KeepRecentTokens: 1, Summarizer: &recordingSummarizer{err: errors.New("provider down")}})
	if !errors.Is(err, ErrSummaryFailed) || len(session.Entries()) != before {
		t.Fatalf("provider failure = %v entries=%d", err, len(session.Entries()))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = session.Compact(ctx, CompactRequest{KeepRecentTokens: 1, Summarizer: &recordingSummarizer{output: SummaryOutput{Text: "nope"}}})
	if !errors.Is(err, ErrAppendCanceled) || len(session.Entries()) != before {
		t.Fatalf("canceled compact = %v entries=%d", err, len(session.Entries()))
	}
	_, err = session.Compact(context.Background(), CompactRequest{KeepRecentTokens: 1, Summarizer: &recordingSummarizer{output: SummaryOutput{Text: "ok"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.Compact(context.Background(), CompactRequest{KeepRecentTokens: 1, Summarizer: &recordingSummarizer{output: SummaryOutput{Text: "duplicate"}}})
	if !errors.Is(err, ErrAlreadyCompacted) || len(session.Entries()) != before+1 {
		t.Fatalf("repeat compact = %v entries=%d", err, len(session.Entries()))
	}
}

func TestCompactRejectsSelectionChangeAndConcurrentDuplicate(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	session, err := Create(filepath.Join(directory, "select.jsonl"), CreateOptions{ID: "select", WorkingDir: directory, NewEntryID: sequenceIDs("root", "left", "right", "compact")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	root, err := session.Append(context.Background(), mustUserMessage(t, "root", time.UnixMilli(1)), AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	left, err := session.Append(context.Background(), mustUserMessage(t, "left", time.UnixMilli(2)), AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.SelectLeaf(root.ID()); err != nil {
		t.Fatal(err)
	}
	right, err := session.Append(context.Background(), mustUserMessage(t, "right", time.UnixMilli(3)), AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.SelectLeaf(left.ID()); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := session.Compact(context.Background(), CompactRequest{KeepRecentTokens: 1, Summarizer: &recordingSummarizer{output: SummaryOutput{Text: "left"}, entered: entered, release: release}})
		done <- err
	}()
	<-entered
	if err := session.SelectLeaf(right.ID()); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrCompactionConflict) {
		t.Fatalf("selection conflict = %v", err)
	}

	// Two snapshots of the same selected branch may both summarize, but only
	// one is allowed to cross the durable append boundary.
	entered = make(chan struct{}, 2)
	release = make(chan struct{})
	shared := summarizerFunc(func(ctx context.Context, input SummaryInput) (SummaryOutput, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return SummaryOutput{Text: "right"}, nil
		case <-ctx.Done():
			return SummaryOutput{}, context.Cause(ctx)
		}
	})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := session.Compact(context.Background(), CompactRequest{KeepRecentTokens: 1, Summarizer: shared})
			results <- err
		}()
	}
	<-entered
	<-entered
	close(release)
	first, second := <-results, <-results
	if !(first == nil && errors.Is(second, ErrCompactionConflict) || second == nil && errors.Is(first, ErrCompactionConflict)) {
		t.Fatalf("concurrent compact results = %v, %v", first, second)
	}
}

func TestCompactionSelectedBranchDoesNotLeakSiblingAndForkRoundTrips(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	session, err := Create(filepath.Join(directory, "branch.jsonl"), CreateOptions{ID: "branch", WorkingDir: directory, NewEntryID: sequenceIDs("root", "left", "right", "compact")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	root, err := session.Append(context.Background(), mustUserMessage(t, "root", time.UnixMilli(1)), AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(context.Background(), mustUserMessage(t, "left-secret", time.UnixMilli(2)), AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := session.SelectLeaf(root.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(context.Background(), mustUserMessage(t, "right-work", time.UnixMilli(3)), AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = session.Compact(context.Background(), CompactRequest{KeepRecentTokens: 1, Summarizer: &recordingSummarizer{output: SummaryOutput{Text: "right summary"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := messageTexts(session.Context().Messages()); fmt.Sprint(got) != fmt.Sprint([]string{CompactionSummaryPrefix + "right summary" + CompactionSummarySuffix, "right-work"}) {
		t.Fatalf("selected branch context = %q", got)
	}
	target := filepath.Join(directory, "fork.jsonl")
	fork, err := session.Fork(context.Background(), ExtractOptions{TargetPath: target, ID: "fork", WorkingDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()
	if got := messageTexts(fork.Context().Messages()); fmt.Sprint(got) != fmt.Sprint([]string{CompactionSummaryPrefix + "right summary" + CompactionSummarySuffix, "right-work"}) {
		t.Fatalf("fork context = %q", got)
	}
}

func TestRepeatedCompactionUsesLatestCheckpointAndNewTail(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	session, err := Create(filepath.Join(directory, "repeated.jsonl"), CreateOptions{ID: "repeated", WorkingDir: directory, NewEntryID: sequenceIDs("one", "two", "first", "three", "second")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for index, text := range []string{"old", "kept"} {
		if _, err := session.Append(context.Background(), mustUserMessage(t, text, time.UnixMilli(int64(index+1))), AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := session.Compact(context.Background(), CompactRequest{KeepRecentTokens: 1, Summarizer: &recordingSummarizer{output: SummaryOutput{Text: "first summary"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(context.Background(), mustUserMessage(t, "new tail", time.UnixMilli(3)), AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	second := &recordingSummarizer{output: SummaryOutput{Text: "second summary"}}
	if _, err := session.Compact(context.Background(), CompactRequest{KeepRecentTokens: 1, Summarizer: second}); err != nil {
		t.Fatal(err)
	}
	input := second.input(t)
	gotMessages := messageTexts(input.Messages)
	if input.PreviousSummary != "first summary" || fmt.Sprint(gotMessages) != fmt.Sprint([]string{"kept"}) {
		t.Fatalf("second input previous=%q messages=%q", input.PreviousSummary, gotMessages)
	}
	if got := messageTexts(session.BuildContext().Messages()); fmt.Sprint(got) != fmt.Sprint([]string{CompactionSummaryPrefix + "second summary" + CompactionSummarySuffix, "new tail"}) {
		t.Fatalf("repeated context = %q", got)
	}
}

func TestOpenReportsCompactionOutsideParentPathWithoutRewriting(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "invalid-compaction.jsonl")
	data := []byte(`{"type":"session","version":3,"id":"bad","timestamp":"2026-08-02T00:00:00Z","cwd":"/tmp"}
{"type":"message","id":"root","parentId":null,"timestamp":"2026-08-02T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"root"}],"timestamp":1}}
{"type":"message","id":"sibling","parentId":"root","timestamp":"2026-08-02T00:00:02Z","message":{"role":"user","content":[{"type":"text","text":"sibling"}],"timestamp":2}}
{"type":"message","id":"other","parentId":"root","timestamp":"2026-08-02T00:00:03Z","message":{"role":"user","content":[{"type":"text","text":"other"}],"timestamp":3}}
{"type":"compaction","id":"badcompact","parentId":"other","timestamp":"2026-08-02T00:00:04Z","summary":"x","firstKeptEntryId":"sibling","tokensBefore":1}
{"type":"message","id":"tail","parentId":"badcompact","timestamp":"2026-08-02T00:00:05Z","message":{"role":"user","content":[{"type":"text","text":"tail survives"}],"timestamp":5}}
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatalf("Open invalid compaction = %v", err)
	}
	defer session.Close()
	diagnostics := session.LoadDiagnostics()
	if len(diagnostics) != 1 || diagnostics[0].Code != LoadDiagnosticCompaction || diagnostics[0].EntryID != "badcompact" {
		t.Fatalf("invalid compaction diagnostics = %#v", diagnostics)
	}
	projection, err := session.projectContextAt("tail")
	if err != nil {
		t.Fatal(err)
	}
	if got := entryIDs(projection.Entries); !slices.Equal(got, []string{"badcompact", "tail"}) {
		t.Fatalf("invalid compaction entries = %v", got)
	}
	if got := messageTexts(projection.Context.Messages()); !slices.Equal(got, []string{CompactionSummaryPrefix + "x" + CompactionSummarySuffix, "tail survives"}) {
		t.Fatalf("invalid compaction context = %v", got)
	}
}

func TestCreateBranchedSessionInvalidCompactionLeavesNoPublishedCandidate(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "source.jsonl")
	data := []byte(`{"type":"session","version":3,"id":"bad-branch","timestamp":"2026-08-02T00:00:00Z","cwd":"` + directory + `"}
{"type":"message","id":"root","parentId":null,"timestamp":"2026-08-02T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"root"}],"timestamp":1}}
{"type":"message","id":"sibling","parentId":"root","timestamp":"2026-08-02T00:00:02Z","message":{"role":"user","content":[{"type":"text","text":"sibling"}],"timestamp":2}}
{"type":"message","id":"other","parentId":"root","timestamp":"2026-08-02T00:00:03Z","message":{"role":"assistant","content":[{"type":"text","text":"other"}],"api":"scripted","provider":"scripted","model":"scripted-1","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":3}}
{"type":"compaction","id":"badcompact","parentId":"other","timestamp":"2026-08-02T00:00:04Z","summary":"x","firstKeptEntryId":"sibling","tokensBefore":1}
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := OpenSessionManager(path, directory, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	before, err := filepath.Glob(filepath.Join(directory, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.CreateBranchedSession(context.Background(), "badcompact"); err == nil {
		t.Fatal("CreateBranchedSession accepted an invalid compaction candidate")
	}
	after, err := filepath.Glob(filepath.Join(directory, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before, after) {
		t.Fatalf("failed branch published residual file: before=%v after=%v", before, after)
	}
	if got, _ := manager.LeafID(); got != "badcompact" {
		t.Fatalf("failed branch moved source leaf to %q", got)
	}
}

func TestForeignCompactionAndUnknownSuccessorRoundTripThroughFork(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	source := filepath.Join(directory, "foreign.jsonl")
	compactionRaw := `{"type":"compaction","id":"compact","parentId":"kept","timestamp":"2026-08-02T00:00:03Z","summary":"foreign summary","firstKeptEntryId":"root","tokensBefore":4,"details":{"future":[1,true]}}`
	data := []byte(`{"type":"session","version":3,"id":"foreign","timestamp":"2026-08-02T00:00:00Z","cwd":"/tmp"}` + "\n" +
		`{"type":"message","id":"root","parentId":null,"timestamp":"2026-08-02T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"root"}],"timestamp":1}}` + "\n" +
		`{"type":"message","id":"kept","parentId":"root","timestamp":"2026-08-02T00:00:02Z","message":{"role":"user","content":[{"type":"text","text":"kept"}],"timestamp":2}}` + "\n" +
		compactionRaw + "\n" +
		`{"type":"future_entry","id":"future","parentId":"compact","timestamp":"2026-08-02T00:00:04Z","payload":{"opaque":true}}` + "\n")
	if err := os.WriteFile(source, data, 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(source, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if got := messageTexts(session.BuildContext().Messages()); fmt.Sprint(got) != fmt.Sprint([]string{CompactionSummaryPrefix + "foreign summary" + CompactionSummarySuffix, "root", "kept"}) {
		t.Fatalf("foreign context = %q", got)
	}
	target := filepath.Join(directory, "foreign-fork.jsonl")
	fork, err := session.Fork(context.Background(), ExtractOptions{TargetPath: target, ID: "foreign-fork", WorkingDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()
	entries := fork.Entries()
	if len(entries) != 4 || !bytes.Equal(entries[2].RawJSON(), []byte(compactionRaw)) || entries[3].Type() != "future_entry" {
		t.Fatalf("fork did not preserve foreign compaction/unknown entries: %#v", entries)
	}
	extract, err := session.ExtractBranch(context.Background(), "future", ExtractOptions{TargetPath: filepath.Join(directory, "foreign-extract.jsonl"), ID: "foreign-extract", WorkingDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer extract.Close()
	extracted := extract.Entries()
	if len(extracted) != 4 || !bytes.Equal(extracted[2].RawJSON(), []byte(compactionRaw)) || extracted[3].Type() != "future_entry" {
		t.Fatalf("extract did not preserve foreign compaction/unknown entries: %#v", extracted)
	}
}

func TestCompactStorageFaultPreservesOrPoisonsByCommitBoundary(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		started bool
		want    error
		poison  bool
	}{
		{name: "before write", want: ErrStorage},
		{name: "after write", started: true, want: ErrCommitUnknown, poison: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			storage := &fakeStorage{createCreated: true, appendStarted: true}
			calls := 0
			storage.appendFunc = func(context.Context, []byte) (bool, error) {
				calls++
				if calls == 3 {
					return tt.started, errors.New("compaction storage fault")
				}
				return true, nil
			}
			session := newFakeSession(t, storage, sequenceIDs("one", "two", "compact"))
			for index, text := range []string{"old", "recent"} {
				if _, err := session.Append(context.Background(), mustUserMessage(t, text, time.UnixMilli(int64(index+1))), AppendOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			before := len(session.Entries())
			_, err := session.Compact(context.Background(), CompactRequest{KeepRecentTokens: 1, Summarizer: &recordingSummarizer{output: SummaryOutput{Text: "summary"}}})
			if !errors.Is(err, tt.want) || session.Poisoned() != tt.poison || len(session.Entries()) != before {
				t.Fatalf("Compact() = %v poisoned=%t entries=%d", err, session.Poisoned(), len(session.Entries()))
			}
		})
	}
}

func FuzzCompactionHelpersNeverPanic(f *testing.F) {
	f.Add("user", "assistant", "tool output")
	f.Fuzz(func(t *testing.T, user, assistant, toolOutput string) {
		userMessage, err := llm.NewUserTextMessage(user, time.UnixMilli(1))
		if err != nil {
			t.Skip()
		}
		block, err := llm.NewTextBlock(assistant)
		if err != nil {
			t.Skip()
		}
		usage, err := llm.NewUsage(llm.UsageSpec{Input: 1, Output: 1})
		if err != nil {
			t.Fatal(err)
		}
		assistantMessage, err := newAssistantTextMessage([]llm.TextBlock{block}, llm.FinishStop, usage, time.UnixMilli(2))
		if err != nil {
			t.Fatal(err)
		}
		toolBlock, err := llm.NewTextBlock(toolOutput)
		if err != nil {
			t.Skip()
		}
		toolMessage, err := llm.NewToolResultMessage("call", "tool", []llm.TextBlock{toolBlock}, false, time.UnixMilli(3))
		if err != nil {
			t.Fatal(err)
		}
		messages := []llm.ConversationMessage{userMessage, assistantMessage, toolMessage}
		_, _ = EstimateContextTokens(messages)
		_ = SerializeConversation(messages)
	})
}

func TestEstimateContextTokensUsesToolUsageAndTrailingSuffix(t *testing.T) {
	t.Parallel()
	usage, err := llm.NewUsage(llm.UsageSpec{Input: 40, Output: 10})
	if err != nil {
		t.Fatal(err)
	}
	call := mustToolCall(t, "call", "read", []byte(`{"path":"a"}`))
	assistant, err := newAssistantToolUseMessage([]llm.AssistantBlock{call}, usage, time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	tail := mustUserMessage(t, "12345", time.UnixMilli(2))
	estimate, err := EstimateContextTokens([]llm.ConversationMessage{assistant, tail})
	if err != nil {
		t.Fatal(err)
	}
	if estimate.LastUsageIndex != 0 || estimate.UsageTokens != 50 || estimate.TrailingTokens != 2 || estimate.Tokens != 52 {
		t.Fatalf("estimate = %#v", estimate)
	}
	compact, err := ShouldCompact([]llm.ConversationMessage{assistant, tail}, 53, 2)
	if err != nil || !compact {
		t.Fatalf("ShouldCompact() = (%t, %v), want true", compact, err)
	}
}

func TestSerializeConversationAndEstimateRichMessagesOnce(t *testing.T) {
	t.Parallel()
	image, err := llm.NewImageDataBlock("image/png", []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	text := mustTextBlock(t, "hello")
	user, err := llm.NewUserContentMessage([]llm.UserContentBlock{text, image}, time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	thinking, err := llm.NewThinkingBlock("reasoning")
	if err != nil {
		t.Fatal(err)
	}
	usage, err := llm.NewUsage(llm.UsageSpec{Input: 1, Output: 1})
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := newAssistantRichMessage([]llm.AssistantBlock{thinking, mustTextBlock(t, "answer")}, llm.FinishStop, usage, time.UnixMilli(2))
	if err != nil {
		t.Fatal(err)
	}
	tool, err := llm.NewToolResultContentMessage("id", "tool", []llm.ToolResultContentBlock{mustTextBlock(t, "output"), image}, false, time.UnixMilli(3))
	if err != nil {
		t.Fatal(err)
	}
	serialized := SerializeConversation([]llm.ConversationMessage{user, assistant, tool})
	for _, want := range []string{"[User]: hello", "[Assistant thinking]: reasoning", "[Assistant]: answer", "[Tool result]: output"} {
		if !bytes.Contains([]byte(serialized), []byte(want)) {
			t.Fatalf("SerializeConversation() = %q, missing %q", serialized, want)
		}
	}
	// Images use the production 4,800-char estimate; tool-result ID and name do
	// not contribute to pi's content-only estimate.
	if tokens, err := estimateMessageTokens(tool); err != nil || tokens != 1202 {
		t.Fatalf("rich tool estimate = (%d, %v), want 1202", tokens, err)
	}
}

func TestTokenEstimateOverflowFailsPolicyCutAndCompactWithoutWrite(t *testing.T) {
	t.Parallel()
	maxUsage, err := llm.NewUsage(llm.UsageSpec{Input: math.MaxUint64})
	if err != nil {
		t.Fatal(err)
	}
	assistantBlock := mustTextBlock(t, "assistant")
	assistant, err := newAssistantTextMessage([]llm.TextBlock{assistantBlock}, llm.FinishStop, maxUsage, time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	tail := mustUserMessage(t, "x", time.UnixMilli(2))
	messages := []llm.ConversationMessage{assistant, tail}
	if _, err := EstimateContextTokens(messages); !errors.Is(err, ErrTokenEstimateOverflow) {
		t.Fatalf("EstimateContextTokens() = %v, want ErrTokenEstimateOverflow", err)
	}
	if compact, err := ShouldCompact(messages, math.MaxUint64, 1); compact || !errors.Is(err, ErrTokenEstimateOverflow) {
		t.Fatalf("ShouldCompact() = (%t, %v), want false ErrTokenEstimateOverflow", compact, err)
	}

	if _, err := checkedAddTokens(math.MaxUint64-1, 2); !errors.Is(err, ErrTokenEstimateOverflow) {
		t.Fatalf("checkedAddTokens() = %v, want ErrTokenEstimateOverflow", err)
	}

	directory := t.TempDir()
	path := filepath.Join(directory, "overflow.jsonl")
	session, err := Create(path, CreateOptions{ID: "overflow", WorkingDir: directory, NewEntryID: sequenceIDs("assistant", "tail", "compact")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Append(context.Background(), assistant, AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Append(context.Background(), tail, AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	summarizer := &recordingSummarizer{output: SummaryOutput{Text: "must not run"}}
	if _, err := session.Compact(context.Background(), CompactRequest{KeepRecentTokens: 1, Summarizer: summarizer}); !errors.Is(err, ErrTokenEstimateOverflow) {
		t.Fatalf("Compact() = %v, want ErrTokenEstimateOverflow", err)
	}
	summarizer.mu.Lock()
	calls := len(summarizer.inputs)
	summarizer.mu.Unlock()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || !bytes.Equal(before, after) || len(session.Entries()) != 2 || session.Poisoned() {
		t.Fatalf("overflow side effects: calls=%d bytesChanged=%t entries=%d poisoned=%t", calls, !bytes.Equal(before, after), len(session.Entries()), session.Poisoned())
	}
}

func FuzzCheckedTokenAdditionNeverWraps(f *testing.F) {
	f.Add(uint64(0), uint64(0))
	f.Add(uint64(math.MaxUint64), uint64(1))
	f.Add(uint64(math.MaxUint64-1), uint64(1))
	f.Fuzz(func(t *testing.T, left, right uint64) {
		sum, err := checkedAddTokens(left, right)
		if left > math.MaxUint64-right {
			if !errors.Is(err, ErrTokenEstimateOverflow) || sum != 0 {
				t.Fatalf("overflow add (%d,%d) = (%d,%v)", left, right, sum, err)
			}
			return
		}
		if err != nil || sum < left || sum < right || sum != left+right {
			t.Fatalf("checked add (%d,%d) = (%d,%v)", left, right, sum, err)
		}
	})
}

func messageTexts(messages []llm.ConversationMessage) []string {
	result := make([]string, 0, len(messages))
	for _, message := range messages {
		switch message := message.(type) {
		case llm.UserTextMessage:
			result = append(result, joinTextBlocks(message.Content()))
		case llm.UserContentMessage:
			var blocks []llm.TextBlock
			for _, content := range message.Content() {
				if text, ok := content.(llm.TextBlock); ok {
					blocks = append(blocks, text)
				}
			}
			result = append(result, joinTextBlocks(blocks))
		case llm.AssistantTextMessage:
			result = append(result, joinTextBlocks(message.Content()))
		default:
			result = append(result, message.Role().String())
		}
	}
	return result
}

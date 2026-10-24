package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

func appendReadRun(t *testing.T, manager *SessionManager, label string, tools int) []Entry {
	t.Helper()
	entries := []Entry{}
	appendMessage := func(message llm.ConversationMessage, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		entry, err := manager.AppendLLMMessage(context.Background(), message)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	appendMessage(llm.NewUserTextMessage(label, time.UnixMilli(1000)))
	for i := 0; i < tools; i++ {
		call, _ := llm.NewToolCallBlock(fmt.Sprintf("%s-%d", label, i), "read", []byte(`{"path":"fixture"}`))
		comment, _ := llm.NewTextBlock("intermediate commentary")
		appendMessage(newAssistantToolUseMessage([]llm.AssistantBlock{comment, call}, llm.Usage{}, time.UnixMilli(int64(2000+i*2))))
		text, _ := llm.NewTextBlock(fmt.Sprintf("result %d", i))
		appendMessage(llm.NewToolResultMessage(call.ID(), "read", []llm.TextBlock{text}, false, time.UnixMilli(int64(2001+i*2))))
	}
	thinking, _ := llm.NewThinkingBlock("complete process reasoning")
	answer, _ := llm.NewTextBlock("final answer for " + label)
	appendMessage(newAssistantRichMessage([]llm.AssistantBlock{thinking, answer}, llm.FinishStop, llm.Usage{}, time.UnixMilli(int64(3000+tools*2))))
	return entries
}

func TestRunOverviewLocatesCompleteResultWithoutReadingItsProcess(t *testing.T) {
	m, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	first := appendReadRun(t, m, "first", 0)
	long := appendReadRun(t, m, "long", 320)
	last := appendReadRun(t, m, "last", 0)
	for _, target := range []string{long[0].ID(), long[301].ID(), long[len(long)-1].ID()} {
		page, err := m.ReadRuns(RunsQuery{AroundID: target, Limit: 1})
		if err != nil || page.Total != 3 || page.Start != 1 || page.End != 2 || len(page.Items) != 1 {
			t.Fatalf("run locate = %#v, %v", page, err)
		}
		item := page.Items[0]
		if item.Run.ID != long[0].ID() || item.Header.ID() != long[0].ID() || item.Result.ID() != long[len(long)-1].ID() || item.Run.ProcessCount != 641 || !item.Run.ResultHasProcess {
			t.Fatalf("partial page guessed a result: %#v", item.Run)
		}
		if page.Older == nil || page.Newer == nil {
			t.Fatal("overview lost neighboring runs")
		}
	}
	index, err := m.ReadRuns(RunsQuery{Direction: PageNewer, MetadataOnly: true})
	if err != nil || len(index.Items) != 3 {
		t.Fatalf("index = %#v, %v", index, err)
	}
	for i, expected := range []string{first[0].ID(), long[0].ID(), last[0].ID()} {
		item := index.Items[i]
		if item.Run.ID != expected || item.Header != nil || item.Result != nil {
			t.Fatal("navigation index loaded message bodies")
		}
	}
}

func TestRunDetailsPageBothWaysWithStableHeaderAndBoundedScope(t *testing.T) {
	m, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	appendReadRun(t, m, "before", 0)
	entries := appendReadRun(t, m, "long", 320)
	after := appendReadRun(t, m, "after", 0)
	page, err := m.ReadHistory(HistoryQuery{RunID: entries[0].ID(), Direction: PageNewer})
	if err != nil || len(page.Items) != 128 || page.Start != 0 || page.Older != nil {
		t.Fatalf("first detail page = %#v, %v", page, err)
	}
	var ids []string
	for {
		if page.Run == nil || page.Run.ID != entries[0].ID() || page.Run.Result.ID != entries[len(entries)-1].ID() || page.Total != len(entries) {
			t.Fatal("detail page lost its complete group context")
		}
		ids = append(ids, pageEntryIDs(page.Items)...)
		if page.Newer == nil {
			break
		}
		if _, err := m.ReadHistory(HistoryQuery{RunID: after[0].ID(), Cursor: page.Newer}); !errors.Is(err, ErrInvalidReadQuery) {
			t.Fatal("cursor crossed run", err)
		}
		page, err = m.ReadHistory(HistoryQuery{Cursor: page.Newer})
		if err != nil {
			t.Fatal(err)
		}
	}
	want := make([]string, len(entries))
	for i := range entries {
		want[i] = entries[i].ID()
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatal("run paging omitted, repeated, or crossed messages")
	}
	if page.End != len(entries) || page.Newer != nil {
		t.Fatal("run details continued into a different run")
	}
	if _, err := m.ReadHistory(HistoryQuery{RunID: entries[0].ID(), AroundID: after[0].ID()}); !errors.Is(err, ErrEntryNotFound) {
		t.Fatal("foreign node entered run", err)
	}
	back, err := m.ReadHistory(HistoryQuery{Cursor: page.Older})
	if err != nil || back.End != page.Start || back.Run.ID != page.Run.ID {
		t.Fatalf("reverse page = %#v, %v", back, err)
	}
	// Branch selection cannot alter a cursor's original branch or final result.
	if err := m.Branch(entries[0].ID()); err != nil {
		t.Fatal(err)
	}
	appendReadRun(t, m, "another branch", 0)
	backAgain, err := m.ReadHistory(HistoryQuery{Cursor: page.Older})
	if err != nil || !reflect.DeepEqual(pageEntryIDs(back.Items), pageEntryIDs(backAgain.Items)) || backAgain.Run.Result.ID != entries[len(entries)-1].ID() {
		t.Fatal("branch switch rewrote existing detail pages", err)
	}
}

func TestRunTailUpdatesReplaceOnlyChangedOverviewAndNeverPromoteToolCommentary(t *testing.T) {
	m, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	user, _ := m.AppendLLMMessage(context.Background(), mustUserMessage(t, "question", time.UnixMilli(1)))
	call, _ := llm.NewToolCallBlock("call", "read", []byte(`{}`))
	comment, _ := llm.NewTextBlock("I will inspect this")
	assistant, _ := newAssistantToolUseMessage([]llm.AssistantBlock{comment, call}, llm.Usage{}, time.UnixMilli(2))
	tool, _ := m.AppendLLMMessage(context.Background(), assistant)
	before, err := m.ReadRuns(RunsQuery{})
	if err != nil || before.Items[0].Run.Result != nil || before.Items[0].Result != nil {
		t.Fatal("tool commentary became a final result")
	}
	result, _ := llm.NewToolResultMessage("call", "read", []llm.TextBlock{comment}, false, time.UnixMilli(3))
	if _, err := m.AppendLLMMessage(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	answer, _ := newAssistantTextMessage([]llm.TextBlock{comment}, llm.FinishStop, llm.Usage{}, time.UnixMilli(4))
	final, _ := m.AppendLLMMessage(context.Background(), answer)
	tail, err := m.ReadRuns(RunsQuery{AfterID: tool.ID()})
	if err != nil || !tail.Connected || tail.Start != 0 || len(tail.Items) != 1 || tail.Items[0].Run.ID != user.ID() || tail.Items[0].Result.ID() != final.ID() {
		t.Fatalf("changed run = %#v, %v", tail, err)
	}
	unchanged, err := m.ReadRuns(RunsQuery{AfterID: final.ID()})
	if err != nil || !unchanged.Connected || len(unchanged.Items) != 0 {
		t.Fatal("unchanged run was downloaded again", err)
	}
	oldLeaf := tool.ID()
	old, err := m.ReadRuns(RunsQuery{At: &before.Version, LeafID: &oldLeaf})
	if err != nil || old.Items[0].Result != nil {
		t.Fatal("future completion leaked into fixed snapshot", err)
	}
	if _, err := m.AppendCompaction(context.Background(), "context checkpoint", user.ID(), 10, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	compacted, err := m.ReadRuns(RunsQuery{})
	if err != nil || compacted.Total != 1 || len(compacted.Items[0].Run.Anchors) != 2 || compacted.Items[0].Run.Result.ID != final.ID() {
		t.Fatal("compaction split or hid the original run", err)
	}
	appendReadRun(t, m, "next", 0)
	next, err := m.ReadRuns(RunsQuery{AfterID: compacted.AnchorID})
	if err != nil || next.Start != 1 || len(next.Items) != 1 {
		t.Fatalf("new run = %#v, %v", next, err)
	}
}

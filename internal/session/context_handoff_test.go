package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/agentmsg"
)

func TestContextHandoffKeepsOriginalHistoryAcrossWindowsBranchesAndRestart(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	m, err := CreateSessionManager(cwd, cwd, NewSessionOptions{ID: "handoff-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	first, err := m.AppendLLMMessage(ctx, mustUserMessage(t, "original task: preserve this correction", time.UnixMilli(1)))
	if err != nil {
		t.Fatal(err)
	}
	original := first.RawJSON()
	h1, err := m.CommitContextHandoff(ctx, "request-one", "continue in module A", 100000)
	if err != nil {
		t.Fatal(err)
	}
	file, _ := m.SessionFile()
	if _, err := os.Stat(file); err != nil {
		t.Fatal("checkpoint was not persisted before first assistant:", err)
	}
	if len(m.BuildContext().Messages()) != 1 {
		t.Fatal("old dialogue leaked into handoff context")
	}
	if _, err := m.AppendLLMMessage(ctx, managerAssistant(t, "completed A; next B", time.UnixMilli(2))); err != nil {
		t.Fatal(err)
	}
	h2, err := m.CommitContextHandoff(ctx, "request-two", "continue in module B", 80000)
	if err != nil {
		t.Fatal(err)
	}
	if entry, err := m.CommitContextHandoff(ctx, "request-one", "continue in module A", 1); err != nil || entry.ID() != h1.ID() {
		t.Fatalf("idempotent retry: %s %v", entry.ID(), err)
	}
	if _, err := m.CommitContextHandoff(ctx, "request-one", "different", 1); err == nil {
		t.Fatal("conflicting duplicate accepted")
	}
	active := m.BuildContext()
	if len(active.Messages()) != 1 || strings.Contains(handoffText(active), "continue in module A") {
		t.Fatal("older handoff was stacked")
	}
	view, err := m.ReadHistoryView(HistoryViewQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Records) != 2 || view.Records[0].EntryID != first.ID() || len(view.Windows) != 3 || view.Windows[2].Previous != h1.ID() {
		t.Fatalf("history = %#v", view)
	}
	if !reflect.DeepEqual(m.Entries()[0].RawJSON(), original) {
		t.Fatal("original raw entry changed")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = OpenSessionManager(file, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	if window, err := m.CurrentContextWindow(); err != nil || window != h2.ID() {
		t.Fatalf("reopen window %s %v", window, err)
	}
	index, err := OpenReadIndex(file)
	if err != nil {
		t.Fatal(err)
	}
	indexed, err := index.ReadHistoryView(HistoryViewQuery{WindowID: InitialContextWindow})
	if err != nil || len(indexed.Records) != 1 || indexed.Records[0].Blocks[0].Text != "original task: preserve this correction" {
		t.Fatalf("indexed history %#v %v", indexed, err)
	}
	if _, err := m.CommitContextHandoff(ctx, "request-two", "continue in module B", 1); err != nil {
		t.Fatal(err)
	}
	if err := m.Branch(first.ID()); err != nil {
		t.Fatal(err)
	}
	if window, _ := m.CurrentContextWindow(); window != InitialContextWindow {
		t.Fatal("sibling boundary entered branch")
	}
	if _, err := m.ReadHistoryView(HistoryViewQuery{EntryIDs: []string{h2.ID()}}); !errors.Is(err, ErrEntryNotFound) {
		t.Fatal("sibling exposed:", err)
	}
	branched, err := m.CommitContextHandoff(ctx, "branch-request", "different path", 10)
	if err != nil {
		t.Fatal(err)
	}
	if record, _ := branched.ContextHandoff(); record.PreviousWindow != InitialContextWindow {
		t.Fatal("wrong branch predecessor")
	}
}

func handoffText(value Context) string {
	message, ok := value.AgentMessages()[0].(agentmsg.Custom)
	if !ok {
		return ""
	}
	text, _ := message.StringContent()
	return text
}

func TestContextHandoffDefiniteFailureDoesNotChangeContext(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	m, err := CreateSessionManager(cwd, cwd, NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err = m.AppendLLMMessage(ctx, managerAssistant(t, "before", time.UnixMilli(1))); err != nil {
		t.Fatal(err)
	}
	before := m.Entries()
	oldStorage := m.store.storage
	m.store.storage = &fakeStorage{appendErr: errors.New("disk unavailable"), appendStarted: false}
	if _, err = m.CommitContextHandoff(ctx, "failed-request", "continue", 100); !errors.Is(err, ErrStorage) {
		t.Fatal(err)
	}
	m.store.storage = oldStorage
	if len(m.Entries()) != len(before) || len(m.BuildContext().Messages()) != 1 {
		t.Fatal("failed boundary changed context")
	}
	if window, _ := m.CurrentContextWindow(); window != InitialContextWindow {
		t.Fatal("failed handoff published")
	}
}

func TestHistoryDialoguePreservesCompleteBlocksPhaseAndAttachments(t *testing.T) {
	cwd := t.TempDir()
	file := filepath.Join(cwd, "dialogue.jsonl")
	long := strings.Repeat("visible line\n", 3000) + "final needle"
	header := map[string]any{"type": "session", "version": 3, "id": "dialogue-test", "cwd": cwd, "timestamp": "2026-11-09T00:00:00.000Z"}
	user := map[string]any{"type": "message", "id": "u1", "parentId": nil, "timestamp": "2026-11-09T00:00:01.000Z", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": long}, map[string]any{"type": "image", "mimeType": "image/png", "data": "eA=="}}, "timestamp": 1794182401000}}
	assistant := map[string]any{"type": "message", "id": "a1", "parentId": "u1", "timestamp": "2026-11-09T00:00:02.000Z", "message": map[string]any{"role": "assistant", "api": "openai-responses", "provider": "openai", "model": "fixture", "content": []any{map[string]any{"type": "thinking", "thinking": "hidden reasoning"}, map[string]any{"type": "text", "text": "working", "textSignature": `{"v":1,"id":"msg1","phase":"commentary"}`}, map[string]any{"type": "toolCall", "id": "call1", "name": "bash", "arguments": map[string]any{"command": "hidden tool command"}}, map[string]any{"type": "text", "text": "done", "phase": "final_answer"}}, "stopReason": "stop", "timestamp": 1794182402000}}
	var data []byte
	for _, value := range []any{header, user, assistant} {
		raw, _ := json.Marshal(value)
		data = append(append(data, raw...), '\n')
	}
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	index, err := OpenReadIndex(file)
	if err != nil {
		t.Fatal(err)
	}
	view, err := index.ReadHistoryView(HistoryViewQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Records) != 2 || len(view.Records[0].Blocks) != 2 || view.Records[0].Blocks[0].Text != long {
		t.Fatalf("incomplete dialogue %#v", view)
	}
	blocks := view.Records[1].Blocks
	if len(blocks) != 2 || blocks[0].Index != 1 || blocks[0].Phase != "commentary" || blocks[1].Index != 3 || blocks[1].Phase != "final_answer" {
		t.Fatalf("blocks %#v", blocks)
	}
	for _, needle := range []string{"hidden reasoning", "hidden tool command"} {
		found, err := index.ReadHistoryView(HistoryViewQuery{Search: needle})
		if err != nil || len(found.Records) != 0 {
			t.Fatalf("synthetic/private content entered dialogue: %v", err)
		}
	}
	found, err := index.ReadHistoryView(HistoryViewQuery{Search: "needle"})
	if err != nil || len(found.Records) != 1 || found.Records[0].Matches[0].Line != 3001 {
		t.Fatalf("long search %#v %v", found, err)
	}
	page, err := index.ReadHistoryView(HistoryViewQuery{Limit: 1})
	if err != nil || page.NextEntryID != "a1" {
		t.Fatal("record pagination failed", err)
	}
	next, err := index.ReadHistoryView(HistoryViewQuery{FromID: page.NextEntryID, Limit: 1})
	if err != nil || len(next.Records) != 1 || next.Records[0].EntryID != "a1" {
		t.Fatal("record continuation failed", err)
	}
	raw, err := index.ReadHistoryView(HistoryViewQuery{View: "raw", EntryIDs: []string{"a1"}})
	if err != nil || len(raw.Records) != 1 || !strings.Contains(string(raw.Records[0].Raw), "hidden reasoning") {
		t.Fatal("raw expansion lost original fields", err)
	}
}

func TestRepeatedHandoffsKeepFirstTaskAndConstrainLaterSummary(t *testing.T) {
	ctx := context.Background()
	m, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	first, err := m.AppendLLMMessage(ctx, mustUserMessage(t, "first task verbatim", time.UnixMilli(1)))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := m.CommitContextHandoff(ctx, fmt.Sprintf("request-%d", i), fmt.Sprintf("current step %d", i), 100000); err != nil {
			t.Fatal(err)
		}
		if len(m.BuildContext().AgentMessages()) != 1 {
			t.Fatal("handoffs accumulated in context")
		}
	}
	view, err := m.ReadHistoryView(HistoryViewQuery{EntryIDs: []string{first.ID()}})
	if err != nil || len(view.Records) != 1 || view.Records[0].Blocks[0].Text != "first task verbatim" || len(view.Windows) != 101 {
		t.Fatal("first task unavailable after repeated handoffs", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := m.AppendLLMMessage(ctx, mustUserMessage(t, strings.Repeat("current question ", 50), time.UnixMilli(int64(2*i+2)))); err != nil {
			t.Fatal(err)
		}
		if _, err := m.AppendLLMMessage(ctx, managerAssistant(t, "current answer", time.UnixMilli(int64(2*i+3)))); err != nil {
			t.Fatal(err)
		}
	}
	input, err := m.PrepareCompaction(ctx, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if input.PreviousSummary != "current step 99" || strings.Contains(input.Prompt, "first task verbatim") || strings.Contains(input.Prompt, "current step 98") {
		t.Fatal("summary reloaded previous windows")
	}
	result, err := m.CommitCompaction(ctx, input, SummaryOutput{Text: "summarized current work"})
	if err != nil || !result.Committed {
		t.Fatal("normal compaction after handoff failed", err)
	}
	if strings.Contains(string(result.Entry.RawJSON()), ContextHandoffType) {
		t.Fatal("summary masqueraded as handoff")
	}
}

func TestInitialHandoffRejectsClosedManagerAndFailedFlush(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(fmt.Sprint(closed), func(t *testing.T) {
			ctx := context.Background()
			cwd := t.TempDir()
			m, err := CreateSessionManager(cwd, cwd, NewSessionOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if _, err := m.AppendLLMMessage(ctx, mustUserMessage(t, "keep me", time.UnixMilli(1))); err != nil {
				t.Fatal(err)
			}
			if closed {
				if err := m.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				// A non-directory parent makes snapshot creation fail before writing.
				parent := filepath.Join(cwd, "blocked")
				if err := os.WriteFile(parent, []byte("untouched"), 0600); err != nil {
					t.Fatal(err)
				}
				m.sessionFile = filepath.Join(parent, "session.jsonl")
			}
			if _, err := m.CommitContextHandoff(ctx, "not-committed", "next", 100); err == nil {
				t.Fatal("invalid initial handoff succeeded")
			}
			if len(m.Entries()) != 1 || len(m.BuildContext().Messages()) != 1 {
				t.Fatal("failed initial handoff changed context")
			}
		})
	}
}

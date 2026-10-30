package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

func TestManagerSummaryPreservesHistoryAcrossCompactionBranchAndReplacement(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	manager, err := CreateSessionManager(cwd, cwd, NewSessionOptions{ID: "summary-history"})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	created := manager.Header().Timestamp()
	if got := manager.Summary(); got.MessageCount != 0 || got.FirstMessage != "(no messages)" || !got.Modified.Equal(created) {
		t.Fatalf("empty summary = %#v", got)
	}
	userAt, assistantAt := created.Add(time.Second), created.Add(2*time.Second)
	first, err := manager.AppendLLMMessage(ctx, mustUserMessage(t, "first question", userAt))
	if err != nil {
		t.Fatal(err)
	}
	path, _ := manager.SessionFile()
	if got := manager.Summary(); got.MessageCount != 1 || got.FirstMessage != "first question" || !got.Modified.Equal(userAt) {
		t.Fatalf("delayed summary = %#v", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("summary caused persistence: %v", err)
	}
	assistant, err := manager.AppendLLMMessage(ctx, managerAssistant(t, "answer", assistantAt))
	if err != nil {
		t.Fatal(err)
	}
	tool, err := llm.NewToolResultMessage("call", "read", []llm.TextBlock{mustTextBlock(t, "output")}, false, created.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(ctx, tool); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendSessionInfo(ctx, "  Named\nSession  "); err != nil {
		t.Fatal(err)
	}
	if got := manager.Summary(); got.MessageCount != 3 || got.Name != "Named Session" || !got.HasName || !got.Modified.Equal(assistantAt) {
		t.Fatalf("tool/name changed conversation activity: %#v", got)
	}
	if err := manager.Branch(first.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendLLMMessage(ctx, mustUserMessage(t, "other branch", userAt)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendCompaction(ctx, "summary", first.ID(), 100, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendSessionInfo(ctx, " \n "); err != nil {
		t.Fatal(err)
	}
	want := manager.Summary()
	if want.MessageCount != 4 || want.FirstMessage != "first question" || want.HasName || want.Name != "" || !want.Modified.Equal(assistantAt) || want.LastAssistantMessageID != assistant.ID() {
		t.Fatalf("branch/compaction/title clear changed summary: %#v", want)
	}
	if len(manager.BuildContext().Messages()) >= want.MessageCount {
		t.Fatal("fixture did not compact/branch its context")
	}
	copy := manager.Summary()
	copy.FirstMessage, copy.Name = "caller mutation", "caller mutation"
	if got := manager.Summary(); !reflect.DeepEqual(got, want) {
		t.Fatalf("mutable summary = %#v", got)
	}
	clone, err := manager.CloneBranchedSession(ctx, first.ID())
	if err != nil {
		t.Fatal(err)
	}
	cloned := clone.Summary()
	if cloned.MessageCount != 1 || cloned.FirstMessage != "first question" || cloned.ParentSessionPath != path || !cloned.HasParentSession || cloned.LastAssistantMessageID != "" {
		t.Fatalf("fork summary = %#v", cloned)
	}
	if err := clone.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.NewSession(NewSessionOptions{ID: "summary-empty"}); err != nil {
		t.Fatal(err)
	}
	if got := manager.Summary(); got.MessageCount != 0 || got.ID != "summary-empty" || got.HasName {
		t.Fatalf("replacement summary = %#v", got)
	}
	if err := manager.SetSessionFile(path); err != nil {
		t.Fatal(err)
	}
	if got := manager.Summary(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened summary = %#v, want %#v", got, want)
	}
	if disk, ok := buildSessionCatalogInfo(path); !ok || !reflect.DeepEqual(disk, want) {
		t.Fatalf("durable summary = %#v, %v; want %#v", disk, ok, want)
	}
}

func TestManagerSummaryUsesPiMessageEnvelopeAndExplicitNameClears(t *testing.T) {
	// Matches pi's buildSessionInfo: count all message entries, use only
	// user/assistant content for activity, and fall back to entry timestamps.
	cwd := t.TempDir()
	path := filepath.Join(cwd, "compatible.jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": "summary-compatible", "cwd": cwd, "timestamp": "2026-08-01T00:00:00.000Z"})
	lines := []string{
		`{"role":"user","content":[],"timestamp":2000}`,
		`{"role":"user","content":"(no messages)","timestamp":3000}`,
		`{"role":"assistant","content":[{"type":"text","text":"legacy reply"}]}`,
		`{"role":"toolResult","content":"large output","timestamp":2200000000000}`,
		`{"role":"assistant","timestamp":2300000000000}`,
		`{"role":"extension","content":"custom","timestamp":2400000000000}`,
		`{"role":"user","content":"later question","timestamp":4000}`,
	}
	data := append(header, '\n')
	for i, message := range lines {
		var parent any
		if i > 0 {
			parent = fmt.Sprintf("e%d", i-1)
		}
		entry, err := json.Marshal(map[string]any{"type": "message", "id": fmt.Sprintf("e%d", i), "parentId": parent, "timestamp": "2026-08-02T00:00:00.000Z", "message": json.RawMessage(message)})
		if err != nil {
			t.Fatal(err)
		}
		data = append(append(data, entry...), '\n')
	}
	data = append(data, []byte(`{"type":"session_info","id":"name1","parentId":"e6","timestamp":"2026-08-03T00:00:00.000Z","name":"old name"}`+"\n"+`{"type":"session_info","id":"name2","parentId":"name1","timestamp":"2026-08-04T00:00:00.000Z","name":null}`+"\n")...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := OpenSessionManager(path, cwd, "")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	wantActivity := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	got := manager.Summary()
	if got.MessageCount != 7 || got.FirstMessage != "(no messages)" || got.HasName || !got.Modified.Equal(wantActivity) {
		t.Fatalf("compatible summary = %#v", got)
	}
	if disk, ok := buildSessionCatalogInfo(path); !ok || !reflect.DeepEqual(disk, got) {
		t.Fatalf("discovery/loaded summary differ: %#v / %#v", disk, got)
	}
	if _, named := manager.SessionName(); named {
		t.Fatal("explicit clear was lost")
	}
}

func TestManagerSummaryExcludesUnpublishedAndCancelledEntries(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprintf("write_started_%v", started), func(t *testing.T) {
			manager, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			if _, err := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, "retained", time.UnixMilli(1))); err != nil {
				t.Fatal(err)
			}
			want := manager.Summary()
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := manager.AppendSessionInfo(cancelled, "cancelled"); err == nil {
				t.Fatal("cancelled append succeeded")
			}
			manager.store.storage = &fakeStorage{appendStarted: started, appendErr: errors.New("write failed")}
			if _, err := manager.AppendLLMMessage(context.Background(), mustUserMessage(t, "failed", time.UnixMilli(2))); err == nil {
				t.Fatal("failed append succeeded")
			}
			if got := manager.Summary(); !reflect.DeepEqual(got, want) {
				t.Fatalf("unpublished entry in summary: %#v, want %#v", got, want)
			}
		})
	}
}

func TestManagerSummaryConcurrentAppendKeepsCountAndActivityConsistent(t *testing.T) {
	manager, err := InMemorySessionManager(t.TempDir(), NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	epoch := time.UnixMilli(1000)
	var readers sync.WaitGroup
	done := make(chan struct{})
	for range 4 {
		readers.Go(func() {
			lastCount := 0
			for {
				select {
				case <-done:
					return
				default:
				}
				info := manager.Summary()
				if info.MessageCount < lastCount || info.MessageCount > 0 && (!info.Modified.Equal(epoch.Add(time.Duration(info.MessageCount-1)*time.Millisecond)) || info.FirstMessage != "question") {
					t.Errorf("inconsistent summary: %#v", info)
					return
				}
				lastCount = info.MessageCount
			}
		})
	}
	for i := 0; i < 100; i++ {
		message := mustUserMessage(t, "question", epoch.Add(time.Duration(i)*time.Millisecond))
		if _, err := manager.AppendLLMMessage(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	readers.Wait()
	if got := manager.Summary().MessageCount; got != 100 {
		t.Fatalf("final message count = %d", got)
	}
}
